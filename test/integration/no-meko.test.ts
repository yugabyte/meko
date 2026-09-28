import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { buildServer } from '../../src/server/server.js';
import { issueSetupToken } from '../../src/server/auth.js';
import { MekoError } from '../../src/server/meko.js';
import { run } from '../../src/server/db.js';
import { gitRepo, makeEnv, waitFor, type TestEnv } from '../helpers.js';

const pinFile = (projectId: string) => join(process.env.MEKO_WATERMARK_DIR!, `pin-claude_code_${projectId}.json`);

describe('projects without a datapack, and without Meko', () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv();
  });
  afterEach(async () => {
    await env.cleanup();
  });

  const turnMekoOff = async () => {
    env.core.updateConfig((c) => {
      c.meko.enabled = false;
    });
  };

  it('creates a project with no datapack without asking Meko, and runs sessions in it', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Plain', datapack: { mode: 'none' } });
    expect(p.datapacks).toEqual([]);
    expect(existsSync(pinFile(p.id))).toBe(false);
    expect(env.meko.calls.some((c) => c.name === 'createDatapack' || c.name === 'ensureConversation')).toBe(false);
    const s = await env.core.sessions.create(p.id, { name: 'Work', kind: 'quick' });
    expect(s.projectId).toBe(p.id);
    expect(await env.core.projects.search(p.id, 'anything')).toEqual([]);
  });

  it('turns Meko off: sessions get no datapack, and datapacks are refused with a reason', async () => {
    const withDp = await env.core.projects.create({ repoPath: env.repo, name: 'Has', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/has' } });
    await turnMekoOff();
    expect(env.core.mekoStatus).toMatchObject({ configured: false, sharing: null });
    const s = await env.core.sessions.create(withDp.id, { name: 'Work', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'the session to start');
    const argv = join(env.fakeDir, 'argv.jsonl');
    const launched = readFileSync(argv, 'utf8').trim().split('\n').map((l) => JSON.parse(l) as { args: string[]; env: Record<string, string> });
    expect(launched[0].env.MEKO_DATAPACK_ID).toBe('');
    expect(launched[0].args.join(' ')).not.toContain('dp-adlc-proj-demo-repo-has');
    await expect(env.core.projects.create({ repoPath: env.repo, name: 'X', datapack: { mode: 'create', name: 'adlc/proj/x/y' } }))
      .rejects.toMatchObject({ status: 409, code: 'MEKO_OFF' });
    expect((await env.core.projects.create({ repoPath: env.repo, name: 'Y', datapack: { mode: 'none' } })).datapacks).toEqual([]);
  });

  it("says so when Meko's plan has no room for another datapack", async () => {
    (env.meko as { createDatapack: unknown }).createDatapack = async () => {
      throw new MekoError('[FREE_TIER_DATAPACK_LIMIT] Upgrade to Standard to create a new datapack.');
    };
    await expect(env.core.projects.create({ repoPath: env.repo, name: 'Z', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/z' } }))
      .rejects.toMatchObject({ status: 402, code: 'DATAPACK_LIMIT', message: expect.stringMatching(/Create the project without one/) });
    expect(env.core.state().projects).toEqual([]);
  });

  it('creates without a datapack from the clone an earlier try made before Meko refused its datapack', async () => {
    const src = gitRepo(join(env.home, 'upstream', 'cloned-repo'));
    (env.meko as { createDatapack: unknown }).createDatapack = async () => {
      throw new MekoError('FREE_TIER_DATAPACK_LIMIT: Upgrade to Standard to create a new datapack.');
    };
    await expect(env.core.projects.create({ cloneUrl: src, name: 'C', datapack: { mode: 'create', name: 'adlc/proj/cloned-repo/c' } }))
      .rejects.toMatchObject({ status: 402 });
    const p = await env.core.projects.create({ cloneUrl: src, name: 'C', datapack: { mode: 'none' } });
    expect(p.repoPath).toBe(join(env.home, 'code', 'cloned-repo'));
    // Something else in that folder is still left alone.
    await expect(env.core.projects.create({ cloneUrl: gitRepo(join(env.home, 'other', 'cloned-repo')), name: 'D', datapack: { mode: 'none' } }))
      .rejects.toMatchObject({ status: 409 });
  });

  it("doesn't take any refusal that mentions datapacks for the plan limit", async () => {
    (env.meko as { createDatapack: unknown }).createDatapack = async () => {
      throw new MekoError("You don't have permission to create a new datapack in this org.");
    };
    await expect(env.core.projects.create({ repoPath: env.repo, name: 'Z', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/z' } }))
      .rejects.toMatchObject({ status: 502 });
  });

  it('adds one datapack at a time, and starts a new server-side conversation with it', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Race', datapack: { mode: 'none' } });
    run(env.core.db, `UPDATE projects SET meko_conversation_id = 'conv-without-datapack' WHERE id = ?`, p.id);
    const [a, b] = await Promise.allSettled([
      env.core.projects.setDedicatedDatapack(p.id, { mode: 'create', name: 'adlc/proj/demo-repo/one' }),
      env.core.projects.setDedicatedDatapack(p.id, { mode: 'create', name: 'adlc/proj/demo-repo/two' }),
    ]);
    expect(a.status).toBe('fulfilled');
    expect(b).toMatchObject({ status: 'rejected', reason: { status: 409 } });
    expect(env.meko.calls.filter((c) => c.name === 'createDatapack')).toHaveLength(1);
    await waitFor(() => env.meko.calls.filter((c) => c.name === 'ensureConversation').length, (n) => n >= 1, 5000, 'a new conversation');
    expect(env.meko.calls.find((c) => c.name === 'ensureConversation')?.args[1]).toBe('dp-adlc-proj-demo-repo-one');
  });

  it('adds a dedicated datapack later, pinned for new sessions', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Later', datapack: { mode: 'none' } });
    const withDp = await env.core.projects.setDedicatedDatapack(p.id, { mode: 'create', name: 'adlc/proj/demo-repo/later' });
    expect(withDp.datapacks).toEqual([{ id: 'dp-adlc-proj-demo-repo-later', name: 'adlc/proj/demo-repo/later', role: 'dedicated' }]);
    expect(JSON.parse(readFileSync(pinFile(p.id), 'utf8')).datapack_id).toBe('dp-adlc-proj-demo-repo-later');
    await expect(env.core.projects.setDedicatedDatapack(p.id, { mode: 'existing', id: 'dp-2', name: 'other' }))
      .rejects.toMatchObject({ status: 409 });
  });

  it('makes an attached shared datapack the dedicated one', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Shared', datapack: { mode: 'none' } });
    env.core.projects.attachDatapack(p.id, { id: 'dp-kb', name: 'team/kb' });
    const r = await env.core.projects.setDedicatedDatapack(p.id, { mode: 'existing', id: 'dp-kb', name: 'team/kb' });
    expect(r.datapacks).toEqual([{ id: 'dp-kb', name: 'team/kb', role: 'dedicated' }]);
  });

  describe('over HTTP', () => {
    let app: FastifyInstance;
    let cookie: string;
    beforeEach(async () => {
      app = await buildServer(env.core, { tls: null, webDir: null, updater: null });
      const token = issueSetupToken(env.core.db);
      const res = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me', password: 'correct horse battery' } });
      cookie = String(res.headers['set-cookie']).split(';')[0];
    });
    afterEach(async () => {
      await app.close();
    });
    const call = (method: 'GET' | 'POST' | 'PUT', url: string, payload?: unknown) =>
      app.inject({ method, url, headers: { cookie }, ...(payload !== undefined ? { payload: payload as object } : {}) });

    it('answers with an error code the dialog can act on, and empty lists while Meko is off', async () => {
      (env.meko as { createDatapack: unknown }).createDatapack = async () => {
        throw new MekoError('STANDARD_TIER_DATAPACK_LIMIT: Upgrade to Pro to create a new datapack.');
      };
      const res = await call('POST', '/api/projects', { repoPath: env.repo, name: 'L', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/l' } });
      expect(res.statusCode).toBe(402);
      expect(res.json()).toMatchObject({ code: 'DATAPACK_LIMIT' });

      expect((await call('PUT', '/api/settings', { meko: { enabled: false } })).json().meko.enabled).toBe(false);
      const state = (await call('GET', '/api/state')).json();
      expect(state.meko).toMatchObject({ configured: false, sharing: null });
      expect((await call('POST', '/api/invites/check')).json()).toEqual({ datapacks: [], invites: [] });
      expect((await call('GET', '/api/meko/datapacks')).json()).toEqual([]);

      // Back on: the key is checked first.
      env.mekoTools.state.checkError = 'invalid token';
      expect((await call('PUT', '/api/settings', { meko: { enabled: true } })).statusCode).toBe(400);
      expect(env.core.config.meko.enabled).toBe(false);
      env.mekoTools.state.checkError = null;
      expect((await call('PUT', '/api/settings', { meko: { enabled: true } })).json().meko.enabled).toBe(true);
      expect(env.core.mekoStatus.configured).toBe(true);
    });

    it('turns Meko on with a new key', async () => {
      await turnMekoOff();
      const res = await call('PUT', '/api/settings', { meko: { apiKey: 'mko_tkn_brand_new_key' } });
      expect(res.statusCode).toBe(200);
      expect(res.json().meko).toMatchObject({ enabled: true, keyHint: '_key' });
      expect(env.core.mekoStatus.configured).toBe(true);
    });
  });
});
