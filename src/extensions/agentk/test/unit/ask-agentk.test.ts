import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { checkActions } from '../../src/server/actions.js';
import {
  AskAgentk, readAnswer, runClaudeOnce, type AskProject, type AskRun, type AskRunner,
} from '../../src/server/askagentk.js';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import type { Session, Stage } from '../../src/shared/types.js';
import { FAKE_CLAUDE, tempDir } from '../helpers.js';

const session = (id: string, name: string, stage: Stage, over: Partial<Session> = {}): Session =>
  ({ id, projectId: 'p1', name, stage, kind: 'task', status: 'idle', desired: 'running', branch: `ak/${id}`, prNumber: null, ...over }) as Session;

const projects: AskProject[] = [{
  id: 'p1', name: 'agent-k-local', repoPath: '/code/agent-k-local', github: 'yugabyte/agent-k-local',
  sessions: [
    session('s-design', 'Agentic search', 'design'),
    session('s-code', 'Dark mode', 'code', { status: 'needs_input', prNumber: 12 }),
    session('s-review', 'Review #7', 'reviewing-pr', { kind: 'review-pr' }),
    session('s-gone', 'Old work', 'code', { desired: 'archived' }),
  ],
}];

describe("Ask Kode's answers", () => {
  it.each([
    ['a task, its name cut to one line',
      { type: 'start', projectId: 'p1', stage: 'code', name: 'Fix\nthe login', prompt: 'Fix the login redirect' },
      { type: 'start', projectId: 'p1', start: 'code', name: 'Fix the login', prompt: 'Fix the login redirect' }, null],
    ['a task with no stage or name: Design, named from its prompt',
      { type: 'start', projectId: 'p1', prompt: 'Add dark mode to the Usage page' },
      { type: 'start', projectId: 'p1', start: 'design', name: 'Add dark mode to the Usage page', prompt: 'Add dark mode to the Usage page' }, null],
    ['a task in a project that isn\'t open', { type: 'start', projectId: 'nope', prompt: 'x' }, null, "Left out a task in a project that isn't open."],
    ['a task with no prompt', { type: 'start', projectId: 'p1', name: 'x' }, null, 'Left out a task with no prompt.'],
    ['a GitHub link', { type: 'link', ref: 'acme/demo#7' }, { type: 'link', ref: 'acme/demo#7' }, null],
    ['a GitHub issue to start in a project, at Code', { type: 'link', ref: 'acme/demo#8', projectId: 'p1', stage: 'code' },
      { type: 'link', ref: 'acme/demo#8', projectId: 'p1', start: 'code' }, null],
    ['a GitHub issue in a project that isn\'t open', { type: 'link', ref: 'acme/demo#8', projectId: 'gone' }, null, "Left out acme/demo#8 in a project that isn't open."],
    ['a project to open, with a new datapack by default', { type: 'openProject', repo: 'acme/demo', name: 'Demo\nwork' },
      { type: 'openProject', repo: 'acme/demo', name: 'Demo work', datapack: 'new' }, null],
    ['a project to open with no repo', { type: 'openProject', datapack: 'none' }, null, 'Left out opening a project with no repo.'],
    ['a link that isn\'t one', { type: 'link', ref: '#7' }, null, "Left out a link that isn't a GitHub PR or issue."],
    ['going to a session, its project from the session',
      { type: 'go', view: 'session', projectId: 'wrong', sessionId: 's-code' },
      { type: 'go', view: 'session', projectId: 'p1', sessionId: 's-code' }, null],
    ['going to an archived session', { type: 'go', view: 'session', sessionId: 's-gone' }, null, "Left out a session that isn't running."],
    ['going to a closed project, with a running session named too', { type: 'go', view: 'home', projectId: 'gone', sessionId: 's-code' }, null,
      "Left out a project that isn't open."],
    ['going somewhere Kode has no page for', { type: 'go', view: 'deploys', sessionId: 's-code' }, null, "Left out a place Kode can't open."],
    ['going to a project home', { type: 'go', view: 'home', projectId: 'p1' }, { type: 'go', view: 'home', projectId: 'p1' }, null],
    ['going to Usage', { type: 'go', view: 'usage' }, { type: 'go', view: 'usage' }, null],
    ['a move', { type: 'move', sessionId: 's-design', stage: 'code', instructions: ' add tests ' },
      { type: 'move', sessionId: 's-design', stage: 'code', instructions: 'add tests' }, null],
    ['a move to where it is', { type: 'move', sessionId: 's-design', stage: 'design' }, null,
      'Left out moving Agentic search: Nothing to change: this session is already in Design.'],
    ['a move of a Review a PR session', { type: 'move', sessionId: 's-review', stage: 'code' }, null,
      "Left out moving Review #7: A Review a PR session can't be moved to another stage."],
    ['an Open PR at Code', { type: 'openPr', sessionId: 's-code' }, { type: 'openPr', sessionId: 's-code', instructions: '' }, null],
    ['an Open PR at Design', { type: 'openPr', sessionId: 's-design' }, null, "Left out an Open PR for Agentic search: it isn't in Code."],
    ['an action Kode doesn\'t have', { type: 'deploy' }, null, "Left out an action Kode doesn't have."],
    ['a task on an agent and model (the session tools give them)', { type: 'start', projectId: 'p1', name: 'Fast', prompt: 'x', harness: 'kiro', model: ' auto ' },
      { type: 'start', projectId: 'p1', start: 'design', name: 'Fast', prompt: 'x', harness: 'kiro', model: 'auto' }, null],
    ['a GitHub issue on a model', { type: 'link', ref: 'acme/demo#9', model: 'opus' }, { type: 'link', ref: 'acme/demo#9', model: 'opus' }, null],
    ['a task on a model that can\'t be passed on', { type: 'start', projectId: 'p1', name: 'Fast', prompt: 'x', model: '-p' }, null,
      'Left out "Fast": -p isn\'t a model name: use letters, digits and . _ : / @ [ ] - only.'],
    ['a GitHub issue on an agent Kode doesn\'t run', { type: 'link', ref: 'acme/demo#9', harness: 'codex' }, null,
      "Left out acme/demo#9: codex isn't an agent Kode runs (claude, opencode, kiro)."],
  ])('%s', (_what, raw, action, note) => {
    expect(checkActions([raw], projects)).toEqual({ actions: action ? [action] : [], notes: note ? [note] : [] });
  });

  it('offers at most eight actions', () => {
    const go = { type: 'go', view: 'settings' };
    expect(checkActions(Array(9).fill(go), projects).actions).toHaveLength(8);
  });

  const run = (over: Partial<AskRun>): AskRun => ({ stdout: '', stderr: '', code: 0, spawnError: null, timedOut: false, aborted: false, ...over });
  it.each([
    ["Claude Code can't be run", run({ spawnError: 'spawn claude ENOENT', code: null }), 503, "Claude Code couldn't be run (claude)"],
    ['no answer in time', run({ timedOut: true, code: null }), 504, "Kode didn't answer in 45 seconds"],
    ['signed out', run({ code: 1, stdout: JSON.stringify({ is_error: true, result: 'Not logged in · Please run /login' }) }), 502, "Claude Code isn't signed in"],
    ['another failure', run({ code: 1, stderr: 'Unknown option --json-schema\nmore' }), 502, 'Claude Code failed: Unknown option --json-schema'],
    ['an answer not in the shape asked for', run({ stdout: JSON.stringify({ result: 'Sure!' }) }), 502, "Kode's answer couldn't be read"],
  ])('says why there is no answer: %s', (_what, r, status, message) => {
    expect(() => readAnswer(r, 'claude', 45)).toThrow(expect.objectContaining({ status, message: expect.stringContaining(message) }));
  });
});

describe('asking Kode', () => {
  let config: Config;
  let recorded: { ok: boolean; actions: number }[];
  let calls: { cmd: string; args: string[]; input: string }[];
  let answer: Partial<AskRun>;
  const runner: AskRunner = async (cmd, args, o) => {
    calls.push({ cmd, args, input: o.input });
    return { stdout: '', stderr: '', code: 0, spawnError: null, timedOut: false, aborted: false, ...answer };
  };
  const service = (run: AskRunner = runner) => new AskAgentk({
    config: () => config, cwd: join(tempDir('akl-ask-'), 'ask'), projects: () => projects, record: (r) => recorded.push({ ok: r.ok, actions: r.actions }), run,
  });

  beforeEach(() => {
    config = structuredClone(DEFAULT_CONFIG);
    recorded = [];
    calls = [];
    answer = { stdout: JSON.stringify({ structured_output: { reply: 'Starting it.', actions: [{ type: 'go', view: 'usage' }], together: true } }) };
  });

  it('asks Claude Code with no tools, MCP servers or hooks, about the running sessions only, and counts the ask', async () => {
    const r = await service().ask({ text: ' what needs me? ', history: [{ text: 'hi', reply: 'Hello.' }], projectId: 'p1', sessionId: null });
    // One action is no Do all, together or not.
    expect(r).toEqual({ reply: 'Starting it.', actions: [{ type: 'go', view: 'usage' }], together: false, notes: [] });
    const { args, input } = calls[0];
    expect(args.slice(0, 5)).toEqual(['-p', '--model', 'sonnet', '--output-format', 'json']);
    expect(args).toEqual(expect.arrayContaining(['--tools', '', '--strict-mcp-config', '--no-session-persistence', '--json-schema', '--system-prompt']));
    expect(args[args.indexOf('--settings') + 1]).toBe('{"disableAllHooks":true}');
    expect(input).toContain('"onScreen":{"projectId":"p1","sessionId":null}');
    // A bare #5 is the project's GitHub repo's.
    expect(input).toContain('"repo":"agent-k-local","github":"yugabyte/agent-k-local"');
    expect(input).toContain('"id":"s-code","name":"Dark mode","stage":"code","status":"needs_input","branch":"ak/s-code","pr":12');
    expect(input).not.toContain('s-gone');
    expect(input).toContain('User: hi\nKode: Hello.');
    expect(input.endsWith("The user's instruction:\nwhat needs me?")).toBe(true);
    expect(recorded).toEqual([{ ok: true, actions: 1 }]);

    // Steps of one instruction: Do all.
    answer = { stdout: JSON.stringify({ structured_output: {
      reply: 'Both.', together: true, actions: [{ type: 'link', ref: 'yugabyte/agent-k-local#5', projectId: 'p1' }, { type: 'move', sessionId: 's-design', stage: 'code' }],
    } }) };
    expect((await service().ask({ text: 'start #5 and move Agentic search to Code' })).together).toBe(true);
    // A go only navigates: one step and a go is no Do all.
    answer = { stdout: JSON.stringify({ structured_output: {
      reply: 'One.', together: true, actions: [{ type: 'link', ref: 'yugabyte/agent-k-local#5', projectId: 'p1' }, { type: 'go', view: 'usage' }],
    } }) };
    expect((await service().ask({ text: 'start #5 and show usage' })).together).toBe(false);
  });

  it.each([
    ['empty text', () => {}, { text: '  ' }, 400],
    ['too long a text', () => {}, { text: 'x'.repeat(2001) }, 400],
    ['Ask Kode turned off', () => (config.askKode.enabled = false), { text: 'hi' }, 403],
  ])('refuses %s without asking Claude', async (_what, set, body, status) => {
    set();
    await expect(service().ask(body)).rejects.toMatchObject({ status });
    expect(calls).toEqual([]);
  });

  it('answers two asks at a time, and counts a failed one but not a cancelled one', async () => {
    const waiting: (() => void)[] = [];
    const slow: AskRunner = (...a) => new Promise((res) => waiting.push(() => res(runner(...a))));
    const s = service(slow);
    const first = s.ask({ text: 'one' });
    const second = s.ask({ text: 'two' });
    await expect(s.ask({ text: 'three' })).rejects.toMatchObject({ status: 429 });
    answer = { aborted: true, code: null };
    waiting.shift()!();
    await expect(first).rejects.toMatchObject({ status: 499 });
    answer = { timedOut: true, code: null };
    waiting.shift()!();
    await expect(second).rejects.toMatchObject({ status: 504 });
    expect(recorded).toEqual([{ ok: false, actions: 0 }]);
  });
});

describe('running Claude Code once', () => {
  let fake: string;
  let cwd: string;
  const prev = process.env.FAKE_CLAUDE_DIR;
  beforeEach(() => {
    const home = tempDir('akl-ask-run-');
    fake = join(home, 'fake');
    cwd = join(home, 'ask');
    mkdirSync(fake);
    mkdirSync(cwd);
    process.env.FAKE_CLAUDE_DIR = fake;
  });
  afterEach(() => {
    process.env.FAKE_CLAUDE_DIR = prev;
  });

  it('passes the message on stdin and reads the answer from stdout', async () => {
    writeFileSync(join(fake, 'ask-reply.json'), '{"structured_output":{"reply":"ok","actions":[]}}');
    const r = await runClaudeOnce(FAKE_CLAUDE, ['-p', '--tools', ''], { input: 'hello\nthere', cwd, timeoutMs: 10_000 });
    expect(r).toMatchObject({ code: 0, spawnError: null, timedOut: false, aborted: false });
    expect(JSON.parse(r.stdout)).toEqual({ structured_output: { reply: 'ok', actions: [] } });
    expect(JSON.parse(readFileSync(join(fake, 'ask-input.json'), 'utf8'))).toMatchObject({ args: ['-p', '--tools', ''], input: 'hello\nthere' });
  });

  it.each([
    ['stops it after the timeout', 300, false, { timedOut: true, aborted: false }],
    ['stops it when the ask is cancelled', 10_000, true, { timedOut: false, aborted: true }],
  ])('%s', async (_what, timeoutMs, cancel, want) => {
    writeFileSync(join(fake, 'ask-hang'), '');
    const ac = new AbortController();
    const p = runClaudeOnce(FAKE_CLAUDE, ['-p'], { input: 'x', cwd, timeoutMs, signal: ac.signal });
    if (cancel) setTimeout(() => ac.abort(), 300);
    expect(await p).toMatchObject({ code: null, spawnError: null, ...want });
  });

  it("says when Claude Code can't be run", async () => {
    const r = await runClaudeOnce(join(fake, 'no-such-claude'), ['-p'], { input: 'x', cwd, timeoutMs: 10_000 });
    expect(r.spawnError).toContain('ENOENT');
  });
});
