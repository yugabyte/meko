/**
 * `meko kode status`'s output, from what it found, laid out as `meko status` is (Meko's
 * `internal/cli/report.go`): a row per part that runs (the server, the session host, the services),
 * a headline (`● AgentK <version> is running · pid, up`), the links as plain aligned rows, then the
 * commands to run next. `meko status` runs it with MEKO_SUMMARY=1 and shows the one line `headline`
 * gives in its AgentK row, without the mark and "AgentK" (src/meko/docs/install.md), so that line's
 * marks are always ✓, ! and ✗, whatever the locale, and never colored.
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

export type Kind = 'ok' | 'warn' | 'fail';

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

/** How the lines are colored on a terminal, by role, as Meko's palette has them (src/meko/docs/install.md, "Colors"). */
export interface Paint {
  /** Whether it colors at all: the call to action then starts a column early. */
  color: boolean;
  /** A row's mark: green, yellow or red. */
  mark: (k: Kind, mark: string) => string;
  /** The headline's text. */
  bold: (s: string) => string;
  /** Notes after " · ", and the links' labels. */
  dim: (s: string) => string;
  /** A URL: blue, underlined, and a link a terminal can open. */
  link: (url: string) => string;
  /** The first command under Next: white on blue, with a space on each side. */
  cta: (s: string) => string;
  /** The other commands under Next. */
  blue: (s: string) => string;
}

const id = (s: string) => s;
export const PLAIN: Paint = { color: false, mark: (_k, m) => m, bold: id, dim: id, link: id, cta: id, blue: id };

/** The server's pid and uptime: "pid 7, up 2h4m0s", or less of it when it isn't known. */
function about(f: Pick<StatusFacts, 'probe' | 'leasePid' | 'now'>): string {
  const pid = (f.probe.ok ? f.probe.pid : undefined) ?? f.leasePid ?? undefined;
  const started = f.probe.ok && f.probe.startedAt ? Date.parse(f.probe.startedAt) : NaN;
  return [pid ? `pid ${pid}` : '', Number.isNaN(started) ? '' : `up ${upFor(f.now - started)}`].filter(Boolean).join(', ');
}

/** The one line `meko status` shows in its AgentK row: "✓ AgentK 0.2 is running (pid 7, up 2h4m0s)". */
export function headline(f: Pick<StatusFacts, 'version' | 'probe' | 'leasePid' | 'now'>): string {
  const a = about(f);
  if (f.probe.ok) return `${MARKS.ok} AgentK ${f.version} is running${a ? ` (${a})` : ''}`;
  if (serverRuns(f)) return `${MARKS.warn} AgentK ${f.version} is running${a ? ` (${a})` : ''}, but isn't answering`;
  return `${MARKS.fail} AgentK ${f.version} isn't running`;
}

/** The status's lines, colored by `p` (plain by default). */
export function formatStatus(f: StatusFacts, p: Paint = PLAIN): string[] {
  const lines: string[] = [];
  const row = (k: Kind, label: string, text: string) => lines.push(`  ${p.mark(k, MARKS[k])} ${label.padEnd(15)} ${text}`);
  const runs = serverRuns(f);

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
    if (!units.some((u) => u.installed)) row('warn', 'Service', 'not installed (meko kode install sets it up)');
    else if (units.every((u) => u.installed && u.running)) row('ok', 'Service', kind);
    else if (units.every((u) => !u.running)) row('warn', 'Service', `${kind}, stopped`);
    else row('warn', 'Service', `${kind}: ${units.filter((u) => !u.running).map((u) => `${u.id} ${u.installed ? u.text : 'not installed'}`).join('; ')}`);
  }

  // The headline, after a blank line (none before it when there are no rows), its text bold and
  // its note dim, as Meko's "● Meko 1.2 is running · pid 7, up 2h".
  if (lines.length) lines.push('');
  const a = about(f);
  const note = (text: string, n: string) => (n ? `${text}${p.dim(` · ${n}`)}` : text);
  if (f.probe.ok) lines.push(`  ${p.mark('ok', '●')} ${note(p.bold(`AgentK ${f.version} is running`), a)}`);
  else if (runs) lines.push(`  ${p.mark('warn', MARKS.warn)} ${note(p.bold(`AgentK ${f.version} isn't answering`), a)}`);
  else lines.push(`  ${p.mark('fail', MARKS.fail)} ${p.bold(`AgentK ${f.version} isn't running`)}`);

  lines.push('', ...links([
    ['Web UI', f.webUrl ? p.link(f.webUrl) : `none (${f.webWhy ?? 'unknown'})`],
    ['Home', f.home],
    ['Logs', f.logs],
  ], p));

  lines.push('', ...next(
    f.probe.ok ? [['meko kode ui', 'open AgentK']]
      : runs ? [['meko kode logs', "see why it isn't answering"], ['meko kode restart', 'restart its server']]
        : [['meko kode start', 'start AgentK']],
    p,
  ));
  return lines;
}

/** Label and value rows, the labels dim and aligned: Meko's link rows (`report.links`). */
function links(rows: [string, string][], p: Paint): string[] {
  const w = Math.max(...rows.map(([l]) => [...l].length));
  return rows.map(([l, v]) => `  ${p.dim(l)}${' '.repeat(w - [...l].length)}  ${v}`);
}

/** "Next:" and the commands, each with what it does, the first as the call to action: Meko's `report.next`. */
function next(cmds: [string, string][], p: Paint): string[] {
  const w = Math.max(...cmds.map(([c]) => [...c].length));
  return ['Next:', ...cmds.map(([c, what], i) => {
    const pad = ' '.repeat(w - [...c].length + 4);
    // The call to action's color starts a column early, so its text lines up with the commands below it.
    if (i === 0 && p.color) return ` ${p.cta(c)}${pad.slice(1)}${what}`;
    return `  ${i === 0 ? c : p.blue(c)}${pad}${what}`;
  })];
}
