import { readFileSync, realpathSync, statSync } from 'node:fs';
import { lstat, readFile, readlink } from 'node:fs/promises';
import { isAbsolute, join, normalize, sep } from 'node:path';
import { previewImageType, type ChangedFile, type ChangeKind, type ChangeSide, type DiffStat, type SessionChanges } from '../shared/types.js';
import { gitBuffer, gitRaw, tryGit } from './git.js';

/** A single file's patch is cut here. */
export const FILE_PATCH_CAP = 256 * 1024;
/** Patches past this total are left out (the files keep their stats). */
export const TOTAL_PATCH_CAP = 4 * 1024 * 1024;
/** At most this many files are listed; the totals still count the rest (see capFiles). */
export const MAX_FILES = 1000;
/** Untracked files larger than this aren't read (no patch, counted as 0 lines). */
const UNTRACKED_READ_MAX = 1024 * 1024;
/** At most this many bytes of untracked files are read per call; the rest count as 0 lines. */
const UNTRACKED_READ_BUDGET = 32 * 1024 * 1024;
/** Only this many untracked files are looked at, so a missing .gitignore can't stall the server. */
const UNTRACKED_MAX_FILES = 2000;

/**
 * The user's git config must not change the patch format we (and the Changes panel) parse:
 * blank context lines stay " ", paths aren't octal-escaped (headers are matched to files).
 */
const DIFF_CONFIG = ['-c', 'diff.suppressBlankEmpty=false', '-c', 'core.quotePath=false'];
// No external diff drivers or textconv, no color, paths relative to the worktree root,
// a/ and b/ prefixes (whatever diff.noprefix or diff.mnemonicPrefix say), submodules as one line.
const DIFF_OPTS = [
  '-M', '--no-color', '--no-ext-diff', '--no-textconv', '--no-relative', '--src-prefix=a/', '--dst-prefix=b/', '--submodule=short',
];

/** `git diff` in the worktree with the options above. */
function gitDiff(worktree: string, ...args: string[]): Promise<string> {
  return gitRaw(worktree, ...DIFF_CONFIG, 'diff', ...DIFF_OPTS, ...args);
}

/**
 * Where the branch left its base: its merge base with origin/<base> or, in a fork, upstream/<base>,
 * whichever is closer to HEAD (a fork's own <base> can lag the upstream one the branch was rebased
 * onto, and commits already on either aren't the branch's work). Origin wins a tie. The local <base>
 * is used only when neither remote has one.
 */
export async function diffBase(worktree: string, base: string): Promise<{ ref: string; sha: string } | null> {
  const found: { ref: string; sha: string }[] = [];
  for (const ref of [`origin/${base}`, `upstream/${base}`]) {
    if (!(await tryGit(worktree, 'rev-parse', '--verify', '--quiet', `refs/remotes/${ref}`))) continue;
    const sha = await tryGit(worktree, 'merge-base', 'HEAD', ref);
    if (sha) found.push({ ref, sha });
  }
  if (found.length === 2 && found[0].sha !== found[1].sha) {
    const [a, b] = await Promise.all(found.map((f) => tryGit(worktree, 'rev-list', '--count', `${f.sha}..HEAD`)));
    if (a !== null && b !== null && Number(b) < Number(a)) return found[1];
  }
  if (found.length) return found[0];
  const sha = await tryGit(worktree, 'merge-base', 'HEAD', base);
  return sha ? { ref: base, sha } : null;
}

/** Files changed and lines added/removed vs. the base, uncommitted and untracked files included. */
export async function diffStat(worktree: string, base: string): Promise<DiffStat | null> {
  const mb = await diffBase(worktree, base);
  if (!mb) return null;
  const out = await gitDiff(worktree, '--numstat', '-z', mb.sha).catch(() => null);
  if (out === null) return null;
  const counts = parseNumstat(out);
  let added = 0, removed = 0;
  for (const n of counts.values()) {
    added += n.added;
    removed += n.removed;
  }
  const extra = (await untracked(worktree, false)).files;
  for (const u of extra) added += u.added;
  return { files: counts.size + extra.length, added, removed };
}

/** A branch this many commits past its base may have moved onto another branch (see suggestBase). */
export const SUGGEST_MIN_AHEAD = 50;
/** At most this many remote branches are measured for a suggestion, the newest first. */
const SUGGEST_MAX_REFS = 50;

/**
 * A base closer to the branch than `base`, for a branch `ahead` commits past it: of origin's and
 * upstream's branches whose tips are in HEAD's history, the one fewest commits behind HEAD, when
 * that's fewer than `ahead`. Null below SUGGEST_MIN_AHEAD, which is ordinary work on a branch. The
 * branch's own copies on a remote, and `base` itself, don't count. Named without the remote.
 */
export async function suggestBase(worktree: string, base: string, branch: string, ahead: number): Promise<{ branch: string; ahead: number } | null> {
  if (ahead < SUGGEST_MIN_AHEAD) return null;
  const out = await tryGit(worktree, 'for-each-ref', '--merged', 'HEAD', '--sort=-committerdate', '--format=%(refname)', 'refs/remotes/origin', 'refs/remotes/upstream');
  const refs = (out ?? '').split('\n').flatMap((ref) => {
    const name = /^refs\/remotes\/(?:origin|upstream)\/(.+)$/.exec(ref)?.[1];
    return name && name !== 'HEAD' && name !== base && name !== branch ? [{ ref, name }] : [];
  }).slice(0, SUGGEST_MAX_REFS);
  let best: { branch: string; ahead: number } | null = null;
  for (const r of refs) {
    const count = await tryGit(worktree, 'rev-list', '--count', `${r.ref}..HEAD`);
    const n = count === null ? NaN : Number(count);
    if (n < (best?.ahead ?? ahead)) best = { branch: r.name, ahead: n };
  }
  return best;
}

/**
 * Every file changed in the worktree vs. its base, with a patch per file, up to MAX_FILES (the
 * files in `keep` first). Null when there's no merge base.
 */
export async function changes(worktree: string, base: string, keep: ReadonlySet<string> = new Set()): Promise<SessionChanges | null> {
  const mb = await diffBase(worktree, base);
  if (!mb) return null;
  const [status, numstat, porcelain, ahead] = await Promise.all([
    gitDiff(worktree, '--name-status', '-z', mb.sha),
    gitDiff(worktree, '--numstat', '-z', mb.sha),
    gitRaw(worktree, 'status', '--porcelain', '-z', '--untracked-files=no'),
    tryGit(worktree, 'rev-list', '--count', `${mb.sha}..HEAD`),
  ]);
  const entries = parseNameStatus(status);
  const counts = parseNumstat(numstat);
  const dirty = parsePorcelain(porcelain);
  // One diff for the whole tree, split per file and matched to the entries by header. Null when
  // git couldn't produce it (too large for the buffer).
  const whole = await gitDiff(worktree, mb.sha).catch(() => null);
  const patches = whole === null ? null : matchPatches(entries, splitPatch(whole));

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
  const extra = await untracked(worktree, true);
  files.push(...extra.files);
  files.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  const added = files.reduce((t, f) => t + f.added, 0);
  const removed = files.reduce((t, f) => t + f.removed, 0);
  const listed = capFiles(files, keep);
  // Patches left out before the caps: git's diff was too large, or untracked files past the read budgets.
  const left = new Set([...extra.omitted, ...(patches ? [] : entries.filter((e) => !counts.get(e.path)?.binary).map((e) => e.path))]);
  const omittedPatches = capPatches(listed) + listed.filter((f) => left.has(f.path)).length;
  return {
    base: mb.ref,
    files: listed,
    added,
    removed,
    omittedFiles: files.length - listed.length,
    omittedPatches,
    ahead: Number(ahead) || 0,
    suggestedBase: null,
  };
}

/**
 * The files to list, sorted as given: those in `keep` (files with review comments, so no comment
 * loses its file), then the rest in order, up to MAX_FILES in all.
 */
export function capFiles(files: ChangedFile[], keep: ReadonlySet<string>, max = MAX_FILES): ChangedFile[] {
  if (files.length <= max) return files;
  const picked = new Set(files.filter((f) => keep.has(f.path) || (f.oldPath !== null && keep.has(f.oldPath))).slice(0, max));
  for (const f of files) {
    if (picked.size >= max) break;
    picked.add(f);
  }
  return files.filter((f) => picked.has(f));
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

/**
 * A patch per name-status entry, found by each chunk's `diff --git` header (so a file that
 * changed between the two git calls can't shift the rest). A type change (file ↔ symlink)
 * comes as two chunks, a deletion and an addition, and they are joined. Entries with no
 * chunk get null; chunks with no entry are dropped.
 */
export function matchPatches(entries: { path: string; oldPath: string | null }[], chunks: string[]): (string | null)[] {
  const byHeader = new Map<string, number>();
  entries.forEach((e, i) => byHeader.set(`diff --git ${quotePath(`a/${e.oldPath ?? e.path}`)} ${quotePath(`b/${e.path}`)}`, i));
  const res: (string | null)[] = entries.map(() => null);
  for (const chunk of chunks) {
    const nl = chunk.indexOf('\n');
    const i = byHeader.get(nl === -1 ? chunk : chunk.slice(0, nl));
    if (i === undefined) continue;
    const prev = res[i];
    res[i] = prev === null ? chunk : joinChunks(prev, chunk);
  }
  return res;
}

/**
 * Two chunks for one path as one patch: the second's extended headers (e.g. "new file mode")
 * join the first's, then the first's hunks, then the second's.
 */
function joinChunks(a: string, b: string): string {
  const [ah, ab] = headAndHunks(a);
  const [bh, bb] = headAndHunks(b);
  // Its mode lines; not its `diff --git`, `index` or file name lines.
  const bMeta = bh.split('\n').slice(1).filter((l) => !/^(index |--- |\+\+\+ )/.test(l)).join('\n');
  return [ah, bMeta, ab, bb].filter(Boolean).join('\n');
}

/** A chunk's header lines and its hunks. */
function headAndHunks(c: string): [string, string] {
  const at = c.indexOf('\n@@ ');
  return at === -1 ? [c, ''] : [c.slice(0, at), c.slice(at + 1)];
}

/** A path as git writes it in a `diff --git` header with core.quotePath=false. */
export function quotePath(p: string): string {
  const named: Record<string, string> = { '\x07': 'a', '\b': 'b', '\t': 't', '\n': 'n', '\v': 'v', '\f': 'f', '\r': 'r', '"': '"', '\\': '\\' };
  let out = '', quoted = false;
  for (const c of p) {
    const code = c.charCodeAt(0);
    if (code < 0x20 || code === 0x7f || c === '"' || c === '\\') {
      quoted = true;
      out += `\\${named[c] ?? code.toString(8).padStart(3, '0')}`;
    } else {
      out += c;
    }
  }
  return quoted ? `"${out}"` : p;
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

/**
 * Untracked, non-ignored files as changes, read directly (no git per file). Reads are async
 * and bounded (per file and in total), and patches stop once they'd pass TOTAL_PATCH_CAP;
 * `omitted` names the files left without a patch because of those totals.
 */
async function untracked(worktree: string, withPatches: boolean): Promise<{ files: ChangedFile[]; omitted: string[] }> {
  const out = await gitRaw(worktree, 'ls-files', '--others', '--exclude-standard', '-z').catch(() => '');
  const paths = out.split('\0').filter((p) => p && !p.endsWith('/')).slice(0, UNTRACKED_MAX_FILES);
  const files: ChangedFile[] = [];
  const omitted: string[] = [];
  let read = 0, patchBytes = 0;
  for (const path of paths) {
    const f: ChangedFile = {
      path, oldPath: null, kind: 'untracked', added: 0, removed: 0, binary: false, uncommitted: true, patch: null, truncated: false,
    };
    files.push(f);
    const full = join(worktree, path);
    let text: string;
    try {
      const st = await lstat(full);
      if (st.isSymbolicLink()) text = await readlink(full);
      else if (!st.isFile() || st.size > UNTRACKED_READ_MAX) continue;
      else if (read + st.size > UNTRACKED_READ_BUDGET) {
        if (withPatches) omitted.push(path);
        continue;
      } else {
        const buf = await readFile(full);
        read += buf.length;
        if (buf.subarray(0, 8000).includes(0)) {
          f.binary = true;
          continue;
        }
        text = buf.toString('utf8');
      }
    } catch {
      continue;
    }
    f.added = countLines(text);
    if (!withPatches) continue;
    if (patchBytes >= TOTAL_PATCH_CAP) {
      omitted.push(path);
      continue;
    }
    f.patch = newFilePatch(text);
    patchBytes += f.patch.length;
  }
  return { files, omitted };
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
