// Presenter's server: its pages, its API and its WebSockets, on a unix socket only. Meko's web
// listener serves it to browsers at /ext/present/ after its own sign-in, passing the browser's Host
// and Origin as they are, so there's no login here; writes and WebSocket upgrades still refuse
// another site's Origin, and the pages refuse to be framed.

import { createReadStream, existsSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import Fastify, { type FastifyInstance, type FastifyRequest } from 'fastify';
import fastifyStatic from '@fastify/static';
import fastifyWebsocket from '@fastify/websocket';
import type { WebSocket } from 'ws';
import { groupDatapacks } from '../shared/datapacks.js';
import { DeckError, MAX_ASSET_BYTES, SHAPES, emptyBrief, type Brief, type DeckSources } from '../shared/deck.js';
import { OpError, type Op } from '../shared/ops.js';
import type { NavAction } from '../shared/run.js';
import {
  API_VERSION, type ApplyRequest, type CreateDeckRequest, type Health, type JobRequest, type OpsRequest, type RunClientMessage, type ServerEvent, type StartRunRequest,
} from '../shared/types.js';
import { HttpError, type DeckStore } from './decks.js';
import type { Jobs } from './jobs.js';
import { MekoError, type MekoPort } from './meko.js';
import type { Runs } from './runs.js';

/** Where Meko serves Presenter: the page's `<base href>`, under which it builds every URL. */
export const BASE_PATH = '/ext/present/';

export interface Core {
  version: string;
  meko: MekoPort;
  decks: DeckStore;
  jobs: Jobs;
  runs: Runs;
  /** Whether Claude Code is installed, for the health route. */
  claudeFound: () => boolean;
  /** Meko says its Meko Cloud link changed: find Meko again. */
  mekoChanged: () => void;
  /** Every open page's event socket. */
  events: Set<WebSocket>;
}

/** Sends an event to every open page. */
export function broadcast(core: Pick<Core, 'events'>, e: ServerEvent): void {
  const text = JSON.stringify(e);
  for (const ws of core.events) if (ws.readyState === ws.OPEN) ws.send(text);
}

/** The pages' Content-Security-Policy: this server only, and no framing. `host` is for WebSockets. */
export function pageCsp(host: string | undefined): string {
  const ws = host && /^[A-Za-z0-9.:[\]-]+$/.test(host) ? ` wss://${host} ws://${host}` : '';
  return [
    "default-src 'self'",
    "script-src 'self'",
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data: blob:",
    "media-src 'self' blob:",
    "font-src 'self' data:",
    `connect-src 'self'${ws}`,
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
    "frame-ancestors 'none'",
  ].join('; ');
}

/** The built page with `<base href>` naming where Meko serves it, and the server's version. */
export function stampPage(html: string, version: string): string {
  const v = version.replace(/[&"<>]/g, (c) => `&#${c.charCodeAt(0)};`);
  return html
    .replace('<head>', `<head>\n    <base href="${BASE_PATH}" />`)
    .replace('</head>', `  <meta name="present-version" content="${v}" />\n  </head>`);
}

export function findWebDir(serverDir: string): string | null {
  for (const c of [join(serverDir, '..', 'web'), join(serverDir, '..', '..', 'dist', 'web')]) {
    if (existsSync(join(c, 'index.html')) && existsSync(join(c, 'assets'))) return c;
  }
  return null;
}

/**
 * The one byte range a Range header asks for, 'unsatisfiable', or null to send the whole file (no
 * header, several ranges, or a form this doesn't read). Safari plays <video> only with ranges.
 */
export function byteRange(header: string | undefined, size: number): { start: number; end: number } | 'unsatisfiable' | null {
  const m = /^bytes=(\d*)-(\d*)$/.exec(header?.trim() ?? '');
  if (!m || (!m[1] && !m[2])) return null;
  const start = m[1] ? Number(m[1]) : Math.max(0, size - Number(m[2]));
  const end = m[1] && m[2] ? Math.min(Number(m[2]), size - 1) : size - 1;
  if ((!m[1] && Number(m[2]) === 0) || start >= size || start > end) return 'unsatisfiable';
  return { start, end };
}

const str = (v: unknown, max = 2000) => (typeof v === 'string' ? v.slice(0, max) : '');
const strs = (v: unknown) => (Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string').slice(0, 50) : []);

function readBrief(b: unknown): Brief {
  const o = (b && typeof b === 'object' ? b : {}) as Record<string, unknown>;
  const minutes = Number(o.minutes);
  return {
    ...emptyBrief(),
    audience: str(o.audience, 500), goal: str(o.goal, 500), notes: str(o.notes, 4000),
    minutes: Number.isFinite(minutes) && minutes > 0 ? Math.min(Math.round(minutes), 600) : 30,
    shape: SHAPES.includes(o.shape as Brief['shape']) ? (o.shape as Brief['shape']) : 'graph',
  };
}

function readSources(s: unknown): DeckSources {
  const o = (s && typeof s === 'object' ? s : {}) as Record<string, unknown>;
  return { content: strs(o.content), template: typeof o.template === 'string' && o.template ? o.template : null, animations: strs(o.animations) };
}

export async function buildServer(core: Core, opts: { webDir: string | null }): Promise<FastifyInstance> {
  const app = Fastify({ logger: false, bodyLimit: 8 * 1024 * 1024 }) as unknown as FastifyInstance;
  await app.register(fastifyWebsocket, { options: { maxPayload: 1024 * 1024 } });
  // JSON only, and raw bytes for assets. Without a text/plain parser a plain HTML form can't post here.
  app.removeContentTypeParser('text/plain');
  app.addContentTypeParser('application/octet-stream', { parseAs: 'buffer', bodyLimit: MAX_ASSET_BYTES + 1 }, (_req, body, done) => done(null, body));

  /** The route a request matched: access checks key on it, never the raw URL. */
  const routeOf = (req: FastifyRequest): string | undefined => req.routeOptions.url;
  const sameOrigin = (req: FastifyRequest) => {
    const origin = req.headers.origin;
    if (!origin) return true;
    try {
      return new URL(origin).host === req.headers.host;
    } catch {
      return false;
    }
  };

  app.addHook('onRequest', async (req, reply) => {
    const route = routeOf(req);
    if (route?.startsWith('/ws/') && !sameOrigin(req)) return reply.code(403).send({ error: 'Cross-origin connection refused.' });
    if (route?.startsWith('/api/') && req.method !== 'GET' && !sameOrigin(req)) return reply.code(403).send({ error: 'Cross-origin request refused.' });
  });

  app.addHook('onSend', async (req, reply, payload) => {
    reply.header('X-Content-Type-Options', 'nosniff');
    reply.header('X-Frame-Options', 'DENY');
    reply.header('Referrer-Policy', 'same-origin');
    // The asset route says how its files may be kept; nothing else under /api/ is kept.
    if (routeOf(req)?.startsWith('/api/')) {
      if (!reply.hasHeader('Cache-Control')) reply.header('Cache-Control', 'no-store');
    } else if (/^text\/html/.test(String(reply.getHeader('content-type') ?? ''))) reply.header('Content-Security-Policy', pageCsp(req.headers.host));
    return payload;
  });

  app.setErrorHandler((err: unknown, req, reply) => {
    if (err instanceof HttpError) return reply.code(err.status).send({ error: err.message, ...(err.problems ? { problems: err.problems } : {}) });
    if (err instanceof DeckError) return reply.code(400).send({ error: err.problems[0], problems: err.problems });
    if (err instanceof OpError) return reply.code(400).send({ error: err.message });
    if (err instanceof MekoError) return reply.code(502).send({ error: err.message });
    const e = err as { statusCode?: number; message?: string };
    if (e.statusCode === 413) {
      // Only an asset upload (the one PUT) carries a file; other bodies are JSON, up to bodyLimit.
      return reply.code(413).send({ error: req.method === 'PUT' ? 'The file is bigger than 100 MiB, the most one asset can be. Trim or compress it first.' : 'The request is bigger than 8 MiB, the most Presenter takes. Send less at once.' });
    }
    if (e.statusCode && e.statusCode < 500) return reply.code(e.statusCode).send({ error: e.message ?? 'Bad request.' });
    return reply.code(500).send({ error: (err as Error)?.message ?? 'Something went wrong.' });
  });

  type P = { id: string };
  // Live runs follow every change to their deck, by hand, by Claude, undo or restore alike.
  core.decks.listen((s) => core.runs.deckChanged(s.id, s.deck));

  app.get('/api/health', async (): Promise<Health> => {
    const meko = await core.meko.status();
    return { ok: true, version: core.version, api: API_VERSION, meko, mekoAt: await core.meko.at(), claude: core.claudeFound() };
  });
  app.get('/api/datapacks', async () => groupDatapacks(await core.meko.listDatapacks()));

  app.get('/api/decks', async () => core.decks.list());
  app.post<{ Body: CreateDeckRequest }>('/api/decks', async (req) => {
    const b = (req.body ?? {}) as Partial<CreateDeckRequest>;
    return core.decks.create({ title: str(b.title, 200), brief: readBrief(b.brief), sources: readSources(b.sources) });
  });
  app.get<{ Params: P }>('/api/decks/:id', async (req) => core.decks.state(req.params.id));
  app.post<{ Params: P; Body: OpsRequest }>('/api/decks/:id/ops', async (req) => {
    const ops = req.body?.ops;
    if (!Array.isArray(ops) || ops.length === 0 || ops.length > 500) throw new HttpError(400, 'Send between 1 and 500 changes.');
    return core.decks.apply(req.params.id, ops as Op[]);
  });
  app.post<{ Params: P }>('/api/decks/:id/undo', async (req) => core.decks.undo(req.params.id));
  app.post<{ Params: P }>('/api/decks/:id/redo', async (req) => core.decks.redo(req.params.id));
  app.post<{ Params: P }>('/api/decks/:id/save', async (req) => core.decks.save(req.params.id));
  app.get<{ Params: P }>('/api/decks/:id/versions', async (req) => core.decks.versions(req.params.id));
  app.post<{ Params: P; Body: { hash?: string } }>('/api/decks/:id/restore', async (req) => core.decks.restore(req.params.id, str(req.body?.hash, 64)));
  app.get<{ Params: P & { name: string } }>('/api/decks/:id/assets/:name', async (req, reply) => {
    const a = await core.decks.assetFile(req.params.id, req.params.name);
    const size = statSync(a.file).size;
    const etag = `"${a.hash}"`;
    // Never sniffed or run as a page: an SVG is shown only through <img>, which runs no script. The
    // URL names the asset, whose bytes can change (a new file of that name, undo), so the browser
    // asks each time (no-cache) and the ETag, the bytes' hash, spares sending them again.
    reply.type(a.type).header('Content-Disposition', 'inline').header('Content-Security-Policy', "default-src 'none'; style-src 'unsafe-inline'; sandbox")
      .header('Cache-Control', 'private, no-cache').header('ETag', etag).header('Accept-Ranges', 'bytes');
    if (req.headers['if-none-match'] === etag) return reply.code(304).send();
    const range = byteRange(req.headers.range, size);
    if (range === 'unsatisfiable') return reply.code(416).header('Content-Range', `bytes */${size}`).send();
    if (range) {
      return reply.code(206).header('Content-Range', `bytes ${range.start}-${range.end}/${size}`).header('Content-Length', range.end - range.start + 1)
        .send(createReadStream(a.file, range));
    }
    return reply.header('Content-Length', size).send(createReadStream(a.file));
  });
  app.put<{ Params: P & { name: string } }>('/api/decks/:id/assets/:name', async (req) => {
    if (!Buffer.isBuffer(req.body)) throw new HttpError(415, 'Send the file\'s bytes as application/octet-stream.');
    return core.decks.addAsset(req.params.id, req.params.name, str(req.headers['x-asset-type'], 100), new Uint8Array(req.body));
  });

  app.get<{ Params: P }>('/api/decks/:id/jobs', async (req) => core.jobs.list(req.params.id));
  app.post<{ Params: P; Body: JobRequest }>('/api/decks/:id/jobs', async (req) => {
    const b = (req.body ?? {}) as Partial<JobRequest> & Record<string, unknown>;
    const scope = (b.scope && typeof b.scope === 'object' ? b.scope : { kind: 'deck' }) as Record<string, unknown>;
    let job: JobRequest;
    if (b.kind === 'map') job = { kind: 'map', ...(str(b.note) ? { note: str(b.note) } : {}) };
    else if (b.kind === 'build') job = { kind: 'build', scenes: strs(b.scenes) };
    else if (b.kind === 'ask') {
      const kind = ['block', 'slide', 'scene', 'deck'].includes(scope.kind as string) ? (scope.kind as 'deck') : 'deck';
      job = {
        kind: 'ask', instruction: str(b.instruction, 4000),
        scope: { kind, scene: str(scope.scene, 100) || undefined, slide: str(scope.slide, 100) || undefined, block: typeof scope.block === 'number' ? scope.block : undefined, variant: str(scope.variant, 50) || undefined },
      };
    } else throw new HttpError(400, 'A job is a map, a build or an ask.');
    return core.jobs.start(req.params.id, job);
  });
  app.post<{ Params: P }>('/api/jobs/:id/cancel', async (req) => core.jobs.cancel(req.params.id));
  app.get<{ Params: P }>('/api/decks/:id/changesets', async (req) => core.jobs.changeSets(req.params.id));
  app.post<{ Params: P; Body: ApplyRequest }>('/api/changesets/:id/apply', async (req) => {
    const take = Array.isArray(req.body?.take) ? req.body.take.filter((n): n is number => typeof n === 'number') : [];
    return core.jobs.apply(req.params.id, take);
  });
  app.post<{ Params: P }>('/api/changesets/:id/discard', async (req) => core.jobs.discard(req.params.id));
  app.post<{ Params: P }>('/api/decks/:id/session/fresh', async (req) => {
    await core.decks.deck(req.params.id);
    core.jobs.freshSession(req.params.id);
    return { ok: true };
  });

  app.post<{ Params: P; Body: StartRunRequest }>('/api/decks/:id/runs', async (req) => core.runs.start(req.params.id, str(req.body?.variant, 50) || undefined));
  app.get<{ Params: P }>('/api/runs/:id', async (req) => core.runs.get(req.params.id));

  // Meko, on Presenter's socket: something Presenter reads from Meko changed (`cloud`: the Meko Cloud
  // link). Outside /api, so the same-origin rule is checked here; all it does is find Meko again.
  app.post<{ Body: { what?: unknown } }>('/_meko/changed', async (req, reply) => {
    if (!sameOrigin(req)) return reply.code(403).send({ error: 'Cross-origin request refused.' });
    if (req.body?.what === 'cloud') core.mekoChanged();
    return reply.code(204).send();
  });

  // Meko, on Presenter's socket, before it updates Presenter on its own: a live talk makes it wait.
  app.get('/_meko/busy', async () => ({ busy: core.runs.live() }));

  app.get('/ws/events', { websocket: true }, (socket: WebSocket) => {
    core.events.add(socket);
    socket.on('close', () => core.events.delete(socket));
  });
  app.get<{ Params: P }>('/ws/runs/:id', { websocket: true }, (socket: WebSocket, req) => {
    const peer = { send: (m: unknown) => socket.readyState === socket.OPEN && socket.send(JSON.stringify(m)) };
    let leave: (() => void) | null = null;
    core.runs.join(req.params.id, peer).then((l) => {
      leave = l;
      if (socket.readyState !== socket.OPEN) l();
    }, (e: Error) => {
      peer.send({ type: 'error', error: e.message });
      socket.close();
    });
    socket.on('message', (raw) => {
      let m: RunClientMessage;
      try {
        m = JSON.parse(String(raw)) as RunClientMessage;
      } catch {
        return;
      }
      if (m?.type === 'nav' && m.nav && typeof m.nav === 'object') {
        core.runs.nav(req.params.id, m.nav as NavAction).catch((e: Error) => peer.send({ type: 'error', error: e.message }));
      }
    });
    socket.on('close', () => leave?.());
  });

  if (opts.webDir) {
    await app.register(fastifyStatic, { root: opts.webDir, index: false, wildcard: false });
    const page = stampPage(readFileSync(join(opts.webDir, 'index.html'), 'utf8'), core.version);
    app.get('/', (_req, reply) => reply.type('text/html').header('Cache-Control', 'no-cache').send(page));
    app.setNotFoundHandler((req, reply) => {
      const path = req.url.split('?')[0]!;
      if ((req.method !== 'GET' && req.method !== 'HEAD') || path.startsWith('/api/') || path.startsWith('/ws/') || /\.[a-z0-9]+$/i.test(path)) {
        return reply.code(404).send({ error: 'Not found.' });
      }
      return reply.type('text/html').header('Cache-Control', 'no-cache').send(page);
    });
  }
  return app;
}
