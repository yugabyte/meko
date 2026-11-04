// The link under a finger in the terminal, for a tap on a phone. Mouse clicks go through
// @xterm/addon-web-links; this finds the same links, and also joins a URL that Claude Code broke
// over rows itself. Claude wraps its text with wrap-ansi ({ trim: false, hard: true }, as Ink's
// Text does) and writes each row on its own, so xterm never marks the next row wrapped
// (isWrapped), and the addon sees two links. wrap-ansi breaks a word only when it's longer than the
// text's whole width, and carries the rest on at the text's margin; a word that ends exactly at the
// right edge sends the next one down with its space in front, one column past the margin.

import type { IBuffer } from '@xterm/xterm';

/**
 * One terminal row: each cell's characters (' ' for a blank cell, '' for the second cell of a wide
 * character) and whether xterm wrapped the row above onto it.
 */
export interface TermRow {
  cells: string[];
  wrapped: boolean;
}

// @xterm/addon-web-links' URL pattern and check (it exports neither), so a tap and a click agree on
// what a link is: http(s) only, ending before trailing punctuation and brackets.
const URL_RE = /(https?|HTTPS?):[/]{2}[^\s"'!*(){}|\\^<>`]*[^\s"':,.!?{}|\\^~[\]`()<>]/g;

function isUrl(text: string): boolean {
  try {
    const url = new URL(text);
    const base =
      url.password && url.username
        ? `${url.protocol}//${url.username}:${url.password}@${url.host}`
        : url.username
          ? `${url.protocol}//${url.username}@${url.host}`
          : `${url.protocol}//${url.host}`;
    return text.toLocaleLowerCase().startsWith(base.toLocaleLowerCase());
  } catch {
    return false;
  }
}

/** The ASCII characters a URL can hold (RFC 3986): what a row Claude broke a URL onto may carry on with. */
const URL_CHAR = /^[A-Za-z0-9\-._~:/?#[\]@!$&'()*+,;=%]$/;

/** Rows looked at above and below the tapped one, at most. */
const MAX_ROWS = 20;

/** A bullet or list marker in front of a row's text: one symbol (●, -, *, ⎿), or a number and `.` or `)`. */
const MARKER = /^(?:[^\p{L}\p{N}]|\d+[.)])$/u;

/** Where a row's text starts: its first non-blank cell, or past a leading marker and the blanks after it. */
function margin(row: TermRow): number {
  const n = row.cells.length;
  let c = 0;
  while (c < n && row.cells[c] === ' ') c++;
  let end = c;
  while (end < n && row.cells[end] !== ' ') end++;
  if (end === n || !MARKER.test(row.cells.slice(c, end).join(''))) return c;
  while (end < n && row.cells[end] === ' ') end++;
  return end;
}

/**
 * How `row` carries on from `above`: 'soft' when xterm wrapped it, the column its text starts at
 * when Claude broke a word onto it, or null when it starts afresh. A break is taken only as
 * wrap-ansi makes one: the row above runs a word into its last cell, this row starts that word's
 * rest exactly at the row above's margin (a space in front means a new word), and the word is
 * longer than the text's width, so this row's part reaches past the column the word started at.
 */
function continuation(above: TermRow, row: TermRow): 'soft' | number | null {
  if (row.wrapped) return 'soft';
  const n = above.cells.length;
  if (n === 0 || row.cells.length !== n || !URL_CHAR.test(above.cells[n - 1]!)) return null;
  const m = margin(above);
  let start = n - 1;
  while (start > m && above.cells[start - 1] !== ' ') start--;
  if (!URL_CHAR.test(row.cells[m] ?? ' ')) return null;
  for (let c = 0; c < m; c++) if (row.cells[c] !== ' ') return null;
  let end = m;
  while (end < n && row.cells[end] !== ' ') end++;
  return end > start ? m : null;
}

/**
 * The URL whose text covers the cell at `col` (0-based) on buffer row `y`, joined across the rows
 * xterm or Claude wrapped it over, or null when there's no link there. `getRow` reads a buffer row
 * (undefined past either end).
 */
export function linkAt(getRow: (y: number) => TermRow | undefined, y: number, col: number): string | null {
  const cache = new Map<number, TermRow | undefined>();
  const read = (i: number) => {
    if (!cache.has(i)) cache.set(i, getRow(i));
    return cache.get(i);
  };
  if (!read(y)) return null;

  // The rows the link could run over: up while each row carries on from the one above, then down.
  let top = y;
  while (y - top < MAX_ROWS && read(top - 1) && continuation(read(top - 1)!, read(top)!) !== null) top--;
  let bottom = y;
  while (bottom - y < MAX_ROWS && read(bottom + 1) && continuation(read(bottom)!, read(bottom + 1)!) !== null) bottom++;

  // One string over them, with the cell each character came from. A row Claude broke a URL onto
  // adds its URL characters after the indent, then a line break (no cell) before the rest, so the
  // URL stops where they do and the rest of the row still has its own links.
  let text = '';
  const at: ({ row: number; col: number } | null)[] = [];
  const add = (s: string, row: number, c: number | null) => {
    text += s;
    for (let i = 0; i < s.length; i++) at.push(c === null ? null : { row, col: c });
  };
  for (let r = top; r <= bottom; r++) {
    const row = read(r)!;
    let from = 0;
    let urlEnd = row.cells.length;
    const how = r === top ? null : continuation(read(r - 1)!, row);
    if (typeof how === 'number') {
      from = how;
      urlEnd = from;
      while (urlEnd < row.cells.length && URL_CHAR.test(row.cells[urlEnd]!)) urlEnd++;
    }
    const next = r < bottom ? read(r + 1) : undefined;
    for (let c = from; c < row.cells.length; c++) {
      if (c === urlEnd) add('\n', r, null);
      const chars = row.cells[c]!;
      // A wide character that didn't fit in the last cell went to the next row and left this cell
      // blank: skip it, as the addon does.
      if (c === row.cells.length - 1 && chars === ' ' && next?.wrapped && next.cells[1] === '') continue;
      add(chars, r, c);
    }
  }

  for (const m of text.matchAll(URL_RE)) {
    if (!isUrl(m[0])) continue;
    for (let i = m.index; i < m.index + m[0].length; i++) {
      const p = at[i];
      if (p && p.row === y && p.col === col) return m[0];
    }
  }
  return null;
}

/** Reads a row of xterm's buffer for linkAt. */
export function bufferRow(buffer: IBuffer, y: number): TermRow | undefined {
  const line = buffer.getLine(y);
  if (!line) return undefined;
  const cell = buffer.getNullCell();
  const cells: string[] = [];
  for (let x = 0; x < line.length; x++) {
    line.getCell(x, cell);
    cells.push(cell.getWidth() === 0 ? '' : cell.getChars() || ' ');
  }
  return { cells, wrapped: line.isWrapped };
}
