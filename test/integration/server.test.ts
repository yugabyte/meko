import { cpSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { hostname, tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { makeEnv, type TestEnv } from '../helpers.js';
import { buildServer, stampVersion } from '../../src/server/server.js';
import { shortHost } from '../../src/server/core.js';
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
    // The code that confirmed enrollment is used up; the next one (within the drift window) works once.
    const reused = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery', totp: totpCode(secret) } });
    expect(reused.statusCode).toBe(401);
    const next = totpCode(secret, Date.now() + 30_000);
    const withCode = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery', totp: next } });
    expect(withCode.statusCode).toBe(204);
    const replay = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery', totp: next } });
    expect(replay.statusCode).toBe(401);
  });

  it('shows the host name as the server name until Settings sets one', async () => {
    const cookie = await setup();
    const state = async () => (await app.inject({ url: '/api/state', headers: { cookie } })).json();
    const put = (serverName: unknown) => app.inject({ method: 'PUT', url: '/api/settings', headers: { cookie }, payload: { serverName } });
    expect(await state()).toMatchObject({ serverName: shortHost(hostname()), serverHost: hostname() });
    expect((await app.inject({ url: '/api/settings', headers: { cookie } })).json()).toMatchObject({ serverName: '', hostName: shortHost(hostname()) });

    const saved = await put('  work laptop  ');
    expect(saved.statusCode).toBe(200);
    expect(saved.json().serverName).toBe('work laptop');
    expect((await state()).serverName).toBe('work laptop');
    expect(env.core.config.server.name).toBe('work laptop');

    for (const bad of ['x'.repeat(65), 'two\nlines', 5]) {
      const res = await put(bad);
      expect(res.statusCode).toBe(400);
      expect(res.json().error).toMatch(/Server name must be 64 characters or fewer/);
    }
    expect((await state()).serverName).toBe('work laptop');

    expect((await put('')).json().serverName).toBe('');
    expect((await state()).serverName).toBe(shortHost(hostname()));
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
    expect(repos.repos[0].projects).toEqual([]);
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
    const s = await app.inject({ method: 'POST', url: `/api/projects/${pid}/sessions`, headers: { cookie }, payload: { name: 'API session', kind: 'quick' } });
    expect(s.statusCode).toBe(200);
    expect(s.json()).toMatchObject({ kind: 'quick', stage: 'code' });
    const badKind = await app.inject({ method: 'POST', url: `/api/projects/${pid}/sessions`, headers: { cookie }, payload: { name: 'Odd', kind: 'yolo' } });
    expect(badKind.statusCode).toBe(400);
    expect(badKind.json().error).toMatch(/Pick a session type/);
    const home = (await app.inject({ url: `/api/projects/${pid}/home`, headers: { cookie } })).json();
    expect(home.datapacks[0].memoryCount).toBe(3);
    expect(home.activity.length).toBeGreaterThan(0);
    const unknown = await app.inject({ method: 'POST', url: `/api/sessions/${s.json().id}/actions/explode`, headers: { cookie } });
    expect(unknown.statusCode).toBe(400);
    const changes = await app.inject({ url: `/api/sessions/${s.json().id}/changes`, headers: { cookie } });
    expect(changes.statusCode).toBe(200);
    expect(changes.json()).toMatchObject({ base: 'main', files: [], added: 0, removed: 0 });
    expect((await app.inject({ url: `/api/sessions/${s.json().id}/changes` })).statusCode).toBe(401);

    // Review comments: create, edit and delete drafts; everything behind login.
    const url = `/api/sessions/${s.json().id}/comments`;
    const draft = { path: 'a.ts', side: 'new', line: 1, endLine: 1, excerpt: 'x', body: 'Why?' };
    expect((await app.inject({ method: 'POST', url, payload: draft })).statusCode).toBe(401);
    expect((await app.inject({ url })).statusCode).toBe(401);
    const bad = await app.inject({ method: 'POST', url, headers: { cookie }, payload: { ...draft, body: ' ' } });
    expect(bad.statusCode).toBe(400);
    expect(bad.json().error).toMatch(/Write something/);
    const made = await app.inject({ method: 'POST', url, headers: { cookie }, payload: draft });
    expect(made.statusCode).toBe(200);
    expect(made.json()).toMatchObject({ path: 'a.ts', body: 'Why?', state: 'draft' });
    const cid = made.json().id;
    const edited = await app.inject({ method: 'PATCH', url: `${url}/${cid}`, headers: { cookie }, payload: { body: 'Why this?' } });
    expect(edited.json().body).toBe('Why this?');
    expect((await app.inject({ url, headers: { cookie } })).json()).toHaveLength(1);
    const state = (await app.inject({ url: '/api/state', headers: { cookie } })).json();
    expect(state.projects[0].sessions[0].comments).toEqual({ drafts: 1, queued: 0 });
    expect((await app.inject({ method: 'DELETE', url: `${url}/${cid}`, headers: { cookie } })).statusCode).toBe(204);
    expect((await app.inject({ method: 'DELETE', url: `${url}/${cid}`, headers: { cookie } })).statusCode).toBe(404);
    const nothing = await app.inject({ method: 'POST', url: `${url}/send`, headers: { cookie } });
    expect(nothing.statusCode).toBe(409);

    // Image previews: behind login, images only, served so the browser never runs them.
    const sid = s.json().id;
    const img = (q: string, withCookie = true) => app.inject({ url: `/api/sessions/${sid}/changes/image?${q}`, headers: withCookie ? { cookie } : {} });
    writeFileSync(join(s.json().worktreePath, 'pic.png'), Buffer.from([0x89, 0x50, 0x4e, 0x47, 1, 2, 3]));
    expect((await img('path=pic.png&side=new', false)).statusCode).toBe(401);
    expect((await img('path=pic.png&side=sideways')).statusCode).toBe(400);
    expect((await img('path=README.md&side=new')).statusCode).toBe(400);
    expect((await img('path=pic.png&side=old')).statusCode).toBe(404);
    const pic = await img('path=pic.png&side=new');
    expect(pic.statusCode).toBe(200);
    expect(pic.headers['content-type']).toBe('image/png');
    expect(pic.headers['content-security-policy']).toBe("default-src 'none'; sandbox");
    expect(pic.headers['x-content-type-options']).toBe('nosniff');
    expect(pic.rawPayload).toEqual(Buffer.from([0x89, 0x50, 0x4e, 0x47, 1, 2, 3]));
  });
});

describe('web app: manifest and icons for the installed app', () => {
  const web = fileURLToPath(new URL('../../src/web/', import.meta.url));
  let env: TestEnv;
  let app: FastifyInstance;
  let webDir: string;
  beforeEach(async () => {
    env = await makeEnv();
    // Lay the files out the way `vite build` does: index.html plus public/ at the root.
    webDir = mkdtempSync(join(tmpdir(), 'ak-web-'));
    cpSync(join(web, 'index.html'), join(webDir, 'index.html'));
    cpSync(join(web, 'public'), webDir, { recursive: true });
    app = await buildServer(env.core, { tls: null, webDir, updater: null });
  });
  afterEach(async () => {
    await app.close();
    await env.cleanup();
    rmSync(webDir, { recursive: true, force: true });
  });

  /** Width and height from a PNG's IHDR chunk. */
  const pngSize = (b: Buffer) => ({ png: b.subarray(1, 4).toString() === 'PNG', w: b.readUInt32BE(16), h: b.readUInt32BE(20) });

  it('links the manifest from index.html', async () => {
    const res = await app.inject({ url: '/' });
    expect(res.body).toContain('<link rel="manifest" href="/manifest.webmanifest" />');
  });

  it('serves the manifest and its square icons without sign-in', async () => {
    const res = await app.inject({ url: '/manifest.webmanifest' });
    expect(res.statusCode).toBe(200);
    expect(res.headers['content-type']).toMatch(/^application\/manifest\+json/);
    const manifest = res.json() as { name: string; short_name: string; icons: { src: string; sizes: string; purpose: string }[] };
    expect(manifest).toMatchObject({ name: 'AgentK Local', short_name: 'AgentK Local', start_url: '/', display: 'standalone' });
    expect(manifest.icons.map((i) => `${i.sizes} ${i.purpose}`)).toEqual(['192x192 any', '512x512 any', '512x512 maskable']);
    for (const icon of manifest.icons) {
      const img = await app.inject({ url: icon.src });
      expect(img.statusCode, icon.src).toBe(200);
      expect(img.headers['content-type']).toBe('image/png');
      const [w, h] = icon.sizes.split('x').map(Number);
      expect(pngSize(img.rawPayload), icon.src).toEqual({ png: true, w, h });
    }
    for (const [url, size] of [['/icons/favicon-32.png', 32], ['/icons/apple-touch-icon.png', 180]] as const) {
      const img = await app.inject({ url });
      expect(img.statusCode, url).toBe(200);
      expect(pngSize(img.rawPayload), url).toEqual({ png: true, w: size, h: size });
    }
    expect((await app.inject({ url: '/api/state' })).statusCode).toBe(401);
  });

  it('serves the service worker as a script at the root, so its scope is the whole app', async () => {
    const res = await app.inject({ url: '/sw.js' });
    expect(res.statusCode).toBe(200);
    expect(res.headers['content-type']).toMatch(/javascript/);
    expect(res.body).toContain('notificationclick');
    // It must never cache: no fetch handler, so the app and API always come from the server.
    expect(res.body).not.toMatch(/addEventListener\(['"]fetch/);
  });

  it('serves the icons phones and browsers look for at the root', async () => {
    const ico = await app.inject({ url: '/favicon.ico' });
    expect(ico.statusCode).toBe(200);
    expect(ico.rawPayload.subarray(0, 4)).toEqual(Buffer.from([0, 0, 1, 0])); // an ICO header
    for (const url of ['/apple-touch-icon.png', '/apple-touch-icon-precomposed.png']) {
      const img = await app.inject({ url });
      expect(img.statusCode, url).toBe(200);
      expect(img.headers['content-type'], url).toBe('image/png');
      expect(pngSize(img.rawPayload), url).toEqual({ png: true, w: 180, h: 180 });
    }
    expect((await app.inject({ url: '/' })).body).toContain('href="/favicon.ico"');
  });

  it('answers a missing file with 404, and app routes with the app', async () => {
    const missing = await app.inject({ url: '/icons/nope.png' });
    expect(missing.statusCode).toBe(404);
    expect(missing.headers['content-type']).not.toMatch(/html/);
    for (const url of ['/p/demo', '/p/demo/s/s1/design', '/p/v1.2/home']) {
      const res = await app.inject({ url });
      expect(res.statusCode, url).toBe(200);
      expect(res.headers['content-type'], url).toMatch(/^text\/html/);
    }
  });

  it('stamps its version into every page, never cached, so an open page can tell it was updated', async () => {
    for (const url of ['/', '/p/demo/s/s1', '/usage']) {
      const res = await app.inject({ url });
      expect(res.statusCode, url).toBe(200);
      expect(res.headers['content-type'], url).toMatch(/^text\/html/);
      expect(res.headers['cache-control'], url).toBe('no-cache');
      expect(res.body, url).toMatch(/<meta name="ak-version" content="0\.0\.0-test" \/>\s*<\/head>/);
      expect(res.body.match(/ak-version/g), url).toHaveLength(1);
    }
    expect((await app.inject({ method: 'HEAD', url: '/' })).statusCode).toBe(200);
    expect((await app.inject({ method: 'POST', url: '/' })).statusCode).toBe(404);
  });
});

describe('stampVersion', () => {
  it('escapes the version for the attribute', () => {
    expect(stampVersion('<head></head>', '1"<x>&')).toBe('<head>  <meta name="ak-version" content="1&#34;&#60;x&#62;&#38;" />\n  </head>');
  });
});
