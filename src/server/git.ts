import { execFile } from 'node:child_process';
import { existsSync, readdirSync, statSync } from 'node:fs';
import { basename, join } from 'node:path';
import { promisify } from 'node:util';

const exec = promisify(execFile);

export async function git(cwd: string, ...args: string[]): Promise<string> {
  return (await gitRaw(cwd, ...args)).trim();
}

/** Like git(), but stdout is returned untrimmed (porcelain lines can start with a space). */
export async function gitRaw(cwd: string, ...args: string[]): Promise<string> {
  const { stdout } = await exec('git', args, { cwd, maxBuffer: 32 * 1024 * 1024, env: { ...process.env, GIT_TERMINAL_PROMPT: '0' } });
  return stdout;
}

/** Like gitRaw(), but stdout is returned as bytes (for binary blobs). */
export async function gitBuffer(cwd: string, ...args: string[]): Promise<Buffer> {
  const { stdout } = await exec('git', args, {
    cwd, encoding: 'buffer', maxBuffer: 64 * 1024 * 1024, env: { ...process.env, GIT_TERMINAL_PROMPT: '0' },
  });
  return stdout;
}

export async function tryGit(cwd: string, ...args: string[]): Promise<string | null> {
  try {
    return await git(cwd, ...args);
  } catch {
    return null;
  }
}

/** A directory is a repo (not a worktree) when .git is a directory. */
export function isMainRepo(dir: string): boolean {
  try {
    return statSync(join(dir, '.git')).isDirectory();
  } catch {
    return false;
  }
}

export interface ScannedRepo {
  path: string;
  name: string;
  remote: string | null;
  branch: string | null;
  lastTouchedAt: number;
}

/**
 * Lists git repos under the given roots. depth 1 = direct children only.
 * Worktrees (.git is a file) and non-repos are skipped.
 */
export async function scanRepos(roots: string[], depth: number): Promise<ScannedRepo[]> {
  const found: string[] = [];
  const walk = (dir: string, level: number) => {
    let entries: string[];
    try {
      entries = readdirSync(dir);
    } catch {
      return;
    }
    for (const e of entries) {
      if (e.startsWith('.')) continue;
      const p = join(dir, e);
      try {
        if (!statSync(p).isDirectory()) continue;
      } catch {
        continue;
      }
      if (isMainRepo(p)) found.push(p);
      else if (level < depth) walk(p, level + 1);
    }
  };
  for (const r of roots) walk(r, 1);
  return Promise.all(
    found.map(async (p) => {
      const [remote, branch] = await Promise.all([
        tryGit(p, 'config', '--get', 'remote.origin.url'),
        tryGit(p, 'symbolic-ref', '--quiet', '--short', 'HEAD'),
      ]);
      return { path: p, name: basename(p), remote: remote ? prettyRemote(remote) : null, branch, lastTouchedAt: lastTouched(p) };
    }),
  );
}

function lastTouched(repo: string): number {
  // HEAD, index and FETCH_HEAD move on checkout, commit, add and fetch.
  let t = 0;
  for (const f of ['HEAD', 'index', 'FETCH_HEAD', 'logs/HEAD']) {
    try {
      t = Math.max(t, statSync(join(repo, '.git', f)).mtimeMs);
    } catch {
      /* missing */
    }
  }
  return Math.round(t);
}

/** git@github.com:acme/x.git → github.com/acme/x */
export function prettyRemote(url: string): string {
  return url
    .replace(/^git@([^:]+):/, '$1/')
    .replace(/^[a-z+]+:\/\/(?:[^@/]+@)?/, '')
    .replace(/\.git$/, '');
}

/** owner/name for github.com remotes, else null. */
export function githubRepo(remoteUrl: string | null): string | null {
  if (!remoteUrl) return null;
  const m = prettyRemote(remoteUrl).match(/^github\.com\/([^/]+\/[^/]+)$/);
  return m ? m[1] : null;
}

export async function defaultBranch(repo: string): Promise<string> {
  const head = await tryGit(repo, 'symbolic-ref', '--quiet', '--short', 'refs/remotes/origin/HEAD');
  if (head) return head.replace(/^origin\//, '');
  for (const b of ['main', 'master']) {
    if (await tryGit(repo, 'rev-parse', '--verify', '--quiet', `refs/heads/${b}`)) return b;
  }
  return (await tryGit(repo, 'symbolic-ref', '--quiet', '--short', 'HEAD')) ?? 'main';
}

export async function listBranches(repo: string): Promise<string[]> {
  const local = (await tryGit(repo, 'for-each-ref', '--format=%(refname:short)', '--sort=-committerdate', 'refs/heads')) ?? '';
  return local.split('\n').filter(Boolean);
}

/** What happened to the local base branch before a new session branched from it. */
export type SyncResult =
  | { kind: 'updated' | 'current' | 'no-upstream' }
  | { kind: 'skipped'; reason: string };

/**
 * Creates a worktree. A new branch starts from the local <base>, fast-forwarded
 * to origin/<base> first (after a fetch) so new work starts from the latest
 * upstream. If <base> can't be fast-forwarded, the branch starts from
 * origin/<base> instead. Returns null when the branch already existed.
 */
export async function addWorktree(repo: string, path: string, branch: string, base: string): Promise<SyncResult | null> {
  await tryGit(repo, 'fetch', '--quiet', 'origin', base);
  if (await revParse(repo, `refs/heads/${branch}`)) {
    await git(repo, 'worktree', 'add', path, branch);
    return null;
  }
  const sync = await fastForward(repo, base);
  const start = sync.kind === 'skipped' && (await revParse(repo, `refs/remotes/origin/${base}`)) ? `origin/${base}` : base;
  await git(repo, 'worktree', 'add', '--no-track', '-b', branch, path, start);
  return sync;
}

/**
 * Fast-forwards the local <branch> to origin/<branch> (already fetched); never
 * forces. Where the branch is checked out (usually the main checkout) this is
 * `merge --ff-only`, which git refuses when it would overwrite local edits;
 * elsewhere it is a compare-and-swap of the ref.
 */
export async function fastForward(repo: string, branch: string): Promise<SyncResult> {
  const remote = await revParse(repo, `refs/remotes/origin/${branch}`);
  if (!remote) return { kind: 'no-upstream' };
  const local = await revParse(repo, `refs/heads/${branch}`);
  if (!local) return { kind: 'skipped', reason: `there's no local ${branch} branch` };
  if (local === remote) return { kind: 'current' };
  if ((await tryGit(repo, 'merge-base', '--is-ancestor', local, remote)) === null) {
    return { kind: 'skipped', reason: `local ${branch} has commits that aren't on origin` };
  }
  const at = await checkedOutAt(repo, branch);
  const err = at
    ? await gitError(at, 'merge', '--ff-only', '--quiet', remote)
    : await gitError(repo, 'update-ref', `refs/heads/${branch}`, remote, local);
  if (!err) return { kind: 'updated' };
  return { kind: 'skipped', reason: at ? `git couldn't fast-forward it in ${at}: ${err}` : err };
}

/** The worktree (main checkout included) that has <branch> checked out, if any. */
async function checkedOutAt(repo: string, branch: string): Promise<string | null> {
  const out = (await tryGit(repo, 'worktree', 'list', '--porcelain')) ?? '';
  for (const block of out.split('\n\n')) {
    const lines = block.split('\n');
    if (lines.includes(`branch refs/heads/${branch}`)) {
      const wt = lines.find((l) => l.startsWith('worktree '));
      if (wt) return wt.slice('worktree '.length);
    }
  }
  return null;
}

async function revParse(repo: string, ref: string): Promise<string | null> {
  return tryGit(repo, 'rev-parse', '--verify', '--quiet', ref);
}

/** Runs git; null on success, else the first line of what git said. */
async function gitError(cwd: string, ...args: string[]): Promise<string | null> {
  try {
    await git(cwd, ...args);
    return null;
  } catch (e) {
    const err = e as { stderr?: string; message?: string };
    const text = (err.stderr || err.message || '').trim();
    return text.split('\n')[0].replace(/^(fatal|error): /, '') || 'git failed';
  }
}

/** Recreates a missing worktree from its (still existing) branch. */
export async function restoreWorktree(repo: string, path: string, branch: string, base: string): Promise<void> {
  if (existsSync(path)) return;
  await tryGit(repo, 'worktree', 'prune');
  await addWorktree(repo, path, branch, base);
}

export async function worktreeClean(path: string): Promise<boolean> {
  const s = await tryGit(path, 'status', '--porcelain');
  return s === '';
}

/** Changed or untracked files in a worktree; 0 when it is missing. */
export async function uncommittedCount(path: string): Promise<number> {
  if (!existsSync(path)) return 0;
  const s = await tryGit(path, 'status', '--porcelain');
  return s ? s.split('\n').filter(Boolean).length : 0;
}

/** Commits on a branch that no remote has and the local base lacks; 0 when the branch is gone. */
export async function unpushedCount(repo: string, branch: string, base: string): Promise<number> {
  if (!(await tryGit(repo, 'rev-parse', '--verify', '--quiet', `refs/heads/${branch}`))) return 0;
  const baseRef = (await tryGit(repo, 'rev-parse', '--verify', '--quiet', `refs/heads/${base}`)) ? [base] : [];
  const n = await tryGit(repo, 'rev-list', '--count', branch, '--not', '--remotes', ...baseRef);
  return n ? Number(n) : 0;
}

export async function removeWorktree(repo: string, path: string, force = false): Promise<boolean> {
  if (!existsSync(path)) {
    await tryGit(repo, 'worktree', 'prune');
    return true;
  }
  const args = ['worktree', 'remove', ...(force ? ['--force'] : []), path];
  return (await tryGit(repo, ...args)) !== null;
}

export async function deleteBranch(repo: string, branch: string): Promise<void> {
  await tryGit(repo, 'branch', '-D', branch);
}

export async function cloneRepo(url: string, dest: string): Promise<void> {
  const { stderr } = await exec('git', ['clone', '--quiet', url, dest], { env: { ...process.env, GIT_TERMINAL_PROMPT: '0' } });
  void stderr;
}

export function repoNameFromUrl(url: string): string {
  return basename(prettyRemote(url)).replace(/\.git$/, '') || 'repo';
}
