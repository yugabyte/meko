import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, symlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { gitRepo, makeEnv, tempDir, waitFor, type TestEnv } from '../helpers.js';
import { all, get, run } from '../../src/server/db.js';
import type { HookPayload } from '../../src/server/hooks.js';
import { repoFolder } from '../../src/server/worktrees.js';

const git = (cwd: string, ...a: string[]) => execFileSync('git', a, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();

describe('sessions in another repo and on a branch you name (real tmux + git, fake claude)', () => {
  let env: TestEnv;
  /** A second scanned repo, cloned from origin.git, whose default branch is trunk. */
  let other: string;
  /** Someone else's clone of the second repo, for pushing to its origin. */
  let pusher: string;

  beforeEach(async () => {
    env = await makeEnv();
    const seed = gitRepo(join(env.home, 'seed'));
    git(seed, 'branch', '-m', 'main', 'trunk');
    git(env.home, 'clone', '-q', '--bare', seed, 'origin.git');
    other = join(env.home, 'code', 'other-repo');
    git(env.home, 'clone', '-q', join(env.home, 'origin.git'), other);
    pusher = join(env.home, 'pusher');
    git(env.home, 'clone', '-q', join(env.home, 'origin.git'), pusher);
    for (const d of [other, pusher]) {
      git(d, 'config', 'user.email', 'test@example.com');
      git(d, 'config', 'user.name', 'Test');
    }
  });
  afterEach(async () => {
    await env.cleanup();
  });

  const project = (name = 'Demo') =>
    env.core.projects.create({ repoPath: env.repo, name, datapack: { mode: 'existing', id: `dp-${name}`, name: `team/${name}` } });

  async function started(s: { id: string }) {
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, `${s.id} to start`);
    return env.core.sessions.get(s.id);
  }

  const sessionCount = () => get<{ n: number }>(env.core.db, 'SELECT COUNT(*) AS n FROM sessions')!.n;
  const event = (id: string, kind: string) =>
    get<{ text: string }>(env.core.db, 'SELECT text FROM session_events WHERE session_id = ? AND kind = ? ORDER BY id DESC LIMIT 1', id, kind)?.text;
  const reused = (id: string) => get<{ branch_reused: number }>(env.core.db, 'SELECT branch_reused FROM sessions WHERE id = ?', id)!.branch_reused;
  const branches = (repo: string) => git(repo, 'branch', '--list', '--format=%(refname:short)').split('\n').filter(Boolean);

  /** A commit on a new local branch of <repo>, which stays on its current branch. */
  function localBranch(repo: string, branch: string): string {
    const current = git(repo, 'symbolic-ref', '--short', 'HEAD');
    git(repo, 'checkout', '-q', '-b', branch);
    writeFileSync(join(repo, `${branch.replace(/\//g, '-')}.txt`), 'work\n');
    git(repo, 'add', '.');
    git(repo, 'commit', '-q', '-m', `work on ${branch}`);
    const sha = git(repo, 'rev-parse', 'HEAD');
    git(repo, 'checkout', '-q', current);
    return sha;
  }

  it('starts a session in another scanned repo, from that repo\'s default branch, and keeps its worktrees there', async () => {
    const p = await project();
    expect(await env.core.projects.repoBranches(other)).toEqual({ defaultBranch: 'trunk', branches: ['trunk'] });
    const s = await started(await env.core.sessions.create(p.id, { name: 'Other work', prompt: 'hi', kind: 'quick', repoPath: other }));
    expect(s).toMatchObject({ repoPath: other, branch: 'ak/other-work', baseBranch: 'trunk' });
    expect(s.worktreePath).toBe(join(repoFolder(env.core.paths.worktrees, other), 'wt-1'));
    expect(branches(other)).toContain('ak/other-work');
    expect(branches(env.repo)).not.toContain('ak/other-work');
    expect(git(s.worktreePath, 'rev-parse', '--show-toplevel')).toBe(s.worktreePath);

    // The Design-stage guard covers the session's repo, not only the project's.
    const d = await started(await env.core.sessions.create(p.id, { name: 'Design there', prompt: 'hi', repoPath: other }));
    const row = env.core.sessions.row(d.id);
    const edit = (file: string) =>
      env.core.sessions.guard(row.hook_token, { tool_name: 'Edit', tool_input: { file_path: file }, cwd: d.worktreePath } as HookPayload);
    const denied = { hookSpecificOutput: expect.objectContaining({ permissionDecision: 'deny' }) };
    expect(edit(join(other, 'README.md'))).toMatchObject(denied);
    expect(edit(join(d.worktreePath, 'README.md'))).toMatchObject(denied);
    expect(edit(join(env.home, 'notes.md'))).toBeNull();

    // Archive puts the worktree in the other repo's pool, and the next session there reuses it.
    await env.core.sessions.action(s.id, 'archive');
    expect(all<{ repo: string }>(env.core.db, 'SELECT repo FROM worktrees WHERE path = ? AND session_id IS NULL', s.worktreePath)).toEqual([{ repo: other }]);
    await env.core.worktrees.sweep(); // the other repo counts as used by the project
    const next = await started(await env.core.sessions.create(p.id, { name: 'Next there', prompt: 'hi', kind: 'quick', repoPath: other }));
    expect(next.worktreePath).toBe(s.worktreePath);

    // Discard deletes the branch in the session's repo.
    await env.core.sessions.action(next.id, 'discard');
    expect(branches(other)).not.toContain('ak/next-there');

    // The last project using the repo takes its idle worktrees along.
    await env.core.projects.delete(p.id);
    expect(existsSync(s.worktreePath)).toBe(false);
  });

  it('refuses a repo that isn\'t one of the scanned ones', async () => {
    const p = await project();
    const outside = gitRepo(join(tempDir('akl-outside-'), 'elsewhere'));
    const plain = join(env.home, 'code', 'not-a-repo');
    mkdirSync(plain);
    const wt = join(env.home, 'code', 'a-worktree');
    git(other, 'worktree', 'add', '-q', wt);
    const deep = gitRepo(join(env.home, 'code', 'group', 'deep')); // deeper than [repos] depth
    const hidden = gitRepo(join(env.home, 'code', '.hidden'));
    for (const repoPath of [outside, plain, wt, deep, hidden, join(env.home, 'code', 'missing')]) {
      await expect(env.core.sessions.create(p.id, { name: 'Nope', prompt: 'hi', kind: 'quick', repoPath }))
        .rejects.toMatchObject({ status: 400, message: expect.stringMatching(/isn't one of the repos under/) });
      await expect(env.core.projects.repoBranches(repoPath)).rejects.toMatchObject({ status: 400 });
    }
    await expect(env.core.sessions.create(p.id, { name: 'Nope', kind: 'quick', repoPath: 5 as unknown as string }))
      .rejects.toMatchObject({ status: 400 });
    expect(sessionCount()).toBe(0);
    // The project's own repo is always allowed, however it's written.
    const s = await env.core.sessions.create(p.id, { name: 'Here', prompt: 'hi', kind: 'quick', repoPath: `${env.repo}/` });
    expect(s.repoPath).toBe(env.repo);
  });

  it('uses a branch name as typed, and refuses one git doesn\'t accept', async () => {
    const p = await project();
    const s = await started(await env.core.sessions.create(p.id, { name: 'My task', prompt: 'hi', kind: 'quick', branch: ' feature/typed ' }));
    expect(s).toMatchObject({ id: 'demo-repo--my-task', branch: 'feature/typed' });
    expect(reused(s.id)).toBe(0);
    expect(event(s.id, 'created')).toMatch(/^Started on feature\/typed from main/);
    for (const branch of ['-x', 'a..b', 'with space']) {
      await expect(env.core.sessions.create(p.id, { name: 'Bad', prompt: 'hi', kind: 'quick', branch }))
        .rejects.toMatchObject({ status: 400, message: expect.stringMatching(/isn't a valid branch name/) });
    }
    expect(sessionCount()).toBe(1);
  });

  it('picks up an existing local branch as it is, or one only on origin', async () => {
    const p = await project();
    const sha = localBranch(env.repo, 'feature/local');
    const s = await started(await env.core.sessions.create(p.id, { name: 'Carry on', prompt: 'hi', kind: 'quick', branch: 'feature/local' }));
    expect(git(s.worktreePath, 'rev-parse', 'HEAD')).toBe(sha);
    expect(reused(s.id)).toBe(1);
    expect(event(s.id, 'created')).toBe('Started on the existing branch feature/local (base main)');

    git(pusher, 'checkout', '-q', '-b', 'feature/remote');
    writeFileSync(join(pusher, 'remote.txt'), 'pushed\n');
    git(pusher, 'add', '.');
    git(pusher, 'commit', '-q', '-m', 'remote work');
    git(pusher, 'push', '-q', 'origin', 'feature/remote');
    const remote = git(pusher, 'rev-parse', 'HEAD');
    const r = await started(await env.core.sessions.create(p.id, { name: 'Remote', prompt: 'hi', kind: 'quick', repoPath: other, branch: 'feature/remote' }));
    expect(git(r.worktreePath, 'rev-parse', 'HEAD')).toBe(remote);
    expect(git(other, 'rev-parse', 'feature/remote')).toBe(remote);
    expect(reused(r.id)).toBe(1);

    // A branch deleted on origin since the last fetch starts fresh from the base, not from its old tip.
    git(pusher, 'checkout', '-q', '-b', 'feature/merged');
    git(pusher, 'commit', '-q', '--allow-empty', '-m', 'merged work');
    git(pusher, 'push', '-q', 'origin', 'feature/merged');
    git(other, 'fetch', '-q', 'origin');
    git(pusher, 'push', '-q', 'origin', '--delete', 'feature/merged');
    const m = await started(await env.core.sessions.create(p.id, { name: 'Fresh', prompt: 'hi', kind: 'quick', repoPath: other, branch: 'feature/merged' }));
    expect(git(m.worktreePath, 'rev-parse', 'HEAD')).toBe(git(other, 'rev-parse', 'trunk'));
    expect(reused(m.id)).toBe(0);
    expect(() => git(other, 'rev-parse', '--verify', '--quiet', 'refs/remotes/origin/feature/merged')).toThrow();
  });

  it('refuses a branch a running session uses, in any project, and lets one start on it once that session is archived', async () => {
    const p = await project();
    const second = await project('Second');
    const a = await started(await env.core.sessions.create(p.id, { name: 'First', prompt: 'hi', kind: 'quick' }));
    for (const pid of [p.id, second.id]) {
      await expect(env.core.sessions.create(pid, { name: 'Again', prompt: 'hi', kind: 'quick', branch: a.branch }))
        .rejects.toMatchObject({ status: 409, message: 'Branch ak/first is in use by the session "First" in Demo. Archive that session first, or pick another branch.' });
    }
    expect(sessionCount()).toBe(1);

    writeFileSync(join(a.worktreePath, 'done.txt'), 'done\n');
    git(a.worktreePath, 'add', '.');
    git(a.worktreePath, 'commit', '-q', '-m', 'done');
    const sha = git(a.worktreePath, 'rev-parse', 'HEAD');
    await env.core.sessions.action(a.id, 'archive');
    const b = await started(await env.core.sessions.create(second.id, { name: 'Takes over', prompt: 'hi', kind: 'quick', branch: a.branch }));
    expect(git(b.worktreePath, 'rev-parse', 'HEAD')).toBe(sha);
  });

  it('lets only one of two sessions starting at once have the same branch', async () => {
    const p = await project();
    localBranch(env.repo, 'feature/shared');
    const results = await Promise.allSettled(['One', 'Two'].map((name) =>
      env.core.sessions.create(p.id, { name, prompt: 'hi', kind: 'quick', branch: 'feature/shared' })));
    expect(results.map((r) => r.status).sort()).toEqual(['fulfilled', 'rejected']);
    const failed = results.find((r) => r.status === 'rejected') as PromiseRejectedResult;
    expect(failed.reason).toMatchObject({ status: 409 });
    expect(sessionCount()).toBe(1);
  });

  it('refuses a branch another checkout has, naming an archived session that kept its worktree', async () => {
    const p = await project();
    await expect(env.core.sessions.create(p.id, { name: 'Main', prompt: 'hi', kind: 'quick', branch: 'main' }))
      .rejects.toMatchObject({ status: 409, message: `Branch main is checked out at ${env.repo}. Switch that checkout to another branch, or pick another branch.` });

    const a = await started(await env.core.sessions.create(p.id, { name: 'Dirty', prompt: 'hi', kind: 'quick' }));
    writeFileSync(join(a.worktreePath, 'draft.txt'), 'wip\n');
    // Archive refuses this; a PR merged on GitHub still archives with the changes kept.
    await env.core.sessions.archive(env.core.sessions.row(a.id), { keepBranch: true });
    await expect(env.core.sessions.create(p.id, { name: 'Wants it', prompt: 'hi', kind: 'quick', branch: a.branch }))
      .rejects.toMatchObject({ status: 409, message: expect.stringMatching(/still checked out at .*the archived session "Dirty" because it has uncommitted changes/) });
    expect(sessionCount()).toBe(1);

    // Found by its real path when the stored one goes through a symlink.
    const link = join(env.home, 'wt-link');
    symlinkSync(a.worktreePath, link);
    run(env.core.db, 'UPDATE sessions SET worktree_path = ? WHERE id = ?', link, a.id);
    await expect(env.core.sessions.create(p.id, { name: 'Wants it', prompt: 'hi', kind: 'quick', branch: a.branch }))
      .rejects.toMatchObject({ status: 409, message: expect.stringMatching(/the archived session "Dirty"/) });
  });
});
