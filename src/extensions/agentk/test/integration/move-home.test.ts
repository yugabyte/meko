import { execFileSync } from 'node:child_process';
import { chmodSync, existsSync, lstatSync, mkdirSync, readdirSync, readFileSync, readlinkSync, symlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { Core } from '../../src/server/core.js';
import { all, get, openDb, run } from '../../src/server/db.js';
import { SessionHost } from '../../src/server/hostclient.js';
import { startHost, type HostHandle } from '../../src/server/host/host.js';
import { silentLogger } from '../../src/server/log.js';
import { JOURNAL, moveHome, type MovePlan } from '../../src/server/movehome.js';
import { getPaths, homeIn } from '../../src/server/paths.js';
import { makeEnv, readJsonl, testLockDeps, waitFor, type TestEnv } from '../helpers.js';

const key = (cwd: string) => cwd.replace(/[^A-Za-z0-9]/g, '-');

function argvLog(env: TestEnv): { args: string[]; cwd: string }[] {
  const f = join(env.fakeDir, 'argv.jsonl');
  return readJsonl(f);
}

describe("moving AgentK's home into Meko's data folder (real session host + git, fake claude)", () => {
  let env: TestEnv;
  let from: string;
  let to: string;
  const open: { close(): Promise<unknown> | unknown }[] = [];

  beforeEach(async () => {
    env = await makeEnv();
    from = join(env.home, 'ak');
    to = homeIn(join(env.home, 'meko'));
  });
  afterEach(async () => {
    for (const o of open.splice(0).reverse()) await o.close();
    await env.cleanup();
  });

  /** A running session and an idle worktree, with what outside the home names them. */
  async function setUp() {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Fix it', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 15000, 'the first turn to finish');
    const row = get<{ worktree_path: string; claude_session_id: string }>(env.core.db, 'SELECT worktree_path, claude_session_id FROM sessions WHERE id = ?', s.id)!;
    // A Claude Code transcript where --resume looks for it, read for usage.
    const claudeDir = process.env.CLAUDE_CONFIG_DIR!;
    const transcript = join(claudeDir, 'projects', key(row.worktree_path), `${row.claude_session_id}.jsonl`);
    mkdirSync(join(transcript, '..'), { recursive: true });
    writeFileSync(transcript, '{}\n');
    run(env.core.db, 'INSERT INTO usage_files(session_id, path, offset) VALUES (?, ?, 3)', s.id, transcript);
    writeFileSync(join(claudeDir, '.claude.json'), JSON.stringify({ projects: { [row.worktree_path]: { hasTrustDialogAccepted: true } } }));
    mkdirSync(join(process.env.KIRO_HOME!, 'sessions', 'cli'), { recursive: true });
    writeFileSync(join(process.env.KIRO_HOME!, 'sessions', 'cli', 'k-1.json'), JSON.stringify({ cwd: row.worktree_path, created_at: 'x' }));
    mkdirSync(join(env.home, '.local', 'bin'), { recursive: true });
    symlinkSync(join(from, 'app', 'current', 'bin', 'agentk'), join(env.home, '.local', 'bin', 'agentk'));
    // Stopped, as the move's first step does.
    await env.core.stop();
    await env.killHost();
    return { session: s.id, worktree: row.worktree_path, transcript, claudeSession: row.claude_session_id };
  }

  const deps = () => ({
    log: { info: () => undefined, warn: () => undefined },
    stop: async () => undefined,
    services: null,
    claudeDir: process.env.CLAUDE_CONFIG_DIR!,
    kiroHome: process.env.KIRO_HOME!,
    home: env.home,
  });

  /** A server on the new home: its recovery resumes the sessions there. */
  async function startOn(home: string) {
    const paths = getPaths(home);
    const db = openDb(paths.defaultDb);
    let handle: HostHandle | null = null;
    const host = new SessionHost(paths, { services: () => null, start: async () => void (handle ??= await startHost({ paths, version: '0.0.0-test', lockDeps: testLockDeps() })) });
    const core = new Core({ paths, config: env.core.config, db, version: '0.0.0-test', meko: env.meko, mekoCaptureScript: null, host, services: () => null, log: silentLogger, timers: false, mekoApi: env.mekoApi.api, mekoTools: env.mekoTools.tools, localMeko: async () => ({ ...env.local }), ghProbe: async () => env.gh.state });
    open.push({ close: () => db.close() }, { close: () => handle?.close() }, { close: () => core.stop() });
    await core.start();
    return core;
  }

  it.each<MovePlan['method']>(['rename', 'copy'])('moves everything that named the old home (%s), and the sessions resume there', async (method) => {
    const s = await setUp();
    const moved = join(to, s.worktree.slice(from.length));
    // Claude Code's folder for the new path has something already (its memory, say): merged, and
    // an old file of the same name is kept where it was rather than lost.
    const projects = join(process.env.CLAUDE_CONFIG_DIR!, 'projects');
    for (const [dir, file] of [[key(moved), 'mine.md'], [key(s.worktree), 'old.md'], [key(s.worktree), 'mine.md']]) {
      mkdirSync(join(projects, dir, 'memory'), { recursive: true });
      writeFileSync(join(projects, dir, 'memory', file), dir);
    }

    expect(await moveHome({ from, to, method }, deps())).toBe(to);
    expect(readdirSync(join(projects, key(moved), 'memory')).sort()).toEqual(['mine.md', 'old.md']);
    expect(readFileSync(join(projects, key(moved), 'memory', 'mine.md'), 'utf8')).toBe(key(moved));
    expect(readFileSync(join(projects, key(s.worktree), 'memory', 'mine.md'), 'utf8')).toBe(key(s.worktree));

    expect(lstatSync(from).isSymbolicLink() && readlinkSync(from)).toBe(to);
    expect(existsSync(join(to, JOURNAL)) || existsSync(`${from}.moved`)).toBe(false);
    const worktrees = execFileSync('git', ['-C', env.repo, 'worktree', 'list', '--porcelain'], { encoding: 'utf8' });
    expect(worktrees).toContain(`worktree ${moved}\n`);
    execFileSync('git', ['-C', env.repo, 'worktree', 'prune']);
    expect(execFileSync('git', ['-C', env.repo, 'worktree', 'list'], { encoding: 'utf8' })).toContain(moved);

    const db = openDb(join(to, 'state.db'));
    open.push({ close: () => db.close() });
    const newTranscript = join(process.env.CLAUDE_CONFIG_DIR!, 'projects', key(moved), `${s.claudeSession}.jsonl`);
    expect(get(db, 'SELECT worktree_path AS w FROM sessions WHERE id = ?', s.session)).toEqual({ w: moved });
    expect(all(db, 'SELECT path FROM worktrees')).toEqual([{ path: moved }]);
    expect(all(db, 'SELECT path FROM usage_files WHERE offset = 3')).toEqual([{ path: newTranscript }]);
    expect(all(db, "SELECT text FROM session_events WHERE kind = 'home-moved'")).toEqual([{ text: `AgentK moved its home to ${to}; the session resumed there` }]);
    expect(existsSync(newTranscript) && !existsSync(s.transcript)).toBe(true);
    expect(JSON.parse(readFileSync(join(process.env.CLAUDE_CONFIG_DIR!, '.claude.json'), 'utf8')).projects[moved]).toEqual({ hasTrustDialogAccepted: true });
    expect(JSON.parse(readFileSync(join(process.env.KIRO_HOME!, 'sessions', 'cli', 'k-1.json'), 'utf8')).cwd).toBe(moved);
    expect(readlinkSync(join(env.home, '.local', 'bin', 'agentk'))).toBe(join(to, 'app', 'current', 'bin', 'agentk'));

    const before = argvLog(env).length;
    await startOn(to);
    const resumed = await waitFor(() => argvLog(env).slice(before), (l) => l.length > 0, 15000, 'the session to resume');
    expect(resumed[0]).toMatchObject({ cwd: moved, args: expect.arrayContaining(['--resume', s.claudeSession]) });
  });

  it('goes on where a move stopped, without doing a step twice', async () => {
    const s = await setUp();
    // Repointing ~/.local/bin/agentk fails while the folder is read-only: the move stops there.
    chmodSync(join(env.home, '.local', 'bin'), 0o555);
    await expect(moveHome({ from, to, method: 'rename' }, deps())).rejects.toThrow();
    expect(JSON.parse(readFileSync(join(to, JOURNAL), 'utf8'))).toMatchObject({ phase: 'moving', done: ['db', 'paths', 'claude', 'kiro', 'git'] });
    chmodSync(join(env.home, '.local', 'bin'), 0o755);
    // A link step cut off between its temp link and the rename leaves the temp link behind.
    symlinkSync('nowhere', join(env.home, '.local', 'bin', 'agentk.moving'));

    expect(await moveHome({ from, to, method: 'rename' }, deps())).toBe(to);
    expect(existsSync(join(to, JOURNAL))).toBe(false);
    expect(readlinkSync(join(env.home, '.local', 'bin', 'agentk'))).toBe(join(to, 'app', 'current', 'bin', 'agentk'));
    const db = openDb(join(to, 'state.db'));
    open.push({ close: () => db.close() });
    expect(all(db, "SELECT session_id FROM session_events WHERE kind = 'home-moved'")).toEqual([{ session_id: s.session }]);
    expect(readdirSync(join(env.home, 'meko', 'extensions'))).toEqual(['agentk']);
  });
});
