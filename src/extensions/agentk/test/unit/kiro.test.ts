import { existsSync, mkdirSync, readFileSync, rmSync, utimesSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import { agentkHooks, kiroArgs, kiroHarness, kiroMekoTool, kiroTrustByAgent, normalizeKiro, writeAgentkAgent } from '../../src/server/harness/kiro.js';
import {
  agentkAgent, builtinKiroAgent, findKiroSession, parseKiroMekoCalls, parseKiroModels, parseKiroPlan, parseKiroTurns, readKiroAgent, readKiroRun, removeKiroAgent,
  removeOwnKiroAgent, writeKiroAgent,
} from '../../src/server/kiro.js';
import { kiroMekoGaps, kiroMekoServerNames } from '../../src/server/mekokiro.js';
import { silentLogger as silentLog } from '../../src/server/log.js';
import { getPaths } from '../../src/server/paths.js';
import type { SessionRow } from '../../src/server/store.js';
import { tempDir } from '../helpers.js';

const row = (over: Partial<SessionRow>) => ({ kind: 'plan', untrusted: null, agent_session_id: null, created_at: 0, ...over }) as SessionRow;

/** Runs `fn` with KIRO_HOME on a fresh folder. */
async function withKiroHomeAsync(fn: (home: string) => Promise<void>): Promise<void> {
  const home = tempDir('akl-kiro-');
  const old = process.env.KIRO_HOME;
  process.env.KIRO_HOME = home;
  try {
    await fn(home);
  } finally {
    if (old === undefined) delete process.env.KIRO_HOME;
    else process.env.KIRO_HOME = old;
  }
}
const withKiroHome = (fn: (home: string) => void) => withKiroHomeAsync(async (home) => fn(home));

describe("Kiro's agentk agent", () => {
  it("keeps the base agent's tools, MCP servers and hooks (Meko's capture), adds AgentK's and its session tools after them, and drops Kiro's upgrade bookkeeping", () => {
    const meko = {
      name: 'meko-default', description: 'Meko', tools: ['@meko', 'read', 'write'], includeMcpJson: true, kiroUpgrade: { revision: 2 },
      hooks: { agentSpawn: [{ command: 'node kiro-handler.js agentSpawn' }], stop: [{ command: 'node kiro-handler.js stop' }] },
    };
    const ours = { agentSpawn: [{ command: '/run/hook.sh' }], preToolUse: [{ matcher: 'fs_write', command: '/run/kiro-guard.sh' }] };
    const a = agentkAgent(meko, ours);
    // [meko] capture_transcripts off: Meko's capture hooks are left out, as Claude's sessions run without Meko's plugin.
    expect(agentkAgent(meko, ours, { mekoCapture: false }).hooks).toEqual({ agentSpawn: [{ command: '/run/hook.sh' }], stop: [], preToolUse: ours.preToolUse });
    expect(a).toEqual({
      name: 'agentk', description: expect.stringMatching(/^Written by AgentK.* Based on meko-default\.$/), tools: ['@meko', 'read', 'write'], includeMcpJson: true,
      hooks: {
        agentSpawn: [{ command: 'node kiro-handler.js agentSpawn' }, { command: '/run/hook.sh' }],
        stop: [{ command: 'node kiro-handler.js stop' }],
        preToolUse: [{ matcher: 'fs_write', command: '/run/kiro-guard.sh' }],
      },
    });
    const tools = { name: 'agentk_sessions', server: { command: '/ak/node', args: ['/ak/mcpmain.js'] }, skill: '/ak/run/tools/skills/agentk-sessions/SKILL.md' };
    const base = { ...meko, mcpServers: { meko: { url: 'https://meko.test/mcp' } }, resources: ['file://AGENTS.md'], allowedTools: ['read'] };
    expect(agentkAgent(base, ours, { tools })).toMatchObject({
      mcpServers: { meko: { url: 'https://meko.test/mcp' }, agentk_sessions: tools.server },
      resources: ['file://AGENTS.md', 'skill:///ak/run/tools/skills/agentk-sessions/SKILL.md'],
      // Nothing more is allowed than the base allows: Kiro asks before each session tool too.
      allowedTools: ['read'],
    });
  });

  it('is written only when it changed, and never read back as the base agent', () => {
    const home = tempDir('akl-kiro-');
    const a = agentkAgent({ name: 'base', tools: ['*'] }, {});
    expect(writeKiroAgent(a, home)).toBe(true);
    expect(writeKiroAgent(a, home)).toBe(false);
    // A default agent named agentk (or a file with that name) is AgentK's own, not the user's.
    expect(readKiroAgent('agentk', home)).toBeNull();
    // So is one written while the product was AgentK Local.
    writeFileSync(join(home, 'agents', 'agentk-ask.json'), JSON.stringify({ name: 'agentk-ask', description: 'Written by AgentK Local, which rewrites it: edit your default agent instead.' }));
    expect(readKiroAgent('agentk-ask', home)).toBeNull();
    writeFileSync(join(home, 'agents', 'mine.json'), JSON.stringify({ name: 'my-agent', tools: ['read'] }));
    expect(readKiroAgent('my-agent', home)).toEqual({ name: 'my-agent', tools: ['read'] });
  });
});

describe("Kiro's agentk-trusted agent ([kiro] trust_tools on Kiro 3.0)", () => {
  const allowAll = { capability: 'all', effect: 'allow' };
  const hooks = { agentSpawn: [{ command: '/run/hook.sh' }] };

  it("is agentk with every tool allowed after the base's own rules, which still hold (Kiro settles deny and ask first)", () => {
    const base = { name: 'meko-default', tools: ['*'], permissions: { rules: [{ capability: 'shell', match: ['rm *'], effect: 'deny' }], other: 1 } };
    expect(agentkAgent(base, hooks, { trustAll: true })).toEqual({
      ...agentkAgent(base, hooks), name: 'agentk-trusted',
      permissions: { rules: [{ capability: 'shell', match: ['rm *'], effect: 'deny' }, allowAll], other: 1 },
    });
    expect(agentkAgent({ name: 'base' }, hooks, { trustAll: true }).permissions).toEqual({ rules: [allowAll] });
    // agentk itself grants nothing its base doesn't.
    expect(agentkAgent(base, hooks).permissions).toEqual(base.permissions);
    expect(agentkAgent({ name: 'base' }, hooks)).not.toHaveProperty('permissions');
  });

  it('is removed only while it is AgentK\'s, with its copy, and by uninstall with agentk', () => {
    const home = tempDir('akl-kiro-');
    const written = join(tempDir('akl-run-'), 'run', 'kiro-agentk-trusted.json');
    const file = join(home, 'agents', 'agentk-trusted.json');
    expect(removeOwnKiroAgent('agentk-trusted', home, written)).toBe(false);
    writeKiroAgent(agentkAgent({ name: 'base' }, hooks, { trustAll: true }), home, written);
    expect(removeOwnKiroAgent('agentk-trusted', home, written)).toBe(true);
    expect(existsSync(file) || existsSync(written)).toBe(false);
    // Someone's own agent by that name stays.
    writeFileSync(file, JSON.stringify({ name: 'agentk-trusted', description: 'Mine' }));
    expect(removeOwnKiroAgent('agentk-trusted', home)).toBe(false);
    expect(existsSync(file)).toBe(true);

    writeKiroAgent(agentkAgent({ name: 'base' }, hooks), home);
    writeKiroAgent(agentkAgent({ name: 'base' }, hooks, { trustAll: true }), home);
    removeKiroAgent(home);
    expect(existsSync(join(home, 'agents', 'agentk.json')) || existsSync(file)).toBe(false);
  });

  it('is used only with trust_tools on a Kiro read as 3.0 or later, and left as it is while no version is known', () => {
    const cfg = (trustTools: boolean): Config => ({ ...DEFAULT_CONFIG, kiro: { ...DEFAULT_CONFIG.kiro, trustTools } });
    expect(kiroTrustByAgent(cfg(true), '3.0.0')).toBe(true);
    expect(kiroTrustByAgent(cfg(true), '3.1.2')).toBe(true);
    expect(kiroTrustByAgent(cfg(true), '2.34.0')).toBe(false);
    expect(kiroTrustByAgent(cfg(true), null)).toBeNull();
    expect(kiroTrustByAgent(cfg(false), '3.0.0')).toBe(false);
    expect(kiroTrustByAgent(cfg(false), null)).toBe(false);

    withKiroHome((home) => {
      const paths = getPaths(tempDir('akl-kiro-ak-'));
      const trusted = join(home, 'agents', 'agentk-trusted.json');
      expect(writeAgentkAgent(paths, cfg(true), true)).toEqual(["wrote Kiro's agentk agent", "wrote Kiro's agentk-trusted agent"]);
      expect(JSON.parse(readFileSync(trusted, 'utf8')).permissions.rules).toEqual([allowAll]);
      expect(JSON.parse(readFileSync(join(home, 'agents', 'agentk.json'), 'utf8'))).not.toHaveProperty('permissions');
      expect(writeAgentkAgent(paths, cfg(true), true)).toEqual([]);
      // No version known: neither written nor removed.
      expect(writeAgentkAgent(paths, cfg(true), null)).toEqual([]);
      expect(existsSync(trusted)).toBe(true);
      // Trust turned off: removed, agentk left as it was.
      expect(writeAgentkAgent(paths, cfg(false))).toEqual(["removed Kiro's agentk-trusted agent"]);
      expect(existsSync(trusted)).toBe(false);
      expect(existsSync(join(home, 'agents', 'agentk.json'))).toBe(true);
      expect(writeAgentkAgent(paths, cfg(false))).toEqual([]);
    });
  });

  it('runs a launch as the Kiro last read, even after a read failed or with Kiro off for new sessions, and goes once trust_tools is off', async () => {
    await withKiroHomeAsync(async (home) => {
      const paths = getPaths(tempDir('akl-kiro-ak-'));
      const trusted = join(home, 'agents', 'agentk-trusted.json');
      let version: string | null = '3.0.0';
      const reads: string[] = [];
      const kiro = kiroHarness({ log: silentLog, changed: () => {}, readVersion: async (cmd) => (reads.push(cmd), version) });
      const cfg = (trustTools: boolean, enabled = true): Config => ({ ...DEFAULT_CONFIG, kiro: { ...DEFAULT_CONFIG.kiro, trustTools, enabled } });
      const launch = async (c: Config) => {
        await kiro.beforeLaunch({ paths, cfg: c, log: silentLog, row: row({ id: 's1', worktree_path: home }), repo: null } as never);
        return kiro.launcher(paths, c, { row: row({ id: 's1', worktree_path: home }), resume: false, prompt: null, model: null, untrusted: false, env: {}, termTheme: 'dark', unsetEnv: [] } as never).args.slice(0, 3);
      };
      expect(await launch(cfg(true))).toEqual(['chat', '--agent', 'agentk-trusted']);
      expect(existsSync(trusted)).toBe(true);
      // Kiro turned off for new sessions: its sessions still resume as the Kiro last read, never knocked off the agent.
      version = null;
      expect(await launch(cfg(true, false))).toEqual(['chat', '--agent', 'agentk-trusted']);
      expect(existsSync(trusted)).toBe(true);
      // A version read that fails keeps the last one.
      await kiro.version(cfg(true), true);
      expect(await launch(cfg(true))).toEqual(['chat', '--agent', 'agentk-trusted']);
      // Settings runs prepare when trust_tools turns off: gone at once, Kiro on or off.
      await kiro.prepare(paths, cfg(false, false));
      expect(existsSync(trusted)).toBe(false);
      expect(await launch(cfg(false))).toEqual(['chat', '--agent', 'agentk']);
      expect(reads.length).toBeGreaterThan(0);
    });
  });
});

describe("Kiro's built-in agent, as AgentK exported it", () => {
  it("leaves out the copy of Kiro's MCP config the export took, so Kiro reads Meko's current token (#467)", () => {
    const paths = getPaths(tempDir('akl-kiro-'));
    mkdirSync(paths.run, { recursive: true });
    const exported = { name: 'agentk-base', tools: ['*'], includeMcpJson: true, mcpServers: { meko: { url: 'http://127.0.0.1:1/mcp', headers: { Authorization: 'Bearer test-token' } } } };
    writeFileSync(join(paths.run, 'kiro-default.json'), JSON.stringify(exported));
    expect(builtinKiroAgent(paths)).toEqual({ ...exported, mcpServers: {} });
    // An agent that doesn't load the MCP config keeps its own servers: they're all it has.
    const own = { ...exported, includeMcpJson: false };
    writeFileSync(join(paths.run, 'kiro-default.json'), JSON.stringify(own));
    expect(builtinKiroAgent(paths)).toEqual(own);
  });
});

describe("Kiro's agentk agent after Kiro 3.0 upgraded it", () => {
  const hooks = { agentSpawn: [{ command: "'/ak/run/hook.sh'", timeout_ms: 5000 }], preToolUse: [{ matcher: 'fs_write', command: "'/ak/run/kiro-guard.sh'", timeout_ms: 5000 }] };
  const servers = { agentk_sessions: { command: '/ak/node', args: ['/ak/mcpmain.js'] } };
  const a = { ...agentkAgent({ name: 'base', tools: ['*'] }, hooks), mcpServers: servers };
  const t: Record<string, unknown> = { ...agentkAgent({ name: 'base', tools: ['*'] }, hooks, { trustAll: true }), mcpServers: servers };
  const allowAll = { capability: 'all', effect: 'allow' };
  // As `kiro-cli agent upgrade` (Kiro 3.0) rewrites a file: a list of named hooks, fs_write widened to write.
  const upgrade = (agent: Record<string, unknown>, h = hooks) => ({
    ...agent, tools: ['*'],
    hooks: Object.entries(h).flatMap(([trigger, list]) => list.map((x: { matcher?: string; command: string }, i: number) => ({
      name: `${trigger}-${i}`, trigger, ...(x.matcher ? { matcher: x.matcher === 'fs_write' ? 'write' : x.matcher } : {}),
      action: { type: 'command', command: x.command }, timeout: 5,
    }))),
    permissions: { rules: [{ capability: 'fs_read', match: ['./**'], effect: 'allow' }] },
    kiroUpgrade: { revision: 2, fingerprint: 'sha256:1', source: 'sha256:2' },
  });
  it.each([
    // Writing AgentK's over it again would knock running sessions off the agent.
    ["Kiro's upgrade of AgentK's last write is kept", upgrade(a), false],
    ['another AgentK home\'s agent (its own script paths) is written over',
      upgrade(a, { ...hooks, agentSpawn: [{ command: "'/dev-home/run/hook.sh'", timeout_ms: 5000 }] }), true],
    ["an older release's agent (other hooks) is written over", upgrade(a, { agentSpawn: hooks.agentSpawn, preToolUse: [] }), true],
    ['an agent with other MCP servers is written over', upgrade({ ...a, mcpServers: {} }), true],
    ["one that isn't AgentK's is written over", upgrade({ ...a, description: 'Mine' }), true],
    ['a missing file is written', null, true],
    ['agentk that allows every tool is written over', { ...upgrade(a), permissions: { rules: [allowAll] } }, true],
    // Kiro 3.0.0 doesn't upgrade a file with `permissions`; a later one that did and dropped the rule is written over.
    ["agentk-trusted upgraded without its rule is written over", upgrade(t), true, t],
    ["agentk-trusted upgraded with its rule kept is left alone", { ...upgrade(t), permissions: t.permissions }, false, t],
  ])('%s', (_, onDisk, writes, agent: Record<string, unknown> = a) => {
    const home = tempDir('akl-kiro-');
    const written = join(tempDir('akl-run-'), 'run', 'kiro-agentk.json');
    const file = join(home, 'agents', `${String(agent.name)}.json`);
    expect(writeKiroAgent(agent, home, written)).toBe(true);
    if (onDisk) writeFileSync(file, JSON.stringify(onDisk));
    else rmSync(file);
    expect(writeKiroAgent(agent, home, written)).toBe(writes);
    expect(JSON.parse(readFileSync(file, 'utf8'))).toEqual(writes ? agent : onDisk);
  });

  it("is written over when AgentK's own agent changed", () => {
    const home = tempDir('akl-kiro-');
    const written = join(tempDir('akl-run-'), 'run', 'kiro-agentk.json');
    expect(writeKiroAgent(a, home, written)).toBe(true);
    writeFileSync(join(home, 'agents', 'agentk.json'), JSON.stringify(upgrade(a)));
    const b = { ...a, resources: ['skill:///ak/SKILL.md'] };
    expect(writeKiroAgent(b, home, written)).toBe(true);
    expect(JSON.parse(readFileSync(join(home, 'agents', 'agentk.json'), 'utf8'))).toEqual(b);
  });
});

describe("a Kiro session's command line", () => {
  it.each([
    ['a new session with its first prompt, never after "--" (Kiro drops it there)', { conversation: null, trustTools: false, prompt: 'do it' }, ['do it']],
    ['a first prompt that starts with a dash, kept from being read as an option', { conversation: null, trustTools: false, prompt: '---\nfront' }, [' ---\nfront']],
    ["a resume of the session's own conversation, by id", { conversation: 'k-1', trustTools: false, prompt: null }, ['--resume-id', 'k-1']],
    // Never --resume (the newest in the folder can be an earlier session's) nor --trust-all-tools (Kiro asks on screen, "No, exit" first).
    ['tools trusted for this run only ([kiro] trust_tools)', { conversation: 'k-1', trustTools: true, prompt: 'continue' }, ['--trust-tools=*', '--resume-id', 'k-1', 'continue']],
    ["the session's model, at a resume too", { conversation: 'k-1', trustTools: false, prompt: null, model: 'claude-sonnet-4.5' }, ['--model', 'claude-sonnet-4.5', '--resume-id', 'k-1']],
  ])('%s', (_, o, rest) => {
    expect(kiroArgs(o)).toEqual(['chat', '--agent', 'agentk', ...rest]);
  });

});

describe("Kiro's hook events as AgentK's", () => {
  it.each([
    [{ hook_event_name: 'agentSpawn', cwd: '/w', session_id: 'k-1' }, { hook_event_name: 'SessionStart', cwd: '/w', agent_session_id: 'k-1' }],
    [{ hook_event_name: 'userPromptSubmit', session_id: 'k-1', prompt: 'hi' }, { hook_event_name: 'UserPromptSubmit', agent_session_id: 'k-1', prompt: 'hi' }],
    [{ hook_event_name: 'postToolUse', tool_name: 'write', tool_input: { path: 'a.ts' } }, { hook_event_name: 'PostToolUse', tool_name: 'write', tool_input: { path: 'a.ts' } }],
    [{ hook_event_name: 'stop', session_id: 'k-1', assistant_response: 'Done.' }, { hook_event_name: 'Stop', agent_session_id: 'k-1', assistant_response: 'Done.' }],
    // Kiro 3.0 sends Claude Code's names for the same events.
    [{ hook_event_name: 'SessionStart', session_id: 'x' }, { hook_event_name: 'SessionStart', agent_session_id: 'x' }],
    [{ hook_event_name: 'UserPromptSubmit', session_id: 'x', prompt: 'hi' }, { hook_event_name: 'UserPromptSubmit', agent_session_id: 'x', prompt: 'hi' }],
    [{ hook_event_name: 'PreToolUse', tool_name: '@meko/memory_search' }, { hook_event_name: 'PreToolUse', tool_name: '@meko/memory_search' }],
    [{ hook_event_name: 'PostToolUse', tool_name: 'write' }, { hook_event_name: 'PostToolUse', tool_name: 'write' }],
    [{ hook_event_name: 'Stop', session_id: 'x' }, { hook_event_name: 'Stop', agent_session_id: 'x' }],
    [{ hook_event_name: 'SessionEnd', session_id: 'k-1' }, null],
  ])('%j', (raw, event) => {
    expect(normalizeKiro(raw)).toEqual(event);
  });
});

describe("Kiro's session store", () => {
  it("reads each finished turn's credits, tokens and context fill", () => {
    const text = JSON.stringify({
      session_state: {
        conversation_metadata: {
          user_turn_metadatas: [
            { end_timestamp: '2026-10-17T04:40:03Z', model: 'auto', input_token_count: 0, metering_usage: [{ value: 0.1, unit: 'credit' }, { value: 0.05, unit: 'credit' }], final_context_usage_percentage: 2.2 },
            { end_timestamp: '2026-10-17T04:41:00Z', model: 'claude-sonnet-4.5', input_token_count: 120, output_token_count: 30, metering_usage: [] },
            { model: 'auto' },
          ],
        },
      },
    });
    expect(parseKiroTurns(text)).toEqual([
      { key: '2026-10-17T04:40:03Z', at: Date.parse('2026-10-17T04:40:03Z'), model: 'auto', input: 0, output: 0, cacheRead: 0, cacheWrite: 0, credits: expect.closeTo(0.15), contextPercent: 2.2 },
      { key: '2026-10-17T04:41:00Z', at: Date.parse('2026-10-17T04:41:00Z'), model: 'claude-sonnet-4.5', input: 120, output: 30, cacheRead: 0, cacheWrite: 0, credits: 0, contextPercent: null },
    ]);
    expect(parseKiroTurns('not json')).toEqual([]);
  });

  it('finds the newest session started in a folder since a time, by any of its spellings, by when Kiro says it started', () => {
    const home = tempDir('akl-kiro-');
    const dir = join(home, 'sessions', 'cli');
    mkdirSync(dir, { recursive: true });
    const put = (id: string, cwd: string, secs: number) => {
      writeFileSync(join(dir, `${id}.json`), JSON.stringify({ session_id: id, cwd }));
      utimesSync(join(dir, `${id}.json`), secs, secs);
    };
    put('old', '/net/host/w', 1000);
    put('new', '/w', 2000);
    put('other', '/elsewhere', 3000);
    expect(findKiroSession(['/w', '/net/host/w'], 0, home)).toBe('new');
    expect(findKiroSession(['/net/host/w'], 0, home)).toBe('old');
    expect(findKiroSession(['/w'], 2_500_000, home)).toBeNull();
    // Kiro's own start time wins over the file's: an earlier session's file written to later doesn't count as new.
    writeFileSync(join(dir, 'earlier.json'), JSON.stringify({ session_id: 'earlier', cwd: '/w', created_at: '1970-01-01T00:16:40Z' }));
    utimesSync(join(dir, 'earlier.json'), 9000, 9000);
    expect(findKiroSession(['/w'], 1_500_000, home)).toBe('new');
  });
});

describe("a Kiro session's Meko calls", () => {
  it("are told by the forwarder before each call to a Meko server, one matcher per server", () => {
    const hooks = agentkHooks(getPaths(tempDir('akl-kiro-hooks-')), ['meko', 'meko-local']);
    expect(hooks.preToolUse).toEqual([
      { matcher: 'fs_write', command: expect.stringMatching(/kiro-guard\.sh'$/), timeout_ms: 5000 },
      { matcher: '@meko', command: expect.stringMatching(/hook\.sh'$/), timeout_ms: 5000 },
      { matcher: '@meko-local', command: expect.stringMatching(/hook\.sh'$/), timeout_ms: 5000 },
    ]);
  });

  it("finds Meko's servers in Kiro's MCP config and the agent's own, leaving out disabled ones and others; meko without any", () => {
    const home = tempDir('akl-kiro-mcp-');
    expect(kiroMekoServerNames(undefined, home)).toEqual(['meko']);
    mkdirSync(join(home, 'settings'), { recursive: true });
    writeFileSync(join(home, 'settings', 'mcp.json'), JSON.stringify({ mcpServers: { meko: { url: 'u' }, 'meko-old': { url: 'u', disabled: true }, github: {} } }));
    expect(kiroMekoServerNames({ 'meko-local': { command: 'x' }, other: {} }, home)).toEqual(['meko', 'meko-local']);
  });

  it.each([
    [{ tool_name: '@meko/context_search' }, '@meko/context_search'],
    [{ tool_name: '@meko-local/memory_add' }, '@meko-local/memory_add'],
    [{ tool_name: '@github/search' }, null],
    [{ tool_name: 'write' }, null],
    [{}, null],
  ])("are keyed by their tool for the dot's m: %j", (p, key) => {
    expect(kiroMekoTool(p)).toBe(key);
  });

  it("are read from Kiro's session log with their replies, dated by their turn's prompt", () => {
    const line = (kind: string, data: object) => JSON.stringify({ version: 'v1', kind, data });
    const reply = (text: string, isError = false) => ({ content: [{ type: 'text', text }], isError });
    const results = (id: string, server: string, tool: string, r: object, status = 'success') => line('ToolResults', {
      message_id: 'm',
      content: [{ kind: 'toolResult', data: { toolUseId: id, content: [{ kind: 'json', data: r }], status } }],
      results: { [id]: { tool: { kind: { Mcp: { toolName: tool, serverName: server, params: { query: 'q', __tool_use_purpose: 'why' } } } }, result: { Success: { items: [{ Json: r }] } } } },
    });
    const text = [
      line('Prompt', { message_id: 'p1', content: [], meta: { timestamp: 1_700_000_000 } }),
      line('AssistantMessage', { message_id: 'a1', content: [{ kind: 'toolUse', data: { toolUseId: 't1', name: 'memory_search', input: { query: 'q' } } }] }),
      results('t1', 'meko', 'memory_search', reply('{"results":[]}')),
      results('t2', 'github', 'search', reply('[]')),
      results('t3', 'meko-local', 'memory_add', reply('Error: down', true), 'error'),
      // A call the user refused: Kiro's own message, as text, and an error.
      line('ToolResults', {
        message_id: 'm',
        content: [{ kind: 'toolResult', data: { toolUseId: 't5', content: [{ kind: 'text', data: 'Permission request failed: denied' }], status: 'error' } }],
        results: { t5: { tool: { kind: { Mcp: { toolName: 'context_search', serverName: 'meko', params: { query: 'q' } } } } } },
      }),
      'not json',
    ].join('\n');
    expect(parseKiroMekoCalls(text, 42)).toEqual([
      { id: 't1', tool: 'memory_search', at: 1_700_000_000_000, input: { query: 'q' }, text: '{"results":[]}', isError: false },
      { id: 't3', tool: 'memory_add', at: 1_700_000_000_000, input: { query: 'q' }, text: 'Error: down', isError: true },
      { id: 't5', tool: 'context_search', at: 1_700_000_000_000, input: { query: 'q' }, text: 'Permission request failed: denied', isError: true },
    ]);
    // A read that starts after the prompt dates its calls by when it ran.
    expect(parseKiroMekoCalls(results('t4', 'meko', 'context_search', reply('{}')), 42)).toEqual([
      { id: 't4', tool: 'context_search', at: 42, input: { query: 'q' }, text: '{}', isError: false },
    ]);
  });
});

describe("Kiro's one-shot answer", () => {
  it('is the final text of a run that succeeded, and the error of one that failed', () => {
    const ok = [{ type: 'runStarted', data: {} }, { type: 'runFinished', data: { sessionId: 'a-1', status: 'success', finalText: '{"reply":"hi"}' } }];
    expect(readKiroRun(ok.map((e) => JSON.stringify(e)).join('\n'))).toEqual({ text: '{"reply":"hi"}', ok: true, sessionId: 'a-1', error: null });
    const failed = [{ type: 'runFinished', data: { sessionId: 'a-2', status: 'error', finalText: '' } }];
    expect(readKiroRun(JSON.stringify(failed[0]))).toMatchObject({ ok: false, error: 'the run ended with error' });
    expect(readKiroRun('')).toMatchObject({ ok: false, text: '', sessionId: null });
  });
});

describe("Kiro's plan usage (/usage)", () => {
  const NOW = Date.parse('2026-10-17T00:00:00Z');
  const FREE = 'Estimated Usage | resets on 2026-11-01 | KIRO FREE\nCredits (36.02 of 50 covered in plan), 72.0%\nManage your plan at https://app.kiro.dev/account/usage\n';
  const base = { plan: 'Free', used: 36.02, covered: 50, resetsOn: '2026-11-01', bonus: null, overage: null, updatedAt: NOW };
  // Only the free plan's text has been seen (kiro-cli 2.32.0); the paid plans' lines are guesses.
  it.each([
    ['the free plan, as kiro-cli 2.32 prints it', FREE, base],
    ['colors and thousands', '\x1b[1mEstimated Usage | resets on 2026-11-01 | KIRO PRO+\x1b[0m\nCredits (1,200.5 of 2,000 covered in plan), 60%', { ...base, plan: 'Pro+', used: 1200.5, covered: 2000 }],
    ['no reset or plan', 'Credits (3 of 50 covered in plan)', { ...base, plan: null, used: 3, resetsOn: null }],
    ['bonus credits used, with an expiry date', `${FREE}Bonus credits: 380.00/500 (expires 2026-11-30)`,
      { ...base, bonus: { used: 380, total: 500, expires: '2026-11-30', line: 'Bonus credits: 380.00/500 (expires 2026-11-30)' } }],
    ['bonus credits left, expiring in days', `${FREE}Bonus credits (120 of 500 remaining), expires in 3 days`,
      { ...base, bonus: { used: 380, total: 500, expires: '2026-10-20', line: 'Bonus credits (120 of 500 remaining), expires in 3 days' } }],
    ['overage billed', `${FREE}Overages: 14.20 credits ($0.57), enabled`, { ...base, overage: { credits: 14.2, enabled: true, line: 'Overages: 14.20 credits ($0.57), enabled' } }],
    ['overage turned off', `${FREE}Overage: Disabled`, { ...base, overage: { credits: null, enabled: false, line: 'Overage: Disabled' } }],
    ['no credits line', 'Estimated Usage | resets on 2026-11-01 | KIRO FREE', null],
  ])('reads %s', (_, text, want) => {
    expect(parseKiroPlan(text, NOW)).toEqual(want);
  });
});

describe("Kiro's Meko setup", () => {
  it("says so only when the agent Kiro sessions are built from doesn't load Kiro's MCP config (its MCP server is Meko's to report)", () => {
    expect(kiroMekoGaps(true)).toEqual([]);
    expect(kiroMekoGaps(false)).toEqual([expect.stringMatching(/^Kiro's default agent doesn't load your MCP servers/)]);
  });
});

describe("Kiro's model list", () => {
  it.each([
    ['its ModelListResult, in order', JSON.stringify({ models: [{ model_name: 'Auto', model_id: 'auto' }, { model_id: 'claude-sonnet-4.5' }, { model_id: 'auto' }], default_model: 'auto' }),
      ['auto', 'claude-sonnet-4.5']],
    ['plain ids', JSON.stringify(['auto', 'claude-haiku-4.5']), ['auto', 'claude-haiku-4.5']],
    ['anything else', 'Not logged in', []],
    ['a list of something else', JSON.stringify({ models: [1, null, {}] }), []],
  ])('%s', (_, out, ids) => {
    expect(parseKiroModels(out)).toEqual(ids);
  });
});
