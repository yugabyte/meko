import { realpathSync } from 'node:fs';
import { request } from 'node:http';
import { basename, join } from 'node:path';
import type { Paths } from './paths.js';
import type { ServiceManager } from './service.js';

/** What /healthz says: the server's version, and its pid and start time (not from a server before them). */
export type Probe = { ok: true; version: string; pid?: number; startedAt?: string } | { ok: false; error: string };

/** Asks the local server for /healthz on its socket (`Paths.webSock`). */
export function probeLocal(paths: Paths, timeoutMs = 3000): Promise<Probe> {
  return new Promise((resolve) => {
    const r = request(
      { socketPath: paths.webSock, path: '/healthz', method: 'GET', timeout: timeoutMs, headers: { host: 'localhost' } },
      (res) => {
        let body = '';
        res.setEncoding('utf8');
        res.on('data', (c: string) => (body += c));
        res.on('end', () => {
          try {
            const j = JSON.parse(body) as { ok?: boolean; version?: string; pid?: unknown; startedAt?: unknown };
            resolve(j.ok
              ? {
                ok: true,
                version: j.version ?? '?',
                ...(typeof j.pid === 'number' ? { pid: j.pid } : {}),
                ...(typeof j.startedAt === 'string' ? { startedAt: j.startedAt } : {}),
              }
              : { ok: false, error: `unexpected reply: ${body.slice(0, 80)}` });
          } catch {
            resolve({ ok: false, error: `HTTP ${res.statusCode}` });
          }
        });
      },
    );
    r.on('timeout', () => r.destroy(new Error('timed out')));
    r.on('error', (e) => resolve({ ok: false, error: e.message }));
    r.end();
  });
}

/** Polls until the server answers or the deadline passes. */
export async function waitForServer(paths: Paths, totalMs = 30000): Promise<Probe> {
  const end = Date.now() + totalMs;
  let last: Probe = { ok: false, error: 'not started' };
  while (Date.now() < end) {
    last = await probeLocal(paths, 2000);
    if (last.ok) return last;
    await new Promise((r) => setTimeout(r, 500));
  }
  return last;
}

/** The version app/current names (a release's folder is its version), or null without one (a source checkout). */
function currentVersion(paths: Paths): string | null {
  try {
    return basename(realpathSync(join(paths.app, 'current')));
  } catch {
    return null;
  }
}

/** How long `meko kode restart` waits for the server to answer: past three failed starts and Meko's rollback. */
export const RESTART_WAIT_MS = 100_000;

/**
 * `meko kode restart`: restarts the server's service, and only it (the session host and its sessions
 * keep running), then waits until the server answers as the version app/current names. Meko runs it
 * after switching app/current to an update; a version that keeps failing is rolled back by Meko's
 * launcher meanwhile, and then the old version answering ends the wait too. Returns the version that
 * answers; throws when there are no services or nothing answered in time.
 */
export async function restartServer(
  paths: Paths,
  svc: ServiceManager | null,
  o: { waitMs?: number; probe?: typeof probeLocal } = {},
): Promise<string> {
  if (!svc?.installed('server')) {
    throw new Error('No background services run this home, so there is no server to restart: stop meko kode serve and start it again, or install the services with meko kode install.');
  }
  svc.restart('server');
  const waitMs = o.waitMs ?? RESTART_WAIT_MS;
  const end = Date.now() + waitMs;
  for (;;) {
    const p = await (o.probe ?? probeLocal)(paths, 2000);
    const want = currentVersion(paths);
    if (p.ok && (!want || p.version === want)) return p.version;
    const last = p.ok ? `version ${p.version} answers, not ${want}` : p.error;
    if (Date.now() >= end) throw new Error(`The server didn't answer within ${Math.round(waitMs / 1000)} s (${last}). See: meko kode logs`);
    await new Promise((r) => setTimeout(r, 500));
  }
}
