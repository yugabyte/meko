import { randomBytes } from 'node:crypto';
import { createServer, type Server } from 'node:http';
import { chmodSync, lstatSync, mkdirSync, renameSync, rmSync } from 'node:fs';
import { dirname, join } from 'node:path';

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

/**
 * Which socket file is at `path`, or null when there is none. The server compares it with the one
 * it bound: another process can remove or replace the file. The change time is part of it because
 * a file made just after another was deleted can get the same inode number.
 */
export function socketId(path: string): string | null {
  try {
    const st = lstatSync(path, { bigint: true });
    return st.isSocket() ? `${st.dev}:${st.ino}:${st.ctimeNs}` : null;
  } catch {
    return null;
  }
}

/** What a guard hook prints for Claude Code; null lets the tool call go ahead. */
export type GuardDecision = Record<string, unknown> | null;

/** What the SessionStart context hook prints for Claude Code; null adds nothing. */
export type HookContext = Record<string, unknown> | null;

/**
 * Receives Claude Code hook events on a unix socket (mode 0600). Hooks never
 * touch the network port. Each request carries the session's hook token.
 * `/hook` events are answered at once and handled afterwards; `/guard` and `/context`
 * requests wait for the answer, which is returned as JSON (204 when there is none).
 *
 * The socket is bound under a temporary name and renamed into place. Closing a
 * server unlinks the path it was bound to, so this way closing never removes a
 * socket file another server put at `sockPath` since; the caller removes the file
 * itself when it's still its own (socketId).
 */
export function startHookServer(
  sockPath: string,
  onEvent: (token: string, payload: HookPayload) => void,
  onGuard: (token: string, payload: HookPayload) => GuardDecision = () => null,
  onStatusLine: (token: string, input: unknown) => void = () => undefined,
  onContext: (token: string, payload: HookPayload) => Promise<HookContext> = async () => null,
): Promise<Server> {
  const dir = dirname(sockPath);
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const st = lstatSync(dir);
  if (!st.isDirectory() || (typeof process.getuid === 'function' && st.uid !== process.getuid())) {
    throw new Error(`${dir} is not a directory owned by this user; refusing to put the hook socket there.`);
  }
  if (st.mode & 0o077) chmodSync(dir, 0o700);
  // No longer than the socket's own name, so it fits the same path length limit (see paths.ts).
  const bound = join(dir, `.h${randomBytes(3).toString('hex')}`);
  rmSync(bound, { force: true });
  const server = createServer((req, res) => {
    if (req.method !== 'POST' || !['/hook', '/guard', '/statusline', '/context'].includes(req.url ?? '')) {
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
      if (req.url === '/context') {
        let payload: HookPayload | null = null;
        try {
          payload = JSON.parse(body) as HookPayload;
        } catch {
          /* malformed payload: no context */
        }
        const answer = token && payload ? onContext(token, payload).catch((): HookContext => null) : Promise.resolve(null);
        void answer.then((ctx) => {
          if (ctx) res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify(ctx));
          else res.writeHead(204).end();
        });
        return;
      }
      res.writeHead(204).end();
      if (!token) return;
      try {
        if (req.url === '/statusline') onStatusLine(token, JSON.parse(body));
        else onEvent(token, JSON.parse(body) as HookPayload);
      } catch {
        /* malformed payload: ignore */
      }
    });
  });
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(bound, () => {
      try {
        chmodSync(bound, 0o600);
        renameSync(bound, sockPath);
      } catch (e) {
        server.close();
        reject(e instanceof Error ? e : new Error(String(e)));
        return;
      }
      resolve(server);
    });
  });
}
