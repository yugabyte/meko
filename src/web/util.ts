import type { ProjectWithSessions, Session, SessionStatus, TokenUsage } from '../shared/types';

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

/** Last segment of a repo path: /home/me/code/api → api. */
export function repoFolder(path: string): string {
  return path.split('/').filter(Boolean).pop() ?? path;
}

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

/** A datapack's page in the Meko web app, or null when the web app's address is unknown. */
export function mekoDatapackUrl(webUrl: string | null, datapackId: string): string | null {
  return webUrl ? `${webUrl}/datapacks/${encodeURIComponent(datapackId)}` : null;
}

/** The datapack's Share page in the Meko web app (how sharing works in link mode). */
export function mekoDatapackShareUrl(webUrl: string | null, datapackId: string): string | null {
  const u = mekoDatapackUrl(webUrl, datapackId);
  return u ? `${u}/share` : null;
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

export function totalTokens(u: TokenUsage): number {
  return u.input + u.output + u.cacheRead + u.cacheWrite;
}

/** 950 → "950", 12_300 → "12.3k", 4_560_000 → "4.56M". */
export function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  const [div, unit] = n >= 1e9 ? [1e9, 'B'] : n >= 1e6 ? [1e6, 'M'] : [1e3, 'k'];
  const v = n / div;
  return `${v >= 100 ? Math.round(v) : v >= 10 ? v.toFixed(1).replace(/\.0$/, '') : v.toFixed(2).replace(/\.?0+$/, '')}${unit}`;
}

/** USD: "$3.06", "$241", "$1,234", "<$0.01", "−$2.75". */
export function formatCost(usd: number): string {
  const sign = usd < 0 ? '−' : '';
  const v = Math.abs(usd);
  if (v === 0) return '$0';
  if (v < 0.01) return `${sign}<$0.01`;
  return `${sign}$${v < 100 ? v.toFixed(2) : Math.round(v).toLocaleString('en-US')}`;
}

/** One-line breakdown for tooltips. */
export function usageTitle(u: TokenUsage): string {
  const f = (n: number) => n.toLocaleString();
  return `Tokens: ${f(totalTokens(u))} total · ${f(u.output)} output · ${f(u.input)} input · ${f(u.cacheWrite)} cache write · ${f(u.cacheRead)} cache read · ${u.messages} responses`;
}

/** The status detail the server sets when a design waits for approval (`DESIGN_READY` in lifecycle.ts). */
const DESIGN_READY = 'Design ready for your approval';

/** Why Approve design can't be pressed right now, or null when it can. Mirrors the server's checks. */
export function designApprovalBlocker(session: Session): string | null {
  // A question or permission prompt from Claude isn't the design; the server refuses to approve over it.
  // (Plan mode's approval dialog can carry Claude's own wording, so any mention of a plan is allowed.)
  const otherPrompt =
    session.status === 'needs_input' &&
    !!session.statusDetail &&
    session.statusDetail !== DESIGN_READY &&
    !/plan/i.test(session.statusDetail);
  if (!session.hasDesign) return 'AgentK hasn’t proposed a design yet.';
  if (session.status === 'working' || session.status === 'starting') return 'Wait for AgentK to finish its turn.';
  if (session.status === 'exited') return 'Claude has exited. Resume the session first.';
  if (otherPrompt) return 'AgentK is waiting on you in the terminal. Answer it first.';
  return null;
}
