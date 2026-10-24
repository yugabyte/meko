import { existsSync, mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { buildServer } from '../../src/server/server.js';
import type { MekoTrace } from '../../src/server/meko.js';
import { mekoLogPath } from '../../src/server/mekolog.js';
import type { MekoCallDetail, MekoCallPage, MekoLogInfo } from '../../src/shared/types.js';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';

describe('Meko call log', () => {
  let env: TestEnv;
  let app: FastifyInstance;

  beforeEach(async () => {
    env = await makeEnv();
    app = await buildServer(env.core, { webDir: null, updater: null });
  });
  afterEach(async () => {
    await app.close();
    await env.cleanup();
  });

  const call = (method: 'GET' | 'PATCH' | 'DELETE', url: string, payload?: unknown) =>
    app.inject({ method, url, ...(payload !== undefined ? { payload: payload as object } : {}) });

  /** A call AgentK made for the session (this start of it, unless `createdAt` says otherwise), as MekoClient reports it. */
  const traced = (sessionId: string, over: Partial<MekoTrace> = {}, createdAt = rowCreatedAt(sessionId)) => env.core.meko.onTrace!({
    tool: 'context_search', at: Date.now(), ms: 120, ok: true, attempts: 1, args: { query: 'q' },
    text: '{"results":{"memory":[{"id":"m1","memory":"Gotcha: x."}]}}', error: null, tag: null, session: { id: sessionId, createdAt }, ...over,
  });
  const rowCreatedAt = (id: string) => {
    try {
      return env.core.sessions.row(id).created_at;
    } catch {
      return 0;
    }
  };

  async function session() {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Fix it', prompt: 'Fix it', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    return { p, s };
  }

  it("keeps a session's calls only while its project logs them, shows them, and deletes them with the session", async () => {
    const { p, s } = await session();
    const file = mekoLogPath(env.core.paths.designs, s.id);
    expect(p.settings.mekoLog).toBe(false);

    // Off by default: nothing is kept.
    traced(s.id);
    expect(existsSync(file)).toBe(false);
    expect((await call('GET', `/api/sessions/${s.id}/meko-calls`)).json()).toEqual({ calls: [], more: false });
    expect((await call('GET', `/api/projects/${p.id}/meko-logs`)).json()).toEqual([]);
    expect((await call('GET', `/api/sessions/${s.id}/meko-calls.jsonl`)).statusCode).toBe(404);

    expect((await call('PATCH', `/api/projects/${p.id}`, { settings: { mekoLog: 'yes' } })).statusCode).toBe(400);
    const on = await call('PATCH', `/api/projects/${p.id}`, { settings: { mekoLog: true } });
    expect(on.json().settings.mekoLog).toBe(true);

    traced(s.id, { tag: 'L1' });
    traced(s.id, { tool: 'memory_add', ok: false, attempts: 2, args: { text: 'Gotcha: y.' }, text: '{"error":"agent_id required"}', error: 'agent_id required' });
    traced('another-session');
    // A failed start's late call, under the id its Try again reused, isn't this start's.
    traced(s.id, { tool: 'conversation_create' }, rowCreatedAt(s.id) - 1);
    const logs = (await call('GET', `/api/projects/${p.id}/meko-logs`)).json() as MekoLogInfo[];
    expect(logs).toEqual([{ sessionId: s.id, name: 'Fix it', harness: 'claude', bytes: expect.any(Number), updatedAt: expect.any(Number) }]);

    const page = (await call('GET', `/api/sessions/${s.id}/meko-calls`)).json() as MekoCallPage;
    expect(page.more).toBe(false);
    expect(page.calls.map((c) => [c.tool, c.ok, c.attempts, c.learn, c.error])).toEqual([
      ['memory_add', false, 2, null, 'agent_id required'],
      ['context_search', true, 1, 'L1', null],
    ]);
    const failed = (await call('GET', `/api/sessions/${s.id}/meko-calls?failed=1`)).json() as MekoCallPage;
    expect(failed.calls.map((c) => c.tool)).toEqual(['memory_add']);
    const byTool = (await call('GET', `/api/sessions/${s.id}/meko-calls?tool=context_search&limit=1`)).json() as MekoCallPage;
    expect(byTool).toEqual({ calls: [expect.objectContaining({ tool: 'context_search' })], more: false });
    const older = (await call('GET', `/api/sessions/${s.id}/meko-calls?limit=1`)).json() as MekoCallPage;
    expect(older.more).toBe(true);
    const next = (await call('GET', `/api/sessions/${s.id}/meko-calls?limit=1&before=${older.calls[0].offset}`)).json() as MekoCallPage;
    expect(next.calls.map((c) => c.tool)).toEqual(['context_search']);
    expect((await call('GET', `/api/sessions/${s.id}/meko-calls?before=x`)).statusCode).toBe(400);

    const [latest] = page.calls;
    const detail = (await call('GET', `/api/sessions/${s.id}/meko-calls/${latest.offset}?id=${latest.id}`)).json() as MekoCallDetail;
    expect(detail).toEqual({ ...latest, args: { text: 'Gotcha: y.' }, result: { error: 'agent_id required' }, notice: null });
    expect((await call('GET', `/api/sessions/${s.id}/meko-calls/${latest.offset}?id=nope`)).statusCode).toBe(404);
    expect((await call('GET', '/api/sessions/nope/meko-calls')).statusCode).toBe(404);
    expect((await call('GET', '/api/projects/nope/meko-logs')).statusCode).toBe(404);

    const download = await call('GET', `/api/sessions/${s.id}/meko-calls.jsonl`);
    expect(download.statusCode).toBe(200);
    expect(download.headers['content-disposition']).toBe(`attachment; filename="${s.id}-meko-calls.jsonl"`);
    expect(download.body.trim().split('\n')).toHaveLength(2);

    expect((await call('DELETE', `/api/sessions/${s.id}/meko-calls`)).statusCode).toBe(204);
    expect(existsSync(file)).toBe(false);
    expect((await call('GET', `/api/sessions/${s.id}/meko-calls`)).json()).toEqual({ calls: [], more: false });

    // Turned off: new calls aren't kept.
    await call('PATCH', `/api/projects/${p.id}`, { settings: { mekoLog: false } });
    traced(s.id);
    expect(existsSync(file)).toBe(false);

    // Archiving deletes the log, and an archived session gets no new one.
    await call('PATCH', `/api/projects/${p.id}`, { settings: { mekoLog: true } });
    traced(s.id);
    expect(existsSync(file)).toBe(true);
    await env.core.sessions.action(s.id, 'archive');
    expect(existsSync(file)).toBe(false);
    traced(s.id);
    expect(existsSync(file)).toBe(false);
    expect((await call('GET', `/api/projects/${p.id}/meko-logs`)).json()).toEqual([]);
  });

  it("keeps Claude's Meko calls as AgentK's mod reports them, with a reply too large for Claude read from where Claude Code saved it", async () => {
    const { p, s } = await session();
    const row = () => env.core.sessions.row(s.id);
    const mod = (body: Record<string, unknown>) => env.core.sessions.onMod(row().hook_token, '/mod/meko', body, new AbortController().signal);
    const end = (id: string, over: Record<string, unknown> = {}) => mod({
      id, state: 'end', tool: 'mcp__meko__memory_search', args: { query: 'login' }, ms: 900, ok: true, text: '{"results":[]}', error: null, subagent: false, ...over,
    });
    const file = mekoLogPath(env.core.paths.designs, s.id);

    // Off: the mod isn't asked for its calls, and an end that brings one anyway isn't kept.
    expect(await mod({ id: 't1', state: 'start' })).toEqual({ status: 204 });
    await end('t1');
    expect(existsSync(file)).toBe(false);

    await call('PATCH', `/api/projects/${p.id}`, { settings: { mekoLog: true } });
    expect(await mod({ id: 't2', state: 'start' })).toEqual({ status: 200, body: { log: true } });
    await end('t2');
    traced(s.id);
    // Claude Code saved a reply too large for Claude in the session's tool-results folder; one in
    // another session's folder beside it isn't read.
    const transcript = row().transcript_path!;
    const results = join(dirname(transcript), row().claude_session_id!, 'tool-results');
    mkdirSync(results, { recursive: true });
    writeFileSync(join(results, 'mcp-meko-memory_get_all-1.txt'), '{"result":"all of it"}');
    const notice = (path: string) => `Error: result (95,000 characters) exceeds maximum allowed tokens. Output has been saved to ${path}.\nFormat: JSON`;
    await end('t3', { tool: 'mcp__plugin_x_meko__memory_get_all', subagent: true, text: notice(join(results, 'mcp-meko-memory_get_all-1.txt')) });
    const other = join(dirname(transcript), 'another-claude-session', 'tool-results');
    mkdirSync(other, { recursive: true });
    writeFileSync(join(other, 'mcp-meko-memory_search-1.txt'), 'secret');
    await end('t4', { text: notice(join(other, 'mcp-meko-memory_search-1.txt')) });
    await end('t5', { tool: 'mcp__meko__memory_add', ok: false, text: null, error: 'Interrupted' });
    await end('t6', { tool: 'Bash' });

    const page = (await call('GET', `/api/sessions/${s.id}/meko-calls`)).json() as MekoCallPage;
    expect(page.calls.map((c) => [c.tool, c.by, c.subagent, c.ok, c.error])).toEqual([
      ['memory_add', 'claude', false, false, 'Interrupted'],
      ['memory_search', 'claude', false, true, null],
      ['memory_get_all', 'claude', true, true, null],
      ['context_search', 'agentk', false, true, null],
      ['memory_search', 'claude', false, true, null],
    ]);
    const claude = (await call('GET', `/api/sessions/${s.id}/meko-calls?by=claude`)).json() as MekoCallPage;
    expect(claude.calls).toHaveLength(4);
    const agentk = (await call('GET', `/api/sessions/${s.id}/meko-calls?by=agentk`)).json() as MekoCallPage;
    expect(agentk.calls.map((c) => c.tool)).toEqual(['context_search']);
    expect((await call('GET', `/api/sessions/${s.id}/meko-calls?by=someone`)).statusCode).toBe(400);

    const open = async (c: MekoCallPage['calls'][number]) =>
      (await call('GET', `/api/sessions/${s.id}/meko-calls/${c.offset}?id=${c.id}`)).json() as MekoCallDetail;
    expect(await open(page.calls[4])).toEqual({ ...page.calls[4], args: { query: 'login' }, result: { results: [] }, notice: null });
    const saved = await open(page.calls[2]);
    expect([saved.result, saved.notice]).toEqual([{ result: 'all of it' }, expect.stringMatching(/^Error: result/)]);
    const elsewhere = await open(page.calls[1]);
    expect([elsewhere.result, elsewhere.notice]).toEqual([expect.stringMatching(/^Error: result/), null]);
  });

  it("counts a session's own Meko calls as its: the conversation and recall at start", async () => {
    const { s } = await session();
    const conv = env.meko.calls.filter((c) => c.name === 'ensureConversation' && c.args[2] === s.claudeSessionId);
    const searches = env.meko.calls.filter((c) => c.name === 'search');
    expect(conv.length).toBeGreaterThan(0);
    expect(searches.length).toBeGreaterThan(0);
    expect([...conv, ...searches].map((c) => c.scope)).toEqual([...conv, ...searches].map(() => s.id));
    // The project home's own calls belong to no session.
    await env.core.projects.searchDatapacks(s.projectId, 'q');
    expect(env.meko.calls.filter((c) => c.name === 'search').at(-1)?.scope).toBeNull();
  });
});
