import { readFileSync } from 'node:fs';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { buildServer } from '../../src/server/server.js';
import { issueSetupToken } from '../../src/server/auth.js';
import { run } from '../../src/server/db.js';
import { KnowledgeService } from '../../src/server/knowledge.js';
import { mekoLogPath } from '../../src/server/mekolog.js';
import type { ServerEvent } from '../../src/shared/types.js';
import { makeEnv, waitFor, type ApiCall, type TestEnv } from '../helpers.js';

describe('adding files to a knowledge base', () => {
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

  const project = (name: string, datapack = true) =>
    env.core.projects.create({ repoPath: env.repo, name, datapack: datapack ? { mode: 'existing', id: `dp-${name}`, name: `adlc/proj/r/${name}` } : { mode: 'none' } });
  const session = (projectId: string, id: string) =>
    run(env.core.db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name,
      hook_token, desired, status, stage, created_at) VALUES (?, ?, ?, ?, 'c', 'b', 'main', '/w', 't', 'tok', 'running', 'idle', 'code', 0)`, id, projectId, id, id);
  const upload = (id: string, name: string, payload: Buffer | string | object, headers: Record<string, string> = {}) =>
    app.inject({
      method: 'POST', url: `/api/sessions/${id}/knowledge?name=${encodeURIComponent(name)}`,
      headers: { cookie, 'content-type': 'application/octet-stream', ...headers }, payload,
    });
  const uploads = () => env.mekoApi.calls.filter((c) => c.path.endsWith('/knowledge-bases/upload'));

  it("sends the file to the project datapack's knowledge base, behind the login and only as raw bytes", async () => {
    const p = await project('alpha');
    session(p.id, 's1');
    await app.inject({ method: 'PATCH', url: `/api/projects/${p.id}`, headers: { cookie }, payload: { settings: { mekoLog: true } } });
    env.mekoApi.handle((c: ApiCall) => c.path.endsWith('/upload')
      ? { status: 202, body: { data: { document_key: 'dk-1', filename: 'notes.md', status: 'processing' } } }
      : { status: 404, body: { error: { code: 'NOT_FOUND', message: 'not registered yet' } } });

    expect((await upload('s1', 'notes.md', Buffer.from('# Hi'), { cookie: '' })).statusCode).toBe(401);
    expect((await upload('s1', 'notes.md', Buffer.from('# Hi'), { origin: 'https://evil.example' })).statusCode).toBe(403);
    // No form can reach it: a JSON or text body is refused before it's read.
    expect((await upload('s1', 'notes.md', { a: 1 }, { 'content-type': 'application/json' })).statusCode).toBe(415);
    expect((await upload('s1', 'notes.md', 'hi', { 'content-type': 'text/plain' })).statusCode).toBe(415);
    expect((await upload('s1', 'pic.png', Buffer.from('x'))).statusCode).toBe(400);
    expect((await upload('s1', 'notes.md', Buffer.alloc(0))).statusCode).toBe(400);
    expect((await upload('nope', 'notes.md', Buffer.from('# Hi'))).statusCode).toBe(404);
    expect(uploads()).toEqual([]);

    const ok = await upload('s1', 'notes.md', Buffer.from('# Hi'));
    expect(ok.statusCode).toBe(200);
    expect(ok.json()).toEqual({ filename: 'notes.md', documentKey: 'dk-1', datapackName: 'adlc/proj/r/alpha', status: 'processing' });
    const [sent] = uploads();
    expect(sent.path).toBe('/datapacks/dp-alpha/knowledge-bases/upload');
    expect(await (sent.form?.get('file') as File).text()).toBe('# Hi');
    // In the session's Meko call log, as AgentK's call.
    expect(readFileSync(mekoLogPath(env.core.paths.designs, 's1'), 'utf8')).toContain('"tool":"knowledge_base_upload"');

    // Meko's refusal comes back in words.
    env.mekoApi.handle(() => ({ status: 403, body: { error: { code: 'FORBIDDEN', message: 'forbidden' } } }));
    const refused = await upload('s1', 'notes.md', Buffer.from('# Hi'));
    expect(refused.statusCode).toBe(403);
    expect(refused.json().error).toMatch(/only its owner and maintainers can/);
  });

  it('refuses with no datapack, with Meko off, and for an archived session', async () => {
    const bare = await project('bare', false);
    session(bare.id, 's-bare');
    const none = await upload('s-bare', 'notes.md', Buffer.from('# Hi'));
    expect(none.statusCode).toBe(409);
    expect(none.json().error).toMatch(/no datapack/);

    const p = await project('beta');
    session(p.id, 's2');
    env.core.updateConfig((c) => {
      c.meko.enabled = false;
    });
    const off = await upload('s2', 'notes.md', Buffer.from('# Hi'));
    expect(off.statusCode).toBe(409);
    expect(off.json().error).toMatch(/Meko is off/);
    env.core.updateConfig((c) => {
      c.meko.enabled = true;
    });
    run(env.core.db, "UPDATE sessions SET desired = 'archived' WHERE id = 's2'");
    expect((await upload('s2', 'notes.md', Buffer.from('# Hi'))).statusCode).toBe(409);
    expect(uploads()).toEqual([]);
  });

  it('follows the indexing until Meko is done, and says how it went', async () => {
    const p = await project('gamma');
    session(p.id, 's3');
    const kb = new KnowledgeService(env.core, env.mekoApi.api, { fastPollMs: 5, fastForMs: 1000, slowPollMs: 5, giveUpMs: 60_000 });
    const events: ServerEvent[] = [];
    env.core.on('notify', (e: ServerEvent) => events.push(e));
    const statuses: Record<string, unknown[]> = {
      // Not registered yet, then indexing, then done.
      'dk-a': [404, 'PROCESSING', 'COMPLETED'],
      'dk-b': ['FAILED'],
    };
    env.mekoApi.handle((c: ApiCall) => {
      if (c.path.endsWith('/upload')) {
        const name = (c.form?.get('file') as File).name;
        return { status: name === 'old.md' ? 200 : 202, body: { data: { document_key: `dk-${name[0]}`, status: name === 'old.md' ? 'already_processed' : 'processing' } } };
      }
      const key = new URL(`https://x${c.path}`).searchParams.get('document_key')!;
      const next = statuses[key].length > 1 ? statuses[key].shift() : statuses[key][0];
      return next === 404 ? { status: 404, body: { error: { code: 'NOT_FOUND', message: 'no such document' } } } : { body: { data: { document_status: next } } };
    });
    try {
      expect((await kb.upload('s3', 'a.md', Buffer.from('a'))).status).toBe('processing');
      expect((await kb.upload('s3', 'b.md', Buffer.from('b'))).status).toBe('processing');
      expect((await kb.upload('s3', 'old.md', Buffer.from('o'))).status).toBe('already_processed');
      await waitFor(() => events.length, (n) => n >= 2);
      expect(events.sort((x, y) => JSON.stringify(x).localeCompare(JSON.stringify(y)))).toEqual([
        { type: 'knowledge', sessionId: 's3', filename: 'a.md', result: 'indexed' },
        { type: 'knowledge', sessionId: 's3', filename: 'b.md', result: 'failed' },
      ]);
      // A file Meko already had isn't followed.
      expect(env.mekoApi.calls.some((c) => c.path.includes('document_key=dk-o'))).toBe(false);
    } finally {
      kb.stop();
    }
  });
});
