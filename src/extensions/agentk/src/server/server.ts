import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import Fastify, { type FastifyInstance, type FastifyRequest } from 'fastify';
import fastifyStatic from '@fastify/static';
import fastifyWebsocket from '@fastify/websocket';
import type { WebSocket } from 'ws';
import type {
  AttachDatapackRequest, CreateProjectRequest, CreateSessionRequest, DatapackPeople, DesignDoc, DiscardCheck, InviteCheck,
  ChangedText, EventsClientMessage, HarnessModels, HarnessStatus, KnowledgeUpload, MekoDatapack, MoveSessionRequest, PastedImage, ReviewComment, SendCommentsResult, ServerEvent, SessionChanges, SessionAction, Settings, SettingsUpdate, SetDedicatedDatapackRequest, StagedImage,
  HeldReview, HeldReviewsResult, Workshop, WorkshopStart, ShareContact, ShareDatapackRequest, ShareDatapackResult, ShareFailure, UpdateProjectRequest, UpdateSessionRequest, CloneRepoRequest, PrReviewers, RepoInfo, ReviewablePrs, TaskPrs,
  AskAgentkRequest, AskAgentkResult, LinkPlan, RunActionsRequest, RunActionsResult, SessionMatch, StartFromLinkRequest, StartFromLinkResult,
  JoinByLinkRequest, JoinByLinkResult, LinkGrant, ShareLink, ShareLinkChange, ShareLinkState,
  CloseWorkItemsRequest, CreateWorkItemRequest, ReorderWorkItemsRequest, UpdateWorkItemRequest, WorkItem, WorkLinkMatch, WorkSync,
} from '../shared/types.js';
import { ACTIONS_MAX, LINK_GRANTS, MAX_IDLE_STOP_HOURS, MAX_KB_UPLOAD_BYTES, MAX_PASTE_BYTES, SHARE_GRANTS, WORK_SYNC_MINUTES_MAX } from '../shared/types.js';
import { AGENTK_PATH, shareLinkToken, shareLinkUrl } from '../shared/mekourls.js';
import { HARNESS_IDS, isHarnessId } from '../shared/harnesses.js';
import { EMAIL_RE, MAX_SHARE_EMAILS } from '../shared/emails.js';
import { MAX_DEPTH, MAX_ROUNDS, STRATEGIES, isStrategySetting, type CodeReviewDetail, type CodeReviewFinding } from '../shared/codereview.js';
import { checkActions, runActions } from './actions.js';
import { ybCallHomeDisabled } from './callhome.js';
import { forgetContact, learnNames, listContacts, recordShares } from './contacts.js';
import type { Core } from './core.js';
import { audit, get } from './db.js';
import { errMsg } from './log.js';
import type { MekoDatapackRow } from './meko.js';
import { MekoApiError, asGrant, routeMissing, type MekoShareLinkChange, type RedeemedShareLink, type ShareLinkRow } from './mekoapi.js';
import { globalReview, resolveMekoWebUrl } from './config.js';
import { setUpClaudeMeko, setUpKiroMeko } from './mekosettings.js';
import { prDesignSource } from './prdesign.js';
import { HttpError } from './projects.js';
import { searchSessions } from './sessionsearch.js';
import { slackListId } from './slack.js';
import { attachTerminal, type TermQuery } from './terminal.js';
import { notesRange, type Updates } from './updates.js';
import { usageOverview } from './usage.js';
import { taskPrs } from './taskprs.js';
import { MAX_IDLE_DAYS, MAX_IDLE_PER_REPO } from './worktrees.js';

/** A browser with this much unsent data skips state snapshots until it catches up. */
const MAX_BUFFERED_STATE = 4 * 1024 * 1024;
/** How long opening Settings waits for a read of the Meko on this computer (`meko` itself may take 5 s). */
const LOCAL_MEKO_WAIT_MS = 1500;
/** Every SessionAction, keyed so a new one can't be left out (the route refuses what isn't here). */
const ACTIONS: Record<SessionAction, true> = {
  'approve-design': true, 'cancel-approval': true, 'open-pr': true, merge: true, 'cancel-queued': true,
  resume: true, archive: true, discard: true, 'update-pr': true, 'close-review': true, learn: true,
  'review-run': true, 'review-cancel': true, 'review-retry': true, 'review-open-anyway': true, 'review-send': true,
};
/**
 * The app page's Content-Security-Policy. No script-src: the design panel's Artifact frame is a
 * srcdoc iframe, which takes this policy on top of its own (design-frame.ts), and Artifacts run
 * inline scripts and scripts from two CDNs. What it does stop: images from anywhere but here,
 * data:, blob: and GitHub (a remote image in an outsider's review would tell its author who read
 * it, and when), connections to other servers, plugins, a <base> or a form that points elsewhere,
 * and framing. `host`: the page's Host, for its WebSockets (older Safari doesn't take 'self' for them).
 */
export function appCsp(host: string | undefined): string {
  const ws = host && /^[A-Za-z0-9.:[\]-]+$/.test(host) ? ` wss://${host} ws://${host}` : '';
  return [
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
    "frame-ancestors 'none'",
    `connect-src 'self'${ws}`,
    "img-src 'self' data: blob: https://github.com https://*.githubusercontent.com",
    "font-src 'self' data: https://fonts.gstatic.com",
    "style-src 'self' 'unsafe-inline' https://fonts.googleapis.com",
  ].join('; ');
}

/** How long a closing server waits for a browser's socket to answer its close before dropping it. */
export const SHUTDOWN_CLOSE_MS = 1000;

/** The page learns where Meko serves it from the `<base href>` stamped into it (stampPage). */
const BASE_PATH = AGENTK_PATH;

export interface ServerOptions {
  webDir: string | null;
  /** What AgentK knows of its updates (updates.ts); null in tests, where `core.updateStatus` stands in. */
  updates: Updates | null;
}

export async function buildServer(core: Core, opts: ServerOptions): Promise<FastifyInstance> {
  const app = Fastify({ logger: false, bodyLimit: 1024 * 1024 }) as unknown as FastifyInstance;
  const db = core.db;

  await app.register(fastifyWebsocket, { options: { maxPayload: 1024 * 1024 } });
  // The API only takes JSON. Without a text/plain parser, a plain HTML form (which can't send
  // JSON) can't post to it, even from our own origin.
  app.removeContentTypeParser('text/plain');

  // There's no login here: the server listens only on its unix socket, which only this user (and so
  // Meko, which serves it to browsers at BASE_PATH after its own sign-in) can connect to.
  /**
   * The route a request matched (`/api/sessions/:id`), or undefined when none did. Access checks
   * key on this, never the raw URL: the router decodes `/%61pi/...` to `/api/...`, and an
   * absolute-form target (`GET http://host/api/...`) doesn't start with `/api/` at all.
   */
  const routeOf = (req: FastifyRequest): string | undefined => req.routeOptions.url;
  const isApi = (req: FastifyRequest) => routeOf(req)?.startsWith('/api/') ?? false;

  /**
   * Same-origin check for writes and WebSocket upgrades: another site's page can't act here through a
   * browser that Meko let in. Meko passes the browser's Host and Origin on as they are.
   */
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
    // Every app page (`/`, an app route, or /index.html by name) gets the same policy.
    else if (/^text\/html/.test(String(reply.getHeader('content-type') ?? ''))) reply.header('Content-Security-Policy', appCsp(req.headers.host));
    return payload;
  });

  app.addHook('onRequest', async (req, reply) => {
    const route = routeOf(req);
    if (!route?.startsWith('/api/')) return;
    if (req.method !== 'GET' && !sameOrigin(req)) return reply.code(403).send({ error: 'Cross-origin request refused.' });
  });

  app.setErrorHandler((err: unknown, req, reply) => {
    if (err instanceof HttpError) return reply.code(err.status).send({ error: err.message, ...(err.code ? { code: err.code } : {}) });
    const e = err as { statusCode?: number; message?: string };
    if (e.statusCode && e.statusCode < 500) return reply.code(e.statusCode).send({ error: e.message ?? 'Bad request.' });
    core.log.error(`${req.method} ${req.url} failed`, err);
    return reply.code(500).send({ error: errMsg(err) });
  });

  // ---- health ----
  // pid and startedAt are for `me_koder status`'s headline ("running (pid 7, up 2h4m0s)").
  const startedAt = new Date(Date.now() - process.uptime() * 1000).toISOString();
  app.get('/healthz', async () => ({ ok: true, version: core.version, pid: process.pid, startedAt }));

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
  /** Throws unless the datapack is in a live project here and AgentK can share. */
  const shareable = (datapackId: string): void => {
    const inUse = get(db, `SELECT 1 AS x FROM project_datapacks d JOIN projects p ON p.id = d.project_id
                           WHERE d.datapack_id = ? AND p.deleted_at IS NULL`, datapackId);
    if (!inUse) throw new HttpError(404, "That datapack isn't in any of your projects.");
    core.invites.requireSharing('share datapacks');
  };
  /** Runs a share API call for a datapack one of our projects uses; Meko's errors become HTTP errors (see apiFailure). */
  const sharing = async <T>(datapackId: string, fn: () => Promise<T>): Promise<T> => {
    shareable(datapackId);
    try {
      return await fn();
    } catch (e) {
      throw core.invites.apiFailure(e, 'Meko');
    }
  };
  /** A datapack's lists, naming the share contacts who are members. */
  const people = async (id: string): Promise<DatapackPeople> => {
    const p = await core.invites.api.people(id);
    learnNames(db, p.members);
    return p;
  };
  /** The lists after a change that went through; a failure to read them doesn't make the change look failed. */
  const peopleAfter = (id: string): Promise<DatapackPeople> =>
    people(id).catch((e) => ({ members: [], invites: [], refreshError: `Couldn't read the list again: ${errMsg(e)}` }));
  app.get<{ Params: { id: string } }>('/api/meko/datapacks/:id/people', async (req): Promise<DatapackPeople> =>
    sharing(req.params.id, () => people(req.params.id)));
  app.post<{ Params: { id: string }; Body: ShareDatapackRequest }>('/api/meko/datapacks/:id/people', async (req): Promise<ShareDatapackResult> => {
    const { email, emails: list, grant, login } = req.body ?? ({} as ShareDatapackRequest);
    if ((email === undefined) === (list === undefined)) throw new HttpError(400, 'Give either one email or a list of emails.');
    if (list !== undefined && !Array.isArray(list)) throw new HttpError(400, 'The list of emails must be an array.');
    const typed = (list ?? [email]).map((e) => (typeof e === 'string' ? e.trim() : ''));
    if (!typed.length) throw new HttpError(400, 'Enter the email address of the person to share with.');
    const bad = typed.filter((e) => !EMAIL_RE.test(e));
    if (bad.length) {
      throw new HttpError(400, list === undefined ? 'Enter the email address of the person to share with.'
        : `Not an email: ${bad.map((e) => e || '(blank)').join(', ')}. Fix or remove ${bad.length === 1 ? 'it' : 'them'}.`);
    }
    const emails = typed.filter((e, i) => typed.findIndex((o) => o.toLowerCase() === e.toLowerCase()) === i);
    if (emails.length > MAX_SHARE_EMAILS) throw new HttpError(400, `Invite at most ${MAX_SHARE_EMAILS} people at a time.`);
    if (!SHARE_GRANTS.includes(grant)) throw new HttpError(400, 'Access must be viewer, contributor or maintainer.');
    if (login != null && emails.length !== 1) throw new HttpError(400, 'A GitHub login goes with a single email.');
    shareable(req.params.id);
    // One after another, in the order given. When none go through, the first error is the answer.
    const invited: string[] = [];
    const failed: ShareFailure[] = [];
    let first: Error | null = null;
    for (const [i, to] of emails.entries()) {
      try {
        await core.invites.api.share(req.params.id, to, grant);
        invited.push(to);
        core.log.info(`invited ${to} to datapack ${req.params.id} as ${grant}`);
      } catch (e) {
        const err = core.invites.apiFailure(e, 'Meko');
        first ??= err;
        // A refused key or an unreachable Meko (status 0, after a timeout of up to 30 s) fails the
        // rest the same way: don't send them.
        const rest = e instanceof MekoApiError && (e.status === 401 || e.status === 0) ? emails.slice(i) : [to];
        failed.push(...rest.map((r) => ({ email: r, error: err.message })));
        if (rest.length > 1) break;
      }
    }
    if (!invited.length && first) throw first;
    // Shared with a PR's reviewer: their email comes up for them next time.
    if (login != null) core.prReview.rememberEmail(login, emails[0]);
    // Saving the suggestions must never make invites that went through look failed (and be sent again).
    try {
      recordShares(db, invited);
    } catch (e) {
      core.log.warn("couldn't save the people invited for the Share dialog's suggestions", errMsg(e));
    }
    return { ...(await peopleAfter(req.params.id)), invited, failed };
  });
  app.post<{ Params: { id: string; inviteId: string } }>('/api/meko/datapacks/:id/people/invites/:inviteId/resend', async (req): Promise<DatapackPeople> =>
    sharing(req.params.id, () => core.invites.api.resend(req.params.id, req.params.inviteId)).then(() => peopleAfter(req.params.id)));
  app.delete<{ Params: { id: string; inviteId: string } }>('/api/meko/datapacks/:id/people/invites/:inviteId', async (req): Promise<DatapackPeople> =>
    sharing(req.params.id, () => core.invites.api.revoke(req.params.id, req.params.inviteId)).then(() => peopleAfter(req.params.id)));

  // ---- share links (Meko's: one per datapack; anyone signed in who opens it joins) ----
  const noWebUrl = (what: string) =>
    new HttpError(409, `Meko's web app URL isn't known, so AgentK can't ${what}. Set it in Meko's web UI (Settings › Meko Cloud).`);
  /** For the browser: the link on Meko's web app while it's on. The token goes nowhere else, the log included. */
  const shareLinkOut = (l: ShareLinkRow, webUrl: string): ShareLink => ({
    active: l.active,
    grant: l.grant,
    url: l.active && l.token ? shareLinkUrl(webUrl, l.token) : null,
    joined: l.joined,
    atLinkRole: l.atLinkRole,
  });
  app.get<{ Params: { id: string } }>('/api/meko/datapacks/:id/share-link', async (req): Promise<ShareLinkState> => {
    shareable(req.params.id);
    const webUrl = resolveMekoWebUrl(core.config);
    if (!webUrl) return { link: null, unavailable: 'no-web-url' };
    try {
      return { link: shareLinkOut(await core.invites.api.shareLink(req.params.id), webUrl) };
    } catch (e) {
      if (e instanceof MekoApiError && e.status === 403) return { link: null, unavailable: 'permission' };
      if (routeMissing(e)) return { link: null, unavailable: 'unsupported' };
      throw core.invites.apiFailure(e, 'Meko');
    }
  });
  app.put<{ Params: { id: string }; Body: ShareLinkChange }>('/api/meko/datapacks/:id/share-link', async (req): Promise<ShareLink> => {
    const b = (req.body ?? {}) as Record<string, unknown>;
    const keys = Object.keys(b).sort().join(',');
    if (b.grant !== undefined && !LINK_GRANTS.includes(b.grant as LinkGrant)) throw new HttpError(400, 'A share link gives viewer or contributor access.');
    const grant = b.grant as LinkGrant;
    // Meko takes one change per call; turning on with a role is two.
    let change: MekoShareLinkChange;
    let what: string;
    if (keys === 'active,grant' && b.active === true) [change, what] = [{ status: 'active' }, `turned on as ${grant}`];
    else if (keys === 'active' && b.active === false) [change, what] = [{ status: 'off' }, 'turned off'];
    else if (keys === 'grant') [change, what] = [{ grant }, `set to ${grant}`];
    else if (keys === 'reset' && b.reset === true) [change, what] = [{ reset: true }, 'reset'];
    else throw new HttpError(400, 'Make one change: turn the link on with a role, turn it off, change its role, or reset it.');
    shareable(req.params.id);
    const webUrl = resolveMekoWebUrl(core.config);
    if (!webUrl) throw noWebUrl('share by link');
    return sharing(req.params.id, async () => {
      let link = await core.invites.api.setShareLink(req.params.id, change);
      // Turned on, then given the role picked. Anyone who joins in between moves with the role change.
      if (b.active === true && link.grant !== grant) link = await core.invites.api.setShareLink(req.params.id, { grant });
      core.log.info(`share link of datapack ${req.params.id} ${what}`);
      return shareLinkOut(link, webUrl);
    });
  });
  app.post<{ Body: JoinByLinkRequest }>('/api/meko/share-links/join', async (req): Promise<JoinByLinkResult> => {
    core.invites.requireSharing('join datapacks by link');
    const webUrl = resolveMekoWebUrl(core.config);
    if (!webUrl) throw noWebUrl('join datapacks by link');
    // Only a link on our own Meko's web app: a look-alike link's token never reaches Meko.
    const token = shareLinkToken(typeof req.body?.url === 'string' ? req.body.url : '', webUrl);
    if (!token) throw new HttpError(400, `That isn't a share link from ${new URL(webUrl).host}.`);
    let joined: RedeemedShareLink;
    try {
      joined = await core.invites.api.redeemShareLink(token);
    } catch (e) {
      if (e instanceof MekoApiError && e.code === 'REMOVED_FROM_DATAPACK') {
        throw new HttpError(403, "You were removed from this datapack, so its link doesn't let you back in. Ask its owner to invite you again.");
      }
      if (routeMissing(e)) throw new HttpError(409, "This Meko doesn't have share links.");
      if (e instanceof MekoApiError && e.status === 404) {
        throw new HttpError(404, "This link doesn't work any more: its owner may have turned it off or reset it. Ask them for a new one.");
      }
      throw core.invites.apiFailure(e, "Meko couldn't join you through that link");
    }
    const id = joined.datapackId;
    core.invites.joinedHere(id);
    core.log.info(`joined datapack ${id} through a share link${joined.alreadyMember ? ' (already a member)' : ''}`);
    // The share as the Open project dialog offers it. joinedHere() made any sync already running
    // read Meko again, so the rows sync() uses are from after the join, ours or its own.
    let listed: MekoDatapackRow | undefined;
    try {
      const stamp = core.invites.stamp();
      const rows = await core.meko.listDatapacks();
      listed = rows.find((r) => r.id === id);
      await core.invites.sync({ rows, stamp });
    } catch (e) {
      core.log.warn("joined a datapack through a share link, but couldn't read the datapack list", errMsg(e));
    }
    const project = get<{ id: string; name: string }>(db, `SELECT p.id, p.name FROM project_datapacks d JOIN projects p ON p.id = d.project_id
                           WHERE d.datapack_id = ? AND p.deleted_at IS NULL ORDER BY d.role = 'dedicated' DESC, p.position LIMIT 1`, id) ?? null;
    // Our access as the datapack list has it (a maintainer may paste a viewer link), else the link's.
    const grant: JoinByLinkResult['grant'] = listed?.grant === 'owner' ? 'owner' : asGrant(listed?.grant) ?? joined.grant;
    const share = project ? null : core.invites.openShare(id);
    return {
      datapackId: id,
      name: listed?.name ?? share?.name ?? null,
      grant,
      alreadyMember: joined.alreadyMember,
      invite: share && grant !== 'viewer' ? share : null,
      project,
    };
  });

  // The people invited before, suggested in the Share dialog. Local only, so they don't need Meko.
  app.get('/api/share-contacts', async (): Promise<ShareContact[]> => listContacts(db));
  app.delete<{ Params: { email: string } }>('/api/share-contacts/:email', async (req, reply) => {
    if (!forgetContact(db, req.params.email)) throw new HttpError(404, "That person isn't in the suggestions any more.");
    return reply.code(204).send();
  });

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

  // ---- workshops (bundled tours, src/server/workshops.ts) ----
  app.get('/api/workshops', async (): Promise<Workshop[]> => core.workshops.list());
  app.post<{ Params: { id: string } }>('/api/workshops/:id/start', async (req): Promise<WorkshopStart> => core.workshops.start(req.params.id));
  app.post<{ Params: { id: string } }>('/api/workshops/:id/go', async (req, reply) => {
    await core.workshops.go(req.params.id);
    return reply.code(204).send();
  });

  // ---- projects ----
  app.post<{ Body: CreateProjectRequest }>('/api/projects', async (req) => core.projects.open(req.body));
  app.get<{ Params: { id: string } }>('/api/projects/:id', async (req) => core.projects.get(req.params.id));
  app.patch<{ Params: { id: string }; Body: UpdateProjectRequest }>('/api/projects/:id', async (req) => core.projects.update(req.params.id, req.body ?? {}));
  app.delete<{ Params: { id: string } }>('/api/projects/:id', async (req, reply) => {
    await core.projects.delete(req.params.id);
    return reply.code(204).send();
  });
  app.get<{ Params: { id: string } }>('/api/projects/:id/branches', async (req) => core.projects.branches(req.params.id));
  app.get<{ Params: { id: string }; Querystring: { repoPath?: string; base?: string; where?: string; months?: string } }>(
    '/api/projects/:id/learn-scope', async (req) => core.projects.learnScope(req.params.id, req.query));
  app.post<{ Params: { id: string }; Body: AttachDatapackRequest }>('/api/projects/:id/datapacks', async (req) => core.projects.attachDatapack(req.params.id, req.body));
  app.post<{ Params: { id: string }; Body: SetDedicatedDatapackRequest }>('/api/projects/:id/dedicated-datapack', async (req) =>
    core.projects.setDedicatedDatapack(req.params.id, req.body));
  app.delete<{ Params: { id: string; dpId: string } }>('/api/projects/:id/datapacks/:dpId', async (req) => core.projects.detachDatapack(req.params.id, req.params.dpId));
  app.get<{ Params: { id: string } }>('/api/projects/:id/home', async (req) => core.projects.home(req.params.id));
  app.get<{ Params: { id: string } }>('/api/projects/:id/pr-reviewers', async (req): Promise<PrReviewers[]> => core.prReview.reviewers(req.params.id));
  app.get<{ Params: { id: string }; Querystring: { pr?: string; repoPath?: string } }>('/api/projects/:id/review-prs', async (req): Promise<ReviewablePrs> =>
    core.prReview.reviewable(req.params.id, { pr: req.query.pr, repoPath: req.query.repoPath }));
  app.get<{ Params: { id: string }; Querystring: { pr?: string; repoPath?: string } }>('/api/projects/:id/task-prs', async (req): Promise<TaskPrs> =>
    taskPrs(core, req.params.id, { pr: req.query.pr, repoPath: req.query.repoPath }));
  app.get<{ Querystring: { ref?: string; project?: string } }>('/api/github-link', async (req): Promise<LinkPlan> =>
    core.githubLinks.plan(req.query.ref, req.query.project));
  app.post<{ Body: StartFromLinkRequest }>('/api/github-link/start', async (req): Promise<StartFromLinkResult> =>
    core.githubLinks.start(req.body && typeof req.body === 'object' ? req.body : ({} as StartFromLinkRequest)));
  // Claude answers while the request is open: a browser that goes away (the palette closed) stops it.
  app.post<{ Body: AskAgentkRequest }>('/api/ask-agentk', async (req, reply): Promise<AskAgentkResult> => {
    const gone = new AbortController();
    reply.raw.on('close', () => {
      if (!reply.raw.writableFinished) gone.abort();
    });
    return core.askAgentk.ask(req.body, gone.signal);
  });
  // Ask AgentK's Do all: checked again against the state now, then run in order.
  app.post<{ Body: RunActionsRequest }>('/api/actions/run', async (req): Promise<RunActionsResult> => {
    const raw = req.body && typeof req.body === 'object' && Array.isArray(req.body.actions) ? req.body.actions : [];
    if (raw.length > ACTIONS_MAX) throw new HttpError(400, `Run at most ${ACTIONS_MAX} actions at once.`);
    const { actions, notes } = checkActions(raw.filter((a) => a?.type !== 'go'), core.actionProjects());
    if (!actions.length) throw new HttpError(400, notes[0] ?? 'There was nothing to run.');
    return { results: await runActions(core, actions, { session: null }), notes };
  });
  app.get('/api/usage', async () => usageOverview(core.db));

  // ---- work queue ----
  app.get<{ Querystring: { status?: string } }>('/api/work-items', async (req): Promise<WorkItem[]> =>
    req.query.status === 'closed' ? core.workQueue.closed() : core.workQueue.list());
  app.post<{ Body: CreateWorkItemRequest }>('/api/work-items', async (req): Promise<WorkItem> => core.workQueue.create(req.body ?? {}));
  app.patch<{ Params: { id: string }; Body: UpdateWorkItemRequest }>('/api/work-items/:id', async (req): Promise<WorkItem> =>
    core.workQueue.update(req.params.id, req.body ?? {}));
  app.put<{ Body: ReorderWorkItemsRequest }>('/api/work-items/order', async (req, reply) => {
    core.workQueue.reorder(req.body ?? { ids: [] });
    return reply.code(204).send();
  });
  app.post<{ Body: CloseWorkItemsRequest }>('/api/work-items/close', async (req): Promise<{ closed: number }> => ({ closed: core.workQueue.closeAll(req.body) }));
  app.post('/api/work-items/sync', async (): Promise<WorkSync> => core.workQueue.sync(true));
  app.get<{ Querystring: { link?: string } }>('/api/work-items/by-link', async (req): Promise<WorkLinkMatch[]> => core.workQueue.byLink(req.query.link));
  app.get<{ Querystring: { q?: string; project?: string } }>('/api/sessions/search', async (req): Promise<SessionMatch[]> =>
    searchSessions(core.db, req.query.q ?? '', req.query.project || undefined));
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
  app.post<{ Params: { id: string; action: string }; Body: unknown }>('/api/sessions/:id/actions/:action', async (req) => {
    const action = req.params.action as SessionAction;
    if (!Object.hasOwn(ACTIONS, action)) throw new HttpError(400, `Unknown action ${action}.`);
    return core.sessions.action(req.params.id, action, req.body);
  });
  app.patch<{ Params: { id: string }; Body: UpdateSessionRequest | null }>('/api/sessions/:id', async (req) =>
    core.sessions.edit(req.params.id, req.body && typeof req.body === 'object' ? req.body : {}));
  app.get<{ Params: { id: string } }>('/api/sessions/:id/base-branches', async (req) => core.sessions.baseBranches(req.params.id));
  app.post<{ Params: { id: string }; Body: MoveSessionRequest }>('/api/sessions/:id/move', async (req) =>
    core.sessions.move(req.params.id, req.body));
  app.get<{ Params: { id: string } }>('/api/sessions/:id/code-review', async (req): Promise<CodeReviewDetail> => {
    core.sessions.row(req.params.id);
    return core.reviews.detail(req.params.id);
  });
  app.patch<{ Params: { id: string; key: string }; Body: unknown }>('/api/sessions/:id/code-review/findings/:key', async (req): Promise<CodeReviewFinding> => {
    core.sessions.row(req.params.id);
    return core.reviews.setFinding(req.params.id, req.params.key, req.body);
  });
  app.get<{ Params: { id: string } }>('/api/sessions/:id/discard-check', async (req): Promise<DiscardCheck> => core.sessions.discardCheck(req.params.id));
  app.get<{ Params: { id: string }; Querystring: { commit?: string } }>('/api/sessions/:id/changes', async (req): Promise<SessionChanges> =>
    core.sessions.changes(req.params.id, req.query.commit || undefined));
  app.get<{ Params: { id: string }; Querystring: { path?: string; side?: string; commit?: string } }>('/api/sessions/:id/changes/image', async (req, reply) => {
    const { path = '', side, commit } = req.query;
    if (side !== 'old' && side !== 'new') throw new HttpError(400, "side must be 'old' or 'new'.");
    const img = await core.sessions.changedImage(req.params.id, path, side, commit || undefined);
    // Bytes from the repo: never let the browser run or sniff them, even when opened directly.
    return reply
      .type(img.type)
      .header('Content-Security-Policy', "default-src 'none'; sandbox")
      .header('Content-Disposition', 'inline')
      .send(img.data);
  });
  app.get<{ Params: { id: string }; Querystring: { path?: string; old?: string; commit?: string } }>('/api/sessions/:id/changes/file', async (req): Promise<ChangedText> =>
    core.sessions.changedText(req.params.id, req.query.path ?? '', req.query.old || null, req.query.commit || undefined));
  // An image pasted into the terminal: the body is the image itself. Its parser lives only in this
  // scope, so no other route takes a body that isn't JSON (and an HTML form can't send image/*).
  await app.register(async (scope) => {
    scope.addContentTypeParser(/^image\//, { parseAs: 'buffer', bodyLimit: MAX_PASTE_BYTES }, (_req, body, done) => done(null, body));
    scope.post<{ Params: { id: string } }>('/api/sessions/:id/images', async (req): Promise<PastedImage> => core.sessions.pasteImage(req.params.id, req.body));
    // One added in the New session dialog, before there's a session: kept under the project until one starts with it.
    scope.post<{ Params: { id: string } }>('/api/projects/:id/images', async (req): Promise<StagedImage> => core.sessions.stageImage(req.params.id, req.body));
  });
  // A file for Claude, or for the session's knowledge base: the body is the file. As with images,
  // the parser lives only in this scope; application/octet-stream is a type no HTML form can send.
  await app.register(async (scope) => {
    scope.addContentTypeParser('application/octet-stream', { parseAs: 'buffer', bodyLimit: MAX_KB_UPLOAD_BYTES }, (_req, body, done) => done(null, body));
    scope.post<{ Params: { id: string }; Querystring: { name?: string } }>('/api/sessions/:id/files',
      async (req): Promise<PastedImage> => core.sessions.pasteFile(req.params.id, req.body, req.query.name));
    scope.post<{ Params: { id: string }; Querystring: { name?: string } }>('/api/sessions/:id/knowledge',
      async (req): Promise<KnowledgeUpload> => core.knowledge.upload(req.params.id, req.query.name, req.body));
  });
  app.get<{ Params: { id: string } }>('/api/sessions/:id/comments', async (req): Promise<ReviewComment[]> => core.comments.list(req.params.id));
  app.post<{ Params: { id: string }; Body: unknown }>('/api/sessions/:id/comments', async (req): Promise<ReviewComment> => core.comments.add(req.params.id, req.body));
  app.post<{ Params: { id: string } }>('/api/sessions/:id/comments/send', async (req): Promise<SendCommentsResult> => core.comments.send(req.params.id));
  app.get<{ Params: { id: string } }>('/api/sessions/:id/held-reviews', async (req): Promise<HeldReview[]> => core.lifecycle.heldReviews(req.params.id));
  app.post<{ Params: { id: string }; Body: unknown }>('/api/sessions/:id/held-reviews',
    async (req): Promise<HeldReviewsResult> => core.lifecycle.decideHeldReviews(req.params.id, req.body));
  app.patch<{ Params: { id: string; cid: string }; Body: { body?: unknown } }>('/api/sessions/:id/comments/:cid',
    async (req): Promise<ReviewComment> => core.comments.edit(req.params.id, req.params.cid, req.body?.body));
  app.delete<{ Params: { id: string; cid: string } }>('/api/sessions/:id/comments/:cid', async (req, reply) => {
    core.comments.remove(req.params.id, req.params.cid);
    return reply.code(204).send();
  });
  app.get<{ Params: { id: string } }>('/api/sessions/:id/design', async (req): Promise<DesignDoc> => {
    const row = core.sessions.row(req.params.id);
    const r = get<{ markdown: string; html: string | null; artifact_url: string | null; updated_at: number }>(
      db, 'SELECT markdown, html, artifact_url, updated_at FROM designs WHERE session_id = ?', req.params.id);
    const q = core.questions.get(row);
    return {
      markdown: r?.markdown || null, html: r?.html ?? null, artifactUrl: r?.artifact_url ?? null, updatedAt: r?.updated_at ?? null,
      questions: q.questions, questionsSource: q.source, fromPr: prDesignSource(core, row.id)?.ref.name ?? null,
    };
  });
  app.post<{ Params: { id: string }; Body: unknown }>('/api/sessions/:id/design/answers', async (req, reply) => {
    await core.lifecycle.reviseDesign(core.sessions.row(req.params.id), req.body);
    return reply.code(204).send();
  });
  app.post<{ Params: { id: string; askId: string }; Body: unknown }>('/api/sessions/:id/ask/:askId', async (req, reply) => {
    core.sessions.answerAsk(req.params.id, req.params.askId, req.body);
    return reply.code(204).send();
  });

  // ---- settings ----
  const settings = (): Settings => ({
    reposRoots: core.config.repos.roots,
    reposDepth: core.config.repos.depth,
    cloneInto: core.config.repos.cloneInto,
    serverName: core.config.server.name,
    hostName: core.hostName(),
    idleWorktreesPerRepo: core.worktrees.cap(),
    idleWorktreeDays: core.worktrees.idleDays(),
    idleStopHours: core.config.sessions.idleStopHours,
    callHomeEnabled: core.config.callHome.enabled,
    ...(ybCallHomeDisabled() ? { callHomeOffBy: 'YB_DISABLE_CALLHOME' as const } : {}),
    resolveConflicts: core.config.pr.resolveConflicts,
    toolsEnabled: core.config.tools.enabled,
    defaultHarness: core.config.agents.default,
    claudeEnabled: core.config.claude.enabled,
    opencodeEnabled: core.config.opencode.enabled,
    opencodeDisableTracking: core.config.opencode.disableTracking,
    kiroEnabled: core.config.kiro.enabled,
    kiroTrustTools: core.config.kiro.trustTools,
    workQueueGithub: core.config.workQueue.github,
    workQueueSlack: core.config.workQueue.slack,
    workQueueSlackList: core.config.workQueue.slackList,
    workQueueSyncMinutes: core.config.workQueue.syncMinutes,
    codeReviewStrategy: globalReview(core.config).strategy,
    codeReviewRounds: core.config.codeReview.rounds,
    codeReviewDepth: core.config.codeReview.depth,
    codeReviewCheck: core.config.codeReview.check,
  });
  app.get('/api/settings', async () => {
    // Opening Settings reads the Meko on this computer and its Meko Cloud link again (it may have
    // started, stopped, moved or been linked), waiting a moment for it (a `meko` that's slow finishes
    // in the background and pushes the change), then Meko's list of agents, and checks Claude Code's
    // Meko again (both in the background; the check reuses recent results).
    const read = core.refreshMeko().catch((e: unknown) => core.log.warn('reading the Meko on this computer failed', errMsg(e)));
    await Promise.race([read, new Promise((r) => setTimeout(r, LOCAL_MEKO_WAIT_MS).unref())]);
    void read.then(() => core.mekoAgents.refresh());
    void core.claudeMeko.refresh();
    return settings();
  });
  app.put<{ Body: SettingsUpdate }>('/api/settings', async (req) => {
    const b = req.body ?? {};
    if (b.reposRoots !== undefined && (!Array.isArray(b.reposRoots) || !b.reposRoots.length || b.reposRoots.some((r) => typeof r !== 'string' || !r.trim()))) {
      throw new HttpError(400, 'List at least one repos folder.');
    }
    if (b.reposDepth !== undefined && (!Number.isInteger(b.reposDepth) || b.reposDepth < 1 || b.reposDepth > 4)) throw new HttpError(400, 'Scan depth must be between 1 and 4.');
    if (b.cloneInto !== undefined && typeof b.cloneInto !== 'string') throw new HttpError(400, 'The clone folder must be a path.');
    if (b.idleWorktreesPerRepo !== undefined && (!Number.isInteger(b.idleWorktreesPerRepo) || b.idleWorktreesPerRepo < 0 || b.idleWorktreesPerRepo > MAX_IDLE_PER_REPO)) {
      throw new HttpError(400, `Idle worktrees per repo must be a whole number from 0 to ${MAX_IDLE_PER_REPO}.`);
    }
    if (b.idleWorktreeDays !== undefined && (!Number.isInteger(b.idleWorktreeDays) || b.idleWorktreeDays < 0 || b.idleWorktreeDays > MAX_IDLE_DAYS)) {
      throw new HttpError(400, `Days to keep an unused idle worktree must be a whole number from 0 to ${MAX_IDLE_DAYS}.`);
    }
    if (b.idleStopHours !== undefined && (!Number.isInteger(b.idleStopHours) || b.idleStopHours < 0 || b.idleStopHours > MAX_IDLE_STOP_HOURS)) {
      throw new HttpError(400, `Hours before an idle agent is stopped must be a whole number from 0 to ${MAX_IDLE_STOP_HOURS}.`);
    }
    // eslint-disable-next-line no-control-regex
    if (b.serverName !== undefined && (typeof b.serverName !== 'string' || b.serverName.trim().length > 64 || /[\u0000-\u001f\u007f]/.test(b.serverName))) {
      throw new HttpError(400, 'Server name must be 64 characters or fewer, with no line breaks.');
    }
    if (b.callHomeEnabled !== undefined && typeof b.callHomeEnabled !== 'boolean') throw new HttpError(400, 'callHomeEnabled must be true or false.');
    if (b.resolveConflicts !== undefined && typeof b.resolveConflicts !== 'boolean') throw new HttpError(400, 'resolveConflicts must be true or false.');
    if (b.toolsEnabled !== undefined && typeof b.toolsEnabled !== 'boolean') throw new HttpError(400, 'toolsEnabled must be true or false.');
    if (b.defaultHarness !== undefined && !isHarnessId(b.defaultHarness)) throw new HttpError(400, `The default agent must be one of ${HARNESS_IDS.join(', ')}.`);
    if (b.workQueueSyncMinutes !== undefined && (!Number.isInteger(b.workQueueSyncMinutes) || b.workQueueSyncMinutes < 0 || b.workQueueSyncMinutes > WORK_SYNC_MINUTES_MAX)) {
      throw new HttpError(400, `Sync every must be a whole number of minutes from 0 to ${WORK_SYNC_MINUTES_MAX}.`);
    }
    if (b.workQueueSlackList !== undefined && (typeof b.workQueueSlackList !== 'string' || (b.workQueueSlackList.trim() && !slackListId(b.workQueueSlackList)))) {
      throw new HttpError(400, "That isn't a link to a Slack list: in the list, use ⋯ › Copy link.");
    }
    if (b.codeReviewStrategy !== undefined && !isStrategySetting(b.codeReviewStrategy)) {
      throw new HttpError(400, `The review strategy must be off or one of ${STRATEGIES.map((x) => x.id).join(', ')}.`);
    }
    if (b.codeReviewRounds !== undefined && (!Number.isInteger(b.codeReviewRounds) || b.codeReviewRounds < 0 || b.codeReviewRounds > MAX_ROUNDS)) {
      throw new HttpError(400, `Re-review rounds must be a whole number from 0 to ${MAX_ROUNDS}.`);
    }
    if (b.codeReviewDepth !== undefined && (!Number.isInteger(b.codeReviewDepth) || b.codeReviewDepth < 1 || b.codeReviewDepth > MAX_DEPTH)) {
      throw new HttpError(400, `Review depth must be a whole number from 1 to ${MAX_DEPTH}.`);
    }
    for (const k of ['claudeEnabled', 'opencodeEnabled', 'opencodeDisableTracking', 'kiroEnabled', 'kiroTrustTools', 'workQueueGithub', 'workQueueSlack', 'codeReviewCheck'] as const) {
      if (b[k] !== undefined && typeof b[k] !== 'boolean') throw new HttpError(400, `${k} must be true or false.`);
    }
    const slackListBefore = core.config.workQueue.slackList;
    const enabledBefore = Object.fromEntries(HARNESS_IDS.map((id) => [id, core.config[id].enabled]));
    const harnessBefore = Object.fromEntries(HARNESS_IDS.map((id) => [id, JSON.stringify(core.config[id])]));
    const before = { cap: core.worktrees.cap(), days: core.worktrees.idleDays() };
    core.updateConfig((c) => {
      if (b.reposRoots) c.repos.roots = b.reposRoots.map((r) => r.trim());
      if (b.reposDepth) c.repos.depth = b.reposDepth;
      if (b.cloneInto?.trim()) c.repos.cloneInto = b.cloneInto.trim();
      if (b.serverName !== undefined) c.server.name = b.serverName.trim();
      if (b.idleWorktreesPerRepo !== undefined) c.worktrees.idlePerRepo = b.idleWorktreesPerRepo;
      if (b.idleWorktreeDays !== undefined) c.worktrees.idleDays = b.idleWorktreeDays;
      if (b.idleStopHours !== undefined) c.sessions.idleStopHours = b.idleStopHours;
      if (b.callHomeEnabled !== undefined) c.callHome.enabled = b.callHomeEnabled;
      if (b.resolveConflicts !== undefined) c.pr.resolveConflicts = b.resolveConflicts;
      if (b.toolsEnabled !== undefined) c.tools.enabled = b.toolsEnabled;
      if (b.defaultHarness !== undefined) c.agents.default = b.defaultHarness;
      if (b.claudeEnabled !== undefined) c.claude.enabled = b.claudeEnabled;
      if (b.opencodeEnabled !== undefined) c.opencode.enabled = b.opencodeEnabled;
      if (b.opencodeDisableTracking !== undefined) c.opencode.disableTracking = b.opencodeDisableTracking;
      if (b.kiroEnabled !== undefined) c.kiro.enabled = b.kiroEnabled;
      if (b.kiroTrustTools !== undefined) c.kiro.trustTools = b.kiroTrustTools;
      if (b.workQueueGithub !== undefined) c.workQueue.github = b.workQueueGithub;
      if (b.workQueueSlack !== undefined) c.workQueue.slack = b.workQueueSlack;
      if (b.workQueueSlackList !== undefined) c.workQueue.slackList = b.workQueueSlackList.trim();
      if (b.workQueueSyncMinutes !== undefined) c.workQueue.syncMinutes = b.workQueueSyncMinutes;
      if (b.codeReviewStrategy !== undefined) c.codeReview.strategy = b.codeReviewStrategy;
      if (b.codeReviewRounds !== undefined) c.codeReview.rounds = b.codeReviewRounds;
      if (b.codeReviewDepth !== undefined) c.codeReview.depth = b.codeReviewDepth;
      if (b.codeReviewCheck !== undefined) c.codeReview.check = b.codeReviewCheck;
    });
    if (core.config.workQueue.slackList !== slackListBefore) core.workQueue.slackListChanged();
    // An agent turned on: its version (and Kiro's agent file) follow, in the background. One turned
    // off forgets its last read, so it isn't offered on that read once it's turned on again.
    // Another of its own settings changed: its files follow now, not at its next launch (Kiro's
    // agentk-trusted goes as soon as [kiro] trust_tools is off).
    for (const id of HARNESS_IDS) {
      const h = core.harnesses.of(id);
      const prepare = () => h.prepare(core.paths, core.config).catch((e) => core.log.warn(`preparing ${h.caps.label} failed`, errMsg(e)));
      if (core.config[id].enabled === enabledBefore[id]) {
        if (JSON.stringify(core.config[id]) !== harnessBefore[id]) void prepare();
        continue;
      }
      if (!core.config[id].enabled) void h.version(core.config);
      else void h.version(core.config, true).then(prepare);
    }
    // Removing build trees can take a while, so it runs in the background, and only when a limit went down.
    const cleanup = (what: string, fn: () => Promise<void>) => void fn().catch((e) => core.log.warn(`${what} failed`, errMsg(e)));
    if (core.worktrees.cap() < before.cap) cleanup('trimming idle worktrees', () => core.worktrees.trimAll());
    const days = core.worktrees.idleDays();
    if (days && (!before.days || days < before.days)) cleanup('removing unused idle worktrees', () => core.worktrees.expire());
    return settings();
  });
  // The page connects the agent's `meko` MCP server through Meko's own route first; these run Meko's
  // setup package only for what Meko's capture needs (nothing when that's all there).
  app.post('/api/settings/meko/claude-setup', async (): Promise<Settings> => {
    try {
      if (await setUpClaudeMeko(core, core.mekoTools)) audit(db, 'meko_claude_setup', { url: core.mekoStatus.url });
    } finally {
      // Whatever the setup managed, the banner shows what's left (the saved token results are
      // stale now; the setup writes the MCP server too). In the background: the answer doesn't wait
      // on Meko, the state stream updates.
      void core.claudeMeko.refresh(true);
      void core.mekoAgents.refresh();
    }
    return settings();
  });

  app.post('/api/settings/meko/kiro-setup', async (): Promise<Settings> => {
    try {
      if (await setUpKiroMeko(core, core.mekoTools)) audit(db, 'meko_kiro_setup', { url: core.mekoStatus.url });
    } finally {
      void core.mekoAgents.refresh();
      core.kiroMeko(true);
      // Meko's setup makes its agent Kiro's default: Kiro sessions' agent is built from it again.
      await core.harnesses.of('kiro').prepare(core.paths, core.config).catch((e) => core.log.warn('preparing Kiro failed', errMsg(e)));
      core.changed();
    }
    return settings();
  });

  // Meko, on AgentK's socket: something AgentK reads from Meko changed (`cloud`: the Meko Cloud link).
  // Outside /api, so the same-origin rule is checked here; all it does is read Meko again.
  app.post<{ Body: { what?: unknown } }>('/_meko/changed', async (req, reply) => {
    if (!sameOrigin(req)) return reply.code(403).send({ error: 'Cross-origin request refused.' });
    if (req.body?.what === 'cloud') void core.refreshMeko().catch((e: unknown) => core.log.warn('reading Meko\'s Meko Cloud link failed', errMsg(e)));
    return reply.code(204).send();
  });

  // ---- updates ----
  app.post('/api/claude/update', async () => core.claudeCode.update());
  // Check again (Settings → Harness), and Connect to Meko after the page connected an agent through
  // Meko's own route: a version or Meko list that changed reaches every page through `changed()`.
  app.post('/api/harnesses/refresh', async (): Promise<HarnessStatus[]> => core.refreshHarnesses(true));
  // New session's Model choices for an agent whose list comes from its CLI (OpenCode's, Kiro's).
  app.get<{ Params: { id: string } }>('/api/harnesses/:id/models', async (req): Promise<HarnessModels> => {
    if (!isHarnessId(req.params.id)) throw new HttpError(404, 'There is no such agent.');
    return { models: await core.harnesses.models(core.config, req.params.id) };
  });
  app.post<{ Params: { id: string } }>('/api/harnesses/:id/update', async (req): Promise<HarnessStatus> => {
    if (!isHarnessId(req.params.id)) throw new HttpError(404, 'There is no such agent.');
    const h = core.harnesses.of(req.params.id);
    await h.update(core.config);
    return core.harnesses.statusOf(h, core.config);
  });
  // Meko installs updates: the page checks, updates and sets Update automatically through Meko's own
  // routes. What AgentK keeps is which update this version came from, and What's new.
  app.get('/api/update', async () => (opts.updates ? opts.updates.status() : core.updateStatus()));
  app.get<{ Querystring: { from?: string; to?: string } }>('/api/update/notes', async (req) => {
    const { from, to } = req.query;
    if (typeof from !== 'string' || typeof to !== 'string' || !notesRange(from, to)) throw new HttpError(400, 'Give two versions, the older one first.');
    if (!opts.updates) throw new HttpError(409, "What's new isn't available here.");
    try {
      return await opts.updates.notes(from, to);
    } catch (e) {
      throw new HttpError(502, errMsg(e).replace(/\.?$/, '.'));
    }
  });

  // ---- websockets ----
  const clients = new Set<WebSocket>();
  /** Every open socket (events and terminals), and whether it answered the last ping. */
  const sockets = new Map<WebSocket, { alive: boolean }>();
  // Heartbeat: a phone that drops off the network sends no close, and its attach to the session
  // host would otherwise stay open (perhaps holding the pane at its size) until the server restarts.
  const heartbeat = setInterval(() => {
    for (const [ws, s] of sockets) {
      if (!s.alive) {
        ws.terminate();
        continue;
      }
      s.alive = false;
      ws.ping();
    }
  }, 30_000);
  heartbeat.unref();
  app.addHook('onClose', async () => clearInterval(heartbeat));
  // Closing, @fastify/websocket asks every socket to close, and the server's close waits for each
  // to answer. ws gives a peer that never does (a sleeping phone or laptop) 30 s, and every restart,
  // an update's included, waited that long: drop the sockets still open after SHUTDOWN_CLOSE_MS.
  let dropOpen: NodeJS.Timeout | null = null;
  app.addHook('preClose', (done) => {
    dropOpen = setTimeout(() => {
      for (const ws of app.websocketServer.clients) ws.terminate();
    }, SHUTDOWN_CLOSE_MS);
    dropOpen.unref();
    done();
  });
  app.addHook('onClose', async () => {
    if (dropOpen) clearTimeout(dropOpen);
  });
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
  const pushState = () => {
    pending ??= setTimeout(() => {
      pending = null;
      if (clients.size) broadcast({ type: 'state', state: core.state() });
    }, 100);
  };
  const notify = (ev: ServerEvent) => broadcast(ev);
  core.on('change', pushState);
  core.on('notify', notify);
  // A push after the server closes would read state after the database may be closed. A page's
  // socket can still be in `clients` then, closing, and the core can still change until its stop()
  // begins (shutdown, and a test's cleanup, close the server first): so the server stops listening
  // to the core here, as well as dropping the push already due.
  app.addHook('onClose', async () => {
    core.off('change', pushState);
    core.off('notify', notify);
    if (pending) clearTimeout(pending);
    pending = null;
  });

  const wsGuard = (socket: WebSocket, req: FastifyRequest): boolean => {
    if (!sameOrigin(req)) {
      socket.close(1008, 'cross-origin');
      return false;
    }
    const entry = { alive: true };
    sockets.set(socket, entry);
    socket.on('pong', () => (entry.alive = true));
    socket.on('close', () => sockets.delete(socket));
    return true;
  };

  app.get('/ws/events', { websocket: true }, (socket: WebSocket, req: FastifyRequest) => {
    if (!wsGuard(socket, req)) return;
    clients.add(socket);
    socket.send(JSON.stringify({ type: 'state', state: core.state() } satisfies ServerEvent));
    // A page opening is when stale versions are read again (nothing polls them); a change is pushed.
    void core.harnesses.refresh(core.config).catch((e) => core.log.warn('reading the agents\' versions failed', errMsg(e)));
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

  app.get<{ Params: { id: string }; Querystring: TermQuery }>('/ws/term/:id', { websocket: true },
    (socket: WebSocket, req: FastifyRequest<{ Params: { id: string }; Querystring: TermQuery }>) => {
    if (!wsGuard(socket, req)) return;
    attachTerminal(core, socket, req.params.id, req.query, { ip: req.ip, userAgent: req.headers['user-agent'] });
  });

  // ---- web app ----
  if (opts.webDir) {
    // `/` is served by the not-found handler below too, so every page carries this server's version.
    await app.register(fastifyStatic, { root: opts.webDir, wildcard: false, index: false });
    const indexHtml = stampPage(readFileSync(join(opts.webDir, 'index.html'), 'utf8'), core.version);
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
 * Readies the built page: `<base href>` names BASE_PATH, where Meko serves AgentK, so the page's
 * relative URLs (its assets, API calls and WebSockets: src/web/base.ts) resolve under it on every
 * route; and `<meta name="ak-version">` carries the server's version, so an open page that sees the
 * server report another version reloads itself (src/web/reload.ts).
 */
export function stampPage(html: string, version: string): string {
  const v = version.replace(/[&"<>]/g, (c) => `&#${c.charCodeAt(0)};`);
  return html
    .replace('<head>', `<head>\n    <base href="${BASE_PATH}" />`)
    .replace('</head>', `  <meta name="ak-version" content="${v}" />\n  </head>`);
}

export function findWebDir(serverDir: string): string | null {
  // dist/server → dist/web; src/server (tsx dev) → dist/web at the repo root.
  for (const c of [join(serverDir, '..', 'web'), join(serverDir, '..', '..', 'dist', 'web')]) {
    if (existsSync(join(c, 'index.html')) && existsSync(join(c, 'assets'))) return c;
  }
  return null;
}
