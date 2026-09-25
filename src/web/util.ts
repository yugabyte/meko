import type { ProjectWithSessions, Session, SessionStatus } from '../shared/types';

/** Higher is more urgent. Used for the project's aggregate dot in the rail. */
const URGENCY: Record<SessionStatus, number> = {
  needs_input: 6,
  exited: 5,
  working: 4,
  starting: 4,
  recovered: 3,
  idle: 2,
  archived: 0,
};

export function liveSessions(p: ProjectWithSessions): Session[] {
  return p.sessions.filter((s) => s.desired !== 'archived' && s.status !== 'archived');
}

export function aggregateStatus(p: ProjectWithSessions): SessionStatus {
  let best: SessionStatus = 'idle';
  for (const s of liveSessions(p)) if (URGENCY[s.status] > URGENCY[best]) best = s.status;
  return best;
}

export const STATUS_TEXT: Record<SessionStatus, string> = {
  starting: 'starting',
  working: 'working',
  needs_input: 'needs you',
  idle: 'idle',
  exited: 'exited',
  recovered: 'recovered after restart',
  archived: 'archived',
};

export function relativeTime(ms: number | null | undefined, now = Date.now()): string {
  if (!ms) return '—';
  const s = Math.round((now - ms) / 1000);
  if (s < 45) return 'just now';
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.round(m / 60);
  if (h < 24) return h === 1 ? '1 hour ago' : `${h} hours ago`;
  const d = Math.round(h / 24);
  if (d === 1) return 'Yesterday';
  if (d < 14) return `${d} days ago`;
  const w = Math.round(d / 7);
  if (d < 60) return `${w} weeks ago`;
  return new Date(ms).toLocaleDateString();
}

export function shortTime(ms: number, now = Date.now()): string {
  const d = new Date(ms);
  const today = new Date(now);
  if (d.toDateString() === today.toDateString()) {
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  }
  const y = new Date(now - 86400000);
  if (d.toDateString() === y.toDateString()) return 'Yesterday';
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
}

export function duration(ms: number | null | undefined, now = Date.now()): string {
  if (!ms) return '';
  const s = Math.max(0, Math.round((now - ms) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

export function slugify(s: string): string {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 60);
}

export function isMac(): boolean {
  return /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
}

export function modKey(e: KeyboardEvent): boolean {
  return isMac() ? e.metaKey : e.ctrlKey;
}

export function cx(...parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(' ');
}
