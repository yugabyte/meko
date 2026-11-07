/**
 * One row of Meko's summary, which `meko start` and `meko stop` print (src/meko/docs/install.md, "The
 * summary rows"): two spaces, ✓, ! or ✗, the title padded to 15, then the text. The mark is colored
 * (green, bold yellow, red, as Meko's are) on a terminal unless NO_COLOR is set or TERM is dumb.
 */
export type Mark = '✓' | '!' | '✗';

const COLORS: Record<Mark, string> = { '✓': '\x1b[32m', '!': '\x1b[1;33m', '✗': '\x1b[31m' };

const colored = () => process.stdout.isTTY === true && !process.env.NO_COLOR && process.env.TERM !== 'dumb';

/**
 * Meko's blue, for commands, picked for the terminal's background as Meko's rows pick it
 * (src/meko/internal/spin/background.go, and AgentK's tty.ts blueCode): #79aeff on dark, #1f5fd1 on
 * light, or the nearest of the 256 colors without 24-bit color (the theme's own blue with only 16).
 * The background is MEKO_TERM_BG, else COLORFGBG's last field, else dark.
 */
export function blueCode(env: NodeJS.ProcessEnv = process.env): string {
  let bg = env.MEKO_TERM_BG === 'light' || env.MEKO_TERM_BG === 'dark' ? env.MEKO_TERM_BG : '';
  if (!bg) {
    const n = Number((env.COLORFGBG ?? '').split(';').pop());
    bg = env.COLORFGBG?.includes(';') && Number.isInteger(n) && (n === 7 || (n >= 9 && n <= 15)) ? 'light' : 'dark';
  }
  const sixteen = /^(linux|vt|ansi|cons25|sun)/.test(env.TERM ?? '');
  const depth = env.COLORTERM === 'truecolor' || env.COLORTERM === '24bit' ? 0 : sixteen ? 2 : 1;
  return (bg === 'dark' ? ['38;2;121;174;255', '38;5;111', '94'] : ['38;2;31;95;209', '38;5;26', '34'])[depth];
}

export function summaryRow(mark: Mark, title: string, text: string, color = colored()): string {
  return `  ${color ? `${COLORS[mark]}${mark}\x1b[0m` : mark} ${title.padEnd(15)} ${text}`;
}

/**
 * Whether Meko runs this command for its summary (MEKO_SUMMARY=1): `start` and `stop` then print
 * one row, and `status` only its headline, which `meko status` shows in Present's row.
 */
export function forMekoSummary(env: NodeJS.ProcessEnv = process.env): boolean {
  return env.MEKO_SUMMARY === '1';
}
