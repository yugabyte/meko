import { createServer, type Server } from 'node:http';
import { chmodSync, lstatSync, mkdirSync, rmSync } from 'node:fs';
import { dirname } from 'node:path';

export interface HookPayload {
  hook_event_name?: string;
  session_id?: string;
  transcript_path?: string;
  cwd?: string;
  source?: string;
  tool_name?: string;
  tool_input?: Record<string, unknown>;
  tool_response?: unknown;
  message?: string;
  notification_type?: string;
  prompt?: string;
  reason?: string;
  [k: string]: unknown;
}

/** What a guard hook prints for Claude Code; null lets the tool call go ahead. */
export type GuardDecision = Record<string, unknown> | null;

/**
 * Receives Claude Code hook events on a unix socket (mode 0600). Hooks never
 * touch the network port. Each request carries the session's hook token.
 * `/hook` events are answered at once and handled afterwards; `/guard` requests
 * wait for the decision, which is returned as JSON (204 when there is none).
 */
export function startHookServer(
  sockPath: string,
  onEvent: (token: string, payload: HookPayload) => void,
  onGuard: (token: string, payload: HookPayload) => GuardDecision = () => null,
): Promise<Server> {
  const dir = dirname(sockPath);
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const st = lstatSync(dir);
  if (!st.isDirectory() || (typeof process.getuid === 'function' && st.uid !== process.getuid())) {
    throw new Error(`${dir} is not a directory owned by this user; refusing to put the hook socket there.`);
  }
  if (st.mode & 0o077) chmodSync(dir, 0o700);
  rmSync(sockPath, { force: true });
  const server = createServer((req, res) => {
    if (req.method !== 'POST' || (req.url !== '/hook' && req.url !== '/guard')) {
      res.writeHead(404).end();
      return;
    }
    const token = String(req.headers['x-ak-token'] ?? '');
    let body = '';
    req.setEncoding('utf8');
    req.on('data', (c: string) => {
      body += c;
      if (body.length > 4 * 1024 * 1024) req.destroy();
    });
    req.on('end', () => {
      if (req.url === '/guard') {
        let decision: GuardDecision = null;
        try {
          if (token) decision = onGuard(token, JSON.parse(body) as HookPayload);
        } catch {
          /* malformed payload: no decision */
        }
        if (decision) res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify(decision));
        else res.writeHead(204).end();
        return;
      }
      res.writeHead(204).end();
      if (!token) return;
      try {
        onEvent(token, JSON.parse(body) as HookPayload);
      } catch {
        /* malformed payload: ignore */
      }
    });
  });
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(sockPath, () => {
      chmodSync(sockPath, 0o600);
      resolve(server);
    });
  });
}
