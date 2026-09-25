import { request as httpRequest } from 'node:http';
import { request as httpsRequest } from 'node:https';
import { isLoopback, type Config } from './config.js';

export type Probe = { ok: true; version: string } | { ok: false; error: string };

/**
 * Asks the local server for /healthz over loopback. The certificate isn't
 * verified: this only talks to 127.0.0.1, and the default cert is self-signed.
 */
export function probeLocal(cfg: Config, timeoutMs = 3000): Promise<Probe> {
  const tls = !!(cfg.tls.cert && cfg.tls.key) || !isLoopback(cfg.server.bind);
  const req = tls ? httpsRequest : httpRequest;
  return new Promise((resolve) => {
    const r = req(
      { host: '127.0.0.1', port: cfg.server.port, path: '/healthz', method: 'GET', timeout: timeoutMs, rejectUnauthorized: false },
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
export async function waitForServer(cfg: Config, totalMs = 30000): Promise<Probe> {
  const end = Date.now() + totalMs;
  let last: Probe = { ok: false, error: 'not started' };
  while (Date.now() < end) {
    last = await probeLocal(cfg, 2000);
    if (last.ok) return last;
    await new Promise((r) => setTimeout(r, 500));
  }
  return last;
}
