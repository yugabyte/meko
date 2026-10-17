import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';
import { sessionAgentId } from '../../src/server/meko.js';
import { get } from '../../src/server/db.js';

const lines = (env: TestEnv, file: string) => {
  const f = join(process.env.FAKE_KIRO_DIR!, file);
  return existsSync(f) ? readFileSync(f, 'utf8').trim().split('\n').map((l) => JSON.parse(l)) : [];
};

describe('sessions on Kiro (real session host + git, fake kiro-cli)', () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv({ kiro: true });
    // Kiro is offered once its version is read and its agent file written (in the background at start).
    await waitFor(() => env.core.state().harnesses.find((h) => h.id === 'kiro')!.available, Boolean, 10000, 'Kiro to be available');
    await waitFor(() => existsSync(join(process.env.KIRO_HOME!, 'agents', 'agentk.json')), Boolean, 10000, "Kiro's agentk agent");
  });
  afterEach(async () => {
    await env.cleanup();
  });

  it("runs a session as AgentK's Kiro agent: system lines at spawn, the Design guard, credits and Kiro's plan, exit and Resume", async () => {
    const agent = JSON.parse(readFileSync(join(process.env.KIRO_HOME!, 'agents', 'agentk.json'), 'utf8'));
    // Built from Kiro's built-in default (exported with `agent create --from`), its upgrade bookkeeping left out.
    expect(agent).toMatchObject({ name: 'agentk', tools: ['*'], includeMcpJson: true, resources: ['file://AGENTS.md'] });
    expect(agent).not.toHaveProperty('kiroUpgrade');

    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Kiro task', prompt: 'write src.ts now', start: 'design', harness: 'kiro' });
    expect(s.harness).toBe('kiro');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle' || st === 'needs_input', 10000, 'first turn to end');

    const [first] = lines(env, 'argv.jsonl');
    expect(first.args).toEqual(['chat', '--agent', 'agentk', expect.stringContaining('write src.ts now')]);
    const row = env.core.sessions.row(s.id);
    expect(first.env.MEKO_AGENT_ID).toBe(sessionAgentId(row));
    expect(row.meko_agent_id).toMatch(/^kiro:kiro-task-[0-9a-f]{8}$/);
    // Kiro has no system-prompt flag: the session's lines come from its context hook at spawn.
    expect(lines(env, 'spawn.jsonl')[0].context).toMatch(/^This Kiro session is managed by AgentK Local\. Project: Demo\. Task: Kiro task\./);
    // In Design, the guard denies Kiro's write into the worktree, by exit code with the reason.
    expect(lines(env, 'writes.jsonl')).toEqual([{ path: 'src.ts', denied: expect.stringMatching(/^This session is in the Design stage: .* Write the design to /) }]);
    expect(existsSync(join(row.worktree_path, 'src.ts'))).toBe(false);
    // Kiro's id came with its hooks; its credits and context fill from its session file.
    expect(row.agent_session_id).toBe(lines(env, 'spawn.jsonl')[0].sessionId);
    const usage = await waitFor(() => env.core.sessions.get(s.id).usage, (u) => u.credits === 0.5, 10000, 'credits');
    // Read once: the next start's backfill leaves the session alone.
    expect(get(env.core.db, 'SELECT COUNT(*) AS n FROM usage_files WHERE session_id = ?', s.id)).toEqual({ n: 1 });
    expect(usage).toEqual({ input: 0, output: 0, cacheRead: 0, cacheWrite: 0, messages: 1, credits: 0.5 });
    expect(env.core.sessions.get(s.id).contextPercent).toBe(13);
    // Credits aren't priced: the project's cost stays at zero, its credits are summed beside it.
    const home = await env.core.projects.home(p.id);
    expect(home.usage.cost).toEqual({ usd: 0, unpricedMessages: 0 });
    expect(home.usage.total.credits).toBe(0.5);
    // The first session on Kiro read Kiro's plan for the rail, and the session that run left was deleted.
    const plan = await waitFor(() => env.core.state().kiroPlan, Boolean, 10000, "Kiro's plan");
    expect(plan).toMatchObject({ plan: 'Free', used: 12.5, covered: 50, resetsOn: '2099-01-01' });
    const [read] = lines(env, 'usage.jsonl');
    await waitFor(() => lines(env, 'deleted.jsonl'), (d) => d.some((x: { cwd: string }) => x.cwd === read.cwd), 10000, "the plan read's session deleted");

    // Kiro fires no hook when it exits: reconcile finds the dead pane.
    await env.core.sessions.submit(s.id, 'exit');
    await waitFor(async () => {
      await env.core.sessions.reconcile();
      return env.core.sessions.get(s.id).status;
    }, (st) => st === 'exited', 10000, 'exit');
    expect(env.core.sessions.get(s.id).statusDetail).toMatch(/^Kiro exited/);

    // Resume continues Kiro's conversation by its id.
    await env.core.sessions.action(s.id, 'resume');
    await waitFor(() => lines(env, 'argv.jsonl').length, (n) => n === 2, 10000, 'resume');
    expect(lines(env, 'argv.jsonl')[1].args).toEqual(['chat', '--agent', 'agentk', '--resume-id', row.agent_session_id]);
  });

  it('refuses a new session on a harness that is turned off', async () => {
    env.core.updateConfig((c) => {
      c.kiro.enabled = false;
    });
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
    await expect(env.core.sessions.create(p.id, { name: 'x', harness: 'kiro' })).rejects.toThrow(/^Kiro isn't available for new sessions: turn it on in Settings/);
    // A default that isn't available any more never decides: a request that names no harness starts on Claude Code.
    env.core.updateConfig((c) => {
      c.agents.default = 'kiro';
    });
    expect((await env.core.harnesses.pick(env.core.config, undefined)).id).toBe('claude');
  });

  it('runs without Claude Code: sessions and Ask AgentK on Kiro, and nothing at all with neither', async () => {
    // Claude Code is the default but isn't installed: AgentK's hook files are there all the same.
    env.core.updateConfig((c) => {
      c.claude.command = join(env.home, 'no-such-claude');
    });
    expect(existsSync(join(env.core.paths.run, 'hook.sh'))).toBe(true);
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'none' } });
    const s = await env.core.sessions.create(p.id, { name: 'No Claude', prompt: 'hi', start: 'code' });
    expect(s.harness).toBe('kiro');
    expect(env.core.state().harnesses.find((h) => h.id === 'claude')).toMatchObject({ available: false, checked: true, version: null });
    expect(env.core.state().meko.claude.gaps).toEqual([]);
    writeFileSync(join(process.env.FAKE_KIRO_DIR!, 'ask-reply.jsonl'), JSON.stringify({ type: 'runFinished', data: { sessionId: 'ask-2', status: 'success', finalText: '{"reply":"ok","actions":[]}' } }));
    expect((await env.core.askAgentk.ask({ text: 'hello' })).reply).toBe('ok');

    // Neither agent: nothing starts, and the palette doesn't offer Ask AgentK.
    env.core.updateConfig((c) => {
      c.kiro.enabled = false;
    });
    expect(env.core.state().askAgentk).toBe(false);
    await expect(env.core.sessions.create(p.id, { name: 'none' })).rejects.toThrow(/^No coding agent is available: install Claude Code or Kiro CLI/);
    await expect(env.core.askAgentk.ask({ text: 'hello' })).rejects.toThrow(/^No coding agent is available/);
  });

  it("answers Ask AgentK on Kiro when it's the default, as a tool-less agent, and deletes the run's session", async () => {
    env.core.updateConfig((c) => {
      c.agents.default = 'kiro';
    });
    const answer = { reply: 'Nothing needs you.', actions: [] };
    writeFileSync(join(process.env.FAKE_KIRO_DIR!, 'ask-reply.jsonl'), [
      { type: 'runStarted', data: { engine: 'v2' } },
      { type: 'runFinished', data: { sessionId: 'ask-1', status: 'success', finalText: `\`\`\`json\n${JSON.stringify(answer)}\n\`\`\`` } },
    ].map((e) => JSON.stringify(e)).join('\n'));
    const r = await env.core.askAgentk.ask({ text: 'what needs me?' });
    expect(r).toEqual({ reply: 'Nothing needs you.', actions: [], notes: [] });
    const input = JSON.parse(readFileSync(join(process.env.FAKE_KIRO_DIR!, 'ask-input.json'), 'utf8'));
    expect(input.args).toEqual(['chat', '--no-interactive', '--output-format', 'stream-json', '--agent', 'agentk-ask']);
    expect(input.input).toContain('what needs me?');
    // Kiro finds the agent in the folder the ask runs in; it has nothing to act with.
    const ask = JSON.parse(readFileSync(join(input.cwd, '.kiro', 'agents', 'agentk-ask.json'), 'utf8'));
    expect(ask).toMatchObject({ tools: [], mcpServers: {}, includeMcpJson: false, hooks: {} });
    await waitFor(() => lines(env, 'deleted.jsonl'), (d) => d.length === 1, 10000, 'the run deleted');
    expect(lines(env, 'deleted.jsonl')[0].id).toBe('ask-1');
  });
});
