import { useEffect, useMemo, useRef, useState } from 'react';
import { STAGE_LABELS, type ProjectWithSessions, type Session } from '../../shared/types';
import { moveBlockedIn } from '../../shared/stages';
import { filterItems, type LabelPart } from '../palette';
import { navigate } from '../router';
import { STATUS_TEXT, cx, liveSessions } from '../util';
import { StatusDot, trapTab } from './common';

interface Item {
  id: string;
  /** Session and project names are parts of their own, so they don't read as commands. */
  label: LabelPart[];
  hint: string;
  keywords: string;
  run: () => void;
  dot?: ProjectWithSessions['sessions'][number]['status'];
}

export function CommandPalette({
  projects,
  currentProjectId,
  currentSession,
  onClose,
  onNewProject,
  onNewSession,
  onMoveSession,
  onOpenPr,
  onRenameSession,
  onSettings,
}: {
  projects: ProjectWithSessions[];
  currentProjectId: string | null;
  /** The session on screen, if any. */
  currentSession: Session | null;
  onClose: () => void;
  onNewProject: () => void;
  onNewSession: (projectId: string) => void;
  /** Opens the Move to another stage dialog for the session on screen. */
  onMoveSession: () => void;
  /** Opens the Open PR with instructions dialog for the session on screen. */
  onOpenPr: () => void;
  /** Opens the Rename session dialog for the session on screen. */
  onRenameSession: () => void;
  onSettings: () => void;
}) {
  const [q, setQ] = useState('');
  const [idx, setIdx] = useState(0);
  const listRef = useRef<HTMLDivElement>(null);
  const paletteRef = useRef<HTMLDivElement>(null);

  const items = useMemo<Item[]>(() => {
    const out: Item[] = [];
    const current = projects.find((p) => p.id === currentProjectId);
    if (current) {
      out.push({
        id: 'new-session',
        label: ['New session in ', { project: current.name }],
        hint: 'Action',
        keywords: 'new session task start',
        run: () => onNewSession(current.id),
      });
    }
    if (currentSession && !moveBlockedIn(currentSession.stage)) {
      out.push({
        id: 'move-session',
        label: ['Move ', { session: currentSession.name }, ' to another stage'],
        hint: 'Action',
        keywords: 'change type stage move exploration explore plan quick code design pr review',
        run: onMoveSession,
      });
    }
    if (currentSession?.stage === 'code' && !currentSession.queued) {
      out.push({
        id: 'open-pr',
        label: ['Open PR for ', { session: currentSession.name }, ' with instructions'],
        hint: 'Action',
        keywords: 'open pr pull request instructions notes draft',
        run: onOpenPr,
      });
    }
    if (currentSession) {
      out.push({
        id: 'rename-session',
        label: ['Rename ', { session: currentSession.name }],
        hint: 'Action',
        keywords: 'rename name title session',
        run: onRenameSession,
      });
    }
    if (currentSession && current?.settings.mekoLog) {
      out.push({
        id: 'meko-calls',
        label: ['Meko calls of ', { session: currentSession.name }],
        hint: 'View',
        keywords: 'meko calls log debug memory search',
        run: () => navigate({ view: 'meko', projectId: current.id, sessionId: currentSession.id }),
      });
    }
    for (const p of projects) {
      for (const s of liveSessions(p)) {
        out.push({
          id: `s-${s.id}`,
          label: [{ session: s.name }],
          hint: `${p.name} · ${STAGE_LABELS[s.stage]} · ${STATUS_TEXT[s.status]}`,
          keywords: `${p.name} ${s.name} ${s.branch}`,
          dot: s.status,
          run: () => navigate({ view: 'project', projectId: p.id, sessionId: s.id }),
        });
      }
    }
    for (const p of projects) {
      out.push({
        id: `home-${p.id}`,
        label: [{ project: p.name }, ': project home'],
        hint: 'Datapacks, search, activity, settings',
        keywords: `${p.name} home datapack search settings`,
        run: () => navigate({ view: 'home', projectId: p.id }),
      });
      if (p.id !== currentProjectId) {
        out.push({
          id: `ns-${p.id}`,
          label: ['New session in ', { project: p.name }],
          hint: 'Action',
          keywords: `new session ${p.name}`,
          run: () => onNewSession(p.id),
        });
      }
    }
    out.push({ id: 'new-project', label: ['Open project'], hint: 'Action', keywords: 'open new create project repo add', run: onNewProject });
    out.push({ id: 'usage', label: ['Usage'], hint: 'View', keywords: 'usage tokens prs commits pushes projects', run: () => navigate({ view: 'usage' }) });
    out.push({ id: 'settings', label: ['Settings'], hint: 'Action', keywords: 'settings password update totp', run: onSettings });
    return out;
  }, [projects, currentProjectId, currentSession, onNewProject, onNewSession, onMoveSession, onOpenPr, onRenameSession, onSettings]);

  const shown = useMemo(() => filterItems(items, q), [items, q]);

  useEffect(() => setIdx(0), [q]);
  useEffect(() => {
    listRef.current?.querySelector('.pal-item.sel')?.scrollIntoView({ block: 'nearest' });
  }, [idx]);

  const choose = (it: Item | undefined) => {
    if (!it) return;
    onClose();
    it.run();
  };

  return (
    <div className="overlay top" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div
        className="palette"
        role="dialog"
        aria-label="Command palette"
        ref={paletteRef}
        onKeyDown={(e) => trapTab(e.nativeEvent, paletteRef.current)}
      >
        <input
          id="palette-input"
          className="pal-input"
          placeholder="Jump to a session, or type a command…"
          value={q}
          autoFocus
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown') {
              e.preventDefault();
              setIdx((i) => Math.min(shown.length - 1, i + 1));
            } else if (e.key === 'ArrowUp') {
              e.preventDefault();
              setIdx((i) => Math.max(0, i - 1));
            } else if (e.key === 'Enter') {
              e.preventDefault();
              choose(shown[idx]);
            } else if (e.key === 'Escape') {
              e.preventDefault();
              onClose();
            }
          }}
        />
        <div className="pal-list" ref={listRef} role="listbox">
          {shown.length === 0 && <p className="muted pad">Nothing matches.</p>}
          {shown.map((it, i) => (
            <button
              key={it.id}
              role="option"
              aria-selected={i === idx}
              className={cx('pal-item', i === idx && 'sel')}
              onMouseEnter={() => setIdx(i)}
              onClick={() => choose(it)}
            >
              {it.dot ? <StatusDot status={it.dot} /> : <span className="pal-icon">›</span>}
              <span className="pal-label">
                {it.label.map((part, j) =>
                  typeof part === 'string' ? (
                    part
                  ) : 'session' in part ? (
                    <span key={j} className="pal-session">{part.session}</span>
                  ) : (
                    <span key={j} className="pal-project">{part.project}</span>
                  ),
                )}
              </span>
              <span className="pal-hint">{it.hint}</span>
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
