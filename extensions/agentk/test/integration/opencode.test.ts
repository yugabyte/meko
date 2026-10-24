import { existsSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';
import { issueSetupToken } from '../../src/server/auth.js';
import { all } from '../../src/server/db.js';
import { sessionAgentId } from '../../src/server/meko.js';
import { openCodeMekoFile, openCodePluginPath } from '../../src/server/opencode.js';
import { buildServer } from '../../src/server/server.js';

const lines = (file: string) => {
  const f = join(process.env.FAKE_OPENCODE_DIR!, file);
  return existsSync(f) ? readFileSync(f, 'utf8').trim().split('\n').map((l) => JSON.parse(l)) : [];
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
    expect(first.args).toEqual([expect.stringMatching(/^--prompt=.*write src\.ts now/s)]);
    // No mod, so no design_questions tool: its Design prompt names only the questions file.
    expect(first.args[0]).toContain(`write that JSON to ${env.core.sessions.questionsFile(env.core.sessions.row(s.id))}.`);
    expect(first.args[0]).not.toContain('design_questions');
    // AgentK's plugin and the session's instructions come in OpenCode's inline config; the design folder is writable.
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
    expect(resumed.args).toEqual([`--session=${row.agent_session_id}`]);
    expect(resumed.env.AK_OPENCODE_SOURCE).toBe('resume');
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

  it("answers Ask AgentK on OpenCode when it's the default, as a tool-less agent, and deletes the run's session", async () => {
    env.core.updateConfig((c) => {
      c.agents.default = 'opencode';
    });
    const answer = { reply: 'Nothing needs you.', actions: [] };
    writeFileSync(join(process.env.FAKE_OPENCODE_DIR!, 'ask-reply.jsonl'), [
      { type: 'step_start', sessionID: 'ses_ask1', part: {} },
      { type: 'text', sessionID: 'ses_ask1', part: { text: `\`\`\`json\n${JSON.stringify(answer)}\n\`\`\`` } },
    ].map((e) => JSON.stringify(e)).join('\n'));
    const r = await env.core.askAgentk.ask({ text: 'what needs me?' });
    expect(r).toEqual({ reply: 'Nothing needs you.', actions: [], notes: [] });
    const input = JSON.parse(readFileSync(join(process.env.FAKE_OPENCODE_DIR!, 'ask-input.json'), 'utf8'));
    expect(input.args).toEqual(['run', '--format', 'json', '--agent', 'agentk-ask']);
    expect(input.input).toContain('what needs me?');
    // OpenCode finds the agent in the ask folder's project config; it has nothing to act with.
    expect(input.config.agent['agentk-ask']).toMatchObject({ mode: 'primary', permission: 'deny' });
    await env.core.stop();
    expect(lines('deleted.jsonl')).toEqual([{ id: 'ses_ask1', cwd: input.cwd }]);
  });

  it('turns OpenCode on and off in Settings', async () => {
    const app = await buildServer(env.core, { tls: null, webDir: null, updater: null });
    try {
      const token = issueSetupToken(env.core.db);
      const res = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me', password: 'correct horse battery' } });
      const cookie = String(res.headers['set-cookie']).split(';')[0];
      const put = (opencodeEnabled: unknown) => app.inject({ method: 'PUT', url: '/api/settings', headers: { cookie }, payload: { opencodeEnabled } });
      expect((await app.inject({ url: '/api/settings', headers: { cookie } })).json().opencodeEnabled).toBe(true);
      expect((await put('no')).statusCode).toBe(400);
      expect((await put(false)).json().opencodeEnabled).toBe(false);
      expect(readFileSync(env.core.paths.config, 'utf8')).toMatch(/\[opencode\][^[]*enabled = false/);
      expect(env.core.state().harnesses.find((h) => h.id === 'opencode')).toMatchObject({ enabled: false, available: false });
    } finally {
      await app.close();
    }
  });
});
