import { mkdirSync, utimesSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { kiroArgs, normalizeKiro } from '../../src/server/harness/kiro.js';
import { agentkAgent, findKiroSession, parseKiroPlan, parseKiroTurns, readKiroAgent, readKiroRun, writeKiroAgent } from '../../src/server/kiro.js';
import { kiroMekoGaps } from '../../src/server/mekokiro.js';
import { tempDir } from '../helpers.js';

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
    const tools = { name: 'agentk_sessions', server: { command: '/ak/node', args: ['/ak/mcpmain.js'] }, skill: '/ak/run/tools/skills/agentk-sessions/SKILL.md', allowed: ['@agentk_sessions/list'] };
    const base = { ...meko, mcpServers: { meko: { url: 'https://meko.test/mcp' } }, resources: ['file://AGENTS.md'], allowedTools: ['read'] };
    expect(agentkAgent(base, ours, { tools })).toMatchObject({
      mcpServers: { meko: { url: 'https://meko.test/mcp' }, agentk_sessions: tools.server },
      resources: ['file://AGENTS.md', 'skill:///ak/run/tools/skills/agentk-sessions/SKILL.md'],
      allowedTools: ['read', '@agentk_sessions/list'],
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

describe("a Kiro session's command line", () => {
  it.each([
    ['a new session with its first prompt, never after "--" (Kiro drops it there)', { conversation: null, trustTools: false, prompt: 'do it' }, ['do it']],
    ['a first prompt that starts with a dash, kept from being read as an option', { conversation: null, trustTools: false, prompt: '---\nfront' }, [' ---\nfront']],
    ["a resume of the session's own conversation, by id", { conversation: 'k-1', trustTools: false, prompt: null }, ['--resume-id', 'k-1']],
    // Never --resume (the newest in the folder can be an earlier session's) nor --trust-all-tools (Kiro asks on screen, "No, exit" first).
    ['tools trusted for this run only ([kiro] trust_tools)', { conversation: 'k-1', trustTools: true, prompt: 'continue' }, ['--trust-tools=*', '--resume-id', 'k-1', 'continue']],
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
  const url = 'https://meko.example.com/mcp';
  it.each([
    [[], ['Kiro has no Meko MCP server.']],
    [[{ name: 'meko', url: 'https://other.example.com/mcp', token: true }], ["Kiro's Meko MCP server points at other.example.com, not meko.example.com."]],
    [[{ name: 'meko', url, token: false }], ["Kiro's Meko MCP server sends no API key."]],
    [[{ name: 'meko', url: `${url}/`, token: true }], []],
  ])('%j', (servers, gaps) => {
    expect(kiroMekoGaps(servers, url)).toEqual(gaps);
  });

  it("says so when the agent Kiro sessions are built from doesn't load Kiro's MCP config", () => {
    expect(kiroMekoGaps([{ name: 'meko', url, token: true }], url, false)).toEqual([expect.stringMatching(/^Kiro's default agent doesn't load your MCP servers/)]);
  });
});
