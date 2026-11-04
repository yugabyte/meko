import { describe, expect, it } from 'vitest';
import { activityOf, claudeArgs, MEKO_KEY_ENV, runClaude, sessionMissing } from '../../src/server/claude.js';
import { ANSWER_SCHEMA } from '../../src/server/prompts.js';

const base = { schema: ANSWER_SCHEMA, systemPrompt: 'sys', mekoUrl: 'http://127.0.0.1:7778/mcp' };
const after = (args: string[], flag: string) => args[args.indexOf(flag) + 1];

describe('claude arguments', () => {
  it.each([
    ['a deck\'s first job starts its session', { id: 'S', started: false }, ['--session-id', 'S'], ['--resume', '--fork-session', '--no-session-persistence']],
    ['a later job resumes it', { id: 'S', started: true }, ['--resume', 'S'], ['--session-id', '--fork-session']],
    ['a parallel build forks it', { id: 'S', started: true, fork: true }, ['--resume', 'S', '--fork-session'], ['--session-id']],
    ['a job with no session keeps none', null, ['--no-session-persistence'], ['--resume', '--session-id']],
  ])('%s', (_n, session, has, hasnt) => {
    const args = claudeArgs({ ...base, session });
    expect(args.join(' ')).toContain(has.join(' '));
    for (const f of hasnt) expect(args).not.toContain(f);
  });

  it('gives Claude every tool, unattended, with Meko beside the user\'s servers and its key only by name', () => {
    const args = claudeArgs({ ...base, session: null });
    expect(after(args, '--permission-mode')).toBe('bypassPermissions');
    expect(args).not.toContain('--strict-mcp-config');
    expect(args).not.toContain('--tools');
    const mcp = JSON.parse(after(args, '--mcp-config')!) as { mcpServers: Record<string, { url: string; headers: Record<string, string> }> };
    expect(mcp.mcpServers.presenter_meko).toEqual({ type: 'http', url: base.mekoUrl, headers: { Authorization: `Bearer \${${MEKO_KEY_ENV}}` } });
    expect(JSON.parse(after(args, '--settings')!)).toEqual({ disableAllHooks: true });
    expect(JSON.parse(after(args, '--json-schema')!)).toEqual(ANSWER_SCHEMA);
    expect(claudeArgs({ ...base, mekoUrl: null, session: null })).not.toContain('--mcp-config');
  });

  it('turns tool calls into activity lines', () => {
    const e = { type: 'assistant', message: { content: [
      { type: 'tool_use', name: 'mcp__presenter_meko__context_search', input: { query: 'xcluster lag' } },
      { type: 'tool_use', name: 'WebSearch', input: { query: 'aurora global' } },
      { type: 'tool_use', name: 'Write', input: { file_path: 'assets/topology.svg' } },
      { type: 'text', text: 'thinking' },
    ] } };
    expect(activityOf(e)).toEqual(['Meko: context search "xcluster lag"', 'searching the web: aurora global', 'writing assets/topology.svg']);
    expect(activityOf({ type: 'result' })).toEqual([]);
  });

  it('runs nothing for a job cancelled before Claude started', async () => {
    const r = await runClaude(['/nonexistent/claude'], [], {
      cwd: '/', input: '', env: {}, timeoutMs: 1000, signal: AbortSignal.abort(), onEvent: () => undefined,
    });
    expect([r.cancelled, r.spawnError, r.code]).toEqual([true, null, null]);
  });

  it('tells a session that isn\'t on this computer', () => {
    expect(sessionMissing('No conversation found with session ID: abc')).toBe(true);
    expect(sessionMissing('rate limited')).toBe(false);
  });
});
