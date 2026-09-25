// Unified diff parsing for the Changes panel.

export interface DiffLine {
  kind: 'hunk' | 'add' | 'del' | 'ctx' | 'meta' | 'note';
  text: string;
  old: number | null;
  new: number | null;
}

/** Unified diff → display lines with old/new line numbers. The git file headers are dropped. */
export function parsePatch(patch: string): DiffLine[] {
  const out: DiffLine[] = [];
  let o = 0, n = 0, inHunk = false;
  for (const line of patch.split('\n')) {
    const h = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line);
    if (h) {
      o = Number(h[1]);
      n = Number(h[2]);
      inHunk = true;
      out.push({ kind: 'hunk', text: line, old: null, new: null });
    } else if (!inHunk) {
      if (!/^(diff --git |index |--- |\+\+\+ )/.test(line) && line) out.push({ kind: 'meta', text: line, old: null, new: null });
    } else if (!line) {
      continue; // a trailing newline; real empty context lines are " "
    } else if (line.startsWith('+')) {
      out.push({ kind: 'add', text: line.slice(1), old: null, new: n++ });
    } else if (line.startsWith('-')) {
      out.push({ kind: 'del', text: line.slice(1), old: o++, new: null });
    } else if (line.startsWith('\\')) {
      out.push({ kind: 'note', text: line.slice(2), old: null, new: null });
    } else {
      out.push({ kind: 'ctx', text: line.slice(1), old: o++, new: n++ });
    }
  }
  return out;
}

/** The line's number on a comment side: `new` for added and unchanged lines, `old` for removed ones. */
export function sideLine(l: DiffLine, side: 'old' | 'new'): number | null {
  if (side === 'new') return l.kind === 'add' || l.kind === 'ctx' ? l.new : null;
  return l.kind === 'del' ? l.old : null;
}

export interface Anchorable {
  side: 'old' | 'new';
  endLine: number;
  excerpt: string;
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
  const whole = (k: number) => want.every((t, j) => side[k - want.length + 1 + j]?.text === t);
  const at = side.findIndex((s) => s.n === c.endLine);
  if (at >= 0 && side[at].text === last) return side[at].i;
  let best: { i: number; full: boolean; dist: number } | null = null;
  for (let k = 0; k < side.length; k++) {
    if (side[k].text !== last) continue;
    const cand = { i: side[k].i, full: whole(k), dist: Math.abs(side[k].n - c.endLine) };
    if (!best || (cand.full && !best.full) || (cand.full === best.full && cand.dist < best.dist)) best = cand;
  }
  return best?.i ?? null;
}
