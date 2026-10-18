import { Fragment, useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import {
  previewImageType,
  type ChangedFile, type ChangeKind, type ChangeSide, type CommentSide, type CommentState, type ReviewComment, type Session,
  type SessionChanges,
} from '../../shared/types';
import { api, errorMessage } from '../api';
import { anchorComment, parsePatch, showsLine, sideLine, type DiffLine } from '../diff';
import { useTouch } from '../hooks/useMedia';
import { agentName, cx } from '../util';
import { Busy, ConfirmButton } from './common';
import { SidePanel } from './SidePanel';

const KIND: Record<ChangeKind, { letter: string; label: string }> = {
  added: { letter: 'A', label: 'added' },
  modified: { letter: 'M', label: 'modified' },
  deleted: { letter: 'D', label: 'deleted' },
  renamed: { letter: 'R', label: 'renamed' },
  untracked: { letter: 'U', label: 'untracked (not added to git yet)' },
};

const STATE_LABEL: Record<CommentState, string> = { draft: 'draft', queued: 'queued', sent: 'sent' };

/** Diffs start collapsed once this many lines are already shown, so big branches open fast. */
const OPEN_LINE_BUDGET = 2000;

const plural = (n: number, word: string) => `${n.toLocaleString()} ${word}${n === 1 ? '' : 's'}`;

/** A comment being written (id null) or edited, and the lines it's about. */
interface Editor {
  id: string | null;
  path: string;
  side: CommentSide;
  line: number;
  endLine: number;
  excerpt: string;
}

/** Where each visible comment goes: after a row of its file's diff, at the top of the file, or above every file. */
interface Placed {
  at: Map<string, Map<number, ReviewComment[]>>;
  outdated: Map<string, ReviewComment[]>;
  /** Outdated comments whose line the diff still shows, with other text; the rest just aren't in the diff shown. */
  changed: Set<string>;
  gone: ReviewComment[];
}

const isTyping = (t: EventTarget) => t instanceof HTMLElement && !!t.closest('input, textarea, select, [contenteditable="true"]');

export function ChangesPanel({ session, onClose }: { session: Session; onClose: () => void }) {
  const touch = useTouch();
  const [data, setData] = useState<SessionChanges | null>(null);
  /** When the diff was last fetched; image previews reload with it. */
  const [fetchedAt, setFetchedAt] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [tick, setTick] = useState(0);
  /** Files the user opened or closed; the rest follow the line budget. */
  const [toggled, setToggled] = useState<Map<string, boolean>>(new Map());
  const sections = useRef(new Map<string, HTMLElement>());
  const body = useRef<HTMLDivElement>(null);
  /** The file last jumped to; short files at the end can't scroll to the top, so position alone can't tell. */
  const lastJump = useRef(-1);

  const [comments, setComments] = useState<ReviewComment[]>([]);
  const [showSent, setShowSent] = useState(false);
  const [editor, setEditor] = useState<Editor | null>(null);
  /** Text typed into the open editor, kept here so it survives the editor moving to another row. */
  const draftText = useRef('');
  /** The line a shift-click extends a range from. */
  const [pick, setPick] = useState<{ path: string; side: CommentSide; line: number } | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [sending, setSending] = useState(false);

  // Refetch when the footer's numbers move (the server refreshes them periodically) or on Refresh.
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api
      .changes(session.id)
      .then((d) => !cancelled && (setData(d), setFetchedAt(Date.now()), setError(null)))
      .catch((e) => !cancelled && setError(errorMessage(e)))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [session.id, session.baseBranch, session.diffStat?.files, session.diffStat?.added, session.diffStat?.removed, tick]);

  // Comments move between draft, queued and sent on the server; the counts in state say when.
  useEffect(() => {
    let cancelled = false;
    api
      .comments(session.id)
      .then((c) => !cancelled && setComments(c))
      .catch((e) => !cancelled && setNote(errorMessage(e)));
    return () => {
      cancelled = true;
    };
  }, [session.id, session.comments.drafts, session.comments.queued, tick]);

  const files = useMemo(() => data?.files ?? [], [data]);

  const parsed = useMemo(() => {
    const m = new Map<string, DiffLine[]>();
    for (const f of files) if (f.patch !== null) m.set(f.path, parsePatch(f.patch));
    return m;
  }, [files]);

  const visible = useMemo(() => comments.filter((c) => showSent || c.state !== 'sent'), [comments, showSent]);
  const sentCount = comments.filter((c) => c.state === 'sent').length;
  const drafts = comments.filter((c) => c.state === 'draft').length;
  const queued = comments.filter((c) => c.state === 'queued').length;

  const placed = useMemo((): Placed => {
    const p: Placed = { at: new Map(), outdated: new Map(), changed: new Set(), gone: [] };
    const paths = new Set(files.map((f) => f.path));
    for (const c of visible) {
      if (!paths.has(c.path)) {
        p.gone.push(c);
        continue;
      }
      const lines = parsed.get(c.path);
      const i = lines ? anchorComment(lines, c) : null;
      if (i === null) {
        if (lines && showsLine(lines, c.side, c.endLine)) p.changed.add(c.id);
        p.outdated.set(c.path, [...(p.outdated.get(c.path) ?? []), c]);
      } else {
        const rows = p.at.get(c.path) ?? new Map<number, ReviewComment[]>();
        rows.set(i, [...(rows.get(i) ?? []), c]);
        p.at.set(c.path, rows);
      }
    }
    return p;
  }, [files, parsed, visible]);

  /** The row a new comment's editor sits under. */
  const editorAt = useMemo(
    () => (editor && !editor.id ? anchorComment(parsed.get(editor.path) ?? [], editor) : null),
    [editor, parsed],
  );

  const countByPath = useMemo(() => {
    const m = new Map<string, number>();
    for (const c of comments) if (c.state !== 'sent') m.set(c.path, (m.get(c.path) ?? 0) + 1);
    return m;
  }, [comments]);

  const openByDefault = useMemo(() => {
    const open = new Set<string>();
    let shown = 0;
    for (const f of files) {
      const n = parsed.get(f.path)?.length ?? 0;
      // A file with comments always opens, so none of them are hidden.
      if (shown < OPEN_LINE_BUDGET || placed.at.has(f.path) || placed.outdated.has(f.path)) open.add(f.path);
      shown += n;
    }
    return open;
  }, [files, parsed, placed]);

  const isOpen = (path: string) => toggled.get(path) ?? openByDefault.has(path);
  const setOpen = (path: string, open: boolean) => setToggled((t) => new Map(t).set(path, open));

  const jump = (path: string) => {
    lastJump.current = files.findIndex((f) => f.path === path);
    setOpen(path, true);
    requestAnimationFrame(() => sections.current.get(path)?.scrollIntoView({ block: 'start' }));
  };

  /** Moves to the next or previous file; back from the middle of a file goes to its top. */
  const step = (d: 1 | -1) => {
    const top = body.current?.getBoundingClientRect().top ?? 0;
    let cur = -1;
    let inside = false;
    files.forEach((f, i) => {
      const y = sections.current.get(f.path)?.getBoundingClientRect().top;
      if (y !== undefined && y - top <= 8) {
        cur = i;
        inside = y - top < -8;
      }
    });
    const el = body.current;
    if (el && el.scrollTop + el.clientHeight >= el.scrollHeight - 2 && lastJump.current > cur) {
      cur = lastJump.current;
      inside = false;
    }
    const next = d < 0 && inside ? cur : cur + d;
    if (next >= 0 && next < files.length) jump(files[next].path);
  };

  const startComment = (path: string, i: number, shift: boolean) => {
    const lines = parsed.get(path);
    const l = lines?.[i];
    if (!lines || !l) return;
    const side: CommentSide = l.kind === 'del' ? 'old' : 'new';
    const n = sideLine(l, side);
    if (n === null) return;
    // Touch has no shift-click: tapping another line while a new comment is still empty extends it.
    const extend = shift || (touch && !!editor && !editor.id && !draftText.current.trim() && pick?.line !== n);
    let from = n, to = n;
    if (extend && pick && pick.path === path && pick.side === side) {
      from = Math.min(pick.line, n);
      to = Math.max(pick.line, n);
    } else {
      setPick({ path, side, line: n });
    }
    let rows = lines.filter((x) => {
      const m = sideLine(x, side);
      return m !== null && m >= from && m <= to;
    });
    // A range across hunks would skip the lines between them; comment on the clicked line instead.
    if (rows.length !== to - from + 1) {
      from = to = n;
      rows = [l];
      setPick({ path, side, line: n });
    }
    // Switching from editing a comment starts a fresh one; a new comment keeps what was typed.
    if (editor?.id) draftText.current = '';
    setEditor({ id: null, path, side, line: from, endLine: to, excerpt: rows.map((r) => r.text).join('\n') });
  };

  const closeEditor = () => {
    setEditor(null);
    draftText.current = '';
  };

  const save = async (text: string) => {
    if (!editor) return;
    if (editor.id) {
      const c = await api.editComment(session.id, editor.id, text);
      setComments((cs) => cs.map((x) => (x.id === c.id ? c : x)));
    } else {
      const { path, side, line, endLine, excerpt } = editor;
      const c = await api.addComment(session.id, { path, side, line, endLine, excerpt, body: text });
      setComments((cs) => [...cs, c]);
    }
    closeEditor();
  };

  const edit = (c: ReviewComment) => {
    draftText.current = c.body;
    setEditor({ id: c.id, path: c.path, side: c.side, line: c.line, endLine: c.endLine, excerpt: c.excerpt });
  };

  const remove = async (c: ReviewComment) => {
    try {
      await api.deleteComment(session.id, c.id);
      setComments((cs) => cs.filter((x) => x.id !== c.id));
      if (editor?.id === c.id) closeEditor();
    } catch (e) {
      setNote(errorMessage(e));
    }
  };

  const send = async () => {
    setSending(true);
    setNote(null);
    try {
      const r = await api.sendComments(session.id);
      if (r.sent) setNote(`Sent ${plural(r.sent, 'comment')} to AgentK.`);
      setComments(await api.comments(session.id));
    } catch (e) {
      setNote(errorMessage(e));
    } finally {
      setSending(false);
    }
  };

  const keys = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key === 'Escape') onClose();
    else if ((e.key === '[' || e.key === ']') && !e.ctrlKey && !e.metaKey && !e.altKey && !isTyping(e.target)) {
      e.preventDefault();
      step(e.key === ']' ? 1 : -1);
    }
  };

  const exited = session.status === 'exited';
  /** A review's base, and a session's with a PR, is its PR's (the server refuses a change). */
  const fixedBase = session.kind === 'review-pr' ? 'A review compares with its PR\'s base.' : session.prNumber ? `The base of PR #${session.prNumber}.` : null;
  const changeBase = (branch: string) => {
    setNote(null);
    // The state push with the new base refetches the diff.
    api.updateSession(session.id, { baseBranch: branch }).catch((e) => setNote(errorMessage(e)));
  };
  const suggested = !fixedBase ? data?.suggestedBase : null;
  /** Comment cards: `away` for comments not placed on a row, `row` for the row the others follow. */
  const thread = (cs: ReviewComment[] | undefined, opts: { away?: boolean; withPath?: boolean; row?: DiffLine } = {}) =>
    cs?.map((c) =>
      editor?.id === c.id ? (
        <CommentEditor key={c.id} editor={editor} draft={draftText} onSave={save} onCancel={closeEditor} />
      ) : (
        <CommentCard
          key={c.id}
          c={c}
          away={opts.away ? (placed.changed.has(c.id) ? 'changed' : 'hidden') : undefined}
          shownAt={opts.row ? sideLine(opts.row, c.side) : null}
          withPath={opts.withPath}
          onEdit={() => edit(c)}
          onDelete={() => remove(c)}
        />
      ),
    );

  return (
    <SidePanel name="changes" label="Changes" className="design-panel changes-panel" onKeyDown={keys}>
      <div className="design-hd">
        <b>Changes</b>
        {data && (
          <span className="muted small">
            vs{' '}
            {fixedBase ? (
              <span className="mono" title={fixedBase}>{data.base}</span>
            ) : (
              <BaseMenu session={session} base={data.base} onPick={changeBase} />
            )}{' '}
            · {plural(files.length, 'file')} ·{' '}
            <span className="add">+{data.added}</span> <span className="del">−{data.removed}</span>
          </span>
        )}
        <span className="sp" />
        {files.length > 0 && <FilesMenu files={files} counts={countByPath} onJump={jump} />}
        {sentCount > 0 && (
          <button className="btn small" aria-pressed={showSent} onClick={() => setShowSent((s) => !s)}>
            {showSent ? 'Hide sent' : `Show sent (${sentCount})`}
          </button>
        )}
        {drafts > 0 && (
          <button
            className="btn small primary"
            disabled={sending || exited}
            title={
              session.kind === 'review-pr'
                ? 'Send your draft comments to AgentK, which drafts them as your review comments and posts nothing until you say'
                : 'Send your draft comments to AgentK'
            }
            onClick={send}
          >
            {sending ? <Busy label="Sending" /> : `Send ${plural(drafts, 'comment')}`}
          </button>
        )}
        <button className="btn small" disabled={loading} onClick={() => setTick((t) => t + 1)}>
          {loading ? <Busy label="Loading" /> : 'Refresh'}
        </button>
        <button className="icon-btn panel-close" onClick={onClose} aria-label="Close changes">
          <span className="desktop-only">×</span>
          <span className="mobile-only">← Back</span>
        </button>
      </div>
      {data && suggested && (
        <div className="chg-bar" role="status">
          <span>
            This branch is {plural(data.ahead, 'commit')} ahead of {session.baseBranch}, but only {plural(suggested.ahead, 'commit')} ahead
            of {suggested.branch}. Comparing with {suggested.branch} makes it this session's base, and the PR goes there.
          </span>
          <button className="btn small" title={`Compare with ${suggested.branch}`} onClick={() => changeBase(suggested.branch)}>
            Compare with <span className="mono base-name">{suggested.branch}</span>
          </button>
        </div>
      )}
      {session.kind === 'review-pr' && drafts > 0 && !queued && !note && !exited && (
        <div className="chg-bar">AgentK drafts these as your review comments on the PR, and posts nothing until you say.</div>
      )}
      {(note || queued > 0 || (exited && drafts > 0)) && (
        <div className="chg-bar" role="status">
          {exited && drafts > 0 && <span>{agentName(session)} has exited. Resume the session to send your comments.</span>}
          {queued > 0 && (
            <span>
              {plural(queued, 'comment')} queued. {queued === 1 ? 'It goes' : 'They go'} to AgentK when it finishes its turn
              {session.stage !== 'explore' && session.stage !== 'code' && session.stage !== 'review' && session.stage !== 'reviewing-pr'
                ? ' in the Code or Review stage'
                : ''}
              .
            </span>
          )}
          {note && <span>{note}</span>}
        </div>
      )}
      <div className="design-body changes-body" ref={body} tabIndex={-1}>
        {error ? (
          <p className="error-text">{error}</p>
        ) : !data ? (
          <p className="muted"><Busy label="Loading" /></p>
        ) : files.length === 0 ? (
          <p className="muted">No changes yet compared to {data.base}.</p>
        ) : (
          <>
            {data.omittedPatches > 0 && (
              <p className="muted small">
                {data.omittedPatches} {data.omittedPatches === 1 ? 'diff is' : 'diffs are'} left out because this
                branch’s diff is very large. Their line counts are still shown.
              </p>
            )}
            {placed.gone.length > 0 && (
              <div className="cmt-gone">
                <p className="muted small">Comments on files that are no longer changed</p>
                {thread(placed.gone, { away: true, withPath: true })}
              </div>
            )}
            {touch ? (
              <p className="muted small chg-hint">
                Use <b>Files</b> to move between files. Tap <b>+</b> beside a line to comment on it; before you type, tap <b>+</b> on
                another line to cover a range.
              </p>
            ) : (
              <p className="muted small chg-hint">
                Use <b>Files</b> or press <kbd>[</kbd> and <kbd>]</kbd> to move between files. Click <b>+</b> beside a line to
                comment on it; shift-click another line to cover a range.
              </p>
            )}
            {files.map((f, n) => (
              <section
                key={f.path}
                className="chg-file"
                ref={(el) => {
                  if (el) sections.current.set(f.path, el);
                  else sections.current.delete(f.path);
                }}
              >
                <button
                  className="chg-file-hd"
                  aria-expanded={isOpen(f.path)}
                  onClick={() => setOpen(f.path, !isOpen(f.path))}
                >
                  <span className="chg-caret" aria-hidden="true">
                    {isOpen(f.path) ? '▾' : '▸'}
                  </span>
                  <KindBadge kind={f.kind} />
                  <FilePath file={f} />
                  <span className="chg-pos mono" aria-label={`file ${n + 1} of ${files.length}`}>
                    {n + 1} / {files.length}
                  </span>
                  <Counts file={f} comments={countByPath.get(f.path) ?? 0} />
                </button>
                {isOpen(f.path) && (
                  <>
                    {placed.outdated.has(f.path) && (
                      <div className="cmt-outdated">{thread(placed.outdated.get(f.path), { away: true })}</div>
                    )}
                    <FileDiff
                      file={f}
                      imageUrl={(side, path) => api.changeImageUrl(session.id, path, side, fetchedAt)}
                      lines={parsed.get(f.path) ?? null}
                      renderAfter={(i) => {
                        const cs = placed.at.get(f.path)?.get(i);
                        const here = editor && !editor.id && editor.path === f.path && editorAt === i;
                        if (!cs && !here) return null;
                        return (
                          <>
                            {thread(cs, { row: parsed.get(f.path)?.[i] })}
                            {here && <CommentEditor key="new" editor={editor} draft={draftText} onSave={save} onCancel={closeEditor} />}
                          </>
                        );
                      }}
                      picked={editor && !editor.id && editor.path === f.path ? editor : null}
                      onComment={(i, shift) => startComment(f.path, i, shift)}
                    />
                  </>
                )}
              </section>
            ))}
          </>
        )}
      </div>
    </SidePanel>
  );
}

function FilesMenu({ files, counts, onJump }: { files: ChangedFile[]; counts: Map<string, number>; onJump: (path: string) => void }) {
  return (
    <FilterMenu
      label="Files ▾"
      title="Jump to a changed file"
      name="Changed files"
      listName="Files"
      placeholder="Filter files"
      items={files}
      itemKey={(f) => f.path}
      matches={(f, needle) => `${f.oldPath ?? ''} ${f.path}`.toLowerCase().includes(needle)}
      none={(q) => `No changed files match “${q}”.`}
      onChoose={(f) => onJump(f.path)}
      renderItem={(f) => (
        <>
          <KindBadge kind={f.kind} />
          <FilePath file={f} />
          <Counts file={f} comments={counts.get(f.path) ?? 0} />
        </>
      )}
    />
  );
}

/** The header's base: a menu of the branches the session can be compared with, which makes the picked one its base. */
function BaseMenu({ session, base, onPick }: { session: Session; base: string; onPick: (branch: string) => void }) {
  const [branches, setBranches] = useState<string[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = () => {
    setBranches(null);
    setError(null);
    api
      .baseBranches(session.id)
      .then(setBranches)
      .catch((e) => setError(errorMessage(e)));
  };
  return (
    <FilterMenu
      label={<><span className="mono base-name">{base}</span> ▾</>}
      title={`Compared with ${base}. Pick another branch to compare with.`}
      note="The branch you pick becomes this session's base, and the PR goes there."
      name="Base branches"
      listName="Branches"
      placeholder="Filter branches"
      align="left"
      items={branches}
      error={error}
      itemKey={(b) => b}
      matches={(b, needle) => b.toLowerCase().includes(needle)}
      none={(q) => `No branch matches “${q}”.`}
      onOpen={load}
      onChoose={(b) => b !== session.baseBranch && onPick(b)}
      renderItem={(b) => <span className={cx('chg-path mono', b === session.baseBranch && 'cur')}>{b}</span>}
    />
  );
}

/**
 * A button that opens a filterable list: type to filter, arrows and Enter to pick, Escape or a click
 * outside to close. `items` null while they load.
 */
function FilterMenu<T>(props: {
  label: ReactNode;
  title: string;
  name: string;
  listName: string;
  placeholder: string;
  items: T[] | null;
  error?: string | null;
  itemKey: (item: T) => string;
  matches: (item: T, needle: string) => boolean;
  none: (q: string) => string;
  renderItem: (item: T) => ReactNode;
  onChoose: (item: T) => void;
  onOpen?: () => void;
  align?: 'left' | 'right';
  /** A line under the filter, about what picking does. */
  note?: string;
}) {
  const { items, itemKey, matches, renderItem, onChoose } = props;
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState('');
  const [sel, setSel] = useState(0);
  const root = useRef<HTMLSpanElement>(null);
  const list = useRef<HTMLUListElement>(null);

  useEffect(() => {
    if (!open) return;
    const down = (e: MouseEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', down);
    return () => document.removeEventListener('mousedown', down);
  }, [open]);

  const needle = q.trim().toLowerCase();
  const shown = !items ? [] : needle ? items.filter((it) => matches(it, needle)) : items;
  const cur = Math.min(sel, Math.max(0, shown.length - 1));

  useEffect(() => {
    list.current?.children[cur]?.scrollIntoView({ block: 'nearest' });
  }, [cur, open]);

  const choose = (it: T | undefined) => {
    if (it === undefined) return;
    setOpen(false);
    onChoose(it);
  };

  const keys = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      setSel(Math.max(0, Math.min(shown.length - 1, cur + (e.key === 'ArrowDown' ? 1 : -1))));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      choose(shown[cur]);
    } else if (e.key === 'Escape') {
      e.stopPropagation(); // closes the menu, not the panel
      setOpen(false);
    }
  };

  return (
    <span className="files-menu" ref={root}>
      <button
        className={cx('btn small', open && 'on')}
        aria-haspopup="dialog"
        aria-expanded={open}
        title={props.title}
        onClick={() => {
          if (!open) props.onOpen?.();
          setOpen((o) => !o);
          setQ('');
          setSel(0);
        }}
      >
        {props.label}
      </button>
      {open && (
        <div className={cx('files-pop', props.align === 'left' && 'left')} role="dialog" aria-label={props.name}>
          <input
            className="files-filter mono"
            placeholder={props.placeholder}
            aria-label={props.placeholder}
            autoFocus
            value={q}
            onChange={(e) => {
              setQ(e.target.value);
              setSel(0);
            }}
            onKeyDown={keys}
          />
          {props.note && <p className="muted small files-note">{props.note}</p>}
          {props.error ? (
            <p className="error-text small files-none">{props.error}</p>
          ) : !items ? (
            <p className="muted small files-none"><Busy label="Loading" /></p>
          ) : shown.length ? (
            <ul className="files-list" role="listbox" ref={list} aria-label={props.listName}>
              {shown.map((it, i) => (
                <li key={itemKey(it)} role="option" aria-selected={i === cur}>
                  <button
                    className={cx('chg-row', i === cur && 'sel')}
                    tabIndex={-1}
                    onMouseEnter={() => setSel(i)}
                    onClick={() => choose(it)}
                  >
                    {renderItem(it)}
                  </button>
                </li>
              ))}
            </ul>
          ) : (
            <p className="muted small files-none">{props.none(q)}</p>
          )}
        </div>
      )}
    </span>
  );
}

function KindBadge({ kind }: { kind: ChangeKind }) {
  return (
    <span className={cx('chg-kind', kind)} title={KIND[kind].label} aria-label={KIND[kind].label}>
      {KIND[kind].letter}
    </span>
  );
}

function FilePath({ file }: { file: ChangedFile }) {
  return (
    <span className="chg-path mono" title={file.oldPath ? `${file.oldPath} → ${file.path}` : file.path}>
      {file.oldPath ? `${file.oldPath} → ${file.path}` : file.path}
    </span>
  );
}

function Counts({ file, comments }: { file: ChangedFile; comments: number }) {
  return (
    <span className="chg-counts mono">
      {comments > 0 && (
        <span className="chg-cmts" title={`${plural(comments, 'comment')} not sent yet`}>
          <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinejoin="round" aria-hidden="true">
            <path d="M4 4h16v12H9l-5 4z" />
          </svg>
          {comments}
        </span>
      )}
      {file.uncommitted && <span className="chg-tag">uncommitted</span>}
      {file.binary ? (
        <span className="muted">binary</span>
      ) : (
        <>
          <span className="add">+{file.added}</span> <span className="del">−{file.removed}</span>
        </>
      )}
    </span>
  );
}

function linesLabel(side: CommentSide, line: number, endLine: number): string {
  const n = line === endLine ? `line ${line}` : `lines ${line}–${endLine}`;
  return side === 'old' ? `removed ${n}` : n;
}

const AWAY_NOTE = {
  changed: 'These lines have changed since the comment was written. It was on:',
  hidden: 'These lines aren’t in the diff shown here. It was on:',
};

function CommentCard({
  c,
  away,
  shownAt,
  withPath,
  onEdit,
  onDelete,
}: {
  c: ReviewComment;
  /** Why the comment isn't on a row: its lines changed, or they aren't in the diff shown. */
  away?: 'changed' | 'hidden';
  /** The line it's shown at now, which moves when lines are added or removed above it. */
  shownAt: number | null;
  withPath?: boolean;
  onEdit: () => void;
  onDelete: () => Promise<void>;
}) {
  const shift = shownAt === null ? 0 : shownAt - c.endLine;
  return (
    <div className={cx('cmt', c.state === 'sent' && 'sent')}>
      <div className="cmt-meta">
        <span className={cx('cmt-state', c.state)}>{STATE_LABEL[c.state]}</span>
        <span>
          {withPath && <span className="mono">{c.path} · </span>}
          {linesLabel(c.side, c.line + shift, c.endLine + shift)}
          {shift !== 0 && ` (was ${c.line === c.endLine ? c.line : `${c.line}–${c.endLine}`})`}
        </span>
        <span className="sp" />
        {c.state === 'draft' && (
          <>
            <button className="link-btn" onClick={onEdit}>
              Edit
            </button>
            <ConfirmButton className="link-btn" label="Delete" confirmLabel="Delete" prompt="Delete this draft?" danger onConfirm={onDelete} />
          </>
        )}
      </div>
      {away && c.excerpt && (
        <>
          <p className={cx('cmt-outdated-note', away === 'hidden' && 'neutral')}>{AWAY_NOTE[away]}</p>
          <pre className="cmt-excerpt">{c.excerpt}</pre>
        </>
      )}
      <div className="cmt-body">{c.body}</div>
    </div>
  );
}

function CommentEditor({
  editor,
  draft,
  onSave,
  onCancel,
}: {
  editor: Editor;
  draft: { current: string };
  onSave: (text: string) => Promise<void>;
  onCancel: () => void;
}) {
  const [text, setText] = useState(draft.current);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const label = linesLabel(editor.side, editor.line, editor.endLine);

  const submit = async () => {
    if (!text.trim() || busy) return;
    setBusy(true);
    setErr(null);
    try {
      await onSave(text);
    } catch (e) {
      setErr(errorMessage(e));
      setBusy(false);
    }
  };

  return (
    <div className="cmt editing">
      <div className="cmt-meta">
        {editor.id && <span className="cmt-state draft">editing</span>}
        <span>{label}</span>
      </div>
      <textarea
        id={`comment-editor-${editor.id ?? 'new'}`}
        className="cmt-input"
        aria-label={`Comment on ${label}`}
        autoFocus
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          draft.current = e.target.value;
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
            e.preventDefault();
            void submit();
          } else if (e.key === 'Escape') {
            e.stopPropagation(); // cancels the comment, not the panel
            onCancel();
          }
        }}
      />
      {err && <p className="error-text small">{err}</p>}
      <div className="cmt-acts">
        <span className="muted small hide-touch">Ctrl+Enter to save</span>
        <button className="btn small" onClick={onCancel}>
          Cancel
        </button>
        <button className="btn small primary" disabled={busy || !text.trim()} onClick={() => void submit()}>
          {busy ? <Busy label="Saving" /> : editor.id ? 'Save' : 'Save draft'}
        </button>
      </div>
    </div>
  );
}

/** A changed image: the base's version next to the worktree's, or just the one that exists. */
function ImageDiff({ file, imageUrl }: { file: ChangedFile; imageUrl: (side: ChangeSide, path: string) => string }) {
  const hasOld = file.kind !== 'added' && file.kind !== 'untracked';
  const hasNew = file.kind !== 'deleted';
  return (
    <div className="img-diff">
      {hasOld && <ImageSide label={hasNew ? 'Before' : 'Deleted'} src={imageUrl('old', file.oldPath ?? file.path)} />}
      {hasNew && <ImageSide label={hasOld ? 'After' : 'Added'} src={imageUrl('new', file.path)} />}
    </div>
  );
}

function ImageSide({ label, src }: { label: string; src: string }) {
  const [size, setSize] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  return (
    <figure className="img-side">
      <figcaption>
        <b>{label}</b>
        {size && <span className="muted mono">{size}</span>}
      </figcaption>
      {failed ? (
        <p className="chg-note">This image couldn’t be shown.</p>
      ) : (
        <a className="img-box" href={src} target="_blank" rel="noopener noreferrer" title="Open full size in a new tab">
          <img
            src={src}
            alt={`${label} image`}
            onLoad={(e) => setSize(`${e.currentTarget.naturalWidth} × ${e.currentTarget.naturalHeight}`)}
            onError={() => setFailed(true)}
          />
        </a>
      )}
    </figure>
  );
}

function FileDiff({
  file,
  imageUrl,
  lines,
  renderAfter,
  picked,
  onComment,
}: {
  file: ChangedFile;
  imageUrl: (side: ChangeSide, path: string) => string;
  lines: DiffLine[] | null;
  /** Comment threads (or the editor) shown under row i, or null. */
  renderAfter: (i: number) => ReactNode;
  /** The range a new comment is being written for. */
  picked: { side: CommentSide; line: number; endLine: number } | null;
  onComment: (i: number, shift: boolean) => void;
}) {
  if (file.binary) {
    return previewImageType(file.path) ? <ImageDiff file={file} imageUrl={imageUrl} /> : <p className="chg-note">Binary file, not shown.</p>;
  }
  if (!lines) return <p className="chg-note">This diff is too large to show here.</p>;
  if (lines.length === 0) return <p className="chg-note">{file.kind === 'untracked' ? 'Empty file.' : 'No content changes.'}</p>;
  return (
    <div className="diff-wrap">
      <table className="diff">
        <tbody>
          {lines.map((l, i) => {
            const side: CommentSide = l.kind === 'del' ? 'old' : 'new';
            const n = sideLine(l, side);
            const inPick = picked !== null && n !== null && picked.side === side && n >= picked.line && n <= picked.endLine;
            const after = renderAfter(i);
            return (
              <Fragment key={i}>
                <tr className={cx(`dl-${l.kind}`, inPick && 'picked')}>
                  <td className="cg">
                    {n !== null && (
                      <button
                        className="cmt-add"
                        tabIndex={-1}
                        title="Comment on this line (shift-click to cover a range)"
                        aria-label={`Comment on ${linesLabel(side, n, n)}`}
                        onClick={(e) => onComment(i, e.shiftKey)}
                      >
                        +
                      </button>
                    )}
                  </td>
                  <td className="ln">{l.old ?? ''}</td>
                  <td className="ln">{l.new ?? ''}</td>
                  <td className="sign">{l.kind === 'add' ? '+' : l.kind === 'del' ? '−' : ''}</td>
                  <td className="code">{l.text}</td>
                </tr>
                {after && (
                  <tr className="cmt-row">
                    <td colSpan={5}>
                      <div className="cmt-thread">{after}</div>
                    </td>
                  </tr>
                )}
              </Fragment>
            );
          })}
        </tbody>
      </table>
      {file.truncated && <p className="chg-note">Diff cut short: this file’s diff is larger than 256 KB.</p>}
    </div>
  );
}
