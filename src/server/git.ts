import { execFile } from 'node:child_process';
import { existsSync, mkdirSync, readdirSync, realpathSync, rmSync, statSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { promisify } from 'node:util';
import { normaliseRemote, remoteRepoName, shortRemote } from '../shared/datapacks.js';
import { repoKey } from './paths.js';

const exec = promisify(execFile);

/**
 * No password prompts. No optional locks: our reads (status, diff) must never take
 * index.lock while Claude commits in the same worktree; locks git needs are still taken.
 */
const GIT_ENV = { GIT_TERMINAL_PROMPT: '0', GIT_OPTIONAL_LOCKS: '0' };

export async function git(cwd: string, ...args: string[]): Promise<string> {
  return (await gitRaw(cwd, ...args)).trim();
}

/** Like git(), but stdout is returned untrimmed (porcelain lines can start with a space). */
export async function gitRaw(cwd: string, ...args: string[]): Promise<string> {
  const { stdout } = await exec('git', args, { cwd, maxBuffer: 32 * 1024 * 1024, env: { ...process.env, ...GIT_ENV } });
  return stdout;
}

/** Like gitRaw(), but stdout is returned as bytes (for binary blobs). */
export async function gitBuffer(cwd: string, ...args: string[]): Promise<Buffer> {
  const { stdout } = await exec('git', args, {
    cwd, encoding: 'buffer', maxBuffer: 64 * 1024 * 1024, env: { ...process.env, ...GIT_ENV },
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
  /** The `upstream` remote of a fork, when it has one. */
  upstream: string | null;
  branch: string | null;
  lastTouchedAt: number;
}

/** A repo's `origin` and `upstream` URLs, read with one git call; null for one it doesn't have. */
export async function readRemotes(repo: string): Promise<{ origin: string | null; upstream: string | null }> {
  const out = (await tryGit(repo, 'config', '--get-regexp', '^remote\\.(origin|upstream)\\.url$')) ?? '';
  const urls: Record<string, string> = {};
  for (const line of out.split('\n')) {
    const m = /^remote\.(origin|upstream)\.url\s+(\S.*)$/.exec(line.trim());
    if (m) urls[m[1]] ??= m[2].trim();
  }
  return { origin: urls.origin || null, upstream: urls.upstream || null };
}

/**
 * The repo's canonical remote, the one to tell other people: `upstream` when it's a fork with a
 * network upstream (the repo everyone works on), else a network `origin`. Null when neither is one
 * (no remotes, or only local paths).
 */
export function canonicalRemote(remotes: { origin: string | null; upstream: string | null }): string | null {
  return [remotes.upstream, remotes.origin].find((u) => u && normaliseRemote(u)) ?? null;
}

/**
 * Lists git repos under the given roots. depth 1 = direct children only.
 * Worktrees (.git is a file) and non-repos are skipped.
 */
export async function scanRepos(roots: string[], depth: number): Promise<ScannedRepo[]> {
  return Promise.all(
    findRepos(roots, depth).map(async (p) => {
      const [{ origin, upstream }, branch] = await Promise.all([readRemotes(p), tryGit(p, 'symbolic-ref', '--quiet', '--short', 'HEAD')]);
      return {
        path: p,
        name: basename(p),
        remote: origin ? prettyRemote(origin) : null,
        upstream: upstream ? prettyRemote(upstream) : null,
        branch,
        lastTouchedAt: lastTouched(p),
      };
    }),
  );
}

/** The paths of the main repos under <roots>, <depth> folders deep at most. */
function findRepos(roots: string[], depth: number): string[] {
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
  return found;
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
  return shortRemote(url);
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
 * Throws unless <name> is a plain branch name git accepts. Names reach git as
 * arguments, so one starting with "-" would be read as an option. Only git's
 * refusal (exit 1) means a bad name: git that couldn't run at all (<repo> gone,
 * say) throws what went wrong instead, so a good name isn't blamed.
 */
export async function checkBranchName(repo: string, name: string): Promise<void> {
  const invalid = new Error(`"${name}" isn't a valid branch name.`);
  if (!name || name.startsWith('-')) throw invalid;
  try {
    await git(repo, 'check-ref-format', `refs/heads/${name}`);
  } catch (e) {
    if ((e as { code?: unknown }).code === 1) throw invalid;
    if (!existsSync(repo)) throw new Error(`${repo} isn't there any more, so git can't run in it. Put it back there and try again.`, { cause: e });
    const err = e as { stderr?: string; message?: string };
    throw new Error(`Couldn't run git in ${repo} to check the branch name "${name}": ${(err.stderr || err.message || '').trim().split('\n')[0] || 'git failed'}`, { cause: e });
  }
}

/**
 * Where a session's existing branch comes from on a remote: <branch> of <remote>, kept at
 * refs/remotes/<remote>/<branch>. Usually origin's branch of the same name; a PR from a fork has
 * its head on the fork's remote, maybe under another name than the local branch.
 */
export interface BranchSource {
  remote: string;
  branch: string;
}

/** The remote-tracking ref of a BranchSource. */
export function sourceRef(from: BranchSource): string {
  return `refs/remotes/${from.remote}/${from.branch}`;
}

/**
 * Where an existing branch is, after fetching it from <from> (origin's branch of the same name by
 * default): `local`, only on the `remote` (fetched into refs/remotes/<remote>), or null when it's in
 * neither. A branch the remote no longer has (merged and deleted on GitHub) loses its stale
 * remote-tracking copy, so it never counts as on the remote.
 */
export async function existingBranch(repo: string, branch: string, from: BranchSource = { remote: 'origin', branch }): Promise<'local' | 'remote' | null> {
  await checkBranchName(repo, branch);
  await checkBranchName(repo, from.branch);
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(from.remote)) throw new Error(`"${from.remote}" isn't a remote name.`);
  const err = await gitError(repo, 'fetch', '--quiet', from.remote, `+refs/heads/${from.branch}:${sourceRef(from)}`);
  if (err && /couldn't find remote ref/i.test(err)) await tryGit(repo, 'update-ref', '-d', sourceRef(from));
  if (await revParse(repo, `refs/heads/${branch}`)) return 'local';
  return (await revParse(repo, sourceRef(from))) ? 'remote' : null;
}

/**
 * The local branch a session on a PR works on, whose head is <from>: the head branch's name, unless
 * <from> isn't origin and a local branch of that name tracks something else (a fork's `main` must
 * never land on the local main); then `<remote>-<head branch>`.
 */
export async function prLocalBranch(repo: string, from: BranchSource): Promise<string> {
  if (from.remote === 'origin') return from.branch;
  if (!(await revParse(repo, `refs/heads/${from.branch}`))) return from.branch;
  const [r, m] = await Promise.all([
    tryGit(repo, 'config', '--get', `branch.${from.branch}.remote`), tryGit(repo, 'config', '--get', `branch.${from.branch}.merge`),
  ]);
  return r === from.remote && m === `refs/heads/${from.branch}` ? from.branch : `${from.remote}-${from.branch}`;
}

/**
 * Makes <branch> track <from> (`branch.<name>.remote` and `.merge`), so a plain `git pull` and,
 * where the names match, `git push` go to the PR's head rather than origin.
 */
export async function trackSource(repo: string, branch: string, from: BranchSource): Promise<void> {
  await git(repo, 'config', `branch.${branch}.remote`, from.remote);
  await git(repo, 'config', `branch.${branch}.merge`, `refs/heads/${from.branch}`);
}

/**
 * True when <base> is a branch of <repo>, locally or on origin. Only a name found in neither is
 * fetched from origin (a branch pushed since the last fetch), so the usual case runs no fetch.
 */
export async function baseExists(repo: string, base: string): Promise<boolean> {
  await checkBranchName(repo, base);
  const here = async () => !!(await revParse(repo, `refs/heads/${base}`)) || !!(await revParse(repo, `refs/remotes/origin/${base}`));
  if (await here()) return true;
  await tryGit(repo, 'fetch', '--quiet', 'origin', `+refs/heads/${base}:refs/remotes/origin/${base}`);
  return here();
}

/**
 * Creates a worktree. A new branch starts from the local <base>, fast-forwarded
 * to origin/<base> first (after a fetch) so new work starts from the latest
 * upstream. If <base> can't be fast-forwarded, the branch starts from
 * origin/<base> instead. Returns null when the branch already existed. With
 * <from>, a branch that is only on that remote (fetched by existingBranch) gets
 * a local branch at its remote-tracking ref.
 */
export async function addWorktree(repo: string, path: string, branch: string, base: string, from: BranchSource | null = null): Promise<SyncResult | null> {
  await checkBranchName(repo, base);
  await checkBranchName(repo, branch);
  await tryGit(repo, 'fetch', '--quiet', 'origin', base);
  if (await revParse(repo, `refs/heads/${branch}`)) {
    await git(repo, 'worktree', 'add', path, branch);
    return null;
  }
  if (from && (await revParse(repo, sourceRef(from)))) {
    await git(repo, 'worktree', 'add', '--no-track', '-b', branch, path, sourceRef(from));
    return null;
  }
  const { sync, start } = await startPoint(repo, base);
  await git(repo, 'worktree', 'add', '--no-track', '-b', branch, path, start);
  return sync;
}

/**
 * Where a new branch starts: the local <base> after fast-forwarding it, or origin/<base>
 * when that was skipped. <base> must already be fetched.
 */
async function startPoint(repo: string, base: string): Promise<{ sync: SyncResult; start: string }> {
  const sync = await fastForward(repo, base);
  const start = sync.kind === 'skipped' && (await revParse(repo, `refs/remotes/origin/${base}`)) ? `origin/${base}` : base;
  return { sync, start };
}

/**
 * Puts a new session's branch in an idle worktree of <repo>, keeping its ignored files (build
 * outputs). The branch starts where addWorktree would start it. Throws, changing nothing, unless
 * <path> is a clean worktree of <repo> on a detached HEAD. Returns null when the branch already existed.
 * <from> is as for addWorktree.
 */
export async function reuseWorktree(repo: string, path: string, branch: string, base: string, from: BranchSource | null = null): Promise<SyncResult | null> {
  await checkBranchName(repo, base);
  await checkBranchName(repo, branch);
  if (!(await isIdleWorktree(repo, path))) throw new Error(`${path} isn't a clean worktree of ${repo} with a detached HEAD`);
  await tryGit(repo, 'fetch', '--quiet', 'origin', base);
  if (await revParse(repo, `refs/heads/${branch}`)) {
    await git(path, 'checkout', '--quiet', branch);
    return null;
  }
  if (from && (await revParse(repo, sourceRef(from)))) {
    await git(path, 'checkout', '--quiet', '--no-track', '-b', branch, sourceRef(from));
    return null;
  }
  const { sync, start } = await startPoint(repo, base);
  await git(path, 'checkout', '--quiet', '--no-track', '-b', branch, start);
  return sync;
}

export interface WorktreeInfo {
  path: string;
  /** The branch checked out there (refs/heads/ stripped), or null. */
  branch: string | null;
  detached: boolean;
}

/** Every worktree of <repo>, its main checkout first, from `git worktree list --porcelain`. */
export async function listWorktrees(repo: string): Promise<WorktreeInfo[]> {
  const out = (await tryGit(repo, 'worktree', 'list', '--porcelain')) ?? '';
  return out.split('\n\n').flatMap((block) => {
    const lines = block.split('\n');
    const wt = lines.find((l) => l.startsWith('worktree '));
    if (!wt) return [];
    const br = lines.find((l) => l.startsWith('branch refs/heads/'));
    return [{ path: wt.slice('worktree '.length), branch: br ? br.slice('branch refs/heads/'.length) : null, detached: lines.includes('detached') }];
  });
}

/**
 * A worktree of <repo> (not its main checkout) on a detached HEAD, with nothing uncommitted or
 * untracked. Pass <list> (from listWorktrees) when checking several worktrees of one repo.
 */
export async function isIdleWorktree(repo: string, path: string, list?: WorktreeInfo[]): Promise<boolean> {
  if (!existsSync(path) || samePath(path, repo)) return false;
  const wt = (list ?? (await listWorktrees(repo))).find((w) => samePath(w.path, path));
  return !!wt?.detached && worktreeClean(path);
}

/** Detaches HEAD at its commit, so the branch is free and no file changes. */
export async function detachWorktree(path: string): Promise<boolean> {
  return (await tryGit(path, 'checkout', '--quiet', '--detach')) !== null;
}

/**
 * Throws away uncommitted changes and untracked files (for Discard). Ignored files, such as
 * build outputs, stay; so do nested repos, which leave the worktree unclean.
 */
export async function discardChanges(path: string): Promise<void> {
  await tryGit(path, 'reset', '--quiet', '--hard');
  await tryGit(path, 'clean', '-fdq');
}

/**
 * Fast-forwards the local <branch> to origin/<branch> (or <from>'s branch; already fetched); never
 * forces. Where the main checkout has the branch checked out this is
 * `merge --ff-only`, which git refuses when it would overwrite local edits
 * (autostash off, so edits are never stashed and re-applied). A branch checked
 * out in another worktree (another session's) is left alone. Otherwise the ref
 * moves by a non-forced local fetch: fast-forward only, a compare-and-swap, and
 * git refuses a branch that is checked out, rebased or bisected anywhere.
 */
export async function fastForward(repo: string, branch: string, from: BranchSource = { remote: 'origin', branch }): Promise<SyncResult> {
  const ref = sourceRef(from);
  const remote = await revParse(repo, ref);
  if (!remote) return { kind: 'no-upstream' };
  const local = await revParse(repo, `refs/heads/${branch}`);
  if (!local) return { kind: 'skipped', reason: `there's no local ${branch} branch` };
  if (local === remote) return { kind: 'current' };
  if ((await tryGit(repo, 'merge-base', '--is-ancestor', local, remote)) === null) {
    return { kind: 'skipped', reason: `local ${branch} has commits that aren't on ${from.remote}` };
  }
  const at = await checkedOutAt(repo, branch);
  if (at && !samePath(at, repo)) return { kind: 'skipped', reason: `${branch} is checked out in another worktree (${at})` };
  const err = at
    ? await gitError(at, '-c', 'merge.autoStash=false', 'merge', '--ff-only', '--quiet', remote)
    : await gitError(repo, 'fetch', '--quiet', '--no-tags', '--no-recurse-submodules', '.', `${ref}:refs/heads/${branch}`);
  if (!err) return { kind: 'updated' };
  return { kind: 'skipped', reason: at ? `git couldn't fast-forward it in ${at}: ${err}` : `git couldn't fast-forward it: ${err}` };
}

function samePath(a: string, b: string): boolean {
  try {
    return realpathSync(a) === realpathSync(b);
  } catch {
    return a === b;
  }
}

/** The worktree (main checkout included) that has <branch> checked out, if any. */
export async function checkedOutAt(repo: string, branch: string): Promise<string | null> {
  return (await listWorktrees(repo)).find((w) => w.branch === branch)?.path ?? null;
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

/** The session's branch is gone locally and on origin, so its worktree can't be restored. */
export class BranchGoneError extends Error {}

/**
 * Recreates a missing worktree from its branch, or from origin/<branch> when only
 * that is left. Never starts a new branch and never touches the main checkout.
 * (The base is no longer needed; the parameter stays for existing callers.)
 */
export async function restoreWorktree(repo: string, path: string, branch: string, _base?: string): Promise<void> {
  if (existsSync(path)) return;
  await tryGit(repo, 'worktree', 'prune');
  await checkBranchName(repo, branch);
  if (await revParse(repo, `refs/heads/${branch}`)) {
    await git(repo, 'worktree', 'add', path, branch);
  } else if (await revParse(repo, `refs/remotes/origin/${branch}`)) {
    await git(repo, 'worktree', 'add', '--no-track', '-b', branch, path, `origin/${branch}`);
  } else {
    throw new BranchGoneError(`Branch ${branch} is gone (locally and on origin), so the session's worktree can't be restored.`);
  }
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
  await exec('git', ['clone', '--quiet', '--', url, dest], { env: { ...process.env, ...GIT_ENV } });
}

export function repoNameFromUrl(url: string): string {
  return remoteRepoName(url) || 'repo';
}

/** git couldn't make a commit because it doesn't know the user's name or email (its C-locale message, which includes stderr). */
function noIdentity(e: unknown): boolean {
  return /tell me who you are|identity unknown|auto-detection is disabled|unable to auto-detect email/i.test(e instanceof Error ? e.message : String(e));
}

/** How long the first commit of a new repo may take before it's given up. */
const FIRST_COMMIT_TIMEOUT_MS = 30_000;

/**
 * Makes a new repo at <dest>, a folder that must not exist yet (its EEXIST is passed on as it is):
 * `git init`, HEAD on the user's `init.defaultBranch` (or main), and an empty first commit, so
 * sessions have a base to branch from. On a failure the folder is removed again.
 */
export async function initRepo(dest: string): Promise<void> {
  const parent = dirname(dest);
  try {
    mkdirSync(parent, { recursive: true });
  } catch (e) {
    // Not the new folder's own EEXIST: the repos folder itself is the problem.
    throw new Error(`Couldn't make the repos folder ${parent} (the clone folder in Settings): ${(e as Error).message}`, { cause: e });
  }
  mkdirSync(dest);
  try {
    await initHere(dest);
    await emptyFirstCommit(dest);
  } catch (e) {
    try {
      rmSync(dest, { recursive: true, force: true });
    } catch {
      // Left behind: the error that matters is why the repo couldn't be made.
    }
    throw e;
  }
}

/**
 * Makes <dir>, an empty folder that isn't a repo, one as initRepo would. One that is a repo by now
 * (another open made it meanwhile) is left as it is. On a failure the `.git` this call made is
 * removed again, so the folder is left as it was.
 */
export async function initFolder(dir: string): Promise<void> {
  await oneAtATime(dir, async () => {
    if (isMainRepo(dir)) return;
    try {
      await initHere(dir);
      await emptyFirstCommit(dir);
    } catch (e) {
      try {
        rmSync(join(dir, '.git'), { recursive: true, force: true });
      } catch {
        // Left behind: the error that matters is why the repo couldn't be made.
      }
      throw e;
    }
  });
}

/** `git init` in <dir>, with HEAD on the user's `init.defaultBranch` (or main). */
async function initHere(dir: string): Promise<void> {
  await git(dir, 'init', '--quiet');
  // Set by hand rather than with `git init -b`, which needs git 2.28.
  const wanted = (await tryGit(dir, 'config', '--get', 'init.defaultBranch')) || 'main';
  const branch = await checkBranchName(dir, wanted).then(() => wanted, () => 'main');
  await git(dir, 'symbolic-ref', 'HEAD', `refs/heads/${branch}`);
}

/** An empty commit on HEAD's branch of <dir>, as the user, saying how to fix a missing name and email. */
async function emptyFirstCommit(dir: string): Promise<void> {
  try {
    // The server has no terminal: no signing (it may ask for a passphrase) and no hooks, which
    // have nothing to check in an empty commit. The C locale keeps git's messages readable by noIdentity.
    await exec('git', ['-c', 'commit.gpgsign=false', '-c', 'core.hooksPath=/dev/null', 'commit', '--quiet', '--allow-empty', '-m', 'Initial commit'], {
      cwd: dir, timeout: FIRST_COMMIT_TIMEOUT_MS, env: { ...process.env, ...GIT_ENV, LC_ALL: 'C' },
    });
  } catch (e) {
    if (!noIdentity(e)) throw e;
    throw new Error('git needs your name and email to make the first commit. Run git config --global user.name "Your Name" and git config --global user.email "you@example.com", then try again.', { cause: e });
  }
}

/** The last first-commit step queued on each repo (by its real path), so they run one at a time. */
const repoLocks = new Map<string, Promise<unknown>>();

/** Runs <fn> once every initFolder and ensureFirstCommit queued on <repo> before it is done. */
async function oneAtATime<T>(repo: string, fn: () => Promise<T>): Promise<T> {
  const key = repoKey(repo);
  const run = (repoLocks.get(key) ?? Promise.resolve()).catch(() => undefined).then(fn);
  repoLocks.set(key, run);
  try {
    return await run;
  } finally {
    if (repoLocks.get(key) === run) repoLocks.delete(key);
  }
}

/**
 * Gives <repo>, a repo with no commits yet (`git init` and nothing more), an empty first commit on
 * HEAD's branch, as initRepo does, so a session's worktree has a base to branch from. Returns
 * whether it made one. A repo with files in it (untracked or staged) is refused, changing nothing:
 * an empty commit would leave them out of every worktree, and committing them is the user's call.
 * Two at once on one repo make one commit.
 */
export async function ensureFirstCommit(repo: string): Promise<boolean> {
  return oneAtATime(repo, async () => {
    if (await hasCommits(repo)) return false;
    if (await tryGit(repo, 'status', '--porcelain')) {
      throw new Error(`${repo} has no commits yet, so a session has nothing to branch from. Commit its files first (git add -A, then git commit -m "Initial commit"), then try again.`);
    }
    await emptyFirstCommit(repo);
    return true;
  });
}

/** <repo> has at least one commit on any ref. An error (not a repo) counts as having some: git reports that later. */
async function hasCommits(repo: string): Promise<boolean> {
  const out = await tryGit(repo, 'rev-list', '-n', '1', '--all');
  return out !== '';
}

/**
 * <dir> is a repo just as initRepo left it: no remotes, nothing uncommitted, untracked or ignored
 * (by the user's global excludes, say), and one commit with no files. A retry can use it again.
 */
export async function isFreshRepo(dir: string): Promise<boolean> {
  if (!isMainRepo(dir)) return false;
  const [remotes, count, files, status] = await Promise.all([
    tryGit(dir, 'remote'),
    tryGit(dir, 'rev-list', '--count', '--all'),
    tryGit(dir, 'ls-tree', '-r', '--name-only', 'HEAD'),
    tryGit(dir, 'status', '--porcelain', '--ignored'),
  ]);
  return remotes === '' && count === '1' && files === '' && status === '';
}

// ---- Review a PR ----

/**
 * The remote of <repo> that is GitHub's <ghRepo> (`owner/name`): `upstream` first (a fork whose
 * PRs are upstream), then `origin`. Null when neither is.
 */
export async function remoteFor(repo: string, ghRepo: string): Promise<'upstream' | 'origin' | null> {
  const remotes = await readRemotes(repo);
  const want = ghRepo.toLowerCase();
  if (githubRepo(remotes.upstream)?.toLowerCase() === want) return 'upstream';
  if (githubRepo(remotes.origin)?.toLowerCase() === want) return 'origin';
  return null;
}

/**
 * The remotes of <repo> on GitHub, by the `owner/name` (lowercased) each is: when two remotes are
 * the same repo, `origin` first, then `upstream`, then the others by name.
 */
export async function githubRemotes(repo: string): Promise<Map<string, string>> {
  const out = (await tryGit(repo, 'config', '--get-regexp', '^remote\\..*\\.url$')) ?? '';
  const found: { name: string; repo: string }[] = [];
  for (const line of out.split('\n')) {
    const m = /^remote\.(.+)\.url\s+(\S.*)$/.exec(line.trim());
    const gh = m ? githubRepo(m[2].trim()) : null;
    if (m && gh) found.push({ name: m[1], repo: gh.toLowerCase() });
  }
  const rank = (n: string) => (n === 'origin' ? 0 : n === 'upstream' ? 1 : 2);
  found.sort((a, b) => rank(a.name) - rank(b.name) || a.name.localeCompare(b.name));
  const map = new Map<string, string>();
  for (const f of found) if (!map.has(f.repo)) map.set(f.repo, f.name);
  return map;
}

/**
 * The remote of <repo> that is GitHub's <ghRepo> (`owner/name`), any remote name (see
 * githubRemotes), or null when none is. For a PR's head, which may be on a fork the user added as a remote.
 */
export async function remoteOf(repo: string, ghRepo: string): Promise<string | null> {
  return (await githubRemotes(repo)).get(ghRepo.toLowerCase()) ?? null;
}

/** Where a Review a PR session keeps the PR's head in its repo (outside refs/heads, so no branch list shows it). */
export function prRef(number: number): string {
  return `refs/agentk/pr/${number}`;
}

/**
 * Fetches PR <number>'s head from <remote> (a fork's PR too: GitHub keeps refs/pull/N/head on the
 * PR's repo) into prRef(), and the PR's <base> with it, so the diff has its merge base. Returns the head.
 */
export async function fetchPrHead(repo: string, remote: string, number: number, base: string): Promise<string> {
  await checkBranchName(repo, base);
  await git(repo, 'fetch', '--quiet', remote, `+refs/pull/${number}/head:${prRef(number)}`);
  await tryGit(repo, 'fetch', '--quiet', remote, base);
  const sha = await revParse(repo, prRef(number));
  if (!sha) throw new Error(`git fetched PR #${number} from ${remote} but left nothing at ${prRef(number)}`);
  return sha;
}

/** The push remote a review branch gets: no remote has this name, so a bare `git push` fails. */
export const NO_PUSH_REMOTE = 'agentk-review-no-push';

/**
 * Puts a new review branch at <sha>, in a new worktree at <path> or (<reuse>) in an idle worktree
 * of <repo>, which must be clean on a detached HEAD. The branch has no upstream, and its
 * `pushRemote` is a remote that doesn't exist, so a bare `git push` fails whatever the push config
 * (`push.default=current` and `push.autoSetupRemote` included); `git branch -D` drops the setting.
 */
export async function addReviewWorktree(repo: string, path: string, branch: string, sha: string, reuse: boolean): Promise<void> {
  await checkBranchName(repo, branch);
  if (!/^[0-9a-f]{7,64}$/.test(sha)) throw new Error(`"${sha}" isn't a commit id.`);
  if (reuse) {
    if (!(await isIdleWorktree(repo, path))) throw new Error(`${path} isn't a clean worktree of ${repo} with a detached HEAD`);
    await git(path, 'checkout', '--quiet', '--no-track', '-b', branch, sha);
  } else {
    await git(repo, 'worktree', 'add', '--no-track', '-b', branch, path, sha);
  }
  await git(repo, 'config', `branch.${branch}.pushRemote`, NO_PUSH_REMOTE);
}

/**
 * Commits on <branch> that <ref> lacks: the user's own on a review branch. Null when git can't tell
 * (either is missing), which callers take as "there may be some".
 */
export async function commitsBeyond(repo: string, branch: string, ref: string): Promise<number | null> {
  const n = await tryGit(repo, 'rev-list', '--count', `${ref}..refs/heads/${branch}`);
  return n && /^\d+$/.test(n) ? Number(n) : null;
}

/** Commits from <from> to <to> when <to> follows on from <from>, or null when it doesn't (a rewritten history). */
export async function commitsBetween(repo: string, from: string, to: string): Promise<number | null> {
  if ((await tryGit(repo, 'merge-base', '--is-ancestor', from, to)) === null) return null;
  const n = await tryGit(repo, 'rev-list', '--count', `${from}..${to}`);
  return n && /^\d+$/.test(n) ? Number(n) : null;
}

/**
 * Moves a clean review worktree's branch to <sha> (`reset --keep`, which refuses to lose local
 * changes). Callers check first that the branch has no commits of the user's own.
 */
export async function moveReviewBranch(worktree: string, sha: string): Promise<void> {
  if (!/^[0-9a-f]{7,64}$/.test(sha)) throw new Error(`"${sha}" isn't a commit id.`);
  await git(worktree, 'reset', '--quiet', '--keep', sha);
}
