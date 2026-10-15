import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { buildServer } from '../../src/server/server.js';
import { issueSetupToken } from '../../src/server/auth.js';
import { run } from '../../src/server/db.js';
import { markGap } from '../../src/server/mekogaps.js';
import type { MekoGapPage, ProjectHome } from '../../src/shared/types.js';
import { makeEnv, type TestEnv } from '../helpers.js';

describe("What Meko doesn't know", () => {
  let env: TestEnv;
  let app: FastifyInstance;
  let cookie: string;

  beforeEach(async () => {
    env = await makeEnv({ start: false });
    app = await buildServer(env.core, { tls: null, webDir: null, updater: null });
    const token = issueSetupToken(env.core.db);
    const res = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me', password: 'correct horse battery' } });
    cookie = String(res.headers['set-cookie']).split(';')[0];
  });
  afterEach(async () => {
    await app.close();
    await env.cleanup();
  });

  const call = (method: 'GET' | 'POST', url: string, payload?: object) =>
    app.inject({ method, url, headers: { cookie }, ...(payload ? { payload } : {}) });
  const page = async (projectId: string, state = 'open') => (await call('GET', `/api/projects/${projectId}/meko-gaps?state=${state}`)).json() as MekoGapPage;

  const project = (name: string, datapack = true) =>
    env.core.projects.create({ repoPath: env.repo, name, datapack: datapack ? { mode: 'existing', id: `dp-${name}`, name: `team/${name}` } : { mode: 'none' } });
  const session = (projectId: string, id: string, name: string) =>
    run(env.core.db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name,
      hook_token, desired, status, stage, created_at) VALUES (?, ?, ?, ?, 'c', 'b', 'main', '/w', 't', 'tok', 'running', 'idle', 'code', 0)`, id, projectId, id, name);
  /** A search Claude made, as the transcript reader records it. */
  const search = (sessionId: string, id: string, query: string, at: number, outcome: 'found' | 'empty', quality: 'empty' | 'weak' | null) =>
    run(env.core.db, `INSERT INTO meko_calls(session_id, tool_use_id, tool, kind, at, input_chars, outcome, results, query, query_key, top_score, quality)
      VALUES (?, ?, 'context_search', 'lookup', ?, 40, ?, ?, ?, ?, ?, ?)`,
      sessionId, id, at, outcome, outcome === 'found' ? 10 : 0, query, query.replace(/\s+/g, ' ').trim().toLowerCase(), quality === 'weak' ? 0.71 : null, quality);

  it('groups poor searches by question, and keeps what the user did with each until it is asked again', async () => {
    const p = await project('Alpha');
    session(p.id, 's1', 'tags');
    session(p.id, 's2', 'release-cleanup');
    search('s1', 't1', 'Release tag retention', 1000, 'empty', 'empty');
    search('s2', 't2', 'release  tag retention', 2000, 'found', 'weak');
    // Found something relevant: not a question. Asked poorly, then answered later: left out.
    search('s1', 't3', 'how the mod works', 3000, 'found', null);
    search('s1', 't4', 'later answered', 1500, 'empty', 'empty');
    search('s2', 't5', 'later answered', 2500, 'found', null);
    // AgentK's searches at start, by what Meko returned: nothing, and (picked away, but a real answer) only a conversation hit.
    const start = (sessionId: string, at: number, query: string, quality: 'empty' | null) =>
      run(env.core.db, `INSERT INTO meko_preloads(session_id, at, outcome, results, chars, ms, query, query_key, quality) VALUES (?, ?, 'empty', 0, 0, 900, ?, ?, ?)`,
        sessionId, at, query, query.toLowerCase(), quality);
    start('s2', 1800, 'Offline mode', 'empty');
    start('s1', 1700, 'Cache hit', null);

    const open = await page(p.id);
    expect(open.gaps.map((g) => [g.query, g.quality, g.source, g.asks, g.sessions.map((s) => s.name)])).toEqual([
      ['release  tag retention', 'weak', 'claude', 2, ['release-cleanup', 'tags']],
      ['Offline mode', 'empty', 'start', 1, ['release-cleanup']],
    ]);
    expect(open).toMatchObject({ counts: { open: 2, dismissed: 0, taught: 0 }, open: { empty: 1, weak: 1 }, searches: 7, since: 1000, more: false });
    expect(((await call('GET', `/api/projects/${p.id}/home`)).json() as ProjectHome).usage.mekoGaps).toEqual({ empty: 1, weak: 1 });

    const key = open.gaps[0].key;
    expect((await call('POST', `/api/projects/${p.id}/meko-gaps/mark`, { key, state: 'dismissed' })).statusCode).toBe(204);
    expect((await page(p.id)).gaps.map((g) => g.query)).toEqual(['Offline mode']);
    expect((await page(p.id, 'dismissed')).gaps.map((g) => g.state)).toEqual(['dismissed']);
    // Asked again: open again.
    search('s1', 't6', 'Release tag retention', Date.now() + 1000, 'empty', 'empty');
    expect((await page(p.id)).gaps[0]).toMatchObject({ key, asks: 3, state: 'open', quality: 'empty' });
    // Put back after a dismissal.
    await call('POST', `/api/projects/${p.id}/meko-gaps/mark`, { key, state: 'dismissed' });
    await call('POST', `/api/projects/${p.id}/meko-gaps/mark`, { key, state: null });
    expect((await page(p.id)).counts).toEqual({ open: 2, dismissed: 0, taught: 0 });

    expect((await call('POST', `/api/projects/${p.id}/meko-gaps/mark`, { key: 'nothing like it', state: 'dismissed' })).statusCode).toBe(404);
    expect((await call('POST', `/api/projects/${p.id}/meko-gaps/mark`, { key, state: 'taught' })).statusCode).toBe(400);
    expect((await call('GET', `/api/projects/${p.id}/meko-gaps?state=closed`)).statusCode).toBe(400);
  });

  it("teaches Meko into the dedicated datapack as AgentK, and says why when it can't", async () => {
    const p = await project('Alpha');
    session(p.id, 's1', 'tags');
    search('s1', 't1', 'Release tag retention', 1000, 'empty', 'empty');
    const key = 'release tag retention';
    const teach = (projectId: string, text: string, k = key) => call('POST', `/api/projects/${projectId}/meko-gaps/teach`, { key: k, text });

    expect((await teach(p.id, '   ')).statusCode).toBe(400);
    expect((await teach(p.id, 'x'.repeat(2001))).statusCode).toBe(400);
    expect((await teach(p.id, 'An answer', 'not asked')).statusCode).toBe(404);

    // Meko refuses: nothing is marked.
    const add = env.meko.addMemory;
    (env.meko as { addMemory: unknown }).addMemory = async () => {
      throw new Error('quota exceeded');
    };
    const refused = await teach(p.id, 'Releases keep the last 30 tags.');
    expect(refused.statusCode).toBe(502);
    expect(refused.json().error).toContain('quota exceeded');
    expect((await page(p.id)).counts.taught).toBe(0);
    (env.meko as { addMemory: unknown }).addMemory = add;

    // A learn step taught it first: the person's teach replaces it, and says a person did it.
    markGap(env.core.db, p.id, key, { kind: 'teach', text: 'Tags are kept.', datapack: 'team/Alpha', session: 's1' }, Date.now() - 10);
    expect((await page(p.id, 'taught')).gaps[0].taught).toMatchObject({ text: 'Tags are kept.', session: { id: 's1', name: 'tags', archived: false } });
    const ok = await teach(p.id, '  Releases keep the last 30 tags.  ');
    expect(ok.statusCode).toBe(200);
    expect(ok.json()).toMatchObject({ key, state: 'taught', taught: { text: 'Releases keep the last 30 tags.', datapack: 'team/Alpha', session: null }, dismissal: null });
    expect(env.meko.calls.filter((c) => c.name === 'addMemory').map((c) => c.args)).toEqual([
      ['Releases keep the last 30 tags.', 'dp-Alpha', 'agent-k-local', 'conv-1', { source: 'agent-k-local', kind: 'taught', question: 'Release tag retention' }],
    ]);
    expect((await page(p.id, 'taught')).gaps).toHaveLength(1);
    // Asked again after it was taught: open again, with what was taught (Meko had it, and the search missed it).
    search('s1', 't2', 'Release tag retention', Date.now() + 5, 'found', 'weak');
    expect((await page(p.id)).gaps[0]).toMatchObject({ state: 'open', taught: { text: 'Releases keep the last 30 tags.' } });
    // Dismissed then, it keeps what was taught, and a restore keeps it too.
    await new Promise((r) => setTimeout(r, 20));
    await call('POST', `/api/projects/${p.id}/meko-gaps/mark`, { key, state: 'dismissed' });
    expect((await page(p.id, 'dismissed')).gaps[0]).toMatchObject({ state: 'dismissed', taught: { text: 'Releases keep the last 30 tags.', datapack: 'team/Alpha' } });
    await call('POST', `/api/projects/${p.id}/meko-gaps/mark`, { key, state: null });
    expect((await page(p.id)).gaps[0]).toMatchObject({ state: 'open', taught: { text: 'Releases keep the last 30 tags.' } });

    // No dedicated datapack, or Meko off: a 409 that says so.
    const plain = await project('Plain', false);
    session(plain.id, 's9', 'plain');
    search('s9', 't9', 'Release tag retention', 1000, 'empty', 'empty');
    expect((await teach(plain.id, 'An answer')).json()).toMatchObject({ code: 'NO_DEDICATED_DATAPACK' });
    env.core.updateConfig((c) => {
      c.meko.enabled = false;
    });
    expect((await teach(p.id, 'An answer')).json()).toMatchObject({ code: 'MEKO_OFF' });
  });

  it('pages through questions asked at once without skipping any at the page boundary', async () => {
    const p = await project('Alpha');
    session(p.id, 's1', 'many');
    // Claude's parallel searches in one response share its timestamp: 99 asked earlier, then 4 at once across the boundary.
    for (let i = 0; i < 99; i++) search('s1', `t${i}`, `question ${String(i).padStart(3, '0')}`, 1000 + i, 'empty', 'empty');
    for (const k of ['a', 'b', 'c', 'd']) search('s1', `same-${k}`, `asked at once ${k}`, 500, 'empty', 'empty');
    const first = await page(p.id);
    expect(first.gaps).toHaveLength(100);
    expect(first.more).toBe(true);
    const last = first.gaps.at(-1)!;
    expect(last.lastAt).toBe(500);
    const rest = (await call('GET', `/api/projects/${p.id}/meko-gaps?before=${last.lastAt}&beforeKey=${encodeURIComponent(last.key)}`)).json() as MekoGapPage;
    expect(rest.more).toBe(false);
    expect(new Set([...first.gaps, ...rest.gaps].map((g) => g.key)).size).toBe(103);
    expect((await call('GET', `/api/projects/${p.id}/meko-gaps?before=500`)).statusCode).toBe(400);
  });
});
