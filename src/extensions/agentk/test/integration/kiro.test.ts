import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { makeEnv, readJsonl, waitFor, type TestEnv } from '../helpers.js';
import { sessionAgentId } from '../../src/server/meko.js';
import { all, get } from '../../src/server/db.js';
import { MEKO_MIN_SHOW_MS } from '../../src/server/mekocalls.js';
import { exportBuiltinKiroAgent } from '../../src/server/kiro.js';

const lines = (env: TestEnv, file: string) => {
  const f = join(process.env.FAKE_KIRO_DIR!, file);
  return readJsonl(f);
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

  it("lists Kiro's models once it's signed in, and starts a session on the one picked", async () => {
    // Signed out, the list would start Kiro's sign-in: AgentK doesn't ask for it.
    writeFileSync(join(process.env.FAKE_KIRO_DIR!, 'signed-out'), '');
    expect(await env.core.harnesses.models(env.core.config, 'kiro')).toEqual([]);
    rmSync(join(process.env.FAKE_KIRO_DIR!, 'signed-out'));
    // Kept for a while; Check again reads it again.
    expect(await env.core.harnesses.models(env.core.config, 'kiro')).toEqual([]);
    await env.core.harnesses.refresh(env.core.config, true);
    expect(await env.core.harnesses.models(env.core.config, 'kiro')).toEqual(['auto', 'claude-sonnet-4.5']);

    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'none' } });
    const s = await env.core.sessions.create(p.id, { name: 'Kiro on Sonnet', prompt: 'hi', start: 'code', harness: 'kiro', model: 'claude-sonnet-4.5' });
    expect(s.model).toBe('claude-sonnet-4.5');
    const [first] = await waitFor(() => lines(env, 'argv.jsonl'), (l) => l.length > 0, 10000, 'launch');
    expect(first.args.slice(0, 5)).toEqual(['chat', '--agent', 'agentk', '--model', 'claude-sonnet-4.5']);
  });

  it('starts Kiro with the terminal colors a browser last reported', async () => {
    env.core.setTermTheme('light');
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'none' } });
    const s = await env.core.sessions.create(p.id, { name: 'Kiro on light', prompt: 'hi', start: 'code', harness: 'kiro' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'the first turn to end');
    const [first] = lines(env, 'argv.jsonl');
    expect(first.env.KIRO_TERMINAL_THEME).toBe('light');
    expect(first.background).toBe('rgb:fafa/fbfb/fafa');
  });

  it("follows Kiro 3.0's sessions, and leaves the agent file as Kiro upgraded it", async () => {
    // Kiro 3.0 sends Claude Code's event names, and upgrades the agent's file in place.
    writeFileSync(join(process.env.FAKE_KIRO_DIR!, 'kiro3'), '');
    const file = join(process.env.KIRO_HOME!, 'agents', 'agentk.json');
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'none' } });
    const s = await env.core.sessions.create(p.id, { name: 'Kiro 3', prompt: 'hi', start: 'code', harness: 'kiro' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'the first turn to end');
    expect(env.core.sessions.row(s.id).agent_session_id).toBe(lines(env, 'spawn.jsonl')[0].sessionId);
    const upgraded = readFileSync(file, 'utf8');
    expect(JSON.parse(upgraded)).toHaveProperty('kiroUpgrade');
    // Another session's launch doesn't write AgentK's over it: the first would reload it and lose the agent.
    const t = await env.core.sessions.create(p.id, { name: 'Kiro 3 again', prompt: 'hi', start: 'code', harness: 'kiro' });
    await waitFor(() => env.core.sessions.get(t.id).status, (st) => st === 'idle', 10000, 'the second turn to end');
    expect(readFileSync(file, 'utf8')).toBe(upgraded);
  });

  it('trusts every tool on Kiro 3.0 by agentk-trusted, which Kiro leaves as written, and stops when trust_tools is off', async () => {
    // Kiro 3.0's engine ignores --trust-tools: a rule in the agent file allows every tool instead.
    writeFileSync(join(process.env.FAKE_KIRO_DIR!, 'kiro3'), '');
    writeFileSync(join(process.env.FAKE_KIRO_DIR!, 'version'), 'kiro-cli 3.0.0\n');
    await env.core.harnesses.refresh(env.core.config, true);
    env.core.updateConfig((c) => {
      c.kiro.trustTools = true;
    });
    const trusted = join(process.env.KIRO_HOME!, 'agents', 'agentk-trusted.json');
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'none' } });
    const s = await env.core.sessions.create(p.id, { name: 'Kiro 3 trusted', prompt: 'hi', start: 'code', harness: 'kiro' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'the first turn to end');
    expect(lines(env, 'argv.jsonl')[0].args.slice(0, 4)).toEqual(['chat', '--agent', 'agentk-trusted', '--trust-tools=*']);
    const agent = JSON.parse(readFileSync(trusted, 'utf8'));
    expect(agent.permissions.rules).toContainEqual({ capability: 'all', effect: 'allow' });
    expect(agent).not.toHaveProperty('kiroUpgrade');
    // AgentK's hooks ran from it: the session followed its turn.
    expect(env.core.sessions.row(s.id).agent_session_id).toBe(lines(env, 'spawn.jsonl')[0].sessionId);
    // agentk itself still grants nothing its base doesn't.
    expect(JSON.parse(readFileSync(join(process.env.KIRO_HOME!, 'agents', 'agentk.json'), 'utf8')).permissions?.rules ?? [])
      .not.toContainEqual({ capability: 'all', effect: 'allow' });

    env.core.updateConfig((c) => {
      c.kiro.trustTools = false;
    });
    const t = await env.core.sessions.create(p.id, { name: 'Kiro 3 asks', prompt: 'hi', start: 'code', harness: 'kiro' });
    await waitFor(() => env.core.sessions.get(t.id).status, (st) => st === 'idle', 10000, 'the second turn to end');
    expect(lines(env, 'argv.jsonl')[1].args.slice(0, 3)).toEqual(['chat', '--agent', 'agentk']);
    expect(lines(env, 'argv.jsonl')[1].args).not.toContain('--trust-tools=*');
    expect(existsSync(trusted)).toBe(false);
  });

  it("runs a session as AgentK's Kiro agent: system lines at spawn, the Design guard, credits and Kiro's plan, exit and Resume", async () => {
    // Kiro's export of its built-in agent copies the servers in its MCP config, Meko's token with them;
    // Meko then replaces that token (#467).
    mkdirSync(join(process.env.KIRO_HOME!, 'settings'), { recursive: true });
    const mcp = (token: string) => writeFileSync(join(process.env.KIRO_HOME!, 'settings', 'mcp.json'),
      JSON.stringify({ mcpServers: { meko: { url: 'http://127.0.0.1:1/mcp', headers: { Authorization: `Bearer ${token}` } } } }));
    mcp('old-token');
    expect(await exportBuiltinKiroAgent(env.core.config.kiro.command, env.core.paths)).toBe(true);
    mcp('new-token');

    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Kiro task', prompt: 'write src.ts now', start: 'design', harness: 'kiro' });
    expect(s.harness).toBe('kiro');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle' || st === 'needs_input', 10000, 'first turn to end');

    const agent = JSON.parse(readFileSync(join(process.env.KIRO_HOME!, 'agents', 'agentk.json'), 'utf8'));
    // Built from Kiro's built-in default (exported with `agent create --from`), its upgrade bookkeeping left out, with AgentK's session tools.
    expect(agent).toMatchObject({
      name: 'agentk', tools: ['*'], includeMcpJson: true,
      resources: ['file://AGENTS.md', `skill://${join(env.core.paths.run, 'tools', 'skills', 'agentk-sessions', 'SKILL.md')}`],
      mcpServers: { agentk_sessions: { command: process.execPath, args: expect.arrayContaining([expect.stringMatching(/mcpmain\.ts$/)]) } },
      // It grants nothing the base doesn't: Kiro asks before each session tool too.
      allowedTools: [],
    });
    expect(agent).not.toHaveProperty('kiroUpgrade');
    // Without the export's copy of Meko's server: Kiro loads it from its MCP config, with the token there now.
    expect(Object.keys(agent.mcpServers)).toEqual(['agentk_sessions']);

    const [first] = lines(env, 'argv.jsonl');
    expect(first.args).toEqual(['chat', '--agent', 'agentk', expect.stringContaining('write src.ts now')]);
    // Kiro has no mod, so no design_questions tool: its Design prompt names only the questions file.
    expect(first.args[3]).toContain(`write that JSON to ${env.core.sessions.questionsFile(env.core.sessions.row(s.id))}.`);
    expect(first.args[3]).not.toContain('design_questions');
    const row = env.core.sessions.row(s.id);
    expect(first.env.MEKO_AGENT_ID).toBe(sessionAgentId(row));
    // AgentK's terminal colors, dark before any browser reported them: told, and answered when Kiro asks the terminal.
    expect(first.env.KIRO_TERMINAL_THEME).toBe('dark');
    expect(first.background).toBe('rgb:1010/1414/1212');
    expect(row.meko_agent_id).toMatch(/^kiro:kiro-task-[0-9a-f]{8}$/);
    // Kiro has no system-prompt flag: the session's lines come from its context hook at spawn.
    expect(lines(env, 'spawn.jsonl')[0].context).toMatch(/^This Kiro session is managed by AgentK\. Project: Demo\. Task: Kiro task\./);
    // In Design, the guard denies Kiro's write into the worktree, by exit code with the reason.
    expect(lines(env, 'writes.jsonl')).toEqual([{ path: 'src.ts', denied: expect.stringMatching(/^This session is in the Design stage: .* Write the design to /) }]);
    expect(existsSync(join(row.worktree_path, 'src.ts'))).toBe(false);
    // Kiro's id came with its hooks; its credits and context fill from its session file.
    expect(row.agent_session_id).toBe(lines(env, 'spawn.jsonl')[0].sessionId);
    const usage = await waitFor(() => env.core.sessions.get(s.id).usage, (u) => u.credits === 0.5, 10000, 'credits');
    // Read once, its session file and its log: the next start's backfill leaves the session alone.
    expect(get(env.core.db, 'SELECT COUNT(*) AS n FROM usage_files WHERE session_id = ?', s.id)).toEqual({ n: 2 });
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

  it("runs a code review one step a turn, each after Kiro's /clear and its tier's /model, none refused as busy, and sends steps a restart lost (#467)", async () => {
    env.core.config.codeReview.models.kiro = { large: 'claude-opus-4.5', mid: '', small: 'claude-haiku-4.5' };
    writeFileSync(join(process.env.FAKE_KIRO_DIR!, 'review-reports.json'), JSON.stringify({
      dependents: { state_ledger: [], summary: 'LGTM', findings: [] },
      claims: { claims: [], summary: 'LGTM', findings: [] },
      diff: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 2, severity: 'blocker', comment: 'Off by one.', gotchas: [] }] },
    }));
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'none' } });
    const s = await env.core.sessions.create(p.id, { name: 'Kiro task', prompt: 'hi', start: 'code', harness: 'kiro' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'the first turn to end');
    const wt = env.core.sessions.row(s.id).worktree_path;
    mkdirSync(join(wt, 'src'), { recursive: true });
    writeFileSync(join(wt, 'src', 'a.ts'), ['export function f(n: number) {', '  return n + 1;', '}', ''].join('\n'));

    // The guide and diff passes don't report the first time: AgentK asks again for both, and the guide
    // pass then runs until the test lets it go.
    const fakeFile = (f: string) => join(process.env.FAKE_KIRO_DIR!, f);
    writeFileSync(fakeFile('review-skip-once'), JSON.stringify(['r1s1:guide', 'r1s1:diff']));
    writeFileSync(fakeFile('review-hold'), JSON.stringify(['r1s1:guide']));

    await env.core.sessions.action(s.id, 'review-run');
    // AgentK restarts while the asked-again guide pass runs: the diff pass, still to send, is lost with it.
    await waitFor(() => existsSync(fakeFile('review-held')), Boolean, 30000, 'the guide pass asked again');
    (env.core.reviews as unknown as { queued: Map<string, unknown> }).queued.clear();
    rmSync(fakeFile('review-hold'));
    const done = await waitFor(() => env.core.sessions.get(s.id).codeReview, (r) => r?.state === 'done' || r?.state === 'failed', 30000, 'the review to finish');
    // The diff pass is still sent, though AgentK had already asked again once: its finding is scored and kept.
    expect(done).toMatchObject({ state: 'done', counts: { blocker: 1, major: 0, minor: 0 } });
    // Each step in a turn of its own on its tier's model, and every report taken.
    const calls = lines(env, 'review-calls.jsonl') as { step: string; model: string; status: number }[];
    expect(calls.map((c) => [c.step, c.model, c.status])).toEqual([
      ['r1s1:dependents', 'claude-opus-4.5', 200], ['r1s1:history', 'claude-opus-4.5', 200], ['r1s1:claims', 'claude-opus-4.5', 200],
      ['r1s1:guide', 'claude-opus-4.5', 200], ['r1s1:diff', 'claude-haiku-4.5', 200], ['r1c1', 'claude-haiku-4.5', 200],
    ]);
    // The first step started the review session on its model; each after it cleared the conversation first.
    expect(lines(env, 'commands.jsonl').map((c) => (c as { command: string }).command)).toEqual([
      '/clear', '/model claude-haiku-4.5', '/clear', '/model claude-opus-4.5', '/clear', '/model claude-opus-4.5',
      '/clear', '/model claude-opus-4.5', '/clear', '/model claude-opus-4.5', '/clear', '/model claude-haiku-4.5',
      '/clear', '/model claude-haiku-4.5',
    ]);
  });

  it("shows Meko's m while Kiro calls Meko, and records its calls for the counts", async () => {
    // Kiro has no mod: its Meko calls are told by a hook before each call to a Meko server.
    env.core.config.claude.mod = false;
    const agent = JSON.parse(readFileSync(join(process.env.KIRO_HOME!, 'agents', 'agentk.json'), 'utf8'));
    expect(agent.hooks.preToolUse).toContainEqual({ matcher: '@meko', command: expect.stringMatching(/hook\.sh'$/), timeout_ms: 5000 });
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Kiro meko', prompt: 'meko 1500', start: 'code', harness: 'kiro' });
    const get = (id: string) => env.core.sessions.get(id);
    expect((await waitFor(() => get(s.id), (x) => x.mekoCall, 10000, 'the Meko call')).status).toBe('working');
    await waitFor(() => get(s.id), (x) => !x.mekoCall && x.status === 'idle', 10000, 'the call and the turn to end');

    // A search that found nothing, then one the user refused, whose m the turn's end takes away.
    await env.core.sessions.submit(s.id, 'meko 10 empty');
    await waitFor(() => all<{ n: number }>(env.core.db, 'SELECT COUNT(*) AS n FROM meko_calls WHERE session_id = ?', s.id)[0].n, (n) => n === 2, 10000, 'the second call');
    await waitFor(() => get(s.id).status, (st) => st === 'idle', 10000, 'the second turn');
    await env.core.sessions.submit(s.id, 'meko 1500 refused');
    await waitFor(() => get(s.id).mekoCall, Boolean, 10000, 'the refused call');
    await waitFor(() => get(s.id), (x) => !x.mekoCall && x.status === 'idle', 10000, 'the turn to end without the call');

    // The refused call is recorded as failed, as Claude's refused calls are.
    await waitFor(() => all<{ n: number }>(env.core.db, 'SELECT COUNT(*) AS n FROM meko_calls WHERE session_id = ?', s.id)[0].n, (n) => n === 3, 10000, 'the refused call');
    const calls = all(env.core.db, 'SELECT tool, kind, agent, outcome, results, model FROM meko_calls WHERE session_id = ? ORDER BY at, rowid', s.id);
    expect(calls).toEqual([
      { tool: 'memory_search', kind: 'lookup', agent: null, outcome: 'found', results: 1, model: null },
      { tool: 'memory_search', kind: 'lookup', agent: null, outcome: 'empty', results: 0, model: null },
      { tool: 'memory_search', kind: 'lookup', agent: null, outcome: 'error', results: null, model: null },
    ]);
    const usage = (await env.core.projects.home(p.id)).usage;
    expect(usage.bySession.find((u) => u.sessionId === s.id)?.meko).toEqual({ lookups: 3, found: 1, saves: 0 });
    expect(usage.meko).toMatchObject({ lookups: 3, found: 1, empty: 1 });

    // A call that ends at once, in a turn that goes on: the hold keeps its m, then lets it go.
    await env.core.sessions.submit(s.id, 'meko 0 then 4000');
    await waitFor(() => get(s.id).mekoCall, Boolean, 10000, 'the quick call');
    const seen = Date.now();
    const after = await waitFor(() => get(s.id), (x) => !x.mekoCall, 10000, 'the m to go');
    expect(Date.now() - seen).toBeGreaterThanOrEqual(MEKO_MIN_SHOW_MS - 500);
    expect(after.status).toBe('working');
  });

  it("refuses a new session on a harness that isn't available, reading its version again first", async () => {
    // Kiro read as too old, then updated: a create that names it reads the version again before refusing.
    const version = join(process.env.FAKE_KIRO_DIR!, 'version');
    writeFileSync(version, 'kiro-cli 2.0.0\n');
    await env.core.harnesses.refresh(env.core.config, true);
    expect(env.core.state().harnesses.find((h) => h.id === 'kiro')!.available).toBe(false);
    rmSync(version);
    expect((await env.core.harnesses.pick(env.core.config, 'kiro')).id).toBe('kiro');

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
    await expect(env.core.sessions.create(p.id, { name: 'none' })).rejects.toThrow(/^No coding agent is available: install Claude Code, OpenCode or Kiro CLI/);
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
    expect(r).toEqual({ reply: 'Nothing needs you.', actions: [], together: false, notes: [] });
    const input = JSON.parse(readFileSync(join(process.env.FAKE_KIRO_DIR!, 'ask-input.json'), 'utf8'));
    expect(input.args).toEqual(['chat', '--no-interactive', '--output-format', 'stream-json', '--agent', 'agentk-ask']);
    expect(input.input).toContain('what needs me?');
    // Kiro finds the agent in the folder the ask runs in; it has nothing to act with.
    const ask = JSON.parse(readFileSync(join(input.cwd, '.kiro', 'agents', 'agentk-ask.json'), 'utf8'));
    expect(ask).toMatchObject({ tools: [], mcpServers: {}, includeMcpJson: false, hooks: {} });
    // The run's session is deleted in the background, and stopping waits for that.
    await env.core.stop();
    expect(lines(env, 'deleted.jsonl')).toEqual([{ id: 'ask-1', cwd: input.cwd }]);
  });
});
