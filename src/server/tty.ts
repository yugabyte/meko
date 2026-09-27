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
  cyan: (s: string) => paint('36', s),
};

const MARK = {
  ok: () => style.green(utf8 ? '✓' : 'ok'),
  warn: () => style.yellow(utf8 ? '!' : '!!'),
  fail: () => style.red(utf8 ? '✗' : 'xx'),
};

const LABEL_WIDTH = 14;
/** A label padded to `width`, with at least one space after it when it's that long or longer. */
export const pad = (label: string, width = LABEL_WIDTH) => (label.length >= width ? `${label} ` : label.padEnd(width));
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
  /** Prompt text for readline: "  Label         [default] › " */
  prompt: (label: string, def: string) => `  ${pad(label, LABEL_WIDTH + 2)}${def ? style.dim(`[${def}] `) : ''}› `,
};
