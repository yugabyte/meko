/**
 * `meko present status`'s output, laid out as `meko status` is (Meko's `internal/cli/report.go`): a
 * row per check, a headline (`● Presenter <version> is running · pid, up`), the links as plain
 * aligned rows, then the commands to run next. `meko status` runs it with MEKO_SUMMARY=1 and shows
 * the one line `headline` gives in its Presenter row, without the mark and "Presenter"
 * (src/meko/docs/install.md), so that line's mark is never colored. Off a terminal the output starts
 * with that line too: a Meko from before MEKO_SUMMARY reads the first line, and would take a row's
 * mark (`✓ Claude Code …` of a stopped Presenter) for Presenter's.
 */
import { blueCode, summaryRow } from './summary.js';

export interface StatusFacts {
  version: string;
  /** The server's pid, from its pid file, while that process is a Presenter server. */
  pid: number | null;
  /** When the server started (its pid file's time), in ms. */
  startedAt: number | null;
  /** Whether the server answers on its socket. */
  answers: boolean;
  /** Whether Claude Code, which builds decks, is installed. */
  claude: boolean;
  home: string;
  now: number;
}

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

/** Whether Presenter runs: its server is up and answers. Meko reads it from the exit code (3 when not). */
export function runs(f: Pick<StatusFacts, 'pid' | 'answers'>): boolean {
  return f.pid !== null && f.answers;
}

/** The one line `meko status` shows in its Presenter row: "✓ Presenter 0.2 is running (pid 7, up 4m3s)". */
export function headline(f: Pick<StatusFacts, 'version' | 'pid' | 'startedAt' | 'answers' | 'now'>): string {
  if (runs(f)) return `✓ Presenter ${f.version} is running (pid ${f.pid}${f.startedAt === null ? '' : `, up ${upFor(f.now - f.startedAt)}`})`;
  if (f.pid !== null) return `✗ Presenter ${f.version} isn't answering (pid ${f.pid})`;
  return `✗ Presenter ${f.version} isn't running`;
}

const paint = (code: string, s: string, color: boolean) => (color ? `\x1b[${code}m${s}\x1b[0m` : s);

/**
 * The status's lines, in color when `color`. Off a `terminal` they start with the `headline` and a
 * blank line, for a Meko from before MEKO_SUMMARY, which reads the first line.
 */
export function statusLines(f: StatusFacts, color = process.stdout.isTTY === true && !process.env.NO_COLOR && process.env.TERM !== 'dumb',
  terminal = process.stdout.isTTY === true): string[] {
  const lines: string[] = terminal ? [] : [headline(f), ''];
  if (runs(f)) lines.push(summaryRow('✓', 'Server', 'healthy', color));
  else if (f.pid !== null) lines.push(summaryRow('✗', 'Server', 'not answering on its socket', color));
  lines.push(f.claude ? summaryRow('✓', 'Claude Code', 'installed', color) : summaryRow('!', 'Claude Code', 'not found: install it to build decks', color));

  // The headline, its text bold and its note dim, as Meko's "● Meko 1.2 is running · pid 7, up 2h".
  const note = (text: string, n: string) => `${paint('1', text, color)}${n ? paint('2', ` · ${n}`, color) : ''}`;
  lines.push('');
  if (runs(f)) lines.push(`  ${paint('32', '●', color)} ${note(`Presenter ${f.version} is running`, `pid ${f.pid}${f.startedAt === null ? '' : `, up ${upFor(f.now - f.startedAt)}`}`)}`);
  else if (f.pid !== null) lines.push(`  ${paint('31', '✗', color)} ${note(`Presenter ${f.version} isn't answering`, `pid ${f.pid}`)}`);
  else lines.push(`  ${paint('31', '✗', color)} ${note(`Presenter ${f.version} isn't running`, '')}`);

  // The links, their labels dim and aligned: Meko's link rows.
  lines.push('');
  lines.push(`  ${paint('2', 'Pages', color)}  meko ui, then Presenter`);
  lines.push(`  ${paint('2', 'Home', color)}   ${f.home}`);
  if (!runs(f) && f.pid !== null) lines.push(`  ${paint('2', 'Logs', color)}   ${f.home}/logs/server.log`);

  // What to run next, the first as the call to action. start does nothing while a pid that doesn't
  // answer runs: the server has to stop first.
  const cmds: [string, string][] = runs(f) ? [['meko ui', 'open the web UI, then Presenter']]
    : f.pid !== null ? [['meko present stop', "stop the server that isn't answering"], ['meko present start', 'then start it again']]
      : [['meko present start', 'start Presenter']];
  const w = Math.max(...cmds.map(([c]) => c.length));
  lines.push('', 'Next:');
  cmds.forEach(([c, what], i) => {
    const pad = ' '.repeat(w - c.length + 4);
    // The call to action's color starts a column early, so its text lines up with the commands below it.
    if (i === 0 && color) lines.push(` ${paint('1;97;44', ` ${c} `, true)}${pad.slice(1)}${what}`);
    else lines.push(`  ${i === 0 ? c : paint(blueCode(), c, color)}${pad}${what}`);
  });
  return lines;
}
