import { EventEmitter } from 'node:events';
import type { Server } from 'node:http';
import { mkdirSync, rmSync } from 'node:fs';
import { claudeConfigPath } from './trust.js';
import { hostname, userInfo } from 'node:os';
import { join } from 'node:path';
import type { AppState, MekoStatus, ServerEvent, TokenUsage, UpdateStatus } from '../shared/types.js';
import { getOwner } from './auth.js';
import * as github from './github.js';
import { TranscriptCapture, mekoPluginKeys } from './capture.js';
import { DesignQuestionStore } from './answers.js';
import { CommentService } from './comments.js';
import { AskAgentk } from './askagentk.js';
import { GithubLinkService } from './ghlinks.js';
import { PrReviewService } from './prreview.js';
import { mekoConfigured, mekoWebUrlWarning, resolveMeko, resolveMekoApiUrl, resolveMekoWebUrl, saveConfig, type Config } from './config.js';
import { all, run, type Db } from './db.js';
import { socketId, startHookServer } from './hooks.js';
import { claudeUserDir, writeHookFiles } from './claude.js';
import { Harnesses } from './harness/index.js';
import { kiroAgentLoadsMcp, kiroMekoGaps, readKiroMeko } from './mekokiro.js';
import { kiroDeletesSettled } from './kiro.js';
import { KIRO_PLAN_EVERY_MS, KiroPlanStore } from './kiroplan.js';
import { PlanLimitsStore } from './limits.js';
import { sweepStaged } from './pastes.js';
import { consoleLogger, errMsg, type Logger } from './log.js';
import { InviteService } from './invites.js';
import { KnowledgeService } from './knowledge.js';
import { MekoClient } from './meko.js';
import { MekoCallLog } from './mekolog.js';
import { MekoClaudeWatch } from './mekoclaude.js';
import { readClaudeMeko } from './mekosetup.js';
import { MekoApi } from './mekoapi.js';
import { defaultMekoTools, type MekoTools } from './mekosettings.js';
import type { Paths } from './paths.js';
import { ProjectService } from './projects.js';
import { SessionManager } from './sessions.js';
import { Learner } from './learn.js';
import { Lifecycle } from './lifecycle.js';
import { openProjectDatapacks, parseSettings, projectRow, projectRows, pruneSessionEvents, sessionRow, sessionRows, toProject } from './store.js';
import { CallHomeService } from './callhome.js';
import { ClaudeCode } from './claudecode.js';
import { SessionHost } from './hostclient.js';
import { legacyTmuxPanes } from './legacytmux.js';
import { servicesFor, type ServiceManager } from './service.js';
import { ZERO_USAGE, addUsage } from './usage.js';
import { WorktreePool } from './worktrees.js';

export interface CoreOptions {
  paths: Paths;
  config: Config;
  db: Db;
  version: string;
  meko: MekoClient;
  host?: SessionHost;
  /** The background services (server, session host); tests pass none. */
  services?: () => ServiceManager | null;
  /** The tmux sessions on older versions' socket (legacytmux.ts); tests pass their own. */
  legacyTmuxPanes?: () => Promise<Set<string>>;
  log?: Logger;
  /** Timers for reconcile, GitHub polling and the share syncs. Tests turn these off. */
  timers?: boolean;
  /** The Meko plugin's capture script (see findMekoCaptureScript); null turns transcript capture off. */
  mekoCaptureScript?: string | null;
  /** Meko's REST API client (sharing, knowledge-base uploads); by default one on the configured Meko. */
  mekoApi?: MekoApi;
  /** Checking Meko credentials and setting up Claude Code, for the Meko tab in Settings. */
  mekoTools?: MekoTools;
  /** How usage stats are sent (tests fake it); by default the global fetch. */
  callHomeFetch?: typeof fetch;
  /** Reads Kiro CLI's version (tests fake it); by default `<command> --version`. */
  kiroVersion?: (command: string) => Promise<string | null>;
}

type NotifyEvent = Extract<ServerEvent, { type: 'notify' }>;
type InviteEvent = Extract<ServerEvent, { type: 'invite' }>;
type KnowledgeEvent = Extract<ServerEvent, { type: 'knowledge' }>;

/** How often invites and shares are synced from Meko. */
const INVITE_SYNC_MS = 5 * 60 * 1000;
/** How often idle worktrees unused for too long are removed. */
const IDLE_WORKTREE_CHECK_MS = 60 * 60 * 1000;
/** How often archived sessions' Meko pins are cleared away. */
const PIN_PRUNE_MS = 6 * 60 * 60 * 1000;
/** How often images staged in the New session dialog that no session started with are looked for. */
const STAGED_SWEEP_MS = 60 * 60 * 1000;
/** How long a read of Kiro's Meko setup is reused (the state is pushed often). */
const KIRO_MEKO_TTL_MS = 10_000;

/** How often the server checks that its hook socket file is still there. */
const HOOK_SOCKET_CHECK_MS = 10_000;
/** How long stopping waits for transcript captures and Kiro's one-shot runs still running (systemd and launchd allow 15 s to stop). */
const STOP_CAPTURE_MS = 5000;
/** How long asking gh for its login may take (a session's start waits for it). */
const GH_LOGIN_TIMEOUT_MS = 5000;
/** After gh took too long to give its login, how long it isn't asked again (starts go on without it). */
const GH_LOGIN_RETRY_MS = 60_000;

/** Waits for `p`, but at most `ms`. */
const within = (p: Promise<void>, ms: number) => Promise.race([p, new Promise<void>((r) => setTimeout(r, ms).unref())]);

export class Core extends EventEmitter {
  readonly paths: Paths;
  readonly db: Db;
  readonly version: string;
  readonly username: string;
  /** The machine's host name as the OS reports it, read once at startup. */
  readonly hostFull: string;
  readonly meko: MekoClient;
  readonly capture: TranscriptCapture;
  /** Each session's Meko calls, with their content, when its project has Log Meko calls on. */
  readonly mekoLog: MekoCallLog;
  readonly host: SessionHost;
  readonly services: () => ServiceManager | null;
  readonly legacyTmuxPanes: () => Promise<Set<string>>;
  readonly log: Logger;
  readonly projects: ProjectService;
  readonly worktrees: WorktreePool;
  readonly sessions: SessionManager;
  readonly lifecycle: Lifecycle;
  /** The learn step: what sessions learned, saved to their project's datapack. */
  readonly learn: Learner;
  readonly comments: CommentService;
  readonly questions: DesignQuestionStore;
  readonly prReview: PrReviewService;
  readonly githubLinks: GithubLinkService;
  readonly askAgentk: AskAgentk;
  readonly limits: PlanLimitsStore;
  /** Kiro's plan allowance for the rail. */
  readonly kiroPlan: KiroPlanStore;
  readonly invites: InviteService;
  readonly knowledge: KnowledgeService;
  readonly mekoTools: MekoTools;
  /** Whether Claude Code's sessions reach AgentK's Meko (MekoStatus.claude). */
  readonly claudeMeko: MekoClaudeWatch;
  readonly callHome: CallHomeService;
  /** The same report to YugabyteDB's call home service, from release installs (to be the only one later). */
  readonly ybCallHome: CallHomeService;
  /** The Claude Code sessions launch with: its version (for AgentK's mod) and updating it. */
  readonly claudeCode: ClaudeCode;
  /** Claude Code was available at its last version change (`claudeChanged`). */
  private claudeAvailable = false;
  /** Every coding agent sessions can run on (src/server/harness/). */
  readonly harnesses: Harnesses;
  /** The last read of Kiro's Meko setup (kiroMeko). */
  private kiroMekoAt: { at: number; value: MekoStatus['kiro'] } | null = null;
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
  /** stop() has run: changes are no longer broadcast. */
  private stopped = false;
  /** A rebind of the hook socket in progress; stop() waits for it. */
  private hookCheck: Promise<void> | null = null;
  /** The login gh is signed in as, once it has said (ghLogin). */
  private ghUser: string | null = null;
  /** The login lookup in flight, which every caller meanwhile waits on. */
  private ghLookup: Promise<string | null> | null = null;
  /** When gh last took too long to give its login (GH_LOGIN_RETRY_MS). */
  private ghTimedOutAt = 0;

  constructor(o: CoreOptions) {
    super();
    this.paths = o.paths;
    this.db = o.db;
    this.version = o.version;
    this.cfg = o.config;
    this.meko = o.meko;
    this.log = o.log ?? consoleLogger;
    this.capture = new TranscriptCapture(o.mekoCaptureScript ?? null, () => {
      const m = resolveMeko(this.cfg);
      return { MEKO_MCP_URL: m.url, MEKO_API_KEY: m.apiKey };
    }, this.log, undefined, (ok, ms) => this.record('INSERT INTO meko_capture_runs(at, ok, ms) VALUES (?, ?, ?)', Date.now(), ok ? 1 : 0, ms));
    // AgentK's own Meko calls, for the usage report (counts and sizes, never their content).
    this.meko.onCall = (r) => this.record('INSERT INTO meko_agentk_calls(at, tool, input_chars, result_chars, ok, ms, learn_id) VALUES (?, ?, ?, ?, ?, ?, ?)',
      r.at, r.tool, r.inputChars, r.resultChars, r.ok ? 1 : 0, r.ms, r.tag);
    this.mekoLog = new MekoCallLog({
      designs: this.paths.designs,
      enabled: (s) => {
        if (this.stopped) return false;
        const r = sessionRow(this.db, s.id);
        // Not a failed start's late calls landing in the log of the start that reused its id.
        const p = r && r.created_at === s.createdAt && r.desired !== 'archived' ? projectRow(this.db, r.project_id) : undefined;
        return !!p && parseSettings(p.settings_json).mekoLog;
      },
      log: this.log,
    });
    this.meko.onTrace = (t) => this.mekoLog.record(t);
    this.services = o.services ?? (() => servicesFor(o.paths.home));
    this.legacyTmuxPanes = o.legacyTmuxPanes ?? (() => legacyTmuxPanes(o.paths));
    this.host = o.host ?? new SessionHost(o.paths, { services: this.services, onHello: () => this.changed() });
    this.timersOn = o.timers ?? true;
    this.username = safeUsername();
    this.hostFull = hostname();
    this.updateStatus = () => ({
      current: this.version, channel: this.cfg.update.channel, latest: null, available: false, applying: false,
      lastCheckedAt: null, lastError: null, justUpdatedFrom: null, lastUpdatedFrom: null, releaseUrl: null,
    });
    this.claudeCode = new ClaudeCode({ config: () => this.cfg, changed: () => this.claudeChanged() });
    this.harnesses = new Harnesses({ claudeCode: this.claudeCode, log: this.log, changed: () => this.changed(), kiroVersion: o.kiroVersion });
    this.projects = new ProjectService(this);
    this.worktrees = new WorktreePool(this);
    this.sessions = new SessionManager(this);
    this.lifecycle = new Lifecycle(this);
    this.learn = new Learner(this);
    this.comments = new CommentService(this);
    this.questions = new DesignQuestionStore(this);
    this.prReview = new PrReviewService(this);
    this.githubLinks = new GithubLinkService(this);
    this.askAgentk = new AskAgentk({
      config: () => this.cfg,
      cwd: join(this.paths.run, 'ask'),
      projects: () => projectRows(this.db).map((r) => ({ id: r.id, name: r.name, repoPath: r.repo_path, sessions: this.sessions.listForProject(r.id) })),
      record: (r) => this.record('INSERT INTO agentk_asks(at, ok, actions, ms) VALUES (?, ?, ?, ?)', r.at, r.ok ? 1 : 0, r.actions, r.ms),
      asker: async () => (await this.harnesses.ensureDefault(this.cfg))?.ask ?? null,
    });
    this.limits = new PlanLimitsStore(this.db, () => this.changed());
    this.kiroPlan = new KiroPlanStore({
      db: this.db,
      available: () => this.harnesses.of('kiro').status(this.cfg).available,
      command: () => this.cfg.kiro.command,
      cwd: join(this.paths.run, 'kiro-usage'),
      changed: () => this.changed(),
      log: this.log,
    });
    this.mekoTools = o.mekoTools ?? defaultMekoTools;
    this.claudeMeko = new MekoClaudeWatch({
      config: () => this.cfg,
      available: () => this.harnesses.of('claude').status(this.cfg).available,
      read: () => readClaudeMeko(claudeUserDir(), claudeConfigPath()),
      mcpRegistered: () => this.mekoTools.claudeHasMcp(this.cfg.claude.command),
      projectDatapacks: () => openProjectDatapacks(this.db),
      ownDatapacks: async () => (await this.meko.listDatapacks()).map((d) => d.id),
      tryToken: (url, token) => this.mekoTools.tryToken(url, token),
      changed: () => {
        this.mekoStatus = { ...this.mekoStatus, claude: this.claudeMeko.status };
        this.changed();
      },
      log: this.log,
    });
    this.mekoStatus = this.mekoStatusFor(o.config);
    this.callHome = new CallHomeService({
      db: this.db, config: () => this.cfg, version: this.version, log: this.log, fetch: o.callHomeFetch,
    });
    this.ybCallHome = new CallHomeService({
      target: 'yugabyte', db: this.db, config: () => this.cfg, version: this.version, log: this.log, fetch: o.callHomeFetch,
    });
    const mekoApi = o.mekoApi ?? new MekoApi(() => ({ baseUrl: resolveMekoApiUrl(this.cfg), apiKey: resolveMeko(this.cfg).apiKey }));
    this.invites = new InviteService(this, mekoApi);
    this.knowledge = new KnowledgeService(this, mekoApi);
  }

  /** A usage row (Meko, Ask AgentK) that may come after stop() closed the database: dropped then. */
  private record(sql: string, ...args: (string | number | null)[]): void {
    if (this.stopped) return;
    try {
      run(this.db, sql, ...args);
    } catch (e) {
      this.log.warn('recording usage failed', errMsg(e));
    }
  }

  get config(): Config {
    return this.cfg;
  }

  updateConfig(fn: (c: Config) => void): Config {
    const next = structuredClone(this.cfg);
    fn(next);
    saveConfig(this.paths, next);
    this.cfg = next;
    this.mekoStatus = this.mekoStatusFor(next);
    this.changed();
    // A Meko change can make Claude Code's Meko differ (or match again).
    void this.claudeMeko.refresh();
    return next;
  }

  private mekoStatusFor(cfg: Config): MekoStatus {
    return { configured: mekoConfigured(cfg), webUrl: resolveMekoWebUrl(cfg), claude: this.claudeMeko.status, kiro: null };
  }

  /**
   * Whether Kiro's sessions reach AgentK's Meko (`MekoStatus.kiro`): null while Meko is off or Kiro
   * isn't available. Its files are read again at most every KIRO_MEKO_TTL_MS, or when `force`d.
   */
  kiroMeko(force = false): MekoStatus['kiro'] {
    if (!mekoConfigured(this.cfg) || !this.harnesses.of('kiro').status(this.cfg).available) return null;
    const now = Date.now();
    if (force || !this.kiroMekoAt || now - this.kiroMekoAt.at > KIRO_MEKO_TTL_MS) {
      this.kiroMekoAt = { at: now, value: { gaps: kiroMekoGaps(readKiroMeko(), resolveMeko(this.cfg).url, kiroAgentLoadsMcp()) } };
    }
    return this.kiroMekoAt.value;
  }

  /** The machine's short host name: what the rail shows when `[server] name` is unset. */
  hostName(): string {
    return shortHost(this.hostFull);
  }

  /** The name shown next to the user name in the rail. */
  serverName(): string {
    return this.cfg.server.name.trim() || this.hostName();
  }

  /**
   * Something visible changed; the server debounces and broadcasts state. Nothing after stop():
   * work still finishing then (a Claude Code version read) must not read state from a closed database.
   */
  changed(): void {
    if (!this.stopped) this.emit('change');
  }

  /** Claude Code's version changed: its Meko is checked again once it's there (or cleared once it's gone). */
  private claudeChanged(): void {
    // A read that finishes after stop(): nothing to check or push any more.
    if (this.stopped) return;
    const available = this.harnesses.of('claude').status(this.cfg).available;
    if (available !== this.claudeAvailable) {
      this.claudeAvailable = available;
      void this.claudeMeko.refresh();
    }
    this.changed();
  }

  notify(n: Omit<NotifyEvent, 'type'>): void {
    this.emit('notify', { type: 'notify', ...n } satisfies NotifyEvent);
  }

  notifyInvite(n: Omit<InviteEvent, 'type'>): void {
    this.emit('notify', { type: 'invite', ...n } satisfies InviteEvent);
  }

  notifyKnowledge(n: Omit<KnowledgeEvent, 'type'>): void {
    this.emit('notify', { type: 'knowledge', ...n } satisfies KnowledgeEvent);
  }

  /** Syncs the invites and shares waiting in Meko. Never throws. */
  async syncInvites(): Promise<void> {
    if (!this.mekoStatus.configured) return;
    await this.invites.sync().catch((e) => this.log.warn('datapack invite sync failed', errMsg(e)));
  }

  /**
   * The Meko URL or key changed (or Meko was turned on or off): reconnect, forget the old
   * account's shares, and sync the new one's in the background.
   */
  async mekoChanged(): Promise<void> {
    await this.meko.reset();
    this.invites.forgetAccount();
    this.knowledge.forget();
    void this.syncInvites();
  }

  /**
   * The login gh is signed in as, looked up once (from <cwd>), one lookup at a time. Null when gh
   * can't say: asked again next time, or after GH_LOGIN_RETRY_MS when it took too long, so a gh
   * that hangs (offline, GitHub slow) doesn't hold up every session start.
   */
  async ghLogin(cwd: string): Promise<string | null> {
    if (this.ghUser) return this.ghUser;
    if (Date.now() - this.ghTimedOutAt < GH_LOGIN_RETRY_MS) return null;
    this.ghLookup ??= this.lookUpGhLogin(cwd).finally(() => {
      this.ghLookup = null;
    });
    return this.ghLookup;
  }

  private async lookUpGhLogin(cwd: string): Promise<string | null> {
    try {
      const login = (await github.viewerLogin(cwd, GH_LOGIN_TIMEOUT_MS)) || null;
      if (login) {
        this.ghUser = login;
        this.changed();
      }
    } catch (e) {
      // execFile's timeout kills gh.
      if ((e as { killed?: boolean }).killed) this.ghTimedOutAt = Date.now();
      this.log.warn('looking up the GitHub login failed', errMsg(e));
    }
    return this.ghUser;
  }

  state(): AppState {
    const harnesses = this.harnesses.status(this.cfg);
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
      meko: { ...this.mekoStatus, kiro: this.kiroMeko() },
      update: this.updateStatus(),
      version: this.version,
      // The login name shown in the UI; the Unix account only as a fallback.
      username: getOwner(this.db)?.username ?? this.username,
      ghLogin: this.ghUser,
      serverName: this.serverName(),
      serverHost: this.hostFull,
      limits: this.limits.current(),
      kiroPlan: this.kiroPlan.current(),
      invites: this.invites.pending(),
      failedStarts: this.sessions.failedStarts(),
      claudeCode: this.claudeCode.status(),
      harnesses,
      // Still offered while an agent that's turned on hasn't answered yet (just after a start).
      askAgentk: this.cfg.askAgentk.enabled && harnesses.some((h) => h.available || !h.checked),
      sessionHost: { version: this.host.hello?.version ?? null, outdated: !!this.host.hello?.outdated },
    };
  }

  async start(): Promise<void> {
    this.stopped = false;
    // A version before the session host ran sessions in tmux, and they may still run there: a host
    // would start each one a second time. `install` ends them (it removes the tmux service), and
    // this version's recovery then resumes them in the host. Refusing here also lets an update
    // from such a version roll back by itself. The host service alone doesn't prove the sessions
    // left tmux (a tmux version reinstalled after the move keeps its file), so with it installed
    // this refuses only while a session of this home that isn't archived is still in tmux.
    // Otherwise the tmux service is a leftover (the move kept it while a pane it didn't know, such
    // as a deleted session's, was still in tmux), and refusing would roll back every update.
    const svc = this.services();
    if (svc?.legacyTmuxInstalled()) {
      const inTmux = svc.installed('host') ? await this.legacyTmuxPanes() : null;
      if (!inTmux || sessionRows(this.db).some((r) => r.status !== 'archived' && inTmux.has(r.tmux_name))) {
        throw new Error("An AgentK version from before the session host is still installed here, and its sessions may still run in tmux. Run: agentk install (it ends them there, and they resume in the session host)");
      }
      this.log.warn("the tmux service of a version before the session host is still installed, though sessions run in the session host now. It's left running, since removing it ends any Claude still in tmux. To remove it: agentk install");
    }
    // AgentK's hook files: a server that can't write them doesn't start, since every session's hooks
    // (Claude Code's --settings file, the scripts Kiro's agent runs) and the Design guard depend on
    // them. With transcript capture off, Claude Code sessions run without the Meko plugin: its own
    // hooks would still send transcripts when a session compacts or ends. Its MCP server is
    // configured apart from it, so Meko's tools stay, and the system prompt gives the conversation_id
    // its SessionStart did. The setting, not whether the plugin's script was found: a plugin
    // installed after AgentK starts runs in sessions once capture is on.
    writeHookFiles(this.paths, { disabledPlugins: this.cfg.meko.captureTranscripts ? [] : mekoPluginKeys() });
    // Each harness's own files wait on its CLI (its version, Kiro's agent file), in the background:
    // no one CLI has to be installed for AgentK to start.
    for (const h of this.harnesses.list()) {
      const ready = h.prepare(this.paths, this.cfg).catch((e) => this.log.warn(`preparing ${h.caps.label} failed`, errMsg(e)));
      // Kiro's plan is read once its version is known (whether it's available), unless stopping began meanwhile.
      if (h.id === 'kiro' && this.timersOn) void ready.then(() => (this.stopped ? undefined : this.kiroPlan.refresh()));
    }
    mkdirSync(this.paths.worktrees, { recursive: true });
    // So new worktrees skip a CLI's folder-trust prompt (Claude Code's).
    for (const h of this.harnesses.list()) await h.trustFolder?.(this.paths.worktrees, this.log);
    // In the background: it may remove large build trees, and every change it makes to an idle
    // worktree is skipped once a session has claimed it.
    void this.worktrees.sweep().catch((e) => this.log.warn('checking idle worktrees failed', errMsg(e)));
    const noWebUrl = mekoWebUrlWarning(this.cfg);
    if (noWebUrl) this.log.warn(noWebUrl);
    // Before any turn can end, so the first one after a restart can tell whether its design's questions changed.
    this.questions.preload();
    // Not awaited: startup doesn't wait on Meko.
    void this.claudeMeko.refresh();
    // Not awaited: the New session dialog shows branch names with it once it's in.
    void this.ghLogin(this.paths.home);
    // A session host that is down must not keep the web server from starting: the UI stays
    // reachable, and recovery (which needs the host) retries on every reconcile tick.
    await this.host.ensureServer().catch((e: unknown) => this.log.error('the session host is not available', e));
    await this.bindHooks();
    this.hooksOn = true;
    this.learn.recover();
    await this.sessions.recover();
    void this.sessions.refreshDiffStats().catch(() => undefined);
    pruneSessionEvents(this.db);
    this.prunePins();
    this.sweepStaged();
    if (this.timersOn) {
      this.every(5000, () => this.sessions.reconcile());
      this.every(HOOK_SOCKET_CHECK_MS, () => this.checkHookSocket());
      this.every(60000, () => this.sessions.refreshDiffStats());
      this.every(IDLE_WORKTREE_CHECK_MS, () => this.worktrees.expire());
      this.every(KIRO_PLAN_EVERY_MS, () => this.kiroPlan.refresh());
      this.every(INVITE_SYNC_MS, () => this.invites.sync());
      this.every(PIN_PRUNE_MS, () => this.prunePins());
      this.every(STAGED_SWEEP_MS, () => this.sweepStaged());
      this.callHome.start((ms, fn) => this.every(ms, fn));
      this.ybCallHome.start((ms, fn) => this.every(ms, fn));
      // Not awaited: startup doesn't wait on Meko's API.
      void this.syncInvites();
      this.lifecycle.start();
    }
  }

  async stop(): Promise<void> {
    this.stopped = true;
    for (const i of this.intervals) clearInterval(i);
    this.intervals = [];
    this.callHome.stop();
    this.ybCallHome.stop();
    this.lifecycle.stop();
    this.hooksOn = false;
    await this.hookCheck?.catch(() => undefined);
    this.sessions.mod.closeAll();
    this.sessions.stop();
    this.learn.stop();
    this.knowledge.stop();
    await this.closeHooks();
    // Let the last turns reach Meko; one cut off here catches up when the server starts again. Kiro's
    // one-shot runs already going (the plan read, deleting a run's session) get as long to end; one
    // slower than that is left to finish on its own, as nothing kills it.
    await Promise.all([
      this.capture.settled(STOP_CAPTURE_MS),
      within(this.kiroPlan.settled().then(kiroDeletesSettled), STOP_CAPTURE_MS),
    ]);
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
      (token, payload) => this.sessions.context(token, payload),
      (token, route, body, signal) => this.sessions.onMod(token, route, body, signal),
    );
    this.hookSockId = socketId(this.paths.hookSock);
    this.sessions.mod.resume();
  }

  /** Stops the hook server. Its socket file is removed only if it is still ours. */
  private async closeHooks(): Promise<void> {
    const server = this.hookServer;
    this.hookServer = null;
    if (!server) return;
    // Held mod polls would keep the socket open until they time out (20 s), and no hook could
    // reach the server meanwhile: answer them now, and hold none until it's bound again.
    this.sessions.mod.pause();
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

  private sweepStaged(): void {
    try {
      sweepStaged(this.paths.pastes);
    } catch (e) {
      this.log.warn('removing old images from the New session dialog failed', errMsg(e));
    }
  }

  private prunePins(): void {
    try {
      this.projects.prunePins();
    } catch (e) {
      this.log.warn('removing old Meko datapack pins failed', errMsg(e));
    }
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
