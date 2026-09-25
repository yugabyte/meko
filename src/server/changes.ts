import { lstatSync, readFileSync, readlinkSync, realpathSync, statSync } from 'node:fs';
import { isAbsolute, join, normalize, sep } from 'node:path';
import { previewImageType, type ChangedFile, type ChangeKind, type ChangeSide, type SessionChanges } from '../shared/types.js';
import { gitBuffer, gitRaw, tryGit } from './git.js';

/** A single file's patch is cut here. */
export const FILE_PATCH_CAP = 256 * 1024;
/** Patches past this total are left out (the files keep their stats). */
export const TOTAL_PATCH_CAP = 4 * 1024 * 1024;
/** Untracked files larger than this get no patch. */
const UNTRACKED_PATCH_MAX = 1024 * 1024;
/** Untracked files larger than this aren't read at all (counted as 0 lines). */
const UNTRACKED_READ_MAX = 16 * 1024 * 1024;
/** Only this many untracked files are read, so a missing .gitignore can't stall the server. */
const UNTRACKED_MAX_FILES = 2000;

// No external diff drivers or textconv, no color, paths relative to the worktree root.
const DIFF_OPTS = ['-M', '--no-color', '--no-ext-diff', '--no-textconv', '--no-relative'];

/** Where the branch left its base: merge base of HEAD and origin/<base>, else <base>. */
export async function diffBase(worktree: string, base: string): Promise<{ ref: string; sha: string } | null> {
  for (const ref of [`origin/${base}`, base]) {
    const sha = await tryGit(worktree, 'merge-base', 'HEAD', ref);
    if (sha) return { ref, sha };
  }
  return null;
}

/** Lines added/removed vs. the base, uncommitted and untracked files included. */
export async function diffStat(worktree: string, base: string): Promise<{ added: number; removed: number } | null> {
  const mb = await diffBase(worktree, base);
  if (!mb) return null;
  const out = await gitRaw(worktree, 'diff', '--numstat', '-z', ...DIFF_OPTS, mb.sha).catch(() => null);
  if (out === null) return null;
  let added = 0, removed = 0;
  for (const n of parseNumstat(out).values()) {
    added += n.added;
    removed += n.removed;
  }
  for (const u of await untracked(worktree, false)) added += u.added;
  return { added, removed };
}

/** Every file changed in the worktree vs. its base, with a patch per file. Null when there's no merge base. */
export async function changes(worktree: string, base: string): Promise<SessionChanges | null> {
  const mb = await diffBase(worktree, base);
  if (!mb) return null;
  const [status, numstat, porcelain] = await Promise.all([
    gitRaw(worktree, 'diff', '--name-status', '-z', ...DIFF_OPTS, mb.sha),
    gitRaw(worktree, 'diff', '--numstat', '-z', ...DIFF_OPTS, mb.sha),
    gitRaw(worktree, 'status', '--porcelain', '-z', '--untracked-files=no'),
  ]);
  const entries = parseNameStatus(status);
  const counts = parseNumstat(numstat);
  const dirty = parsePorcelain(porcelain);
  // One diff for the whole tree, split per file; same args, so same order as --name-status.
  const whole = await gitRaw(worktree, 'diff', ...DIFF_OPTS, mb.sha).catch(() => null);
  const chunks = whole === null ? null : splitPatch(whole);
  const patches = chunks && chunks.length === entries.length ? chunks : null;

  const files: ChangedFile[] = entries.map((e, i) => {
    const n = counts.get(e.path) ?? { added: 0, removed: 0, binary: false };
    return {
      path: e.path,
      oldPath: e.oldPath,
      kind: e.kind,
      added: n.added,
      removed: n.removed,
      binary: n.binary,
      uncommitted: dirty.has(e.path) || (e.oldPath !== null && dirty.has(e.oldPath)),
      patch: n.binary || !patches ? null : patches[i],
      truncated: false,
    };
  });
  files.push(...(await untracked(worktree, true)));
  files.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  const omittedPatches = capPatches(files) + (patches ? 0 : entries.filter((e) => !counts.get(e.path)?.binary).length);
  return {
    base: mb.ref,
    files,
    added: files.reduce((t, f) => t + f.added, 0),
    removed: files.reduce((t, f) => t + f.removed, 0),
    omittedPatches,
  };
}

/** Cuts each patch at FILE_PATCH_CAP and drops patches past TOTAL_PATCH_CAP. Returns how many were dropped. */
export function capPatches(files: ChangedFile[]): number {
  let total = 0, omitted = 0;
  for (const f of files) {
    if (f.patch === null) continue;
    if (f.patch.length > FILE_PATCH_CAP) {
      const cut = f.patch.lastIndexOf('\n', FILE_PATCH_CAP);
      f.patch = f.patch.slice(0, cut > 0 ? cut : FILE_PATCH_CAP);
      f.truncated = true;
    }
    if (total + f.patch.length > TOTAL_PATCH_CAP) {
      f.patch = null;
      f.truncated = false;
      omitted++;
      continue;
    }
    total += f.patch.length;
  }
  return omitted;
}

/** Largest image the Changes panel previews. */
export const IMAGE_PREVIEW_MAX = 20 * 1024 * 1024;

/** A repo-relative path that stays inside the repo: no absolute paths, `..` segments or NULs. */
function safeRelative(path: string): boolean {
  if (!path || path.includes('\0') || isAbsolute(path)) return false;
  const n = normalize(path);
  return n !== '..' && !n.startsWith(`..${sep}`) && !n.split(sep).includes('..');
}

/**
 * One side of a changed image for the Changes panel: the base version (`old`, from the merge
 * base in git) or the worktree's (`new`, uncommitted edits included). Only image types are
 * served. Null when that side doesn't exist (an added file has no old side, a deleted one no
 * new side) or is too large; throws on a path that isn't a previewable image inside the repo.
 */
export async function changedImage(worktree: string, base: string, path: string, side: ChangeSide): Promise<{ type: string; data: Buffer } | null> {
  const type = previewImageType(path);
  if (!type || !safeRelative(path)) throw new Error('not a previewable image in this repo');
  if (side === 'new') {
    const full = join(worktree, path);
    let real: string;
    try {
      real = realpathSync(full);
    } catch {
      return null;
    }
    // A symlink must not lead out of the worktree.
    const root = realpathSync(worktree);
    if (real !== root && !real.startsWith(root + sep)) throw new Error('not a previewable image in this repo');
    const st = statSync(real);
    if (!st.isFile() || st.size > IMAGE_PREVIEW_MAX) return null;
    return { type, data: readFileSync(real) };
  }
  const mb = await diffBase(worktree, base);
  if (!mb) return null;
  const spec = `${mb.sha}:${path.split(sep).join('/')}`;
  const size = Number(await tryGit(worktree, 'cat-file', '-s', spec));
  if (!size || size > IMAGE_PREVIEW_MAX) return null;
  return { type, data: await gitBuffer(worktree, 'cat-file', 'blob', spec) };
}

/** Splits `git diff` output into one chunk per file, at each `diff --git ` line. */
export function splitPatch(out: string): string[] {
  if (!out) return [];
  const chunks: string[] = [];
  let start = 0;
  for (let i = out.indexOf('\ndiff --git '); i !== -1; i = out.indexOf('\ndiff --git ', i + 1)) {
    chunks.push(out.slice(start, i));
    start = i + 1;
  }
  chunks.push(out.slice(start).replace(/\n$/, ''));
  return chunks;
}

/** `git diff --name-status -z` → entries in diff order. */
export function parseNameStatus(out: string): { path: string; oldPath: string | null; kind: ChangeKind }[] {
  const t = out.split('\0');
  const res: { path: string; oldPath: string | null; kind: ChangeKind }[] = [];
  for (let i = 0; i < t.length && t[i]; ) {
    const code = t[i][0];
    if (code === 'R' || code === 'C') {
      res.push({ path: t[i + 2], oldPath: code === 'R' ? t[i + 1] : null, kind: code === 'R' ? 'renamed' : 'added' });
      i += 3;
    } else {
      res.push({ path: t[i + 1], oldPath: null, kind: code === 'A' ? 'added' : code === 'D' ? 'deleted' : 'modified' });
      i += 2;
    }
  }
  return res;
}

/** `git diff --numstat -z` → counts by (new) path. Binary files show `-` and count as 0. */
export function parseNumstat(out: string): Map<string, { added: number; removed: number; binary: boolean }> {
  const t = out.split('\0');
  const res = new Map<string, { added: number; removed: number; binary: boolean }>();
  for (let i = 0; i < t.length && t[i]; ) {
    const [a, r, ...rest] = t[i].split('\t');
    let path = rest.join('\t');
    i++;
    if (path === '') {
      // Rename or copy: the old and new paths follow as separate fields.
      path = t[i + 1];
      i += 2;
    }
    const binary = a === '-';
    res.set(path, { added: binary ? 0 : Number(a), removed: binary ? 0 : Number(r), binary });
  }
  return res;
}

/** Paths with uncommitted changes, from `git status --porcelain -z`. */
function parsePorcelain(out: string): Set<string> {
  const t = out.split('\0');
  const res = new Set<string>();
  for (let i = 0; i < t.length && t[i]; i++) {
    const xy = t[i].slice(0, 2);
    res.add(t[i].slice(3));
    if (/[RC]/.test(xy)) res.add(t[++i]);
  }
  return res;
}

/** Untracked, non-ignored files as changes, read directly (no git per file). */
async function untracked(worktree: string, withPatches: boolean): Promise<ChangedFile[]> {
  const out = await gitRaw(worktree, 'ls-files', '--others', '--exclude-standard', '-z').catch(() => '');
  const paths = out.split('\0').filter((p) => p && !p.endsWith('/')).slice(0, UNTRACKED_MAX_FILES);
  const res: ChangedFile[] = [];
  for (const path of paths) {
    const f: ChangedFile = {
      path, oldPath: null, kind: 'untracked', added: 0, removed: 0, binary: false, uncommitted: true, patch: null, truncated: false,
    };
    res.push(f);
    const full = join(worktree, path);
    let text: string;
    try {
      const st = lstatSync(full);
      if (st.isSymbolicLink()) text = readlinkSync(full);
      else if (!st.isFile() || st.size > UNTRACKED_READ_MAX) continue;
      else {
        const buf = readFileSync(full);
        if (buf.subarray(0, 8000).includes(0)) {
          f.binary = true;
          continue;
        }
        text = buf.toString('utf8');
        if (st.size > UNTRACKED_PATCH_MAX) {
          f.added = countLines(text);
          continue;
        }
      }
    } catch {
      continue;
    }
    f.added = countLines(text);
    if (withPatches) f.patch = newFilePatch(text);
  }
  return res;
}

function countLines(text: string): number {
  if (!text) return 0;
  let n = 0;
  for (let i = text.indexOf('\n'); i !== -1; i = text.indexOf('\n', i + 1)) n++;
  return text.endsWith('\n') ? n : n + 1;
}

/** A unified diff hunk that adds the whole file. */
export function newFilePatch(text: string): string {
  if (!text) return '';
  const noEol = !text.endsWith('\n');
  const lines = (noEol ? text : text.slice(0, -1)).split('\n');
  return [`@@ -0,0 +1,${lines.length} @@`, ...lines.map((l) => `+${l}`), ...(noEol ? ['\\ No newline at end of file'] : [])].join('\n');
}
