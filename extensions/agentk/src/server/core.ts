import { CodeReviews } from './review/runner.js';
import { EventEmitter } from 'node:events';
import type { Server } from 'node:http';
import { mkdirSync, rmSync } from 'node:fs';
import { homedir, hostname, userInfo } from 'node:os';
import { join } from 'node:path';
import { HARNESS_CAPS, type HarnessId } from '../shared/harnesses.js';
import type { AppState, HarnessStatus, MekoKeySource, MekoStatus, ServerEvent, TokenUsage, UpdateStatus } from '../shared/types.js';
import * as github from './github.js';
import { TranscriptCapture, mekoPluginKeys } from './capture.js';
import { DesignQuestionStore } from './answers.js';
import { CommentService } from './comments.js';
import { SessionTools, type ActionProject } from './actions.js';
import { AskAgentk } from './askagentk.js';
import { GithubLinkService } from './ghlinks.js';
import { PrReviewService } from './prreview.js';
import {
  envMekoWebUrl, isLoopback, mekoConfigured, mekoWebUrlWarning, ownMekoKey, resolveMeko, resolveMekoApiUrl, resolveMekoWebUrl, resolveMekoWithSource,
  saveConfig, type Config,
} from './config.js';
import { all, kvGet, kvSet, run, type Db } from './db.js';
import { socketId, startHookServer } from './hooks.js';
import { claudeUserDir, writeHookFiles } from './claude.js';
import { writeToolFiles } from './tools.js';
import { Harnesses } from './harness/index.js';
import { kiroMekoGaps } from './mekokiro.js';
import { kiroDeletesSettled } from './kiro.js';
import { openCodeDeletesSettled } from './opencode.js';
import { KIRO_PLAN_EVERY_MS, KiroPlanStore } from './kiroplan.js';
import { PlanLimitsStore } from './limits.js';
import { sweepStaged } from './pastes.js';
import { consoleLogger, errMsg, type Logger } from './log.js';
import { InviteService } from './invites.js';
import { KnowledgeService } from './knowledge.js';
import { MekoClient } from './meko.js';
import { MekoCallLog } from './mekolog.js';
import { MekoAgents, mekoMcpGaps, readMekoAgents, type MekoAgentsRead } from './mekoagents.js';
import { MekoClaudeWatch } from './mekoclaude.js';
import {
  LocalMekoReader, isLocalMeko, localMeko, lookForLocalMeko, mekoLinkHandedOver, originOf, probeLocalMeko, setMekoLinkHandedOver, type LocalMekoProbe,
} from './mekolocal.js';
import { handOverMekoKey } from './mekohandover.js';
import { mekoDirOf } from './movehome.js';
import { readClaudeMeko } from './mekosetup.js';
import { MekoApi } from './mekoapi.js';
import { defaultMekoTools, type MekoTools } from './mekosettings.js';
import type { Paths } from './paths.js';
import { HttpError, ProjectService } from './projects.js';
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
import { readList } from './slack.js';
import { WorkQueue } from './workqueue.js';

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
  /** Meko's setup for Claude Code and Kiro, for Settings › Harness. */
  mekoTools?: MekoTools;
  /** How usage stats are sent (tests fake it); by default the global fetch. */
  callHomeFetch?: typeof fetch;
  /** Reads Kiro CLI's version (tests fake it); by default `<command> --version`. */
  kiroVersion?: (command: string) => Promise<string | null>;
  /** Reads OpenCode's version (tests fake it); by default `<command> --version`. */
  openCodeVersion?: (command: string) => Promise<string | null>;
  /** Asks the Meko on this computer for its URL and token (tests fake it); by default `meko token --json` on Meko's folder. */
  localMeko?: () => Promise<LocalMekoProbe>;
  /** Hands AgentK's own Meko Cloud key to Meko (mekohandover.ts); tests swap it out. */
  mekoHandOver?: typeof handOverMekoKey;
  /** Reads Meko's list of agents (mekoagents.ts; tests fake it); by default GET /api/v1/agents on the Meko running on this computer. */
  mekoAgents?: () => Promise<MekoAgentsRead>;
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
/** How long a read of Kiro's agent (for `MekoStatus.kiro`) and OpenCode's own config (`MekoStatus.opencode`) is reused (the state is pushed often). */
const AGENT_MEKO_TTL_MS = 10_000;
/** How often the Meko on this computer is looked for while there's no Meko, or no `meko` on this computer (`lookForLocalMeko`). */
const LOCAL_MEKO_EVERY_MS = 60_000;
/** After a failed connection to the local Meko, how long until another failure reads it again. */
const LOCAL_MEKO_RECHECK_MS = 30_000;
/** state.db's kv: AgentK handed Meko its own Meko Cloud key, so its own sources are never read again. */
const MEKO_HANDED_OVER_KEY = 'meko-link-handed-over';

/**
 * Which Meko account a Meko is, for whether a change forgets the account's invites: the Meko on
 * this computer is one account on any port or token; any other is its server's origin and key.
 * Null with no Meko.
 */
function mekoAccountOf(m: { url: string; apiKey: string; keySource: MekoKeySource | null }): string | null {
  if (!m.apiKey) return null;
  return m.keySource === 'local' ? 'local' : `${originOf(m.url)} ${m.apiKey}`;
}

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
  readonly workQueue: WorkQueue;
  readonly askAgentk: AskAgentk;
  /** AgentK's session tools: the calls `agentk mcp` makes from sessions. */
  readonly tools: SessionTools;
  /** AgentK's local code review before a task's PR opens (review/runner.ts). */
  readonly reviews: CodeReviews;
  readonly limits: PlanLimitsStore;
  /** Kiro's plan allowance for the rail. */
  readonly kiroPlan: KiroPlanStore;
  readonly invites: InviteService;
  readonly knowledge: KnowledgeService;
  readonly mekoTools: MekoTools;
  /** Whether Meko's capture in Claude Code's sessions reaches AgentK's Meko (MekoStatus.claude, with its MCP server from `mekoAgents`). */
  readonly claudeMeko: MekoClaudeWatch;
  /** Meko's list of the agents on this computer, which new sessions are gated on (mekoagents.ts). */
  readonly mekoAgents: MekoAgents;
  readonly callHome: CallHomeService;
  /** The same report to YugabyteDB's call home service, from release installs (to be the only one later). */
  readonly ybCallHome: CallHomeService;
  /** The Claude Code sessions launch with: its version (for AgentK's mod) and updating it. */
  readonly claudeCode: ClaudeCode;
  /** Claude Code was available at its last version change (`claudeChanged`). */
  private claudeAvailable = false;
  /** Every coding agent sessions can run on (src/server/harness/). */
  readonly harnesses: Harnesses;
  /** The last read of Kiro's agent (kiroMeko). */
  private kiroMekoAt: { at: number; gaps: string[] } | null = null;
  /** The last read of OpenCode's own Meko server (openCodeMeko). */
  private openCodeMekoAt: { at: number; own: boolean } | null = null;
  /** Meko's status as last worked out; `state()` adds the parts read live (`local`, the agents'). */
  mekoStatus: Omit<MekoStatus, 'local' | 'agentList'>;
  /** The Meko on this computer, read when needed (mekolocal.ts); used when no key is set. */
  private localReader: LocalMekoReader;
  /** When a failed connection last read the local Meko again. */
  private localRecheckAt = 0;
  private handOver: typeof handOverMekoKey;
  /** This process tried handing AgentK's own key to Meko (once: a refused key isn't sent again and again). */
  private handOverTried = false;
  /** The Meko account AgentK last used (`mekoAccountOf`), kept through a spell with no Meko. */
  private mekoAccount: string | null = null;
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
    this.localReader = new LocalMekoReader(o.localMeko ?? (() => probeLocalMeko(this.mekoHome())));
    this.handOver = o.mekoHandOver ?? handOverMekoKey;
    setMekoLinkHandedOver(kvGet(this.db, MEKO_HANDED_OVER_KEY) === '1');
    // Meko restarted on another port, or with a new token, or its Meko Cloud link changed while
    // AgentK didn't hear: read it again before the retry.
    this.meko.onConnectFail = async () => {
      const source = resolveMekoWithSource(this.cfg).keySource;
      if ((source !== 'local' && source !== 'meko') || Date.now() - this.localRecheckAt < LOCAL_MEKO_RECHECK_MS) return;
      this.localRecheckAt = Date.now();
      await this.refreshMeko();
    };
    this.services = o.services ?? (() => servicesFor(o.paths.home));
    this.legacyTmuxPanes = o.legacyTmuxPanes ?? (() => legacyTmuxPanes(o.paths));
    this.host = o.host ?? new SessionHost(o.paths, { services: this.services, onHello: () => this.changed() });
    this.timersOn = o.timers ?? true;
    this.username = safeUsername();
    this.hostFull = hostname();
    this.updateStatus = () => ({ current: this.version, justUpdatedFrom: null, lastUpdatedFrom: null });
    this.claudeCode = new ClaudeCode({ config: () => this.cfg, changed: () => this.claudeChanged() });
    this.mekoAgents = new MekoAgents({ db: this.db, read: o.mekoAgents ?? (() => readMekoAgents(localMeko().meko)), changed: () => this.changed(), log: this.log });
    this.harnesses = new Harnesses({
      claudeCode: this.claudeCode, paths: this.paths, log: this.log, changed: () => this.changed(), kiroVersion: o.kiroVersion, openCodeVersion: o.openCodeVersion,
      meko: () => this.mekoAgents.current(), refreshMeko: () => this.mekoAgents.refresh(),
    });
    this.projects = new ProjectService(this);
    this.worktrees = new WorktreePool(this);
    this.sessions = new SessionManager(this);
    this.lifecycle = new Lifecycle(this);
    this.learn = new Learner(this);
    this.comments = new CommentService(this);
    this.questions = new DesignQuestionStore(this);
    this.prReview = new PrReviewService(this);
    this.githubLinks = new GithubLinkService(this);
    this.workQueue = new WorkQueue({
      db: this.db,
      changed: () => this.changed(),
      warn: (m, d) => this.log.warn(m, d),
      reviewRequests: () => github.reviewRequests(homedir()),
      readSlack: () => readList(this.cfg, join(this.paths.run, 'work-sync')),
      claudeAvailable: () => this.harnesses.of('claude').status(this.cfg).available,
      githubTitle: async (repo, number) => (await github.issueOrPr(homedir(), repo, number)).title,
      settings: () => this.cfg.workQueue,
    });
    this.askAgentk = new AskAgentk({
      config: () => this.cfg,
      cwd: join(this.paths.run, 'ask'),
      projects: () => this.actionProjects(),
      record: (r) => this.record('INSERT INTO agentk_asks(at, ok, actions, ms) VALUES (?, ?, ?, ?)', r.at, r.ok ? 1 : 0, r.actions, r.ms),
      asker: async () => {
        const h = await this.harnesses.ensureDefault(this.cfg);
        if (!h) throw new HttpError(409, this.harnesses.noAgent(this.cfg));
        return h.ask;
      },
    });
    this.tools = new SessionTools(this);
    this.reviews = new CodeReviews(this);
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
      read: () => readClaudeMeko(claudeUserDir()),
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
    this.mekoAccount = mekoAccountOf(resolveMekoWithSource(o.config));
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
    const was = resolveMeko(this.cfg);
    const now = resolveMeko(next);
    this.cfg = next;
    // A harness may have written the old Meko key somewhere of its own (OpenCode's Meko server file).
    if (was.url !== now.url || was.apiKey !== now.apiKey) for (const h of this.harnesses.list()) h.mekoChanged?.(this.paths);
    this.mekoStatus = this.mekoStatusFor(next);
    this.changed();
    // A Meko change can make Claude Code's Meko differ (or match again).
    void this.claudeMeko.refresh();
    return next;
  }

  private mekoStatusFor(cfg: Config): Omit<MekoStatus, 'local' | 'agentList'> {
    const m = resolveMekoWithSource(cfg);
    return {
      configured: !!m.apiKey, url: m.apiKey ? m.url : null, source: m.keySource, webUrl: resolveMekoWebUrl(cfg),
      sharing: !!m.apiKey && !!resolveMekoApiUrl(cfg), claude: this.claudeMeko.status, kiro: null, opencode: null,
    };
  }

  /**
   * An agent's `meko` MCP server as a gap sentence, from Meko's list (`mekoMcpGaps`): none while
   * nothing is gated, or it's connected.
   */
  private mcpGaps(id: HarnessId): string[] {
    return mekoMcpGaps(HARNESS_CAPS[id].label, this.harnesses.mekoOf(id), this.mekoAgents.current()?.target.url ?? null);
  }

  /** `MekoStatus.claude`: its MCP server from Meko's list, then the capture part (`claudeMeko`); empty while that is. */
  claudeMekoStatus(): MekoStatus['claude'] {
    const c = this.claudeMeko.status;
    if (!mekoConfigured(this.cfg) || !this.harnesses.of('claude').status(this.cfg).available) return c;
    return { ...c, gaps: [...this.mcpGaps('claude'), ...c.gaps] };
  }

  /**
   * Whether Kiro's sessions reach AgentK's Meko (`MekoStatus.kiro`): its MCP server from Meko's
   * list, then whether the agent Kiro sessions are built from loads it. Null while Meko is off or
   * Kiro isn't available. Kiro's agent is read again at most every AGENT_MEKO_TTL_MS, or when `force`d.
   */
  kiroMeko(force = false): MekoStatus['kiro'] {
    if (!mekoConfigured(this.cfg) || !this.harnesses.of('kiro').status(this.cfg).available) return null;
    const now = Date.now();
    if (force || !this.kiroMekoAt || now - this.kiroMekoAt.at > AGENT_MEKO_TTL_MS) this.kiroMekoAt = { at: now, gaps: kiroMekoGaps() };
    const capture = this.kiroMekoAt.gaps;
    return { gaps: [...this.mcpGaps('kiro'), ...capture], capture };
  }

  /**
   * Which Meko OpenCode's sessions use (`MekoStatus.opencode`): AgentK's, which it adds to their
   * config, or a `meko` server of OpenCode's own (`Harnesses.openCodeOwnMeko`), which AgentK leaves
   * alone. Null while Meko is off or OpenCode isn't available. Read again at most every AGENT_MEKO_TTL_MS.
   */
  openCodeMeko(): MekoStatus['opencode'] {
    if (!mekoConfigured(this.cfg) || !this.harnesses.of('opencode').status(this.cfg).available) return null;
    const now = Date.now();
    if (!this.openCodeMekoAt || now - this.openCodeMekoAt.at > AGENT_MEKO_TTL_MS) this.openCodeMekoAt = { at: now, own: this.harnesses.openCodeOwnMeko() };
    return { own: this.openCodeMekoAt.own };
  }

  /**
   * Reads every harness's version again (`force`: now) and Meko's list of agents: Check again, and
   * Connect to Meko once the page has connected one. Answers with every harness's status.
   */
  async refreshHarnesses(force = false): Promise<HarnessStatus[]> {
    await Promise.all([this.harnesses.refresh(this.cfg, force), this.mekoAgents.refresh()]);
    // Kiro's agent may load its MCP servers now (Meko's connect, or its setup, changed it).
    this.kiroMekoAt = null;
    this.openCodeMekoAt = null;
    return this.harnesses.status(this.cfg);
  }

  /** The open projects with their sessions, as Ask AgentK and the session tools see them (their GitHub repo not read yet). */
  actionProjects(): ActionProject[] {
    return projectRows(this.db).map((r) => ({ id: r.id, name: r.name, repoPath: r.repo_path, sessions: this.sessions.listForProject(r.id) }));
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

  /** Tells open pages the server is restarting into an update Meko installed, before the restart closes their sockets. */
  notifyRestarting(): void {
    this.emit('notify', { type: 'restarting' } satisfies ServerEvent);
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

  /** The Meko data folder AgentK's home is in (movehome.ts), else `[meko] home`; null lets `meko` pick. */
  mekoHome(): string | null {
    return mekoDirOf(this.paths.home) ?? (this.cfg.meko.home || null);
  }

  /**
   * Reads the Meko on this computer and its Meko Cloud link again, and starts handing Meko AgentK's
   * own key when it has none yet (`startHandOver`, in the background). When that changes what AgentK
   * shows (the Meko, its source, its web app or API URL), shows it; when it changes the Meko AgentK
   * uses (it started, stopped, has a new port or token, or the link changed), reconnects. On the
   * same Meko account (the local Meko on any port or token, or the same server and key) it keeps the
   * invites and shares AgentK knows; on another it forgets them, as `mekoChanged` does. A spell with
   * no Meko at all (the local Meko stopped) isn't a change of account. A copy of the local Meko's
   * URL and token in config.toml (older installs stored one, current or stale) is dropped.
   */
  async refreshMeko(): Promise<void> {
    const before = resolveMekoWithSource(this.cfg);
    const shown = this.mekoStatus;
    const apiBefore = resolveMekoApiUrl(this.cfg);
    const wasState = localMeko().state;
    // Another Meko, or the link changed (which moves the agents): Meko's list of agents is read again too.
    if (await this.localReader.refresh()) void this.mekoAgents.refresh();
    if (this.stopped) return;
    this.startHandOver();
    const local = localMeko().meko;
    if (isLocalMeko(local, this.cfg.meko.url, this.cfg.meko.apiKey)) {
      this.updateConfig((c) => {
        c.meko.url = '';
        c.meko.apiKey = '';
      });
      this.log.info('config.toml held the Meko on this computer\'s URL and token; AgentK now reads them from Meko');
    }
    const after = resolveMekoWithSource(this.cfg);
    const status = this.mekoStatusFor(this.cfg);
    const apiAfter = resolveMekoApiUrl(this.cfg);
    const reconnect = before.url !== after.url || before.apiKey !== after.apiKey;
    // Kept on every read (a copy of the local token becoming the live local Meko changes nothing to
    // connect to, but names the account); compared only when there's another Meko to connect to.
    const account = mekoAccountOf(after);
    const switched = reconnect && !!account && !!this.mekoAccount && account !== this.mekoAccount;
    if (account) this.mekoAccount = account;
    if (!reconnect && status.source === shown.source && status.webUrl === shown.webUrl && apiAfter === apiBefore) {
      // Installed, or gone, without changing the Meko AgentK uses: the install banner follows it.
      // With a link, `meko` removed after it was found is seen only at the next read (a restart,
      // Settings, or Meko's notice), since the minute look stops once it's found (`lookForLocalMeko`).
      if (localMeko().state !== wasState) this.changed();
      return;
    }
    this.mekoStatus = status;
    this.changed();
    if (!reconnect) {
      // The same Meko and key, from another source or with another web app or API URL.
      if (apiAfter !== apiBefore) void this.syncInvites();
      return;
    }
    this.log.info(
      after.keySource === 'meko' ? `using Meko's Meko Cloud link (${after.url})`
        : after.apiKey ? `using the Meko on this computer (${after.url})` : 'the Meko on this computer stopped; Meko is not set up',
    );
    // As in updateConfig: a harness may have written the old key somewhere of its own.
    for (const h of this.harnesses.list()) h.mekoChanged?.(this.paths);
    void this.claudeMeko.refresh();
    if (switched) return this.mekoChanged();
    await this.meko.reset();
    void this.syncInvites();
  }

  /**
   * Meko owns the Meko Cloud link: while it has never had one, AgentK hands it the key it used
   * itself (config.toml, MEKO_API_KEY, or Claude Code's settings), once per process, in the
   * background (`meko cloud_link` checks the key with Meko Cloud, which can take tens of seconds).
   * Not a copy of the local Meko's token, a key on this computer, or against a Meko too old to keep
   * a link. Once Meko takes it, AgentK never reads its own sources again (MEKO_HANDED_OVER_KEY), and
   * reads Meko again. config.toml's keys stay: a rollback to the release before, which reads only
   * them, still finds them. A failure is logged and AgentK keeps its key, so nothing moves to
   * another Meko unasked.
   */
  private startHandOver(): void {
    const { cloud, meko: local } = localMeko();
    if (!cloud || cloud.set || mekoLinkHandedOver() || this.handOverTried) return;
    const own = ownMekoKey(this.cfg);
    if (!own.apiKey || isLocalMeko(local, own.url, own.apiKey)) return;
    try {
      if (isLoopback(new URL(own.url).hostname.replace(/^\[|\]$/g, ''))) return;
    } catch {
      return;
    }
    this.handOverTried = true;
    const link = {
      url: own.url, apiKey: own.apiKey,
      webUrl: this.cfg.meko.webUrl || envMekoWebUrl(), apiUrl: this.cfg.meko.apiUrl || process.env.MEKO_API_URL?.trim() || '',
    };
    void this.handOver(this.mekoHome(), link)
      .then(async (r) => {
        if (this.stopped) return;
        if (!r.ok) {
          this.log.warn(`couldn't hand AgentK's Meko Cloud key (from ${own.keySource}) to Meko: ${r.error}. AgentK keeps using it until Meko Cloud is linked in Meko's web UI (Settings › Meko Cloud)`);
          return;
        }
        kvSet(this.db, MEKO_HANDED_OVER_KEY, '1');
        setMekoLinkHandedOver(true);
        this.log.info(`handed AgentK's Meko Cloud key (from ${own.keySource}) to Meko, which keeps the link now`);
        await this.refreshMeko();
      })
      .catch((e: unknown) => this.log.warn('handing AgentK\'s Meko Cloud key to Meko failed', errMsg(e)));
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
      meko: {
        ...this.mekoStatus, claude: this.claudeMekoStatus(), kiro: this.kiroMeko(), opencode: this.openCodeMeko(),
        agentList: this.mekoAgents.known(), local: localMeko().state,
      },
      update: this.updateStatus(),
      version: this.version,
      // The Unix account: Meko signs people in, and its page shows the login's name.
      username: this.username,
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
      workQueue: this.workQueue.list(),
      workSync: this.workQueue.syncState(),
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
    // The session tools' MCP config and skill: without them sessions start without the tools.
    try {
      if (!writeToolFiles(this.paths)) this.log.warn("AgentK's session tools are missing from this install; sessions start without them");
    } catch (e) {
      this.log.warn("couldn't write AgentK's session tools; sessions start without them", errMsg(e));
    }
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
    // Before the sessions are recovered, so a relaunch gets the Meko on this computer and Meko's
    // link. Bounded by `meko`'s own 5 s timeout: handing Meko AgentK's own key runs in the background.
    await this.refreshMeko();
    const noWebUrl = mekoWebUrlWarning(this.cfg);
    if (noWebUrl) this.log.warn(noWebUrl);
    // Before any turn can end, so the first one after a restart can tell whether its design's questions changed.
    this.questions.preload();
    // Not awaited: startup doesn't wait on Meko. Meko's list of agents kept from before gates new sessions meanwhile.
    void this.claudeMeko.refresh();
    void this.mekoAgents.refresh();
    // Not awaited: the New session dialog shows branch names with it once it's in.
    void this.ghLogin(this.paths.home);
    // A session host that is down must not keep the web server from starting: the UI stays
    // reachable, and recovery (which needs the host) retries on every reconcile tick.
    await this.host.ensureServer().catch((e: unknown) => this.log.error('the session host is not available', e));
    await this.bindHooks();
    this.hooksOn = true;
    this.learn.recover();
    await this.sessions.recover();
    await this.reviews.recover().catch((e: unknown) => this.log.warn('recovering local reviews failed', errMsg(e)));
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
      this.every(60_000, () => this.workQueue.tick());
      // Meko normally starts first (it starts AgentK); this covers a boot where it didn't, and an
      // install of Meko while the banner asks for one (with a Meko Cloud link too).
      this.every(LOCAL_MEKO_EVERY_MS, () => (lookForLocalMeko(mekoConfigured(this.cfg), localMeko().state) ? this.refreshMeko() : undefined));
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
    // No new plan read may start now: a turn reaching onTurnEnd during shutdown (hooks aren't
    // awaited) must not run `/usage` after the settled() wait below has already passed.
    this.kiroPlan.stop();
    this.hooksOn = false;
    await this.hookCheck?.catch(() => undefined);
    this.sessions.mod.closeAll();
    this.sessions.stop();
    this.learn.stop();
    this.knowledge.stop();
    await this.closeHooks();
    // Let the last turns reach Meko; one cut off here catches up when the server starts again. Kiro's
    // and OpenCode's one-shot runs already going (the plan read, deleting a run's session) get as long
    // to end; one slower than that is left to finish on its own, as nothing kills it.
    await Promise.all([
      this.capture.settled(STOP_CAPTURE_MS),
      within(this.kiroPlan.settled().then(kiroDeletesSettled), STOP_CAPTURE_MS),
      within(openCodeDeletesSettled(), STOP_CAPTURE_MS),
    ]);
    await this.meko.close();
    this.localReader.clear();
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
    // reach the server meanwhile: answer them now, and hold none until it's bound again. A session
    // tools call can run for minutes (a clone): it's answered now too, though its work may go on.
    this.sessions.mod.pause();
    this.tools.pause();
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
