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

/** Whether Meko runs this start or stop for its summary (MEKO_SUMMARY=1): it then prints one row. */
export function forMekoSummary(env: NodeJS.ProcessEnv = process.env): boolean {
  return env.MEKO_SUMMARY === '1';
}
