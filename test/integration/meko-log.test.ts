import { existsSync } from 'node:fs';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { buildServer } from '../../src/server/server.js';
import { issueSetupToken } from '../../src/server/auth.js';
import type { MekoTrace } from '../../src/server/meko.js';
import { mekoLogPath } from '../../src/server/mekolog.js';
import type { MekoCallDetail, MekoCallPage, MekoLogInfo } from '../../src/shared/types.js';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';

describe('Meko call log', () => {
  let env: TestEnv;
  let app: FastifyInstance;
  let cookie: string;

  beforeEach(async () => {
    env = await makeEnv();
    app = await buildServer(env.core, { tls: null, webDir: null, updater: null });
    const token = issueSetupToken(env.core.db);
    const res = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me', password: 'correct horse battery' } });
    cookie = String(res.headers['set-cookie']).split(';')[0];
  });
  afterEach(async () => {
    await app.close();
    await env.cleanup();
  });

  const call = (method: 'GET' | 'PATCH' | 'DELETE', url: string, payload?: unknown) =>
    app.inject({ method, url, headers: { cookie }, ...(payload !== undefined ? { payload: payload as object } : {}) });

  /** A call AgentK made for the session, as MekoClient reports it. */
  const traced = (sessionId: string, over: Partial<MekoTrace> = {}) => env.core.meko.onTrace!({
    tool: 'context_search', at: Date.now(), ms: 120, ok: true, attempts: 1, args: { query: 'q' },
    text: '{"results":{"memory":[{"id":"m1","memory":"Gotcha: x."}]}}', error: null, tag: null, sessionId, ...over,
  });

  async function session() {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Fix it', prompt: 'Fix it', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    return { p, s };
  }

  it('keeps every route behind the login', async () => {
    for (const [method, url] of [
      ['GET', '/api/projects/p/meko-logs'],
      ['GET', '/api/sessions/s/meko-calls'],
      ['GET', '/api/sessions/s/meko-calls/0?id=x'],
      ['GET', '/api/sessions/s/meko-calls.jsonl'],
      ['DELETE', '/api/sessions/s/meko-calls'],
    ] as const) {
      expect((await app.inject({ method, url })).statusCode, `${method} ${url}`).toBe(401);
    }
  });

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
    const logs = (await call('GET', `/api/projects/${p.id}/meko-logs`)).json() as MekoLogInfo[];
    expect(logs).toEqual([{ sessionId: s.id, name: 'Fix it', bytes: expect.any(Number), updatedAt: expect.any(Number) }]);

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
    expect(detail).toEqual({ ...latest, args: { text: 'Gotcha: y.' }, result: { error: 'agent_id required' } });
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
