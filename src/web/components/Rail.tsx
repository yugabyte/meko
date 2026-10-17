import { Fragment, useEffect, useRef, useState, type KeyboardEvent, type PointerEvent, type RefObject } from 'react';
import { STAGE_LABELS, type LimitWindow, type PlanLimits, type ProjectWithSessions, type Session } from '../../shared/types';
import { useIsMobile, useTouch } from '../hooks/useMedia';
import { RAIL_DEFAULT, clampRailWidth, railCollapsedSaved, saveRailCollapsed } from '../rail';
import type { ThemePref } from '../theme';
import { aggregateDot, cx, dotState, formatTokens, isMac, openSessions, relativeTime, repoFolder, sessionTabTitle, totalTokens } from '../util';
import { HarnessLogo, StatusDot } from './common';

export function Rail({
  projects,
  currentId,
  currentSessionId,
  usageOpen,
  onUsage,
  username,
  serverName,
  serverHost,
  version,
  limits,
  theme,
  onCycleTheme,
  onSelect,
  sessionsShown,
  harnessLogos,
  onSelectSession,
  onNewSession,
  onRenameSession,
  onNewProject,
  onSettings,
  onLogout,
  onReorder,
  onPalette,
  onResize,
  onHide,
  open,
}: {
  projects: ProjectWithSessions[];
  currentId: string | null;
  /** The session on screen, highlighted under its project. */
  currentSessionId: string | null;
  /** The Usage page is showing. */
  usageOpen: boolean;
  onUsage: () => void;
  username: string;
  /** Shown after the user name (`hari@dev-box`); empty from a server too old to send it. */
  serverName: string;
  /** The machine's full host name, added to the tooltip when it isn't what `serverName` shows. */
  serverHost: string;
  version: string;
  limits: PlanLimits | null;
  theme: ThemePref;
  onCycleTheme: () => void;
  onSelect: (p: ProjectWithSessions) => void;
  /** List each project's sessions under it (the Settings toggle); off, the rail lists projects only. */
  sessionsShown: boolean;
  /** Show each listed session's harness logo: open sessions run on more than one harness. */
  harnessLogos: boolean;
  onSelectSession: (s: Session) => void;
  /** New session in this project, offered at the end of each listed project's sessions. */
  onNewSession: (projectId: string) => void;
  /** Double-clicking a session renames it; the phone layout uses Rename session in the ⋯ sheet. */
  onRenameSession: (sessionId: string) => void;
  onNewProject: () => void;
  onSettings: () => void;
  /** Signs out; left out when there's no login (local access). */
  onLogout?: () => void;
  /** Moves a project to this index in the list. Touch screens use Move up/down on the project home. */
  onReorder: (id: string, index: number) => void;
  onPalette: () => void;
  /** Sets the pane's width, null for the default (the wide layout). */
  onResize?: (width: number | null) => void;
  /** Hides the pane (the wide layout); left out on a phone, where the rail is a drawer. */
  onHide?: () => void;
  /** Phone layout: the rail is a drawer, shown while open. */
  open: boolean;
}) {
  const mobile = useIsMobile();
  const touch = useTouch();
  const [dragId, setDragId] = useState<string | null>(null);
  const [overId, setOverId] = useState<string | null>(null);
  const navRef = useRef<HTMLElement>(null);
  const [resizing, setResizing] = useState(false);
  const mod = isMac() ? '⌘' : 'Ctrl+';
  const listRef = useRef<HTMLDivElement>(null);
  const [collapsed, setCollapsed] = useState(railCollapsedSaved);
  const fold = (id: string) => {
    const next = new Set(collapsed);
    if (!next.delete(id)) next.add(id);
    // Projects closed since are dropped, so the list doesn't grow forever.
    for (const c of next) if (!projects.some((p) => p.id === c)) next.delete(c);
    setCollapsed(next);
    saveRailCollapsed(next);
  };
  useEffect(() => {
    listRef.current?.querySelector('.rail-sess.sel')?.scrollIntoView({ block: 'nearest' });
  }, [currentSessionId]);
  const who = [
    onLogout ? `Signed in as ${username}` : username,
    serverName && ` on ${serverName}`,
    serverName && serverHost && serverHost !== serverName && ` (${serverHost})`,
  ]
    .filter(Boolean)
    .join('');

  // The dragged project takes the target's place: before it when moving up, after it when moving down.
  const drop = (targetId: string) => {
    if (!dragId || dragId === targetId) return;
    const to = projects.findIndex((p) => p.id === targetId);
    if (to >= 0) onReorder(dragId, to);
  };
  // Where the dragged project would land, for the drop line: below the target when moving down.
  const dropAfter = (targetId: string) => {
    const ids = projects.map((p) => p.id);
    return !!dragId && ids.indexOf(dragId) < ids.indexOf(targetId);
  };

  return (
    <nav ref={navRef} className={cx('rail', open && 'open', resizing && 'resizing')} aria-label="Projects" inert={mobile && !open}>
      {onResize && <RailResizer nav={navRef} onResize={onResize} onDragging={setResizing} />}
      {/* Looks like a field but is a button: it opens the palette over itself, whose input takes the typing.
          It's named by its visible text, so speech input can say what it sees; the chip stays out of the name. */}
      <button className="rail-search" onClick={onPalette} aria-haspopup="dialog" aria-keyshortcuts={isMac() ? 'Meta+K' : 'Control+K'}>
        <img src="/logo.png" alt="" width={16} height={16} />
        <span className="rail-search-text">Ask AgentK</span>
        {!touch && <kbd aria-hidden="true">{mod}K</kbd>}
      </button>
      <div className="rail-list" ref={listRef}>
        {projects.map((p, i) => {
          const sessions = openSessions(p);
          const count = sessions.length;
          const current = p.id === currentId;
          const listed = sessionsShown && (count > 0 || current);
          const folded = listed && collapsed.has(p.id);
          // The project row is highlighted only when none of its sessions is: one highlight at a time.
          const sessionShown = current && listed && !folded && sessions.some((s) => s.id === currentSessionId);
          return (
            <Fragment key={p.id}>
              <div className={cx('proj-row', listed && 'foldable')}>
                <button
                  className={cx('proj', current && (sessionShown ? 'cur' : 'sel'), overId === p.id && dragId !== p.id && (dropAfter(p.id) ? 'drop-after' : 'drop-target'))}
                  onClick={() => onSelect(p)}
                  aria-current={current && !sessionShown ? 'page' : undefined}
                  draggable={!touch}
                  onDragStart={(e) => {
                    setDragId(p.id);
                    e.dataTransfer.effectAllowed = 'move';
                  }}
                  onDragOver={(e) => {
                    e.preventDefault();
                    setOverId(p.id);
                  }}
                  onDragLeave={() => setOverId((o) => (o === p.id ? null : o))}
                  onDrop={(e) => {
                    e.preventDefault();
                    drop(p.id);
                    setDragId(null);
                    setOverId(null);
                  }}
                  onDragEnd={() => {
                    setDragId(null);
                    setOverId(null);
                  }}
                  title={`${p.name}${i < 9 ? ` (${mod}${i + 1})` : ''} · ${p.repoPath} · ${formatTokens(totalTokens(p.usage))} tokens used`}
                >
                  {count > 0 ? <StatusDot status={aggregateDot(p)} phase={p.id} /> : <i className="dot idle" />}
                  <span className="proj-text">
                    <span className="proj-name">{p.name}</span>
                    <span className="proj-repo">{repoFolder(p.repoPath)}</span>
                  </span>
                  <span className="n">{count}</span>
                </button>
                {listed && (
                  <button
                    className="proj-fold"
                    onClick={() => fold(p.id)}
                    aria-expanded={!folded}
                    aria-controls={`rail-sessions-${p.id}`}
                    aria-label={`${folded ? 'Show' : 'Hide'} the sessions in ${p.name}`}
                    title={folded ? 'Show sessions' : 'Hide sessions'}
                  >
                    <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                      <path d={folded ? 'M9 6l6 6-6 6' : 'M6 9l6 6 6-6'} />
                    </svg>
                  </button>
                )}
              </div>
              {listed && !folded && (
                <div className={cx('rail-sessions', harnessLogos && 'logos')} id={`rail-sessions-${p.id}`} role="group" aria-label={`Sessions in ${p.name}`}>
                  {sessions.map((s) => (
                    <button
                      key={s.id}
                      className={cx('rail-sess', s.id === currentSessionId && 'sel')}
                      onClick={() => onSelectSession(s)}
                      onDoubleClick={mobile ? undefined : () => onRenameSession(s.id)}
                      aria-current={s.id === currentSessionId ? 'page' : undefined}
                      title={sessionTabTitle(s) + (mobile ? '' : '\nDouble-click to rename')}
                    >
                      <StatusDot status={dotState(s)} phase={s.id} />
                      {harnessLogos && <HarnessLogo harness={s.harness} />}
                      <span className="rail-sess-name">{s.name}</span>
                      <span className="stchip">{STAGE_LABELS[s.stage]}</span>
                    </button>
                  ))}
                  <button className="rail-sess rail-sess-new" onClick={() => onNewSession(p.id)}>
                    + New session
                  </button>
                </div>
              )}
            </Fragment>
          );
        })}
        <button className="rail-add" onClick={onNewProject}>
          + Open project
        </button>
        <button
          className={cx('rail-add', 'rail-usage', usageOpen && 'sel')}
          onClick={onUsage}
          aria-current={usageOpen ? 'page' : undefined}
          title="Tokens, merged PRs and pushed commits across every project"
        >
          <Icon d="M4 20V10 M10 20V4 M16 20v-7 M22 20H2" />
          Usage
        </button>
      </div>
      <div className="rail-foot">
        <Limits limits={limits} />
        <div className="rail-actions">
          <button className="icon-btn" onClick={onSettings} title="Settings" aria-label="Settings">
            <Icon d="M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1Z" />
          </button>
          <button
            className="icon-btn"
            onClick={onCycleTheme}
            title={`Theme: ${theme}. Click to change.`}
            aria-label={`Theme: ${theme}`}
          >
            {theme === 'dark' ? (
              <Icon d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z" />
            ) : theme === 'light' ? (
              <Icon d="M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10Z M12 1v2 M12 21v2 M4.2 4.2l1.4 1.4 M18.4 18.4l1.4 1.4 M1 12h2 M21 12h2 M4.2 19.8l1.4-1.4 M18.4 5.6l1.4-1.4" />
            ) : (
              <Icon d="M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z M12 3v18" />
            )}
          </button>
          {onHide && (
            <button className="icon-btn rail-hide" onClick={onHide} title="Hide projects" aria-label="Hide projects" aria-expanded={true}>
              <PaneIcon />
            </button>
          )}
          {onLogout && (
            <button className="icon-btn" onClick={onLogout} title="Sign out" aria-label="Sign out">
              <Icon d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4 M16 17l5-5-5-5 M21 12H9" />
            </button>
          )}
        </div>
        <div className="me">
          <span className="me-who" title={who}>
            <span className="me-user">{username}</span>
            {serverName && <span className="me-host">@{serverName}</span>}
          </span>
          <span className="mono">v{version}</span>
        </div>
      </div>
    </nav>
  );
}

/** When a window resets: a time today, else a weekday and time, else a date. */
function resetLabel(ms: number, now: number): string {
  const d = new Date(ms);
  const time = d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  if (d.toDateString() === new Date(now).toDateString()) return time;
  if (ms - now < 6 * 86_400_000) return `${d.toLocaleDateString([], { weekday: 'short' })} ${time}`;
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
}

/** The account's 5-hour and weekly plan limits, from the newest status line report of any session. */
function Limits({ limits }: { limits: PlanLimits | null }) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(t);
  }, []);
  if (!limits) return null;
  // A window whose reset time has passed no longer applies, even before the server says so.
  const rows = (
    [
      ['5-hour', limits.fiveHour],
      ['Weekly', limits.sevenDay],
    ] as [string, LimitWindow | null][]
  ).filter((r): r is [string, LimitWindow] => !!r[1] && r[1].resetsAt > now);
  if (!rows.length) return null;
  return (
    <div className="limits" aria-label="Plan usage limits">
      {rows.map(([label, w]) => {
        const pct = Math.round(w.usedPercent);
        const level = pct >= 95 ? 'bad' : pct >= 80 ? 'warn' : null;
        const reset = resetLabel(w.resetsAt, now);
        return (
          <div key={label} className={cx('lim', level)} title={`${label} limit: ${pct}% used, resets ${new Date(w.resetsAt).toLocaleString()}`}>
            <div className="lim-hd">
              <span>{label}</span>
              <span className="lim-pct">
                {level && <span aria-hidden="true">▲ </span>}
                {pct}%
              </span>
            </div>
            <div className="lim-bar" role="meter" aria-label={`${label} limit used`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct}>
              <i style={{ width: `${Math.max(pct, 2)}%` }} />
            </div>
            <div className="lim-reset">Resets {reset}</div>
          </div>
        );
      })}
      <div className="lim-age">As of {relativeTime(limits.updatedAt, now).toLowerCase()}</div>
    </div>
  );
}

/** 18px stroke icon from one or more path segments. */
function Icon({ d }: { d: string }) {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d={d} />
    </svg>
  );
}

/** A pane beside a page: the projects pane's hide and show buttons. */
export function PaneIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M5 4h14a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2Z M9 4v16" />
    </svg>
  );
}

const STEP = 20;

/** The pane's right edge: drag it, or use the arrow keys on it; a double-click goes back to the default. */
function RailResizer({
  nav,
  onResize,
  onDragging,
}: {
  nav: RefObject<HTMLElement | null>;
  onResize: (width: number | null) => void;
  onDragging: (on: boolean) => void;
}) {
  const [dragging, setDragging] = useState(false);
  const width = () => nav.current?.getBoundingClientRect().width ?? RAIL_DEFAULT;
  const drag = (on: boolean) => {
    setDragging(on);
    onDragging(on);
  };
  const down = (e: PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return;
    e.preventDefault();
    e.currentTarget.setPointerCapture(e.pointerId);
    drag(true);
  };
  const move = (e: PointerEvent<HTMLDivElement>) => {
    if (!dragging || !nav.current) return;
    onResize(clampRailWidth(e.clientX - nav.current.getBoundingClientRect().left));
  };
  const up = (e: PointerEvent<HTMLDivElement>) => {
    if (e.currentTarget.hasPointerCapture(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId);
    drag(false);
  };
  const keys = (e: KeyboardEvent<HTMLDivElement>) => {
    const d = e.key === 'ArrowRight' ? STEP : e.key === 'ArrowLeft' ? -STEP : 0;
    if (!d) return;
    e.preventDefault();
    onResize(clampRailWidth(width() + d));
  };
  return (
    <div
      className="panel-resizer rail-resizer"
      role="separator"
      aria-orientation="vertical"
      aria-label="Resize the projects pane"
      title="Drag to resize. Double-click for the default width."
      tabIndex={0}
      onPointerDown={down}
      onPointerMove={move}
      onPointerUp={up}
      onPointerCancel={up}
      onDoubleClick={() => onResize(null)}
      onKeyDown={keys}
    />
  );
}
