import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import Fastify, { type FastifyInstance, type FastifyReply, type FastifyRequest } from 'fastify';
import fastifyCookie from '@fastify/cookie';
import fastifyStatic from '@fastify/static';
import fastifyWebsocket from '@fastify/websocket';
import QRCode from 'qrcode';
import type { WebSocket } from 'ws';
import type {
  AttachDatapackRequest, AuthStatus, CreateProjectRequest, CreateSessionRequest, DatapackPeople, DesignAnswer, DesignDoc, DiscardCheck, InviteCheck, LoginRequest,
  EventsClientMessage, MekoDatapack, MoveSessionRequest, PastedImage, ReviewComment, SendCommentsResult, ServerEvent, SessionChanges, SessionAction, Settings, SettingsUpdate, SetDedicatedDatapackRequest, SetupRequest,
  ShareDatapackRequest, UpdateProjectRequest, CloneRepoRequest, PrReviewers, RepoInfo, ReviewablePrs,
} from '../shared/types.js';
import { MAX_PASTE_BYTES, SHARE_GRANTS } from '../shared/types.js';
import {
  COOKIE_NAME, LoginLimiter, SESSION_MAX_AGE_S, authSessionLive, burnPasswordCheck, changePassword, consumeSetupToken, createAuthSession,
  deleteAuthSession, getOwner, localAccess, newTotpSecret, otpauthUrl, setOwner, setupTokenOutstanding, setupTokenValid, totpAccept, validateAuthSession, validatePassword,
  verifyPassword,
} from './auth.js';
import { designQuestions } from './answers.js';
import type { Core } from './core.js';
import { audit, get, run } from './db.js';
import { errMsg } from './log.js';
import type { MekoDatapackRow } from './meko.js';
import { applyMekoChange, mekoSettings, nextMekoConfig, setUpClaudeMeko } from './mekosettings.js';
import { HttpError } from './projects.js';
import { attachTerminal } from './terminal.js';
import type { TlsMaterial } from './tls.js';
import type { Updater } from './updater.js';
import { usageOverview } from './usage.js';
import { MAX_IDLE_DAYS, MAX_IDLE_PER_REPO } from './worktrees.js';

/** A browser with this much unsent data skips state snapshots until it catches up. */
const MAX_BUFFERED_STATE = 4 * 1024 * 1024;
const ACTIONS: SessionAction[] = [
  'approve-design', 'open-pr', 'merge', 'request-review', 'force-review', 'resume', 'archive', 'discard', 'update-pr', 'close-review',
];
const PUBLIC_API = new Set(['/api/auth/status', '/api/auth/setup', '/api/auth/login']);

export interface ServerOptions {
  tls: TlsMaterial | null;
  webDir: string | null;
  updater: Updater | null;
}

export async function buildServer(core: Core, opts: ServerOptions): Promise<FastifyInstance> {
  const app = Fastify({
    logger: false,
    bodyLimit: 1024 * 1024,
    ...(opts.tls ? { https: { cert: opts.tls.cert, key: opts.tls.key } } : {}),
  }) as unknown as FastifyInstance;
  const db = core.db;
  const limiter = new LoginLimiter();
  const secure = !!opts.tls;

  await app.register(fastifyCookie);
  await app.register(fastifyWebsocket, { options: { maxPayload: 1024 * 1024 } });
  // The API only takes JSON. Without a text/plain parser, a plain HTML form (which can't send
  // JSON) can't post to it, even from our own origin.
  app.removeContentTypeParser('text/plain');

  // The address the server listens on (cli.ts listens on the same config), fixed for its lifetime.
  const bind = core.config.server.bind;
  /** No login needed: the server listens only on this computer and the request is local. */
  // `req.ip` is the peer's address: no trustProxy, so a forwarding header can't change it.
  const local = (req: FastifyRequest) => localAccess(bind, req.ip, req.headers);
  // Only the onRequest hook renews a login, since only it can re-issue the cookie with it.
  const authed = (req: FastifyRequest) => local(req) || authSessionLive(db, req.cookies[COOKIE_NAME]);
  /**
   * The route a request matched (`/api/sessions/:id`), or undefined when none did. Access checks
   * key on this, never the raw URL: the router decodes `/%61pi/...` to `/api/...`, and an
   * absolute-form target (`GET http://host/api/...`) doesn't start with `/api/` at all.
   */
  const routeOf = (req: FastifyRequest): string | undefined => req.routeOptions.url;
  const isApi = (req: FastifyRequest) => routeOf(req)?.startsWith('/api/') ?? false;

  /** Same-origin check for writes and WebSocket upgrades (defense in depth on top of SameSite=Strict). */
  const sameOrigin = (req: FastifyRequest) => {
    const origin = req.headers.origin;
    if (!origin) return true;
    try {
      return new URL(origin).host === req.headers.host;
    } catch {
      return false;
    }
  };

  app.addHook('onSend', async (req, reply, payload) => {
    reply.header('X-Content-Type-Options', 'nosniff');
    reply.header('X-Frame-Options', 'DENY');
    reply.header('Referrer-Policy', 'same-origin');
    if (isApi(req)) reply.header('Cache-Control', 'no-store');
    return payload;
  });

  app.addHook('onRequest', async (req, reply) => {
    const route = routeOf(req);
    if (!route?.startsWith('/api/')) return;
    if (req.method !== 'GET' && !sameOrigin(req)) return reply.code(403).send({ error: 'Cross-origin request refused.' });
    if (PUBLIC_API.has(route) || local(req)) return;
    const auth = validateAuthSession(db, req.cookies[COOKIE_NAME]);
    if (!auth.valid) return reply.code(401).send({ error: 'Sign in first.' });
    // The database slides the expiry as the app is used; keep the browser's cookie in step.
    if (auth.renewed) setCookie(reply, req.cookies[COOKIE_NAME]!);
  });

  app.setErrorHandler((err: unknown, req, reply) => {
    if (err instanceof HttpError) return reply.code(err.status).send({ error: err.message, ...(err.code ? { code: err.code } : {}) });
    const e = err as { statusCode?: number; message?: string };
    if (e.statusCode && e.statusCode < 500) return reply.code(e.statusCode).send({ error: e.message ?? 'Bad request.' });
    core.log.error(`${req.method} ${req.url} failed`, err);
    return reply.code(500).send({ error: errMsg(err) });
  });

  const setCookie = (reply: FastifyReply, token: string) =>
    reply.setCookie(COOKIE_NAME, token, { httpOnly: true, secure, sameSite: 'strict', path: '/', maxAge: SESSION_MAX_AGE_S });

  // ---- health ----
  app.get('/healthz', async () => ({ ok: true, version: core.version }));

  // ---- auth ----
  /** The owner, for the account routes; a local-access install may not have one yet. */
  const ownerOr409 = () => {
    const owner = getOwner(db);
    if (!owner) throw new HttpError(409, 'No login is set up. Run `agent-k-local setup-link` on the host to create one.');
    return owner;
  };

  app.get('/api/auth/status', async (req): Promise<AuthStatus> => {
    const owner = getOwner(db);
    const tokenOutstanding = setupTokenOutstanding(db);
    const isLocal = local(req);
    const loggedIn = isLocal || authed(req);
    const setupRequired = !isLocal && (!owner || (tokenOutstanding && !loggedIn));
    return {
      setupRequired,
      reset: setupRequired && !!owner,
      loggedIn,
      username: owner && loggedIn ? owner.username : null,
      totpEnabled: !!owner?.totp_secret,
      localAccess: isLocal,
    };
  });

  app.post<{ Body: SetupRequest }>('/api/auth/setup', async (req, reply) => {
    const { token, username, password } = req.body ?? ({} as SetupRequest);
    const ip = req.ip;
    // A valid setup link gets past the limit: it's how a locked-out owner gets back in. The token
    // is 192 random bits, so letting it through doesn't help anyone guess it.
    if (limiter.blocked(ip) && !setupTokenValid(db, token)) return reply.code(429).send({ error: 'Too many attempts. Try again in 15 minutes.' });
    if (typeof username !== 'string' || !/^[A-Za-z0-9._-]{1,64}$/.test(username)) return reply.code(400).send({ error: 'Use letters, digits, dots, dashes or underscores for the username.' });
    const bad = validatePassword(password ?? '');
    if (bad) return reply.code(400).send({ error: bad });
    if (!consumeSetupToken(db, token)) {
      limiter.fail(ip);
      return reply.code(403).send({ error: 'This setup link is invalid or already used. Run `agent-k-local setup-link` on the host for a new one.' });
    }
    setOwner(db, username, password);
    audit(db, 'owner_set', { username, ip });
    closeRevokedSockets();
    setCookie(reply, createAuthSession(db, req.headers['user-agent'], ip));
    return reply.code(204).send();
  });

  app.post<{ Body: LoginRequest }>('/api/auth/login', async (req, reply) => {
    const ip = req.ip;
    if (limiter.blocked(ip)) return reply.code(429).send({ error: 'Too many failed attempts. Try again in 15 minutes.' });
    const { username, password, totp } = req.body ?? ({} as LoginRequest);
    const owner = getOwner(db);
    const userOk = !!owner && owner.username === username;
    // Check a password either way, so the time taken doesn't tell whether the username is right.
    const passOk = userOk ? verifyPassword(password, owner.pw_scrypt) : (burnPasswordCheck(password), false);
    if (!owner || !passOk) {
      limiter.fail(ip);
      audit(db, 'login_failed', { username: typeof username === 'string' ? username : null, ip });
      return reply.code(401).send({ error: 'Wrong username or password.' });
    }
    if (owner.totp_secret) {
      if (!totp) return reply.code(401).send({ error: 'Enter the code from your authenticator app.', totpRequired: true });
      if (!totpAccept(db, owner.totp_secret, totp)) {
        limiter.fail(ip);
        audit(db, 'login_failed_totp', { username, ip });
        return reply.code(401).send({ error: 'That code is wrong or expired.', totpRequired: true });
      }
    }
    limiter.reset(ip);
    audit(db, 'login', { username, ip });
    setCookie(reply, createAuthSession(db, req.headers['user-agent'], ip));
    return reply.code(204).send();
  });

  app.post('/api/auth/logout', async (req, reply) => {
    deleteAuthSession(db, req.cookies[COOKIE_NAME]);
    closeRevokedSockets();
    reply.clearCookie(COOKIE_NAME, { path: '/' });
    return reply.code(204).send();
  });

  app.post<{ Body: { current: string; next: string } }>('/api/auth/password', async (req, reply) => {
    ownerOr409();
    const err = changePassword(db, req.body?.current ?? '', req.body?.next ?? '', req.cookies[COOKIE_NAME]);
    if (err) return reply.code(400).send({ error: err });
    closeRevokedSockets();
    return reply.code(204).send();
  });

  app.post('/api/auth/totp/enroll', async () => {
    const owner = ownerOr409();
    const secret = newTotpSecret();
    run(db, 'UPDATE owner SET totp_pending = ? WHERE id = 1', secret);
    const url = otpauthUrl(secret, owner.username);
    return { secret, otpauthUrl: url, qrSvg: await QRCode.toString(url, { type: 'svg', margin: 1 }) };
  });

  app.post<{ Body: { code: string } }>('/api/auth/totp/confirm', async (req, reply) => {
    const owner = ownerOr409();
    if (!owner.totp_pending || !totpAccept(db, owner.totp_pending, req.body?.code)) {
      return reply.code(400).send({ error: 'That code doesn\'t match. Check the time on your phone and try again.' });
    }
    run(db, 'UPDATE owner SET totp_secret = totp_pending, totp_pending = NULL WHERE id = 1');
    audit(db, 'totp_enabled');
    return reply.code(204).send();
  });

  app.post<{ Body: { code: string } }>('/api/auth/totp/disable', async (req, reply) => {
    const owner = ownerOr409();
    if (!owner.totp_secret || !totpAccept(db, owner.totp_secret, req.body?.code)) {
      return reply.code(400).send({ error: 'That code is wrong or expired.' });
    }
    run(db, 'UPDATE owner SET totp_secret = NULL WHERE id = 1');
    audit(db, 'totp_disabled');
    return reply.code(204).send();
  });

  // ---- state, repos, datapacks ----
  app.get('/api/state', async () => core.state());
  app.get('/api/repos', async () => core.projects.repos());
  app.get<{ Querystring: { path?: string } }>('/api/repos/branches', async (req) => core.projects.repoBranches(req.query.path));
  app.post<{ Body: CloneRepoRequest }>('/api/repos/clone', async (req): Promise<RepoInfo> => core.prReview.clone(req.body?.url));
  /** A datapack_list row for the browser: without the owner's account id. */
  const toDatapack = ({ ownerUserId: _, ...d }: MekoDatapackRow): MekoDatapack => d;
  app.get('/api/meko/datapacks', async (): Promise<MekoDatapack[]> => {
    if (!core.mekoStatus.configured) return [];
    let rows;
    try {
      rows = await core.meko.listDatapacks();
    } catch (e) {
      throw new HttpError(502, `Couldn't list datapacks from Meko: ${errMsg(e)}`);
    }
    return rows.map(toDatapack);
  });

  // ---- sharing (Meko's share API) ----
  /** Runs a share API call for a datapack one of our projects uses; Meko's errors become HTTP errors (see apiFailure). */
  const sharing = async <T>(datapackId: string, fn: () => Promise<T>): Promise<T> => {
    const inUse = get(db, `SELECT 1 AS x FROM project_datapacks d JOIN projects p ON p.id = d.project_id
                           WHERE d.datapack_id = ? AND p.deleted_at IS NULL`, datapackId);
    if (!inUse) throw new HttpError(404, "That datapack isn't in any of your projects.");
    core.invites.requireSharing('share datapacks');
    try {
      return await fn();
    } catch (e) {
      throw core.invites.apiFailure(e, 'Meko');
    }
  };
  /** The lists after a change that went through; a failure to read them doesn't make the change look failed. */
  const peopleAfter = (id: string): Promise<DatapackPeople> =>
    core.invites.api.people(id).catch((e) => ({ members: [], invites: [], refreshError: `Couldn't read the list again: ${errMsg(e)}` }));
  app.get<{ Params: { id: string } }>('/api/meko/datapacks/:id/people', async (req): Promise<DatapackPeople> =>
    sharing(req.params.id, () => core.invites.api.people(req.params.id)));
  app.post<{ Params: { id: string }; Body: ShareDatapackRequest }>('/api/meko/datapacks/:id/people', async (req): Promise<DatapackPeople> => {
    const email = typeof req.body?.email === 'string' ? req.body.email.trim() : '';
    const grant = req.body?.grant;
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) throw new HttpError(400, 'Enter the email address of the person to share with.');
    if (!SHARE_GRANTS.includes(grant)) throw new HttpError(400, 'Access must be contributor, maintainer or viewer.');
    await sharing(req.params.id, () => core.invites.api.share(req.params.id, email, grant));
    core.log.info(`invited ${email} to datapack ${req.params.id} as ${grant}`);
    // Shared with a PR's reviewer: their email comes up for them next time.
    if (req.body.login != null) core.prReview.rememberEmail(req.body.login, email);
    return peopleAfter(req.params.id);
  });
  app.post<{ Params: { id: string; inviteId: string } }>('/api/meko/datapacks/:id/people/invites/:inviteId/resend', async (req): Promise<DatapackPeople> =>
    sharing(req.params.id, () => core.invites.api.resend(req.params.id, req.params.inviteId)).then(() => peopleAfter(req.params.id)));
  app.delete<{ Params: { id: string; inviteId: string } }>('/api/meko/datapacks/:id/people/invites/:inviteId', async (req): Promise<DatapackPeople> =>
    sharing(req.params.id, () => core.invites.api.revoke(req.params.id, req.params.inviteId)).then(() => peopleAfter(req.params.id)));

  // ---- datapacks shared with us ----
  app.post('/api/invites/check', async (): Promise<InviteCheck> => {
    if (!core.mekoStatus.configured) return { datapacks: [], invites: [], closed: [] };
    let rows: MekoDatapackRow[];
    try {
      // One datapack_list serves both the dialog's list and the sync; the stamp catches a change here in between.
      const stamp = core.invites.stamp();
      rows = await core.meko.listDatapacks();
      await core.invites.sync({ rows, stamp });
    } catch (e) {
      throw new HttpError(502, `Couldn't check Meko for datapacks and shares: ${errMsg(e)}`);
    }
    return { datapacks: rows.map(toDatapack), invites: core.invites.open(), closed: core.projects.closed() };
  });
  app.post<{ Params: { id: string } }>('/api/invites/:id/accept', async (req, reply) => {
    await core.invites.accept(req.params.id);
    return reply.code(204).send();
  });
  app.post<{ Params: { id: string } }>('/api/invites/:id/decline', async (req, reply) => {
    await core.invites.decline(req.params.id);
    return reply.code(204).send();
  });
  app.post<{ Params: { id: string } }>('/api/invites/:id/later', async (req, reply) => {
    core.invites.later(req.params.id);
    return reply.code(204).send();
  });

  // ---- projects ----
  app.post<{ Body: CreateProjectRequest }>('/api/projects', async (req) => core.projects.create(req.body));
  app.get<{ Params: { id: string } }>('/api/projects/:id', async (req) => core.projects.get(req.params.id));
  app.patch<{ Params: { id: string }; Body: UpdateProjectRequest }>('/api/projects/:id', async (req) => core.projects.update(req.params.id, req.body ?? {}));
  app.delete<{ Params: { id: string } }>('/api/projects/:id', async (req, reply) => {
    await core.projects.delete(req.params.id);
    return reply.code(204).send();
  });
  app.get<{ Params: { id: string } }>('/api/projects/:id/branches', async (req) => core.projects.branches(req.params.id));
  app.post<{ Params: { id: string }; Body: AttachDatapackRequest }>('/api/projects/:id/datapacks', async (req) => core.projects.attachDatapack(req.params.id, req.body));
  app.post<{ Params: { id: string }; Body: SetDedicatedDatapackRequest }>('/api/projects/:id/dedicated-datapack', async (req) =>
    core.projects.setDedicatedDatapack(req.params.id, req.body));
  app.delete<{ Params: { id: string; dpId: string } }>('/api/projects/:id/datapacks/:dpId', async (req) => core.projects.detachDatapack(req.params.id, req.params.dpId));
  app.get<{ Params: { id: string } }>('/api/projects/:id/home', async (req) => core.projects.home(req.params.id));
  app.get<{ Params: { id: string } }>('/api/projects/:id/pr-reviewers', async (req): Promise<PrReviewers[]> => core.prReview.reviewers(req.params.id));
  app.get<{ Params: { id: string }; Querystring: { pr?: string; repoPath?: string } }>('/api/projects/:id/review-prs', async (req): Promise<ReviewablePrs> =>
    core.prReview.reviewable(req.params.id, { pr: req.query.pr, repoPath: req.query.repoPath }));
  app.get('/api/usage', async () => usageOverview(core.db));
  app.post<{ Params: { id: string }; Body: { query: string } }>('/api/projects/:id/search', async (req) => core.projects.search(req.params.id, req.body?.query ?? ''));

  // ---- sessions ----
  // Answers once the session is written: its worktree and Claude start after, which can take
  // minutes on a big repo, so the dialog doesn't wait. A worktree or PR head that can't be had then
  // is a failed start in the state; Claude failing to start leaves the session exited with the error.
  app.post<{ Params: { id: string }; Body: CreateSessionRequest }>('/api/projects/:id/sessions', async (req) => {
    const { session, done } = await core.sessions.begin(req.params.id, req.body ?? ({} as CreateSessionRequest));
    done.catch((e: unknown) => core.log.warn(`session ${session.id} didn't start`, errMsg(e)));
    return session;
  });
  app.delete<{ Params: { id: string } }>('/api/failed-starts/:id', async (req, reply) => {
    core.sessions.dismissFailedStart(req.params.id);
    return reply.code(204).send();
  });
  app.post<{ Params: { id: string; action: string } }>('/api/sessions/:id/actions/:action', async (req) => {
    const action = req.params.action as SessionAction;
    if (!ACTIONS.includes(action)) throw new HttpError(400, `Unknown action ${action}.`);
    return core.sessions.action(req.params.id, action);
  });
  app.post<{ Params: { id: string }; Body: MoveSessionRequest }>('/api/sessions/:id/move', async (req) =>
    core.sessions.move(req.params.id, req.body));
  app.get<{ Params: { id: string } }>('/api/sessions/:id/discard-check', async (req): Promise<DiscardCheck> => core.sessions.discardCheck(req.params.id));
  app.get<{ Params: { id: string } }>('/api/sessions/:id/changes', async (req): Promise<SessionChanges> => core.sessions.changes(req.params.id));
  app.get<{ Params: { id: string }; Querystring: { path?: string; side?: string } }>('/api/sessions/:id/changes/image', async (req, reply) => {
    const { path = '', side } = req.query;
    if (side !== 'old' && side !== 'new') throw new HttpError(400, "side must be 'old' or 'new'.");
    const img = await core.sessions.changedImage(req.params.id, path, side);
    // Bytes from the repo: never let the browser run or sniff them, even when opened directly.
    return reply
      .type(img.type)
      .header('Content-Security-Policy', "default-src 'none'; sandbox")
      .header('Content-Disposition', 'inline')
      .send(img.data);
  });
  // An image pasted into the terminal: the body is the image itself. Its parser lives only in this
  // scope, so no other route takes a body that isn't JSON (and an HTML form can't send image/*).
  await app.register(async (scope) => {
    scope.addContentTypeParser(/^image\//, { parseAs: 'buffer', bodyLimit: MAX_PASTE_BYTES }, (_req, body, done) => done(null, body));
    scope.post<{ Params: { id: string } }>('/api/sessions/:id/images', async (req): Promise<PastedImage> => core.sessions.pasteImage(req.params.id, req.body));
  });
  app.get<{ Params: { id: string } }>('/api/sessions/:id/comments', async (req): Promise<ReviewComment[]> => core.comments.list(req.params.id));
  app.post<{ Params: { id: string }; Body: unknown }>('/api/sessions/:id/comments', async (req): Promise<ReviewComment> => core.comments.add(req.params.id, req.body));
  app.post<{ Params: { id: string } }>('/api/sessions/:id/comments/send', async (req): Promise<SendCommentsResult> => core.comments.send(req.params.id));
  app.patch<{ Params: { id: string; cid: string }; Body: { body?: unknown } }>('/api/sessions/:id/comments/:cid',
    async (req): Promise<ReviewComment> => core.comments.edit(req.params.id, req.params.cid, req.body?.body));
  app.delete<{ Params: { id: string; cid: string } }>('/api/sessions/:id/comments/:cid', async (req, reply) => {
    core.comments.remove(req.params.id, req.params.cid);
    return reply.code(204).send();
  });
  app.get<{ Params: { id: string } }>('/api/sessions/:id/design', async (req): Promise<DesignDoc> => {
    core.sessions.row(req.params.id);
    const r = get<{ markdown: string; html: string | null; artifact_url: string | null; updated_at: number; questions_source: string | null }>(
      db, 'SELECT markdown, html, artifact_url, updated_at, questions_source FROM designs WHERE session_id = ?', req.params.id);
    return {
      markdown: r?.markdown || null, html: r?.html ?? null, artifactUrl: r?.artifact_url ?? null, updatedAt: r?.updated_at ?? null,
      questions: designQuestions(db, req.params.id),
      questionsSource: r?.questions_source === 'file' || r?.questions_source === 'markdown' ? r.questions_source : null,
    };
  });
  app.get<{ Params: { id: string } }>('/api/sessions/:id/design/answers', async (req): Promise<DesignAnswer[]> => core.answers.list(req.params.id));
  app.put<{ Params: { id: string; qid: string }; Body: unknown }>('/api/sessions/:id/design/answers/:qid',
    async (req): Promise<DesignAnswer[]> => core.answers.put(req.params.id, req.params.qid, req.body));
  app.delete<{ Params: { id: string; qid: string } }>('/api/sessions/:id/design/answers/:qid', async (req, reply) => {
    core.answers.remove(req.params.id, req.params.qid);
    return reply.code(204).send();
  });
  app.post<{ Params: { id: string } }>('/api/sessions/:id/design/answers/send', async (req): Promise<SendCommentsResult> => core.answers.send(req.params.id));

  // ---- settings ----
  const settings = (): Settings => ({
    reposRoots: core.config.repos.roots,
    reposDepth: core.config.repos.depth,
    cloneInto: core.config.repos.cloneInto,
    updateChannel: core.config.update.channel,
    bind: core.config.server.bind,
    port: core.config.server.port,
    serverName: core.config.server.name,
    hostName: core.hostName(),
    idleWorktreesPerRepo: core.worktrees.cap(),
    idleWorktreeDays: core.worktrees.idleDays(),
    callHomeEnabled: core.config.callHome.enabled,
    resolveConflicts: core.config.pr.resolveConflicts,
    meko: mekoSettings(core.config),
  });
  app.get('/api/settings', async () => settings());
  app.put<{ Body: SettingsUpdate }>('/api/settings', async (req) => {
    const b = req.body ?? {};
    if (b.reposRoots !== undefined && (!Array.isArray(b.reposRoots) || !b.reposRoots.length || b.reposRoots.some((r) => typeof r !== 'string' || !r.trim()))) {
      throw new HttpError(400, 'List at least one repos folder.');
    }
    if (b.reposDepth !== undefined && (!Number.isInteger(b.reposDepth) || b.reposDepth < 1 || b.reposDepth > 4)) throw new HttpError(400, 'Scan depth must be between 1 and 4.');
    if (b.updateChannel !== undefined && !['stable', 'prerelease', 'off'].includes(b.updateChannel)) throw new HttpError(400, 'Unknown update channel.');
    if (b.cloneInto !== undefined && typeof b.cloneInto !== 'string') throw new HttpError(400, 'The clone folder must be a path.');
    if (b.idleWorktreesPerRepo !== undefined && (!Number.isInteger(b.idleWorktreesPerRepo) || b.idleWorktreesPerRepo < 0 || b.idleWorktreesPerRepo > MAX_IDLE_PER_REPO)) {
      throw new HttpError(400, `Idle worktrees per repo must be a whole number from 0 to ${MAX_IDLE_PER_REPO}.`);
    }
    if (b.idleWorktreeDays !== undefined && (!Number.isInteger(b.idleWorktreeDays) || b.idleWorktreeDays < 0 || b.idleWorktreeDays > MAX_IDLE_DAYS)) {
      throw new HttpError(400, `Days to keep an unused idle worktree must be a whole number from 0 to ${MAX_IDLE_DAYS}.`);
    }
    // eslint-disable-next-line no-control-regex
    if (b.serverName !== undefined && (typeof b.serverName !== 'string' || b.serverName.trim().length > 64 || /[\u0000-\u001f\u007f]/.test(b.serverName))) {
      throw new HttpError(400, 'Server name must be 64 characters or fewer, with no line breaks.');
    }
    if (b.callHomeEnabled !== undefined && typeof b.callHomeEnabled !== 'boolean') throw new HttpError(400, 'callHomeEnabled must be true or false.');
    if (b.resolveConflicts !== undefined && typeof b.resolveConflicts !== 'boolean') throw new HttpError(400, 'resolveConflicts must be true or false.');
    if (b.meko !== undefined && (typeof b.meko !== 'object' || b.meko === null || Array.isArray(b.meko))) throw new HttpError(400, 'meko must be an object.');
    // Checked against Meko before anything is saved.
    const meko = b.meko ? await nextMekoConfig(core, core.mekoTools, b.meko) : null;
    const before = { cap: core.worktrees.cap(), days: core.worktrees.idleDays() };
    core.updateConfig((c) => {
      if (b.reposRoots) c.repos.roots = b.reposRoots.map((r) => r.trim());
      if (b.reposDepth) c.repos.depth = b.reposDepth;
      if (b.cloneInto?.trim()) c.repos.cloneInto = b.cloneInto.trim();
      if (b.updateChannel) c.update.channel = b.updateChannel;
      if (b.serverName !== undefined) c.server.name = b.serverName.trim();
      if (b.idleWorktreesPerRepo !== undefined) c.worktrees.idlePerRepo = b.idleWorktreesPerRepo;
      if (b.idleWorktreeDays !== undefined) c.worktrees.idleDays = b.idleWorktreeDays;
      if (b.callHomeEnabled !== undefined) c.callHome.enabled = b.callHomeEnabled;
      if (b.resolveConflicts !== undefined) c.pr.resolveConflicts = b.resolveConflicts;
      if (meko) applyMekoChange(c, meko.next, meko.changed);
    });
    // Removing build trees can take a while, so it runs in the background, and only when a limit went down.
    const cleanup = (what: string, fn: () => Promise<void>) => void fn().catch((e) => core.log.warn(`${what} failed`, errMsg(e)));
    if (core.worktrees.cap() < before.cap) cleanup('trimming idle worktrees', () => core.worktrees.trimAll());
    const days = core.worktrees.idleDays();
    if (days && (!before.days || days < before.days)) cleanup('removing unused idle worktrees', () => core.worktrees.expire());
    if (meko) {
      audit(db, 'meko_settings_changed', { url: mekoSettings(core.config).effectiveUrl });
      core.log.info(`Meko settings changed (${mekoSettings(core.config).effectiveUrl})`);
      if (meko.reconnect) await core.mekoChanged();
      else if (meko.resync) void core.syncInvites();
    }
    return settings();
  });
  app.post('/api/settings/meko/claude-setup', async (): Promise<Settings> => {
    await setUpClaudeMeko(core, core.mekoTools);
    audit(db, 'meko_claude_setup', { url: mekoSettings(core.config).effectiveUrl });
    return settings();
  });

  // ---- updates ----
  app.get('/api/update', async () => (opts.updater ? opts.updater.status() : core.updateStatus()));
  app.post('/api/update/check', async () => (opts.updater ? opts.updater.check() : core.updateStatus()));
  app.post('/api/update/apply', async () => {
    if (!opts.updater) throw new HttpError(409, 'Updates are disabled when running from a source checkout.');
    return opts.updater.apply();
  });

  // ---- websockets ----
  const clients = new Set<WebSocket>();
  /** Every open socket (events and terminals) with the login cookie it was opened with (none for local access). */
  const sockets = new Map<WebSocket, { token: string | null; alive: boolean }>();
  /** Closes sockets whose login is gone (logout, password change or reset, expiry). */
  function closeRevokedSockets() {
    for (const [ws, s] of sockets) if (s.token !== null && !authSessionLive(db, s.token)) ws.close(4401, 'unauthorized');
  }
  // Heartbeat: a phone that drops off the network sends no close, and its `tmux attach` would
  // otherwise stay attached (holding the window at its size) until the server restarts.
  const heartbeat = setInterval(() => {
    for (const [ws, s] of sockets) {
      if (!s.alive) {
        ws.terminate();
        continue;
      }
      s.alive = false;
      ws.ping();
    }
    closeRevokedSockets();
  }, 30_000);
  heartbeat.unref();
  app.addHook('onClose', async () => clearInterval(heartbeat));
  let pending: NodeJS.Timeout | null = null;
  /**
   * Browsers that aren't keeping up (a phone on a slow link) skip state snapshots rather than
   * queue them without limit in memory; each snapshot is complete, so they catch up with the
   * next one once they've drained. Notifications are small and always sent.
   */
  const lagging = new Set<WebSocket>();
  const broadcast = (ev: ServerEvent) => {
    const msg = JSON.stringify(ev);
    for (const ws of clients) {
      if (ws.readyState !== ws.OPEN) continue;
      if (ev.type === 'state' && ws.bufferedAmount > MAX_BUFFERED_STATE) {
        if (!lagging.has(ws)) catchUp(ws);
        continue;
      }
      ws.send(msg);
    }
  };
  /** Sends the current state to a lagging socket once its backlog has drained. */
  const catchUp = (ws: WebSocket) => {
    lagging.add(ws);
    const check = setInterval(() => {
      if (ws.readyState !== ws.OPEN) {
        clearInterval(check);
        lagging.delete(ws);
      } else if (ws.bufferedAmount <= MAX_BUFFERED_STATE / 4) {
        clearInterval(check);
        lagging.delete(ws);
        ws.send(JSON.stringify({ type: 'state', state: core.state() } satisfies ServerEvent));
      }
    }, 250);
    check.unref();
  };
  core.on('change', () => {
    pending ??= setTimeout(() => {
      pending = null;
      if (clients.size) broadcast({ type: 'state', state: core.state() });
    }, 100);
  });
  core.on('notify', (ev: ServerEvent) => broadcast(ev));

  const wsGuard = (socket: WebSocket, req: FastifyRequest): boolean => {
    if (!sameOrigin(req) || !authed(req)) {
      socket.close(4401, 'unauthorized');
      return false;
    }
    const entry = { token: local(req) ? null : req.cookies[COOKIE_NAME]!, alive: true };
    sockets.set(socket, entry);
    socket.on('pong', () => (entry.alive = true));
    socket.on('close', () => sockets.delete(socket));
    return true;
  };

  app.get('/ws/events', { websocket: true }, (socket: WebSocket, req: FastifyRequest) => {
    if (!wsGuard(socket, req)) return;
    clients.add(socket);
    socket.send(JSON.stringify({ type: 'state', state: core.state() } satisfies ServerEvent));
    // A page checking that its socket is still there (src/web/events.ts).
    socket.on('message', (data) => {
      let msg: EventsClientMessage | null;
      try {
        msg = JSON.parse(String(data)) as EventsClientMessage;
      } catch {
        return;
      }
      if (msg?.type === 'ping' && socket.readyState === socket.OPEN) socket.send(JSON.stringify({ type: 'pong' } satisfies ServerEvent));
    });
    socket.on('close', () => clients.delete(socket));
  });

  app.get<{ Params: { id: string }; Querystring: { cols?: string; rows?: string } }>('/ws/term/:id', { websocket: true },
    (socket: WebSocket, req: FastifyRequest<{ Params: { id: string }; Querystring: { cols?: string; rows?: string } }>) => {
    if (!wsGuard(socket, req)) return;
    attachTerminal(core, socket, req.params.id, req.query);
  });

  // ---- web app ----
  if (opts.webDir) {
    // `/` is served by the not-found handler below too, so every page carries this server's version.
    await app.register(fastifyStatic, { root: opts.webDir, wildcard: false, index: false });
    const indexHtml = stampVersion(readFileSync(join(opts.webDir, 'index.html'), 'utf8'), core.version);
    app.setNotFoundHandler((req, reply) => {
      const path = req.url.split('?')[0];
      // A missing file (say /favicon.ico) is a 404, not the app: a browser would take the HTML for the file.
      const missingFile = !path.startsWith('/p/') && /\.[a-z0-9]+$/i.test(path);
      if ((req.method !== 'GET' && req.method !== 'HEAD') || path.startsWith('/api/') || path.startsWith('/ws/') || missingFile) {
        return reply.code(404).send({ error: 'Not found.' });
      }
      return reply.type('text/html').header('Cache-Control', 'no-cache').send(indexHtml);
    });
  }

  return app;
}

/**
 * Adds the server's version to the page as `<meta name="ak-version">`: an open page that sees the
 * server report another version reloads itself (src/web/reload.ts).
 */
export function stampVersion(html: string, version: string): string {
  const v = version.replace(/[&"<>]/g, (c) => `&#${c.charCodeAt(0)};`);
  return html.replace('</head>', `  <meta name="ak-version" content="${v}" />\n  </head>`);
}

export function findWebDir(serverDir: string): string | null {
  // dist/server → dist/web; src/server (tsx dev) → dist/web at the repo root.
  for (const c of [join(serverDir, '..', 'web'), join(serverDir, '..', '..', 'dist', 'web')]) {
    if (existsSync(join(c, 'index.html')) && existsSync(join(c, 'assets'))) return c;
  }
  return null;
}
