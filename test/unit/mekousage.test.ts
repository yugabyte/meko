import { appendFileSync, mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { all, get, openDb, run, schemaVersion } from '../../src/server/db.js';
import { REPORTED_TOOLS, classifyResult, mekoCalls, mekoStarts, mekoTool, parseMekoCalls, parseMekoResults, toolCounts } from '../../src/server/mekousage.js';
import { searchSaving } from '../../src/server/mekosavings.js';
import { recordPreload } from '../../src/server/recall.js';
import { ingestSessionUsage, parseTurnMark, projectUsage, usageOverview } from '../../src/server/usage.js';

const usage = { input_tokens: 2, output_tokens: 100, cache_read_input_tokens: 200_000, cache_creation_input_tokens: 998 };

/** A model response calling tools, as Claude Code writes it. */
function call(msgId: string, tools: [id: string, name: string][], opts: { model?: string; at?: string } = {}): string {
  return JSON.stringify({
    type: 'assistant',
    timestamp: opts.at ?? new Date().toISOString(),
    message: {
      id: msgId,
      model: opts.model ?? 'claude-opus-5-5',
      usage,
      content: tools.map(([id, name]) => ({ type: 'tool_use', id, name, input: { query: 'q' } })),
    },
  }) + '\n';
}

/** A tool result line. Meko wraps its JSON in `{"result": "<json>"}`. */
function result(id: string, content: unknown, isError = false): string {
  return JSON.stringify({
    type: 'user',
    timestamp: new Date().toISOString(),
    message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: id, content, ...(isError ? { is_error: true } : {}) }] },
  }) + '\n';
}

const wrapped = (v: unknown) => JSON.stringify({ result: JSON.stringify(v) });
const found = wrapped({ results: [{ id: 'm1', memory: 'x'.repeat(4000) }] });

function reply(msgId: string, at?: string, stopReason: string | null = null): string {
  return JSON.stringify({
    type: 'assistant', timestamp: at ?? new Date().toISOString(), message: { id: msgId, model: 'claude-opus-5-5', stop_reason: stopReason, usage },
  }) + '\n';
}

function setup() {
  const dir = mkdtempSync(join(tmpdir(), 'akl-meko-'));
  const db = openDb(':memory:');
  const now = Date.now();
  run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p', 'P', '/r', 'main', ?)`, now);
  run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name,
           hook_token, desired, status, stage, created_at) VALUES ('s1', 'p', 's1', 'S1', 'c', 'b', 'main', '/w', 't', 'tok', 'running', 'idle', 'code', ?)`, now);
  return { dir, db };
}

describe('Meko calls in transcripts', () => {
  it('recognizes the Meko server under its own name or a plugin, and nothing else', () => {
    expect(mekoTool('mcp__meko__memory_search')).toBe('memory_search');
    expect(mekoTool('mcp__plugin_meko-agent-skills_meko__memory_add')).toBe('memory_add');
    expect(mekoTool('mcp__claude_ai_MekoDev__authenticate')).toBeNull();
    expect(mekoTool('mcp__phorge__phabricator_task_search')).toBeNull();
    expect(mekoTool('Read')).toBeNull();
  });

  it('reads calls from a model response, with its model and context', () => {
    const calls = parseMekoCalls(call('m1', [['t1', 'mcp__meko__memory_search'], ['t2', 'Read'], ['t3', 'mcp__meko__memory_add']]));
    expect(calls).toEqual([
      expect.objectContaining({ toolUseId: 't1', tool: 'memory_search', kind: 'lookup', model: 'claude-opus-5-5', contextTokens: 201_000 }),
      expect.objectContaining({ toolUseId: 't3', tool: 'memory_add', kind: 'save' }),
    ]);
    expect(parseMekoCalls('{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__meko__')).toEqual([]);
    expect(parseMekoCalls(result('t1', 'mcp__meko__memory_search'))).toEqual([]);
  });

  it('tells a lookup that found something from one that found nothing or failed', () => {
    expect(classifyResult('lookup', found, false)).toMatchObject({ outcome: 'found', results: 1 });
    expect(classifyResult('lookup', wrapped({ results: [], count: 0 }), false)).toMatchObject({ outcome: 'empty', results: 0 });
    const buckets = (memory: unknown[]) => wrapped({ results: { knowledge_base: [], memory, conversation: { hits: [], thread: [] } }, token_savings: { total_tokens: 10000 } });
    expect(classifyResult('lookup', buckets([]), false)).toMatchObject({ outcome: 'empty' });
    expect(classifyResult('lookup', buckets([{ memory: 'a' }, { memory: 'b' }]), false)).toMatchObject({ outcome: 'found', results: 2 });
    expect(classifyResult('lookup', wrapped({ memories: [], total: 0 }), false).outcome).toBe('empty');
    expect(classifyResult('lookup', wrapped({ id: 'm1', memory: 'one record' }), false).outcome).toBe('found');
    expect(classifyResult('lookup', "Error calling tool 'memory_search': [Errno 111] Connection refused", true).outcome).toBe('error');
    expect(classifyResult('lookup', wrapped({ error: 'agent_id_mismatch', detail: 'x' }), false).outcome).toBe('error');
    expect(classifyResult('save', wrapped({ message: 'Memory updated successfully!' }), false).outcome).toBe('ok');
    // Too large to show: Claude Code saved it to a file. Only its notice entered the context.
    const notice = 'Error: result (70,748 characters) exceeds maximum allowed tokens. Output has been saved to /x.txt';
    expect(classifyResult('lookup', notice, false)).toEqual({ outcome: 'found', results: null, resultChars: notice.length });
    expect(classifyResult('lookup', 'MCP tool "meko/memory_search" is still running after 120s. It was moved to the background', false).outcome).toBe('empty');
  });

  it('reads plain-text replies, empty error fields and unwrapped buckets right', () => {
    expect(classifyResult('lookup', JSON.stringify({ result: 'No memories found for this query.' }), false).outcome).toBe('empty');
    expect(classifyResult('lookup', 'No results found', false).outcome).toBe('empty');
    expect(classifyResult('lookup', JSON.stringify({ result: 'The deploy runs on Fridays.' }), false).outcome).toBe('found');
    expect(classifyResult('lookup', wrapped({ results: [{ id: 'm1' }], error: null }), false)).toMatchObject({ outcome: 'found', results: 1 });
    expect(classifyResult('lookup', wrapped({ results: [], error: '' }), false).outcome).toBe('empty');
    // context_search's buckets without the results wrapper.
    expect(classifyResult('lookup', wrapped({ memory: [], knowledge_base: [], conversation: { hits: [] } }), false).outcome).toBe('empty');
    expect(classifyResult('lookup', wrapped({ memory: [{ memory: 'a' }], knowledge_base: [], conversation: { hits: [] } }), false).outcome).toBe('found');
  });

  it('reads results only for calls waiting for one, as text or text blocks', () => {
    const pending = new Map([['t1', 'lookup' as const]]);
    expect(parseMekoResults(result('t1', [{ type: 'text', text: found }]), pending)).toEqual([
      { toolUseId: 't1', outcome: 'found', results: 1, resultChars: found.length },
    ]);
    expect(parseMekoResults(result('t9', found), pending)).toEqual([]);
    expect(parseMekoResults(result('t1', found), new Map())).toEqual([]);
  });

  it('records calls, their results across reads and subagents, once each', () => {
    const { dir, db } = setup();
    const main = join(dir, 'c.jsonl');
    writeFileSync(main, call('m1', [['t1', 'mcp__meko__memory_search']]));
    mkdirSync(join(dir, 'c', 'subagents'), { recursive: true });
    writeFileSync(join(dir, 'c', 'subagents', 'agent-x1.jsonl'), call('sub1', [['t2', 'mcp__meko__knowledgebase_search']]) + result('t2', wrapped({ results: [] })));
    expect(ingestSessionUsage(db, 's1', main)).toBe(true);
    const rows = () => all<{ tool_use_id: string; agent: string | null; outcome: string | null }>(db,
      'SELECT tool_use_id, agent, outcome FROM meko_calls ORDER BY tool_use_id');
    expect(rows()).toEqual([
      { tool_use_id: 't1', agent: null, outcome: null }, // still waiting
      { tool_use_id: 't2', agent: 'x1', outcome: 'empty' },
    ]);
    // The result lands in a later read.
    appendFileSync(main, result('t1', found) + reply('m2'));
    expect(ingestSessionUsage(db, 's1', main)).toBe(true);
    expect(rows()[0].outcome).toBe('found');
    // Reading everything again (a migration cleared the offsets) adds nothing.
    run(db, 'DELETE FROM usage_files');
    ingestSessionUsage(db, 's1', main);
    expect(rows()).toHaveLength(2);
  });

  it('splits 1-hour cache writes out, and counts them all as 1-hour without the split', () => {
    const { dir, db } = setup();
    const main = join(dir, 'c.jsonl');
    const split = { ...usage, cache_creation: { ephemeral_1h_input_tokens: 900, ephemeral_5m_input_tokens: 98 } };
    writeFileSync(main,
      JSON.stringify({ type: 'assistant', timestamp: new Date().toISOString(), message: { id: 'a', model: 'claude-opus-5-5', usage: split } }) + '\n' + reply('b'));
    ingestSessionUsage(db, 's1', main);
    expect(all(db, 'SELECT message_id, cache_write, cache_write_1h FROM usage_messages ORDER BY message_id')).toEqual([
      { message_id: 'a', cache_write: 998, cache_write_1h: 900 },
      { message_id: 'b', cache_write: 998, cache_write_1h: 998 },
    ]);
  });

  it('migrates a database from before Meko calls, reading every transcript again', () => {
    const dir = mkdtempSync(join(tmpdir(), 'akl-migrate-'));
    const path = join(dir, 'state.db');
    let db = openDb(path);
    const v = schemaVersion(db);
    run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p', 'P', '/r', 'main', 0)`);
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name,
             hook_token, desired, status, stage, created_at) VALUES ('s1', 'p', 's1', 'S1', 'c', 'b', 'main', '/w', 't', 'tok', 'running', 'idle', 'code', 0)`);
    run(db, `INSERT INTO usage_files(session_id, path, offset) VALUES ('s1', '/t.jsonl', 123)`);
    // Back to schema 16: undo 20 (sessions.meko_agent_id, project_events), 19 (first-turn Meko stats),
    // 18 (sessions.meko_datapack_*) and 17 (Meko calls), then upgrade again.
    db.exec('ALTER TABLE sessions DROP COLUMN meko_agent_id; DROP TABLE project_events');
    db.exec('DROP TABLE meko_preloads; ALTER TABLE sessions DROP COLUMN first_turn_end_at');
    db.exec('ALTER TABLE sessions DROP COLUMN meko_datapack_id; ALTER TABLE sessions DROP COLUMN meko_datapack_name');
    db.exec(`DROP TABLE meko_calls; DROP INDEX usage_messages_agent_at; ALTER TABLE usage_messages DROP COLUMN cache_write_1h; PRAGMA user_version = ${v - 4}`);
    db.close();
    db = openDb(path);
    expect(schemaVersion(db)).toBe(v);
    expect(all(db, 'SELECT * FROM usage_files')).toEqual([]);
    expect(all(db, 'SELECT * FROM meko_calls')).toEqual([]);
    expect(all(db, 'SELECT * FROM meko_preloads')).toEqual([]);
    expect(all(db, 'SELECT first_turn_end_at FROM sessions')).toEqual([{ first_turn_end_at: null }]);
    db.close();
  });

  it('counts the responses after a search through an index, not a scan of the session', () => {
    const { db } = setup();
    const plan = all<{ detail: string }>(db, `EXPLAIN QUERY PLAN SELECT COUNT(*) FROM usage_messages u
      WHERE u.session_id = ? AND u.agent IS ? AND u.at > ?`, 's1', null, 0).map((r) => r.detail).join('\n');
    expect(plan).toContain('usage_messages_agent_at');
  });
});

describe('what Meko saved', () => {
  it('credits a search with the exploring it replaced (the worked example)', () => {
    // Context 216k, a 1,420-token result, 10 or more responses after it, on Opus 5.5.
    const s = searchSaving({ contextTokens: 216_000, resultTokens: 1420, laterResponses: 211, model: 'claude-opus-5-5' });
    expect(s.tokens).toBe(432_000 + 480 + 4580 + 45_800);
    // Re-reads and carry at the cache-read price, output at the output price, new content at the 1-hour write price.
    expect(s.usd).toBeCloseTo((432_000 * 0.2 + 480 * 20 + 4580 * 8 + 45_800 * 0.2) / 1e6, 10);
  });

  it('counts fewer later responses near the end, lets a large result count for less, and has no price for an unknown model', () => {
    expect(searchSaving({ contextTokens: 0, resultTokens: 0, laterResponses: 3, model: null }))
      .toEqual({ tokens: 480 + 6000 * 4, usd: null });
    expect(searchSaving({ contextTokens: 0, resultTokens: 8000, laterResponses: 10, model: 'claude-opus-5-5' }).tokens).toBe(480 - 2000 * 11);
    expect(searchSaving({ contextTokens: 1000, resultTokens: 0, laterResponses: 0, model: 'claude-fake-1' }).usd).toBeNull();
  });

  it('sums only searches that found something, per session, project and overall', () => {
    const { dir, db } = setup();
    const main = join(dir, 'c.jsonl');
    const at = (s: number) => new Date(Date.UTC(2026, 8, 29, 12, 0, s)).toISOString();
    writeFileSync(main,
      call('m1', [['s-found', 'mcp__meko__memory_search'], ['s-empty', 'mcp__meko__context_search'], ['get', 'mcp__meko__memory_get_by_id'], ['add', 'mcp__meko__memory_add']], { at: at(0) }) +
      result('s-found', found) + result('s-empty', wrapped({ results: [] })) + result('get', wrapped({ id: 'm1', memory: 'x' })) +
      result('add', "Error calling tool 'memory_add': refused", true) +
      reply('m2', at(1)) + reply('m3', at(2)));
    ingestSessionUsage(db, 's1', main);
    const expected = searchSaving({ contextTokens: 201_000, resultTokens: Math.round(found.length / 4), laterResponses: 2, model: 'claude-opus-5-5' });

    const pu = projectUsage(db, 'p');
    expect(pu.meko).toMatchObject({ calls: 4, lookups: 3, found: 2, empty: 1, errors: 1, saves: 1 });
    expect(pu.meko.savings).toEqual({ searches: 1, seeds: 0, tokens: expected.tokens, usd: expected.usd, unpricedSearches: 0, unpricedSeeds: 0 });
    expect(pu.bySession[0].meko).toEqual({ lookups: 3, found: 2, saves: 1 });
    expect(pu.cost.usd).toBeGreaterThan(0);
    expect(pu.cost.unpricedMessages).toBe(0);

    const o = usageOverview(db);
    expect(o.meko.savings).toEqual(pu.meko.savings);
    expect(o.byProject[0]).toMatchObject({ meko: pu.meko, cost: pu.cost });
    expect(o.cost).toEqual(pu.cost);
  });

  it('keeps searches on unpriced models in the tokens, and says the dollars leave them out', () => {
    const { dir, db } = setup();
    const main = join(dir, 'c.jsonl');
    writeFileSync(main,
      call('m1', [['a', 'mcp__meko__memory_search']]) + result('a', found) +
      call('m2', [['b', 'mcp__meko__memory_search']], { model: 'claude-fake-1' }) + result('b', found) + reply('m3'));
    ingestSessionUsage(db, 's1', main);
    const s = projectUsage(db, 'p').meko.savings;
    expect(s).toMatchObject({ searches: 2, unpricedSearches: 1 });
    expect(s.usd).toBeGreaterThan(0);
  });

  it('shows nothing saved, and no Meko use, for a project that never called Meko', () => {
    const { db } = setup();
    expect(projectUsage(db, 'p').meko).toEqual({
      calls: 0, lookups: 0, found: 0, empty: 0, errors: 0, saves: 0, foundTokens: 0, savings: { searches: 0, seeds: 0, tokens: 0, usd: 0, unpricedSearches: 0, unpricedSeeds: 0 },
    });
  });

  it('credits a seed AgentK passed at start like a search by the first response, once there is one', () => {
    const { dir, db } = setup();
    recordPreload(db, 's1', { outcome: 'found', results: 2, chars: 4000, ms: 900 });
    // No response yet: nothing to credit it against.
    expect(projectUsage(db, 'p').meko.savings).toMatchObject({ seeds: 0, tokens: 0 });

    const at = (s: number) => new Date(Date.UTC(2026, 8, 29, 12, 0, s)).toISOString();
    const main = join(dir, 'c.jsonl');
    writeFileSync(main, reply('m1', at(0)) + reply('m2', at(1)) + reply('m3', at(2)));
    ingestSessionUsage(db, 's1', main);
    // The first response's context (201,000) held the 1,000-token seed; a search's context doesn't hold its result yet.
    const expected = searchSaving({ contextTokens: 200_000, resultTokens: 1000, laterResponses: 2, model: 'claude-opus-5-5' });
    const pu = projectUsage(db, 'p');
    expect(pu.meko).toMatchObject({ calls: 0, lookups: 0, found: 0, foundTokens: 0 });
    expect(pu.meko.savings).toEqual({ searches: 0, seeds: 1, tokens: expected.tokens, usd: expected.usd, unpricedSearches: 0, unpricedSeeds: 0 });
    expect(usageOverview(db).byProject[0].meko.savings).toEqual(pu.meko.savings);

    // A seed on a model with no price counts in the tokens and as an unpriced seed, not an unpriced search.
    run(db, `UPDATE usage_messages SET model = 'claude-fake-1' WHERE message_id = 'm1'`);
    expect(projectUsage(db, 'p').meko.savings).toMatchObject({ seeds: 1, tokens: expected.tokens, usd: null, unpricedSearches: 0, unpricedSeeds: 1 });

    // A search at start that found nothing, or came too late, passed nothing.
    run(db, `UPDATE meko_preloads SET outcome = 'late'`);
    expect(projectUsage(db, 'p').meko.savings).toMatchObject({ seeds: 0, tokens: 0 });
  });
});

describe('Meko calls for the usage stats', () => {
  it('keeps when each call and seed was made, and counts calls by the tools the report names', () => {
    const { dir, db } = setup();
    const at = (s: number) => new Date(Date.UTC(2026, 8, 29, 12, 0, s)).toISOString();
    recordPreload(db, 's1', { outcome: 'found', results: 1, chars: 400, ms: 700 }, Date.UTC(2026, 8, 29, 11, 59));
    const main = join(dir, 'c.jsonl');
    writeFileSync(main,
      reply('m0', at(0)) +
      call('m1', [['a', 'mcp__meko__memory_search'], ['b', 'mcp__plugin_x_meko__memory_search'], ['c', 'mcp__meko__datapack_list'],
        ['d', 'mcp__meko__memory_add']], { at: at(1) }) +
      result('a', found) + result('b', wrapped({ results: [] })) + result('c', wrapped({ datapacks: [] })) + result('d', wrapped({ id: 'x' })));
    ingestSessionUsage(db, 's1', main);
    const rows = mekoCalls(db);
    expect(rows.map((r) => [r.tool, r.at]).sort()).toEqual([
      ['datapack_list', Date.parse(at(1))], ['memory_add', Date.parse(at(1))], ['memory_search', Date.parse(at(1))], ['memory_search', Date.parse(at(1))],
      // The seed is dated by the search at start, not the first response it's credited against.
      ['seed', Date.UTC(2026, 8, 29, 11, 59)],
    ]);
    // Admin tools are `other`; seeds aren't calls.
    expect(toolCounts(rows)).toEqual({ memory_search: 2, memory_add: 1, other: 1 });
    expect(toolCounts([])).toEqual({});
  });

  it('names exactly the lookups and saves', () => {
    expect([...REPORTED_TOOLS].sort()).toEqual([
      'artifact_get', 'artifact_put', 'context_search', 'conversation_add_message', 'conversation_get', 'knowledgebase_search',
      'memory_add', 'memory_get_all', 'memory_get_by_id', 'memory_promote', 'memory_search', 'memory_update',
    ]);
  });

  it('lists the searches at start', () => {
    const { db } = setup();
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name,
             hook_token, desired, status, stage, created_at) VALUES ('s2', 'p', 's2', 'S2', 'c', 'b2', 'main', '/w2', 't2', 'tok2', 'running', 'idle', 'code', 0)`);
    recordPreload(db, 's1', { outcome: 'late', results: 0, chars: 0, ms: 5000 }, 1000);
    recordPreload(db, 's2', { outcome: 'empty', results: 0, chars: 0, ms: 300 }, 2000);
    expect(mekoStarts(db).sort((a, b) => a.at - b.at)).toEqual([{ at: 1000, outcome: 'late', ms: 5000 }, { at: 2000, outcome: 'empty', ms: 300 }]);
  });
});

describe('Meko in the first turn', () => {
  const at = (s: number) => new Date(Date.UTC(2026, 8, 30, 12, 0, s)).toISOString();
  const endAt = (db: ReturnType<typeof openDb>, id = 's1') =>
    all<{ first_turn_end_at: number | null }>(db, 'SELECT first_turn_end_at FROM sessions WHERE id = ?', id)[0].first_turn_end_at;
  /** Something the user sent, as Claude Code writes it. */
  const said = (content: unknown, s: number, extra: Record<string, unknown> = {}) =>
    JSON.stringify({ type: 'user', timestamp: at(s), message: { role: 'user', content }, ...extra }) + '\n';
  const synthetic = (s: number) =>
    JSON.stringify({ type: 'assistant', timestamp: at(s), message: { id: 'e1', model: '<synthetic>', content: [{ type: 'text', text: 'API Error' }] } }) + '\n';

  function addSession(db: ReturnType<typeof openDb>, id: string, project = 'p') {
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name,
             hook_token, desired, status, stage, created_at) VALUES (?, ?, ?, ?, ?, 'b', 'main', '/w', ?, ?, 'running', 'idle', 'code', 0)`,
      id, project, id, id, `c-${id}`, `t-${id}`, `tok-${id}`);
  }
  const addDatapack = (db: ReturnType<typeof openDb>, project = 'p') =>
    run(db, `INSERT INTO project_datapacks(project_id, datapack_id, name, role) VALUES (?, ?, ?, 'dedicated')`, project, `dp-${project}`, `adlc/proj/${project}`);

  it('reads the lines that tell where a turn ended', () => {
    expect(parseTurnMark(reply('m1', at(0), 'end_turn'))).toEqual({ kind: 'end', at: Date.parse(at(0)) });
    expect(parseTurnMark(synthetic(1))).toEqual({ kind: 'end', at: Date.parse(at(1)) });
    expect(parseTurnMark(reply('m1', at(0), 'tool_use'))).toBeNull();
    expect(parseTurnMark(reply('m1', at(0)))).toBeNull();
    expect(parseTurnMark(said('Fix the thing', 2))).toEqual({ kind: 'user', at: Date.parse(at(2)) });
    expect(parseTurnMark(said([{ type: 'text', text: '[Request interrupted by user]' }], 3))).toEqual({ kind: 'user', at: Date.parse(at(3)) });
    expect(parseTurnMark(said([{ type: 'tool_result', tool_use_id: 't', content: 'ok' }], 4))).toBeNull();
    expect(parseTurnMark(said([{ type: 'text', text: 'Base directory for this skill: …' }], 5, { isMeta: true }))).toBeNull();
    expect(parseTurnMark(said('This session is being continued…', 6, { isCompactSummary: true }))).toBeNull();
    expect(parseTurnMark(said('  ', 7))).toBeNull();
  });

  it("keeps when the main transcript's first turn ended, once, and ignores subagents", () => {
    const { dir, db } = setup();
    const main = join(dir, 'c.jsonl');
    const sub = join(dir, 'c', 'subagents');
    mkdirSync(sub, { recursive: true });
    writeFileSync(join(sub, 'agent-a1.jsonl'), reply('x1', at(1), 'end_turn'));
    writeFileSync(main, said('Fix the thing', 0) + call('m1', [['t', 'Read']], { at: at(1) }) + reply('m2', at(2), 'end_turn'));
    ingestSessionUsage(db, 's1', main);
    expect(endAt(db)).toBe(Date.parse(at(2)));
    appendFileSync(main, said('Now the next thing', 5) + reply('m3', at(9), 'end_turn'));
    ingestSessionUsage(db, 's1', main);
    expect(endAt(db)).toBe(Date.parse(at(2)));
  });

  it('ends an interrupted first turn at the interruption, and one that failed at its API error', () => {
    const { dir, db } = setup();
    addSession(db, 's2');
    const main = join(dir, 'c.jsonl');
    // The first prompt starts the turn (no response before it); the interruption, read later, ends it.
    writeFileSync(main, said('Fix the thing', 0) + call('m1', [['t', 'Read']], { at: at(1) }));
    ingestSessionUsage(db, 's1', main);
    expect(endAt(db)).toBeNull();
    appendFileSync(main, said([{ type: 'text', text: '[Request interrupted by user]' }], 3) + said('Try again', 4) + reply('m2', at(9), 'end_turn'));
    ingestSessionUsage(db, 's1', main);
    expect(endAt(db)).toBe(Date.parse(at(3)));

    const other = join(dir, 's2.jsonl');
    writeFileSync(other, said('Fix the thing', 0) + synthetic(2) + said('again', 5) + reply('m9', at(8), 'end_turn'));
    ingestSessionUsage(db, 's2', other);
    expect(endAt(db, 's2')).toBe(Date.parse(at(2)));
  });

  it('counts the tokens Meko brought into first turns: searches before the turn ended, and what AgentK passed at startup', () => {
    const { dir, db } = setup();
    addDatapack(db);
    for (const id of ['s2', 's3', 's4', 's5', 's6']) addSession(db, id);
    // A project with no datapack had no Meko to search: its sessions don't count.
    run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('q', 'Q', '/q', 'main', 0)`);
    addSession(db, 'q1', 'q');
    const write = (id: string, text: string) => {
      const f = join(dir, `${id}.jsonl`);
      writeFileSync(f, text);
      ingestSessionUsage(db, id, f);
    };
    // s1: a search in the first turn, and one that found nothing. s2: a search only after it. s3:
    // nothing, but AgentK's search passed something. s4: both. s5: its first turn hasn't ended. s6:
    // from before AgentK searched at start.
    write('s1', call('m1', [['a', 'mcp__meko__context_search'], ['z', 'mcp__meko__memory_search']], { at: at(0) }) + result('a', found) +
      result('z', wrapped({ results: [] })) + reply('m2', at(1), 'end_turn'));
    write('s2', reply('m1', at(0), 'end_turn') + call('m2', [['b', 'mcp__meko__context_search']], { at: at(5) }) + result('b', found));
    write('s3', reply('m1', at(0), 'end_turn'));
    write('s4', call('m1', [['c', 'mcp__meko__memory_search']], { at: at(0) }) + result('c', found) + reply('m2', at(1), 'end_turn'));
    write('s5', call('m1', [['d', 'mcp__meko__memory_search']], { at: at(0) }) + result('d', found));
    write('s6', reply('m1', at(0), 'end_turn'));
    write('q1', call('m1', [['e', 'mcp__meko__memory_search']], { at: at(0) }) + result('e', found) + reply('m2', at(1), 'end_turn'));
    recordPreload(db, 's3', { outcome: 'found', results: 2, chars: 300, ms: 900 });
    recordPreload(db, 's4', { outcome: 'found', results: 1, chars: 100, ms: 900 });
    recordPreload(db, 's2', { outcome: 'late', results: 1, chars: 100, ms: 9000 });
    recordPreload(db, 's1', { outcome: 'empty', results: 0, chars: 0, ms: 900 });

    const tokens = Math.round(found.length / 4);
    expect(get(db, `SELECT result_chars FROM meko_calls WHERE tool_use_id = 'a'`)).toEqual({ result_chars: found.length });
    // The late and empty searches at start passed nothing, so only s3's 300 and s4's 100 characters count.
    const want = { sessions: 5, startSearches: 4, searchedTokens: 2 * tokens, seedTokens: 100 };
    expect(projectUsage(db, 'p').mekoFirstTurn).toEqual(want);
    expect(projectUsage(db, 'q').mekoFirstTurn).toEqual({ sessions: 0, startSearches: 0, searchedTokens: 0, seedTokens: 0 });
    expect(usageOverview(db).mekoFirstTurn).toEqual(want);
  });
});
