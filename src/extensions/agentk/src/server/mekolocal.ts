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
 * What AgentK needs of the Meko Cloud link Meko keeps (`cloud` in `meko token --json`, Meko's
 * docs/cloud.md). Meko keeps the key and passes calls on to Meko Cloud itself, so AgentK doesn't
 * know or ask whether it's linked. `account` names the account behind it without its key ('' while
 * unlinked): a new one is another account, whose invites aren't AgentK's. `webUrl` is the web app
 * Meko names for share links ('' when none).
 */
export interface MekoCloudLink {
  account: string;
  webUrl: string;
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

/**
 * Meko's `cloud`, or undefined when it isn't one (an older Meko has none) or Meko couldn't read it. An
 * older Meko's link, which handed out its key, has no `linked`, and reads as no account.
 */
function cloudOf(v: unknown): MekoCloudLink | undefined {
  if (!v || typeof v !== 'object' || typeof (v as { set?: unknown }).set !== 'boolean' || (v as { error?: unknown }).error) return undefined;
  const c = v as Record<string, unknown>;
  const linked = c.linked === true;
  return { account: linked ? str(c.account) : '', webUrl: linked && /^https?:\/\//.test(str(c.web_url)) ? str(c.web_url) : '' };
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
 * Meko's REST API on the Meko on this computer: `/api/v1` on its MCP URL's origin, its API listener.
 * It serves its datapacks and passes the rest on to Meko Cloud while it's linked. Null with no Meko.
 */
export function mekoApiBase(local: LocalMeko | null): string | null {
  const origin = local ? originOf(local.url) : null;
  return origin ? `${origin}/api/v1` : null;
}

let last: LocalMekoProbe = { state: 'none', meko: null };
/**
 * The Meko on this computer and its Meko Cloud link, as last read by a `LocalMekoReader`. Config's
 * resolution reads both (`resolveMeko`); it's one per process, like the environment
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
