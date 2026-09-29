import type { MekoSettings, MekoSettingsUpdate } from '../shared/types.js';
import { DEFAULT_MEKO_URL, readClaudeSettingsEnv, resolveMeko, resolveMekoApiUrl, resolveMekoWebUrl, resolveMekoWithSource, type Config } from './config.js';
import type { Core } from './core.js';
import { errMsg } from './log.js';
import { checkMeko, strOrNull } from './meko.js';
import { looksLikeMekoApi } from './mekoapi.js';
import { claudeHasMekoMcp, latestMekoSetup, manualMekoSetup, mekoClaudeGaps, mekoKeyForConfig, runMekoSetup } from './mekosetup.js';
import { claudeUserDir } from './claude.js';
import { HttpError } from './projects.js';

/** What the Meko tab needs to talk to Meko and to set up Claude Code; tests swap them out. */
export interface MekoTools {
  check: (url: string, apiKey: string) => Promise<{ ok: true } | { ok: false; error: string }>;
  /** Whether a URL answers like Meko's REST API (asked without the key). */
  checkApi: (apiUrl: string) => Promise<{ ok: true } | { ok: false; error: string }>;
  /** Runs Meko's setup for Claude Code against this server and key; rejects when it doesn't finish. */
  setUpClaude: (url: string, apiKey: string, log: (line: string) => void) => Promise<void>;
}

export const defaultMekoTools: MekoTools = {
  check: checkMeko,
  checkApi: (apiUrl) => looksLikeMekoApi(apiUrl),
  async setUpClaude(url, apiKey, log) {
    const manual = manualMekoSetup(url);
    const pkg = await latestMekoSetup().catch((e: unknown) => {
      throw new Error(`Couldn't look up Meko's setup on npm (${errMsg(e)}). Run it yourself: ${manual}`);
    });
    const code = await runMekoSetup(pkg, { url, apiKey, log });
    const left = mekoClaudeGaps({ claudeDir: claudeUserDir(), mcpRegistered: claudeHasMekoMcp, apiKey });
    if (code !== 0 || left.length) {
      throw new Error(`Meko's setup didn't finish${left.length ? ` (still missing: ${left.join(', ')})` : ''}. Run it yourself: ${manual}`);
    }
  },
};

export function mekoSettings(cfg: Config): MekoSettings {
  const eff = resolveMekoWithSource(cfg);
  const claude = readClaudeSettingsEnv();
  return {
    enabled: cfg.meko.enabled,
    url: cfg.meko.url,
    effectiveUrl: eff.url,
    defaultUrl: DEFAULT_MEKO_URL,
    webUrl: cfg.meko.webUrl,
    apiUrl: cfg.meko.apiUrl,
    effectiveWebUrl: resolveMekoWebUrl(cfg),
    effectiveApiUrl: resolveMekoApiUrl(cfg),
    // Never the key itself; short keys get no hint at all.
    keyHint: eff.apiKey ? (eff.apiKey.length >= 12 ? eff.apiKey.slice(-4) : '') : null,
    keySource: eff.keySource,
    claudeUrl: strOrNull(claude.MEKO_MCP_URL)?.trim() ?? null,
    claudeKeyMatches: !!eff.apiKey && claude.MEKO_API_KEY === eff.apiKey,
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

/** The Meko settings the tab can change; all but `enabled` are strings. */
const TEXT_KEYS = ['url', 'apiKey', 'webUrl', 'apiUrl'] as const;
const MEKO_KEYS = ['enabled', ...TEXT_KEYS] as const;
type MekoKey = (typeof MEKO_KEYS)[number];

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
 * to check the sharing mode again; null when nothing changes.
 */
export async function nextMekoConfig(
  core: Core,
  tools: MekoTools,
  m: MekoSettingsUpdate,
): Promise<{ next: Config; changed: MekoKey[]; reconnect: boolean; recheck: boolean } | null> {
  for (const k of TEXT_KEYS) {
    if (m[k] !== undefined && typeof m[k] !== 'string') throw new HttpError(400, `meko.${k} must be a string.`);
  }
  if (m.enabled !== undefined && typeof m.enabled !== 'boolean') throw new HttpError(400, 'meko.enabled must be true or false.');
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
    // Adding a key turns Meko on.
    next.meko.enabled = true;
  }
  if (m.enabled !== undefined) next.meko.enabled = m.enabled;
  // What this save changes, against the config as it was when it began.
  const changed = MEKO_KEYS.filter((k) => next.meko[k] !== base.meko[k]);
  if (!changed.length) return null;
  const before = resolveMeko(base);
  const after = resolveMeko(next);
  const reconnect = before.url !== after.url || before.apiKey !== after.apiKey;
  if (reconnect && next.meko.enabled) {
    if (!after.apiKey) throw new HttpError(400, 'Enter a Meko API key.');
    const r = await tools.check(after.url, after.apiKey);
    if (!r.ok) throw new HttpError(400, `Meko at ${after.url} didn't accept that key: ${r.error}. Nothing was saved.`);
  }
  const apiAfter = resolveMekoApiUrl(next);
  const recheck = reconnect || resolveMekoApiUrl(base) !== apiAfter;
  // Worked out for Meko Cloud, the API URL is trusted; one that came from a typed URL is checked.
  const typed = next.meko.apiUrl !== base.meko.apiUrl || next.meko.webUrl !== base.meko.webUrl;
  if (typed && apiAfter && apiAfter !== resolveMekoApiUrl(base)) {
    const r = await tools.checkApi(apiAfter);
    if (!r.ok) throw new HttpError(400, `${apiAfter} doesn't look like Meko's API: ${r.error}. Check the API or web app URL. Nothing was saved.`);
  }
  return { next, changed, reconnect, recheck };
}

/**
 * Runs Meko's setup for Claude Code with AgentK's Meko. After it, config.toml drops a key that
 * Claude Code's settings now hold, so the two can't drift apart (same rule as the installer).
 */
export async function setUpClaudeMeko(core: Core, tools: MekoTools): Promise<void> {
  const meko = resolveMeko(core.config);
  if (!meko.apiKey) throw new HttpError(400, 'Set a Meko API key first.');
  const tail: string[] = [];
  try {
    await tools.setUpClaude(meko.url, meko.apiKey, (line) => {
      core.log.info(`meko setup: ${line}`);
      tail.push(line);
      if (tail.length > 5) tail.shift();
    });
  } catch (e) {
    const why = errMsg(e).replace(/([^.!?])$/, '$1.');
    throw new HttpError(502, `${why}${tail.length ? ` Last output: ${tail.join(' / ')}` : ''}`);
  }
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
  core.log.info(`set up Meko for Claude Code (${meko.url})`);
}
