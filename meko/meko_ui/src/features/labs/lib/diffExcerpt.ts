// Turns a unified diff back into numbered file lines, so a finding that says
// "api/spend.py:18-20" can be shown as the code it is talking about with
// those lines marked.
//
// The line numbers are the NEW file's, rebuilt from each hunk header, because
// that is the frame a reviewer quotes. Nothing here trusts the model further
// than that: a location naming a file the diff does not touch, or lines
// outside every hunk, returns null and the caller shows no excerpt rather
// than highlighting the wrong code.

export type DiffLineKind = "add" | "del" | "context";

export type DiffLine = {
  /** Line number in the new file. Null for a removed line, which has none. */
  lineNumber: number | null;
  text: string;
  kind: DiffLineKind;
};

export type DiffFile = { path: string; lines: DiffLine[] };

const HUNK_HEADER = /^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@/;

/** Strips git's `a/` / `b/` prefix and any leading slash, so the two sides of
 *  a comparison are spelled the same way. */
function normalizePath(path: string): string {
  return path.replace(/^[ab]\//, "").replace(/^\/+/, "").trim();
}

export function parseUnifiedDiff(diff: string): DiffFile[] {
  const files: DiffFile[] = [];
  let current: DiffFile | null = null;
  let newLine = 0;

  for (const raw of diff.split("\n")) {
    const gitHeader = /^diff --git a\/(\S+) b\/(\S+)/.exec(raw);
    if (gitHeader) {
      current = { path: normalizePath(gitHeader[2]), lines: [] };
      files.push(current);
      continue;
    }
    if (raw.startsWith("+++ ")) {
      const path = normalizePath(raw.slice(4));
      // A diff with no `diff --git` line (a bare `git diff` body, or a
      // patch pasted from a review tool) still names its file here.
      if (!current) {
        current = { path, lines: [] };
        files.push(current);
      } else if (path && path !== "dev/null") {
        current.path = path;
      }
      continue;
    }
    if (raw.startsWith("--- ") || raw.startsWith("index ")) continue;

    const hunk = HUNK_HEADER.exec(raw);
    if (hunk) {
      newLine = Number(hunk[1]);
      continue;
    }
    if (!current || newLine === 0) continue;

    if (raw.startsWith("+")) {
      current.lines.push({ lineNumber: newLine, text: raw.slice(1), kind: "add" });
      newLine += 1;
    } else if (raw.startsWith("-")) {
      current.lines.push({ lineNumber: null, text: raw.slice(1), kind: "del" });
    } else {
      current.lines.push({ lineNumber: newLine, text: raw.slice(1), kind: "context" });
      newLine += 1;
    }
  }
  return files;
}

export type FindingLocation = { path: string; start: number; end: number };

/** Parses a finding's `location`. The agent writes `api/spend.py:23` for one
 *  line and `api/spend.py:14-15` for a range, uses an en dash as often as a
 *  hyphen, and often tacks a note on the end — `spend.py:15 (string
 *  concatenation in SQL)`. Anything after the line number is ignored rather
 *  than rejected: an unparsed location renders no code at all, which was the
 *  common case in practice. */
export function parseFindingLocation(location: string): FindingLocation | null {
  const match = /^(.*?):(\d+)(?:\s*[-–—]\s*(\d+))?(?:\s+.*)?$/.exec(location.trim());
  if (!match) return null;
  const path = normalizePath(match[1]);
  if (!path) return null;
  const start = Number(match[2]);
  const end = match[3] ? Number(match[3]) : start;
  if (!Number.isFinite(start) || !Number.isFinite(end) || end < start) return null;
  return { path, start, end };
}

export type DiffExcerpt = {
  path: string;
  lines: DiffLine[];
  /** The range the finding named, clamped to lines the diff actually has. */
  highlightStart: number;
  highlightEnd: number;
};

/** The lines around a finding's location, with the finding's own range marked.
 *  Null when the location cannot be resolved against this diff at all, so the
 *  caller renders no excerpt rather than one pointing at unrelated code. */
export function excerptForFinding(
  files: DiffFile[],
  location: string,
  { padding = 3 }: { padding?: number } = {},
): DiffExcerpt | null {
  const parsed = parseFindingLocation(location);
  if (!parsed) return null;

  // Basename match, not equality: the agent quotes the path it was shown,
  // which may carry a leading slash or a repo prefix the diff does not.
  const wanted = parsed.path.split("/").pop();
  const file =
    files.find((f) => f.path === parsed.path) ??
    files.find((f) => f.path.split("/").pop() === wanted) ??
    (files.length === 1 ? files[0] : null);
  if (!file) return null;

  const numbered = file.lines.filter((l) => l.lineNumber !== null);
  if (numbered.length === 0) return null;
  const first = numbered[0].lineNumber as number;
  const last = numbered[numbered.length - 1].lineNumber as number;
  if (parsed.end < first || parsed.start > last) return null;

  const highlightStart = Math.max(parsed.start, first);
  const highlightEnd = Math.min(parsed.end, last);
  const from = highlightStart - padding;
  const to = highlightEnd + padding;

  return {
    path: file.path,
    lines: file.lines.filter((l) => l.lineNumber !== null && l.lineNumber >= from && l.lineNumber <= to),
    highlightStart,
    highlightEnd,
  };
}
