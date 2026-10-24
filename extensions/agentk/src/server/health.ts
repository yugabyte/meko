import { request } from 'node:http';
import type { Paths } from './paths.js';

export type Probe = { ok: true; version: string } | { ok: false; error: string };

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
            const j = JSON.parse(body) as { ok?: boolean; version?: string };
            resolve(j.ok ? { ok: true, version: j.version ?? '?' } : { ok: false, error: `unexpected reply: ${body.slice(0, 80)}` });
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
