import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { buildServer } from '../../src/server/server.js';
import type { MekoDatapackRow } from '../../src/server/meko.js';
import { makeEnv, waitFor, type ApiCall, type TestEnv } from '../helpers.js';
import { all } from '../../src/server/db.js';

describe('HTTP API: sharing, invites and Meko settings', () => {
  let env: TestEnv;
  let app: FastifyInstance;

  beforeEach(async () => {
    env = await makeEnv({ start: false });
    app = await buildServer(env.core, { webDir: null, updater: null });
  });
  afterEach(async () => {
    await app.close();
    await env.cleanup();
  });

  const call = (method: 'GET' | 'POST' | 'PUT' | 'DELETE', url: string, payload?: unknown) =>
    app.inject({ method, url, ...(payload !== undefined ? { payload: payload as object } : {}) });

  async function project(dpId = 'dp-1') {
    return env.core.projects.create({ repoPath: env.repo, name: 'p', datapack: { mode: 'existing', id: dpId, name: 'adlc/proj/demo-repo/p' } });
  }

  it('clones a PR\'s repo only from a network remote, and finds a clone of it already there', async () => {
    expect((await call('POST', '/api/repos/clone', { url: '/tmp/x.git' })).statusCode).toBe(400);
    expect((await call('POST', '/api/repos/clone', {})).statusCode).toBe(400);
    // Something else is at the folder the clone would go to.
    const taken = await call('POST', '/api/repos/clone', { url: 'https://github.com/acme/demo-repo.git' });
    expect(taken.statusCode).toBe(409);
    execFileSync('git', ['remote', 'add', 'origin', 'https://github.com/acme/demo-repo.git'], { cwd: env.repo });
    const found = await call('POST', '/api/repos/clone', { url: 'git@github.com:acme/demo-repo.git' });
    expect(found.statusCode).toBe(200);
    expect(found.json()).toMatchObject({ name: 'demo-repo', remote: 'github.com/acme/demo-repo' });
  });

  it("shares only datapacks in a project here, only with Meko's API URL known, and passes on Meko's errors", async () => {
    expect((await call('GET', '/api/meko/datapacks/dp-1/people')).statusCode).toBe(404);
    await project();
    // A self-hosted Meko whose web app and API URLs aren't known: no sharing.
    env.core.updateConfig((c) => {
      c.meko.url = 'https://meko.example.com/mcp';
    });
    const noApi = await call('GET', '/api/meko/datapacks/dp-1/people');
    expect(noApi.statusCode).toBe(409);
    expect(noApi.json().error).toMatch(/Meko's API URL isn't known, so AgentK can't share datapacks. Set the web app URL/);
    env.core.updateConfig((c) => {
      c.meko.url = '';
    });

    let shareStatus = 200;
    env.mekoApi.handle((c: ApiCall) => {
      if (c.path === '/datapacks/incoming-shares') return { body: { data: [] } };
      if (c.path.endsWith('/member-list')) return { body: { data: { members: [{ is_owner: true, user_name: 'Me', user_email_id: 'me@example.com', grant: 'owner' }] } } };
      if (c.path.endsWith('/outgoing-shares')) return { body: { data: { pending_invites: [{ invite_id: 'i1', grant: 'contributor', recipient_email_id: 'bob@example.com' }] } } };
      if (c.path.endsWith('/share')) {
        return shareStatus === 200
          ? { body: { data: { status: 'pending', invite_id: 'i1' } } }
          : { status: shareStatus, body: { error: { code: 'CONFLICT', message: 'an invite for bob@example.com is already pending' } } };
      }
      return { body: { data: {} } };
    });

    const people = await call('GET', '/api/meko/datapacks/dp-1/people');
    expect(people.json()).toEqual({
      members: [{ name: 'Me', email: 'me@example.com', grant: 'owner' }],
      invites: [{ inviteId: 'i1', email: 'bob@example.com', grant: 'contributor' }],
    });
    expect((await call('POST', '/api/meko/datapacks/dp-1/people', { email: 'not an email', grant: 'viewer' })).statusCode).toBe(400);
    expect((await call('POST', '/api/meko/datapacks/dp-1/people', { email: 'bob@example.com', grant: 'owner' })).statusCode).toBe(400);
    const shared = await call('POST', '/api/meko/datapacks/dp-1/people', { email: ' bob@example.com ', grant: 'contributor' });
    expect(shared.statusCode).toBe(200);
    expect(env.mekoApi.calls.find((c) => c.path === '/datapacks/dp-1/share')?.body).toEqual({ recipient_email: 'bob@example.com', grant: 'contributor' });
    // Shared with a PR's reviewer: the email is remembered for their GitHub login (and only a login).
    expect((await call('POST', '/api/meko/datapacks/dp-1/people', { email: 'bob@example.com', grant: 'contributor', login: 'Bob-1' })).statusCode).toBe(200);
    expect((await call('POST', '/api/meko/datapacks/dp-1/people', { email: 'eve@example.com', grant: 'contributor', login: '../x' })).statusCode).toBe(200);
    expect(all(env.core.db, "SELECT key, value FROM kv WHERE key LIKE 'reviewer-email:%'")).toEqual([{ key: 'reviewer-email:bob-1', value: 'bob@example.com' }]);
    // Each one went through, a reviewer's too: counted for the suggestions.
    expect((await call('GET', '/api/share-contacts')).json()).toMatchObject([{ email: 'bob@example.com', shares: 2 }, { email: 'eve@example.com', shares: 1 }]);
    // Meko's own refusals keep their status; a server error is a 502.
    shareStatus = 409;
    const refused = await call('POST', '/api/meko/datapacks/dp-1/people', { email: 'bob@example.com', grant: 'contributor' });
    expect(refused.statusCode).toBe(409);
    expect(refused.json().error).toBe('Meko: an invite for bob@example.com is already pending');
    shareStatus = 500;
    expect((await call('POST', '/api/meko/datapacks/dp-1/people', { email: 'bob@example.com', grant: 'contributor' })).statusCode).toBe(502);
    expect((await call('DELETE', '/api/meko/datapacks/dp-1/people/invites/i1')).statusCode).toBe(200);
    expect(env.mekoApi.calls.some((c) => c.method === 'DELETE' && c.path === '/datapacks/dp-1/invite/i1')).toBe(true);

    // The invite went through but the lists can't be read again: still a success, and it says so.
    shareStatus = 200;
    const handler = env.mekoApi.calls.length;
    env.mekoApi.handle((c: ApiCall) => {
      if (c.path.endsWith('/share')) {
        return shareStatus === 200
          ? { body: { data: { status: 'pending', invite_id: 'i2' } } }
          : { status: shareStatus, body: { error: { code: 'UNAUTHORIZED', message: 'invalid or expired token' } } };
      }
      if (c.path === '/datapacks/incoming-shares') return { body: { data: [] } };
      return { status: 500, body: { error: { code: 'INTERNAL_ERROR', message: 'boom' } } };
    });
    const partly = await call('POST', '/api/meko/datapacks/dp-1/people', { email: 'eve@example.com', grant: 'viewer' });
    expect(partly.statusCode).toBe(200);
    expect(partly.json()).toMatchObject({ members: [], invites: [], refreshError: expect.stringMatching(/Couldn't read the list again: boom/) });
    expect(env.mekoApi.calls.length).toBeGreaterThan(handler);

    // Meko refuses the key: an error that says to check it.
    shareStatus = 401;
    const keyRefused = await call('POST', '/api/meko/datapacks/dp-1/people', { email: 'carol@example.com', grant: 'viewer' });
    expect(keyRefused.statusCode).toBe(502);
    expect(keyRefused.json().error).toBe("Meko: AgentK's API key wasn't accepted (invalid or expired token). Check the key on the Meko tab in Settings.");
  });

  it('invites a list of emails one after another and says who went through', async () => {
    await project();
    /** Meko's answer to /share per address; anything not here is invited. */
    let refuse: Record<string, number> = {};
    env.mekoApi.handle((c: ApiCall) => {
      if (c.path === '/datapacks/incoming-shares') return { body: { data: [] } };
      if (c.path.endsWith('/member-list')) {
        return { body: { data: { members: [
          { is_owner: true, user_name: 'Me', user_email_id: 'me@example.com', grant: 'owner' },
          { is_owner: false, user_name: 'Ann', user_email_id: 'A@example.com', grant: 'viewer' },
        ] } } };
      }
      if (c.path.endsWith('/outgoing-shares')) return { body: { data: { pending_invites: [] } } };
      if (c.path.endsWith('/share')) {
        const to = (c.body as { recipient_email: string }).recipient_email;
        const status = refuse[to];
        if (status === 0) throw new Error('connect ECONNREFUSED');
        if (status === undefined) return { body: { data: { status: 'pending', invite_id: `i-${to}` } } };
        return status === 401
          ? { status, body: { error: { code: 'UNAUTHORIZED', message: 'invalid or expired token' } } }
          : { status, body: { error: { code: 'CONFLICT', message: `an invite for ${to} is already pending` } } };
      }
      return { body: { data: {} } };
    });
    const shares = () => env.mekoApi.calls.filter((c) => c.path === '/datapacks/dp-1/share').map((c) => c.body);
    const lists = () => env.mekoApi.calls.filter((c) => c.path.endsWith('/member-list')).length;
    const post = (body: object) => call('POST', '/api/meko/datapacks/dp-1/people', body);

    // Every one is invited, in order, at the grant asked for, with repeats sent once; the lists are read once.
    const before = lists();
    const all3 = await post({ emails: ['a@example.com', ' b@example.com ', 'c@example.com', 'A@example.com'], grant: 'maintainer' });
    expect(all3.statusCode).toBe(200);
    expect(all3.json()).toMatchObject({ invited: ['a@example.com', 'b@example.com', 'c@example.com'], failed: [], members: [{ email: 'me@example.com' }, { email: 'A@example.com' }] });
    expect(shares()).toEqual(['a', 'b', 'c'].map((n) => ({ recipient_email: `${n}@example.com`, grant: 'maintainer' })));
    expect(lists() - before).toBe(1);

    // One refused: the rest still go through, and it says which one didn't and why.
    refuse = { 'e@example.com': 409 };
    const partly = await post({ emails: ['d@example.com', 'e@example.com', 'f@example.com'], grant: 'viewer' });
    expect(partly.statusCode).toBe(200);
    expect(partly.json()).toMatchObject({
      invited: ['d@example.com', 'f@example.com'],
      failed: [{ email: 'e@example.com', error: 'Meko: an invite for e@example.com is already pending' }],
    });

    // None went through: the first error, as for one email.
    refuse = { 'g@example.com': 409, 'h@example.com': 409 };
    const none = await post({ emails: ['g@example.com', 'h@example.com'], grant: 'viewer' });
    expect(none.statusCode).toBe(409);
    expect(none.json().error).toBe('Meko: an invite for g@example.com is already pending');

    // A refused key stops the list: the rest aren't sent.
    refuse = { 'j@example.com': 401, 'k@example.com': 401 };
    let sent = shares().length;
    const keyRefused = await post({ emails: ['j@example.com', 'k@example.com'], grant: 'viewer' });
    expect(keyRefused.statusCode).toBe(502);
    expect(keyRefused.json().error).toMatch(/AgentK's API key wasn't accepted/);
    expect(shares().length - sent).toBe(1);
    // …and after some went through, the rest are listed as failed with that error.
    refuse = { 'm@example.com': 401, 'n@example.com': 401 };
    sent = shares().length;
    const keyLater = await post({ emails: ['l@example.com', 'm@example.com', 'n@example.com'], grant: 'viewer' });
    expect(keyLater.statusCode).toBe(200);
    expect(keyLater.json().invited).toEqual(['l@example.com']);
    expect(keyLater.json().failed.map((f: { email: string }) => f.email)).toEqual(['m@example.com', 'n@example.com']);
    expect(shares().length - sent).toBe(2);
    // So does an unreachable Meko (each try could wait out a timeout), first or partway through.
    refuse = { 'o@example.com': 0, 'q@example.com': 0, 'r@example.com': 0 };
    sent = shares().length;
    const down = await post({ emails: ['o@example.com', 'q@example.com'], grant: 'viewer' });
    expect(down.statusCode).toBe(502);
    expect(down.json().error).toMatch(/Couldn't reach Meko's API at .*: connect ECONNREFUSED/);
    expect(shares().length - sent).toBe(1);
    sent = shares().length;
    const downLater = await post({ emails: ['p@example.com', 'q@example.com', 'r@example.com'], grant: 'viewer' });
    expect(downLater.statusCode).toBe(200);
    expect(downLater.json().invited).toEqual(['p@example.com']);
    expect(downLater.json().failed.map((f: { email: string }) => f.email)).toEqual(['q@example.com', 'r@example.com']);
    expect(shares().length - sent).toBe(2);

    // Checked before anything is sent.
    sent = shares().length;
    const bad = await post({ emails: ['p@example.com', 'nope', ''], grant: 'viewer' });
    expect(bad.statusCode).toBe(400);
    expect(bad.json().error).toBe('Not an email: nope, (blank). Fix or remove them.');
    const many = Array.from({ length: 51 }, (_, i) => `u${i}@example.com`);
    expect((await post({ emails: many, grant: 'viewer' })).json().error).toBe('Invite at most 50 people at a time.');
    expect((await post({ emails: [], grant: 'viewer' })).statusCode).toBe(400);
    expect((await post({ emails: 'p@example.com', grant: 'viewer' })).statusCode).toBe(400);
    expect((await post({ email: 'p@example.com', emails: ['q@example.com'], grant: 'viewer' })).statusCode).toBe(400);
    expect((await post({ grant: 'viewer' })).statusCode).toBe(400);
    expect((await post({ emails: ['p@example.com', 'q@example.com'], grant: 'viewer', login: 'p' })).statusCode).toBe(400);
    expect((await post({ emails: ['p@example.com'], grant: 'owner' })).json().error).toBe('Access must be viewer, contributor or maintainer.');
    expect(shares().length).toBe(sent);

    // Only the addresses invited are kept to suggest, named once Meko lists them as members; one can be forgotten.
    const contacts = async () => (await call('GET', '/api/share-contacts')).json() as { email: string; name: string | null; shares: number }[];
    expect((await contacts()).map((c) => c.email).sort()).toEqual(['a', 'b', 'c', 'd', 'f', 'l', 'p'].map((n) => `${n}@example.com`));
    expect((await contacts()).find((c) => c.email === 'a@example.com')).toMatchObject({ name: 'Ann', shares: 1 });
    expect((await call('DELETE', '/api/share-contacts/B%40example.com')).statusCode).toBe(204);
    expect((await call('DELETE', '/api/share-contacts/b%40example.com')).statusCode).toBe(404);
    expect((await contacts()).map((c) => c.email)).not.toContain('b@example.com');
  });

  it.each([
    ['a 403', { status: 403, body: { error: { code: 'FORBIDDEN', message: 'owner or maintainer only' } } }, 'permission'],
    ['a 405', { status: 405, body: { error: { code: 'METHOD_NOT_ALLOWED', message: 'not allowed' } } }, 'unsupported'],
    ['a 404 with no error code', { status: 404 }, 'unsupported'],
  ])('says why a datapack has no share link when Meko answers %s', async (_, reply, unavailable) => {
    await project();
    env.mekoApi.handle((c: ApiCall) => (c.path === '/datapacks/dp-1/share-link' ? reply : { body: { data: [] } }));
    expect((await call('GET', '/api/meko/datapacks/dp-1/share-link')).json()).toEqual({ link: null, unavailable });
  });

  it('turns a share link on with the role picked, changes and resets it, and never logs its token', async () => {
    expect((await call('GET', '/api/meko/datapacks/dp-1/share-link')).statusCode).toBe(404);
    await project();
    let link = { status: 'off', token: null as string | null, grant: 'viewer', redemption_count: 0, members_at_link_role: 0 };
    env.mekoApi.handle((c: ApiCall) => {
      if (c.path !== '/datapacks/dp-1/share-link') return { body: { data: [] } };
      const b = (c.body ?? {}) as { status?: string; grant?: string; reset?: boolean };
      if (b.status) link = { ...link, status: b.status, token: link.token ?? 'tok-1' };
      if (b.grant) link = { ...link, grant: b.grant };
      if (b.reset) link = { ...link, token: 'tok-2' };
      return { body: { data: { share_link: link } } };
    });
    const info = vi.spyOn(env.core.log, 'info');
    const url = '/api/meko/datapacks/dp-1/share-link';
    expect((await call('GET', url)).json()).toEqual({ link: { active: false, grant: 'viewer', url: null, joined: 0, atLinkRole: 0 } });
    // One change at a time; turning on names the role, and a link can't make a maintainer.
    for (const bad of [{}, { active: true }, { active: false, reset: true }, { grant: 'maintainer' }, { reset: false }]) {
      expect((await call('PUT', url, bad)).statusCode).toBe(400);
    }
    const puts = () => env.mekoApi.calls.filter((c) => c.method === 'PUT').map((c) => c.body);
    expect(puts()).toEqual([]);
    // Meko keeps viewer, so the role picked follows.
    expect((await call('PUT', url, { active: true, grant: 'contributor' })).json())
      .toEqual({ active: true, grant: 'contributor', url: 'https://cloud.mekodata.ai/share/tok-1', joined: 0, atLinkRole: 0 });
    expect((await call('PUT', url, { active: false })).json()).toMatchObject({ active: false, url: null });
    // On again with the role it has: one call.
    await call('PUT', url, { active: true, grant: 'contributor' });
    expect(puts()).toEqual([{ status: 'active' }, { grant: 'contributor' }, { status: 'off' }, { status: 'active' }]);
    expect((await call('PUT', url, { grant: 'viewer' })).json().grant).toBe('viewer');
    expect((await call('PUT', url, { reset: true })).json().url).toBe('https://cloud.mekodata.ai/share/tok-2');
    expect(JSON.stringify(info.mock.calls)).not.toMatch(/tok-/);
    // A Meko whose API URL is set but not its web app's: no link to build.
    env.core.updateConfig((c) => {
      c.meko.url = 'https://meko.example.com/mcp';
      c.meko.apiUrl = 'https://meko.example.com/api/v1';
    });
    expect((await call('GET', url)).json()).toEqual({ link: null, unavailable: 'no-web-url' });
    expect((await call('PUT', url, { reset: true })).statusCode).toBe(409);
  });

  it('joins by a share link from our Meko only, and offers the datapack as a project to open', async () => {
    await project();
    const rows: MekoDatapackRow[] = [
      { id: 'dp-own', name: 'x', grant: 'owner', createdAt: null, ownerName: null, ownerEmail: null, description: null, ownerUserId: 'u-self' },
    ];
    (env.meko as { listDatapacks: unknown }).listDatapacks = async () => rows;
    const shared = (id: string, grant: string): MekoDatapackRow =>
      ({ id, name: `adlc/proj/demo-repo/${id}`, grant, createdAt: null, ownerName: 'Alice', ownerEmail: 'a@example.com', description: null, ownerUserId: 'u-a' });
    const links: Record<string, { status?: number; body: unknown; row?: MekoDatapackRow }> = {
      'tok-a': { body: { data: { datapack_id: 'dp-a', grant: 'contributor', already_member: false } }, row: shared('dp-a', 'contributor') },
      'tok-v': { body: { data: { datapack_id: 'dp-v', grant: 'viewer', already_member: false } }, row: shared('dp-v', 'viewer') },
      'tok-p': { body: { data: { datapack_id: 'dp-1', grant: 'contributor', already_member: true } } },
      'tok-m': { body: { data: { datapack_id: 'dp-m', grant: 'viewer', already_member: true } }, row: shared('dp-m', 'maintainer') },
      'tok-old': { status: 404, body: undefined },
      'tok-off': { status: 404, body: { error: { code: 'NOT_FOUND', message: 'share link not found' } } },
      'tok-gone': { status: 403, body: { error: { code: 'REMOVED_FROM_DATAPACK', message: 'removed' } } },
    };
    env.mekoApi.handle((c: ApiCall) => {
      const token = /^\/share\/([^/]+)\/redeem$/.exec(c.path)?.[1];
      if (!token) return { body: { data: [] } };
      const l = links[token];
      if (l.row && !rows.includes(l.row)) rows.push(l.row);
      return { status: l.status, body: l.body };
    });
    const notify = vi.spyOn(env.core, 'notifyInvite');
    const join = (url: string) => call('POST', '/api/meko/share-links/join', { url });
    const redeems = () => env.mekoApi.calls.filter((c) => c.path.startsWith('/share/')).length;

    // A look-alike link's token never reaches Meko.
    for (const url of ['https://evil.example/share/tok-a', 'https://cloud.mekodata.ai/datapacks/tok-a', '']) expect((await join(url)).statusCode).toBe(400);
    expect(redeems()).toBe(0);
    // The first sync records the shares there already quietly, but one joined here is offered for
    // Open project, with no banner or notification: the person who joined knows.
    const a = await join('https://cloud.mekodata.ai/share/tok-a');
    expect(a.json()).toMatchObject({
      datapackId: 'dp-a', name: 'adlc/proj/demo-repo/dp-a', grant: 'contributor', alreadyMember: false, project: null,
      invite: { datapackId: 'dp-a', state: 'later' },
    });
    expect(notify).not.toHaveBeenCalled();
    expect((await call('GET', '/api/state')).json().invites).toEqual([]);
    // A maintainer who pastes the datapack's viewer link keeps their own access.
    expect((await join('https://cloud.mekodata.ai/share/tok-m')).json()).toMatchObject({ grant: 'maintainer', invite: { datapackId: 'dp-m' } });
    // A viewer can't open it as a project; a datapack a project here uses opens that project.
    expect((await join('https://cloud.mekodata.ai/share/tok-v')).json()).toMatchObject({ grant: 'viewer', invite: null, project: null });
    expect((await join('https://cloud.mekodata.ai/share/tok-p')).json()).toMatchObject({ alreadyMember: true, invite: null, project: { name: 'p' } });
    const off = await join('https://cloud.mekodata.ai/share/tok-off');
    expect([off.statusCode, off.json().error]).toEqual([404, expect.stringMatching(/turned it off or reset it/)]);
    // A Meko without the redeem route isn't a link to ask the owner about again.
    expect((await join('https://cloud.mekodata.ai/share/tok-old')).json().error).toBe("This Meko doesn't have share links.");
    const gone = await join('https://cloud.mekodata.ai/share/tok-gone');
    expect([gone.statusCode, gone.json().error]).toEqual([403, expect.stringMatching(/You were removed from this datapack/)]);
  });

  it('lists invites in the state and puts one off with Later', async () => {
    const rows: MekoDatapackRow[] = [
      { id: 'dp-own', name: 'x', grant: 'owner', createdAt: null, ownerName: null, ownerEmail: null, description: null, ownerUserId: 'u-self' },
    ];
    let lists = 0;
    (env.meko as { listDatapacks: unknown }).listDatapacks = async () => {
      lists++;
      return rows;
    };
    expect((await call('POST', '/api/invites/check')).json().invites).toEqual([]); // the first sync: baseline
    rows.push({ id: 'dp-a', name: 'adlc/proj/demo-repo/a', grant: 'maintainer', createdAt: null, ownerName: 'Alice', ownerEmail: 'a@example.com', description: null, ownerUserId: 'u-a' });
    const checked = await call('POST', '/api/invites/check');
    expect(checked.json().invites.map((i: { datapackId: string; state: string }) => [i.datapackId, i.state])).toEqual([['dp-a', 'shared']]);
    // The same one read of Meko gives the dialog its datapack list, without account ids.
    expect(checked.json().datapacks.map((d: { id: string }) => d.id)).toEqual(['dp-own', 'dp-a']);
    expect(checked.json().datapacks[0]).not.toHaveProperty('ownerUserId');
    const before = lists;
    await call('POST', '/api/invites/check');
    expect(lists).toBe(before + 1);
    expect((await call('GET', '/api/state')).json().invites).toHaveLength(1);
    expect((await call('POST', '/api/invites/dp-a/later')).statusCode).toBe(204);
    expect((await call('GET', '/api/state')).json().invites).toEqual([]);
    // An accepted share has no invite left to decline.
    expect((await call('POST', '/api/invites/dp-a/decline')).statusCode).toBe(409);
    // The datapack list the dialog fetches never carries account ids.
    const list = (await call('GET', '/api/meko/datapacks')).json();
    expect(list[0]).not.toHaveProperty('ownerUserId');
  });

  it("shows the Meko settings without the key, and checks a change before saving it", async () => {
    env.core.updateConfig((c) => {
      c.meko.apiKey = 'mko_tkn_abcdefghij1234';
    });
    const s = (await call('GET', '/api/settings')).json();
    expect(s.meko).toMatchObject({ url: '', effectiveUrl: 'https://mcp.mekodata.ai/mcp', keyHint: '1234', keySource: 'config', effectiveApiUrl: 'https://cloud.mekodata.ai/api/v1', fallbackUrl: 'https://mcp.mekodata.ai/mcp', envWebUrl: '' });
    expect(JSON.stringify(s)).not.toContain('abcdefghij');

    expect((await call('PUT', '/api/settings', { meko: { url: 'ftp://x' } })).statusCode).toBe(400);
    env.mekoTools.state.checkError = 'invalid or expired token';
    const bad = await call('PUT', '/api/settings', { meko: { url: 'https://meko.example.com/mcp', apiKey: 'mko_tkn_newkey000000' } });
    expect(bad.statusCode).toBe(400);
    expect(bad.json().error).toMatch(/didn't accept that key: invalid or expired token. Nothing was saved/);
    expect(env.core.config.meko.url).toBe('');
    const onDisk = existsSync(env.core.paths.config) ? readFileSync(env.core.paths.config, 'utf8') : '';
    expect(onDisk).not.toContain('meko.example.com');

    env.mekoTools.state.checkError = null;
    const before = env.meko.calls.filter((c) => c.name === 'reset').length;
    const good = await call('PUT', '/api/settings', { meko: { url: 'https://meko.example.com/mcp', apiKey: 'mko_tkn_newkey000000' } });
    expect(good.statusCode).toBe(200);
    expect(good.json().meko).toMatchObject({ url: 'https://meko.example.com/mcp', keyHint: '0000', effectiveWebUrl: null, fallbackUrl: 'https://mcp.mekodata.ai/mcp' });
    expect(env.mekoTools.calls.find((c) => c.name === 'check')?.args).toEqual(['https://meko.example.com/mcp', 'mko_tkn_newkey000000']);
    expect(env.meko.calls.filter((c) => c.name === 'reset').length).toBe(before + 1);
    expect(readFileSync(env.core.paths.config, 'utf8')).toContain('meko.example.com/mcp');
    // A self-hosted MCP URL has no web app to work out until one is typed, and no API URL to check
    // (or share through).
    expect(env.core.state().meko).toMatchObject({ webUrl: null });
    expect(env.mekoTools.calls.filter((c) => c.name === 'checkApi')).toEqual([]);

    // The saved key goes only where it already goes: a new server takes it typed again, and gets nothing meanwhile.
    const sent = env.mekoTools.calls.length;
    const elsewhere = await call('PUT', '/api/settings', { meko: { url: 'https://other.example.org/mcp' } });
    expect(elsewhere.statusCode).toBe(400);
    expect(elsewhere.json().error).toBe('Enter the Meko API key again to use it with https://other.example.org: AgentK sends a saved key only to a Meko server it already uses. Nothing was saved.');
    expect(env.mekoTools.calls.length).toBe(sent);
    // Nor alongside a switch to the Meko on this computer.
    const local = await call('PUT', '/api/settings', { meko: { useLocal: true, url: 'https://other.example.org/mcp' } });
    expect(local.statusCode).toBe(400);
    expect(env.core.config.meko).toMatchObject({ url: 'https://meko.example.com/mcp' });
    expect(env.mekoTools.calls.length).toBe(sent);

    // Back on Meko Cloud, its web app and API URLs are worked out and trusted: the API URL isn't checked.
    const cloud = await call('PUT', '/api/settings', { meko: { url: 'https://mcp.mekodata.ai/mcp' } });
    expect(cloud.statusCode).toBe(200);
    expect(cloud.json().meko).toMatchObject({ url: '', effectiveWebUrl: 'https://cloud.mekodata.ai', effectiveApiUrl: 'https://cloud.mekodata.ai/api/v1' });
    expect(env.mekoTools.calls.filter((c) => c.name === 'checkApi')).toEqual([]);
    expect(env.core.state().meko).toMatchObject({ webUrl: 'https://cloud.mekodata.ai' });

    // A typed web app URL moves the API URL: that is checked (without the key), and nothing reconnects.
    // On another server, the key the API is sent is typed again.
    const checks = env.mekoTools.calls.length;
    const resets = env.meko.calls.filter((c) => c.name === 'reset').length;
    expect((await call('PUT', '/api/settings', { meko: { webUrl: 'https://meko.example.com/' } })).statusCode).toBe(400);
    expect((await call('PUT', '/api/settings', { meko: { webUrl: 'https://meko.example.com/', apiKey: 'mko_tkn_newkey000000' } })).json().meko.effectiveWebUrl).toBe('https://meko.example.com');
    expect(env.mekoTools.calls.slice(checks)).toEqual([{ name: 'checkApi', args: ['https://meko.example.com/api/v1'] }]);
    expect(env.meko.calls.filter((c) => c.name === 'reset').length).toBe(resets);

    // A typed API URL that doesn't answer like Meko's API isn't saved.
    env.mekoTools.state.apiError = "it answered 404, not like Meko's API";
    const badApi = await call('PUT', '/api/settings', { meko: { apiUrl: 'https://typo.example.com/api/v1', apiKey: 'mko_tkn_newkey000000' } });
    expect(badApi.statusCode).toBe(400);
    expect(badApi.json().error).toMatch(/doesn't look like Meko's API: it answered 404/);
    expect(env.core.config.meko.apiUrl).toBe('');

    // A good one is saved, and the invites waiting there are synced.
    env.mekoTools.state.apiError = null;
    env.mekoApi.handle((c) => (c.path === '/datapacks/incoming-shares' ? { body: { data: [] } } : undefined));
    const synced = env.mekoApi.calls.filter((c) => c.path === '/datapacks/incoming-shares').length;
    expect((await call('PUT', '/api/settings', { meko: { apiUrl: 'https://api.example.com/api/v1', apiKey: 'mko_tkn_newkey000000' } })).statusCode).toBe(200);
    await waitFor(() => env.mekoApi.calls.filter((c) => c.path === '/datapacks/incoming-shares').length, (n) => n > synced, 5000, 'the invite sync');
  });

  it('saves only the Meko fields it changed, over a change made while it checked', async () => {
    env.core.updateConfig((c) => {
      c.meko.apiKey = 'mko_tkn_abcdefghij1234';
    });
    // While the web app URL is being checked, Claude Code's setup clears the key in config.toml.
    const checkApi = env.mekoTools.tools.checkApi;
    env.mekoTools.tools.checkApi = async (u) => {
      env.core.updateConfig((c) => {
        c.meko.apiKey = '';
      });
      return checkApi(u);
    };
    expect((await call('PUT', '/api/settings', { meko: { webUrl: 'https://cloud.mekodata.ai/eu' } })).statusCode).toBe(200);
    expect(env.core.config.meko).toMatchObject({ webUrl: 'https://cloud.mekodata.ai/eu', apiKey: '' });
  });

  it("shows when Claude Code's Meko differs, and changes Claude Code only when asked", async () => {
    const dir = process.env.CLAUDE_CONFIG_DIR!;
    /** Claude Code's Meko as Meko's setup writes it, on this server with this token. */
    const writeClaude = (url: string, token: string) => {
      mkdirSync(join(dir, 'plugins', 'p'), { recursive: true });
      writeFileSync(join(dir, 'plugins', 'installed_plugins.json'), JSON.stringify({ plugins: { 'meko-agent-skills@m': [{ installPath: join(dir, 'plugins', 'p') }] } }));
      writeFileSync(join(dir, 'settings.json'), JSON.stringify({ enabledPlugins: { 'meko-agent-skills@m': true }, env: { MEKO_MCP_URL: url, MEKO_API_KEY: token } }));
      const cfgFile = join(dir, '.claude.json');
      const cfg = existsSync(cfgFile) ? JSON.parse(readFileSync(cfgFile, 'utf8')) : {};
      writeFileSync(cfgFile, JSON.stringify({ ...cfg, mcpServers: { meko: { type: 'http', url, headers: { Authorization: `Bearer ${token}` } } } }));
    };
    await project();
    env.core.updateConfig((c) => {
      c.meko.apiKey = 'mko_tkn_abcdefghij1234';
    });
    // Claude Code is on another Meko (and installed: checked only once its version is in).
    writeClaude('https://other.example.org/mcp', 'claude-token');
    await env.core.harnesses.of('claude').version(env.core.config);
    await env.core.claudeMeko.refresh();
    expect(env.core.state().meko.claude).toEqual({
      gaps: [
        "Claude Code's settings point at other.example.org, not mcp.mekodata.ai.",
        "Claude Code's meko MCP server points at other.example.org, not mcp.mekodata.ai.",
      ],
      unchecked: [],
      checking: false,
    });

    // A token Meko turns down.
    writeClaude('https://mcp.mekodata.ai/mcp', 'claude-token');
    await env.core.claudeMeko.refresh();
    expect(env.core.state().meko.claude.gaps).toEqual([
      "The token in Claude Code's settings isn't accepted by mcp.mekodata.ai.",
      "The token on Claude Code's meko MCP server isn't accepted by mcp.mekodata.ai.",
    ]);

    // On this Meko, another account's token can't see the project's datapack, which AgentK's can.
    env.meko.listDatapacks = async () => [
      { id: 'dp-1', name: 'adlc/proj/demo-repo/p', grant: 'owner', createdAt: null, ownerName: null, ownerEmail: null, description: null, ownerUserId: 'u-self' },
    ];
    env.mekoTools.state.tokens['claude-token'] = { ok: true, datapacks: ['dp-other'] };
    writeClaude('https://mcp.mekodata.ai/mcp', 'claude-token');
    await env.core.claudeMeko.refresh(true);
    expect(env.core.state().meko.claude.gaps).toEqual([
      "The token in Claude Code's settings can't see the datapack adlc/proj/demo-repo/p on mcp.mekodata.ai.",
      "The token on Claude Code's meko MCP server can't see the datapack adlc/proj/demo-repo/p on mcp.mekodata.ai.",
    ]);
    // A different token that sees it is fine.
    env.mekoTools.state.tokens['claude-token'] = { ok: true, datapacks: ['dp-1'] };
    await env.core.claudeMeko.refresh(true);
    expect(env.core.state().meko.claude.gaps).toEqual([]);

    // Moving AgentK to another Meko shows the difference; nothing changes Claude Code.
    expect((await call('PUT', '/api/settings', { meko: { url: 'https://meko.example.com/mcp', apiKey: 'mko_tkn_abcdefghij1234' } })).statusCode).toBe(200);
    await env.core.claudeMeko.refresh();
    expect(env.core.state().meko.claude.gaps).toEqual([
      "Claude Code's settings point at mcp.mekodata.ai, not meko.example.com.",
      "Claude Code's meko MCP server points at mcp.mekodata.ai, not meko.example.com.",
    ]);
    expect(env.mekoTools.calls.filter((c) => c.name === 'setUpClaude')).toEqual([]);

    // Asked to, the setup runs once, and the check after it finds nothing left.
    const setUp = env.mekoTools.tools.setUpClaude;
    env.mekoTools.tools.setUpClaude = async (url, apiKey, log, claude) => {
      await setUp(url, apiKey, log, claude);
      writeClaude(url, apiKey);
    };
    expect((await call('POST', '/api/settings/meko/claude-setup')).statusCode).toBe(200);
    expect(env.mekoTools.calls.filter((c) => c.name === 'setUpClaude')).toEqual([{ name: 'setUpClaude', args: ['https://meko.example.com/mcp', 'mko_tkn_abcdefghij1234'] }]);
    // The check after it runs in the background; the state stream carries what it finds.
    await waitFor(() => env.core.state().meko.claude, (c) => !c.gaps.length && !c.checking, 5000, "the check after Claude Code's setup");
    expect((await call('GET', '/api/settings')).json().meko).not.toHaveProperty('claudeUrl');

    // A setup that doesn't finish is reported, with what it printed last.
    env.mekoTools.state.setupError = "Meko's setup didn't finish";
    const failed = await call('POST', '/api/settings/meko/claude-setup');
    expect(failed.statusCode).toBe(502);
    expect(failed.json().error).toMatch(/didn't finish. Last output: setting up/);
  });
});
