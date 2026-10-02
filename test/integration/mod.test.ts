import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
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

describe("AgentK's mod in Claude (real tmux + git, fake claude)", () => {
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
    expect(env.core.sessions.get(s.id).statusDetail).toBe("A plugin in Claude refused AgentK's prompt: held by another plugin");
  });

  it('forgets the mod when Claude is relaunched, until the new one polls', async () => {
    env = await makeEnv();
    fakeFiles(env, { mod: '' });
    const s = await session();
    await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, 'the mod to poll');
    const reset = vi.spyOn(env.core.sessions.mod, 'reset');
    await env.core.tmux.kill(env.core.sessions.row(s.id).tmux_name);
    await env.core.sessions.reconcile();
    await waitFor(() => launches(env).length, (n) => n === 2, 10000, 'the relaunch');
    expect(reset).toHaveBeenCalledWith(s.id);
    await waitFor(() => env.core.sessions.mod.live(s.id), (l) => l, 10000, "the new Claude's mod to poll");
  });

  it('leaves the mod out for a Claude Code older than 2.1.286, or when the config turns it off', async () => {
    env = await makeEnv({ start: false });
    fakeFiles(env, { version: '2.1.285' });
    await env.core.start();
    await session('Old Claude');
    env.core.updateConfig((c) => {
      c.claude.mod = false;
    });
    fakeFiles(env, { version: '2.1.287' });
    await session('Mod off');
    for (const l of launches(env)) {
      expect(l.args).not.toContain('--plugin-dir');
      expect(l.env.CLAUDE_CODE_ENABLE_FUNCTION_HOOKS).toBeUndefined();
    }
    expect(launches(env)).toHaveLength(2);
  });
});
