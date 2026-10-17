import { mkdirSync, utimesSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { kiroArgs, normalizeKiro } from '../../src/server/harness/kiro.js';
import { agentkAgent, findKiroSession, parseKiroTurns, readKiroAgent, readKiroRun, writeKiroAgent } from '../../src/server/kiro.js';
import { kiroMekoGaps } from '../../src/server/mekokiro.js';
import { tempDir } from '../helpers.js';

describe("Kiro's agentk agent", () => {
  it("keeps the base agent's tools, MCP servers and hooks (Meko's capture), adds AgentK's after them, and drops Kiro's upgrade bookkeeping", () => {
    const meko = {
      name: 'meko-default', description: 'Meko', tools: ['@meko', 'read', 'write'], includeMcpJson: true, kiroUpgrade: { revision: 2 },
      hooks: { agentSpawn: [{ command: 'node kiro-handler.js agentSpawn' }], stop: [{ command: 'node kiro-handler.js stop' }] },
    };
    const ours = { agentSpawn: [{ command: '/run/hook.sh' }], preToolUse: [{ matcher: 'fs_write', command: '/run/kiro-guard.sh' }] };
    const a = agentkAgent(meko, ours);
    // [kiro] trust_tools: every tool allowed in the agent, never --trust-all-tools (Kiro asks on screen, "No, exit" first).
    expect(agentkAgent(meko, ours, { trustTools: true }).allowedTools).toEqual(['*']);
    // [meko] capture_transcripts off: Meko's capture hooks are left out, as Claude's sessions run without Meko's plugin.
    expect(agentkAgent(meko, ours, { mekoCapture: false }).hooks).toEqual({ agentSpawn: [{ command: '/run/hook.sh' }], stop: [], preToolUse: ours.preToolUse });
    expect(a).toEqual({
      name: 'agentk', description: expect.stringMatching(/^Written by AgentK Local.* Based on meko-default\.$/), tools: ['@meko', 'read', 'write'], includeMcpJson: true,
      hooks: {
        agentSpawn: [{ command: 'node kiro-handler.js agentSpawn' }, { command: '/run/hook.sh' }],
        stop: [{ command: 'node kiro-handler.js stop' }],
        preToolUse: [{ matcher: 'fs_write', command: '/run/kiro-guard.sh' }],
      },
    });
  });

  it('is written only when it changed, and never read back as the base agent', () => {
    const home = tempDir('akl-kiro-');
    const a = agentkAgent({ name: 'base', tools: ['*'] }, {});
    expect(writeKiroAgent(a, home)).toBe(true);
    expect(writeKiroAgent(a, home)).toBe(false);
    // A default agent named agentk (or a file with that name) is AgentK's own, not the user's.
    expect(readKiroAgent('agentk', home)).toBeNull();
    writeFileSync(join(home, 'agents', 'mine.json'), JSON.stringify({ name: 'my-agent', tools: ['read'] }));
    expect(readKiroAgent('my-agent', home)).toEqual({ name: 'my-agent', tools: ['read'] });
  });
});

describe("a Kiro session's command line", () => {
  it.each([
    ['a new session with its first prompt, never after "--" (Kiro drops it there)', { resume: false, conversation: null, prompt: 'do it' }, ['do it']],
    ['a first prompt that starts with a dash, kept from being read as an option', { resume: false, conversation: null, prompt: '---\nfront' }, [' ---\nfront']],
    ['a resume of a conversation Kiro gave an id', { resume: true, conversation: 'k-1', prompt: null }, ['--resume-id', 'k-1']],
    ['a resume before Kiro said its id: the newest in the worktree', { resume: true, conversation: null, prompt: 'continue' }, ['--resume', 'continue']],
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
    [{ hook_event_name: 'SessionStart', session_id: 'k-1' }, null],
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

  it('finds the newest session started in a folder, by any of its spellings', () => {
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

describe("Kiro's Meko setup", () => {
  const url = 'https://meko.example.com/mcp';
  it.each([
    [[], ['Kiro has no Meko MCP server.']],
    [[{ name: 'meko', url: 'https://other.example.com/mcp', token: true }], ["Kiro's Meko MCP server points at other.example.com, not meko.example.com."]],
    [[{ name: 'meko', url, token: false }], ["Kiro's Meko MCP server sends no API key."]],
    [[{ name: 'meko', url: `${url}/`, token: true }], []],
  ])('%j', (servers, gaps) => {
    expect(kiroMekoGaps(servers, url)).toEqual(gaps);
  });
});
