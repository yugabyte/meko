import type { FileTypeCount } from '../shared/types.js';

/** A changed file: its path (never stored or sent) and its lines. */
export interface ChangedFile {
  path: string;
  additions: number;
  deletions: number;
}

/** Lists keep this many extensions; the rest are summed into `other`. */
export const FILE_TYPE_LIMIT = 25;

const EXT = /^[a-z0-9]{1,10}$/;

/**
 * The extension a file counts under: the last dot-suffix of its name, lowercased. A name with no
 * suffix, or a dotfile like `.gitignore`, is `none`; a suffix that isn't 1 to 10 letters and
 * digits is `other`, so a file name can never pass for an extension.
 */
export function extOf(path: string): string {
  const base = path.slice(path.lastIndexOf('/') + 1);
  const dot = base.lastIndexOf('.');
  if (dot <= 0 || dot === base.length - 1) return 'none';
  const ext = base.slice(dot + 1).toLowerCase();
  return EXT.test(ext) ? ext : 'other';
}

/** Sums changed files into per-extension counts, most files first, capped at FILE_TYPE_LIMIT. */
export function countFileTypes(files: ChangedFile[]): FileTypeCount[] {
  return sumFileTypes([files.map((f) => ({ ext: extOf(f.path), files: 1, additions: f.additions, deletions: f.deletions }))]);
}

/** Adds lists of per-extension counts together, capped at FILE_TYPE_LIMIT. Unknown extensions count as `other`. */
export function sumFileTypes(lists: FileTypeCount[][]): FileTypeCount[] {
  const by = new Map<string, FileTypeCount>();
  for (const list of lists) {
    for (const c of list) {
      const ext = c.ext === 'none' || c.ext === 'other' || EXT.test(c.ext) ? c.ext : 'other';
      const t = by.get(ext) ?? { ext, files: 0, additions: 0, deletions: 0 };
      t.files += num(c.files);
      t.additions += num(c.additions);
      t.deletions += num(c.deletions);
      by.set(ext, t);
    }
  }
  const sorted = [...by.values()].sort((a, b) =>
    b.files - a.files || b.additions + b.deletions - (a.additions + a.deletions) || (a.ext < b.ext ? -1 : a.ext > b.ext ? 1 : 0));
  if (sorted.length <= FILE_TYPE_LIMIT) return sorted;
  const keep = sorted.filter((c) => c.ext !== 'other').slice(0, FILE_TYPE_LIMIT - 1);
  const kept = new Set(keep.map((c) => c.ext));
  const other = { ext: 'other', files: 0, additions: 0, deletions: 0 };
  for (const c of sorted) {
    if (kept.has(c.ext)) continue;
    other.files += c.files;
    other.additions += c.additions;
    other.deletions += c.deletions;
  }
  return [...keep, other];
}

/**
 * The files in `git diff --numstat -z` output. Binary files (`-` for both counts) have 0 lines;
 * a rename counts once, under its new name.
 */
export function parseNumstatZ(out: string): ChangedFile[] {
  const parts = out.split('\0');
  const files: ChangedFile[] = [];
  for (let i = 0; i < parts.length; i++) {
    const m = /^(\d+|-)\t(\d+|-)\t(.*)$/s.exec(parts[i]);
    if (!m) continue;
    let path = m[3];
    // A rename or copy: an empty path, then the old and new paths as the next two fields.
    if (!path) {
      path = parts[i + 2] ?? '';
      i += 2;
    }
    files.push({ path, additions: m[1] === '-' ? 0 : Number(m[1]), deletions: m[2] === '-' ? 0 : Number(m[2]) });
  }
  return files;
}

/** Per-extension counts kept as JSON (pushes.file_types, the merged event's payload), or null. */
export function readFileTypes(v: unknown): FileTypeCount[] | null {
  const list = typeof v === 'string' ? safeJson(v) : v;
  if (!Array.isArray(list)) return null;
  return list.filter((c): c is FileTypeCount => !!c && typeof c === 'object' && typeof (c as FileTypeCount).ext === 'string');
}

function safeJson(s: string): unknown {
  try {
    return JSON.parse(s);
  } catch {
    return null;
  }
}

function num(v: unknown): number {
  return typeof v === 'number' && Number.isFinite(v) && v > 0 ? Math.round(v) : 0;
}
