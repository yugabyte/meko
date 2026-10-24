import { execFile } from 'node:child_process';
import { homedir } from 'node:os';
import type { LocalMekoState } from '../shared/types.js';
import { withLocalBin } from './paths.js';

/** A Meko running on this computer: its MCP URL and the client token its clients send. */
export interface LocalMeko {
  url: string;
  token: string;
}

/** What `meko token --json` said: a running Meko, one that's installed but stopped, or none. */
export interface LocalMekoProbe {
  state: LocalMekoState;
  meko: LocalMeko | null;
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
        const r = JSON.parse(stdout) as { url?: unknown; token?: unknown; running?: unknown };
        if (typeof r.url !== 'string' || !/^https?:\/\//.test(r.url) || typeof r.token !== 'string' || !r.token) return resolve(failed);
        resolve(r.running === true ? { state: 'running', meko: { url: r.url, token: r.token } } : { state: 'stopped', meko: null });
      } catch {
        resolve(failed);
      }
    });
  });
}

/** The running Meko on this computer (`probeLocalMeko`), or null. */
export async function findLocalMeko(mekoHome: string | null, env: NodeJS.ProcessEnv = process.env): Promise<LocalMeko | null> {
  return (await probeLocalMeko(mekoHome, env)).meko;
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
 * A key and URL are a copy of the local Meko's: its token, or a key for its URL's origin. Meko's
 * setup for Claude Code writes the token into Claude Code's settings, older installs stored it in
 * config.toml, and `meko install_extension` passes both. A copy goes stale when Meko moves to
 * another port or gets a new token, so AgentK uses the live pair instead (`resolveMekoWithSource`).
 */
export function isLocalMeko(local: LocalMeko | null, url: string, key: string): boolean {
  if (!local || !key) return false;
  return key === local.token || (originOf(url) !== null && originOf(url) === originOf(local.url));
}

let last: LocalMekoProbe = { state: 'none', meko: null };

/**
 * The Meko on this computer as last read by a `LocalMekoReader`. Config's key resolution falls back
 * to it when no key is set; it's one per process, like the environment that resolution also reads.
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

  /** Reads it again; true when what AgentK would use changed (Meko started or stopped, a new port or token). */
  refresh(): Promise<boolean> {
    this.reading ??= this.probe()
      .then((p) => {
        const changed = p.meko?.url !== last.meko?.url || p.meko?.token !== last.meko?.token;
        last = p;
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
