import { EventEmitter } from 'node:events';
import type { Server } from 'node:http';
import { mkdirSync, rmSync } from 'node:fs';
import { ensureWorkspaceTrust } from './trust.js';
import { userInfo } from 'node:os';
import type { AppState, MekoStatus, ServerEvent, TokenUsage, UpdateStatus } from '../shared/types.js';
import { getOwner } from './auth.js';
import { TranscriptCapture } from './capture.js';
import { CommentService } from './comments.js';
import { resolveMeko, resolveMekoWebUrl, saveConfig, type Config } from './config.js';
import { all, type Db } from './db.js';
import { startHookServer } from './hooks.js';
import { writeHookFiles } from './claude.js';
import { PlanLimitsStore } from './limits.js';
import { consoleLogger, errMsg, type Logger } from './log.js';
import { MekoClient } from './meko.js';
import type { Paths } from './paths.js';
import { ProjectService } from './projects.js';
import { SessionManager } from './sessions.js';
import { Lifecycle } from './lifecycle.js';
import { projectRows, pruneSessionEvents, toProject } from './store.js';
import { Tmux, writeTmuxConf } from './tmux.js';
import { ZERO_USAGE, addUsage } from './usage.js';

export interface CoreOptions {
  paths: Paths;
  config: Config;
  db: Db;
  version: string;
  meko: MekoClient;
  tmux?: Tmux;
  log?: Logger;
  /** Timers for reconcile, GitHub polling and Meko checks. Tests turn these off. */
  timers?: boolean;
  /** The Meko plugin's capture script (see findMekoCaptureScript); null turns transcript capture off. */
  mekoCaptureScript?: string | null;
}

type NotifyEvent = Extract<ServerEvent, { type: 'notify' }>;

export class Core extends EventEmitter {
  readonly paths: Paths;
  readonly db: Db;
  readonly version: string;
  readonly username: string;
  readonly meko: MekoClient;
  readonly capture: TranscriptCapture;
  readonly tmux: Tmux;
  readonly log: Logger;
  readonly projects: ProjectService;
  readonly sessions: SessionManager;
  readonly lifecycle: Lifecycle;
  readonly comments: CommentService;
  readonly limits: PlanLimitsStore;
  mekoStatus: MekoStatus;
  updateStatus: () => UpdateStatus;
  private cfg: Config;
  private timersOn: boolean;
  private intervals: NodeJS.Timeout[] = [];
  private hookServer: Server | null = null;

  constructor(o: CoreOptions) {
    super();
    this.paths = o.paths;
    this.db = o.db;
    this.version = o.version;
    this.cfg = o.config;
    this.meko = o.meko;
    this.mekoStatus = { ok: true, error: null, checkedAt: null, webUrl: resolveMekoWebUrl(o.config) };
    this.log = o.log ?? consoleLogger;
    this.capture = new TranscriptCapture(o.mekoCaptureScript ?? null, () => {
      const m = resolveMeko(this.cfg);
      return { MEKO_MCP_URL: m.url, MEKO_API_KEY: m.apiKey };
    }, this.log);
    this.tmux = o.tmux ?? new Tmux(o.config.tmux.socket, o.paths.tmuxConf);
    this.timersOn = o.timers ?? true;
    this.username = safeUsername();
    this.updateStatus = () => ({
      current: this.version, channel: this.cfg.update.channel, latest: null, available: false, applying: false,
      lastCheckedAt: null, lastError: null, justUpdatedFrom: null, releaseUrl: null,
    });
    this.projects = new ProjectService(this);
    this.sessions = new SessionManager(this);
    this.lifecycle = new Lifecycle(this);
    this.comments = new CommentService(this);
    this.limits = new PlanLimitsStore(this.db, () => this.changed());
  }

  get config(): Config {
    return this.cfg;
  }

  updateConfig(fn: (c: Config) => void): Config {
    const next = structuredClone(this.cfg);
    fn(next);
    saveConfig(this.paths, next);
    this.cfg = next;
    this.mekoStatus = { ...this.mekoStatus, webUrl: resolveMekoWebUrl(next) };
    this.changed();
    return next;
  }

  /** Something visible changed; the server debounces and broadcasts state. */
  changed(): void {
    this.emit('change');
  }

  notify(n: Omit<NotifyEvent, 'type'>): void {
    this.emit('notify', { type: 'notify', ...n } satisfies NotifyEvent);
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
      limits: this.limits.current(),
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
    this.hookServer = await startHookServer(
      this.paths.hookSock,
      (token, payload) => {
        this.sessions.onHook(token, payload).catch((e) => this.log.error('hook handling failed', e));
      },
      (token, payload) => this.sessions.guard(token, payload),
      (token, input) => this.sessions.onStatusLine(token, input),
    );
    // A tmux service that is down must not keep the web server from starting: the UI stays
    // reachable, and recovery (which needs tmux) retries on every reconcile tick.
    await this.tmux.ensureServer().catch((e) => this.log.error('tmux is not available', e));
    await this.checkMeko();
    await this.sessions.recover();
    void this.sessions.refreshDiffStats().catch(() => undefined);
    pruneSessionEvents(this.db);
    if (this.timersOn) {
      this.every(5000, () => this.sessions.reconcile());
      this.every(60000, () => this.checkMeko());
      this.every(60000, () => this.sessions.refreshDiffStats());
      this.lifecycle.start();
    }
  }

  async stop(): Promise<void> {
    for (const i of this.intervals) clearInterval(i);
    this.intervals = [];
    this.lifecycle.stop();
    await new Promise<void>((r) => (this.hookServer ? this.hookServer.close(() => r()) : r()));
    rmSync(this.paths.hookSock, { force: true });
    await this.meko.close();
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

  async checkMeko(): Promise<void> {
    const before = this.mekoStatus.ok;
    try {
      await this.meko.check();
      this.mekoStatus = { ...this.mekoStatus, ok: true, error: null, checkedAt: Date.now() };
    } catch (e) {
      this.mekoStatus = { ...this.mekoStatus, ok: false, error: errMsg(e), checkedAt: Date.now() };
      if (before) this.log.warn('Meko unreachable', this.mekoStatus.error);
    }
    if (before !== this.mekoStatus.ok) this.changed();
  }
}

function safeUsername(): string {
  try {
    return userInfo().username;
  } catch {
    return process.env.USER ?? 'user';
  }
}
