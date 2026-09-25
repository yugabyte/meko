import { useEffect, useMemo, useRef, useState } from 'react';
import { STAGE_LABELS, type ProjectWithSessions } from '../../shared/types';
import { navigate } from '../router';
import { STATUS_TEXT, cx, liveSessions } from '../util';
import { StatusDot } from './common';

interface Item {
  id: string;
  label: string;
  hint: string;
  keywords: string;
  run: () => void;
  dot?: ProjectWithSessions['sessions'][number]['status'];
}

export function CommandPalette({
  projects,
  currentProjectId,
  onClose,
  onNewProject,
  onNewSession,
  onSettings,
}: {
  projects: ProjectWithSessions[];
  currentProjectId: string | null;
  onClose: () => void;
  onNewProject: () => void;
  onNewSession: (projectId: string) => void;
  onSettings: () => void;
}) {
  const [q, setQ] = useState('');
  const [idx, setIdx] = useState(0);
  const listRef = useRef<HTMLDivElement>(null);

  const items = useMemo<Item[]>(() => {
    const out: Item[] = [];
    const current = projects.find((p) => p.id === currentProjectId);
    if (current) {
      out.push({
        id: 'new-session',
        label: `New session in ${current.name}`,
        hint: 'Action',
        keywords: 'new session task start',
        run: () => onNewSession(current.id),
      });
    }
    for (const p of projects) {
      for (const s of liveSessions(p)) {
        out.push({
          id: `s-${s.id}`,
          label: s.name,
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
        label: `${p.name}: project home`,
        hint: 'Datapacks, search, activity, settings',
        keywords: `${p.name} home datapack search settings`,
        run: () => navigate({ view: 'home', projectId: p.id }),
      });
      if (p.id !== currentProjectId) {
        out.push({
          id: `ns-${p.id}`,
          label: `New session in ${p.name}`,
          hint: 'Action',
          keywords: `new session ${p.name}`,
          run: () => onNewSession(p.id),
        });
      }
    }
    out.push({ id: 'new-project', label: 'New project', hint: 'Action', keywords: 'new project repo add', run: onNewProject });
    out.push({ id: 'settings', label: 'Settings', hint: 'Action', keywords: 'settings password update totp', run: onSettings });
    return out;
  }, [projects, currentProjectId, onNewProject, onNewSession, onSettings]);

  const shown = useMemo(() => {
    const terms = q.toLowerCase().split(/\s+/).filter(Boolean);
    if (!terms.length) return items;
    return items.filter((it) => {
      const hay = `${it.label} ${it.keywords}`.toLowerCase();
      return terms.every((t) => hay.includes(t));
    });
  }, [items, q]);

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
      <div className="palette" role="dialog" aria-label="Command palette">
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
              <span className="pal-label">{it.label}</span>
              <span className="pal-hint">{it.hint}</span>
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
