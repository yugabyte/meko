import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';
import { get, run } from '../../src/server/db.js';
import { ensureFirstCommit } from '../../src/server/git.js';

const git = (cwd: string, ...a: string[]) => execFileSync('git', a, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();

describe('repos with no commits yet (real session host + git, fake claude)', () => {
  let env: TestEnv;
  let code: string;

  /** Points git at a global config of our own (HOME's is empty in tests, so it has no identity). */
  const gitConfig = (text: string) => {
    const file = join(env.home, 'gitconfig');
    writeFileSync(file, text);
    process.env.GIT_CONFIG_GLOBAL = file;
  };

  beforeEach(async () => {
    env = await makeEnv();
    code = join(env.home, 'code');
    gitConfig('[user]\n\tname = Test\n\temail = test@example.com\n');
  });
  afterEach(async () => {
    delete process.env.GIT_CONFIG_GLOBAL;
    await env.cleanup();
  });

  /** `git init` and nothing more, as a user leaves a repo they just started. */
  const initOnly = (dir: string) => {
    execFileSync('git', ['init', '-q', dir]);
    git(dir, 'symbolic-ref', 'HEAD', 'refs/heads/main');
    return dir;
  };
  const openFolder = (repoPath: string, name = 'Proto') => env.core.projects.create({ repoPath, name, datapack: { mode: 'none' } });
  const commits = (dir: string) => git(dir, 'log', '--format=%s');
  const sessionCount = () => get<{ n: number }>(env.core.db, 'SELECT COUNT(*) AS n FROM sessions')!.n;

  async function started(s: { id: string }) {
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, `${s.id} to start`);
    return env.core.sessions.get(s.id);
  }

  it('gives a repo with no commits an empty first commit when it is opened, so a session can start', async () => {
    const dir = initOnly(join(env.home, 'elsewhere', 'proto'));
    const p = await openFolder(dir);
    expect(p).toMatchObject({ repoPath: dir, defaultBranch: 'main' });
    expect(commits(dir)).toBe('Initial commit');
    const s = await started(await env.core.sessions.create(p.id, { name: 'First work', prompt: 'hi', kind: 'quick' }));
    expect(s).toMatchObject({ branch: 'ak/first-work', baseBranch: 'main' });
    expect(git(s.worktreePath, 'rev-parse', '--abbrev-ref', 'HEAD')).toBe('ak/first-work');
    // Opening it again (another project on it) makes no second commit.
    await openFolder(dir, 'Other');
    expect(commits(dir)).toBe('Initial commit');
  });

  it('makes an empty folder typed under Other folder a repo, once when two opens of it run at once', async () => {
    const dir = join(env.home, 'elsewhere', 'empty');
    mkdirSync(dir, { recursive: true });
    const [a, b] = await Promise.all([openFolder(dir, 'One'), openFolder(dir, 'Two')]);
    expect([a.repoPath, b.repoPath]).toEqual([dir, dir]);
    expect(commits(dir)).toBe('Initial commit');
    expect(readdirSync(dir)).toEqual(['.git']);
  });

  it('writes nothing to the folder when the open is refused for a taken name', async () => {
    const repo = initOnly(join(env.home, 'elsewhere', 'proto'));
    const empty = join(env.home, 'elsewhere', 'empty');
    mkdirSync(empty, { recursive: true });
    // A project named Taken on each, as one opened before Kode made first commits.
    for (const [id, dir] of [['p1', repo], ['p2', empty]]) {
      run(env.core.db, `INSERT INTO projects(id, name, repo_path, default_branch, position, settings_json, created_at)
                        VALUES (?, 'Taken', ?, 'main', 0, '{}', ?)`, id, dir, Date.now());
      await expect(openFolder(dir, 'Taken')).rejects.toMatchObject({ status: 409 });
    }
    expect(git(repo, 'rev-list', '-n', '1', '--all')).toBe('');
    expect(readdirSync(empty)).toEqual([]);
  });

  it('leaves a folder with files that is not a repo alone, and says how to fix it', async () => {
    const dir = join(env.home, 'elsewhere', 'notes');
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, 'notes.txt'), 'mine\n');
    await expect(openFolder(dir)).rejects.toMatchObject({ status: 400, message: expect.stringMatching(/isn't a git repository.*git init/) });
    expect(readdirSync(dir)).toEqual(['notes.txt']);
  });

  it('removes the .git it made in an empty folder when the first commit fails', async () => {
    gitConfig('[user]\n\tuseConfigOnly = true\n');
    const dir = join(env.home, 'elsewhere', 'empty');
    mkdirSync(dir, { recursive: true });
    await expect(openFolder(dir)).rejects.toMatchObject({ status: 400, message: expect.stringMatching(/git needs your name and email/) });
    expect(readdirSync(dir)).toEqual([]);
  });

  it("refuses a repo with no commits but files in it, committing nothing: they'd be missing from every worktree", async () => {
    const dir = initOnly(join(env.home, 'elsewhere', 'proto'));
    writeFileSync(join(dir, 'app.py'), 'print(1)\n');
    await expect(openFolder(dir)).rejects.toMatchObject({ status: 400, message: expect.stringMatching(/has no commits yet.*git add -A/) });
    expect(git(dir, 'rev-list', '-n', '1', '--all')).toBe('');
    expect(env.core.state().projects).toEqual([]);
  });

  it("gives a session's repo with no commits its first commit at start, or refuses it before making the session", async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'none' } });
    // A scanned repo the session works in, as one opened before Kode made first commits.
    const other = initOnly(join(code, 'scratch'));
    writeFileSync(join(other, 'draft.md'), 'draft\n');
    await expect(env.core.sessions.create(p.id, { name: 'There', prompt: 'hi', kind: 'quick', repoPath: other }))
      .rejects.toMatchObject({ status: 400, message: expect.stringMatching(/has no commits yet/) });
    expect(sessionCount()).toBe(0);

    rmSync(join(other, 'draft.md'));
    const s = await started(await env.core.sessions.create(p.id, { name: 'There', prompt: 'hi', kind: 'quick', repoPath: other }));
    expect(s).toMatchObject({ repoPath: other, branch: 'ak/there', baseBranch: 'main' });
    expect(commits(other)).toBe('Initial commit');
  });

  it('makes one first commit when two are asked for at once', async () => {
    const dir = initOnly(join(code, 'twice'));
    const made = await Promise.all([ensureFirstCommit(dir), ensureFirstCommit(dir)]);
    expect(made.sort()).toEqual([false, true]);
    expect(commits(dir)).toBe('Initial commit');
    // A repo with commits is left as it is.
    expect(await ensureFirstCommit(env.repo)).toBe(false);
    expect(existsSync(join(dir, '.git'))).toBe(true);
  });
});
