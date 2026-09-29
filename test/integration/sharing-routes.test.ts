import { existsSync, readFileSync } from 'node:fs';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { buildServer } from '../../src/server/server.js';
import { issueSetupToken } from '../../src/server/auth.js';
import type { MekoDatapackRow } from '../../src/server/meko.js';
import { makeEnv, waitFor, type ApiCall, type TestEnv } from '../helpers.js';

describe('HTTP API: sharing, invites and Meko settings', () => {
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

  const call = (method: 'GET' | 'POST' | 'PUT' | 'DELETE', url: string, payload?: unknown) =>
    app.inject({ method, url, headers: { cookie }, ...(payload !== undefined ? { payload: payload as object } : {}) });

  async function project(dpId = 'dp-1') {
    return env.core.projects.create({ repoPath: env.repo, name: 'p', datapack: { mode: 'existing', id: dpId, name: 'adlc/proj/demo-repo/p' } });
  }

  it('keeps every new route behind the login', async () => {
    for (const [method, url] of [
      ['GET', '/api/meko/datapacks/dp-1/people'],
      ['POST', '/api/meko/datapacks/dp-1/people'],
      ['POST', '/api/meko/datapacks/dp-1/people/invites/i1/resend'],
      ['DELETE', '/api/meko/datapacks/dp-1/people/invites/i1'],
      ['POST', '/api/invites/check'],
      ['POST', '/api/invites/dp-1/accept'],
      ['POST', '/api/invites/dp-1/decline'],
      ['POST', '/api/invites/dp-1/later'],
      ['POST', '/api/settings/meko/claude-setup'],
    ] as const) {
      expect((await app.inject({ method, url })).statusCode, `${method} ${url}`).toBe(401);
    }
  });

  it("shares only datapacks in a project here, only in API mode, and passes on Meko's errors", async () => {
    expect((await call('GET', '/api/meko/datapacks/dp-1/people')).statusCode).toBe(404);
    await project();
    const link = await call('GET', '/api/meko/datapacks/dp-1/people');
    expect(link.statusCode).toBe(409);
    expect(link.json().error).toMatch(/Share it in Meko/);

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
    await env.core.invites.checkMode();

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

    // Meko stops taking the key: link mode from then on.
    shareStatus = 401;
    const keyRefused = await call('POST', '/api/meko/datapacks/dp-1/people', { email: 'carol@example.com', grant: 'viewer' });
    expect(keyRefused.statusCode).toBe(409);
    expect(keyRefused.json().error).toMatch(/doesn't accept AgentK's key for sharing/);
    expect(env.core.mekoStatus.sharing).toBe('link');
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
    // Link mode: no invites to answer here.
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
    expect(s.meko).toMatchObject({ url: '', effectiveUrl: 'https://mcp.mekodata.ai/mcp', keyHint: '1234', keySource: 'config', effectiveApiUrl: 'https://cloud.mekodata.ai/api/v1' });
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
    expect(good.json().meko).toMatchObject({ url: 'https://meko.example.com/mcp', keyHint: '0000', effectiveWebUrl: null });
    expect(env.mekoTools.calls.find((c) => c.name === 'check')?.args).toEqual(['https://meko.example.com/mcp', 'mko_tkn_newkey000000']);
    expect(env.meko.calls.filter((c) => c.name === 'reset').length).toBe(before + 1);
    expect(readFileSync(env.core.paths.config, 'utf8')).toContain('meko.example.com/mcp');
    // A self-hosted MCP URL has no web app to work out until one is typed, and no API URL to check.
    expect(env.core.state().meko.webUrl).toBeNull();
    expect(env.mekoTools.calls.filter((c) => c.name === 'checkApi')).toEqual([]);

    // Back on Meko Cloud, its web app and API URLs are worked out and trusted: the API URL isn't checked.
    const cloud = await call('PUT', '/api/settings', { meko: { url: 'https://mcp.mekodata.ai/mcp' } });
    expect(cloud.statusCode).toBe(200);
    expect(cloud.json().meko).toMatchObject({ url: '', effectiveWebUrl: 'https://cloud.mekodata.ai', effectiveApiUrl: 'https://cloud.mekodata.ai/api/v1' });
    expect(env.mekoTools.calls.filter((c) => c.name === 'checkApi')).toEqual([]);
    expect(env.core.state().meko.webUrl).toBe('https://cloud.mekodata.ai');

    // A typed web app URL moves the API URL: that is checked (without the key), and nothing reconnects.
    const checks = env.mekoTools.calls.length;
    const resets = env.meko.calls.filter((c) => c.name === 'reset').length;
    expect((await call('PUT', '/api/settings', { meko: { webUrl: 'https://meko.example.com/' } })).json().meko.effectiveWebUrl).toBe('https://meko.example.com');
    expect(env.mekoTools.calls.slice(checks)).toEqual([{ name: 'checkApi', args: ['https://meko.example.com/api/v1'] }]);
    expect(env.meko.calls.filter((c) => c.name === 'reset').length).toBe(resets);

    // A typed API URL that doesn't answer like Meko's API isn't saved.
    env.mekoTools.state.apiError = "it answered 404, not like Meko's API";
    const badApi = await call('PUT', '/api/settings', { meko: { apiUrl: 'https://typo.example.com/api/v1' } });
    expect(badApi.statusCode).toBe(400);
    expect(badApi.json().error).toMatch(/doesn't look like Meko's API: it answered 404/);
    expect(env.core.config.meko.apiUrl).toBe('');

    // A good one is saved, and the sharing mode is checked again.
    env.mekoTools.state.apiError = null;
    env.mekoApi.handle((c) => (c.path === '/datapacks/incoming-shares' ? { body: { data: [] } } : undefined));
    expect((await call('PUT', '/api/settings', { meko: { apiUrl: 'https://api.example.com/api/v1' } })).statusCode).toBe(200);
    await waitFor(() => env.core.mekoStatus.sharing, (m) => m === 'api', 5000, 'the sharing mode');
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
    expect((await call('PUT', '/api/settings', { meko: { webUrl: 'https://meko.example.com' } })).statusCode).toBe(200);
    expect(env.core.config.meko).toMatchObject({ webUrl: 'https://meko.example.com', apiKey: '' });
  });

  it("runs Meko's setup for Claude Code and reports when it doesn't finish", async () => {
    env.core.updateConfig((c) => {
      c.meko.apiKey = 'mko_tkn_abcdefghij1234';
    });
    expect((await call('POST', '/api/settings/meko/claude-setup')).statusCode).toBe(200);
    expect(env.mekoTools.calls.find((c) => c.name === 'setUpClaude')?.args).toEqual(['https://mcp.mekodata.ai/mcp', 'mko_tkn_abcdefghij1234']);
    env.mekoTools.state.setupError = "Meko's setup didn't finish";
    const failed = await call('POST', '/api/settings/meko/claude-setup');
    expect(failed.statusCode).toBe(502);
    expect(failed.json().error).toMatch(/didn't finish. Last output: setting up/);
  });
});
