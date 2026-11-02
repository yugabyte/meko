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

/** Rows as wrap-ansi draws them: the first one fills the screen's width. */
function rows(...texts: string[]): TermRow[] {
  return texts.map((t) => row(t, Array.from(texts[0]!).length));
}

/** linkAt over these rows, at the cell holding the first `needle` on row `y`. */
function tap(rs: TermRow[], y: number, needle: string): string | null {
  const col = rs[y]!.cells.join('').indexOf(needle);
  expect(col, `${needle} on row ${y}`).toBeGreaterThanOrEqual(0);
  return linkAt((i) => rs[i], y, col);
}

const SLACK = 'https://acme.slack.com/archives/C024BE91L/p1700000000123456';

describe('the link under a tap', () => {
  // Each URL is longer than its text's width, so wrap-ansi broke it, and carried it on at the margin.
  it.each([
    ['after a bullet (the reported screen)', rows('● I posted in #team-updates: https://acme.slack.co', '  m/archives/C024BE91L/p1700000000123456'), SLACK, [[0, 'https'], [0, 'slack.co'], [1, 'm/archives'], [1, '3456']]],
    ['under a nested list item', rows('  - Tags: a thread reply: https://acme.slack.co', '    m/archives/C024BE91L/p1700000000123456'), SLACK, [[0, 'https'], [1, 'archives']]],
    ['under a numbered item', rows('1. see https://example.com/abcdefghij', '   klmnopqrstuvwxyz'), 'https://example.com/abcdefghijklmnopqrstuvwxyz', [[1, 'klm']]],
    ['over three rows', rows('- see https://example.com/a', '  bcdefghijklmnopqrstuvwxyz', '  /end and more'), 'https://example.com/abcdefghijklmnopqrstuvwxyz/end', [[0, 'https'], [1, 'q'], [2, 'end']]],
    ['with no margin', rows('x https://example.com/abcdefghijklmnopqrst', 'uvwxyz/ ok'), 'https://example.com/abcdefghijklmnopqrstuvwxyz/', [[1, 'uvw']]],
  ] as [string, TermRow[], string, [number, string][]][])('joins a URL Claude broke %s, from any row', (_name, rs, url, taps) => {
    for (const [y, needle] of taps) expect(tap(rs, y, needle), `${needle} on row ${y}`).toBe(url);
  });

  it('leaves the indent before a carried-on URL to Claude', () => {
    const rs = rows('● I posted in #team-updates: https://acme.slack.co', '  m/archives/C024BE91L/p1700000000123456');
    expect(linkAt((i) => rs[i], 1, 0)).toBeNull();
    expect(linkAt((i) => rs[i], 1, 1)).toBeNull();
    expect(tap(rs, 0, 'posted')).toBeNull();
  });

  it.each([
    // wrap-ansi sends the word after an exactly full row down with its space in front.
    ['a new word after a URL that fills the row', rows('see https://example.com/page', ' Next: it works'), 'Next'],
    ['a row indented deeper than the margin', rows('- see https://example.com/abc', '    defghijklmnop'), 'def'],
    ['a row that starts left of the margin', rows('- see https://example.com/abc', 'defghijklmnop'), 'def'],
    // A URL and that word together fit the text's width, so wrap-ansi wouldn't have broken them.
    ['a next line too short to be the rest', rows('  see https://example.com/abc', '  so it works'), 'so'],
    ['a border', rows('  x https://example.com/abc', '  │ defghijklmnopqrstuvw'), '│'],
  ])("doesn't join %s", (_name, rs, next) => {
    const url = rs[0]!.cells.join('').trimEnd().split(' ').pop();
    expect(tap(rs, 0, 'https')).toBe(url);
    expect(tap(rs, 1, next)).toBeNull();
  });

  it("doesn't join a URL that stops before the row's end", () => {
    const rs = [row('see https://example.com/a', 30), row('bcdefghijklmnopqrstu', 30)];
    expect(tap(rs, 0, 'https')).toBe('https://example.com/a');
    expect(tap(rs, 1, 'bcd')).toBeNull();
  });

  it('takes only URL characters from the row it carries on to', () => {
    const rs = rows('  x https://example.com/abc', '  defg│hijklmnopqrst');
    expect(tap(rs, 0, 'https')).toBe('https://example.com/abcdefg');
    expect(tap(rs, 1, 'hij')).toBeNull();
  });

  it('joins rows xterm wrapped itself', () => {
    const rs = [row('see https://example.com/aaaa', 28), row('bbbb/c done', 28, true)];
    expect(tap(rs, 0, 'example')).toBe('https://example.com/aaaabbbb/c');
    expect(tap(rs, 1, 'c ')).toBe('https://example.com/aaaabbbb/c');
    expect(tap(rs, 1, 'done')).toBeNull();
  });

  it('finds a long URL wherever it is tapped, however much of it is above', () => {
    // 2,400 characters above the last row.
    const first = `https://example.com/${'a'.repeat(100)}`;
    const rs = rows(first, ...Array<string>(19).fill('a'.repeat(120)), 'aaaa done');
    const url = `${first}${'a'.repeat(19 * 120 + 4)}`;
    expect(tap(rs, 20, 'aaaa')).toBe(url);
    expect(tap(rs, 0, 'https')).toBe(url);
  });

  it('leaves out trailing punctuation and brackets', () => {
    const rs = rows('  (see https://example.com/abcdefghij', '  klmnopqrstuvwxyz).');
    expect(tap(rs, 1, 'klm')).toBe('https://example.com/abcdefghijklmnopqrstuvwxyz');
    expect(tap(rs, 1, ')')).toBeNull();
    expect(tap(rs, 1, '.')).toBeNull();
  });

  it('finds the right cells after a wide character', () => {
    // 日 and 本 take two cells each.
    const cells = ['日', '', '本', '', ' ', ...Array.from('https://example.com/abcdef')];
    const rs: TermRow[] = [{ cells, wrapped: false }, row('ghijklm', cells.length)];
    expect(linkAt((i) => rs[i], 0, 5)).toBe('https://example.com/abcdefghijklm');
    expect(linkAt((i) => rs[i], 0, 1)).toBeNull();
    expect(tap(rs, 1, 'ghij')).toBe('https://example.com/abcdefghijklm');
  });

  it('tells apart two links on a row, and a link after a joined one', () => {
    const rs = rows('https://a.io https://b.io/x', 'yyyyyyyyyyyyyy https://c.io');
    expect(tap(rs, 0, 'a.io')).toBe('https://a.io');
    expect(tap(rs, 0, 'b.io')).toBe('https://b.io/xyyyyyyyyyyyyyy');
    expect(tap(rs, 1, 'y')).toBe('https://b.io/xyyyyyyyyyyyyyy');
    expect(tap(rs, 1, 'c.io')).toBe('https://c.io');
  });

  it('finds a link on one row, and nothing past it or without a host', () => {
    const rs = [row('open https://example.com now', 40), row('just https:// here', 40)];
    expect(tap(rs, 0, 'example')).toBe('https://example.com');
    expect(tap(rs, 0, 'now')).toBeNull();
    expect(tap(rs, 1, 'https')).toBeNull();
    expect(linkAt((i) => rs[i], 5, 0)).toBeNull();
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
    // As Claude draws it: the first row, then the cursor to the next row and the rest at the margin.
    const at = await screen(20, '  x https://a.io/bcd\r\n  efghij\r\nhttps://b.io/klmnopqrstu');
    expect(at(0, 4)).toBe('https://a.io/bcdefghij');
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
