import { existsSync, mkdirSync, readFileSync, writeFileSync, renameSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { parse, stringify } from 'smol-toml';
import { claudeUserDir } from './claude.js';
import { DEFAULT_MEKO_URL, DEFAULT_MEKO_WEB_URL, mekoWebUrl } from '../shared/mekourls.js';
import type { MekoKeySource } from '../shared/types.js';
import { DEFAULT_HARNESS, HARNESS_IDS, isHarnessId, type HarnessId } from '../shared/harnesses.js';
import { expandHome, type Paths } from './paths.js';

export type UpdateChannel = 'stable' | 'prerelease' | 'off';

export interface Config {
  /** name: shown in the rail next to the user name; empty uses the machine's short host name. */
  server: { bind: string; port: number; name: string };
  tls: { cert: string; key: string };
  repos: { roots: string[]; depth: number; cloneInto: string };
  update: { channel: UpdateChannel; repo: string; requireAttestation: boolean; intervalHours: number };
  /**
   * captureTranscripts: send session transcripts to Meko conversations. Off by default until Meko's capture stops
   * extracting loaded skills as user requirements (MEKO-710); off, sessions also run without the Meko plugin.
   * apiUrl: Meko's REST API base (sharing); empty means the web app URL plus /api/v1.
   * enabled: false turns Meko off even when a key is set (here, in the environment or in Claude
   * Code's settings); AgentK then works without datapacks.
   * home: Meko's data folder, as Meko last told AgentK (the MEKO_HOME it sets on the commands it
   * runs); empty means Meko's default. AgentK's home is in it (paths.ts `mekoHome`).
   */
  meko: { enabled: boolean; url: string; apiKey: string; webUrl: string; apiUrl: string; captureTranscripts: boolean; home: string };
  /**
   * dbPath: where state.db is when the home is on a network filesystem, which only a home from before
   * the move into Meko's data folder can be (Meko keeps its folder on a local disk). `agentk install`
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
   */
  opencode: { command: string; enabled: boolean };
  /**
   * idlePerRepo: worktrees each repo keeps for the next session, build outputs and all (0 to 20; 0 removes them).
   * idleDays: an idle worktree no session has used for this many days is removed (0 to 365; 0 keeps it).
   */
  worktrees: { idlePerRepo: number; idleDays: number };
  /**
   * Anonymous usage stats (callhome.ts). enabled: send the report (daily; hourly in an install's first day). url: where to POST it
   * (the collector's full report URL); empty uses the one release builds carry (CALL_HOME_URL next to VERSION), and with neither
   * nothing is sent. The same report also goes to YugabyteDB's call home service: diagnosticsUrl, or else
   * https://diagnostics.yugabyte.com from release installs only. enabled (or YB_DISABLE_CALLHOME) turns off both.
   */
  callHome: { enabled: boolean; url: string; diagnosticsUrl: string };
  /**
   * resolveConflicts: when a PR in PR & Review conflicts with its base, AgentK asks Claude to resolve the conflicts
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
   * AgentK's session tools (tools.ts): enabled gives every session the `agentk_sessions` MCP server (`agentk mcp`) and its
   * skill, which open projects, start sessions and move them between stages, from each session's next launch. Off, the
   * tools' route refuses calls from sessions that still have them. Set in Settings → General.
   */
  tools: { enabled: boolean };
}

export { DEFAULT_MEKO_URL, DEFAULT_MEKO_WEB_URL, mekoWebUrl };

export const DEFAULT_CONFIG: Config = {
  server: { bind: '0.0.0.0', port: 7777, name: '' },
  tls: { cert: '', key: '' },
  repos: { roots: ['~/code'], depth: 1, cloneInto: '~/code' },
  update: { channel: 'stable', repo: 'yugabyte/agent-k-local', requireAttestation: false, intervalHours: 6 },
  meko: { enabled: true, url: '', apiKey: '', webUrl: '', apiUrl: '', captureTranscripts: false, home: '' },
  state: { dbPath: '' },
  claude: { command: 'claude', enabled: true, mod: true },
  agents: { default: DEFAULT_HARNESS },
  kiro: { command: 'kiro-cli', enabled: true, trustTools: false },
  opencode: { command: 'opencode', enabled: true },
  worktrees: { idlePerRepo: 3, idleDays: 14 },
  callHome: { enabled: true, url: '', diagnosticsUrl: '' },
  pr: { resolveConflicts: true },
  askAgentk: { enabled: true, model: 'sonnet', timeoutSeconds: 45 },
  tools: { enabled: true },
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
  const cfg = existsSync(paths.config)
    ? merge(structuredClone(DEFAULT_CONFIG), mapKeys(parse(readFileSync(paths.config, 'utf8')), camel), warn)
    : structuredClone(DEFAULT_CONFIG);
  // Older versions wrote every default, so a Meko URL equal to the default was never chosen:
  // leave it unset so MEKO_MCP_URL or Claude Code's settings can still name the server.
  if (cfg.meko.url === DEFAULT_MEKO_URL) cfg.meko.url = '';
  if (!isHarnessId(cfg.agents.default)) {
    warn(`config.toml: agents.default should be one of ${HARNESS_IDS.join(', ')}; using ${DEFAULT_HARNESS}`);
    cfg.agents.default = DEFAULT_HARNESS;
  }
  return cfg;
}

export function loadConfig(paths: Paths, warn: (msg: string) => void = (m) => console.warn(m)): Config {
  const cfg = readConfigFile(paths, warn);
  // Environment overrides (used by `npm run dev`); saveConfig keeps them out of the file.
  if (process.env.AK_BIND) cfg.server.bind = process.env.AK_BIND;
  if (process.env.AK_PORT) cfg.server.port = Number(process.env.AK_PORT);
  return cfg;
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
 * installs. A value that came from AK_BIND or AK_PORT is written as the file had it.
 */
export function saveConfig(paths: Paths, cfg: Config): void {
  const out = structuredClone(cfg);
  const bind = process.env.AK_BIND;
  const port = process.env.AK_PORT;
  if ((bind && out.server.bind === bind) || (port && out.server.port === Number(port))) {
    const onDisk = readConfigFile(paths, () => undefined);
    if (bind && out.server.bind === bind) out.server.bind = onDisk.server.bind;
    if (port && out.server.port === Number(port)) out.server.port = onDisk.server.port;
  }
  mkdirSync(dirname(paths.config), { recursive: true });
  const header =
    '# AgentK configuration. Settings changed in the web UI are written here too.\n' +
    '# Only settings that differ from the defaults are listed; add any other key to change it.\n' +
    '# Changes to [server] or [tls] take effect after a restart.\n\n';
  const body = (changedFrom(mapKeys(out, snake), mapKeys(DEFAULT_CONFIG, snake)) ?? {}) as Record<string, unknown>;
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

/**
 * Meko credentials: config first, then the environment, then Claude Code's settings.json env.
 * The key comes from the first of those that has one. The URL comes from the first that has one,
 * except that a later source's URL is skipped when that source has a key of its own: it belongs
 * to that other key. With no URL, Meko Cloud.
 */
export function resolveMeko(cfg: Config): { url: string; apiKey: string } {
  const { url, apiKey } = resolveMekoWithSource(cfg);
  // Turned off: no key, so nothing talks to Meko.
  return { url, apiKey: cfg.meko.enabled ? apiKey : '' };
}

/** Meko is on: not turned off, and there's a key. Without it AgentK works without datapacks. */
export function mekoConfigured(cfg: Config): boolean {
  return !!resolveMeko(cfg).apiKey;
}

/** resolveMeko, plus where the key came from (null when there is none). */
export function resolveMekoWithSource(cfg: Config): { url: string; apiKey: string; keySource: MekoKeySource | null } {
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
  const url = sources.find((s, i) => s.url && (keyAt < 0 || i <= keyAt || !s.apiKey))?.url;
  const keySource = keyAt < 0 ? null : (['config', 'env', 'claude'] as const)[keyAt];
  return { url: url || DEFAULT_MEKO_URL, apiKey, keySource };
}

/** Meko web app URL: config first, then the environment, then derived from the MCP URL. */
export function resolveMekoWebUrl(cfg: Config): string | null {
  return mekoWebUrl(resolveMeko(cfg).url, cfg.meko.webUrl || envMekoWebUrl());
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
 * Meko's REST API base (used for sharing): config first, then MEKO_API_URL, then the web app URL
 * plus /api/v1 (the Meko web app serves its API on the same origin). Null when neither is known.
 */
export function resolveMekoApiUrl(cfg: Config): string | null {
  const override = cfg.meko.apiUrl.trim() || process.env.MEKO_API_URL?.trim() || '';
  if (override) return override.replace(/\/+$/, '');
  const web = resolveMekoWebUrl(cfg);
  return web ? `${web}/api/v1` : null;
}

/** Why datapack links (and sharing) are off, for the server log; null when the web app is known or Meko is off. */
export function mekoWebUrlWarning(cfg: Config): string | null {
  if (!mekoConfigured(cfg) || resolveMekoWebUrl(cfg)) return null;
  const off = resolveMekoApiUrl(cfg) ? 'datapack links are' : 'datapack links and sharing are';
  // Only the host: a URL can carry credentials, and this goes to the log.
  let host = 'this Meko';
  let local = false;
  try {
    const u = new URL(resolveMeko(cfg).url);
    host = u.host;
    local = isLoopback(u.hostname.replace(/^\[|\]$/g, ''));
  } catch {
    /* not a URL: keep the generic name */
  }
  // A Meko on this computer serves no web app yet: nothing to set.
  if (local) return `${off} off: the Meko on this computer (${host}) has no web app yet; they come with Meko's web UI`;
  return `the Meko web app for ${host} isn't known, so ${off} off. Set it on the Meko tab in Settings, or with [meko] web_url in config.toml or MEKO_WEB_URL`;
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
