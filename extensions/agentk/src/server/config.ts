import { existsSync, mkdirSync, readFileSync, writeFileSync, renameSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { parse, stringify } from 'smol-toml';
import { claudeUserDir } from './claude.js';
import { DEFAULT_MEKO_URL, DEFAULT_MEKO_WEB_URL, mekoWebUrl } from '../shared/mekourls.js';
import { AGENTK_TOUR, MAX_IDLE_STOP_HOURS, type MekoKeySource } from '../shared/types.js';
import { DEFAULT_HARNESS, HARNESS_IDS, isHarnessId, type HarnessId } from '../shared/harnesses.js';
import {
  DEFAULT_CHECK, DEFAULT_DEPTH, DEFAULT_ROUNDS, DEFAULT_STRATEGY, MAX_DEPTH, MAX_ROUNDS, STRATEGIES, isStrategySetting, type ReviewSettings,
} from '../shared/codereview.js';
import { isLocalMeko, isMekoCloudKey, linkedThroughMeko, localMeko, mekoLinkHandedOver, originOf } from './mekolocal.js';
import { expandHome, type Paths } from './paths.js';

export interface Config {
  /**
   * name: shown in the rail next to the user name; empty uses the machine's short host name.
   * bind and port, and [tls]: where AgentK listened, and its certificate, when it served browsers
   * itself. Meko serves it now (on AgentK's old port, at /ext/agentk/), and its handoff takes them
   * over once (`me_koder _handoff`, handoff.ts); nothing else reads them, and they're kept in
   * config.toml only for that.
   */
  server: { bind: string; port: number; name: string };
  tls: { cert: string; key: string };
  repos: { roots: string[]; depth: number; cloneInto: string };
  /**
   * captureTranscripts: send session transcripts to Meko conversations. Off by default until Meko's capture stops
   * extracting loaded skills as user requirements (MEKO-710); off, sessions also run without the Meko plugin.
   * apiUrl: Meko's REST API base (sharing); empty means the web app URL plus /api/v1.
   * url, apiKey, webUrl and apiUrl: a Meko Cloud (or other) key from before Meko owned the link. AgentK
   * hands them to Meko once (mekohandover.ts) and never reads them again, but leaves them here, for a
   * rollback to the release before; until then (or under a Meko too old to keep a link) they stand in
   * for it. Meko's link, or else the Meko running on this computer, is
   * read from Meko (mekolocal.ts), never stored here.
   * home: Meko's data folder, as Meko last told AgentK (the MEKO_HOME it sets on the commands it
   * runs); empty means Meko's default. AgentK's home is in it (paths.ts `mekoHome`).
   */
  meko: { url: string; apiKey: string; webUrl: string; apiUrl: string; captureTranscripts: boolean; home: string };
  /**
   * dbPath: where state.db is when the home is on a network filesystem, which only a home from before
   * the move into Meko's data folder can be (Meko keeps its folder on a local disk). `me_koder install`
   * sets it for such a home (`localDbLocation`), and the move brings the database into the new home
   * and clears it (movehome.ts).
   */
  state: { dbPath: string };
  /**
   * command: how Claude Code is run. enabled: offer it for new sessions when `<command> --version`
   * answers. mod: load AgentK's mod into sessions (Claude Code 2.1.287 or later), which takes AgentK's
   * prompts and reports the context fill; off, prompts are typed into the terminal.
   */
  claude: { command: string; enabled: boolean; mod: boolean };
  /** default: the harness new sessions start on (`claude`, `opencode` or `kiro`) unless the New session dialog picks another. */
  agents: { default: HarnessId };
  /**
   * Kiro CLI (src/server/harness/kiro.ts). command: how it's run. enabled: offer it for new sessions when
   * `<command> --version` answers. trustTools: Kiro sessions start with every tool trusted (`--trust-tools=*`,
   * from a session's next launch or resume), so Kiro runs any tool without asking.
   */
  kiro: { command: string; enabled: boolean; trustTools: boolean };
  /**
   * OpenCode (src/server/harness/opencode.ts). command: how it's run (the default, `opencode`, is also found in
   * `~/.opencode/bin`, where OpenCode's installer puts it). enabled: offer it for new sessions when `<command> --version` answers.
   * disableTracking: the OpenCode AgentK runs (sessions, from their next launch or resume, and Ask AgentK) sends no telemetry, checks
   * for no update and offers none of OpenCode Zen's free models (`OPENCODE_NO_TRACKING_ENV` in opencode.ts).
   */
  opencode: { command: string; enabled: boolean; disableTracking: boolean };
  /**
   * idlePerRepo: worktrees each repo keeps for the next session, build outputs and all (0 to 20; 0 removes them).
   * idleDays: an idle worktree no session has used for this many days is removed (0 to 365; 0 keeps it).
   */
  worktrees: { idlePerRepo: number; idleDays: number };
  /**
   * idleStopHours: a session's agent idle this many hours is stopped to free its memory, and started again with `--resume`
   * when it's needed (SessionManager.stopIdle, docs/design/recovery.md); 0 to MAX_IDLE_STOP_HOURS, 0 never stops one.
   * Set in Settings › General.
   */
  sessions: { idleStopHours: number };
  /**
   * Anonymous usage stats (callhome.ts). enabled: send the report (daily; hourly in an install's first day). url: where to POST it
   * (the collector's full report URL); empty uses the one release builds carry (CALL_HOME_URL next to VERSION), and with neither
   * nothing is sent. The same report also goes to YugabyteDB's call home service: diagnosticsUrl, or else
   * YB_DIAGNOSTICS_URL (callhome.ts) from release installs only. enabled (or YB_DISABLE_CALLHOME) turns off both.
   */
  callHome: { enabled: boolean; url: string; diagnosticsUrl: string };
  /**
   * resolveConflicts: when a PR in Review & PR conflicts with its base, AgentK asks Claude to resolve the conflicts
   * (false leaves them to the user). Set in Settings → General; an edit to config.toml takes effect after a restart.
   */
  pr: { resolveConflicts: boolean };
  /**
   * Ask AgentK in the command palette (askagentk.ts): an instruction in plain words, answered by one `claude -p`.
   * enabled: offer it. model: the model it asks (a Claude Code alias or a full name). timeoutSeconds: how long an
   * answer may take before Claude is stopped (5 to 300).
   */
  askAgentk: { enabled: boolean; model: string; timeoutSeconds: number };
  /**
   * AgentK's session tools (tools.ts): enabled gives every session the `agentk_sessions` MCP server (`me_koder mcp`) and its
   * skill, which open projects, start sessions and move them between stages, from each session's next launch. Off, the
   * tools' route refuses calls from sessions that still have them. Set in Settings → General.
   */
  tools: { enabled: boolean };
  /**
   * The work queue's sync (workqueue.ts), also in Settings › Work queue. github and slack: which sources it reads (both
   * off, the default: no sync at all). syncMinutes: how often it syncs by itself (0: only Sync). slackList: the link
   * to the Slack list that Slack sync reads. slackModel: the model of the `claude -p` that reads it through your
   * Slack connector (slack.ts), and
   * slackTimeoutSeconds how long it may take (30 to 600). slackTools: the tools that run may call, your Slack
   * connector's tool that reads a file (the claude.ai Slack connector's `slack_read_file` by default, with
   * `slack_read_user_profile` to name the people a row mentions), besides ToolSearch, which needs no
   * permission and only loads tools. It loads none of your Claude Code settings but the sign-in part, so
   * nothing else is allowed (slack.ts).
   */
  workQueue: { github: boolean; slack: boolean; syncMinutes: number; slackList: string; slackModel: string; slackTimeoutSeconds: number; slackTools: string[] };
  /**
   * The code review before a PR opens (review/runner.ts, docs/design/code-review.md), also in Settings › Code review;
   * a project's settings and a session's own go over these. strategy: `off`, `sirius-1.0` or `sagittarius-3.0`. rounds:
   * re-reviews after the first (0 to MAX_ROUNDS). depth: sweeps in each review (1 to MAX_DEPTH). check: a round's findings
   * wait for the user to note or dismiss them before they go to the task (Run to merge never waits). models: the model each
   * tier of review step runs on, per harness (`[code_review.models.<harness>]`: large, mid, small); empty runs it on the
   * session's own model.
   */
  codeReview: { strategy: string; rounds: number; depth: number; check: boolean; models: Record<HarnessId, ReviewModels> };
  /**
   * tour: the bundled workshop a fresh install starts by itself (`agentk-101`), written by `me_koder
   * install` only when it made config.toml. The server's start runs it once while there are no
   * projects (src/server/workshops.ts) and then removes the key; empty starts nothing.
   */
  welcome: { tour: string };
}

/** The model for each tier of a review step on one harness (`[code_review.models.<harness>]`); '' is the session's own. */
export interface ReviewModels {
  large: string;
  mid: string;
  small: string;
}

export { DEFAULT_MEKO_URL, DEFAULT_MEKO_WEB_URL, mekoWebUrl };

/** The workshop a fresh install starts by itself (`[welcome] tour`). */
export const FRESH_INSTALL_TOUR = AGENTK_TOUR;

/**
 * The Slack sync may call only the claude.ai Slack connector's file read, and its profile read to name
 * the people a row mentions: no search, and no channel or thread read that a row's text could ask for.
 * The file read takes any file ID, so it's the prompt, not this limit, that keeps the run on the
 * configured list.
 */
const SLACK_READ_TOOLS = ['mcp__claude_ai_Slack__slack_read_file', 'mcp__claude_ai_Slack__slack_read_user_profile'];

export const DEFAULT_CONFIG: Config = {
  server: { bind: '0.0.0.0', port: 7777, name: '' },
  tls: { cert: '', key: '' },
  repos: { roots: ['~/code'], depth: 1, cloneInto: '~/code' },
  meko: { url: '', apiKey: '', webUrl: '', apiUrl: '', captureTranscripts: false, home: '' },
  state: { dbPath: '' },
  claude: { command: 'claude', enabled: true, mod: true },
  agents: { default: DEFAULT_HARNESS },
  kiro: { command: 'kiro-cli', enabled: true, trustTools: false },
  opencode: { command: 'opencode', enabled: true, disableTracking: false },
  worktrees: { idlePerRepo: 3, idleDays: 14 },
  sessions: { idleStopHours: 24 },
  callHome: { enabled: true, url: '', diagnosticsUrl: '' },
  pr: { resolveConflicts: true },
  askAgentk: { enabled: true, model: 'sonnet', timeoutSeconds: 45 },
  tools: { enabled: true },
  workQueue: { github: false, slack: false, syncMinutes: 15, slackList: '', slackModel: 'sonnet', slackTimeoutSeconds: 180, slackTools: [...SLACK_READ_TOOLS] },
  codeReview: {
    strategy: DEFAULT_STRATEGY,
    rounds: DEFAULT_ROUNDS,
    depth: DEFAULT_DEPTH,
    check: DEFAULT_CHECK,
    // Claude Code's aliases follow its newest models. OpenCode's and Kiro's model ids depend on the
    // providers and plans the user has, so their steps run on the session's own model unless set.
    models: {
      claude: { large: 'opus', mid: 'sonnet', small: 'haiku' },
      opencode: { large: '', mid: '', small: '' },
      kiro: { large: '', mid: '', small: '' },
    },
  },
  welcome: { tour: '' },
};

// TOML uses snake_case keys; the in-memory config uses camelCase.
const snake = (s: string) => s.replace(/[A-Z]/g, (c) => '_' + c.toLowerCase());
const camel = (s: string) => s.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase());

function mapKeys(obj: unknown, fn: (k: string) => string): unknown {
  if (Array.isArray(obj)) return obj.map((v) => mapKeys(v, fn));
  if (obj && typeof obj === 'object') {
    return Object.fromEntries(Object.entries(obj).map(([k, v]) => [fn(k), mapKeys(v, fn)]));
  }
  return obj;
}

const isObject = (v: unknown): v is Record<string, unknown> => !!v && typeof v === 'object' && !Array.isArray(v);
const isStringList = (v: unknown): v is string[] => Array.isArray(v) && v.every((x) => typeof x === 'string');

/** Layers file values over the defaults, keeping a default (with a warning) where the file has the wrong type. */
function merge<T>(base: T, over: unknown, warn: (msg: string) => void, at = ''): T {
  if (!isObject(over)) return base;
  const out: Record<string, unknown> = { ...(base as Record<string, unknown>) };
  for (const [k, v] of Object.entries(over)) {
    if (!(k in out)) continue; // ignore unknown keys
    const b = out[k];
    const key = at ? `${at}.${snake(k)}` : snake(k);
    if (isObject(b)) {
      if (isObject(v)) out[k] = merge(b, v, warn, key);
      else warn(`config.toml: [${key}] should be a table; using the defaults`);
    } else if (Array.isArray(b) ? isStringList(v) : typeof v === typeof b) {
      out[k] = v;
    } else {
      warn(`config.toml: ${key} should be ${Array.isArray(b) ? 'a list of strings' : `a ${typeof b}`}; using the default (${JSON.stringify(b)})`);
    }
  }
  return out as T;
}

/** What config.toml says, over the defaults, without environment overrides. */
function readConfigFile(paths: Paths, warn: (msg: string) => void): Config {
  const file = existsSync(paths.config) ? mapKeys(parse(readFileSync(paths.config, 'utf8')), camel) : null;
  // Meko installs AgentK's updates, as the [update] in Meko's own config.toml says. An [update] here
  // is ignored, but for channel = "off", which Meko reads as Update automatically off for AgentK
  // until that switch is set (saveConfig keeps it); the rest goes from the file at the next save.
  if (isObject(file) && 'update' in file) {
    warn(
      "config.toml: [update] is ignored: Meko installs AgentK's updates, as the [update] in Meko's own config.toml says. " +
        'channel = "off" here keeps Update automatically off for AgentK until it\'s turned on (Settings › Updates)',
    );
  }
  const cfg = file ? merge(structuredClone(DEFAULT_CONFIG), file, warn) : structuredClone(DEFAULT_CONFIG);
  // Older versions wrote every default, so a Meko URL equal to the default was never chosen:
  // leave it unset so MEKO_MCP_URL or Claude Code's settings can still name the server.
  if (cfg.meko.url === DEFAULT_MEKO_URL) cfg.meko.url = '';
  if (!isStrategySetting(cfg.codeReview.strategy)) {
    warn(`config.toml: code_review.strategy should be off or one of ${STRATEGIES.map((x) => x.id).join(', ')}; using ${DEFAULT_STRATEGY}`);
    cfg.codeReview.strategy = DEFAULT_STRATEGY;
  }
  const rounds = cfg.codeReview.rounds;
  if (!Number.isInteger(rounds) || rounds < 0 || rounds > MAX_ROUNDS) {
    warn(`config.toml: code_review.rounds should be a whole number from 0 to ${MAX_ROUNDS}; using ${DEFAULT_ROUNDS}`);
    cfg.codeReview.rounds = DEFAULT_ROUNDS;
  }
  const depth = cfg.codeReview.depth;
  if (!Number.isInteger(depth) || depth < 1 || depth > MAX_DEPTH) {
    warn(`config.toml: code_review.depth should be a whole number from 1 to ${MAX_DEPTH}; using ${DEFAULT_DEPTH}`);
    cfg.codeReview.depth = DEFAULT_DEPTH;
  }
  const hours = cfg.sessions.idleStopHours;
  if (!Number.isInteger(hours) || hours < 0 || hours > MAX_IDLE_STOP_HOURS) {
    warn(`config.toml: sessions.idle_stop_hours should be a whole number from 0 to ${MAX_IDLE_STOP_HOURS}; using ${DEFAULT_CONFIG.sessions.idleStopHours}`);
    cfg.sessions.idleStopHours = DEFAULT_CONFIG.sessions.idleStopHours;
  }
  if (!isHarnessId(cfg.agents.default)) {
    warn(`config.toml: agents.default should be one of ${HARNESS_IDS.join(', ')}; using ${DEFAULT_HARNESS}`);
    cfg.agents.default = DEFAULT_HARNESS;
  }
  return cfg;
}

export function loadConfig(paths: Paths, warn: (msg: string) => void = (m) => console.warn(m)): Config {
  return readConfigFile(paths, warn);
}

/** The values in `cfg` that differ from `base`, or undefined when none do. */
function changedFrom(cfg: unknown, base: unknown): unknown {
  if (isObject(cfg) && isObject(base)) {
    const out = Object.fromEntries(
      Object.entries(cfg).map(([k, v]) => [k, changedFrom(v, base[k])]).filter(([, v]) => v !== undefined),
    );
    return Object.keys(out).length ? out : undefined;
  }
  return JSON.stringify(cfg) === JSON.stringify(base) ? undefined : cfg;
}

/**
 * Writes the settings that differ from the defaults, so a changed default reaches existing
 * installs. `[server] bind` and `port` and `[tls]` are written as they are, for Meko's handoff.
 */
/** Whether config.toml has AgentK's old [update] channel = "off" (its own updater's switch). */
function updatesWereOff(paths: Paths): boolean {
  try {
    const t = parse(readFileSync(paths.config, 'utf8')) as { update?: { channel?: unknown } };
    return t.update?.channel === 'off';
  } catch {
    return false;
  }
}

export function saveConfig(paths: Paths, cfg: Config): void {
  const out = structuredClone(cfg);
  mkdirSync(dirname(paths.config), { recursive: true });
  const header =
    '# AgentK configuration. Settings changed in the web UI are written here too.\n' +
    '# Only settings that differ from the defaults are listed; add any other key to change it.\n' +
    "# Meko serves AgentK to browsers: its web listener's port and access are [web] in Meko's config.toml.\n\n";
  const body = (changedFrom(mapKeys(out, snake), mapKeys(DEFAULT_CONFIG, snake)) ?? {}) as Record<string, unknown>;
  // An old [update] channel = "off" stays: Meko reads it as Update automatically off for AgentK.
  if (updatesWereOff(paths)) body.update = { channel: 'off' };
  const tmp = paths.config + '.tmp';
  writeFileSync(tmp, header + stringify(body) + '\n', { mode: 0o600 });
  renameSync(tmp, paths.config);
}

export function dbPath(paths: Paths, cfg: Config): string {
  return cfg.state.dbPath ? expandHome(cfg.state.dbPath) : paths.defaultDb;
}

export function repoRoots(cfg: Config): string[] {
  return cfg.repos.roots.map(expandHome);
}

export function isLoopback(bind: string): boolean {
  return bind === '127.0.0.1' || bind === '::1' || bind === 'localhost';
}

/** A URL whose host is on this computer (`isLoopback`); false for anything that isn't a URL. */
export function onLoopback(url: string): boolean {
  try {
    return isLoopback(new URL(url).hostname.replace(/^\[|\]$/g, ''));
  } catch {
    return false;
  }
}

/**
 * Meko credentials. Meko owns the Meko Cloud link (`cloud` in `meko token --json`): once it has had
 * one (linked, or unlinked since), or once AgentK handed it its own key (`mekoLinkHandedOver`),
 * AgentK uses the Meko running on this computer, and nothing else. While it's linked, that Meko
 * reaches Meko Cloud for AgentK (`linkedThroughMeko`): its URL and token are AgentK's, with the
 * source `meko`. An older Meko handed out the link's key instead, which AgentK then uses itself.
 *
 * Before that (a Meko too old to keep a link, or one that was never set, with the handover still to
 * run) AgentK's own sources stand in: config first, then the environment, then Claude Code's
 * settings.json env. The key comes from the first of those that has one. The URL comes from the
 * first that has one, except that a later source's URL is skipped when that source has a key of
 * its own: it belongs to that other key, and so is a URL on this computer for a Meko Cloud key. With
 * no URL, Meko Cloud. With no key in any of them, the Meko running on this computer, whose token
 * goes only to its own URL; a key that's a copy of its token or for its URL (Meko's setup for
 * Claude Code writes one) is the live local Meko too, so a copy never goes stale. A Meko Cloud key
 * is never a copy (`isLocalMeko`).
 */
export function resolveMeko(cfg: Config): { url: string; apiKey: string } {
  const { url, apiKey } = resolveMekoWithSource(cfg);
  return { url, apiKey };
}

/** There's a Meko to use: a key, or the Meko on this computer. Without one AgentK works without datapacks. */
export function mekoConfigured(cfg: Config): boolean {
  return !!resolveMeko(cfg).apiKey;
}

/**
 * AgentK's own Meko key and URL (config, the environment, Claude Code's settings), before Meko owns
 * the link. A Meko Cloud key never takes a URL on this computer: Meko's setup run against the local
 * Meko without a key writes its URL next to the Meko Cloud key the user had. That holds whether or
 * not the local Meko runs, so the key is handed to Meko (which refuses a URL on this computer) either way.
 */
export function ownMekoKey(cfg: Config): { url: string; apiKey: string; keySource: 'config' | 'env' | 'claude' | null } {
  const sources = [
    () => ({ url: cfg.meko.url, apiKey: cfg.meko.apiKey }),
    () => ({ url: process.env.MEKO_MCP_URL, apiKey: process.env.MEKO_API_KEY }),
    () => {
      const s = readClaudeSettingsEnv();
      return { url: s.MEKO_MCP_URL, apiKey: s.MEKO_API_KEY };
    },
  ].map((f) => {
    const s = f();
    return { url: typeof s.url === 'string' ? s.url.trim() : '', apiKey: typeof s.apiKey === 'string' ? s.apiKey : '' };
  });
  const keyAt = sources.findIndex((s) => s.apiKey);
  const apiKey = keyAt < 0 ? '' : sources[keyAt].apiKey;
  const cloudKey = isMekoCloudKey(apiKey);
  const url = sources.find((s, i) => s.url && (keyAt < 0 || i <= keyAt || !s.apiKey) && !(cloudKey && onLoopback(s.url)))?.url || DEFAULT_MEKO_URL;
  return { url, apiKey, keySource: keyAt < 0 ? null : (['config', 'env', 'claude'] as const)[keyAt] };
}

/** resolveMeko, plus where the key came from (null when there is none). */
export function resolveMekoWithSource(cfg: Config): { url: string; apiKey: string; keySource: MekoKeySource | null } {
  const { meko: local, cloud } = localMeko();
  const fromLocal = local ? { url: local.url, apiKey: local.token, keySource: 'local' as const } : null;
  if (cloud?.set || mekoLinkHandedOver()) {
    if (cloud?.apiKey) return { url: cloud.url, apiKey: cloud.apiKey, keySource: 'meko' };
    if (linkedThroughMeko(cloud)) return local ? { url: local.url, apiKey: local.token, keySource: 'meko' } : { url: DEFAULT_MEKO_URL, apiKey: '', keySource: null };
    return fromLocal ?? { url: DEFAULT_MEKO_URL, apiKey: '', keySource: null };
  }
  const own = ownMekoKey(cfg);
  // The local Meko's URL and token are a pair: no other source's URL ever takes its token.
  if (fromLocal && (!own.apiKey || isLocalMeko(local, own.url, own.apiKey))) return fromLocal;
  return own;
}

/**
 * With the Meko on this computer, a web app or API URL from config or the environment is used only
 * on that Meko's own origin: the local token goes to no other server. Once Meko owns the link,
 * AgentK's own URLs aren't used at all.
 */
function forKey(override: string, eff: { url: string; keySource: MekoKeySource | null }): string {
  if (mekoOwnsLink()) return '';
  return eff.keySource !== 'local' || originOf(override) === originOf(eff.url) ? override : '';
}

/** Meko owns the link: it has had one, or AgentK handed it its key. */
function mekoOwnsLink(): boolean {
  return !!localMeko().cloud?.set || mekoLinkHandedOver();
}

/**
 * Meko web app URL: Meko's link's; else config first, then the environment, then derived from the
 * MCP URL.
 */
export function resolveMekoWebUrl(cfg: Config): string | null {
  const eff = resolveMekoWithSource(cfg);
  if (eff.keySource === 'meko') return localMeko().cloud?.webUrl || null;
  return mekoWebUrl(eff.url, forKey(cfg.meko.webUrl || envMekoWebUrl(), eff));
}

/** The web app URL MEKO_WEB_URL or Claude Code's settings give, used when config has none; '' when unset. */
export function envMekoWebUrl(): string {
  const fromClaude = readClaudeSettingsEnv().MEKO_WEB_URL;
  return process.env.MEKO_WEB_URL || (typeof fromClaude === 'string' ? fromClaude : '');
}

/**
 * The Meko server's host name, as PR descriptions name it (`<host>://<datapack id>`): the web
 * app's, else the MCP server's (a self-hosted Meko whose web app isn't set), else Meko Cloud's.
 */
export function mekoHost(cfg: Config): string {
  for (const u of [resolveMekoWebUrl(cfg), resolveMeko(cfg).url]) {
    try {
      if (u) return new URL(u).hostname;
    } catch {
      /* not a URL: try the next */
    }
  }
  return new URL(DEFAULT_MEKO_WEB_URL).hostname;
}

/**
 * Whether the Meko behind AgentK's REST API may hand out plain-http upload links: while the Meko on
 * this computer reaches Meko Cloud for AgentK, the Meko Cloud it's linked to (its web app on http, a
 * self-hosted Meko), not the local base's own http; undefined otherwise (MekoApi then asks of its
 * base). A link on the base's own origin, for a datapack that Meko serves itself, is always allowed
 * (`MekoApi.postPresigned`).
 */
export function mekoStorageHttp(cfg: Config): boolean | undefined {
  if (resolveMekoWithSource(cfg).keySource !== 'meko') return undefined;
  const cloud = localMeko().cloud;
  return linkedThroughMeko(cloud) ? (cloud?.webUrl ?? '').startsWith('http:') : undefined;
}

/**
 * Meko's REST API base (used for sharing and uploads): while Meko is linked, the Meko on this
 * computer's own `/api/v1`, which serves its datapacks and sends the rest to Meko Cloud (`routed`), or
 * its `/api/v1/cloud` pass-through from before it routed by id, or an older Meko's link's API URL; else config
 * first, then MEKO_API_URL, then the web app URL plus /api/v1 (the Meko web app serves its API on
 * the same origin). Null when neither is known.
 */
export function resolveMekoApiUrl(cfg: Config): string | null {
  const eff = resolveMekoWithSource(cfg);
  if (eff.keySource === 'meko') {
    const cloud = localMeko().cloud;
    if (!linkedThroughMeko(cloud)) return cloud?.apiUrl || null;
    return `${originOf(eff.url)}${cloud?.routed ? '/api/v1' : '/api/v1/cloud'}`;
  }
  const override = forKey(cfg.meko.apiUrl.trim() || process.env.MEKO_API_URL?.trim() || '', resolveMekoWithSource(cfg));
  if (override) return override.replace(/\/+$/, '');
  const web = resolveMekoWebUrl(cfg);
  return web ? `${web}/api/v1` : null;
}

/**
 * Why datapack links or sharing are off, for the server log; null when the web app is known or Meko
 * is off. The Meko on this computer links its datapacks to its own web UI, so for it only sharing
 * can be off: when no API URL is known (one set on its own origin, before Meko owns the link, counts).
 */
export function mekoWebUrlWarning(cfg: Config): string | null {
  if (!mekoConfigured(cfg) || resolveMekoWebUrl(cfg)) return null;
  // Only the host: a URL can carry credentials, and this goes to the log.
  let host = 'this Meko';
  try {
    host = new URL(resolveMeko(cfg).url).host;
  } catch {
    /* not a URL: keep the generic name */
  }
  if (resolveMekoWithSource(cfg).keySource === 'local') {
    if (resolveMekoApiUrl(cfg)) return null;
    return `sharing is off: the Meko on this computer (${host}) doesn't share datapacks. Link it to Meko Cloud in Meko's web UI, under Settings › Meko Cloud`;
  }
  const off = resolveMekoApiUrl(cfg) ? 'datapack links are' : 'datapack links and sharing are';
  return `the Meko web app for ${host} isn't known, so ${off} off. Set it in Meko's web UI, under Settings › Meko Cloud`;
}

/** The `env` block of Claude Code's user settings.json; empty when there's none. Values may be any JSON type. */
export function readClaudeSettingsEnv(): Record<string, unknown> {
  try {
    const s = JSON.parse(readFileSync(join(claudeUserDir(), 'settings.json'), 'utf8'));
    return isObject(s) && isObject(s.env) ? s.env : {};
  } catch {
    return {};
  }
}

/** The global code review settings, as `resolveReview` takes them. */
export function globalReview(cfg: Config): ReviewSettings {
  return { strategy: cfg.codeReview.strategy as ReviewSettings['strategy'], rounds: cfg.codeReview.rounds, depth: cfg.codeReview.depth, check: cfg.codeReview.check };
}
