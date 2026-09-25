import { useEffect, useState } from 'react';
import type { LimitWindow, PlanLimits, ProjectWithSessions } from '../../shared/types';
import { useIsMobile, useTouch } from '../hooks/useMedia';
import type { ThemePref } from '../theme';
import { aggregateStatus, cx, formatTokens, isMac, liveSessions, relativeTime, repoFolder, totalTokens } from '../util';
import { StatusDot } from './common';

export function Rail({
  projects,
  currentId,
  username,
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
  username: string;
  version: string;
  limits: PlanLimits | null;
  theme: ThemePref;
  onCycleTheme: () => void;
  onSelect: (p: ProjectWithSessions) => void;
  onNewProject: () => void;
  onSettings: () => void;
  onLogout: () => void;
  onReorder: (ids: string[]) => void;
  onPalette: () => void;
  /** Phone layout: the rail is a drawer, shown while open. */
  open: boolean;
}) {
  const mobile = useIsMobile();
  const touch = useTouch();
  const [dragId, setDragId] = useState<string | null>(null);
  const [overId, setOverId] = useState<string | null>(null);
  const mod = isMac() ? '⌘' : 'Ctrl+';

  const drop = (targetId: string) => {
    if (!dragId || dragId === targetId) return;
    const ids = projects.map((p) => p.id).filter((id) => id !== dragId);
    ids.splice(ids.indexOf(targetId), 0, dragId);
    onReorder(ids);
  };

  return (
    <nav className={cx('rail', open && 'open')} aria-label="Projects" inert={mobile && !open}>
      <div className="brand">
        <img src="/logo.png" alt="" width={26} height={26} />
        <span>AgentK Local</span>
      </div>
      <div className="rail-list">
        {projects.map((p, i) => {
          const count = liveSessions(p).length;
          return (
            <button
              key={p.id}
              className={cx('proj', p.id === currentId && 'sel', overId === p.id && dragId !== p.id && 'drop-target')}
              onClick={() => onSelect(p)}
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
              {count > 0 ? <StatusDot status={aggregateStatus(p)} /> : <i className="dot idle" />}
              <span className="proj-text">
                <span className="proj-name">{p.name}</span>
                <span className="proj-repo">{repoFolder(p.repoPath)}</span>
              </span>
              <span className="n">{count}</span>
            </button>
          );
        })}
        <button className="rail-add" onClick={onNewProject}>
          + New project
        </button>
        {mobile && (
          <button className="rail-add" onClick={onPalette}>
            Search and commands
          </button>
        )}
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
          <button className="icon-btn" onClick={onLogout} title="Sign out" aria-label="Sign out">
            <Icon d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4 M16 17l5-5-5-5 M21 12H9" />
          </button>
        </div>
        <div className="me">
          <span title={`Signed in as ${username}`}>{username}</span>
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
