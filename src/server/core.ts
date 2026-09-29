import { EventEmitter } from 'node:events';
import type { Server } from 'node:http';
import { mkdirSync, rmSync } from 'node:fs';
import { ensureWorkspaceTrust } from './trust.js';
import { hostname, userInfo } from 'node:os';
import type { AppState, MekoStatus, ServerEvent, TokenUsage, UpdateStatus } from '../shared/types.js';
import { getOwner } from './auth.js';
import { TranscriptCapture } from './capture.js';
import { CommentService } from './comments.js';
import { mekoConfigured, mekoWebUrlWarning, resolveMeko, resolveMekoApiUrl, resolveMekoWebUrl, saveConfig, type Config } from './config.js';
import { all, type Db } from './db.js';
import { socketId, startHookServer } from './hooks.js';
import { writeHookFiles } from './claude.js';
import { PlanLimitsStore } from './limits.js';
import { consoleLogger, errMsg, type Logger } from './log.js';
import { InviteService } from './invites.js';
import { MekoClient } from './meko.js';
import { MekoApi } from './mekoapi.js';
import { defaultMekoTools, type MekoTools } from './mekosettings.js';
import type { Paths } from './paths.js';
import { ProjectService } from './projects.js';
import { SessionManager } from './sessions.js';
import { Lifecycle } from './lifecycle.js';
import { projectRows, pruneSessionEvents, toProject } from './store.js';
import { Tmux, writeTmuxConf } from './tmux.js';
import { ZERO_USAGE, addUsage } from './usage.js';
import { WorktreePool } from './worktrees.js';

export interface CoreOptions {
  paths: Paths;
  config: Config;
  db: Db;
  version: string;
  meko: MekoClient;
  tmux?: Tmux;
  log?: Logger;
  /** Timers for reconcile, GitHub polling and the share syncs. Tests turn these off. */
  timers?: boolean;
  /** The Meko plugin's capture script (see findMekoCaptureScript); null turns transcript capture off. */
  mekoCaptureScript?: string | null;
  /** Meko's REST API client (sharing); by default one on the configured Meko. */
  mekoApi?: MekoApi;
  /** Checking Meko credentials and setting up Claude Code, for the Meko tab in Settings. */
  mekoTools?: MekoTools;
}

type NotifyEvent = Extract<ServerEvent, { type: 'notify' }>;
type InviteEvent = Extract<ServerEvent, { type: 'invite' }>;

/** How often invites and shares are synced from Meko, and how often the share API is re-checked. */
const INVITE_SYNC_MS = 5 * 60 * 1000;
const SHARING_CHECK_MS = 60 * 60 * 1000;
/** How often idle worktrees unused for too long are removed. */
const IDLE_WORKTREE_CHECK_MS = 60 * 60 * 1000;

/** How often the server checks that its hook socket file is still there. */
const HOOK_SOCKET_CHECK_MS = 10_000;

export class Core extends EventEmitter {
  readonly paths: Paths;
  readonly db: Db;
  readonly version: string;
  readonly username: string;
  /** The machine's host name as the OS reports it, read once at startup. */
  readonly hostFull: string;
  readonly meko: MekoClient;
  readonly capture: TranscriptCapture;
  readonly tmux: Tmux;
  readonly log: Logger;
  readonly projects: ProjectService;
  readonly worktrees: WorktreePool;
  readonly sessions: SessionManager;
  readonly lifecycle: Lifecycle;
  readonly comments: CommentService;
  readonly limits: PlanLimitsStore;
  readonly invites: InviteService;
  readonly mekoTools: MekoTools;
  mekoStatus: MekoStatus;
  updateStatus: () => UpdateStatus;
  private cfg: Config;
  private timersOn: boolean;
  private intervals: NodeJS.Timeout[] = [];
  private hookServer: Server | null = null;
  /** The socket file the hook server bound (socketId), to tell it apart from one put there since. */
  private hookSockId: string | null = null;
  /** Between start() and stop(): the hook socket should be bound (and is bound again when it goes). */
  private hooksOn = false;
  /** A rebind of the hook socket in progress; stop() waits for it. */
  private hookCheck: Promise<void> | null = null;

  constructor(o: CoreOptions) {
    super();
    this.paths = o.paths;
    this.db = o.db;
    this.version = o.version;
    this.cfg = o.config;
    this.meko = o.meko;
    const configured = mekoConfigured(o.config);
    this.mekoStatus = { configured, webUrl: resolveMekoWebUrl(o.config), sharing: null };
    this.log = o.log ?? consoleLogger;
    this.capture = new TranscriptCapture(o.mekoCaptureScript ?? null, () => {
      const m = resolveMeko(this.cfg);
      return { MEKO_MCP_URL: m.url, MEKO_API_KEY: m.apiKey };
    }, this.log);
    this.tmux = o.tmux ?? new Tmux(o.config.tmux.socket, o.paths.tmuxConf);
    this.timersOn = o.timers ?? true;
    this.username = safeUsername();
    this.hostFull = hostname();
    this.updateStatus = () => ({
      current: this.version, channel: this.cfg.update.channel, latest: null, available: false, applying: false,
      lastCheckedAt: null, lastError: null, justUpdatedFrom: null, releaseUrl: null,
    });
    this.projects = new ProjectService(this);
    this.worktrees = new WorktreePool(this);
    this.sessions = new SessionManager(this);
    this.lifecycle = new Lifecycle(this);
    this.comments = new CommentService(this);
    this.limits = new PlanLimitsStore(this.db, () => this.changed());
    this.mekoTools = o.mekoTools ?? defaultMekoTools;
    this.invites = new InviteService(this, o.mekoApi ?? new MekoApi(() => ({ baseUrl: resolveMekoApiUrl(this.cfg), apiKey: resolveMeko(this.cfg).apiKey })));
  }

  get config(): Config {
    return this.cfg;
  }

  updateConfig(fn: (c: Config) => void): Config {
    const next = structuredClone(this.cfg);
    fn(next);
    saveConfig(this.paths, next);
    this.cfg = next;
    const configured = mekoConfigured(next);
    this.mekoStatus = { ...this.mekoStatus, webUrl: resolveMekoWebUrl(next), configured, ...(configured ? {} : { sharing: null }) };
    this.changed();
    return next;
  }

  /** The machine's short host name: what the rail shows when `[server] name` is unset. */
  hostName(): string {
    return shortHost(this.hostFull);
  }

  /** The name shown next to the user name in the rail. */
  serverName(): string {
    return this.cfg.server.name.trim() || this.hostName();
  }

  /** Something visible changed; the server debounces and broadcasts state. */
  changed(): void {
    this.emit('change');
  }

  notify(n: Omit<NotifyEvent, 'type'>): void {
    this.emit('notify', { type: 'notify', ...n } satisfies NotifyEvent);
  }

  notifyInvite(n: Omit<InviteEvent, 'type'>): void {
    this.emit('notify', { type: 'invite', ...n } satisfies InviteEvent);
  }

  /** Re-checks the share API, then syncs invites. Never throws. */
  async refreshSharing(): Promise<void> {
    if (!this.mekoStatus.configured) return;
    await this.invites.checkMode();
    await this.invites.sync().catch((e) => this.log.warn('datapack invite sync failed', errMsg(e)));
  }

  /**
   * The Meko URL or key changed (or Meko was turned on or off): reconnect, forget the old
   * account's shares, and check sharing again in the background.
   */
  async mekoChanged(): Promise<void> {
    await this.meko.reset();
    this.invites.forgetAccount();
    this.sharingChanged();
  }

  /** Meko's API URL changed (or the account did): check the sharing mode again, in the background. */
  sharingChanged(): void {
    this.mekoStatus = { ...this.mekoStatus, sharing: null };
    this.changed();
    void this.refreshSharing();
  }

  state(): AppState {
    return {
      projects: (() => {
        const projects = projectRows(this.db).map((r) => ({ ...toProject(this.db, r), sessions: this.sessions.listForProject(r.id) }));
        // Summed from the session manager's per-session totals (loaded by listForProject), archived
        // sessions included, instead of scanning every usage row on each state push.
        const usage = new Map<string, TokenUsage>();
        for (const s of all<{ id: string; project_id: string }>(this.db, 'SELECT id, project_id FROM sessions')) {
          usage.set(s.project_id, addUsage(usage.get(s.project_id) ?? ZERO_USAGE, this.sessions.runtime(s.id).usage));
        }
        return projects.map((p) => ({ ...p, usage: usage.get(p.id) ?? { ...ZERO_USAGE } }));
      })(),
      meko: this.mekoStatus,
      update: this.updateStatus(),
      version: this.version,
      // The login name shown in the UI; the Unix account only as a fallback.
      username: getOwner(this.db)?.username ?? this.username,
      serverName: this.serverName(),
      serverHost: this.hostFull,
      limits: this.limits.current(),
      invites: this.invites.pending(),
    };
  }

  async start(): Promise<void> {
    writeTmuxConf(this.paths);
    writeHookFiles(this.paths);
    mkdirSync(this.paths.worktrees, { recursive: true });
    try {
      if (ensureWorkspaceTrust(this.paths.worktrees) === 'added') this.log.info(`trusted ${this.paths.worktrees} in Claude Code so new worktrees skip the folder-trust prompt`);
    } catch (e) {
      this.log.warn('could not update Claude Code folder trust', errMsg(e));
    }
    // In the background: it may remove large build trees, and every change it makes to an idle
    // worktree is skipped once a session has claimed it.
    void this.worktrees.sweep().catch((e) => this.log.warn('checking idle worktrees failed', errMsg(e)));
    const noWebUrl = mekoWebUrlWarning(this.cfg);
    if (noWebUrl) this.log.warn(noWebUrl);
    await this.bindHooks();
    this.hooksOn = true;
    // A tmux service that is down must not keep the web server from starting: the UI stays
    // reachable, and recovery (which needs tmux) retries on every reconcile tick.
    await this.tmux.ensureServer().catch((e) => this.log.error('tmux is not available', e));
    await this.sessions.recover();
    void this.sessions.refreshDiffStats().catch(() => undefined);
    pruneSessionEvents(this.db);
    if (this.timersOn) {
      this.every(5000, () => this.sessions.reconcile());
      this.every(HOOK_SOCKET_CHECK_MS, () => this.checkHookSocket());
      this.every(60000, () => this.sessions.refreshDiffStats());
      this.every(IDLE_WORKTREE_CHECK_MS, () => this.worktrees.expire());
      this.every(INVITE_SYNC_MS, () => this.invites.sync());
      this.every(SHARING_CHECK_MS, () => this.invites.checkMode());
      // Not awaited: startup doesn't wait on Meko's API.
      void this.refreshSharing();
      this.lifecycle.start();
    }
  }

  async stop(): Promise<void> {
    for (const i of this.intervals) clearInterval(i);
    this.intervals = [];
    this.lifecycle.stop();
    this.hooksOn = false;
    await this.hookCheck?.catch(() => undefined);
    await this.closeHooks();
    await this.meko.close();
  }

  private async bindHooks(): Promise<void> {
    this.hookServer = await startHookServer(
      this.paths.hookSock,
      (token, payload) => {
        this.sessions.onHook(token, payload).catch((e) => this.log.error('hook handling failed', e));
      },
      (token, payload) => this.sessions.guard(token, payload),
      (token, input) => this.sessions.onStatusLine(token, input),
    );
    this.hookSockId = socketId(this.paths.hookSock);
  }

  /** Stops the hook server. Its socket file is removed only if it is still ours. */
  private async closeHooks(): Promise<void> {
    const server = this.hookServer;
    this.hookServer = null;
    if (!server) return;
    await new Promise<void>((r) => server.close(() => r()));
    if (this.hookSockId && socketId(this.paths.hookSock) === this.hookSockId) rmSync(this.paths.hookSock, { force: true });
    this.hookSockId = null;
  }

  /**
   * Binds the hook socket again when its file was removed or replaced (by another server on
   * this home, or a temp cleaner). Without it, hooks find no socket and are dropped quietly,
   * so sessions never leave "Starting" and their status stops changing.
   */
  checkHookSocket(): Promise<void> {
    if (!this.hooksOn) return Promise.resolve();
    if (this.hookCheck) return this.hookCheck;
    // A rebind that failed left no server: try again.
    if (this.hookServer && socketId(this.paths.hookSock) === this.hookSockId) return Promise.resolve();
    this.log.warn(`the hook socket ${this.paths.hookSock} was removed or replaced; listening on it again`);
    this.hookCheck = (async () => {
      await this.closeHooks();
      if (this.hooksOn) await this.bindHooks();
    })().finally(() => {
      this.hookCheck = null;
    });
    return this.hookCheck;
  }

  private every(ms: number, fn: () => Promise<unknown> | unknown): void {
    let running = false;
    this.intervals.push(
      setInterval(async () => {
        if (running) return;
        running = true;
        try {
          await fn();
        } catch (e) {
          this.log.warn('periodic task failed', errMsg(e));
        } finally {
          running = false;
        }
      }, ms),
    );
  }

}

/** A host name up to its first dot (`dev-box.example.com` → `dev-box`), or all of it when that leaves nothing. */
export function shortHost(name: string): string {
  return name.split('.')[0] || name;
}

function safeUsername(): string {
  try {
    return userInfo().username;
  } catch {
    return process.env.USER ?? 'user';
  }
}
