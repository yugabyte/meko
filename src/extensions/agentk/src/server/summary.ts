/**
 * One row of Meko's summary, which `meko start` and `meko stop` print (src/meko/docs/install.md, "The
 * summary rows"): two spaces, ✓, ! or ✗, the title padded to 15, then the text. The mark is colored
 * (green, bold yellow, red, as Meko's are) on a terminal unless NO_COLOR is set or TERM is dumb.
 */
export type Mark = '✓' | '!' | '✗';

const COLORS: Record<Mark, string> = { '✓': '\x1b[32m', '!': '\x1b[1;33m', '✗': '\x1b[31m' };

const colored = () => process.stdout.isTTY === true && !process.env.NO_COLOR && process.env.TERM !== 'dumb';

export function summaryRow(mark: Mark, title: string, text: string, color = colored()): string {
  return `  ${color ? `${COLORS[mark]}${mark}\x1b[0m` : mark} ${title.padEnd(15)} ${text}`;
}

/**
 * What AgentK's row of `meko stop` says. Its sessions resume when the server starts again: at `meko
 * start` with services, but without them `meko start`'s `me_kode start` can't, and `meko kode serve` does.
 */
export function stopRowText(stopped: boolean, sessions: number, services: boolean): string {
  if (!stopped) return "wasn't running";
  if (!sessions) return 'stopped';
  return `stopped; ${sessions} session(s) resume at ${services ? 'meko start' : 'meko kode serve'}`;
}

/**
 * Whether Meko runs this command for its summary (MEKO_SUMMARY=1): `start` and `stop` then print
 * one row, and `status` only its headline, which `meko status` shows in AgentK's row.
 */
export function forMekoSummary(env: NodeJS.ProcessEnv = process.env): boolean {
  return env.MEKO_SUMMARY === '1';
}
