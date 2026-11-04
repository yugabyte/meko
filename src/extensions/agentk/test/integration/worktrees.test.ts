import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';
import { buildServer } from '../../src/server/server.js';
import { all, get, run } from '../../src/server/db.js';
import { repoFolder } from '../../src/server/worktrees.js';

const out = (cwd: string, ...a: string[]) => execFileSync('git', a, { cwd, encoding: 'utf8' }).trim();

describe('worktree reuse (real session host + git, fake claude)', () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv();
    // Build outputs are ignored, as in a real repo.
    writeFileSync(join(env.repo, '.gitignore'), 'build/\n');
    execFileSync('git', ['add', '.gitignore'], { cwd: env.repo });
    execFileSync('git', ['commit', '-q', '-m', 'ignore build'], { cwd: env.repo });
  });
  afterEach(async () => {
    await env.cleanup();
  });

  const project = (name = 'Demo', repoPath = env.repo) =>
    env.core.projects.create({ repoPath, name, datapack: { mode: 'existing', id: `dp-${name}`, name: `team/${name}` } });

  async function session(projectId: string, name: string) {
    const s = await env.core.sessions.create(projectId, { name, prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, `${name} to start`);
    return s;
  }

  /** Builds something (ignored) and commits something on the session's branch, then archives it. */
  async function buildAndArchive(s: { id: string; worktreePath: string }) {
    mkdirSync(join(s.worktreePath, 'build'), { recursive: true });
    writeFileSync(join(s.worktreePath, 'build', 'out.o'), 'object code');
    writeFileSync(join(s.worktreePath, 'feature.txt'), 'work\n');
    execFileSync('git', ['add', 'feature.txt'], { cwd: s.worktreePath });
    execFileSync('git', ['commit', '-q', '-m', 'feature'], { cwd: s.worktreePath });
    await env.core.sessions.action(s.id, 'archive');
  }

  const idle = () => all<{ path: string }>(env.core.db, 'SELECT path FROM worktrees WHERE session_id IS NULL AND stale = 0 ORDER BY released_at').map((r) => r.path);
  const stale = () => all<{ path: string }>(env.core.db, 'SELECT path FROM worktrees WHERE stale = 1').map((r) => r.path);
  const event = (id: string, kind: string) =>
    get<{ text: string }>(env.core.db, 'SELECT text FROM session_events WHERE session_id = ? AND kind = ? ORDER BY id DESC LIMIT 1', id, kind)?.text;

  it('hands an ended session\'s worktree, build outputs and all, to the next session', async () => {
    const p = await project();
    const a = await session(p.id, 'First');
    expect(a.worktreePath).toBe(join(repoFolder(env.core.paths.worktrees, env.repo), 'wt-1'));
    expect(basename(dirname(a.worktreePath))).toMatch(/^demo-repo\.[0-9a-f]{8}$/);
    await buildAndArchive(a);
    expect(event(a.id, 'archived')).toBe('Archived; worktree kept for the next session');
    expect(idle()).toEqual([a.worktreePath]);
    const built = statSync(join(a.worktreePath, 'build', 'out.o')).mtimeMs;

    const b = await session(p.id, 'Second');
    expect(b.worktreePath).toBe(a.worktreePath);
    expect(out(b.worktreePath, 'symbolic-ref', '--short', 'HEAD')).toBe('ak/second');
    expect(out(b.worktreePath, 'rev-parse', 'HEAD')).toBe(out(env.repo, 'rev-parse', 'main'));
    expect(existsSync(join(b.worktreePath, 'feature.txt'))).toBe(false); // the first branch's commit isn't there
    expect(readFileSync(join(b.worktreePath, 'build', 'out.o'), 'utf8')).toBe('object code');
    expect(statSync(join(b.worktreePath, 'build', 'out.o')).mtimeMs).toBe(built);
    expect(event(b.id, 'created')).toBe('Started on ak/second from main in a reused worktree (wt-1)');
    expect(idle()).toEqual([]);

    // The first session's branch is free, and it no longer shows the worktree's changes.
    execFileSync('git', ['branch', '-D', 'ak/first'], { cwd: env.repo, stdio: 'ignore' });
    await expect(env.core.sessions.changes(a.id)).rejects.toThrow(/worktree is gone/);
    expect((await env.core.sessions.discardCheck(a.id)).worktreeExists).toBe(false);
    expect((await env.core.sessions.changes(b.id)).files).toEqual([]);
  });

  it('shares idle worktrees between the projects on a repo, including one reached by a symlink', async () => {
    const link = join(env.home, 'code', 'demo-link');
    symlinkSync(env.repo, link);
    const p1 = await project('Demo');
    const p2 = await project('Other', link);
    const a = await session(p1.id, 'In one');
    await buildAndArchive(a);
    const b = await session(p2.id, 'In two');
    expect(b.worktreePath).toBe(a.worktreePath);
    expect(existsSync(join(b.worktreePath, 'build', 'out.o'))).toBe(true);
  });

  it('gives one idle worktree to only one of two sessions starting at once in different projects', async () => {
    const p1 = await project('Demo');
    const p2 = await project('Other');
    const a = await session(p1.id, 'Warm');
    await buildAndArchive(a);
    const [x, y] = await Promise.all([
      env.core.sessions.create(p1.id, { name: 'X', prompt: 'hi', kind: 'quick' }),
      env.core.sessions.create(p2.id, { name: 'Y', prompt: 'hi', kind: 'quick' }),
    ]);
    expect([x.worktreePath, y.worktreePath].sort()).toEqual([a.worktreePath, join(dirname(a.worktreePath), 'wt-2')]);
  });

  it('never reuses a worktree with uncommitted changes or untracked files', async () => {
    const p = await project();
    const dirty = await session(p.id, 'Dirty');
    writeFileSync(join(dirty.worktreePath, 'draft.txt'), 'wip\n');
    // Archive refuses this; a PR merged on GitHub still archives with the changes kept.
    await env.core.sessions.archive(env.core.sessions.row(dirty.id), { keepBranch: true });
    expect(event(dirty.id, 'archived')).toBe('Archived; worktree kept because it has uncommitted changes');
    const next = await session(p.id, 'Next');
    expect(next.worktreePath).not.toBe(dirty.worktreePath);
    expect(readFileSync(join(dirty.worktreePath, 'draft.txt'), 'utf8')).toBe('wip\n');

    // An idle worktree someone wrote into by hand is left alone, recorded as stale so it's
    // neither reused nor forgotten, and becomes reusable again once it's clean.
    await env.core.sessions.action(next.id, 'archive');
    expect(idle()).toEqual([next.worktreePath]);
    writeFileSync(join(next.worktreePath, 'notes.txt'), 'mine\n');
    const third = await session(p.id, 'Third');
    expect([dirty.worktreePath, next.worktreePath]).not.toContain(third.worktreePath);
    expect(readFileSync(join(next.worktreePath, 'notes.txt'), 'utf8')).toBe('mine\n');
    expect(idle()).toEqual([]);
    expect(stale()).toEqual([next.worktreePath]);
    expect(env.core.sessions.ownsWorktree(env.core.sessions.row(next.id))).toBe(false);
    execFileSync('rm', [join(next.worktreePath, 'notes.txt')]);
    await env.core.worktrees.sweep();
    expect(idle()).toEqual([next.worktreePath]);
  });

  it('keeps ignored files through Discard, and removes worktrees when the setting is 0', async () => {
    const p = await project();
    const s = await session(p.id, 'Throw away');
    mkdirSync(join(s.worktreePath, 'build'));
    writeFileSync(join(s.worktreePath, 'build', 'out.o'), 'object code');
    writeFileSync(join(s.worktreePath, 'README.md'), 'edited\n');
    writeFileSync(join(s.worktreePath, 'draft.txt'), 'wip\n');
    await env.core.sessions.action(s.id, 'discard', { skipLearn: true });
    expect(event(s.id, 'discarded')).toBe('Discarded; changes and branch ak/throw-away deleted, worktree kept for the next session');
    expect(readFileSync(join(s.worktreePath, 'README.md'), 'utf8')).toBe('# test\n');
    expect(existsSync(join(s.worktreePath, 'draft.txt'))).toBe(false);
    expect(existsSync(join(s.worktreePath, 'build', 'out.o'))).toBe(true);
    expect(idle()).toEqual([s.worktreePath]);

    env.core.updateConfig((c) => (c.worktrees.idlePerRepo = 0));
    await env.core.worktrees.trimAll();
    expect(existsSync(s.worktreePath)).toBe(false);
    const t = await session(p.id, 'Cold');
    await env.core.sessions.action(t.id, 'archive');
    expect(event(t.id, 'archived')).toBe('Archived; worktree removed');
    expect(existsSync(t.worktreePath)).toBe(false);
  });

  it('keeps at most the setting\'s number of idle worktrees per repo, the most recent ones', async () => {
    env.core.updateConfig((c) => (c.worktrees.idlePerRepo = 1));
    const p1 = await project('Demo');
    const p2 = await project('Other');
    const a = await session(p1.id, 'A');
    const b = await session(p2.id, 'B');
    await env.core.sessions.action(a.id, 'archive');
    await env.core.sessions.action(b.id, 'archive');
    expect(idle()).toEqual([b.worktreePath]);
    expect(existsSync(a.worktreePath)).toBe(false);
  });

  it('removes idle worktrees with the last project on their repo, and forgets ones deleted by hand', async () => {
    const p1 = await project('Demo');
    const p2 = await project('Other');
    const a = await session(p1.id, 'A');
    const b = await session(p2.id, 'B');
    await env.core.sessions.action(a.id, 'archive');
    await env.core.projects.delete(p1.id);
    expect(existsSync(a.worktreePath)).toBe(true); // Other can still use it
    await env.core.sessions.action(b.id, 'archive');
    expect(idle()).toHaveLength(2);

    execFileSync('rm', ['-rf', b.worktreePath]);
    await env.core.worktrees.sweep();
    expect(idle()).toEqual([a.worktreePath]);

    await env.core.projects.delete(p2.id);
    expect(existsSync(a.worktreePath)).toBe(false);
    expect(idle()).toEqual([]);
  });

  it('never removes a worktree a session claimed while a cleanup was running', async () => {
    const p = await project();
    const a = await session(p.id, 'A');
    const b = await session(p.id, 'B');
    await env.core.sessions.action(a.id, 'archive');
    await env.core.sessions.action(b.id, 'archive');
    run(env.core.db, 'UPDATE worktrees SET released_at = ?', Date.now() - 30 * 24 * 60 * 60 * 1000);
    // The cleanup has listed both and is removing the first when a session claims the second.
    const expiring = env.core.worktrees.expire();
    const claimed = env.core.worktrees.claim(env.repo, 'someone');
    await expiring;
    expect(claimed).not.toBeNull();
    expect(existsSync(claimed!)).toBe(true);
    expect(get(env.core.db, 'SELECT session_id FROM worktrees WHERE path = ?', claimed!)).toEqual({ session_id: 'someone' });
    expect(idle()).toEqual([]);
  });

  it('keeps the idle worktree when a start fails for a reason that has nothing to do with it', async () => {
    const p = await project();
    const a = await session(p.id, 'A');
    await buildAndArchive(a);
    await expect(env.core.sessions.create(p.id, { name: 'Bad base', prompt: 'hi', kind: 'quick', baseBranch: '-oops' })).rejects.toThrow(/isn't a valid branch name/);
    expect(idle()).toEqual([a.worktreePath]);
    expect(existsSync(join(a.worktreePath, 'build', 'out.o'))).toBe(true);
    expect((await session(p.id, 'Good')).worktreePath).toBe(a.worktreePath);
  });

  it('never gives a worktree back to a session that let go of it, even if the pool forgets it', async () => {
    const p = await project();
    const a = await session(p.id, 'A');
    await env.core.sessions.action(a.id, 'archive');
    env.core.worktrees.drop(a.worktreePath);
    const row = env.core.sessions.row(a.id);
    expect(row.worktree_released).toBe(1);
    expect(env.core.sessions.ownsWorktree(row)).toBe(false);
    expect((await env.core.sessions.discardCheck(a.id)).worktreeExists).toBe(false);
  });

  it('removes idle worktrees no session has used for the set number of days', async () => {
    const p = await project();
    const a = await session(p.id, 'Old');
    const b = await session(p.id, 'Recent');
    await env.core.sessions.action(a.id, 'archive');
    await env.core.sessions.action(b.id, 'archive');
    const day = 24 * 60 * 60 * 1000;
    run(env.core.db, 'UPDATE worktrees SET released_at = ? WHERE path = ?', Date.now() - 15 * day, a.worktreePath);
    run(env.core.db, 'UPDATE worktrees SET released_at = ? WHERE path = ?', Date.now() - 13 * day, b.worktreePath);

    env.core.updateConfig((c) => (c.worktrees.idleDays = 0)); // 0 keeps them
    await env.core.worktrees.expire();
    expect(idle()).toHaveLength(2);

    env.core.updateConfig((c) => (c.worktrees.idleDays = 14)); // the default
    await env.core.worktrees.expire();
    expect(idle()).toEqual([b.worktreePath]);
    expect(existsSync(a.worktreePath)).toBe(false);
    expect(existsSync(b.worktreePath)).toBe(true);

    // Startup removes expired ones too.
    run(env.core.db, 'UPDATE worktrees SET released_at = ? WHERE path = ?', Date.now() - 30 * day, b.worktreePath);
    await env.core.worktrees.sweep();
    expect(idle()).toEqual([]);
  });

  it('shows and checks the settings in Settings', async () => {
    const app = await buildServer(env.core, { webDir: null, updates: null });
    try {
      const put = (idleWorktreesPerRepo: unknown) => app.inject({ method: 'PUT', url: '/api/settings', payload: { idleWorktreesPerRepo } });
      expect((await app.inject({ url: '/api/settings' })).json().idleWorktreesPerRepo).toBe(3);
      for (const bad of [-1, 21, 1.5, '2']) expect((await put(bad)).statusCode).toBe(400);
      expect((await put(5)).json().idleWorktreesPerRepo).toBe(5);
      expect(readFileSync(env.core.paths.config, 'utf8')).toMatch(/\[worktrees\]\s+idle_per_repo = 5/);

      const days = (idleWorktreeDays: unknown) => app.inject({ method: 'PUT', url: '/api/settings', payload: { idleWorktreeDays } });
      expect((await app.inject({ url: '/api/settings' })).json().idleWorktreeDays).toBe(14);
      for (const bad of [-1, 366, 2.5, '7']) expect((await days(bad)).statusCode).toBe(400);
      expect((await days(30)).json().idleWorktreeDays).toBe(30);
      expect(readFileSync(env.core.paths.config, 'utf8')).toMatch(/idle_days = 30/);
    } finally {
      await app.close();
    }
  });
});
