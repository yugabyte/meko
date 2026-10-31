import { readFileSync } from 'node:fs';
import { request, type IncomingHttpHeaders } from 'node:http';
import { connect } from 'node:net';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { FastifyInstance } from 'fastify';
import type { WebSocket } from 'ws';
import { makeEnv, type TestEnv } from '../helpers.js';
import { buildServer, SHUTDOWN_CLOSE_MS } from '../../src/server/server.js';
import { listenPrivate } from '../../src/server/hooks.js';

/** Every REST route in the contract at the bottom of src/shared/types.ts. */
function contractRoutes(): { method: string; path: string }[] {
  const src = readFileSync(new URL('../../src/shared/types.ts', import.meta.url), 'utf8');
  return [...src.matchAll(/^\/\/ (GET|POST|PATCH|PUT|DELETE)\s+(\/api\/\S+)/gm)].map((m) => ({
    method: m[1],
    path: m[2].split('?')[0].replace(/:[a-zA-Z]+/g, 'x'),
  }));
}

/** Sends `raw` on the server's unix socket and returns the whole reply. */
function rawRequest(sock: string, raw: string): Promise<string> {
  return new Promise((resolve, reject) => {
    const peer = connect(sock, () => peer.write(raw));
    let data = '';
    peer.on('data', (d) => (data += d));
    peer.on('end', () => resolve(data));
    peer.on('error', reject);
  });
}

describe('HTTP API: access control', () => {
  let env: TestEnv;
  let app: FastifyInstance;
  beforeEach(async () => {
    env = await makeEnv();
    app = await buildServer(env.core, { webDir: null, updates: null });
  });
  afterEach(async () => {
    await app.close();
    await env.cleanup();
  });

  /** Listens on the home's web socket, as `agentk serve` does: Meko is what connects to it. */
  async function listen(): Promise<string> {
    await app.ready();
    await listenPrivate(app.server, env.core.paths.webSock, 'the web socket');
    return env.core.paths.webSock;
  }

  it('lets a request with no cookie reach the API: the server trusts its socket, and Meko decides who gets in', async () => {
    const sock = await listen();
    const reply = await new Promise<{ status: number; headers: IncomingHttpHeaders; body: string }>((resolve, reject) => {
      const r = request({ socketPath: sock, path: '/api/state', headers: { host: 'localhost' } }, (res) => {
        let body = '';
        res.setEncoding('utf8');
        res.on('data', (c: string) => (body += c));
        res.on('end', () => resolve({ status: res.statusCode ?? 0, headers: res.headers, body }));
      });
      r.on('error', reject);
      r.end();
    });
    expect(reply.status).toBe(200);
    expect(JSON.parse(reply.body).version).toBe('0.0.0-test');
    // Uncached, unframed and unsniffed.
    expect(reply.headers).toMatchObject({
      'x-content-type-options': 'nosniff', 'x-frame-options': 'DENY', 'referrer-policy': 'same-origin', 'cache-control': 'no-store',
    });
  });

  it('refuses a cross-origin write on every contract route, however the path is spelled', async () => {
    const writes = contractRoutes().filter((r) => r.method !== 'GET');
    expect(writes.length).toBeGreaterThan(30);
    const headers = { origin: 'https://evil.example', host: 'devbox:7777' };
    for (const { method, path } of writes) {
      for (const url of [path, path.replace('/api/', '/%61pi/'), path.replace('/api/', '/%61%70%69/')]) {
        const res = await app.inject({ method: method as 'POST', url, headers, payload: {} });
        expect(res.statusCode, `${method} ${url}`).toBe(403);
      }
    }
    // An absolute-form target (`POST http://host/api/...`) doesn't start with /api/, and is refused too.
    const reply = await rawRequest(await listen(), 'POST http://devbox:7777/api/projects HTTP/1.1\r\nHost: devbox:7777\r\n'
      + 'Origin: https://evil.example\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}');
    expect(reply).toMatch(/^HTTP\/1\.1 403/);
  });

  it('refuses form-style bodies, so a plain HTML form can\'t post to the API', async () => {
    const res = await app.inject({ method: 'POST', url: '/api/invites/check', headers: { 'content-type': 'text/plain' }, payload: 'x' });
    expect(res.statusCode).toBe(415);
    // The web client's bodiless actions send no content type and still work.
    expect((await app.inject({ method: 'POST', url: '/api/invites/check' })).statusCode).toBe(200);
  });

  it('answers bodies of the wrong type with 4xx, not 500', async () => {
    const settings = await app.inject({ method: 'PUT', url: '/api/settings', payload: { cloneInto: 5 } });
    expect(settings.statusCode).toBe(400);
  });

  describe('WebSockets', () => {
    const closed = (ws: WebSocket) => new Promise<number>((resolve) => ws.on('close', (code) => resolve(code)));

    it('opens a socket without a cookie, and closes one from another origin', async () => {
      await app.ready();
      let onFirst: (m: string) => void = () => undefined;
      const first = new Promise<string>((resolve) => (onFirst = resolve));
      const ws = await app.injectWS('/ws/events', { headers: { host: 'devbox:7777', origin: 'https://devbox:7777' } }, {
        onInit: (w) => w.once('message', (m) => onFirst(String(m))),
      });
      expect(JSON.parse(await first).type).toBe('state');
      ws.terminate();
      const foreign = { headers: { origin: 'https://evil.example', host: 'devbox:7777' } };
      expect(await closed(await app.injectWS('/ws/events', foreign))).toBe(1008);
      expect(await closed(await app.injectWS('/ws/term/nope', foreign))).toBe(1008);
    });

    it('answers a ping on the events socket, and ignores anything else', async () => {
      await app.ready();
      const got: string[] = [];
      let onPong: () => void = () => undefined;
      const pong = new Promise<void>((resolve) => (onPong = resolve));
      const ws = await app.injectWS('/ws/events', {}, {
        onInit: (w) => w.on('message', (m) => {
          const type = (JSON.parse(String(m)) as { type: string }).type;
          got.push(type);
          if (type === 'pong') onPong();
        }),
      });
      ws.send('not json');
      ws.send(JSON.stringify({ type: 'other' }));
      ws.send(JSON.stringify({ type: 'ping' }));
      await pong;
      expect(got).toEqual(['state', 'pong']);
      // Restarting into an update: pages hear so before the close.
      const closing = new Promise<void>((resolve) => ws.once('close', () => resolve()));
      env.core.notifyRestarting();
      await app.close();
      await closing;
      expect(got).toEqual(['state', 'pong', 'restarting']);
    });

    it("closes without waiting out a socket that never answers the close (ws's 30 s)", async () => {
      const sock = await listen();
      // A sleeping phone: the socket opens, then nothing it receives is ever answered.
      const peer = connect(sock);
      const upgraded = new Promise<void>((resolve) => peer.once('data', () => resolve()));
      peer.write('GET /ws/events HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n'
        + 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n');
      await upgraded;
      const started = Date.now();
      await app.close();
      expect(Date.now() - started).toBeLessThan(SHUTDOWN_CLOSE_MS + 2000);
      // The dropped socket is still on its way out, and a change between the close and the core's
      // stop must read no state: the database may be closed by the time a push would.
      const state = vi.spyOn(env.core, 'state');
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      try {
        env.core.changed();
        vi.advanceTimersByTime(1000);
      } finally {
        vi.useRealTimers();
      }
      expect(state).not.toHaveBeenCalled();
      peer.destroy();
    });
  });
});
