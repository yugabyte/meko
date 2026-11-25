import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { makeEnv, type TestEnv } from '../helpers.js';
import { run } from '../../src/server/db.js';
import { addWorktree, isFreshRepo } from '../../src/server/git.js';

const git = (cwd: string, ...a: string[]) => execFileSync('git', a, { cwd, encoding: 'utf8' }).trim();

describe('Open project on a new repo', () => {
  let env: TestEnv;
  let code: string;

  /** Points git at a global config of our own (HOME's is empty in tests, so it has no identity). */
  const gitConfig = (text: string) => {
    const file = join(env.home, 'gitconfig');
    writeFileSync(file, text);
    process.env.GIT_CONFIG_GLOBAL = file;
  };

  beforeEach(async () => {
    env = await makeEnv({ start: false });
    code = join(env.home, 'code');
    gitConfig('[user]\n\tname = Test\n\temail = test@example.com\n');
  });
  afterEach(async () => {
    for (const k of ['GIT_CONFIG_GLOBAL', 'LANG', 'LANGUAGE']) delete process.env[k];
    await env.cleanup();
  });

  const open = (newRepo: string, name = 'Fresh') => env.core.projects.create({ newRepo, name, datapack: { mode: 'none' } });

  it('makes the repo in the clone folder with an empty first commit, ready for a worktree', async () => {
    const p = await open('fresh');
    const dest = join(code, 'fresh');
    expect(p).toMatchObject({ id: 'fresh', repoPath: dest, defaultBranch: 'main' });
    expect(git(dest, 'symbolic-ref', '--short', 'HEAD')).toBe('main');
    expect(git(dest, 'log', '--format=%s %an')).toBe('Initial commit Test');
    expect(await isFreshRepo(dest)).toBe(true);
    // A session's branch can start from it straight away.
    const wt = join(env.home, 'wt');
    await addWorktree(dest, wt, 'ak/first', 'main');
    expect(git(wt, 'rev-parse', '--abbrev-ref', 'HEAD')).toBe('ak/first');
    // It's listed with the scanned repos, and the list says where new repos go.
    const repos = await env.core.projects.repos();
    expect(repos.cloneInto).toBe(code);
    expect(repos.repos.find((r) => r.path === dest)?.projects.map((x) => x.name)).toEqual(['Fresh']);
  });

  it("uses the user's init.defaultBranch", async () => {
    gitConfig('[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = trunk\n');
    const p = await open('fresh');
    expect(p.defaultBranch).toBe('trunk');
    expect(git(join(code, 'fresh'), 'symbolic-ref', '--short', 'HEAD')).toBe('trunk');
  });

  it("says how to fix a missing git identity, in any locale, and leaves no folder behind", async () => {
    gitConfig('[user]\n\tuseConfigOnly = true\n');
    // git's messages would be German here (where its translations are installed).
    process.env.LANG = 'de_DE.UTF-8';
    process.env.LANGUAGE = 'de';
    await expect(open('fresh')).rejects.toThrow(/git needs your name and email.*git config --global user\.name/);
    expect(existsSync(join(code, 'fresh'))).toBe(false);
  });

  it("makes the first commit without the user's signing or hooks, which could wait on a prompt", async () => {
    const hooks = join(env.home, 'hooks');
    mkdirSync(hooks);
    writeFileSync(join(hooks, 'pre-commit'), '#!/bin/sh\nexit 1\n', { mode: 0o755 });
    gitConfig(`[user]\n\tname = Test\n\temail = test@example.com\n[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = false\n[core]\n\thooksPath = ${hooks}\n`);
    await open('fresh');
    expect(git(join(code, 'fresh'), 'log', '--format=%s')).toBe('Initial commit');
  });

  it('names the repos folder when it is the problem, not the new repo', async () => {
    const file = join(env.home, 'not-a-folder');
    writeFileSync(file, '');
    env.core.updateConfig((c) => {
      c.repos.cloneInto = join(file, 'code');
    });
    await expect(open('fresh')).rejects.toMatchObject({ status: 400, message: expect.stringMatching(/Couldn't make the repos folder .*not-a-folder/) });
  });

  it('refuses a second open of a new repo while the first is still being opened', async () => {
    let answer!: () => void;
    const meko = env.meko as { createDatapack: unknown };
    const createDatapack = meko.createDatapack;
    meko.createDatapack = () => new Promise((res) => (answer = () => res({ id: 'dp-fresh', name: 'adlc/proj/fresh/fresh' })));
    const first = env.core.projects.create({ newRepo: 'fresh', name: 'Fresh', datapack: { mode: 'create', name: 'adlc/proj/fresh/fresh' } });
    await vi.waitFor(() => expect(answer).toBeDefined());
    meko.createDatapack = createDatapack;
    // The repo the first one made looks like an earlier try's, but it's still being opened.
    expect(await isFreshRepo(join(code, 'fresh'))).toBe(true);
    await expect(open('fresh', 'Other')).rejects.toMatchObject({ status: 409, message: expect.stringMatching(/already exists/) });
    answer();
    expect((await first).repoPath).toBe(join(code, 'fresh'));
    expect(env.core.state().projects.map((p) => p.name)).toEqual(['Fresh']);
  });

  it('refuses names that are not one plain folder name, creating nothing', async () => {
    const before = readdirSync(code).sort();
    // One name per rule (test/unit/new-repo-name.test.ts has the rest).
    for (const n of ['../x', '..', ' ', 'x'.repeat(101)]) {
      await expect(open(n), n).rejects.toMatchObject({ status: 400 });
    }
    expect(readdirSync(code).sort()).toEqual(before);
    expect(existsSync(join(env.home, 'x'))).toBe(false);
  });

  it('leaves a folder that is already there alone, unless it is a repo an earlier try made', async () => {
    mkdirSync(join(code, 'plain'));
    writeFileSync(join(code, 'plain', 'notes.txt'), 'mine\n');
    await expect(open('plain')).rejects.toMatchObject({ status: 409, message: expect.stringMatching(/already exists/) });
    expect(readdirSync(join(code, 'plain'))).toEqual(['notes.txt']);
    // A repo with commits of its own (the test repo) isn't taken over either.
    await expect(open('demo-repo')).rejects.toMatchObject({ status: 409 });
    // Nor is an empty one with a remote.
    execFileSync('git', ['init', '-q', join(code, 'remoted')]);
    git(join(code, 'remoted'), 'commit', '-q', '--allow-empty', '-m', 'Initial commit');
    git(join(code, 'remoted'), 'remote', 'add', 'origin', 'https://example.com/a/remoted.git');
    await expect(open('remoted')).rejects.toMatchObject({ status: 409 });
  });

  /** A repo made by hand just as an earlier try would have left it (one empty commit, no remote). */
  const handMade = (n: string) => {
    const dir = join(code, n);
    execFileSync('git', ['init', '-q', dir]);
    git(dir, 'commit', '-q', '--allow-empty', '-m', 'Initial commit');
    return dir;
  };

  it('counts files git ignores, so a folder worked in is never taken for an earlier try', async () => {
    const dir = handMade('proto');
    writeFileSync(join(dir, '.git', 'info', 'exclude'), '.env\n');
    writeFileSync(join(dir, '.env'), 'SECRET=1\n');
    expect(await isFreshRepo(dir)).toBe(false);
    await expect(open('proto')).rejects.toMatchObject({ status: 409 });
  });

  it("doesn't take a repo another project's running session works in", async () => {
    const dir = handMade('fresh');
    const other = await env.core.projects.create({ repoPath: env.repo, name: 'Other', datapack: { mode: 'none' } });
    run(env.core.db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, repo_path, worktree_path,
                        tmux_name, hook_token, desired, status, kind, stage, created_at)
                      VALUES ('s1', ?, 's1', 'S1', 'c1', 'ak/s1', 'main', ?, ?, 't1', 'h1', 'running', 'idle', 'quick', 'code', ?)`,
      other.id, dir, join(env.home, 'wt'), Date.now());
    await expect(open('fresh')).rejects.toMatchObject({ status: 409 });
    // Once that session is archived, the repo is free again.
    run(env.core.db, `UPDATE sessions SET desired = 'archived' WHERE id = 's1'`);
    expect((await open('fresh')).repoPath).toBe(dir);
  });

  it('never takes a folder spelled differently, as "Fresh" finds "fresh" on a case-insensitive disk', async () => {
    handMade('fresh');
    // On Linux "Fresh" is its own new folder; on macOS it's the repo above, and is refused.
    const caseless = existsSync(join(code, 'FRESH'));
    if (caseless) await expect(open('Fresh')).rejects.toMatchObject({ status: 409 });
    else expect((await open('Fresh')).repoPath).toBe(join(code, 'Fresh'));
  });

  it('words a clone folder already there the same way', async () => {
    mkdirSync(join(code, 'thing'));
    await expect(env.core.projects.create({ cloneUrl: 'https://example.com/a/thing.git', name: 'Thing', datapack: { mode: 'none' } }))
      .rejects.toMatchObject({ status: 409, message: `${join(code, 'thing')} already exists. Pick it from the list instead.` });
  });

  it('uses the repo again when a try failed after making it, but not once a project is on it', async () => {
    const meko = env.meko as { createDatapack: unknown };
    const createDatapack = meko.createDatapack;
    meko.createDatapack = async () => {
      throw new Error('Meko is down');
    };
    const withDp = () => env.core.projects.create({ newRepo: 'fresh', name: 'Fresh', datapack: { mode: 'create', name: 'adlc/proj/fresh/fresh' } });
    await expect(withDp()).rejects.toMatchObject({ status: 502 });
    expect(await isFreshRepo(join(code, 'fresh'))).toBe(true);
    meko.createDatapack = createDatapack;
    const p = await withDp();
    expect(p.repoPath).toBe(join(code, 'fresh'));
    expect(git(join(code, 'fresh'), 'rev-list', '--count', 'HEAD')).toBe('1');
    await expect(open('fresh', 'Other')).rejects.toMatchObject({ status: 409 });
  });

  it('needs exactly one repo source', async () => {
    await expect(env.core.projects.create({ newRepo: 'x', repoPath: env.repo, name: 'X', datapack: { mode: 'none' } })).rejects.toMatchObject({ status: 400 });
    await expect(env.core.projects.create({ name: 'X', datapack: { mode: 'none' } })).rejects.toMatchObject({ status: 400 });
    expect(existsSync(join(code, 'x'))).toBe(false);
  });
});
