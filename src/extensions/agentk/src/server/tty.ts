/**
 * Small helpers for readable CLI output: sections, aligned rows and status
 * marks. Color only on a terminal, and never when NO_COLOR is set.
 */

const useColor = (stream: NodeJS.WriteStream) => !!stream.isTTY && !process.env.NO_COLOR && process.env.TERM !== 'dumb';
const utf8 = /utf-?8/i.test(`${process.env.LC_ALL ?? ''}${process.env.LC_CTYPE ?? ''}${process.env.LANG ?? ''}`);

function paint(code: string, s: string, stream: NodeJS.WriteStream = process.stdout): string {
  return useColor(stream) ? `\x1b[${code}m${s}\x1b[0m` : s;
}

export const style = {
  bold: (s: string) => paint('1', s),
  dim: (s: string) => paint('2', s),
  green: (s: string) => paint('32', s),
  yellow: (s: string) => paint('33', s),
  red: (s: string) => paint('31', s),
  blue: (s: string) => paint('34', s),
};

const MARK = {
  ok: () => style.green(utf8 ? '✓' : 'ok'),
  warn: () => style.yellow(utf8 ? '!' : '!!'),
  fail: () => style.red(utf8 ? '✗' : 'xx'),
};

/** The dots a running step shows, and how fast they turn: gh's spinner, blue, as Meko's steps show it. */
const SPIN_FRAMES = utf8 ? ['⣾', '⣽', '⣻', '⢿', '⡿', '⣟', '⣯', '⣷'] : ['|', '/', '-', '\\'];
const SPIN_INTERVAL_MS = 120;

const LABEL_WIDTH = 14;
/** A label padded to `width`, with at least one space after it when it's that long or longer. */
export const pad = (label: string, width = LABEL_WIDTH) => (label.length >= width ? `${label} ` : label.padEnd(width));

/**
 * A running step's row, "  ⣾ Label         doing", cut to one character short of `columns` (when
 * known) with "…", so it never reaches the last column and wraps. With `color` the dots are blue,
 * as Meko's are (its palette: src/meko/docs/install.md, "Colors"); the cut counts only what shows.
 */
export function spinRow(frame: string, label: string, doing: string, columns?: number, color = false): string {
  const chars = [...`  ${frame} ${pad(label)}${doing}`];
  const row = !columns || chars.length < columns ? chars.join('') : `${chars.slice(0, Math.max(1, columns - 2)).join('')}…`;
  const at = `  ${frame}`;
  return color && row.startsWith(at) ? `  \x1b[34m${frame}\x1b[0m${row.slice(at.length)}` : row;
}

const out = (s = '') => process.stdout.write(s + '\n');

export const ui = {
  blank: () => out(),
  title: (s: string) => {
    out();
    out(style.bold(s));
  },
  section: (s: string) => {
    out();
    out(style.bold(s));
  },
  /** "  ✓ Label         detail" */
  ok: (label: string, detail = '') => out(`  ${MARK.ok()} ${detail ? pad(label) + style.dim(detail) : label}`),
  warn: (label: string, detail = '') => out(`  ${MARK.warn()} ${detail ? pad(label) + detail : label}`),
  fail: (label: string, detail = '') => process.stderr.write(`  ${MARK.fail()} ${detail ? pad(label) + detail : label}\n`),
  /** "    Label         value" (no mark) */
  row: (label: string, value: string) => out(`    ${style.dim(pad(label))}${value}`),
  note: (s: string) => out(`    ${style.dim(s)}`),
  text: (s: string) => out(s),
  /**
   * A step that takes a while: on a terminal its row shows rotating dots ("  ⣾ Label         doing")
   * until `stop` erases it, for the row that says how it went; anywhere else it prints `doing` as
   * a plain line when it starts, so a log isn't left empty. Meko's steps look the same. The row is
   * cut to the terminal's width (`spinRow`), since one that wrapped would leave a line per frame.
   */
  spin: (label: string, doing: string): { stop: () => void } => {
    if (!process.stdout.isTTY || process.env.TERM === 'dumb') {
      out(`    ${doing}`);
      return { stop: () => {} };
    }
    let i = 0;
    const draw = () => process.stdout.write(`\r${spinRow(SPIN_FRAMES[i++ % SPIN_FRAMES.length], label, doing, process.stdout.columns, useColor(process.stdout))}\x1b[K`);
    draw();
    const timer = setInterval(draw, SPIN_INTERVAL_MS);
    let stopped = false;
    return {
      stop: () => {
        if (stopped) return;
        stopped = true;
        clearInterval(timer);
        process.stdout.write('\r\x1b[K');
      },
    };
  },
  /** Prompt text for readline: "  Label         [default] › " */
  prompt: (label: string, def: string) => `  ${pad(label, LABEL_WIDTH + 2)}${def ? style.dim(`[${def}] `) : ''}› `,
};
