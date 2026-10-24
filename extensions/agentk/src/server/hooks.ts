import { randomBytes } from 'node:crypto';
import { createServer, type Server, type ServerResponse } from 'node:http';
import type { Server as NetServer } from 'node:net';
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

/**
 * What a guard hook prints: JSON for Claude Code, plain text (the reason) for a harness that denies
 * by exit code (Kiro's guard script); null lets the tool call go ahead.
 */
export type GuardDecision = Record<string, unknown> | string | null;

/** What the context hook prints: JSON for Claude Code's SessionStart, plain text for Kiro's agentSpawn; null adds nothing. */
export type HookContext = Record<string, unknown> | string | null;

/** Answers a guard or context request: JSON, plain text, or 204 for nothing. */
function answer(res: ServerResponse, v: Record<string, unknown> | string | null): void {
  if (typeof v === 'string' && v) res.writeHead(200, { 'Content-Type': 'text/plain; charset=utf-8' }).end(v);
  else if (v && typeof v === 'object') res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify(v));
  else res.writeHead(204).end();
}

/** The routes AgentK's mod in Claude calls (src/mod/agentk); see modlink.ts. */
export const MOD_ROUTES = ['/mod/hello', '/mod/poll', '/mod/ack', '/mod/measure', '/mod/held', '/mod/ask', '/mod/ask/done', '/mod/questions', '/mod/learn', '/mod/learn/start', '/mod/meko', '/mod/turn'];

/** The route AgentK's session tools call (`agentk mcp`, src/server/mcp.ts), answered like the mod's routes. */
export const TOOLS_ROUTE = '/tools';

/** The routes that take a JSON body and answer JSON (`onMod`): the mod's and the session tools'. */
const JSON_ROUTES = [...MOD_ROUTES, TOOLS_ROUTE];

/** How the server answers the mod: the status, and a JSON body (always sent with a 200). */
export interface ModReply {
  status: number;
  body?: unknown;
}

/**
 * Receives the hook events of every session's agent (Claude Code, Kiro) on a unix socket (mode 0600). Hooks never
 * touch the network port. Each request carries the session's hook token.
 * `/hook` events are answered at once and handled afterwards; `/guard` and `/context`
 * requests wait for the answer: JSON for Claude Code, plain text for a harness whose hook prints
 * what it gets (Kiro's guard reason and context), 204 when there is none.
 * The `/mod/*` routes are AgentK's mod in Claude: `onMod` answers them, and may hold a
 * `/mod/poll` open (its signal aborts when the mod's connection closes). `onMod` also answers
 * `/tools`, the session tools' calls from `agentk mcp` in any harness's session.
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
  onMod: (token: string, route: string, body: Record<string, unknown>, signal: AbortSignal) => Promise<ModReply> = async () => ({ status: 404 }),
): Promise<Server> {
  const server = createServer((req, res) => {
    if (req.method !== 'POST' || ![...JSON_ROUTES, '/hook', '/guard', '/statusline', '/context'].includes(req.url ?? '')) {
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
      if (JSON_ROUTES.includes(req.url ?? '')) {
        let payload: unknown = null;
        try {
          payload = JSON.parse(body || '{}');
        } catch {
          /* malformed payload: read as empty */
        }
        const closed = new AbortController();
        res.on('close', () => closed.abort());
        const reply = token
          ? onMod(token, req.url!, payload && typeof payload === 'object' && !Array.isArray(payload) ? payload as Record<string, unknown> : {}, closed.signal)
            .catch((): ModReply => ({ status: 500 }))
          : Promise.resolve<ModReply>({ status: 403 });
        // One connection per request: a kept-alive one the mod keeps polling on would hold the
        // server open when it closes. A poll comes every few seconds at most, so this costs nothing.
        void reply.then((r) => {
          if (res.writableEnded || res.destroyed) return;
          if (r.status === 200 || r.body !== undefined) res.writeHead(r.status, { 'Content-Type': 'application/json', Connection: 'close' }).end(JSON.stringify(r.body ?? null));
          else res.writeHead(r.status, { Connection: 'close' }).end();
        });
        return;
      }
      if (req.url === '/guard') {
        let decision: GuardDecision = null;
        try {
          if (token) decision = onGuard(token, JSON.parse(body) as HookPayload);
        } catch {
          /* malformed payload: no decision */
        }
        answer(res, decision);
        return;
      }
      if (req.url === '/context') {
        let payload: HookPayload | null = null;
        try {
          payload = JSON.parse(body) as HookPayload;
        } catch {
          /* malformed payload: no context */
        }
        const context = token && payload ? onContext(token, payload).catch((): HookContext => null) : Promise.resolve(null);
        void context.then((ctx) => answer(res, ctx));
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
  return listenPrivate(server, sockPath, 'the hook socket').then(() => server);
}

/**
 * Listens on a unix socket only this user can reach: its directory is owner-only, the socket
 * mode 0600. It binds under a temporary name and renames into place, because closing a server
 * unlinks the path it was bound to: this way closing never removes a socket file another server
 * put at `sockPath` since. Used for the hook socket and the session host's.
 */
export function listenPrivate(server: NetServer, sockPath: string, what: string): Promise<void> {
  const dir = dirname(sockPath);
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const st = lstatSync(dir);
  if (!st.isDirectory() || (typeof process.getuid === 'function' && st.uid !== process.getuid())) {
    return Promise.reject(new Error(`${dir} is not a directory owned by this user; refusing to put ${what} there.`));
  }
  if (st.mode & 0o077) chmodSync(dir, 0o700);
  // No longer than the socket's own name, so it fits the same path length limit (see paths.ts).
  const bound = join(dir, `.h${randomBytes(3).toString('hex')}`);
  rmSync(bound, { force: true });
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
      server.off('error', reject);
      resolve();
    });
  });
}
