import { useState } from 'react';
import type { ProjectWithSessions } from '../../shared/types';
import type { ThemePref } from '../theme';
import { aggregateStatus, cx, isMac, liveSessions } from '../util';
import { StatusDot } from './common';

export function Rail({
  projects,
  currentId,
  username,
  version,
  theme,
  onCycleTheme,
  onSelect,
  onNewProject,
  onSettings,
  onLogout,
  onReorder,
}: {
  projects: ProjectWithSessions[];
  currentId: string | null;
  username: string;
  version: string;
  theme: ThemePref;
  onCycleTheme: () => void;
  onSelect: (p: ProjectWithSessions) => void;
  onNewProject: () => void;
  onSettings: () => void;
  onLogout: () => void;
  onReorder: (ids: string[]) => void;
}) {
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
    <nav className="rail" aria-label="Projects">
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
              draggable
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
              title={i < 9 ? `${p.name} (${mod}${i + 1})` : p.name}
            >
              {count > 0 ? <StatusDot status={aggregateStatus(p)} /> : <i className="dot idle" />}
              <span className="proj-name">{p.name}</span>
              <span className="n">{count}</span>
            </button>
          );
        })}
        <button className="rail-add" onClick={onNewProject}>
          + New project
        </button>
      </div>
      <div className="rail-foot">
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

/** 18px stroke icon from one or more path segments. */
function Icon({ d }: { d: string }) {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d={d} />
    </svg>
  );
}
