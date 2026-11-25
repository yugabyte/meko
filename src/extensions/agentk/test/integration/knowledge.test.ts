import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { buildServer } from '../../src/server/server.js';
import { run } from '../../src/server/db.js';
import { KnowledgeService } from '../../src/server/knowledge.js';
import type { ServerEvent } from '../../src/shared/types.js';
import { makeEnv, noMeko, testMeko, waitFor, type ApiCall, type TestEnv } from '../helpers.js';

describe('adding files to a knowledge base', () => {
  let env: TestEnv;
  let app: FastifyInstance;

  beforeEach(async () => {
    env = await makeEnv({ start: false });
    app = await buildServer(env.core, { webDir: null, updates: null });
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
      headers: { 'content-type': 'application/octet-stream', ...headers }, payload,
    });
  const uploads = () => env.mekoApi.calls.filter((c) => c.path.endsWith('/knowledge-bases/upload-url'));
  const STORAGE = 'https://storage.test/kb-bucket';
  const stored = () => env.mekoApi.calls.filter((c) => c.path.startsWith(STORAGE));
  /** Meko's answer to upload-url: a signed post to the storage, with the document's key. */
  const presigned = (key: string) =>
    ({ body: { data: { url: STORAGE, fields: { key, policy: 'cG9saWN5', 'x-amz-signature': 'secret-sig' }, key, already_uploaded: false } } });

  it("sends the file to the project datapack's knowledge base, same-origin only and only as raw bytes", async () => {
    const p = await project('alpha');
    session(p.id, 's1');
    env.mekoApi.handle((c: ApiCall) => c.path.endsWith('/upload-url') ? presigned('dk-1')
      : c.path.startsWith(STORAGE) ? { status: 204 }
      : { status: 404, body: { error: { code: 'NOT_FOUND', message: 'not registered yet' } } });

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
    const [asked] = uploads();
    expect([asked.path, asked.auth]).toEqual(['/datapacks/dp-alpha/knowledge-bases/upload-url', 'Bearer test-key']);
    expect(asked.body).toEqual({ filename: 'notes.md', size: 4, content_hash: createHash('sha256').update('# Hi').digest('hex'), client: 'agent-k-local' });
    // The bytes go to the storage Meko named, with its signed fields first and without the key.
    const [post] = stored();
    expect(post.auth).toBeNull();
    expect([...post.form!.keys()]).toEqual(['key', 'policy', 'x-amz-signature', 'file']);
    expect(await (post.form?.get('file') as File).text()).toBe('# Hi');

    // Meko's refusal comes back (worded by uploadError, tested on its own).
    env.mekoApi.handle(() => ({ status: 403, body: { error: { code: 'FORBIDDEN', message: 'forbidden' } } }));
    expect((await upload('s1', 'notes.md', Buffer.from('# Hi'))).statusCode).toBe(403);
    // So does the storage's.
    env.mekoApi.handle((c: ApiCall) => c.path.endsWith('/upload-url') ? presigned('dk-1')
      : { status: 403, body: '<Error><Code>AccessDenied</Code><Message>Invalid according to Policy: Policy expired.</Message></Error>' });
    expect((await upload('s1', 'notes.md', Buffer.from('# Hi'))).statusCode).toBe(502);
  });

  it('saves a file of any type for Claude, same-origin only, owner-only, and only as raw bytes', async () => {
    const p = await project('files');
    session(p.id, 'sf');
    const attach = (name: string, payload: Buffer | string | object, headers: Record<string, string> = {}) =>
      app.inject({
        method: 'POST', url: `/api/sessions/sf/files?name=${encodeURIComponent(name)}`,
        headers: { 'content-type': 'application/octet-stream', ...headers }, payload,
      });
    expect((await attach('a.zip', Buffer.from('PK'), { origin: 'https://evil.example' })).statusCode).toBe(403);
    expect((await attach('a.zip', { path: '/etc/passwd' }, { 'content-type': 'application/json' })).statusCode).toBe(415);
    const ok = await attach('../a b.zip', Buffer.from('PK'));
    expect(ok.statusCode).toBe(200);
    const path: string = ok.json().path;
    expect(path.startsWith(join(env.core.paths.pastes, 'sf') + '/')).toBe(true);
    expect(readFileSync(path, 'utf8')).toBe('PK');
    // Nothing went to Meko.
    expect(env.mekoApi.calls).toEqual([]);
  });

  it('refuses with no datapack, with Meko off, and for an archived session', async () => {
    const bare = await project('bare', false);
    session(bare.id, 's-bare');
    const none = await upload('s-bare', 'notes.md', Buffer.from('# Hi'));
    expect(none.statusCode).toBe(409);
    expect(none.json().error).toMatch(/no datapack/);

    const p = await project('beta');
    session(p.id, 's2');
    await noMeko(env);
    const off = await upload('s2', 'notes.md', Buffer.from('# Hi'));
    expect(off.statusCode).toBe(409);
    expect(off.json().error).toMatch(/Meko isn't running on this computer/);
    await testMeko(env);
    run(env.core.db, "UPDATE sessions SET desired = 'archived' WHERE id = 's2'");
    expect((await upload('s2', 'notes.md', Buffer.from('# Hi'))).statusCode).toBe(409);
    expect(uploads()).toEqual([]);
  });

  it('follows the indexing until Meko is done, says how it went, and stops at an error that waiting won\'t fix', async () => {
    const p = await project('gamma');
    session(p.id, 's3');
    const kb = new KnowledgeService(env.core, env.mekoApi.api, { fastPollMs: 5, fastForMs: 1000, slowPollMs: 5, giveUpMs: 60_000 });
    const events: ServerEvent[] = [];
    env.core.on('notify', (e: ServerEvent) => events.push(e));
    const statuses: Record<string, unknown[]> = {
      // Not registered yet, then waiting for the bytes, then indexing, then done.
      'dk-a': [404, 'awaiting_upload', 'indexing', 'completed'],
      'dk-b': ['failed'],
      // Its link expired before the bytes arrived.
      'dk-x': ['abandoned'],
      // An older Meko with no state.
      'dk-y': [{ document_status: 'PROCESSING' }, { document_status: 'COMPLETED' }],
      // A refused key: no point waiting 15 minutes.
      'dk-c': [401],
    };
    env.mekoApi.handle((c: ApiCall) => {
      if (c.path.endsWith('/upload-url')) {
        const name = (c.body as { filename: string }).filename;
        if (name === 'old.md') return { body: { data: { url: '', key: 'dk-o', already_uploaded: true, existing_filename: 'old.md' } } };
        return presigned(`dk-${name[0]}`);
      }
      if (c.path.startsWith(STORAGE)) return { status: 204 };
      const key = new URL(`https://x${c.path}`).searchParams.get('document_key')!;
      const next = statuses[key].length > 1 ? statuses[key].shift() : statuses[key][0];
      if (next === 404) return { status: 404, body: { error: { code: 'NOT_FOUND', message: 'document not found for document_key' } } };
      if (next === 401) return { status: 401, body: { error: { code: 'UNAUTHORIZED', message: 'invalid or expired token' } } };
      return { body: { data: typeof next === 'string' ? { document_key: key, state: next } : next } };
    });
    try {
      expect((await kb.upload('s3', 'a.md', Buffer.from('a'))).status).toBe('processing');
      expect((await kb.upload('s3', 'b.md', Buffer.from('b'))).status).toBe('processing');
      expect((await kb.upload('s3', 'c.md', Buffer.from('c'))).status).toBe('processing');
      expect((await kb.upload('s3', 'x.md', Buffer.from('x'))).status).toBe('processing');
      expect((await kb.upload('s3', 'y.md', Buffer.from('y'))).status).toBe('processing');
      // Meko already had the bytes: nothing goes to the storage.
      const posts = stored().length;
      expect(await kb.upload('s3', 'old.md', Buffer.from('o'))).toMatchObject({ status: 'already_uploaded', documentKey: 'dk-o' });
      expect(stored()).toHaveLength(posts);
      await waitFor(() => events.length, (n) => n >= 5);
      expect(events.sort((x, y) => JSON.stringify(x).localeCompare(JSON.stringify(y)))).toEqual([
        { type: 'knowledge', sessionId: 's3', filename: 'a.md', result: 'indexed' },
        { type: 'knowledge', sessionId: 's3', filename: 'b.md', result: 'failed' },
        { type: 'knowledge', sessionId: 's3', filename: 'c.md', result: 'unknown' },
        { type: 'knowledge', sessionId: 's3', filename: 'x.md', result: 'failed' },
        { type: 'knowledge', sessionId: 's3', filename: 'y.md', result: 'indexed' },
      ]);
      // The refused key was asked once; a file Meko already had isn't followed.
      expect(env.mekoApi.calls.filter((c) => c.path.includes('document_key=dk-c'))).toHaveLength(1);
      expect(env.mekoApi.calls.some((c) => c.path.includes('document_key=dk-o'))).toBe(false);

      // Meko turned off, or its URL or key changed: no more status calls, and nothing said.
      statuses['dk-d'] = ['indexing'];
      statuses['dk-e'] = ['indexing'];
      await kb.upload('s3', 'd.md', Buffer.from('d'));
      await noMeko(env);
      kb.forget();
      await kb.upload('s3', 'e.md', Buffer.from('e')).catch(() => undefined);
      await testMeko(env);
      // A change of Meko's settings while an upload is on its way: its document isn't followed on the new Meko.
      statuses['dk-f'] = ['indexing'];
      const late = kb.upload('s3', 'f.md', Buffer.from('f'));
      kb.forget();
      expect((await late).status).toBe('processing');
      const asked = env.mekoApi.calls.length;
      await new Promise((r) => setTimeout(r, 50));
      expect(env.mekoApi.calls.length).toBe(asked);
      expect(events).toHaveLength(5);

      // A file Kode uploads itself (a workshop's gotchas) always hears how it ended: a Meko change
      // while it's followed or on its way ends it as unknown, and a stopping server says nothing.
      const ends: string[] = [];
      const dp = { id: 'dp-g', name: 'adlc/repo/demo/gotchas' };
      statuses['dk-g'] = ['indexing'];
      await kb.uploadFile(dp, 'g.md', Buffer.from('g'), (r) => ends.push(`g ${r}`));
      kb.forget();
      statuses['dk-h'] = ['indexing'];
      const onItsWay = kb.uploadFile(dp, 'h.md', Buffer.from('h'), (r) => ends.push(`h ${r}`));
      kb.forget();
      await onItsWay;
      statuses['dk-i'] = ['indexing'];
      await kb.uploadFile(dp, 'i.md', Buffer.from('i'), (r) => ends.push(`i ${r}`));
      kb.stop();
      expect(ends).toEqual(['g unknown', 'h unknown']);
    } finally {
      kb.stop();
    }
  });
});
