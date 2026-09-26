import { readFileSync } from 'node:fs';
import { connect } from 'node:net';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import type { WebSocket } from 'ws';
import { makeEnv, type TestEnv } from '../helpers.js';
import { buildServer } from '../../src/server/server.js';
import { COOKIE_NAME, issueSetupToken } from '../../src/server/auth.js';
import { get, kvGet, kvSet, run } from '../../src/server/db.js';
import { createHash } from 'node:crypto';

const PUBLIC = new Set(['/api/auth/status', '/api/auth/setup', '/api/auth/login']);

/** Every REST route in the contract at the bottom of src/shared/types.ts. */
function contractRoutes(): { method: string; path: string }[] {
  const src = readFileSync(new URL('../../src/shared/types.ts', import.meta.url), 'utf8');
  return [...src.matchAll(/^\/\/ (GET|POST|PATCH|PUT|DELETE)\s+(\/api\/\S+)/gm)].map((m) => ({
    method: m[1],
    path: m[2].split('?')[0].replace(/:[a-zA-Z]+/g, 'x'),
  }));
}

describe('HTTP API: access control', () => {
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
  const tokenOf = (cookie: string) => decodeURIComponent(cookie.slice(COOKIE_NAME.length + 1));
  const PASSWORD = 'correct horse battery';

  async function setup(): Promise<string> {
    const token = issueSetupToken(env.core.db);
    const res = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me', password: PASSWORD } });
    expect(res.statusCode).toBe(204);
    return cookieOf(res);
  }

  it('requires sign-in on every contract route but the three public ones, however the path is spelled', async () => {
    await setup();
    const routes = contractRoutes();
    expect(routes.length).toBeGreaterThan(30);
    for (const { method, path } of routes) {
      if (PUBLIC.has(path)) continue;
      for (const url of [path, path.replace('/api/', '/%61pi/'), path.replace('/api/', '/%61%70%69/')]) {
        const res = await app.inject({ method: method as 'GET', url });
        expect(res.statusCode, `${method} ${url}`).toBe(401);
      }
    }
  });

  it('checks the origin of writes to encoded API paths', async () => {
    const cookie = await setup();
    const res = await app.inject({
      method: 'POST', url: '/%61pi/projects', headers: { cookie, origin: 'https://evil.example', host: 'devbox:7777' }, payload: {},
    });
    expect(res.statusCode).toBe(403);
  });

  it('refuses an absolute-form request target without a cookie', async () => {
    await setup();
    await app.listen({ host: '127.0.0.1', port: 0 });
    const { port } = app.server.address() as { port: number };
    const reply = await new Promise<string>((resolve, reject) => {
      const sock = connect(port, '127.0.0.1', () => sock.write(`GET http://127.0.0.1:${port}/api/state HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n`));
      let data = '';
      sock.on('data', (d) => (data += d));
      sock.on('end', () => resolve(data));
      sock.on('error', reject);
    });
    expect(reply).toMatch(/^HTTP\/1\.1 401/);
  });

  it('refuses form-style bodies, so a plain HTML form can\'t post to the API', async () => {
    const cookie = await setup();
    const res = await app.inject({ method: 'POST', url: '/api/update/apply', headers: { cookie, 'content-type': 'text/plain' }, payload: 'x' });
    expect(res.statusCode).toBe(415);
    // The web client's bodiless actions send no content type and still work.
    expect((await app.inject({ method: 'POST', url: '/api/update/check', headers: { cookie } })).statusCode).toBe(200);
  });

  it('answers bodies of the wrong type with 4xx, not 500', async () => {
    const cookie = await setup();
    const login = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 123 } });
    expect(login.statusCode).toBe(401);
    const totp = await app.inject({ method: 'POST', url: '/api/auth/totp/confirm', headers: { cookie }, payload: { code: 5 } });
    expect(totp.statusCode).toBe(400);
    const settings = await app.inject({ method: 'PUT', url: '/api/settings', headers: { cookie }, payload: { cloneInto: 5 } });
    expect(settings.statusCode).toBe(400);
  });

  it('renews the cookie as the login slides forward', async () => {
    const cookie = await setup();
    const fresh = await app.inject({ url: '/api/state', headers: { cookie } });
    expect(fresh.headers['set-cookie']).toBeUndefined();
    const hash = createHash('sha256').update(tokenOf(cookie)).digest('hex');
    run(env.core.db, 'UPDATE auth_sessions SET expires_at = ? WHERE token_sha256 = ?', Date.now() + 3600_000, hash);
    const renewed = await app.inject({ url: '/api/state', headers: { cookie } });
    expect(renewed.statusCode).toBe(200);
    expect(String(renewed.headers['set-cookie'])).toMatch(/Max-Age=1209600/);
    const row = get<{ expires_at: number }>(env.core.db, 'SELECT expires_at FROM auth_sessions WHERE token_sha256 = ?', hash)!;
    expect(row.expires_at).toBeGreaterThan(Date.now() + 13 * 24 * 3600_000);
  });

  it('forgets an expired setup link, so sign-in is offered again', async () => {
    await setup();
    kvSet(env.core.db, 'setup_token', JSON.stringify({ sha256: 'x'.repeat(64), expiresAt: Date.now() - 1000 }));
    const st = await app.inject({ url: '/api/auth/status' });
    expect(st.json()).toMatchObject({ setupRequired: false, loggedIn: false });
    expect(kvGet(env.core.db, 'setup_token')).toBeNull();
  });

  it('signs out other browsers on a password change', async () => {
    const mine = await setup();
    const other = cookieOf(await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: PASSWORD } }));
    const res = await app.inject({ method: 'POST', url: '/api/auth/password', headers: { cookie: mine }, payload: { current: PASSWORD, next: 'another long password' } });
    expect(res.statusCode).toBe(204);
    expect((await app.inject({ url: '/api/state', headers: { cookie: mine } })).statusCode).toBe(200);
    expect((await app.inject({ url: '/api/state', headers: { cookie: other } })).statusCode).toBe(401);
  });

  describe('WebSockets', () => {
    const closed = (ws: WebSocket) => new Promise<number>((resolve) => ws.on('close', (code) => resolve(code)));

    it('refuses a socket without a login or from another origin', async () => {
      const cookie = await setup();
      await app.ready();
      const anon = await app.injectWS('/ws/events');
      expect(await closed(anon)).toBe(4401);
      const foreign = await app.injectWS('/ws/events', { headers: { cookie, origin: 'https://evil.example', host: 'devbox:7777' } });
      expect(await closed(foreign)).toBe(4401);
      const term = await app.injectWS('/ws/term/nope');
      expect(await closed(term)).toBe(4401);
    });

    it('closes open sockets when their login is signed out', async () => {
      const cookie = await setup();
      await app.ready();
      const ws = await app.injectWS('/ws/events', { headers: { cookie } });
      const done = closed(ws);
      expect((await app.inject({ method: 'POST', url: '/api/auth/logout', headers: { cookie } })).statusCode).toBe(204);
      expect(await done).toBe(4401);
    });

  });
});
