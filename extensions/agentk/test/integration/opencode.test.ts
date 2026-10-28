import { existsSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { makeEnv, readJsonl, waitFor, type TestEnv } from '../helpers.js';
import { all } from '../../src/server/db.js';
import { sessionAgentId } from '../../src/server/meko.js';
import { openCodeMekoFile, openCodePluginPath, openCodeSessionDir } from '../../src/server/opencode.js';

const lines = (file: string) => {
  const f = join(process.env.FAKE_OPENCODE_DIR!, file);
  return readJsonl(f);
};

describe('sessions on OpenCode (real session host + git, fake opencode running AgentK’s plugin)', () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv({ opencode: true });
    await waitFor(() => env.core.state().harnesses.find((h) => h.id === 'opencode')!.available, Boolean, 10000, 'OpenCode to be available');
  });
  afterEach(async () => {
    await env.cleanup();
  });

  it("runs a session with AgentK's plugin: instructions, the Design guard, Needs you, priced usage, subagents, exit and Resume", async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'OpenCode task', prompt: 'write src.ts now', start: 'design', harness: 'opencode' });
    expect(s.harness).toBe('opencode');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle' || st === 'needs_input', 10000, 'first turn to end');

    const [first] = lines('argv.jsonl');
    // A server of its own, which reads the session's variables (the fake refuses to start without it).
    expect(first.args).toEqual(['--standalone', expect.stringMatching(/^--prompt=.*write src\.ts now/s)]);
    // No mod, so no design_questions tool: its Design prompt names only the questions file.
    expect(first.args[1]).toContain(`write that JSON to ${env.core.sessions.questionsFile(env.core.sessions.row(s.id))}.`);
    expect(first.args[1]).not.toContain('design_questions');
    // AgentK's plugin folder and the session's instructions come in OpenCode's inline config; the design folder is writable.
    expect(first.config).toMatchObject({ plugin: [openCodePluginPath(env.core.paths)], permission: { external_directory: { [join(env.core.paths.designs, s.id, '*')]: 'allow' } } });
    const row = env.core.sessions.row(s.id);
    expect(first.env).toMatchObject({ MEKO_AGENT_ID: sessionAgentId(row), AK_SESSION: s.id, AK_OPENCODE_SOURCE: 'startup', OPENCODE_DISABLE_PROJECT_CONFIG: null });
    expect(row.meko_agent_id).toMatch(/^opencode:opencode-task-[0-9a-f]{8}$/);
    const [sys] = lines('system.jsonl');
    expect(sys.instructions[0]).toMatch(/^This OpenCode session is managed by AgentK\. Project: Demo\. Task: OpenCode task\./);
    // AgentK's Meko as OpenCode's `meko` server, from an owner-only file: the key never goes in the environment.
    expect(first.env.OPENCODE_CONFIG).toBe(openCodeMekoFile(env.core.paths));
    expect(statSync(first.env.OPENCODE_CONFIG).mode & 0o777).toBe(0o600);
    expect(JSON.parse(readFileSync(first.env.OPENCODE_CONFIG, 'utf8')).mcp.meko).toMatchObject({ type: 'remote', headers: { Authorization: 'Bearer test-key' } });
    expect(env.core.state().meko.opencode).toEqual({ own: false });
    // In Design, the guard denies OpenCode's write into the worktree: the plugin throws its reason.
    expect(lines('writes.jsonl')).toEqual([{ path: 'src.ts', denied: expect.stringMatching(/^This session is in the Design stage: .* Write the design to /) }]);
    expect(existsSync(join(row.worktree_path, 'src.ts'))).toBe(false);
    // OpenCode's id came with the plugin's events; its tokens are priced like Claude Code's, under the agent's name.
    expect(row.agent_session_id).toBe(lines('spawn.jsonl')[0].sessionId);
    const usage = await waitFor(() => env.core.sessions.get(s.id).usage, (u) => u.messages === 1, 10000, 'usage');
    expect(usage).toMatchObject({ input: 100, output: 25, cacheRead: 1000, cacheWrite: 50, messages: 1 });
    const home = await env.core.projects.home(p.id);
    expect(home.usage.cost.usd).toBeGreaterThan(0);
    expect(home.usage.byModel).toEqual([expect.objectContaining({ harness: 'opencode', model: 'claude-sonnet-4-5' })]);

    // A permission ask shows as Needs you, and the answer sets it working again.
    await env.core.sessions.submit(s.id, 'run ls');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'needs_input' && x.statusDetail === 'Permission needed for bash', 10000, 'Needs you');
    await env.core.sessions.submit(s.id, 'allow');
    await waitFor(() => env.core.sessions.get(s.id).usage.messages, (m) => m === 2, 10000, 'the answered turn');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle again');

    // A subagent's idle doesn't end the turn; its response counts, as its own agent.
    await env.core.sessions.submit(s.id, 'run a subagent');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'needs_input', 10000, 'the subagent turn');
    await env.core.sessions.submit(s.id, 'allow');
    await waitFor(() => env.core.sessions.get(s.id).usage.messages, (m) => m === 4, 10000, "the subagent's usage");
    expect(all<{ agent: string | null }>(env.core.db, 'SELECT agent FROM usage_messages WHERE session_id = ? ORDER BY at', s.id).map((r) => r.agent))
      .toContain(`${row.agent_session_id}_child`);

    // A turn the user interrupts ends as one, as Claude Code's mod reports it.
    await env.core.sessions.submit(s.id, 'interrupt this');
    await waitFor(() => all<{ text: string }>(env.core.db, "SELECT text FROM session_events WHERE session_id = ? AND kind = 'turn'", s.id).map((e) => e.text),
      (t) => t.includes('You interrupted the turn'), 10000, 'the interrupted turn');

    // OpenCode fires nothing when it exits: reconcile finds the dead pane.
    await env.core.sessions.submit(s.id, 'exit');
    await waitFor(async () => {
      await env.core.sessions.reconcile();
      return env.core.sessions.get(s.id).status;
    }, (st) => st === 'exited', 10000, 'exit');
    expect(env.core.sessions.get(s.id).statusDetail).toMatch(/^OpenCode exited/);

    // Resume continues OpenCode's session by its id.
    await env.core.sessions.action(s.id, 'resume');
    await waitFor(() => lines('argv.jsonl').length, (n) => n === 2, 10000, 'resume');
    const resumed = lines('argv.jsonl')[1];
    expect(resumed.args).toEqual(['--standalone', `--session=${row.agent_session_id}`]);
    expect(resumed.env.AK_OPENCODE_SOURCE).toBe('resume');

    // Turning Meko off removes the file with its key at once, not at OpenCode's next launch.
    env.core.updateConfig((c) => {
      c.meko.apiKey = '';
    });
    expect(existsSync(openCodeMekoFile(env.core.paths))).toBe(false);
    // A closed project's sessions never run again: their OpenCode folders go with it.
    await env.core.projects.delete(p.id);
    expect(existsSync(openCodeSessionDir(env.core.paths, s.id))).toBe(false);
  });

  it("shows Meko's m while OpenCode calls Meko, and records its calls, code mode's too, for the counts, What Meko doesn't know and the call log", async () => {
    env.core.config.claude.mod = false;
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
    env.core.projects.update(p.id, { settings: { mekoLog: true } });
    const s = await env.core.sessions.create(p.id, { name: 'OpenCode meko', prompt: 'meko 1500', start: 'code', harness: 'opencode' });
    const get = (id: string) => env.core.sessions.get(id);
    expect((await waitFor(() => get(s.id), (x) => x.mekoCall, 10000, 'the Meko call')).status).toBe('working');
    await waitFor(() => get(s.id), (x) => !x.mekoCall && x.status === 'idle', 10000, 'the call and the turn to end');
    // Two calls from code mode, inside one execute call and sharing its id: one found something, one nothing.
    await env.core.sessions.submit(s.id, 'meko code');
    const count = () => all<{ n: number }>(env.core.db, 'SELECT COUNT(*) AS n FROM meko_calls WHERE session_id = ?', s.id)[0].n;
    await waitFor(count, (n) => n === 3, 10000, 'the code-mode calls');
    await waitFor(() => get(s.id).status, (st) => st === 'idle', 10000, 'the second turn');

    const calls = all(env.core.db, 'SELECT tool_use_id, tool, outcome, results, quality, query, model FROM meko_calls WHERE session_id = ? ORDER BY at, query', s.id);
    expect(calls).toEqual([
      { tool_use_id: expect.stringMatching(/^opencode:call_m\d+:meko_memory_search#1$/), tool: 'memory_search', outcome: 'found', results: 1, quality: null, query: 'meko 1500', model: null },
      { tool_use_id: expect.stringMatching(/^opencode:call_x\d+:meko_memory_search#1$/), tool: 'memory_search', outcome: 'found', results: 1, quality: null, query: 'first', model: null },
      { tool_use_id: expect.stringMatching(/^opencode:call_x\d+:meko_memory_search#2$/), tool: 'memory_search', outcome: 'empty', results: 0, quality: 'empty', query: 'second', model: null },
    ]);
    const usage = (await env.core.projects.home(p.id)).usage;
    expect(usage.bySession.find((u) => u.sessionId === s.id)?.meko).toEqual({ lookups: 3, found: 2, saves: 0 });
    expect(env.core.projects.mekoGaps(p.id, 'open', undefined, undefined).gaps.map((g) => g.query)).toContain('second');
    // The call log has them as the session's agent's, with the time each took.
    const page = await env.core.sessions.mekoCalls(s.id, { by: 'claude' });
    expect(page.calls).toHaveLength(3);
    expect(page.calls.every((c) => c.tool === 'memory_search' && c.ok && typeof c.ms === 'number')).toBe(true);
    expect(page.calls.some((c) => (c.ms ?? 0) >= 1000)).toBe(true);
    expect(env.core.sessions.mekoLogs(p.id)).toEqual([expect.objectContaining({ sessionId: s.id, harness: 'opencode' })]);
  });

  it("leaves the user's own Meko server in OpenCode's config alone", async () => {
    const dir = join(process.env.HOME!, '.config', 'opencode');
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, 'opencode.jsonc'), '{\n  // mine\n  "mcp": { "meko": { "type": "remote", "url": "https://meko.example/mcp" }, },\n}\n');
    try {
      const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'none' } });
      const s = await env.core.sessions.create(p.id, { name: 'Own Meko', prompt: 'hi', start: 'code', harness: 'opencode' });
      await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn to end');
      expect(lines('argv.jsonl')[0].env.OPENCODE_CONFIG).toBeNull();
      expect(existsSync(openCodeMekoFile(env.core.paths))).toBe(false);
      await waitFor(() => env.core.state().meko.opencode, (m) => m?.own === true, 15000, "OpenCode's own Meko");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("answers Ask AgentK on OpenCode as a tool-less agent, without the user's plugins and MCP servers, and deletes the run's session even when it fails", async () => {
    env.core.updateConfig((c) => {
      c.agents.default = 'opencode';
    });
    // The user's own config: their model is kept for the ask, their plugins and MCP servers aren't.
    const own = join(process.env.HOME!, '.config', 'opencode');
    mkdirSync(own, { recursive: true });
    writeFileSync(join(own, 'opencode.json'), JSON.stringify({ model: 'anthropic/claude-sonnet-4-5', plugin: ['my-plugin'], mcp: { mine: { type: 'local', command: ['x'] } } }));
    const reply = (events: object[]) => writeFileSync(join(process.env.FAKE_OPENCODE_DIR!, 'ask-reply.jsonl'), events.map((e) => JSON.stringify(e)).join('\n'));
    try {
      const answer = { reply: 'Nothing needs you.', actions: [] };
      reply([
        { type: 'step_start', sessionID: 'ses_ask1', part: {} },
        { type: 'text', sessionID: 'ses_ask1', part: { text: `\`\`\`json\n${JSON.stringify(answer)}\n\`\`\`` } },
      ]);
      const r = await env.core.askAgentk.ask({ text: 'what needs me?' });
      expect(r).toEqual({ reply: 'Nothing needs you.', actions: [], together: false, notes: [] });
      const input = JSON.parse(readFileSync(join(process.env.FAKE_OPENCODE_DIR!, 'ask-input.json'), 'utf8'));
      expect(input.args).toEqual(['run', '--standalone', '--format', 'json', '--agent', 'agentk-ask']);
      expect(input.input).toContain('what needs me?');
      // OpenCode reads the ask's own global config in place of the user's, and no project config.
      expect(input.env).toMatchObject({ OPENCODE_DISABLE_PROJECT_CONFIG: '1', OPENCODE_DISABLE_CLAUDE_CODE: '1' });
      expect(input.config).toEqual({
        model: 'anthropic/claude-sonnet-4-5', agent: { 'agentk-ask': expect.objectContaining({ mode: 'primary', permission: 'deny' }) },
      });

      // A failed run (no provider signed in) is deleted too.
      reply([{ type: 'error', sessionID: 'ses_ask2', error: { type: 'provider.auth', message: 'No API key for anthropic', status: 401 } }]);
      await expect(env.core.askAgentk.ask({ text: 'again' })).rejects.toThrow(/^OpenCode can't reach a model provider/);
      await env.core.stop();
      expect(lines('deleted.jsonl')).toEqual([
        { id: 'ses_ask1', cwd: input.cwd, xdg: input.env.XDG_CONFIG_HOME },
        { id: 'ses_ask2', cwd: input.cwd, xdg: input.env.XDG_CONFIG_HOME },
      ]);
    } finally {
      rmSync(own, { recursive: true, force: true });
    }
  });
});
