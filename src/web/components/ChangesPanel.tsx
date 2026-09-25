import { useEffect, useMemo, useRef, useState } from 'react';
import type { ChangedFile, ChangeKind, Session, SessionChanges } from '../../shared/types';
import { api, errorMessage } from '../api';
import { parsePatch, type DiffLine } from '../diff';
import { cx } from '../util';
import { SidePanel } from './SidePanel';

const KIND: Record<ChangeKind, { letter: string; label: string }> = {
  added: { letter: 'A', label: 'added' },
  modified: { letter: 'M', label: 'modified' },
  deleted: { letter: 'D', label: 'deleted' },
  renamed: { letter: 'R', label: 'renamed' },
  untracked: { letter: 'U', label: 'untracked (not added to git yet)' },
};

/** Diffs start collapsed once this many lines are already shown, so big branches open fast. */
const OPEN_LINE_BUDGET = 2000;

export function ChangesPanel({ session, onClose }: { session: Session; onClose: () => void }) {
  const [data, setData] = useState<SessionChanges | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [tick, setTick] = useState(0);
  /** Files the user opened or closed; the rest follow the line budget. */
  const [toggled, setToggled] = useState<Map<string, boolean>>(new Map());
  const sections = useRef(new Map<string, HTMLElement>());

  // Refetch when the footer's numbers move (the server refreshes them periodically) or on Refresh.
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api
      .changes(session.id)
      .then((d) => !cancelled && (setData(d), setError(null)))
      .catch((e) => !cancelled && setError(errorMessage(e)))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [session.id, session.diffStat?.added, session.diffStat?.removed, tick]);

  const parsed = useMemo(() => {
    const m = new Map<string, DiffLine[]>();
    for (const f of data?.files ?? []) if (f.patch !== null) m.set(f.path, parsePatch(f.patch));
    return m;
  }, [data]);

  const openByDefault = useMemo(() => {
    const open = new Set<string>();
    let shown = 0;
    for (const f of data?.files ?? []) {
      const n = parsed.get(f.path)?.length ?? 0;
      if (shown < OPEN_LINE_BUDGET) open.add(f.path);
      shown += n;
    }
    return open;
  }, [data, parsed]);

  const isOpen = (path: string) => toggled.get(path) ?? openByDefault.has(path);
  const setOpen = (path: string, open: boolean) => setToggled((t) => new Map(t).set(path, open));

  const jump = (path: string) => {
    setOpen(path, true);
    requestAnimationFrame(() => sections.current.get(path)?.scrollIntoView({ block: 'start' }));
  };

  const files = data?.files ?? [];

  return (
    <SidePanel
      name="changes"
      label="Changes"
      className="design-panel changes-panel"
      onKeyDown={(e) => {
        if (e.key === 'Escape') onClose();
      }}
    >
      <div className="design-hd">
        <b>Changes</b>
        {data && (
          <span className="muted small">
            vs <span className="mono">{data.base}</span> · {files.length} {files.length === 1 ? 'file' : 'files'} ·{' '}
            <span className="add">+{data.added}</span> <span className="del">−{data.removed}</span>
          </span>
        )}
        <span className="sp" />
        <button className="btn small" disabled={loading} onClick={() => setTick((t) => t + 1)}>
          {loading ? 'Loading…' : 'Refresh'}
        </button>
        <button className="icon-btn" onClick={onClose} aria-label="Close changes">
          ×
        </button>
      </div>
      <div className="design-body changes-body">
        {error ? (
          <p className="error-text">{error}</p>
        ) : !data ? (
          <p className="muted">Loading…</p>
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
            <ul className="chg-list">
              {files.map((f) => (
                <li key={f.path}>
                  <button className="chg-row" onClick={() => jump(f.path)}>
                    <KindBadge kind={f.kind} />
                    <FilePath file={f} />
                    <Counts file={f} />
                  </button>
                </li>
              ))}
            </ul>
            {files.map((f) => (
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
                  <Counts file={f} />
                </button>
                {isOpen(f.path) && <FileDiff file={f} lines={parsed.get(f.path) ?? null} />}
              </section>
            ))}
          </>
        )}
      </div>
    </SidePanel>
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

function Counts({ file }: { file: ChangedFile }) {
  return (
    <span className="chg-counts mono">
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

function FileDiff({ file, lines }: { file: ChangedFile; lines: DiffLine[] | null }) {
  if (file.binary) return <p className="chg-note">Binary file, not shown.</p>;
  if (!lines) return <p className="chg-note">This diff is too large to show here.</p>;
  if (lines.length === 0) return <p className="chg-note">{file.kind === 'untracked' ? 'Empty file.' : 'No content changes.'}</p>;
  return (
    <div className="diff-wrap">
      <table className="diff">
        <tbody>
          {lines.map((l, i) => (
            <tr key={i} className={`dl-${l.kind}`}>
              <td className="ln">{l.old ?? ''}</td>
              <td className="ln">{l.new ?? ''}</td>
              <td className="sign">{l.kind === 'add' ? '+' : l.kind === 'del' ? '−' : ''}</td>
              <td className="code">{l.text}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {file.truncated && <p className="chg-note">Diff cut short: this file’s diff is larger than 256 KB.</p>}
    </div>
  );
}
