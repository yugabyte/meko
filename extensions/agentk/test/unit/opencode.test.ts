import { appendFileSync, chmodSync, existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { DatabaseSync } from 'node:sqlite';
import { describe, expect, it } from 'vitest';
import { DEFAULT_CONFIG } from '../../src/server/config.js';
import { all, openDb, run } from '../../src/server/db.js';
import { normalizeOpenCode, openCodeArgs, openCodeHarness } from '../../src/server/harness/opencode.js';
import { silentLogger } from '../../src/server/log.js';
import {
  openCodeCanResume, openCodeCommand, openCodeHasOwnMeko, openCodePluginPath, openCodeStoredSession, readOpenCodeRun, writeOpenCodePlugin,
} from '../../src/server/opencode.js';
import { getPaths } from '../../src/server/paths.js';
import type { SessionRow } from '../../src/server/store.js';
import { ingestOpenCodeUsage } from '../../src/server/usage.js';
import { tempDir } from '../helpers.js';

describe("OpenCode's events as AgentK's", () => {
  it.each([
    ['the plugin loading', { hook_event_name: 'agentk.start', source: 'resume', session_id: 'ses_1' }, { hook_event_name: 'SessionStart', source: 'resume', agent_session_id: 'ses_1' }],
    ['a prompt', { hook_event_name: 'chat.message', session_id: 'ses_1' }, { hook_event_name: 'UserPromptSubmit', agent_session_id: 'ses_1' }],
    ['an edit the guard checks', { hook_event_name: 'tool.execute.before', tool: 'write', args: { path: 'a.ts' }, session_id: 'ses_1' },
      { hook_event_name: 'PreToolUse', tool_name: 'write', tool_input: { path: 'a.ts' }, agent_session_id: 'ses_1' }],
    ["a subagent's tool, with no session id", { hook_event_name: 'tool.execute.after', tool: 'read' }, { hook_event_name: 'PostToolUse', tool_name: 'read' }],
    ['a permission ask', { hook_event_name: 'permission.asked', permission: 'bash', session_id: 'ses_1' }, { hook_event_name: 'PermissionRequest', tool_name: 'bash', agent_session_id: 'ses_1' }],
    ['its answer', { hook_event_name: 'permission.replied' }, { hook_event_name: 'PostToolUse' }],
    ['the end of a turn', { hook_event_name: 'session.idle', session_id: 'ses_1' }, { hook_event_name: 'Stop', agent_session_id: 'ses_1' }],
    ['anything else', { hook_event_name: 'session.compacted' }, null],
  ])('%s', (_, raw, expected) => {
    expect(normalizeOpenCode(raw)).toEqual(expected);
  });
});

describe("an OpenCode session's command line", () => {
  it.each([
    ['a new session with its prompt', { conversation: null, prompt: 'hi' }, ['--standalone', '--prompt=hi']],
    ['a prompt that starts with "-", which stays the value', { conversation: null, prompt: '---\nfront' }, ['--standalone', '--prompt=---\nfront']],
    ['a resume by id, with no prompt', { conversation: 'ses_1', prompt: null }, ['--standalone', '--session=ses_1']],
  ])('%s', (_, o, expected) => {
    expect(openCodeArgs(o)).toEqual(expected);
  });

  it('leaves a PR head’s own config out, and gives AgentK’s plugin, instructions and design folder, and its session tools to a trusted session', () => {
    const paths = getPaths(tempDir('akl-oc-'));
    const h = openCodeHarness({ paths, log: silentLogger, changed: () => undefined });
    const row = { id: 's1', worktree_path: '/w', agent_session_id: null } as SessionRow;
    const spec = h.launcher(paths, structuredClone(DEFAULT_CONFIG), {
      row, resume: false, prompt: 'hi', systemPrompt: 'This OpenCode session is managed by AgentK.', addDirs: ['/designs/s1'], modDir: null, planMode: false,
      env: { AK_SESSION: 's1' }, unsetEnv: [], untrusted: true, tools: false,
    });
    expect(spec.env).toMatchObject({ AK_SESSION: 's1', AK_OPENCODE_SOURCE: 'startup', OPENCODE_DISABLE_PROJECT_CONFIG: '1' });
    // Meko is off here: no Meko server for it.
    expect(spec.env).not.toHaveProperty('OPENCODE_CONFIG');
    const cfg = JSON.parse(spec.env.OPENCODE_CONFIG_CONTENT);
    expect(cfg).toEqual({
      plugin: [openCodePluginPath(paths)], instructions: [expect.stringMatching(/sessions\/s1\/system\.md$/)],
      permission: { external_directory: { '/designs/s1/*': 'allow' } },
    });
    expect(readFileSync(cfg.instructions[0], 'utf8')).toBe('This OpenCode session is managed by AgentK.\n');
    // The session tools: their MCP server, their skill's folder, and an ask before each tool that changes something.
    const trusted = h.launcher(paths, structuredClone(DEFAULT_CONFIG), {
      row, resume: false, prompt: 'hi', systemPrompt: '', addDirs: ['/designs/s1'], modDir: null, planMode: false,
      env: {}, unsetEnv: [], untrusted: false, tools: true,
    });
    expect(JSON.parse(trusted.env.OPENCODE_CONFIG_CONTENT)).toMatchObject({
      mcp: { agentk_sessions: { type: 'local', command: expect.arrayContaining([process.execPath, expect.stringMatching(/mcpmain\.ts$/)]) } },
      skills: { paths: [join(paths.run, 'tools', 'skills')] },
      permission: {
        external_directory: { '/designs/s1/*': 'allow' },
        agentk_sessions_start_sessions: 'ask', agentk_sessions_move_session: 'ask', agentk_sessions_open_pr: 'ask', agentk_sessions_open_project: 'ask',
      },
    });
  });

  it("is found in ~/.opencode/bin when the default isn't on the PATH, and a command set in config.toml is kept", () => {
    const home = tempDir('akl-oc-home-');
    expect(openCodeCommand('opencode', home, '')).toBe('opencode');
    mkdirSync(join(home, '.opencode', 'bin'), { recursive: true });
    const own = join(home, '.opencode', 'bin', 'opencode');
    writeFileSync(own, '#!/bin/sh\n');
    chmodSync(own, 0o755);
    expect(openCodeCommand('opencode', home, '')).toBe(own);
    const onPath = tempDir('akl-oc-path-');
    writeFileSync(join(onPath, 'opencode'), '#!/bin/sh\n');
    chmodSync(join(onPath, 'opencode'), 0o755);
    expect(openCodeCommand('opencode', home, onPath)).toBe('opencode');
    expect(openCodeCommand('/opt/oc', home, '')).toBe('/opt/oc');
  });
});

describe('the files an OpenCode edit would change', () => {
  const h = openCodeHarness({ paths: getPaths('/nonexistent/agentk'), log: silentLogger, changed: () => undefined });
  const patch = '*** Begin Patch\n*** Add File: a.ts\n+x\n*** Update File: src/b.ts\n*** Move to: src/c.ts\n@@\n-y\n+z\n*** Delete File: d.ts\n*** End Patch';
  it.each([
    ['write', { path: '/w/a.ts', content: 'x' }, ['/w/a.ts']],
    ['edit', { path: 'b.ts', oldString: 'x' }, ['b.ts']],
    ['patch', { patchText: patch }, ['a.ts', 'src/b.ts', 'src/c.ts', 'd.ts']],
    ['read', { path: '/w/a.ts' }, null],
  ])('%s', (tool, input, expected) => {
    expect(h.editedPaths({ tool_name: tool, tool_input: input })).toEqual(expected);
  });
});

describe("an OpenCode session's usage", () => {
  it('reads the log from where it stopped, leaves a half-written line for later, and counts subagents as their own agent', () => {
    const db = openDb(':memory:');
    run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p', 'P', '/r', 'main', 0)`);
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name,
             hook_token, desired, status, stage, created_at) VALUES ('s1', 'p', 's1', 'S1', 'c', 'b', 'main', '/w', 't', 'tok', 'running', 'idle', 'code', 0)`);
    const file = join(tempDir('akl-oc-usage-'), 'messages.jsonl');
    const msg = (id: string, session: string) => JSON.stringify({ id, session, model: 'claude-sonnet-4-5', at: 1, input: 10, output: 3, reasoning: 2, cacheRead: 5, cacheWrite: 1 });
    writeFileSync(file, `${msg('m1', 'ses_main')}\n${msg('m2', 'ses_child')}\n${msg('m3', 'ses_main').slice(0, 20)}`);
    expect(ingestOpenCodeUsage(db, 's1', file, 'ses_main')).toBe(true);
    const rows = () => all<{ message_id: string; agent: string | null; output: number }>(db, 'SELECT message_id, agent, output FROM usage_messages ORDER BY message_id');
    expect(rows()).toEqual([{ message_id: 'opencode:m1', agent: null, output: 5 }, { message_id: 'opencode:m2', agent: 'ses_child', output: 5 }]);
    expect(ingestOpenCodeUsage(db, 's1', file, 'ses_main')).toBe(false);
    appendFileSync(file, `${msg('m3', 'ses_main').slice(20)}\n`);
    expect(ingestOpenCodeUsage(db, 's1', file, 'ses_main')).toBe(true);
    expect(rows().map((r) => r.message_id)).toEqual(['opencode:m1', 'opencode:m2', 'opencode:m3']);
  });
});

describe('OpenCode outside its sessions', () => {
  it("finds a meko server in the user's own config, comments and trailing commas included", () => {
    const dir = tempDir('akl-oc-cfg-');
    expect(openCodeHasOwnMeko(dir)).toBe(false);
    writeFileSync(join(dir, 'opencode.jsonc'), '{\n  // "mcp": { "meko": {} }\n  "mcp": { "other": { "type": "local", "command": ["x"] }, },\n}\n');
    expect(openCodeHasOwnMeko(dir)).toBe(false);
    writeFileSync(join(dir, 'opencode.json'), '{ "mcp": { "meko": { "type": "remote", "url": "https://m/x//y" /* mine */ } } }');
    expect(openCodeHasOwnMeko(dir)).toBe(true);
  });

  it("resumes only a session OpenCode's store still has, and goes by the usage log when the store can't be read", () => {
    const data = tempDir('akl-oc-data-');
    const log = join(tempDir('akl-oc-log-'), 'messages.jsonl');
    writeFileSync(log, `${JSON.stringify({ id: 'm1', session: 'ses_gone', model: 'm' })}\n`);
    // No store at all: the log decides.
    expect(openCodeStoredSession('ses_gone', data)).toBeNull();
    expect(openCodeCanResume(log, 'ses_gone', data)).toBe(true);
    // A store of a schema this doesn't know can't say either.
    const other = new DatabaseSync(join(data, 'opencode.db'));
    other.exec('CREATE TABLE elsewhere (id TEXT)');
    other.close();
    expect(openCodeStoredSession('ses_gone', data)).toBeNull();
    // A release channel's store has the session; the session the user deleted is in none.
    const store = new DatabaseSync(join(data, 'opencode-beta.db'));
    store.exec(`CREATE TABLE session_v2 (id TEXT PRIMARY KEY); INSERT INTO session_v2(id) VALUES ('ses_here')`);
    store.close();
    expect(openCodeStoredSession('ses_here', data)).toBe(true);
    expect(openCodeCanResume(log, 'ses_gone', data)).toBe(false);
  });

  it("reads an Ask AgentK run's answer, session and error", () => {
    const ev = (e: object) => JSON.stringify({ timestamp: 1, sessionID: 'ses_a', ...e });
    expect(readOpenCodeRun([ev({ type: 'step_start', part: {} }), ev({ type: 'text', part: { text: '{"reply":' } }), ev({ type: 'text', part: { text: '"ok"}' } }), 'noise'].join('\n')))
      .toEqual({ text: '{"reply":\n"ok"}', sessionId: 'ses_a', error: null });
    expect(readOpenCodeRun(ev({ type: 'error', error: { type: 'provider.auth', message: 'No API key', status: 401 } })))
      .toEqual({ text: '', sessionId: 'ses_a', error: 'No API key' });
  });

  it("writes AgentK's plugin once, never over a copy a session loaded, and the plugin does nothing without a session's variables", async () => {
    const paths = getPaths(tempDir('akl-oc-plugin-'));
    // The plugin file of AgentK before OpenCode 2.
    mkdirSync(join(paths.run, 'opencode'), { recursive: true });
    writeFileSync(join(paths.run, 'opencode', 'agentk-plugin.js'), '');
    writeOpenCodePlugin(paths);
    const file = join(openCodePluginPath(paths), 'server.js');
    const before = statSync(file);
    writeOpenCodePlugin(paths);
    // OpenCode reloads a plugin whose file changes, even to the same text.
    expect(statSync(file)).toMatchObject({ ino: before.ino, mtimeMs: before.mtimeMs });
    expect(existsSync(join(paths.run, 'opencode', 'agentk-plugin.js'))).toBe(false);
    const mod = (await import(pathToFileURL(file).href)) as { default: { id: string; setup: (ctx: object) => Promise<unknown> } };
    expect(mod.default.id).toBe('agentk');
    expect(await mod.default.setup({ location: { directory: '/w' } })).toBeUndefined();
  });
});
