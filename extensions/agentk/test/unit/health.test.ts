import { mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { createServer, type Server } from 'node:http';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { probeLocal, waitForServer } from '../../src/server/health.js';
import { getPaths, type Paths } from '../../src/server/paths.js';

describe('health probe', () => {
  let home: string;
  let paths: Paths;
  let server: Server | null = null;
  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'ak-health-'));
    paths = getPaths(home);
    mkdirSync(dirname(paths.webSock), { recursive: true });
  });
  afterEach(async () => {
    await new Promise<void>((r) => (server ? server.close(() => r()) : r()));
    server = null;
    rmSync(home, { recursive: true, force: true });
  });

  /** Serves /healthz on the home's web socket, as the server does. */
  function serve(): Promise<void> {
    const s = createServer((req, res) => res.end(JSON.stringify(req.url === '/healthz' ? { ok: true, version: '1.2.3' } : {})));
    server = s;
    return new Promise((resolve) => s.listen(paths.webSock, () => resolve()));
  }

  it('asks the server on its socket, and waits for one that starts late', async () => {
    await serve();
    expect(await probeLocal(paths)).toEqual({ ok: true, version: '1.2.3' });
    await new Promise<void>((r) => server!.close(() => r()));
    server = null;
    const waiting = waitForServer(paths, 10_000);
    setTimeout(() => void serve(), 300);
    expect(await waiting).toEqual({ ok: true, version: '1.2.3' });
  });

  it('says the server is not running when nothing listens on its socket', async () => {
    expect(await probeLocal(paths)).toMatchObject({ ok: false, error: expect.stringMatching(/ENOENT|ECONNREFUSED/) });
    expect((await waitForServer(paths, 600)).ok).toBe(false);
  });
});
