import headless from '@xterm/headless';
import type { IBuffer } from '@xterm/xterm';
import { describe, expect, it } from 'vitest';
import { bufferRow, linkAt, type TermRow } from '../../src/web/termlinks.js';

/** A row of `cols` cells holding `text`, one cell a character, padded with blanks. */
function row(text: string, cols: number, wrapped = false): TermRow {
  const cells = Array.from(text);
  expect(cells.length, text).toBeLessThanOrEqual(cols);
  while (cells.length < cols) cells.push(' ');
  return { cells, wrapped };
}

/** linkAt over these rows, at the cell holding the `n`th (0-based) `needle` on row `y`. */
function tap(rows: TermRow[], y: number, needle: string, n = 0): string | null {
  const text = rows[y]!.cells.join('');
  let col = -1;
  for (let i = 0; i <= n; i++) col = text.indexOf(needle, col + 1);
  expect(col, `${needle} on row ${y}`).toBeGreaterThanOrEqual(0);
  return linkAt((i) => rows[i], y, col);
}

const SLACK = 'https://yugabyte.slack.com/archives/C0BE0G1N9ND/p1792458792999779';

describe('the link under a tap', () => {
  it('joins a URL Claude broke onto an indented row, from either row, but not from the indent', () => {
    // The reported screen: the URL fills the first row, and Claude carries it on under the text.
    const first = '● I posted in #amp-data-plane-dev: https://yugabyte.slack.co';
    const cols = first.length;
    const rows = [row('Called Slack', cols), row(first, cols), row('  m/archives/C0BE0G1N9ND/p1792458792999779', cols), row('', cols)];
    expect(tap(rows, 1, 'https')).toBe(SLACK);
    expect(tap(rows, 1, 'slack.co')).toBe(SLACK);
    expect(tap(rows, 2, 'm/archives')).toBe(SLACK);
    expect(tap(rows, 2, '9779')).toBe(SLACK);
    expect(linkAt((i) => rows[i], 2, 0)).toBeNull();
    expect(linkAt((i) => rows[i], 2, 1)).toBeNull();
    expect(tap(rows, 1, 'posted')).toBeNull();
  });

  it('joins a URL over three rows', () => {
    const rows = [row('- see https://example.com/a', 27), row('  bcdefghijklmnopqrstuvwxyz', 27), row('  /end and more', 27)];
    const url = 'https://example.com/abcdefghijklmnopqrstuvwxyz/end';
    expect(tap(rows, 0, 'https')).toBe(url);
    expect(tap(rows, 1, 'q')).toBe(url);
    expect(tap(rows, 2, 'end')).toBe(url);
    expect(tap(rows, 2, 'more')).toBeNull();
  });

  it('joins under a nested list item', () => {
    const first = '  - Tags: a thread reply: https://yugabyte.slack.co';
    const rows = [row(first, first.length), row('    m/archives/C0BE0G1N9ND/p1792458792999779', first.length)];
    expect(tap(rows, 1, 'archives')).toBe(SLACK);
    expect(tap(rows, 0, 'https')).toBe(SLACK);
  });

  it('joins rows xterm wrapped itself', () => {
    const rows = [row('see https://example.com/aaaa', 28), row('bbbb/c done', 28, true)];
    expect(tap(rows, 0, 'example')).toBe('https://example.com/aaaabbbb/c');
    expect(tap(rows, 1, 'c ')).toBe('https://example.com/aaaabbbb/c');
    expect(tap(rows, 1, 'done')).toBeNull();
  });

  it("doesn't join a URL that stops before the row's end", () => {
    const rows = [row('see https://example.com/a', 30), row('  bcd', 30)];
    expect(tap(rows, 0, 'https')).toBe('https://example.com/a');
    expect(tap(rows, 1, 'bcd')).toBeNull();
  });

  it("doesn't join a row indented deeper than where the URL started", () => {
    const rows = [row('x https://example.com/a', 23), row('      bcd', 23)];
    expect(tap(rows, 0, 'https')).toBe('https://example.com/a');
    expect(tap(rows, 1, 'bcd')).toBeNull();
  });

  it("doesn't join across a border", () => {
    for (const next of ['  │ bcd', '  | bcd', '│ bcd']) {
      const rows = [row('x https://example.com/a', 23), row(next, 23)];
      expect(tap(rows, 0, 'https'), next).toBe('https://example.com/a');
    }
  });

  it('takes only URL characters from the row Claude carried it onto', () => {
    const rows = [row('x https://example.com/a', 23), row('  bc│de', 23)];
    expect(tap(rows, 0, 'https')).toBe('https://example.com/abc');
    expect(tap(rows, 1, 'de')).toBeNull();
  });

  it('leaves out trailing punctuation and brackets', () => {
    const rows = [row('(see https://example.com/a', 26), row('  bc).', 26)];
    expect(tap(rows, 1, 'bc')).toBe('https://example.com/abc');
    expect(tap(rows, 1, ')')).toBeNull();
    expect(tap(rows, 1, '.')).toBeNull();
  });

  it('finds the right cells after a wide character', () => {
    // 日 and 本 take two cells each.
    const cells = ['日', '', '本', '', ' ', ...Array.from('https://example.com/a')];
    const rows: TermRow[] = [{ cells, wrapped: false }, row('  bc', cells.length)];
    expect(linkAt((i) => rows[i], 0, 5)).toBe('https://example.com/abc');
    expect(linkAt((i) => rows[i], 0, 1)).toBeNull();
    expect(tap(rows, 1, 'bc')).toBe('https://example.com/abc');
  });

  it('tells apart two links on a row, and a link after a joined one', () => {
    const rows = [row('https://a.io and https://b.io/x', 31), row('  y then https://c.io', 31)];
    expect(tap(rows, 0, 'a.io')).toBe('https://a.io');
    expect(tap(rows, 0, 'b.io')).toBe('https://b.io/xy');
    expect(tap(rows, 1, 'y')).toBe('https://b.io/xy');
    expect(tap(rows, 1, 'c.io')).toBe('https://c.io');
    expect(tap(rows, 1, 'then')).toBeNull();
  });

  it('finds a link on one row, and nothing past it or without a host', () => {
    const rows = [row('open https://example.com now', 40), row('just https:// here', 40)];
    expect(tap(rows, 0, 'example')).toBe('https://example.com');
    expect(tap(rows, 0, 'now')).toBeNull();
    expect(tap(rows, 1, 'https')).toBeNull();
    expect(linkAt((i) => rows[i], 5, 0)).toBeNull();
  });
});

describe("the link under a tap, in xterm's buffer", () => {
  /** A headless xterm of `cols` columns after `data` is written to it, and linkAt over its buffer. */
  async function screen(cols: number, data: string) {
    const term = new headless.Terminal({ cols, rows: 6, allowProposedApi: true });
    await new Promise<void>((r) => term.write(data, r));
    const buf = term.buffer.active as unknown as IBuffer;
    return (y: number, col: number) => linkAt((i) => bufferRow(buf, i), y, col);
  }

  it('joins a URL Claude broke with a cursor move, and one xterm wrapped', async () => {
    // As Ink draws it: the first row, then the cursor to the next row and the indented rest.
    const at = await screen(20, 'x https://a.io/bcdef\r\n  ghij\r\nhttps://b.io/klmnopqrstu');
    expect(at(0, 2)).toBe('https://a.io/bcdefghij');
    expect(at(1, 3)).toBe('https://a.io/bcdefghij');
    expect(at(1, 0)).toBeNull();
    // 24 characters on 20 columns: xterm wraps the last 4 onto row 3.
    expect(at(2, 0)).toBe('https://b.io/klmnopqrstu');
    expect(at(3, 2)).toBe('https://b.io/klmnopqrstu');
  });

  it('reads wide characters as two cells', async () => {
    const at = await screen(30, '日本 https://a.io/x');
    expect(at(0, 5)).toBe('https://a.io/x');
    expect(at(0, 4)).toBeNull();
  });
});
