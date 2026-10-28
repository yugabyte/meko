import { Fragment, memo, useCallback, useEffect, useMemo, useRef, useState, type CSSProperties, type KeyboardEvent, type ReactNode, type RefObject } from 'react';
import {
  previewImageType,
  type BranchCommit, type ChangedFile, type ChangeKind, type ChangeSide, type CommentSide, type CommentState, type ReviewComment, type Session,
  type SessionChanges,
} from '../../shared/types';
import { ApiError, api, errorMessage } from '../api';
import { anchorComment, defaultOpen, DESKTOP_BUDGET, keepUnchanged, PHONE_BUDGET, pickComment, reparse, sameItems, showsLine, sideLine, type DiffLine } from '../diff';
import { matches, MOBILE_QUERY, useTouch } from '../hooks/useMedia';
import { useRangeDrag, type CommentPick } from '../hooks/useRangeDrag';
import { agentName, cx, relativeTime } from '../util';
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

/**
 * What a file's section can do. The panel makes one object that never changes, calling its latest
 * handlers through a ref, so a handler is never a reason to draw a section again.
 */
interface SectionActions {
  toggle: (path: string) => void;
  more: (path: string, rows: number) => void;
  comment: (path: string, i: number, pick?: CommentPick) => void;
  edit: (c: ReviewComment) => void;
  remove: (c: ReviewComment) => Promise<void>;
  save: (text: string) => Promise<void>;
  cancel: () => void;
  section: (path: string, el: HTMLElement | null) => void;
}

/** Whether two files' comment threads hold the same comments after the same rows. */
function sameThreads(a: Map<number, ReviewComment[]>, b: Map<number, ReviewComment[]>): boolean {
  if (a.size !== b.size) return false;
  for (const [i, cs] of b) {
    const o = a.get(i);
    if (!o || !sameItems(o, cs)) return false;
  }
  return true;
}

/**
 * `next`, keeping `prev`'s per-file maps and lists (and its `changed` set) where they hold the same
 * comments, so a refetch doesn't hand every file with comments new props.
 */
function steady(prev: Placed | null, next: Placed): Placed {
  if (!prev) return next;
  for (const [path, rows] of next.at) {
    const o = prev.at.get(path);
    if (o && sameThreads(o, rows)) next.at.set(path, o);
  }
  for (const [path, cs] of next.outdated) {
    const o = prev.outdated.get(path);
    if (o && sameItems(o, cs)) next.outdated.set(path, o);
  }
  const changed = prev.changed.size === next.changed.size && [...next.changed].every((id) => prev.changed.has(id)) ? prev.changed : next.changed;
  return { ...next, changed };
}

const isTyping = (t: EventTarget) => t instanceof HTMLElement && !!t.closest('input, textarea, select, [contenteditable="true"]');

export function ChangesPanel({ session, onClose }: { session: Session; onClose: () => void }) {
  const touch = useTouch();
  // A phone lays rows out several times slower, so it draws fewer at a time. Read once per panel, so
  // turning a phone sideways or resizing the window doesn't open or close diffs under the user.
  const [budget] = useState(() => (matches(MOBILE_QUERY) ? PHONE_BUDGET : DESKTOP_BUDGET));
  const [data, setData] = useState<SessionChanges | null>(null);
  /** When the diff was last fetched; image previews reload with it. */
  const [fetchedAt, setFetchedAt] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [tick, setTick] = useState(0);
  /** The one commit shown (its sha), or null for everything vs. the base. */
  const [commit, setCommit] = useState<string | null>(null);
  /** Files the user opened or closed; the rest follow the line budget. */
  const [toggled, setToggled] = useState<Map<string, boolean>>(new Map());
  /** Rows shown of each large diff the user asked to see (the rest show none until asked). */
  const [rowsShown, setRowsShown] = useState<Map<string, number>>(new Map());
  const sections = useRef(new Map<string, HTMLElement>());
  const body = useRef<HTMLDivElement>(null);
  /** The file last jumped to; short files at the end can't scroll to the top, so position alone can't tell. */
  const lastJump = useRef(-1);

  const [comments, setComments] = useState<ReviewComment[]>([]);
  const [showSent, setShowSent] = useState(false);
  const [editor, setEditor] = useState<Editor | null>(null);
  /** Text typed into the open editor, kept here so it survives the editor moving to another row. */
  const draftText = useRef('');
  /** The line a shift-click (or a second tap) extends a range from. */
  const [pick, setPick] = useState<{ path: string; side: CommentSide; line: number } | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [sending, setSending] = useState(false);

  // Refetch when the footer's numbers move (the server refreshes them periodically) or on Refresh.
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api
      .changes(session.id, commit)
      .then((d) => {
        if (cancelled) return;
        // Files that didn't change keep their objects, so their sections aren't drawn again.
        setData((prev) => (prev ? { ...d, files: keepUnchanged(prev.files, d.files, (f) => f.path) } : d));
        setFetchedAt(Date.now());
        setError(null);
      })
      .catch((e) => {
        if (cancelled) return;
        // The commit shown was rewritten (amended, rebased): back to All changes, with a fresh commit list.
        if (commit && e instanceof ApiError && e.status === 404) {
          setNote('The commit you were viewing isn’t on the branch any more (it was amended or rebased), so this shows All changes.');
          setCommit(null);
        } else {
          setError(errorMessage(e));
        }
      })
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [session.id, session.baseBranch, session.diffStat?.files, session.diffStat?.added, session.diffStat?.removed, tick, commit]);

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

  /** Parsed rows by patch text, kept across refetches so a file whose patch didn't change keeps its rows. */
  const byPatch = useRef(new Map<string, DiffLine[]>());
  const parsed = useMemo(() => {
    byPatch.current = reparse(byPatch.current, files);
    const m = new Map<string, DiffLine[]>();
    for (const f of files) {
      const lines = f.patch === null ? undefined : byPatch.current.get(f.patch);
      if (lines) m.set(f.path, lines);
    }
    return m;
  }, [files]);

  // A comment's sides are the base and the worktree, so comments belong to the full diff only:
  // they follow the diff on screen, not the pick still loading.
  const shownCommit = data?.commit ?? null;
  const visible = useMemo(() => (shownCommit ? [] : comments.filter((c) => showSent || c.state !== 'sent')), [comments, showSent, shownCommit]);
  const sentCount = comments.filter((c) => c.state === 'sent').length;
  const drafts = comments.filter((c) => c.state === 'draft').length;
  const queued = comments.filter((c) => c.state === 'queued').length;

  /** The last placement, whose per-file maps and lists are kept while they still hold the same comments. */
  const lastPlaced = useRef<Placed | null>(null);
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
    return steady(lastPlaced.current, p);
  }, [files, parsed, visible]);
  lastPlaced.current = placed;

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

  const openByDefault = useMemo(
    () => defaultOpen(files, (path) => parsed.get(path)?.length ?? 0, (path) => placed.at.has(path) || placed.outdated.has(path), budget),
    [files, parsed, placed, budget],
  );

  const isOpen = (path: string) => toggled.get(path) ?? openByDefault.has(path);
  const setOpen = (path: string, open: boolean) => setToggled((t) => new Map(t).set(path, open));
  /** How many of a file's rows to render: all of a small diff, and of a large one what the user asked for. */
  const rowLimit = (path: string, rows: number) => (rows <= budget.large ? rows : Math.min(rows, rowsShown.get(path) ?? 0));
  const showMore = (path: string, rows: number) => setRowsShown((m) => new Map(m).set(path, Math.min(rows, (m.get(path) ?? 0) + budget.page)));

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

  const startComment = (path: string, i: number, how: CommentPick = {}) => {
    const lines = parsed.get(path);
    const l = lines?.[i];
    if (!lines || !l) return;
    const side: CommentSide = l.kind === 'del' ? 'old' : 'new';
    // Touch has no shift-click: tapping another line while a new comment is still empty extends it.
    const extend = how.shift || (touch && !!editor && !editor.id && !draftText.current.trim() && pick?.line !== sideLine(l, side));
    // A drag starts its range on the line it was pressed on; a shift-click or second tap extends the
    // range from the line it started on. It stops at the diff's next skipped lines (pickRange).
    const c = pickComment(lines, i, how.from ?? (extend && pick && pick.path === path && pick.side === side ? pick.line : null));
    if (!c) return;
    setPick({ path, side, line: c.anchor });
    // Switching from editing a comment starts a fresh one; a new comment keeps what was typed.
    if (editor?.id) draftText.current = '';
    setEditor({ id: null, path, side, line: c.line, endLine: c.endLine, excerpt: c.excerpt });
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
  const suggested = !fixedBase && !data?.commit ? data?.suggestedBase : null;
  const handlers = useRef<SectionActions | null>(null);
  handlers.current = {
    toggle: (path) => setOpen(path, !isOpen(path)),
    more: showMore,
    comment: startComment,
    edit,
    remove,
    save,
    cancel: closeEditor,
    section: (path, el) => {
      if (el) sections.current.set(path, el);
      else sections.current.delete(path);
    },
  };
  const actions = useMemo((): SectionActions => {
    const h = () => handlers.current as SectionActions;
    return {
      toggle: (path) => h().toggle(path),
      more: (path, rows) => h().more(path, rows),
      comment: (path, i, pick) => h().comment(path, i, pick),
      edit: (c) => h().edit(c),
      remove: (c) => h().remove(c),
      save: (text) => h().save(text),
      cancel: () => h().cancel(),
      section: (path, el) => h().section(path, el),
    };
  }, []);
  const commitSha = data?.commit?.sha;
  const imageUrl = useCallback(
    (side: ChangeSide, path: string, v: number) => api.changeImageUrl(session.id, path, side, v, commitSha),
    [session.id, commitSha],
  );

  return (
    <SidePanel name="changes" label="Changes" className="design-panel changes-panel" onKeyDown={keys}>
      <div className="design-hd">
        <b>Changes</b>
        {data && <CommitMenu commits={data.commits} current={data.commit} onPick={setCommit} />}
        {data && (
          <span className="muted small">
            {data.commit ? (
              <span className="mono" title={data.commit.subject}>{data.commit.sha.slice(0, 7)}</span>
            ) : (
              <>
                vs{' '}
                {fixedBase ? (
                  <span className="mono" title={fixedBase}>{data.base}</span>
                ) : (
                  <BaseMenu session={session} base={data.base} onPick={changeBase} />
                )}
              </>
            )}{' '}
            · {plural(files.length + (data.omittedFiles ?? 0), 'file')} ·{' '}
            <span className="add">+{data.added}</span> <span className="del">−{data.removed}</span>
          </span>
        )}
        <span className="sp" />
        {files.length > 0 && <FilesMenu files={files} counts={countByPath} onJump={jump} />}
        {sentCount > 0 && !shownCommit && (
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
          <p className="muted">{data.commit ? 'This commit changes no files.' : `No changes yet compared to ${data.base}.`}</p>
        ) : (
          <>
            {data.omittedFiles > 0 && (
              <p className="muted small">
                Showing {files.length.toLocaleString()} of {plural(files.length + data.omittedFiles, 'changed file')}: every file
                with comments, then the first by path. The line counts cover every file.
              </p>
            )}
            {data.omittedPatches > 0 && (
              <p className="muted small">
                {data.omittedPatches} {data.omittedPatches === 1 ? 'diff is' : 'diffs are'} left out because this
                branch’s diff is very large. Their line counts are still shown.
              </p>
            )}
            {data.commit && editor && (
              // Kept on screen (not just in draftText) so it can be finished here, and so a reload waits for it.
              <div className="cmt-gone">
                <p className="muted small">Your comment on <span className="mono">{editor.path}</span>, in All changes</p>
                <CommentEditor key={editor.id ?? 'new'} editor={editor} draft={draftText} onSave={save} onCancel={closeEditor} />
              </div>
            )}
            {placed.gone.length > 0 && (
              <div className="cmt-gone">
                <p className="muted small">Comments on files that are no longer changed</p>
                <Thread comments={placed.gone} editor={editor} draft={draftText} changed={placed.changed} actions={actions} away withPath />
              </div>
            )}
            {data.commit ? (
              <p className="muted small chg-hint">
                One commit’s changes. Comments go on <b>All changes</b>, where the after side is the worktree.
              </p>
            ) : touch ? (
              <p className="muted small chg-hint">
                Use <b>Files</b> to move between files. Tap <b>+</b> beside a line to comment on it; hold it and drag, or tap <b>+</b>
                on another line before you type, to cover a range.
              </p>
            ) : (
              <p className="muted small chg-hint">
                Use <b>Files</b> or press <kbd>[</kbd> and <kbd>]</kbd> to move between files. Click <b>+</b> beside a line to
                comment on it; drag it, or shift-click another line, to cover a range.
              </p>
            )}
            {files.map((f, n) => {
              const lines = parsed.get(f.path) ?? null;
              // A comment's editor is passed only to its own file, and only in All changes (a commit's shows above).
              const ed = !data.commit && editor?.path === f.path ? editor : null;
              const outdated = placed.outdated.get(f.path);
              return (
                <FileSection
                  key={f.path}
                  file={f}
                  n={n}
                  total={files.length}
                  open={isOpen(f.path)}
                  lines={lines}
                  limit={rowLimit(f.path, lines?.length ?? 0)}
                  page={budget.page}
                  comments={countByPath.get(f.path) ?? 0}
                  threads={placed.at.get(f.path)}
                  outdated={outdated}
                  changed={outdated ? placed.changed : undefined}
                  editor={ed}
                  editorAt={ed && !ed.id ? editorAt : null}
                  readOnly={!!data.commit}
                  imageUrl={imageUrl}
                  imageVersion={f.binary ? fetchedAt : 0}
                  draft={draftText}
                  actions={actions}
                />
              );
            })}
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

type CommitItem = BranchCommit | 'all';

/** The header's scope: everything vs. the base, or one of the branch's commits alone (newest first). */
function CommitMenu({ commits, current, onPick }: { commits: BranchCommit[]; current: BranchCommit | null; onPick: (sha: string | null) => void }) {
  const items: CommitItem[] = ['all', ...commits];
  return (
    <FilterMenu<CommitItem>
      label={<>{current ? 'Commit' : 'All changes'} ▾</>}
      title="Show all changes, or one commit"
      name="Commits"
      listName="Commits"
      placeholder="Filter commits"
      align="left"
      items={items}
      itemKey={(c) => (c === 'all' ? 'all' : c.sha)}
      matches={(c, needle) => c === 'all' || `${c.sha} ${c.subject}`.toLowerCase().includes(needle)}
      none={(q) => `No commit matches “${q}”.`}
      onChoose={(c) => onPick(c === 'all' ? null : c.sha)}
      renderItem={(c) =>
        c === 'all' ? (
          <span className={cx('chg-path', !current && 'cur')}>All changes, uncommitted included</span>
        ) : (
          <span className={cx('chg-commit', current?.sha === c.sha && 'cur')}>
            <span className="mono">{c.sha.slice(0, 7)}</span> <span className="chg-path">{c.subject}</span>{' '}
            <span className="muted small">{relativeTime(c.at)}</span>
          </span>
        )
      }
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
 * outside to close. `items` null while they load. The filter takes focus on opening, except on touch.
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
  // On touch, focusing the filter would bring up the keyboard over the list; a tap on it does that.
  const touch = useTouch();

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
            autoFocus={!touch}
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

/**
 * A path that wraps between folders, not in a name: each folder and the name are kept whole (a
 * name's '-' is a break point too), with a break point after each '/', and the folders muted.
 */
function PathText({ path }: { path: string }) {
  const dirs = path.split('/');
  const name = dirs.pop();
  return (
    <>
      {dirs.length > 0 && (
        <span className="chg-dir">
          {dirs.map((d, i) => (
            <Fragment key={i}>
              <span className="chg-seg">{d}/</span>
              <wbr />
            </Fragment>
          ))}
        </span>
      )}
      <span className="chg-seg chg-name">{name}</span>
    </>
  );
}

/** A changed file's whole path (a rename's both), wrapped rather than cut so the name always shows. */
export function FilePath({ file }: { file: ChangedFile }) {
  return (
    <span className="chg-path chg-file-path mono" title={file.oldPath ? `${file.oldPath} → ${file.path}` : file.path}>
      {file.oldPath ? (
        <>
          <PathText path={file.oldPath} /> → <PathText path={file.path} />
        </>
      ) : (
        <PathText path={file.path} />
      )}
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

/** Comment cards, the one being edited as its editor: `away` for comments not placed on a row, `row` for the row the others follow. */
function Thread({
  comments,
  editor,
  draft,
  changed,
  actions,
  away,
  withPath,
  row,
}: {
  comments: ReviewComment[];
  editor: Editor | null;
  draft: RefObject<string>;
  /** Comments whose lines the diff still shows, with other text (for `away` cards). */
  changed?: Set<string>;
  actions: SectionActions;
  away?: boolean;
  withPath?: boolean;
  row?: DiffLine;
}) {
  return comments.map((c) =>
    editor?.id === c.id ? (
      <CommentEditor key={c.id} editor={editor} draft={draft} onSave={actions.save} onCancel={actions.cancel} />
    ) : (
      <CommentCard
        key={c.id}
        c={c}
        away={away ? (changed?.has(c.id) ? 'changed' : 'hidden') : undefined}
        shownAt={row ? sideLine(row, c.side) : null}
        withPath={withPath}
        onEdit={() => actions.edit(c)}
        onDelete={() => actions.remove(c)}
      />
    ),
  );
}

function CommentEditor({
  editor,
  draft,
  onSave,
  onCancel,
}: {
  editor: Editor;
  draft: RefObject<string>;
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

/**
 * One changed file: its header, outdated comments and diff. Memoized, and given only its own
 * file's data, so a state push or a refetch that leaves the file alone doesn't draw its rows again.
 */
const FileSection = memo(function FileSection({
  file,
  n,
  total,
  open,
  lines,
  limit,
  page,
  comments,
  threads,
  outdated,
  changed,
  editor,
  editorAt,
  readOnly,
  imageUrl,
  imageVersion,
  draft,
  actions,
}: {
  file: ChangedFile;
  /** Its place in the list, from 0, and the number of files. */
  n: number;
  total: number;
  open: boolean;
  lines: DiffLine[] | null;
  limit: number;
  page: number;
  /** Its comments not sent yet. */
  comments: number;
  /** Comments placed after its rows, by row. */
  threads: Map<number, ReviewComment[]> | undefined;
  /** Its comments whose lines aren't in the diff now. */
  outdated: ReviewComment[] | undefined;
  changed: Set<string> | undefined;
  /** The comment editor, when it's on this file. */
  editor: Editor | null;
  /** The row a new comment's editor sits under. */
  editorAt: number | null;
  /** A commit is shown: no comments. */
  readOnly: boolean;
  imageUrl: (side: ChangeSide, path: string, v: number) => string;
  /** When an image's previews were last fetched (they reload with it); 0 for text files, so a refetch leaves them alone. */
  imageVersion: number;
  draft: RefObject<string>;
  actions: SectionActions;
}) {
  const path = file.path;
  // Rows it draws, for the height the browser assumes before drawing it (styles.css .chg-file).
  const rows = !open ? 0 : file.binary ? 12 : !lines ? 2 : limit + (limit < lines.length ? 2 : 0);
  return (
    <section className="chg-file" style={{ '--chg-rows': rows } as CSSProperties} ref={(el) => actions.section(path, el)}>
      <button className="chg-file-hd" aria-expanded={open} onClick={() => actions.toggle(path)}>
        <span className="chg-caret" aria-hidden="true">
          {open ? '▾' : '▸'}
        </span>
        <KindBadge kind={file.kind} />
        <FilePath file={file} />
        <span className="chg-pos mono" aria-label={`file ${n + 1} of ${total}`}>
          {n + 1} / {total}
        </span>
        <Counts file={file} comments={comments} />
      </button>
      {open && (
        <>
          {outdated && (
            <div className="cmt-outdated">
              <Thread comments={outdated} editor={editor} draft={draft} changed={changed} actions={actions} away />
            </div>
          )}
          <FileDiff
            file={file}
            imageUrl={(side, p) => imageUrl(side, p, imageVersion)}
            lines={lines}
            limit={limit}
            page={page}
            onMore={() => actions.more(path, lines?.length ?? 0)}
            renderAfter={(i) => {
              const cs = threads?.get(i);
              const here = editor !== null && !editor.id && editorAt === i;
              if (!cs && !here) return null;
              return (
                <>
                  {cs && <Thread comments={cs} editor={editor} draft={draft} actions={actions} row={lines?.[i]} />}
                  {here && <CommentEditor key="new" editor={editor} draft={draft} onSave={actions.save} onCancel={actions.cancel} />}
                </>
              );
            }}
            picked={editor && !editor.id ? editor : null}
            onComment={readOnly ? undefined : (i, pick) => actions.comment(path, i, pick)}
          />
        </>
      )}
    </section>
  );
});

export function FileDiff({
  file,
  imageUrl,
  lines,
  limit,
  page,
  onMore,
  renderAfter,
  picked,
  onComment,
}: {
  file: ChangedFile;
  imageUrl: (side: ChangeSide, path: string) => string;
  lines: DiffLine[] | null;
  /** How many of the rows to render; a large diff renders none until Show diff. */
  limit: number;
  /** Rows Show diff and Show more add. */
  page: number;
  /** Shows the next `page` rows. */
  onMore: () => void;
  /** Comment threads (or the editor) shown under row i, or null. */
  renderAfter: (i: number) => ReactNode;
  /** The range a new comment is being written for. */
  picked: { side: CommentSide; line: number; endLine: number } | null;
  /** Undefined when the diff takes no comments. */
  onComment?: (i: number, pick: CommentPick) => void;
}) {
  const drag = useRangeDrag(lines, onComment);
  if (file.binary) {
    return previewImageType(file.path) ? <ImageDiff file={file} imageUrl={imageUrl} /> : <p className="chg-note">Binary file, not shown.</p>;
  }
  if (!lines) return <p className="chg-note">This diff is too large to show here.</p>;
  if (lines.length === 0) return <p className="chg-note">{file.kind === 'untracked' ? 'Empty file.' : 'No content changes.'}</p>;
  const left = lines.length - limit;
  /** The range being dragged, else the one a new comment is being written for. */
  const shown = drag.range ?? picked;
  /** Lines of the file among rows [from, to), for the labels: hunk headers and notes aren't lines. */
  const fileLines = (from: number, to = lines.length) => lines.slice(from, to).filter((l) => l.kind === 'add' || l.kind === 'del' || l.kind === 'ctx').length;
  // Comments on rows not rendered yet go under the button that shows them.
  const later: ReactNode[] = [];
  for (let i = limit; i < lines.length; i++) {
    const after = renderAfter(i);
    if (after) later.push(<Fragment key={i}>{after}</Fragment>);
  }
  return (
    <div className="diff-wrap">
      {limit > 0 && (
        <table className={cx('diff', drag.range && 'dragging')} ref={drag.tableRef}>
          <tbody>
            {lines.slice(0, limit).map((l, i) => {
              const side: CommentSide = l.kind === 'del' ? 'old' : 'new';
              const n = sideLine(l, side);
              const inPick = shown !== null && n !== null && shown.side === side && n >= shown.line && n <= shown.endLine;
              const after = renderAfter(i);
              return (
                <Fragment key={i}>
                  <tr className={cx(`dl-${l.kind}`, inPick && 'picked')} data-i={i}>
                    <td className="cg">
                      {n !== null && onComment && (
                        <button
                          className="cmt-add"
                          tabIndex={-1}
                          title="Comment on this line (drag or shift-click to cover a range)"
                          aria-label={`Comment on ${linesLabel(side, n, n)}`}
                          onPointerDown={(e) => drag.start(e, i)}
                          onContextMenu={drag.menu}
                          onClick={(e) => {
                            if (!drag.swallow()) onComment(i, { shift: e.shiftKey });
                          }}
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
      )}
      {left > 0 && (
        <div className="chg-more">
          {limit === 0 ? (
            <>
              <span>Large diff: {plural(fileLines(0), 'line')}.</span>
              <button className="btn small" onClick={onMore}>Show diff</button>
            </>
          ) : (
            <button className="btn small" onClick={onMore}>
              {left > page
                ? `Show ${fileLines(limit, limit + page).toLocaleString()} more lines (${fileLines(limit).toLocaleString()} left)`
                : `Show the last ${plural(fileLines(limit), 'line')}`}
            </button>
          )}
        </div>
      )}
      {later.length > 0 && <div className="cmt-thread chg-later">{later}</div>}
      {file.truncated && <p className="chg-note">Diff cut short: this file’s diff is larger than 256 KB.</p>}
    </div>
  );
}
