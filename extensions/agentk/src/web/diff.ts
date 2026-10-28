// Unified diff parsing for the Changes panel.

export interface DiffLine {
  kind: 'hunk' | 'add' | 'del' | 'ctx' | 'meta' | 'note';
  text: string;
  old: number | null;
  new: number | null;
}

/**
 * Unified diff → display lines with old/new line numbers. The git file headers are dropped.
 * The hunk header's line counts say where each hunk ends, so an empty line inside a hunk is an
 * empty context line (git writes those as "" rather than " " with diff.suppressBlankEmpty).
 */
export function parsePatch(patch: string): DiffLine[] {
  const out: DiffLine[] = [];
  let o = 0, n = 0, inHunk = false;
  /** Old and new lines the current hunk still has to show. */
  let oLeft = 0, nLeft = 0;
  for (const line of patch.split('\n')) {
    const h = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/.exec(line);
    if (h) {
      o = Number(h[1]);
      n = Number(h[3]);
      oLeft = h[2] === undefined ? 1 : Number(h[2]);
      nLeft = h[4] === undefined ? 1 : Number(h[4]);
      inHunk = true;
      out.push({ kind: 'hunk', text: line, old: null, new: null });
    } else if (!inHunk) {
      if (!/^(diff --git |index |--- |\+\+\+ )/.test(line) && line) out.push({ kind: 'meta', text: line, old: null, new: null });
    } else if (!line && (oLeft <= 0 || nLeft <= 0)) {
      continue; // a trailing newline after the hunk
    } else if (line.startsWith('+')) {
      out.push({ kind: 'add', text: line.slice(1), old: null, new: n++ });
      nLeft--;
    } else if (line.startsWith('-')) {
      out.push({ kind: 'del', text: line.slice(1), old: o++, new: null });
      oLeft--;
    } else if (line.startsWith('\\')) {
      out.push({ kind: 'note', text: line.slice(2), old: null, new: null });
    } else {
      out.push({ kind: 'ctx', text: line.slice(1), old: o++, new: n++ });
      oLeft--;
      nLeft--;
    }
  }
  return out;
}

/** How many diff rows the Changes panel draws at a time. */
export interface RowBudget {
  /** Diffs open by default until this many rows are shown, so big branches open fast. */
  open: number;
  /** A file's diff with more rows than this shows only when asked for. */
  large: number;
  /** Rows a large diff shows per click of Show diff or Show more. */
  page: number;
}

export const DESKTOP_BUDGET: RowBudget = { open: 2000, large: 1000, page: 2000 };
/** A phone lays rows out several times slower than a desktop, so it draws fewer at a time. */
export const PHONE_BUDGET: RowBudget = { open: 1000, large: 500, page: 500 };

/**
 * The files whose diffs open by default: in order, each that fits what's left of the budget's
 * `open` rows (a large one never does), and every file with comments, so none are hidden (a large
 * one opens to its Show diff button).
 */
export function defaultOpen(
  files: { path: string }[],
  rowsOf: (path: string) => number,
  hasComments: (path: string) => boolean,
  budget: RowBudget,
): Set<string> {
  const open = new Set<string>();
  let shown = 0;
  for (const { path } of files) {
    const n = rowsOf(path);
    const large = n > budget.large;
    // A file with no rows (binary, an image) still takes a slot, so a branch of images doesn't load them all.
    if (hasComments(path) || (!large && shown + Math.max(n, 1) <= budget.open)) {
      open.add(path);
      if (!large) shown += Math.max(n, 1);
    }
  }
  return open;
}

/**
 * The rows of each patch in `files`, by patch text: `prev`'s rows for a patch it already has, so a
 * file whose patch didn't change keeps the same rows across a refetch.
 */
export function reparse(prev: ReadonlyMap<string, DiffLine[]>, files: { patch: string | null }[]): Map<string, DiffLine[]> {
  const next = new Map<string, DiffLine[]>();
  for (const { patch } of files) if (patch !== null && !next.has(patch)) next.set(patch, prev.get(patch) ?? parsePatch(patch));
  return next;
}

/** The line's number on a comment side: `new` for added and unchanged lines, `old` for removed ones. */
export function sideLine(l: DiffLine, side: 'old' | 'new'): number | null {
  if (side === 'new') return l.kind === 'add' || l.kind === 'ctx' ? l.new : null;
  return l.kind === 'del' ? l.old : null;
}

/** Whether the diff shows line `n` on this side (so a comment there that no longer matches has changed). */
export function showsLine(lines: DiffLine[], side: 'old' | 'new', n: number): boolean {
  return lines.some((l) => sideLine(l, side) === n);
}

/**
 * The lines a comment covers when it's picked from line `anchor` to line `n` on one side, with
 * their rows. It stops where the side's line numbers skip (a hunk's edge), so it never covers lines
 * the diff doesn't show. Null when the diff doesn't show `anchor` on that side.
 */
export function pickRange(lines: DiffLine[], side: 'old' | 'new', anchor: number, n: number): { from: number; to: number; rows: DiffLine[] } | null {
  const rows = lines.filter((l) => sideLine(l, side) !== null);
  const num = (k: number) => sideLine(rows[k], side) as number;
  const at = rows.findIndex((_, k) => num(k) === anchor);
  if (at < 0) return null;
  let lo = at, hi = at;
  while (hi + 1 < rows.length && num(hi + 1) === num(hi) + 1 && num(hi + 1) <= n) hi++;
  while (lo > 0 && num(lo - 1) === num(lo) - 1 && num(lo - 1) >= n) lo--;
  return { from: num(lo), to: num(hi), rows: rows.slice(lo, hi + 1) };
}

export interface Anchorable {
  side: 'old' | 'new';
  endLine: number;
  excerpt: string;
}

/** Stored excerpt lines this long may have been cut short by the server; shorter ones must match exactly. */
const CUT_MIN = 200;

/**
 * Whether a row's text is the excerpt line `want`: the same, or, for a long line the server
 * shortened, the row's text starting or ending with it (a trailing or leading "…" ignored).
 */
export function sameLine(text: string, want: string): boolean {
  if (text === want) return true;
  const core = want.replace(/^…|…$/g, '');
  return core.length >= CUT_MIN && text.length > core.length && (text.startsWith(core) || text.endsWith(core));
}

/**
 * Where a comment belongs in a file's current diff: the index of the row it follows, or null
 * when its lines are gone (outdated). The row at the stored end line wins if its text still
 * matches; otherwise the nearest row on the same side whose text matches, preferring rows
 * where the whole excerpt still matches over rows where only the last line does.
 */
export function anchorComment(lines: DiffLine[], c: Anchorable): number | null {
  const side: { i: number; n: number; text: string }[] = [];
  lines.forEach((l, i) => {
    const n = sideLine(l, c.side);
    if (n !== null) side.push({ i, n, text: l.text });
  });
  const want = c.excerpt.split('\n');
  const last = want[want.length - 1];
  const whole = (k: number) => want.every((t, j) => {
    const row = side[k - want.length + 1 + j];
    return row !== undefined && sameLine(row.text, t);
  });
  const at = side.findIndex((s) => s.n === c.endLine);
  if (at >= 0 && sameLine(side[at].text, last)) return side[at].i;
  let best: { i: number; full: boolean; dist: number } | null = null;
  for (let k = 0; k < side.length; k++) {
    if (!sameLine(side[k].text, last)) continue;
    const cand = { i: side[k].i, full: whole(k), dist: Math.abs(side[k].n - c.endLine) };
    if (!best || (cand.full && !best.full) || (cand.full === best.full && cand.dist < best.dist)) best = cand;
  }
  return best?.i ?? null;
}

/**
 * `next`, with each item whose fields all equal those of the previous item under its key replaced
 * by that previous object, so a memoized view of an unchanged item isn't drawn again after a refetch.
 */
export function keepUnchanged<T extends object>(prev: readonly T[], next: T[], key: (t: T) => string): T[] {
  const old = new Map(prev.map((t) => [key(t), t]));
  return next.map((t) => {
    const o = old.get(key(t));
    return o && sameFields(o, t) ? o : t;
  });
}

function sameFields(a: object, b: object): boolean {
  const ka = Object.keys(a);
  return ka.length === Object.keys(b).length && ka.every((k) => Object.is((a as Record<string, unknown>)[k], (b as Record<string, unknown>)[k]));
}

/** Whether two lists hold the same items in the same order, by identity. */
export function sameItems<T>(a: readonly T[], b: readonly T[]): boolean {
  return a.length === b.length && a.every((x, i) => x === b[i]);
}
