import { existsSync, mkdirSync, readFileSync, writeFileSync, renameSync } from 'node:fs';
import { dirname } from 'node:path';
import { parse, stringify } from 'smol-toml';
import { DEFAULT_MEKO_WEB_URL } from '../shared/mekourls.js';
import { AGENTK_TOUR, MAX_IDLE_STOP_HOURS } from '../shared/types.js';
import { DEFAULT_HARNESS, HARNESS_IDS, isHarnessId, type HarnessId } from '../shared/harnesses.js';
import {
  DEFAULT_CHECK, DEFAULT_DEPTH, DEFAULT_ROUNDS, DEFAULT_STRATEGY, MAX_DEPTH, MAX_ROUNDS, STRATEGIES, isStrategySetting, type ReviewSettings,
} from '../shared/codereview.js';
import { localMeko, mekoApiBase } from './mekolocal.js';
import { expandHome, type Paths } from './paths.js';

export interface Config {
  /**
   * name: shown in the rail next to the user name; empty uses the machine's short host name.
   * bind and port, and [tls]: where Kode listened, and its certificate, when it served browsers
   * itself. Meko serves it now (on Kode's old port, at /ext/kode/), and its handoff takes them
   * over once (`me_kode _handoff`, handoff.ts); nothing else reads them, and they're kept in
   * config.toml only for that.
   */
  server: { bind: string; port: number; name: string };
  tls: { cert: string; key: string };
  repos: { roots: string[]; depth: number; cloneInto: string };
  /**
   * captureTranscripts: send session transcripts to Meko conversations. Off by default until Meko's capture stops
   * extracting loaded skills as user requirements (MEKO-710); off, sessions also run without the Meko plugin.
   * The Meko Kode uses is the one running on this computer, read from Meko (mekolocal.ts), never
   * stored here. The url, api_key, web_url and api_url keys of releases before are ignored, but kept
   * in the file on save (`oldMekoKeys`): a rollback to such a release restores only state.db and reads
   * its key from them, and they may be the only copy of a key never linked in Meko.
   * home: Meko's data folder, as Meko last told Kode (the MEKO_HOME it sets on the commands it
   * runs); empty means Meko's default. Kode's home is in it (paths.ts `mekoHome`).
   */
  meko: { captureTranscripts: boolean; home: string };
  /**
   * dbPath: where state.db is when the home is on a network filesystem, which only a home from before
   * the move into Meko's data folder can be (Meko keeps its folder on a local disk). `meko kode install`
   * sets it for such a home (`localDbLocation`), and the move brings the database into the new home
   * and clears it (movehome.ts).
   */
  state: { dbPath: string };
  /**
   * command: how Claude Code is run. enabled: offer it for new sessions when `<command> --version`
   * answers. mod: load Kode's mod into sessions (Claude Code 2.1.287 or later), which takes Kode's
   * prompts and reports the context fill; off, prompts are typed into the terminal.
   */
  claude: { command: string; enabled: boolean; mod: boolean };
  /** default: the harness new sessions start on (`claude`, `opencode` or `kiro`) unless the New session dialog picks another. */
  agents: { default: HarnessId };
  /**
   * Kiro CLI (src/server/harness/kiro.ts). command: how it's run. enabled: offer it for new sessions when
   * `<command> --version` answers. trustTools: Kiro sessions start with every tool trusted (`--trust-tools=*`,
   * and on Kiro 3.0, whose engine ignores that flag, as the `kode-trusted` agent, which allows every
   * tool; from a session's next launch or resume), so Kiro runs any tool without asking.
   */
  kiro: { command: string; enabled: boolean; trustTools: boolean };
  /**
   * OpenCode (src/server/harness/opencode.ts). command: how it's run (the default, `opencode`, is also found in
   * `~/.opencode/bin`, where OpenCode's installer puts it). enabled: offer it for new sessions when `<command> --version` answers.
   * disableTracking: the OpenCode Kode runs (sessions, from their next launch or resume, and Ask Kode) sends no telemetry, checks
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
   * Anonymous usage stats (callhome.ts). enabled: send the report (daily; hourly in an install's first day) to YugabyteDB's
   * call home service (YB_DISABLE_CALLHOME turns it off too). diagnosticsUrl: where to POST it; empty is YB_DIAGNOSTICS_URL
   * (callhome.ts) from release installs only, so a dev server sends nothing.
   */
  callHome: { enabled: boolean; diagnosticsUrl: string };
  /**
   * resolveConflicts: when a PR in Pull Request conflicts with its base, Kode asks Claude to resolve the conflicts
   * (false leaves them to the user). Set in Settings → General; an edit to config.toml takes effect after a restart.
   */
  pr: { resolveConflicts: boolean };
  /**
   * Ask Kode in the command palette (askagentk.ts): an instruction in plain words, answered by one `claude -p`.
   * enabled: offer it. model: the model it asks (a Claude Code alias or a full name). timeoutSeconds: how long an
   * answer may take before Claude is stopped (5 to 300).
   */
  askKode: { enabled: boolean; model: string; timeoutSeconds: number };
  /**
   * Kode's session tools (tools.ts): enabled gives every session the `kode_sessions` MCP server (`me_kode mcp`) and its
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
   * tour: the bundled workshop a fresh install starts by itself (`agentk-101`), written by `meko
   * kode install` only when it made config.toml. The server's start runs it once while there are no
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

export { DEFAULT_MEKO_WEB_URL };

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
  meko: { captureTranscripts: false, home: '' },
  state: { dbPath: '' },
  claude: { command: 'claude', enabled: true, mod: true },
  agents: { default: DEFAULT_HARNESS },
  kiro: { command: 'kiro-cli', enabled: true, trustTools: false },
  opencode: { command: 'opencode', enabled: true, disableTracking: false },
  worktrees: { idlePerRepo: 3, idleDays: 14 },
  sessions: { idleStopHours: 24 },
  callHome: { enabled: true, diagnosticsUrl: '' },
  pr: { resolveConflicts: true },
  askKode: { enabled: true, model: 'sonnet', timeoutSeconds: 45 },
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
  // Meko installs Kode's updates, as the [update] in Meko's own config.toml says. An [update] here
  // is ignored, but for channel = "off", which Meko reads as Update automatically off for Kode
  // until that switch is set (saveConfig keeps it); the rest goes from the file at the next save.
  if (isObject(file) && 'update' in file) {
    warn(
      "config.toml: [update] is ignored: Meko installs Kode's updates, as the [update] in Meko's own config.toml says. " +
        'channel = "off" here keeps Update automatically off for Kode until it\'s turned on (Settings › Updates)',
    );
  }
  // [ask_agentk], Ask Kode's section before it was called Ask Kode, is [ask_kode] (the next save
  // writes it under that name).
  if (isObject(file) && 'askAgentk' in file) {
    if (!('askKode' in file)) file.askKode = file.askAgentk;
    delete file.askAgentk;
  }
  const cfg = file ? merge(structuredClone(DEFAULT_CONFIG), file, warn) : structuredClone(DEFAULT_CONFIG);
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
/** Whether config.toml has Kode's old [update] channel = "off" (its own updater's switch). */
function updatesWereOff(paths: Paths): boolean {
  try {
    const t = parse(readFileSync(paths.config, 'utf8')) as { update?: { channel?: unknown } };
    return t.update?.channel === 'off';
  } catch {
    return false;
  }
}

/** The `[meko]` keys of releases that kept a Meko key of their own, as config.toml has them; empty when none. */
function oldMekoKeys(paths: Paths): Record<string, string> {
  try {
    const t = parse(readFileSync(paths.config, 'utf8')) as { meko?: Record<string, unknown> };
    return Object.fromEntries(OLD_MEKO_KEYS.flatMap((k) => (typeof t.meko?.[k] === 'string' && t.meko[k] ? [[k, t.meko[k] as string]] : [])));
  } catch {
    return {};
  }
}

/** `[meko]` keys Kode no longer reads, which `saveConfig` keeps (`oldMekoKeys`). */
const OLD_MEKO_KEYS = ['url', 'api_key', 'web_url', 'api_url'] as const;

export function saveConfig(paths: Paths, cfg: Config): void {
  const out = structuredClone(cfg);
  mkdirSync(dirname(paths.config), { recursive: true });
  const header =
    '# Kode configuration. Settings changed in the web UI are written here too.\n' +
    '# Only settings that differ from the defaults are listed; add any other key to change it.\n' +
    "# Meko serves Kode to browsers: its web listener's port and access are [web] in Meko's config.toml.\n\n";
  const body = (changedFrom(mapKeys(out, snake), mapKeys(DEFAULT_CONFIG, snake)) ?? {}) as Record<string, unknown>;
  // An old [update] channel = "off" stays: Meko reads it as Update automatically off for Kode.
  if (updatesWereOff(paths)) body.update = { channel: 'off' };
  // An old Meko key stays too, unread, for a rollback and for linking it in Meko.
  const old = oldMekoKeys(paths);
  if (Object.keys(old).length) body.meko = { ...old, ...((body.meko as Record<string, unknown> | undefined) ?? {}) };
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

/**
 * The Meko Kode uses: always the one running on this computer, as `meko token --json` reports it
 * (mekolocal.ts), with its MCP URL and client token. That Meko serves its own datapacks and, while
 * it's linked to Meko Cloud, passes calls for the rest on to it, so Kode never tells them apart.
 * Empty while no Meko runs here: Kode then works without datapacks.
 */
export function resolveMeko(): { url: string; apiKey: string } {
  const local = localMeko().meko;
  return local ? { url: local.url, apiKey: local.token } : { url: '', apiKey: '' };
}

/** There's a Meko to use (one runs on this computer). Without one Kode works without datapacks. */
export function mekoConfigured(): boolean {
  return !!resolveMeko().apiKey;
}

/**
 * The web app Meko names for its datapacks' share links (`cloud.web_url`: Meko Cloud's while it's
 * linked); null when it names none.
 */
export function resolveMekoWebUrl(): string | null {
  return localMeko().cloud?.webUrl || null;
}

/**
 * The Meko server's host name, as PR descriptions name it (`<host>://<datapack id>`): the web app
 * Meko names, else the Meko on this computer's, else Meko Cloud's.
 */
export function mekoHost(): string {
  for (const u of [resolveMekoWebUrl(), resolveMeko().url]) {
    try {
      if (u) return new URL(u).hostname;
    } catch {
      /* not a URL: try the next */
    }
  }
  return new URL(DEFAULT_MEKO_WEB_URL).hostname;
}

/**
 * Meko's REST API base (sharing, uploads): the Meko on this computer's `/api/v1`, on its MCP URL's
 * origin (`mekoApiBase`), which serves its datapacks and passes the rest on to Meko Cloud while
 * it's linked. Null with no Meko.
 */
export function resolveMekoApiUrl(): string | null {
  return mekoApiBase(localMeko().meko);
}

/** The global code review settings, as `resolveReview` takes them. */
export function globalReview(cfg: Config): ReviewSettings {
  return { strategy: cfg.codeReview.strategy as ReviewSettings['strategy'], rounds: cfg.codeReview.rounds, depth: cfg.codeReview.depth, check: cfg.codeReview.check };
}
