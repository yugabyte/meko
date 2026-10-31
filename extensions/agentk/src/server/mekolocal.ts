import { execFile } from 'node:child_process';
import { homedir } from 'node:os';
import type { LocalMekoState } from '../shared/types.js';
import { withLocalBin } from './paths.js';

/**
 * A Meko running on this computer: its MCP URL, the client token its clients send, and its web
 * listener's URL, where browsers find AgentK at /ext/agentk/ (null from a Meko too old to serve it,
 * or while its web UI is off: `webOff`, after `meko ui stop`).
 */
export interface LocalMeko {
  url: string;
  token: string;
  webUrl?: string | null;
  webOff?: true;
}

/**
 * The Meko Cloud link Meko keeps for its extensions (`cloud` in `meko token --json`, Meko's
 * docs/cloud.md). `set`: Meko has had a link, linked or since unlinked; until then AgentK may hand
 * it a key of its own, once.
 *
 * Meko keeps the key and reaches Meko Cloud for its extensions itself (its `/mcp`, and its REST API
 * under `/api/v1/cloud`): its `cloud` says only whether it's linked (`linked`, set only from such a
 * Meko) and the web app, and `url`, `apiKey` and `apiUrl` are ''. An older Meko handed out the key:
 * linked, its `url` and `apiKey` are set, and AgentK calls Meko Cloud with them; unlinked, they're ''.
 */
export interface MekoCloudLink {
  set: boolean;
  url: string;
  apiKey: string;
  webUrl: string;
  apiUrl: string;
  linked?: boolean;
  /** The linked account, named without its key (Meko's `account`): a new key is another account. */
  account?: string;
}

/**
 * What `meko token --json` said: a running Meko, one that's installed but stopped, or none; and
 * the Meko Cloud link, which Meko reports running or not. `cloud` is undefined from a Meko too old
 * to keep one, and when `meko` couldn't be asked.
 */
export interface LocalMekoProbe {
  state: LocalMekoState;
  meko: LocalMeko | null;
  cloud?: MekoCloudLink;
  /** Meko couldn't read its link (its `cloud.error`): the link last read stands. */
  cloudUnreadable?: true;
}

const str = (v: unknown) => (typeof v === 'string' ? v : '');

/** Meko's `cloud`, or undefined when it isn't one (an older Meko has none) or Meko couldn't read it. */
function cloudOf(v: unknown): MekoCloudLink | undefined {
  if (!v || typeof v !== 'object' || typeof (v as { set?: unknown }).set !== 'boolean' || (v as { error?: unknown }).error) return undefined;
  const c = v as Record<string, unknown>;
  if (typeof c.linked === 'boolean' && !('api_key' in c)) {
    const webUrl = c.linked && /^https?:\/\//.test(str(c.web_url)) ? str(c.web_url) : '';
    return { set: c.set as boolean, url: '', apiKey: '', webUrl, apiUrl: '', linked: c.linked, account: c.linked ? str(c.account) : '' };
  }
  const url = /^https?:\/\//.test(str(c.url)) ? str(c.url) : '';
  const apiKey = url ? str(c.api_key) : '';
  return { set: c.set as boolean, url: apiKey ? url : '', apiKey, webUrl: apiKey ? str(c.web_url) : '', apiUrl: apiKey ? str(c.api_url) : '' };
}

const TIMEOUT_MS = 5000;

/**
 * The Meko on this computer, as `meko token --json` reports it (`meko` on PATH or in ~/.local/bin,
 * where Meko's installer links it), asked about the Meko in `mekoHome` when given: the data folder
 * AgentK's home is in (`mekoDirOf` in movehome.ts), else `[meko] home`. With neither (a dev or
 * test home), `meko` finds its own folder. Meko keeps its
 * token in the Keychain on macOS and knows whether its server runs, so AgentK asks it rather than
 * reading its files. `none`: no `meko` at all. `failed`: `meko` is there but failed, took too long,
 * or answered with something that isn't what Meko prints.
 */
export function probeLocalMeko(mekoHome: string | null, env: NodeJS.ProcessEnv = process.env): Promise<LocalMekoProbe> {
  const none: LocalMekoProbe = { state: 'none', meko: null };
  const failed: LocalMekoProbe = { state: 'failed', meko: null };
  return new Promise((resolve) => {
    const PATH = withLocalBin(env.PATH ?? '', env.HOME || homedir());
    const run = { ...env, PATH, ...(mekoHome ? { MEKO_HOME: mekoHome } : {}) };
    execFile('meko', ['token', '--json'], { env: run, timeout: TIMEOUT_MS, encoding: 'utf8' }, (err, stdout) => {
      if (err) return resolve((err as NodeJS.ErrnoException).code === 'ENOENT' ? none : failed);
      try {
        const r = JSON.parse(stdout) as { url?: unknown; token?: unknown; running?: unknown; web_url?: unknown; web_off?: unknown; cloud?: unknown };
        if (typeof r.url !== 'string' || !/^https?:\/\//.test(r.url) || typeof r.token !== 'string' || !r.token) return resolve(failed);
        const webUrl = typeof r.web_url === 'string' && /^https?:\/\//.test(r.web_url) ? r.web_url : null;
        const cloud = cloudOf(r.cloud);
        const unreadable = !!r.cloud && typeof r.cloud === 'object' && !!(r.cloud as { error?: unknown }).error;
        resolve({
          ...(r.running === true
            ? { state: 'running', meko: { url: r.url, token: r.token, webUrl, ...(r.web_off === true ? { webOff: true as const } : {}) } }
            : { state: 'stopped', meko: null }),
          ...(cloud ? { cloud } : {}),
          ...(unreadable ? { cloudUnreadable: true as const } : {}),
        });
      } catch {
        resolve(failed);
      }
    });
  });
}

/**
 * Whether the server's every-minute look for the Meko on this computer runs: while there's no Meko
 * at all (Meko normally starts AgentK; this covers a boot where it didn't), and, with a Meko Cloud
 * link, while there's no `meko` here, so the banner asking to install it goes once it's installed.
 * Once found, it isn't looked at again until the next read (a restart, Settings, Meko's notice of a
 * change to the link).
 */
export function lookForLocalMeko(configured: boolean, local: LocalMekoState): boolean {
  return !configured || local === 'none';
}

/** A URL's origin (scheme, host and port), or null when it isn't one. */
export function originOf(url: string): string | null {
  try {
    return new URL(url).origin;
  } catch {
    return null;
  }
}

/**
 * Meko is linked to Meko Cloud and reaches it for AgentK itself: through its `/mcp`, and its REST
 * API under `/api/v1/cloud` (`MekoCloudLink`). False under an older Meko, which handed out the key.
 */
export function linkedThroughMeko(cloud: MekoCloudLink | undefined): boolean {
  return cloud?.linked === true;
}

/**
 * A Meko Cloud API key (`mko_tkn_…`). The local Meko's client token is hex, so it's never one.
 */
export function isMekoCloudKey(key: string): boolean {
  return key.startsWith('mko_tkn_');
}

/**
 * A key and URL are a copy of the local Meko's: its token, or a key for its URL's origin. Meko's
 * setup for Claude Code writes the token into Claude Code's settings, older installs stored it in
 * config.toml, and `meko extension install` passes both. A copy goes stale when Meko moves to
 * another port or gets a new token, so AgentK uses the live pair instead (`resolveMekoWithSource`),
 * and never hands a copy to Meko as a Meko Cloud key. A Meko Cloud key is never a copy, even on the
 * local Meko's URL: a setup run against the local Meko without a key leaves MEKO_MCP_URL there next
 * to the Meko Cloud key the user had (`ownMekoKey` gives it Meko Cloud's URL).
 */
export function isLocalMeko(local: LocalMeko | null, url: string, key: string): boolean {
  if (!local || !key) return false;
  if (key === local.token) return true;
  return !isMekoCloudKey(key) && originOf(url) !== null && originOf(url) === originOf(local.url);
}

let last: LocalMekoProbe = { state: 'none', meko: null };
let handedOver = false;

/**
 * AgentK has handed Meko its own Meko Cloud key (mekohandover.ts, kept in state.db's kv): from then
 * on AgentK's own sources are never used, even while `meko` can't be asked.
 */
export function mekoLinkHandedOver(): boolean {
  return handedOver;
}

export function setMekoLinkHandedOver(v: boolean): void {
  handedOver = v;
}

/**
 * The Meko on this computer and its Meko Cloud link, as last read by a `LocalMekoReader`. Config's
 * key resolution reads both (`resolveMekoWithSource`); it's one per process, like the environment
 * that resolution also reads.
 */
export function localMeko(): LocalMekoProbe {
  return last;
}

/**
 * Reads the Meko on this computer when asked (one read at a time) and keeps the answer in memory,
 * never on disk: its port and token are Meko's, and can change.
 */
export class LocalMekoReader {
  private reading: Promise<boolean> | null = null;

  constructor(private probe: () => Promise<LocalMekoProbe>) {}

  /**
   * Reads it again; true when what AgentK would use changed (Meko started or stopped, a new port or
   * token, another Meko Cloud link). A read that couldn't ask `meko`, or whose link Meko couldn't
   * read, keeps the link last read, so a slow `meko` or a locked Keychain doesn't move AgentK to
   * another Meko.
   */
  refresh(): Promise<boolean> {
    this.reading ??= this.probe()
      .then((p) => {
        const keep = !p.cloud && (p.cloudUnreadable || (p.state !== 'running' && p.state !== 'stopped'));
        const next = keep && last.cloud ? { ...p, cloud: last.cloud } : p;
        const changed = next.meko?.url !== last.meko?.url || next.meko?.token !== last.meko?.token || JSON.stringify(next.cloud) !== JSON.stringify(last.cloud);
        last = next;
        return changed;
      })
      .finally(() => {
        this.reading = null;
      });
    return this.reading;
  }

  /** Forgets it (the server is stopping). */
  clear(): void {
    last = { state: 'none', meko: null };
  }
}
