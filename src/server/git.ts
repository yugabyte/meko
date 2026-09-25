import { execFile } from 'node:child_process';
import { existsSync, readdirSync, statSync } from 'node:fs';
import { basename, join } from 'node:path';
import { promisify } from 'node:util';

const exec = promisify(execFile);

export async function git(cwd: string, ...args: string[]): Promise<string> {
  const { stdout } = await exec('git', args, { cwd, maxBuffer: 32 * 1024 * 1024, env: { ...process.env, GIT_TERMINAL_PROMPT: '0' } });
  return stdout.trim();
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

/**
 * Creates a worktree on a new branch. The base is origin/<base> when it exists
 * (after a fetch) so new work starts from the latest upstream.
 */
export async function addWorktree(repo: string, path: string, branch: string, base: string): Promise<void> {
  await tryGit(repo, 'fetch', '--quiet', 'origin', base);
  const remoteRef = await tryGit(repo, 'rev-parse', '--verify', '--quiet', `refs/remotes/origin/${base}`);
  const start = remoteRef ? `origin/${base}` : base;
  if (await tryGit(repo, 'rev-parse', '--verify', '--quiet', `refs/heads/${branch}`)) {
    await git(repo, 'worktree', 'add', path, branch);
  } else {
    await git(repo, 'worktree', 'add', '--no-track', '-b', branch, path, start);
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

export async function diffStat(worktree: string, base: string): Promise<{ added: number; removed: number } | null> {
  const mergeBase =
    (await tryGit(worktree, 'merge-base', 'HEAD', `origin/${base}`)) ?? (await tryGit(worktree, 'merge-base', 'HEAD', base));
  if (!mergeBase) return null;
  // Working tree vs merge base, so uncommitted work counts too.
  const out = await tryGit(worktree, 'diff', '--numstat', mergeBase);
  if (out === null) return null;
  let added = 0, removed = 0;
  for (const line of out.split('\n')) {
    const [a, r] = line.split('\t');
    if (a && a !== '-') added += Number(a);
    if (r && r !== '-') removed += Number(r);
  }
  return { added, removed };
}

export async function cloneRepo(url: string, dest: string): Promise<void> {
  const { stderr } = await exec('git', ['clone', '--quiet', url, dest], { env: { ...process.env, GIT_TERMINAL_PROMPT: '0' } });
  void stderr;
}

export function repoNameFromUrl(url: string): string {
  return basename(prettyRemote(url)).replace(/\.git$/, '') || 'repo';
}
