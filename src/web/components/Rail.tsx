import { useEffect, useState } from 'react';
import type { LimitWindow, PlanLimits, ProjectWithSessions } from '../../shared/types';
import { useIsMobile, useTouch } from '../hooks/useMedia';
import type { ThemePref } from '../theme';
import { aggregateDot, cx, formatTokens, isMac, liveSessions, relativeTime, repoFolder, totalTokens } from '../util';
import { StatusDot } from './common';

export function Rail({
  projects,
  currentId,
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
  onNewProject,
  onSettings,
  onLogout,
  onReorder,
  onPalette,
  open,
}: {
  projects: ProjectWithSessions[];
  currentId: string | null;
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
  onNewProject: () => void;
  onSettings: () => void;
  /** Signs out; left out when there's no login (local access). */
  onLogout?: () => void;
  /** Moves a project to this index in the list. Touch screens use Move up/down on the project home. */
  onReorder: (id: string, index: number) => void;
  onPalette: () => void;
  /** Phone layout: the rail is a drawer, shown while open. */
  open: boolean;
}) {
  const mobile = useIsMobile();
  const touch = useTouch();
  const [dragId, setDragId] = useState<string | null>(null);
  const [overId, setOverId] = useState<string | null>(null);
  const mod = isMac() ? '⌘' : 'Ctrl+';
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
    <nav className={cx('rail', open && 'open')} aria-label="Projects" inert={mobile && !open}>
      {/* Looks like a field but is a button: it opens the palette over itself, whose input takes the typing.
          It's named by its visible text, so speech input can say what it sees; the chip stays out of the name. */}
      <button className="rail-search" onClick={onPalette} aria-haspopup="dialog" aria-keyshortcuts={isMac() ? 'Meta+K' : 'Control+K'}>
        <img src="/logo.png" alt="" width={16} height={16} />
        <span className="rail-search-text">Search AgentK Local</span>
        {!touch && <kbd aria-hidden="true">{mod}K</kbd>}
      </button>
      <div className="rail-list">
        {projects.map((p, i) => {
          const count = liveSessions(p).length;
          return (
            <button
              key={p.id}
              className={cx('proj', p.id === currentId && 'sel', overId === p.id && dragId !== p.id && (dropAfter(p.id) ? 'drop-after' : 'drop-target'))}
              onClick={() => onSelect(p)}
              aria-current={p.id === currentId ? 'page' : undefined}
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
