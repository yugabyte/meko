import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { makeEnv, type TestEnv } from '../helpers.js';
import { buildServer } from '../../src/server/server.js';
import { issueSetupToken, totpCode } from '../../src/server/auth.js';

describe('HTTP API: auth, origin checks, projects', () => {
  let env: TestEnv;
  let app: FastifyInstance;
  beforeEach(async () => {
    env = await makeEnv();
    app = await buildServer(env.core, { tls: null, webDir: null, updater: null });
  });
  afterEach(async () => {
    await app.close();
    await env.cleanup();
  });

  const cookieOf = (res: { headers: Record<string, unknown> }) => String(res.headers['set-cookie']).split(';')[0];

  async function setup(): Promise<string> {
    const token = issueSetupToken(env.core.db);
    const res = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me', password: 'correct horse battery' } });
    expect(res.statusCode).toBe(204);
    return cookieOf(res);
  }

  it('requires setup, then login; rejects reuse of a setup token', async () => {
    const st = await app.inject({ url: '/api/auth/status' });
    expect(st.json()).toMatchObject({ setupRequired: true, loggedIn: false });
    expect((await app.inject({ url: '/api/state' })).statusCode).toBe(401);

    const token = issueSetupToken(env.core.db);
    const weak = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me', password: 'short' } });
    expect(weak.statusCode).toBe(400);
    const ok = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me', password: 'correct horse battery' } });
    expect(ok.statusCode).toBe(204);
    const cookie = cookieOf(ok);
    expect(String(ok.headers['set-cookie'])).toMatch(/HttpOnly/i);
    expect(String(ok.headers['set-cookie'])).toMatch(/SameSite=Strict/i);
    const again = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'x', password: 'correct horse battery' } });
    expect(again.statusCode).toBe(403);

    const state = await app.inject({ url: '/api/state', headers: { cookie } });
    expect(state.statusCode).toBe(200);
    expect(state.json().version).toBe('0.0.0-test');
    expect(state.json().meko.webUrl).toBe('https://cloud.mekodata.ai');

    const bad = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'nope' } });
    expect(bad.statusCode).toBe(401);
    const good = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery' } });
    expect(good.statusCode).toBe(204);
  });

  it('rate limits failed logins', async () => {
    await setup();
    for (let i = 0; i < 5; i++) {
      await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'wrong' } });
    }
    const res = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery' } });
    expect(res.statusCode).toBe(429);
  });

  it('enforces TOTP once enrolled', async () => {
    const cookie = await setup();
    const enroll = await app.inject({ method: 'POST', url: '/api/auth/totp/enroll', headers: { cookie } });
    const { secret, qrSvg } = enroll.json();
    expect(qrSvg).toContain('<svg');
    const confirm = await app.inject({ method: 'POST', url: '/api/auth/totp/confirm', headers: { cookie }, payload: { code: totpCode(secret) } });
    expect(confirm.statusCode).toBe(204);
    const noCode = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery' } });
    expect(noCode.statusCode).toBe(401);
    expect(noCode.json().totpRequired).toBe(true);
    const withCode = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery', totp: totpCode(secret) } });
    expect(withCode.statusCode).toBe(204);
  });

  it('refuses cross-origin writes', async () => {
    const cookie = await setup();
    const res = await app.inject({
      method: 'POST', url: '/api/projects', headers: { cookie, origin: 'https://evil.example', host: 'devbox:7777' },
      payload: {},
    });
    expect(res.statusCode).toBe(403);
  });

  it('creates a project and a session through the API', async () => {
    const cookie = await setup();
    const repos = (await app.inject({ url: '/api/repos', headers: { cookie } })).json();
    expect(repos.repos).toHaveLength(1);
    const p = await app.inject({
      method: 'POST', url: '/api/projects', headers: { cookie },
      payload: { repoPath: repos.repos[0].path, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } },
    });
    expect(p.statusCode).toBe(200);
    const pid = p.json().id;
    const attach = await app.inject({ method: 'POST', url: `/api/projects/${pid}/datapacks`, headers: { cookie }, payload: { id: 'dp-shared', name: 'team/kb' } });
    expect(attach.json().datapacks.map((d: { role: string }) => d.role)).toEqual(['dedicated', 'shared']);
    const detachDedicated = await app.inject({ method: 'DELETE', url: `/api/projects/${pid}/datapacks/dp-1`, headers: { cookie } });
    expect(detachDedicated.statusCode).toBe(400);
    const s = await app.inject({ method: 'POST', url: `/api/projects/${pid}/sessions`, headers: { cookie }, payload: { name: 'API session', skipDesign: true } });
    expect(s.statusCode).toBe(200);
    expect(s.json().stage).toBe('code');
    const home = (await app.inject({ url: `/api/projects/${pid}/home`, headers: { cookie } })).json();
    expect(home.datapacks[0].memoryCount).toBe(3);
    expect(home.activity.length).toBeGreaterThan(0);
    const unknown = await app.inject({ method: 'POST', url: `/api/sessions/${s.json().id}/actions/explode`, headers: { cookie } });
    expect(unknown.statusCode).toBe(400);
    const changes = await app.inject({ url: `/api/sessions/${s.json().id}/changes`, headers: { cookie } });
    expect(changes.statusCode).toBe(200);
    expect(changes.json()).toMatchObject({ base: 'main', files: [], added: 0, removed: 0 });
    expect((await app.inject({ url: `/api/sessions/${s.json().id}/changes` })).statusCode).toBe(401);
  });
});
