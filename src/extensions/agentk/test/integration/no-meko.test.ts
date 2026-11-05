import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { buildServer } from '../../src/server/server.js';
import { MekoError } from '../../src/server/meko.js';
import { run } from '../../src/server/db.js';
import { TEST_MEKO_URL, gitRepo, makeEnv, noMeko, pinFiles, pinnedDatapack, readJsonl, waitFor, type TestEnv } from '../helpers.js';


describe('projects without a datapack, and without Meko', () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv();
  });
  afterEach(async () => {
    await env.cleanup();
  });

  /** No Meko running on this computer: there's no Meko. */
  const turnMekoOff = () => noMeko(env);

  it('says no Meko is installed here, and pushes the change once `meko` is found', async () => {
    await noMeko(env, 'none');
    expect(env.core.mekoStatus.configured).toBe(false);
    expect(env.core.state().meko.local).toBe('none');
    const onChange = vi.fn();
    env.core.on('change', onChange);
    env.local.state = 'stopped';
    await env.core.refreshMeko();
    expect(env.core.state().meko.local).toBe('stopped');
    expect(onChange).toHaveBeenCalled();
  });

  it('creates a project with no datapack without asking Meko, and runs sessions in it', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Plain', datapack: { mode: 'none' } });
    expect(p.datapacks).toEqual([]);
    expect(env.meko.calls.some((c) => c.name === 'createDatapack' || c.name === 'ensureConversation')).toBe(false);
    const s = await env.core.sessions.create(p.id, { name: 'Work', kind: 'quick' });
    expect(s.projectId).toBe(p.id);
    expect(pinFiles()).toEqual([]);
    expect(await env.core.projects.search(p.id, 'anything')).toEqual([]);
  });

  it('with no Meko, sessions get no datapack, and datapacks are refused with a reason', async () => {
    const withDp = await env.core.projects.create({ repoPath: env.repo, name: 'Has', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/has' } });
    await turnMekoOff();
    expect(env.core.mekoStatus).toMatchObject({ configured: false });
    const s = await env.core.sessions.create(withDp.id, { name: 'Work', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'the session to start');
    const argv = join(env.fakeDir, 'argv.jsonl');
    const launched = readJsonl<{ args: string[]; env: Record<string, string> }>(argv);
    expect(launched[0].env.MEKO_DATAPACK_ID).toBe('');
    expect(launched[0].args.join(' ')).not.toContain('dp-adlc-proj-demo-repo-has');
    // No search at start, and no Meko lines in the prompts.
    expect(env.meko.calls.some((c) => c.name === 'search')).toBe(false);
    expect(launched[0].args.join(' ')).not.toMatch(/Meko/);
    await expect(env.core.projects.create({ repoPath: env.repo, name: 'X', datapack: { mode: 'create', name: 'adlc/proj/x/y' } }))
      .rejects.toMatchObject({ status: 409, code: 'MEKO_OFF' });
    expect((await env.core.projects.create({ repoPath: env.repo, name: 'Y', datapack: { mode: 'none' } })).datapacks).toEqual([]);
  });

  it.each([
    ["says so when Meko's plan has no room for another datapack", '[FREE_TIER_DATAPACK_LIMIT] Upgrade to Standard to create a new datapack.',
      { status: 402, code: 'DATAPACK_LIMIT', message: expect.stringMatching(/Pick one of your projects or datapacks, open the project without one, or upgrade/) }],
    ["doesn't take any refusal that mentions datapacks for the plan limit", "You don't have permission to create a new datapack in this org.", { status: 502 }],
  ])('%s', async (_, refusal, want) => {
    (env.meko as { createDatapack: unknown }).createDatapack = async () => {
      throw new MekoError(refusal);
    };
    await expect(env.core.projects.create({ repoPath: env.repo, name: 'Z', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/z' } }))
      .rejects.toMatchObject(want);
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
    const s = await env.core.sessions.create(p.id, { name: 'Work', kind: 'quick' });
    expect(pinnedDatapack(env.core.sessions.row(s.id))).toBe('dp-adlc-proj-demo-repo-later');
    await expect(env.core.projects.setDedicatedDatapack(p.id, { mode: 'existing', id: 'dp-2', name: 'other' }))
      .rejects.toMatchObject({ status: 409 });
  });

  describe('over HTTP', () => {
    let app: FastifyInstance;
    beforeEach(async () => {
      app = await buildServer(env.core, { webDir: null, updates: null });
    });
    afterEach(async () => {
      await app.close();
    });
    const call = (method: 'GET' | 'POST' | 'PUT', url: string, payload?: unknown) =>
      app.inject({ method, url, ...(payload !== undefined ? { payload: payload as object } : {}) });

    it("answers with an error code the dialog can act on, and empty lists while there's no Meko", async () => {
      (env.meko as { createDatapack: unknown }).createDatapack = async () => {
        throw new MekoError('STANDARD_TIER_DATAPACK_LIMIT: Upgrade to Pro to create a new datapack.');
      };
      const res = await call('POST', '/api/projects', { repoPath: env.repo, name: 'L', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/l' } });
      expect(res.statusCode).toBe(402);
      expect(res.json()).toMatchObject({ code: 'DATAPACK_LIMIT' });

      await turnMekoOff();
      const state = (await call('GET', '/api/state')).json();
      expect(state.meko).toMatchObject({ configured: false });
      expect(state.meko).not.toHaveProperty('sharing');
      expect(state.meko).not.toHaveProperty('source');
      expect((await call('POST', '/api/invites/check')).json()).toEqual({ datapacks: [], invites: [], closed: [] });
      expect((await call('GET', '/api/meko/datapacks')).json()).toEqual([]);

      // On again once Meko runs on this computer: opening Settings reads it.
      Object.assign(env.local, { state: 'running', meko: { url: TEST_MEKO_URL, token: 'test-key' } });
      await call('GET', '/api/settings');
      expect((await call('GET', '/api/state')).json().meko).toMatchObject({ configured: true, url: TEST_MEKO_URL });
    });

    it('follows the Meko on this computer as it starts, moves and stops, linked or not', async () => {
      const local = { url: 'http://127.0.0.1:7778/mcp', token: 'local-token-1234' };
      Object.assign(env.local, { state: 'running', meko: local, cloud: { account: '', webUrl: '' } });
      await call('GET', '/api/settings');
      const resets = () => env.meko.calls.filter((c) => c.name === 'reset').length;
      const r0 = resets();
      await call('GET', '/api/settings');
      expect((await call('GET', '/api/state')).json().meko).toMatchObject({ configured: true, url: local.url, webUrl: null });
      expect(resets()).toBe(r0);

      // Restarted on another port: reconnects there, keeping the invites and shares it knows (the same account).
      const forget = vi.spyOn(env.core.invites, 'forgetAccount');
      env.local.meko = { ...local, url: 'http://127.0.0.1:7790/mcp' };
      await call('GET', '/api/settings');
      expect(env.core.mekoStatus.url).toBe('http://127.0.0.1:7790/mcp');
      expect(resets()).toBe(r0 + 1);
      expect(forget).not.toHaveBeenCalled();

      // Stopped: no Meko, and sessions still start.
      Object.assign(env.local, { state: 'stopped', meko: null });
      await call('GET', '/api/settings');
      expect(env.core.state().meko).toMatchObject({ url: null, local: 'stopped' });
      expect(env.core.mekoStatus.configured).toBe(false);
      const p = await env.core.projects.create({ repoPath: env.repo, name: 'Plain', datapack: { mode: 'none' } });
      const s = await env.core.sessions.create(p.id, { name: 'Work', kind: 'quick' });
      await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'the session to start');

      // Started again: the same account, so neither the stop nor the start forgot its invites.
      Object.assign(env.local, { state: 'running', meko: local });
      await call('GET', '/api/settings');
      expect(env.core.mekoStatus).toMatchObject({ configured: true, url: local.url });
      expect(forget).not.toHaveBeenCalled();

      // Linked to Meko Cloud: the same URL and token, another account behind them, whose web app
      // names the share links. Its invites aren't the unlinked Meko's: they're forgotten.
      env.local.cloud = { account: '3f1c09a4be7d5e21', webUrl: 'https://cloud.mekodata.ai' };
      await call('GET', '/api/settings');
      expect((await call('GET', '/api/state')).json().meko).toMatchObject({ configured: true, url: local.url, webUrl: 'https://cloud.mekodata.ai' });
      expect(forget).toHaveBeenCalledTimes(1);
      // Linked to another account: forgotten again. Unlinked: and again.
      env.local.cloud = { account: '0123456789abcdef', webUrl: 'https://cloud.mekodata.ai' };
      await call('GET', '/api/settings');
      expect(forget).toHaveBeenCalledTimes(2);
      env.local.cloud = { account: '', webUrl: '' };
      await call('GET', '/api/settings');
      expect(forget).toHaveBeenCalledTimes(3);
      expect(env.core.mekoStatus).toMatchObject({ configured: true, url: local.url, webUrl: null });
    });
  });
});
