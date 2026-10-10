import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import type { Project } from '../../src/shared/types.js';
import { buildServer } from '../../src/server/server.js';
import { issueSetupToken } from '../../src/server/auth.js';
import { MekoError, sessionAgentId } from '../../src/server/meko.js';
import { get, run } from '../../src/server/db.js';
import { makeEnv, pinnedDatapack, waitFor, type TestEnv } from '../helpers.js';

const OLD = { id: 'dp-adlc-proj-demo-repo-a', name: 'adlc/proj/demo-repo/a' };
const NEW = { id: 'dp-adlc-proj-demo-repo-b', name: 'adlc/proj/demo-repo/b' };

describe('switching the dedicated datapack', () => {
  let env: TestEnv;
  let p: Project;
  beforeEach(async () => {
    env = await makeEnv();
    p = await env.core.projects.create({ repoPath: env.repo, name: 'Switch', datapack: { mode: 'create', name: OLD.name } });
    run(env.core.db, `UPDATE projects SET meko_conversation_id = 'conv-in-old' WHERE id = ?`, p.id);
    env.meko.calls.length = 0;
  });
  afterEach(async () => {
    await env.cleanup();
  });

  const conversation = () => get<{ c: string | null }>(env.core.db, 'SELECT meko_conversation_id AS c FROM projects WHERE id = ?', p.id)?.c;
  const creates = () => env.meko.calls.filter((c) => c.name === 'createDatapack').length;

  it('switches to a new datapack, keeping the old one attached as shared, and launches new sessions with it', async () => {
    const before = await env.core.sessions.create(p.id, { name: 'Before', kind: 'quick' });
    const r = await env.core.projects.setDedicatedDatapack(p.id, { mode: 'create', name: NEW.name, replace: OLD.id });
    expect(r.datapacks).toEqual([
      { ...NEW, role: 'dedicated' },
      { ...OLD, role: 'shared' },
    ]);
    // A session started before keeps its pin on the old one; a new session's pin is the new one.
    const after = await env.core.sessions.create(p.id, { name: 'After', kind: 'quick' });
    expect(pinnedDatapack(env.core.sessions.row(before.id))).toBe(OLD.id);
    expect(pinnedDatapack(env.core.sessions.row(after.id))).toBe(NEW.id);
    // The conversation made in the old datapack is dropped, so a new one is made in the new one.
    await waitFor(() => conversation(), (c) => c === 'conv-1', 5000, 'a new conversation');
    const projectConv = env.meko.calls.find((c) => c.name === 'ensureConversation' && c.args[2] === `agent-k-local-project-${p.id}`);
    expect(projectConv?.args[1]).toBe(NEW.id);

    // The new session launches with the new datapack, and the old one to search.
    await waitFor(() => env.core.sessions.get(after.id).status, (st) => st === 'idle', 10000, 'the session to start');
    const launched = readFileSync(join(env.fakeDir, 'argv.jsonl'), 'utf8').trim().split('\n')
      .map((l) => JSON.parse(l) as { args: string[]; env: Record<string, string> })
      .find((l) => l.env.MEKO_AGENT_ID === sessionAgentId(env.core.sessions.row(after.id)))!;
    expect(launched.env.MEKO_DATAPACK_ID).toBe(NEW.id);
    const prompt = launched.args.join(' ');
    expect(prompt).toContain(`dedicated datapack is "${NEW.name}" (datapack_id ${NEW.id})`);
    expect(prompt).toMatch(new RegExp(`Shared datapacks you may search.*"${OLD.name}" \\(${OLD.id}\\)`));
  });

  it.each([
    ['keeps the old one as shared, swapping roles with an attached shared datapack', false, [{ id: 'dp-kb', name: 'team/kb', role: 'dedicated' }, { ...OLD, role: 'shared' }]],
    ['detaches the old one when asked', true, [{ id: 'dp-kb', name: 'team/kb', role: 'dedicated' }]],
  ])('switching to an existing datapack %s', async (_, detachOld, want) => {
    env.core.projects.attachDatapack(p.id, { id: 'dp-kb', name: 'team/kb' });
    const r = await env.core.projects.setDedicatedDatapack(p.id, { mode: 'existing', id: 'dp-kb', name: 'team/kb', replace: OLD.id, detachOld });
    expect(r.datapacks).toEqual(want);
    expect(creates()).toBe(0);
  });

  it('refuses a stale or wrong switch', async () => {
    await expect(env.core.projects.setDedicatedDatapack(p.id, { mode: 'create', name: NEW.name, replace: 'dp-other' }))
      .rejects.toMatchObject({ status: 409, message: expect.stringMatching(/another tab/) });
    await expect(env.core.projects.setDedicatedDatapack(p.id, { mode: 'existing', ...OLD, replace: OLD.id }))
      .rejects.toMatchObject({ status: 400 });
    await expect(env.core.projects.setDedicatedDatapack(p.id, { mode: 'create', name: NEW.name, detachOld: true }))
      .rejects.toMatchObject({ status: 400 });
    await expect(env.core.projects.setDedicatedDatapack(p.id, { mode: 'create', name: NEW.name, replace: OLD.id, detachOld: 'yes' as unknown as boolean }))
      .rejects.toMatchObject({ status: 400 });
    await expect(env.core.projects.setDedicatedDatapack(p.id, { mode: 'create', name: NEW.name }))
      .rejects.toMatchObject({ status: 409 });
    expect(creates()).toBe(0);
    expect(env.core.projects.get(p.id).datapacks).toEqual([{ ...OLD, role: 'dedicated' }]);
  });

  it("keeps the old one dedicated when Meko's plan has no room", async () => {
    (env.meko as { createDatapack: unknown }).createDatapack = async () => {
      throw new MekoError('[FREE_TIER_DATAPACK_LIMIT] Upgrade to Standard to create a new datapack.');
    };
    await expect(env.core.projects.setDedicatedDatapack(p.id, { mode: 'create', name: NEW.name, replace: OLD.id, detachOld: true }))
      .rejects.toMatchObject({ status: 402, code: 'DATAPACK_LIMIT', message: expect.stringMatching(/Pick one of your datapacks under Use an existing one/) });
    expect(env.core.projects.get(p.id).datapacks).toEqual([{ ...OLD, role: 'dedicated' }]);
    expect(conversation()).toBe('conv-in-old');
  });

  it("keeps a running session's Meko conversation and designs in the datapack it was launched with", async () => {
    const before = await env.core.sessions.create(p.id, { name: 'Before', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(before.id).status, (st) => st === 'idle', 10000, 'the first session to start');
    await env.core.projects.setDedicatedDatapack(p.id, { mode: 'create', name: NEW.name, replace: OLD.id, detachOld: true });

    // Renamed, its conversation is retitled (and looked up) there, never made in the new datapack.
    env.meko.calls.length = 0;
    env.core.sessions.rename(before.id, { name: 'Renamed' });
    const retitled = await waitFor(() => env.meko.calls.find((c) => c.name === 'retitleConversation'), (c) => !!c, 5000, 'the Meko retitle');
    expect(retitled!.args.slice(0, 4)).toEqual([sessionAgentId(env.core.sessions.row(before.id)), OLD.id, 'conv-1', 'Renamed (Switch)']);
    expect(env.meko.calls.filter((c) => c.name === 'ensureConversation').map((c) => c.args[1])).toEqual([OLD.id]);

    const after = await env.core.sessions.create(p.id, { name: 'After', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(after.id).status, (st) => st === 'idle', 10000, 'the second session to start');

    const sentTo = async (id: string, slug: string) => {
      await env.core.lifecycle.saveDesign(id, { markdown: `# ${slug}` });
      const put = await waitFor(() => env.meko.calls.find((c) => c.name === 'putArtifact' && c.args[0] === `design-${slug}.md`), (c) => !!c, 5000, `${slug}'s design sent`);
      // Under the session's own agent id and Meko conversation, in the datapack it was launched with.
      const row = env.core.sessions.row(id);
      expect(put!.args[4]).toBe(sessionAgentId(row));
      expect(env.meko.calls.some((c) => c.name === 'ensureConversation' && c.args[0] === sessionAgentId(row) && c.args[1] === put!.args[3] && c.args[2] === row.claude_session_id)).toBe(true);
      return put!.args[3];
    };
    expect(await sentTo(before.id, 'before')).toBe(OLD.id);
    expect(await sentTo(after.id, 'after')).toBe(NEW.id);
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

    it('switches, and the home lists both datapacks', async () => {
      const res = await app.inject({
        method: 'POST', url: `/api/projects/${p.id}/dedicated-datapack`, headers: { cookie },
        payload: { mode: 'create', name: NEW.name, replace: OLD.id },
      });
      expect(res.statusCode).toBe(200);
      expect((res.json() as Project).datapacks.map((d) => [d.id, d.role])).toEqual([[NEW.id, 'dedicated'], [OLD.id, 'shared']]);
      const home = (await app.inject({ method: 'GET', url: `/api/projects/${p.id}/home`, headers: { cookie } })).json();
      expect(home.datapacks.map((d: { id: string; role: string }) => [d.id, d.role])).toEqual([[NEW.id, 'dedicated'], [OLD.id, 'shared']]);
    });
  });
});
