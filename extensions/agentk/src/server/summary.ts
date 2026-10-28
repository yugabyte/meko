/**
 * One row of Meko's summary, which `meko start` and `meko stop` print (backend/docs/install.md, "The
 * summary rows"): two spaces, ✓, ! or ✗, the title padded to 15, then the text. The mark is colored on
 * a terminal unless NO_COLOR is set.
 */
export type Mark = '✓' | '!' | '✗';

const COLORS: Record<Mark, string> = { '✓': '\x1b[32m', '!': '\x1b[33m', '✗': '\x1b[31m' };

export function summaryRow(mark: Mark, title: string, text: string, color = process.stdout.isTTY === true && !process.env.NO_COLOR): string {
  return `  ${color ? `${COLORS[mark]}${mark}\x1b[0m` : mark} ${title.padEnd(15)} ${text}`;
}

/**
 * What AgentK's row of `meko stop` says. Its sessions resume when the server starts again: at `meko
 * start` with services, but without them `meko start`'s `agentk start` can't, and `agentk serve` does.
 */
export function stopRowText(stopped: boolean, sessions: number, services: boolean): string {
  if (!stopped) return "wasn't running";
  if (!sessions) return 'stopped';
  return `stopped; ${sessions} session(s) resume at ${services ? 'meko start' : 'agentk serve'}`;
}

/** Whether Meko runs this start or stop for its summary (MEKO_SUMMARY=1): it then prints one row. */
export function forMekoSummary(env: NodeJS.ProcessEnv = process.env): boolean {
  return env.MEKO_SUMMARY === '1';
}
