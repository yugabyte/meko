/**
 * One row of Meko's summary, which `meko start` and `meko stop` print (backend/docs/design.md, "The
 * summary rows"): two spaces, ✓, ! or ✗, the title padded to 15, then the text. The mark is colored on
 * a terminal unless NO_COLOR is set.
 */
export type Mark = '✓' | '!' | '✗';

const COLORS: Record<Mark, string> = { '✓': '\x1b[32m', '!': '\x1b[33m', '✗': '\x1b[31m' };

export function summaryRow(mark: Mark, title: string, text: string, color = process.stdout.isTTY === true && !process.env.NO_COLOR): string {
  return `  ${color ? `${COLORS[mark]}${mark}\x1b[0m` : mark} ${title.padEnd(15)} ${text}`;
}

/** Whether Meko runs this start or stop for its summary (MEKO_SUMMARY=1): it then prints one row. */
export function forMekoSummary(env: NodeJS.ProcessEnv = process.env): boolean {
  return env.MEKO_SUMMARY === '1';
}
