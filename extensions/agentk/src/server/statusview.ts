/**
 * `me_koder status`'s output, from what it found: shaped like `meko status`. A marked headline,
 * then a row per part that runs (the server, the session host, the services), then the links.
 * Meko shows the headline in its own status row, without the mark and "AgentK" (backend/docs/install.md),
 * so its marks are always ✓, ! and ✗, whatever the locale.
 */
import type { Probe } from './health.js';

export interface StatusFacts {
  /** The installed version. */
  version: string;
  /** The server's /healthz. */
  probe: Probe;
  /** The pid holding the home's lease: a server that runs, answering or not. */
  leasePid: number | null;
  /** The session host: up with its pid and sessions, not running, or not answering. */
  host: { up: true; pid?: number; sessions?: number; running?: number } | { up: false } | { error: string };
  /**
   * The background services: each one's state, or `other` when the installed services run
   * another home, or null where there are none (a platform without them).
   */
  services: { kind: string; units: { id: string; installed: boolean; running: boolean; text: string }[] } | 'other' | null;
  webUrl: string | null;
  /** Why there's no web URL. */
  webWhy?: string;
  home: string;
  logs: string;
  /** Now, for the uptime. */
  now: number;
}

type Kind = 'ok' | 'warn' | 'fail';

const MARKS: Record<Kind, string> = { ok: '✓', warn: '!', fail: '✗' };

/** The uptime as Go writes a duration (Meko's `up 2h4m0s`), to the second. */
export function upFor(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h) return `${h}h${m}m${s}s`;
  if (m) return `${m}m${s}s`;
  return `${s}s`;
}

/** Whether a server runs this home: it answers, or holds the home's lease (only slow to answer). */
export function serverRuns(f: Pick<StatusFacts, 'probe' | 'leasePid'>): boolean {
  return f.probe.ok || f.leasePid !== null;
}

/**
 * The status's lines. `paint` colors a mark (green, yellow, red on a terminal); the headline's mark
 * stays plain for Meko, which reads it.
 */
export function formatStatus(f: StatusFacts, paint: (k: Kind, mark: string) => string = (_k, m) => m): string[] {
  const lines: string[] = [];
  const row = (k: Kind, label: string, text: string) => lines.push(`  ${paint(k, MARKS[k])} ${label.padEnd(15)} ${text}`);
  const runs = serverRuns(f);
  const pid = (f.probe.ok ? f.probe.pid : undefined) ?? f.leasePid ?? undefined;
  const started = f.probe.ok && f.probe.startedAt ? Date.parse(f.probe.startedAt) : NaN;
  const about = [pid ? `pid ${pid}` : '', Number.isNaN(started) ? '' : `up ${upFor(f.now - started)}`].filter(Boolean).join(', ');
  if (f.probe.ok) lines.push(`${MARKS.ok} AgentK ${f.version} is running${about ? ` (${about})` : ''}`);
  else if (runs) lines.push(`${MARKS.warn} AgentK ${f.version} is running${about ? ` (${about})` : ''}, but isn't answering`);
  else lines.push(`${MARKS.fail} AgentK ${f.version} isn't running`);

  if (f.probe.ok) row('ok', 'Server', 'healthy');
  else if (runs) row('warn', 'Server', `not answering (${f.probe.error})`);

  if ('error' in f.host) row('warn', 'Session host', `not answering (${f.host.error})`);
  else if (f.host.up) {
    const n = f.host.sessions;
    const sessions = n === undefined ? '' : `, ${n} session${n === 1 ? '' : 's'}${f.host.running !== undefined && f.host.running !== n ? ` (${f.host.running} running)` : ''}`;
    row('ok', 'Session host', `running${f.host.pid ? ` (pid ${f.host.pid})` : ''}${sessions}`);
  } else if (runs) row('warn', 'Session host', 'not running');

  if (f.services === 'other') row('warn', 'Service', 'none for this home (the installed services run another one)');
  else if (f.services) {
    const { kind, units } = f.services;
    if (!units.some((u) => u.installed)) row('warn', 'Service', 'not installed (me_koder install sets it up)');
    else if (units.every((u) => u.installed && u.running)) row('ok', 'Service', kind);
    else if (units.every((u) => !u.running)) row('warn', 'Service', `${kind}, stopped`);
    else row('warn', 'Service', `${kind}: ${units.filter((u) => !u.running).map((u) => `${u.id} ${u.installed ? u.text : 'not installed'}`).join('; ')}`);
  }

  lines.push('');
  const links: [string, string][] = [['Web UI', f.webUrl ?? `none (${f.webWhy ?? 'unknown'})`], ['Home', f.home], ['Logs', f.logs]];
  for (const [label, value] of links) lines.push(`  ${label.padEnd(9)}${value}`);
  if (!runs) {
    lines.push('');
    lines.push('  Start it: me_koder start');
  }
  return lines;
}
