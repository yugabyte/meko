import { execFile } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { createServer, type Server } from 'node:http';
import { hostname, tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
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

  /** `agentk status` for this home, run as its own process (the probe answers in this one): its exit code. */
  function status(): Promise<number> {
    const cli = fileURLToPath(new URL('../../src/server/cli.ts', import.meta.url));
    const tsx = fileURLToPath(new URL('../../node_modules/.bin/tsx', import.meta.url));
    const env: NodeJS.ProcessEnv = { ...process.env, AK_HOME: home, HOME: home, PATH: `${dirname(process.execPath)}:/usr/bin:/bin` };
    delete env.AK_APP_DIR;
    delete env.MEKO_HOME;
    return new Promise((resolve) => execFile(tsx, [cli, 'status'], { env }, (err) => resolve(err ? Number(err.code) : 0)));
  }

  // Meko reads it as running or not (meko status, and whether an update restarts AgentK).
  it('agentk status exits 3 while no server runs the home, and 0 while one answers or holds the lease', async () => {
    expect(await status()).toBe(3);
    await serve();
    expect(await status()).toBe(0);
    await new Promise<void>((r) => server!.close(() => r()));
    server = null;
    // A server that holds the lease but didn't answer in time runs.
    writeFileSync(paths.lock, JSON.stringify({ host: hostname(), pid: process.pid, beatAt: Date.now() }));
    expect(await status()).toBe(0);
  }, 60_000);
});
