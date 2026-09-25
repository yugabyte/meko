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
