import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { afterEach, describe, expect, it } from 'vitest';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import { probeHost, probeLocal, serverUrl } from '../../src/server/health.js';

describe('health probe', () => {
  let server: Server | null = null;
  afterEach(() => new Promise<void>((r) => (server ? server.close(() => r()) : r())));

  it('reaches wildcard and localhost binds over loopback, and others at their address', () => {
    for (const b of ['0.0.0.0', '::', '[::]', 'localhost', '']) expect(probeHost(b)).toBe('127.0.0.1');
    expect(probeHost('::1')).toBe('::1');
    expect(probeHost('[::1]')).toBe('::1');
    expect(probeHost('10.1.2.3')).toBe('10.1.2.3');
  });

  it.each([
    ['127.0.0.1', 'http://127.0.0.1:7777'],
    ['localhost', 'http://127.0.0.1:7777'],
    ['::1', 'http://[::1]:7777'],
  ])('sends a browser to the loopback address a %s bind listens on', (bind, url) => {
    const cfg: Config = structuredClone(DEFAULT_CONFIG);
    cfg.server = { ...cfg.server, bind, port: 7777 };
    expect(serverUrl(cfg, false)).toBe(url);
  });

  /** Serves /healthz on `host`, or returns null when this host has no such address. */
  async function serveOn(host: string): Promise<number | null> {
    const s = createServer((_req, res) => res.end(JSON.stringify({ ok: true, version: '1.2.3' })));
    const port = await new Promise<number | null>((resolve) => {
      s.once('error', () => resolve(null));
      s.listen(0, host, () => resolve((s.address() as AddressInfo).port));
    });
    if (port) server = s;
    return port;
  }

  it('asks the server on the address it listens on', async (ctx) => {
    const port = await serveOn('::1');
    if (!port) return ctx.skip();
    const cfg: Config = structuredClone(DEFAULT_CONFIG);
    cfg.server = { ...cfg.server, bind: '::1', port };
    expect(await probeLocal(cfg)).toEqual({ ok: true, version: '1.2.3' });
  });
});
