import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { modDirPath } from '../../src/server/claude.js';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';

/** Launches the fake Claude recorded: its arguments and the variables it was started with. */
function launches(env: TestEnv): { args: string[]; env: Record<string, string> }[] {
  const f = join(env.fakeDir, 'argv.jsonl');
  return existsSync(f) ? readFileSync(f, 'utf8').trim().split('\n').map((l) => JSON.parse(l)) : [];
}

/** The prompts the fake Claude took through its mod. */
function modPrompts(env: TestEnv): { id: string; text: string }[] {
  const f = join(env.fakeDir, 'mod-prompts.jsonl');
  return existsSync(f) ? readFileSync(f, 'utf8').trim().split('\n').map((l) => JSON.parse(l)) : [];
}

/** What the fake Claude read from its terminal (every chunk, while its dir has a `debug` file). */
function typed(env: TestEnv): string {
  const f = join(env.fakeDir, 'stdin.log');
  return existsSync(f) ? readFileSync(f, 'utf8') : '';
}

/** Files in the fake Claude's dir that turn its behaviours on (see fake-claude.mjs). */
function fakeFiles(env: TestEnv, files: Record<string, string>): void {
  mkdirSync(env.fakeDir, { recursive: true });
  for (const [f, text] of Object.entries(files)) writeFileSync(join(env.fakeDir, f), text);
}

describe("AgentK's mod in Claude (real session host + git, fake claude)", () => {
  let env: TestEnv;
  afterEach(async () => {
    await env.cleanup();
  });

  async function session(name = 'Fix the thing') {
    const p = env.core.state().projects[0] ?? await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name, prompt: 'do it', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    return s;
  }

  it('is written to the run folder and loaded into each session, with hooks modules turned on', async () => {
    env = await makeEnv();
    expect(existsSync(join(modDirPath(env.core.paths), 'hooks', 'register.ts'))).toBe(true);
    await session();
    const [first] = launches(env);
    expect(first.args).toEqual(expect.arrayContaining(['--plugin-dir', modDirPath(env.core.paths)]));
    expect(first.env.CLAUDE_CODE_ENABLE_FUNCTION_HOOKS).toBe('1');
  });

  it('sends prompts through the mod while it polls, never typing them, and shows the context fill', async () => {
    env = await makeEnv();
    fakeFiles(env, { mod: '', debug: '', 'context-pct': '64' });
    const s = await session();
    await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, 'the mod to poll');
    await env.core.sessions.submit(s.id, 'second turn through the mod');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'second turn');
    expect(modPrompts(env).map((p) => p.text)).toEqual(['second turn through the mod']);
    expect(typed(env)).not.toContain('second turn');
    // The turn ran as a typed one would: its hooks moved the status, and it's in the transcript.
    const transcript = readFileSync(env.core.sessions.row(s.id).transcript_path!, 'utf8');
    expect(transcript).toContain('second turn through the mod');
    await waitFor(() => env.core.sessions.get(s.id).contextPercent, (p) => p === 64, 5000, 'the context fill');
    expect(env.core.limits.current()).toMatchObject({ fiveHour: { usedPercent: 21 }, sevenDay: { usedPercent: 9 } });
  });

  it('types prompts into the terminal when Claude runs without the mod', async () => {
    env = await makeEnv();
    fakeFiles(env, { debug: '' });
    const s = await session();
    await env.core.sessions.submit(s.id, 'typed turn');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'second turn');
    expect(typed(env)).toContain('typed turn');
    expect(modPrompts(env)).toEqual([]);
    expect(env.core.sessions.mod.live(s.id)).toBe(false);
    expect(env.core.sessions.get(s.id).contextPercent).toBeNull();
  });

  it("fails a prompt the mod doesn't take, without also typing it", async () => {
    env = await makeEnv();
    fakeFiles(env, { mod: '', debug: '', 'mod-silent': '' });
    const s = await session();
    await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, 'the mod to poll');
    await expect(env.core.sessions.submit(s.id, 'never acked')).rejects.toThrow(/didn't take the prompt/);
    expect(modPrompts(env).map((p) => p.text)).toEqual(['never acked']);
    expect(typed(env)).not.toContain('never acked');
  });

  it("asks the user to step in when another plugin refuses AgentK's prompt", async () => {
    env = await makeEnv();
    fakeFiles(env, { mod: '', 'mod-drop': '' });
    const s = await session();
    await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, 'the mod to poll');
    await env.core.sessions.submit(s.id, 'refused');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'needs_input', 5000, 'needs input');
    expect(env.core.sessions.get(s.id).statusDetail).toBe(
      "A plugin in Claude refused AgentK's prompt \"refused\": held by another plugin. Its text is in Claude's prompt box: press Enter in the terminal to send it, or edit it first.",
    );
  });

  it("answers held polls when the hook socket is bound again, so hooks aren't kept out", async () => {
    env = await makeEnv();
    fakeFiles(env, { mod: '' });
    const s = await session();
    await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, 'the mod to poll');
    rmSync(env.core.paths.hookSock);
    const started = Date.now();
    await env.core.checkHookSocket();
    expect(Date.now() - started).toBeLessThan(3000); // not the 20 s a held poll would take
    expect(existsSync(env.core.paths.hookSock)).toBe(true);
    // The mod polls the new socket, and prompts go through it again.
    await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, 'the mod to poll again');
    await env.core.sessions.submit(s.id, 'after the rebind');
    await waitFor(() => modPrompts(env).map((p) => p.text), (t) => t.includes('after the rebind'), 10000, 'the prompt');
  });

  it('types prompts again once the config turns the mod off, though running sessions keep it loaded', async () => {
    env = await makeEnv();
    fakeFiles(env, { mod: '', debug: '' });
    const s = await session();
    await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, 'the mod to poll');
    env.core.updateConfig((c) => {
      c.claude.mod = false;
    });
    await env.core.sessions.submit(s.id, 'typed after turning it off');
    await waitFor(() => typed(env), (t) => t.includes('typed after turning it off'), 10000, 'the typed prompt');
    expect(modPrompts(env)).toEqual([]);
  });

  it('forgets the mod when Claude is relaunched, until the new one polls', async () => {
    env = await makeEnv();
    fakeFiles(env, { mod: '' });
    const s = await session();
    await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, 'the mod to poll');
    const reset = vi.spyOn(env.core.sessions.mod, 'reset');
    await env.core.host.kill(env.core.sessions.row(s.id).tmux_name);
    await env.core.sessions.reconcile();
    await waitFor(() => launches(env).length, (n) => n === 2, 10000, 'the relaunch');
    expect(reset.mock.calls.map(([id]) => id)).toContain(s.id);
    await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, "the new Claude's mod to poll");
  });

  it('shows a too-old Claude Code, reports a failed update, and loads the mod after one that works', async () => {
    env = await makeEnv({ start: false });
    fakeFiles(env, { version: '2.1.280', 'update-fail': 'Error: permission denied' });
    await env.core.start();
    await waitFor(() => env.core.state().claudeCode.version, (v) => v === '2.1.280', 5000, 'the version');
    expect(env.core.state().claudeCode).toMatchObject({ supportsMod: false, minModVersion: '2.1.287', modEnabled: true, updating: false });
    await session('Before');
    expect(launches(env)[0].args).not.toContain('--plugin-dir');
    expect(launches(env)[0].env.CLAUDE_CODE_ENABLE_FUNCTION_HOOKS).toBeUndefined();

    // A failed update is reported.
    const failed = await env.core.claudeCode.update();
    expect(failed).toMatchObject({ version: '2.1.280', supportsMod: false });
    expect(failed.lastUpdate).toMatchObject({ ok: false, message: 'Error: permission denied' });

    rmSync(join(env.fakeDir, 'update-fail'));
    fakeFiles(env, { 'update-to': '2.1.291' });
    const after = await env.core.claudeCode.update();
    expect(after).toMatchObject({ version: '2.1.291', supportsMod: true });
    expect(after.lastUpdate).toMatchObject({ ok: true, from: '2.1.280', to: '2.1.291', message: expect.stringContaining('Successfully updated') });
    await session('After');
    expect(launches(env)[1].args).toEqual(expect.arrayContaining(['--plugin-dir', modDirPath(env.core.paths)]));
  });
});
