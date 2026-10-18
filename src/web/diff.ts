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

/** Diffs open by default until this many rows are shown, so big branches open fast. */
export const OPEN_LINE_BUDGET = 2000;
/** A file's diff with more rows than this shows only when asked for. */
export const LARGE_DIFF_ROWS = 1000;
/** Rows a large diff shows per click of Show diff or Show more. */
export const ROW_PAGE = 2000;

/**
 * The files whose diffs open by default: in order, each that fits what's left of
 * OPEN_LINE_BUDGET (a large one never does), and every file with comments, so none are hidden
 * (a large one opens to its Show diff button).
 */
export function defaultOpen(files: { path: string }[], rowsOf: (path: string) => number, hasComments: (path: string) => boolean): Set<string> {
  const open = new Set<string>();
  let shown = 0;
  for (const { path } of files) {
    const n = rowsOf(path);
    const large = n > LARGE_DIFF_ROWS;
    // A file with no rows (binary, an image) still takes a slot, so a branch of images doesn't load them all.
    if (hasComments(path) || (!large && shown + Math.max(n, 1) <= OPEN_LINE_BUDGET)) {
      open.add(path);
      if (!large) shown += n;
    }
  }
  return open;
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
