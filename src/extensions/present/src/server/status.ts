/**
 * `meko present status`'s output, shaped like `meko status`: a marked headline, a row per check, then
 * the links. Meko shows the headline in its own status row without the mark and "Present"
 * (src/meko/docs/install.md), so the headline's mark is never colored.
 */
import { summaryRow } from './summary.js';

export interface StatusFacts {
  version: string;
  /** The server's pid, from its pid file, while that process is a Present server. */
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

/** Whether Present runs: its server is up and answers. Meko reads it from the exit code (3 when not). */
export function runs(f: Pick<StatusFacts, 'pid' | 'answers'>): boolean {
  return f.pid !== null && f.answers;
}

export function statusLines(f: StatusFacts, color = process.stdout.isTTY === true && !process.env.NO_COLOR): string[] {
  const lines: string[] = [];
  if (runs(f)) {
    const up = f.startedAt === null ? '' : `, up ${upFor(f.now - f.startedAt)}`;
    lines.push(`✓ Present ${f.version} is running (pid ${f.pid}${up})`);
    lines.push(summaryRow('✓', 'Server', 'healthy', color));
  } else if (f.pid !== null) {
    lines.push(`✗ Present ${f.version} isn't answering (pid ${f.pid})`);
    lines.push(summaryRow('✗', 'Server', 'not answering on its socket', color));
  } else {
    lines.push(`✗ Present ${f.version} isn't running`);
  }
  lines.push(f.claude ? summaryRow('✓', 'Claude Code', 'installed', color) : summaryRow('!', 'Claude Code', 'not found: install it to build decks', color));
  lines.push('');
  lines.push(`  ${'Pages'.padEnd(9)}meko ui, then Present in its sidebar`);
  lines.push(`  ${'Home'.padEnd(9)}${f.home}`);
  if (f.pid === null) {
    lines.push('');
    lines.push('  Start it: meko present start');
  } else if (!f.answers) {
    // start does nothing while this pid runs: the server has to stop first.
    lines.push('');
    lines.push(`  Restart it: meko present stop, then meko present start (its log: ${f.home}/logs/server.log)`);
  }
  return lines;
}
