import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';
import { run } from '../../src/server/db.js';

function argvLog(env: TestEnv): { args: string[]; env: Record<string, string> }[] {
  const f = join(env.fakeDir, 'argv.jsonl');
  return existsSync(f) ? readFileSync(f, 'utf8').trim().split('\n').map((l) => JSON.parse(l)) : [];
}

describe('projects and sessions (real tmux + git, fake claude)', () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv();
  });
  afterEach(async () => {
    await env.cleanup();
  });

  async function newProject() {
    return env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
  }

  it('lists repos and creates a project with a dedicated datapack and Meko pin', async () => {
    const repos = await env.core.projects.repos();
    expect(repos.repos.map((r) => r.name)).toEqual(['demo-repo']);
    const p = await newProject();
    expect(p.id).toBe('demo-repo');
    expect(p.datapacks).toEqual([{ id: 'dp-agent-k-local-test-demo', name: 'agent-k-local/test/demo', role: 'dedicated' }]);
    const pin = JSON.parse(readFileSync(join(process.env.MEKO_WATERMARK_DIR!, 'pin-claude_code_demo-repo.json'), 'utf8'));
    expect(pin.datapack_id).toBe('dp-agent-k-local-test-demo');
    expect((await env.core.projects.repos()).repos[0].projectId).toBe('demo-repo');
    await expect(newProject()).rejects.toThrow(/already belongs/);
  });

  it('refuses to create a project when Meko is down', async () => {
    env.meko.check = async () => {
      throw new Error('boom');
    };
    env.core.mekoStatus = { ...env.core.mekoStatus, ok: false, error: 'boom', checkedAt: Date.now() };
    await expect(newProject()).rejects.toThrow(/Meko is unreachable/);
  });

  it('runs a session in a worktree, tracks status from hooks, and archives it', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Fix the thing', prompt: 'do it', skipDesign: true });
    expect(s.branch).toBe('ak/fix-the-thing');
    expect(s.stage).toBe('code');
    expect(existsSync(s.worktreePath)).toBe(true);

    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn to finish');
    expect(env.core.sessions.get(s.id).startup).toBeNull();
    const first = argvLog(env)[0];
    expect(first.args).toContain('--session-id');
    expect(first.args.at(-1)).toContain('do it');
    expect(first.env).toEqual({ MEKO_AGENT_ID: 'claude_code:demo-repo', MEKO_DATAPACK_ID: 'dp-agent-k-local-test-demo' });

    await env.core.sessions.submit(s.id, 'second turn');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'second turn');

    // Token usage is read from the transcript on each Stop (duplicated lines count once).
    const usage = await waitFor(() => env.core.sessions.get(s.id).usage, (u) => u.messages === 2, 10000, 'usage');
    expect(usage).toEqual({ input: 6, output: 100, cacheRead: 2000, cacheWrite: 400, messages: 2 });
    expect(env.core.state().projects[0].usage.output).toBe(100);
    const home = await env.core.projects.home(p.id);
    expect(home.usage.bySession[0]).toMatchObject({ sessionId: s.id, archived: false });
    expect(home.usage.byModel[0].model).toBe('claude-fake-1');

    await env.core.sessions.action(s.id, 'archive');
    const panes = await env.core.tmux.panes();
    expect(panes.has(s.tmuxName)).toBe(false);
    expect(existsSync(s.worktreePath)).toBe(false);
    const branches = execFileSync('git', ['branch', '--list', 'ak/*'], { cwd: env.repo, encoding: 'utf8' });
    expect(branches).toContain('ak/fix-the-thing'); // archive keeps the branch
    expect(env.core.state().projects[0].sessions).toHaveLength(0);
    expect(env.core.state().projects[0].usage.messages).toBe(2); // archived sessions still count
  });

  it('captures the design and moves to Code when it is approved', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Design me', prompt: 'PLAN it' });
    expect(s.stage).toBe('design');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'needs_input' && x.hasDesign, 10000, 'design');
    expect(argvLog(env)[0].args).toEqual(expect.arrayContaining(['--permission-mode', 'plan']));
    await env.core.sessions.action(s.id, 'approve-design');
    expect(env.core.sessions.get(s.id).stage).toBe('code');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'turn end after approval');
    expect(env.meko.calls.some((c) => c.name === 'putArtifact')).toBe(true);
  });

  it('reports startup steps while a session starts', async () => {
    const p = await newProject();
    const steps: string[] = [];
    const onChange = () => {
      const s = env.core.state().projects[0]?.sessions[0];
      if (s?.startup && steps.at(-1) !== s.startup.step) steps.push(s.startup.step);
    };
    env.core.on('change', onChange);
    const s = await env.core.sessions.create(p.id, { name: 'Startup steps', prompt: 'hi', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    env.core.off('change', onChange);
    expect(steps.slice(0, 3)).toEqual(['worktree', 'launch', 'loading']);
    expect(steps).toContain('prompt');
    expect(env.core.sessions.get(s.id).startup).toBeNull();
  });

  it('marks exited sessions and resumes them with --resume', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Exit test', prompt: 'hi', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    await env.core.tmux.submit(s.tmuxName, 'exit');
    await waitFor(async () => {
      await env.core.sessions.reconcile();
      return env.core.sessions.get(s.id).status;
    }, (st) => st === 'exited', 10000, 'exited');
    const before = argvLog(env).length;
    await env.core.sessions.action(s.id, 'resume');
    await waitFor(() => argvLog(env).length, (n) => n > before, 10000, 'resume launch');
    const last = argvLog(env).at(-1)!;
    expect(last.args).toEqual(expect.arrayContaining(['--resume', s.claudeSessionId]));
  });

  it('recovers after tmux dies: auto-continues mid-turn sessions, with a 15 minute guard', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Crash test', prompt: 'hi', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');

    // Simulate a host crash in the middle of a turn.
    let launches = argvLog(env).length;
    run(env.core.db, "UPDATE sessions SET status = 'working' WHERE id = ?", s.id);
    execFileSync('tmux', ['-L', env.socket, 'kill-server']);
    await env.core.sessions.reconcile();
    let after = env.core.sessions.get(s.id);
    expect(after.recoveredAt).not.toBeNull();
    expect(after.statusDetail).toMatch(/continuing/);
    await waitFor(() => argvLog(env).length, (n) => n > launches, 10000, 'relaunch');
    const relaunch = argvLog(env).at(-1)!;
    expect(relaunch.args).toEqual(expect.arrayContaining(['--resume', s.claudeSessionId]));
    expect(relaunch.args.at(-1)).toMatch(/host restarted/);
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'continued turn');

    // A second crash right away: resumed, but not auto-continued.
    launches = argvLog(env).length;
    run(env.core.db, "UPDATE sessions SET status = 'working' WHERE id = ?", s.id);
    execFileSync('tmux', ['-L', env.socket, 'kill-server']);
    await env.core.sessions.reconcile();
    after = env.core.sessions.get(s.id);
    expect(after.status).toBe('recovered');
    expect(after.statusDetail).toMatch(/not auto-continued/);
    await waitFor(() => argvLog(env).length, (n) => n > launches, 10000, 'second relaunch');
    expect(argvLog(env).at(-1)!.args.at(-1)).not.toMatch(/host restarted/);
  });

  it('boot recovery leaves running sessions alone', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Keep running', prompt: 'hi', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    const launches = argvLog(env).length;
    await env.core.sessions.recover();
    expect(argvLog(env).length).toBe(launches);
  });
});
