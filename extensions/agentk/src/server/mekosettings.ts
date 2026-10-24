import type { MekoSettings, MekoSettingsUpdate } from '../shared/types.js';
import { DEFAULT_MEKO_URL, DEFAULT_MEKO_WEB_URL, envMekoWebUrl, readClaudeSettingsEnv, resolveMeko, resolveMekoApiUrl, resolveMekoWebUrl, resolveMekoWithSource, type Config } from './config.js';
import type { Core } from './core.js';
import { errMsg } from './log.js';
import { checkMeko, tryMekoToken, type TokenTry } from './meko.js';
import { looksLikeMekoApi } from './mekoapi.js';
import {
  claudeHasMekoMcpAsync, latestMekoSetup, manualMekoSetup, mekoClaudeGaps, mekoKeyForConfig, readClaudeMeko, runMekoSetup,
} from './mekosetup.js';
import { claudeConfigPath } from './trust.js';
import { claudeUserDir } from './claude.js';
import { kiroAgentLoadsMcp, kiroMekoGaps, readKiroMeko } from './mekokiro.js';
import { localMeko } from './mekolocal.js';
import { HttpError } from './projects.js';

/** What the Meko tab needs to talk to Meko and to set up Claude Code; tests swap them out. */
export interface MekoTools {
  check: (url: string, apiKey: string) => Promise<{ ok: true } | { ok: false; error: string }>;
  /** Whether a URL answers like Meko's REST API (asked without the key). */
  checkApi: (apiUrl: string) => Promise<{ ok: true } | { ok: false; error: string }>;
  /** Runs Meko's setup for Claude Code against this server and key; rejects when it doesn't finish. */
  setUpClaude: (url: string, apiKey: string, log: (line: string) => void, claudeCommand: string) => Promise<void>;
  /** Runs Meko's setup for Kiro (`--client kiro`) against this server and key; rejects when it doesn't finish. */
  setUpKiro: (url: string, apiKey: string, log: (line: string) => void) => Promise<void>;
  /** Tries one of Claude Code's tokens on a Meko server: the datapacks it sees, or why not. */
  tryToken: (url: string, token: string) => Promise<TokenTry>;
  /**
   * Claude Code (`[claude] command`) has a `meko` MCP server (`mcp get meko`), asked when
   * ~/.claude.json has no user-scope one; null when it couldn't be asked.
   */
  claudeHasMcp: (claudeCommand: string) => Promise<boolean | null>;
}

export const defaultMekoTools: MekoTools = {
  check: checkMeko,
  checkApi: (apiUrl) => looksLikeMekoApi(apiUrl),
  async setUpClaude(url, apiKey, log, claudeCommand) {
    const manual = manualMekoSetup(url);
    const pkg = await latestMekoSetup().catch((e: unknown) => {
      throw new Error(`Couldn't look up Meko's setup on npm (${errMsg(e)}). Run it yourself: ${manual}`);
    });
    const code = await runMekoSetup(pkg, { url, apiKey, log });
    const c = readClaudeMeko(claudeUserDir(), claudeConfigPath());
    const registered = c.server ? true : await claudeHasMekoMcpAsync(claudeCommand);
    const left = mekoClaudeGaps(c, url, () => registered);
    if (code !== 0 || left.length) {
      throw new Error(`Meko's setup didn't finish.${left.length ? ` Still: ${left.join(' ')}` : ''} Run it yourself: ${manual}`);
    }
  },
  async setUpKiro(url, apiKey, log) {
    const manual = manualMekoSetup(url, 'kiro');
    const pkg = await latestMekoSetup().catch((e: unknown) => {
      throw new Error(`Couldn't look up Meko's setup on npm (${errMsg(e)}). Run it yourself: ${manual}`);
    });
    const code = await runMekoSetup(pkg, { url, apiKey, client: 'kiro', log });
    const left = kiroMekoGaps(readKiroMeko(), url, kiroAgentLoadsMcp());
    if (code !== 0 || left.length) {
      throw new Error(`Meko's setup for Kiro didn't finish.${left.length ? ` Still: ${left.join(' ')}` : ''} Run it yourself: ${manual}`);
    }
  },
  tryToken: tryMekoToken,
  claudeHasMcp: claudeHasMekoMcpAsync,
};

export function mekoSettings(cfg: Config): MekoSettings {
  const eff = resolveMekoWithSource(cfg);
  return {
    url: cfg.meko.url,
    effectiveUrl: eff.url,
    defaultUrl: DEFAULT_MEKO_URL,
    fallbackUrl: resolveMekoWithSource({ ...cfg, meko: { ...cfg.meko, url: '' } }).url,
    webUrl: cfg.meko.webUrl,
    apiUrl: cfg.meko.apiUrl,
    envWebUrl: envMekoWebUrl(),
    effectiveWebUrl: resolveMekoWebUrl(cfg),
    effectiveApiUrl: resolveMekoApiUrl(cfg),
    // Never the key itself; short keys and the local Meko's token get no hint at all.
    keyHint: eff.apiKey ? (eff.apiKey.length >= 12 && eff.keySource !== 'local' ? eff.apiKey.slice(-4) : '') : null,
    keySource: eff.keySource,
    local: localMeko().state,
  };
}

function checkUrl(v: string, what: string): void {
  if (!v) return;
  let u: URL;
  try {
    u = new URL(v);
  } catch {
    throw new HttpError(400, `The ${what} isn't a URL. It should look like https://meko.example.com.`);
  }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') throw new HttpError(400, `The ${what} must start with https:// or http://.`);
}

/** A URL's origin (scheme, host and port: never credentials), or null when it isn't one. */
function originOf(url: string | null): string | null {
  try {
    return url ? new URL(url).origin : null;
  } catch {
    return null;
  }
}

/** Where a saved key already goes (the MCP server and the API), and Meko Cloud's servers. */
function keyOrigins(cfg: Config): Set<string> {
  return new Set([resolveMekoWithSource(cfg).url, resolveMekoApiUrl(cfg), DEFAULT_MEKO_URL, DEFAULT_MEKO_WEB_URL]
    .map(originOf).filter((o): o is string => !!o));
}

/** The Meko settings the tab can change. */
const MEKO_KEYS = ['url', 'apiKey', 'webUrl', 'apiUrl'] as const;
type MekoKey = (typeof MEKO_KEYS)[number];

/** Where a key comes from that AgentK can't clear, in words. */
const KEY_ELSEWHERE = { env: 'the MEKO_API_KEY environment variable', claude: "Claude Code's settings" } as const;

/**
 * Only the fields a save changed, onto the config as it is now: the checks take a while, and
 * another change (Claude Code's setup clearing the key) may have landed meanwhile.
 */
export function applyMekoChange(c: Config, next: Config, changed: MekoKey[]): void {
  for (const k of changed) (c.meko as Record<MekoKey, unknown>)[k] = next.meko[k];
}

/**
 * Applies a Meko tab change to a copy of the config and checks it before anything is saved: a new
 * server and key must answer, and an API URL that comes from what was typed (the API or web app
 * URL) must answer like Meko's API. Returns the config to save, whether to reconnect, and whether
 * to sync invites again (a reconnect, or a new API URL); null when nothing changes.
 */
export async function nextMekoConfig(
  core: Core,
  tools: MekoTools,
  m: MekoSettingsUpdate,
): Promise<{ next: Config; changed: MekoKey[]; reconnect: boolean; resync: boolean } | null> {
  for (const k of MEKO_KEYS) {
    if (m[k] !== undefined && typeof m[k] !== 'string') throw new HttpError(400, `meko.${k} must be a string.`);
  }
  if (m.useLocal !== undefined && typeof m.useLocal !== 'boolean') throw new HttpError(400, 'meko.useLocal must be true or false.');
  if (m.useLocal && (m.apiKey?.trim() || m.url !== undefined)) throw new HttpError(400, 'Use the Meko on this computer, or save a URL and key: not both at once.');
  // One snapshot throughout: the checks below take a while, and another change may land meanwhile.
  const base = core.config;
  const next = structuredClone(base);
  if (m.url !== undefined) {
    const url = m.url.trim().replace(/\/+$/, '');
    checkUrl(url, 'MCP server URL');
    next.meko.url = url === DEFAULT_MEKO_URL ? '' : url;
  }
  if (m.webUrl !== undefined) {
    const url = m.webUrl.trim().replace(/\/+$/, '');
    checkUrl(url, 'web app URL');
    next.meko.webUrl = url;
  }
  if (m.apiUrl !== undefined) {
    const url = m.apiUrl.trim().replace(/\/+$/, '');
    checkUrl(url, 'API URL');
    next.meko.apiUrl = url;
  }
  if (m.apiKey?.trim()) {
    if (/\s/.test(m.apiKey.trim())) throw new HttpError(400, "A Meko API key has no spaces in it. Paste it again.");
    next.meko.apiKey = m.apiKey.trim();
  }
  if (m.useLocal) {
    // Without a key here, AgentK uses the Meko on this computer: unless the key comes from
    // somewhere AgentK can't clear, or no Meko runs here (asked again now).
    await core.refreshLocalMeko();
    next.meko.apiKey = '';
    next.meko.url = '';
    const to = resolveMekoWithSource(next).keySource;
    if (to === 'env' || to === 'claude') {
      throw new HttpError(400, `AgentK's Meko key comes from ${KEY_ELSEWHERE[to]}, which AgentK can't change. Remove it there to use the Meko on this computer. Nothing was saved.`);
    }
    if (to !== 'local') throw new HttpError(400, 'No Meko is running on this computer. Start it with meko start, then try again. Nothing was saved.');
  }
  // What this save changes, against the config as it was when it began.
  const changed = MEKO_KEYS.filter((k) => next.meko[k] !== base.meko[k]);
  if (!changed.length) return null;
  const before = resolveMeko(base);
  const after = resolveMeko(next);
  // A saved key goes only where it already goes, or to Meko Cloud. Sending it to another server
  // takes it typed again: otherwise anyone with the browser's login could point AgentK at their
  // own server and read the key off the check (or the next invite sync).
  const key = resolveMekoWithSource(next);
  if (!m.apiKey?.trim() && key.apiKey && key.apiKey === resolveMekoWithSource(base).apiKey) {
    const known = keyOrigins(base);
    const to = [key.url, resolveMekoApiUrl(next)].map(originOf).find((o) => o && !known.has(o));
    if (to) throw new HttpError(400, `Enter the Meko API key again to use it with ${to}: AgentK sends a saved key only to a Meko server it already uses. Nothing was saved.`);
  }
  const reconnect = before.url !== after.url || before.apiKey !== after.apiKey;
  if (reconnect) {
    if (!after.apiKey) throw new HttpError(400, 'Enter a Meko API key.');
    const r = await tools.check(after.url, after.apiKey);
    if (!r.ok) throw new HttpError(400, `Meko at ${after.url} didn't accept that key: ${r.error}. Nothing was saved.`);
  }
  const apiAfter = resolveMekoApiUrl(next);
  const resync = reconnect || resolveMekoApiUrl(base) !== apiAfter;
  // Worked out for Meko Cloud, the API URL is trusted; one that came from a typed URL is checked.
  const typed = next.meko.apiUrl !== base.meko.apiUrl || next.meko.webUrl !== base.meko.webUrl;
  if (typed && apiAfter && apiAfter !== resolveMekoApiUrl(base)) {
    const r = await tools.checkApi(apiAfter);
    if (!r.ok) throw new HttpError(400, `${apiAfter} doesn't look like Meko's API: ${r.error}. Check the API or web app URL. Nothing was saved.`);
  }
  return { next, changed, reconnect, resync };
}

/** Why a setup can't run: there's no Meko to set up with. */
const NO_MEKO = "Meko isn't set up: start Meko on this computer (meko start), or add a Meko Cloud key.";

/**
 * Runs Meko's setup for Claude Code with AgentK's Meko. After it, config.toml drops a key that
 * Claude Code's settings now hold, so the two can't drift apart (same rule as the installer).
 */
export async function setUpClaudeMeko(core: Core, tools: MekoTools): Promise<void> {
  const meko = resolveMekoWithSource(core.config);
  if (!meko.apiKey) throw new HttpError(400, NO_MEKO);
  const tail: string[] = [];
  try {
    await tools.setUpClaude(meko.url, meko.apiKey, (line) => {
      core.log.info(`meko setup: ${line}`);
      tail.push(line);
      if (tail.length > 5) tail.shift();
    }, core.config.claude.command);
  } catch (e) {
    const why = errMsg(e).replace(/([^.!?])$/, '$1.');
    throw new HttpError(502, `${why}${tail.length ? ` Last output: ${tail.join(' / ')}` : ''}`);
  }
  core.log.info(`set up Meko for Claude Code (${meko.url})`);
  // The local Meko's token isn't AgentK's to keep: it reads it from Meko.
  if (meko.keySource === 'local') return;
  const env = readClaudeSettingsEnv();
  const keep = mekoKeyForConfig({
    apiKey: meko.apiKey, url: meko.url, typed: false, settingsKey: env.MEKO_API_KEY, settingsUrl: env.MEKO_MCP_URL,
    envKey: process.env.MEKO_API_KEY, defaultUrl: DEFAULT_MEKO_URL,
  });
  if (keep !== 'unchanged') {
    core.updateConfig((c) => {
      c.meko.apiKey = keep.apiKey;
      c.meko.url = keep.url;
    });
  }
}

/**
 * Runs Meko's setup for Kiro with AgentK's Meko: its MCP server and key in Kiro's MCP config, and
 * its agent with the capture hooks as Kiro's default (which Kiro sessions' `agentk` agent is built
 * from). AgentK's own config is left as it is: Kiro keeps its key in its own file.
 */
export async function setUpKiroMeko(core: Core, tools: MekoTools): Promise<void> {
  const meko = resolveMeko(core.config);
  if (!meko.apiKey) throw new HttpError(400, NO_MEKO);
  const tail: string[] = [];
  try {
    await tools.setUpKiro(meko.url, meko.apiKey, (line) => {
      core.log.info(`meko setup (kiro): ${line}`);
      tail.push(line);
      if (tail.length > 5) tail.shift();
    });
  } catch (e) {
    const why = errMsg(e).replace(/([^.!?])$/, '$1.');
    throw new HttpError(502, `${why}${tail.length ? ` Last output: ${tail.join(' / ')}` : ''}`);
  }
  core.log.info(`set up Meko for Kiro (${meko.url})`);
}
