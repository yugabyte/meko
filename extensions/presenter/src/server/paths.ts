// Where Presenter keeps its state: its home, `<Meko's data folder>/extensions/presenter`, by Meko's
// own rule for that folder (Resolve in backend/internal/paths, tested against the same
// resolve-cases.json). PRESENTER_HOME names another home, for dev and tests only.

import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { existsSync, realpathSync } from 'node:fs';
import { homedir, userInfo } from 'node:os';
import { basename, dirname, isAbsolute, join, resolve } from 'node:path';

// Linux names from `stat -f -c %T`; macOS names from `mount`. Meko's paths package checks the same.
const NETWORK_FS = new Set(['nfs', 'nfs4', 'cifs', 'smb2', 'smbfs', 'fuse.sshfs', 'afs', 'lustre', 'gpfs', 'afpfs', 'webdav', 'macfuse', 'osxfuse']);

function trySh(cmd: string, args: string[]): string | null {
  try {
    return execFileSync(cmd, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
  } catch {
    return null;
  }
}

/** A folder's filesystem type, or null when it can't be told. */
export function fsType(dir: string): string | null {
  if (process.platform !== 'darwin') return trySh('stat', ['-f', '-c', '%T', dir]);
  const last = (trySh('df', ['-P', dir]) ?? '').split('\n').at(-1) ?? '';
  const mp = /\s\d+%\s+(.+)$/.exec(last)?.[1];
  if (!mp) return null;
  const marker = ` on ${mp} (`;
  for (const line of (trySh('mount', []) ?? '').split('\n')) {
    const i = line.lastIndexOf(marker);
    if (i >= 0) return /^[^,)]+/.exec(line.slice(i + marker.length))?.[0].trim() ?? null;
  }
  return null;
}

export class MekoHomeError extends Error {}

export interface MekoHomeOptions {
  env?: NodeJS.ProcessEnv;
  home?: string;
  user?: string;
  /** The shared local folder of the last fallback; tests replace it. */
  sharedTmp?: string;
  type?: (dir: string) => string | null;
}

function existingAncestor(p: string): string {
  for (let d = p; ; d = dirname(d)) {
    if (existsSync(d) || dirname(d) === d) return d;
  }
}

/**
 * Meko's data folder: MEKO_HOME, refused on a network filesystem; else ~/.meko on a local disk;
 * else, for a home folder on a network filesystem, $XDG_STATE_HOME/meko when that's local, else
 * /var/tmp/<user>/meko. Makes nothing.
 */
export function mekoHome(o: MekoHomeOptions = {}): string {
  const env = o.env ?? process.env;
  const type = o.type ?? fsType;
  const network = (p: string) => {
    const t = type(existingAncestor(p));
    return !!t && NETWORK_FS.has(t);
  };
  if (env.MEKO_HOME) {
    const abs = resolve(env.MEKO_HOME);
    if (network(abs)) throw new MekoHomeError(`Meko's data folder ${abs} is on a network filesystem, which Meko refuses. Choose a folder on a local disk with MEKO_HOME.`);
    return abs;
  }
  const home = o.home ?? homedir();
  const def = join(home, '.meko');
  if (!network(def)) return def;
  const xdg = env.XDG_STATE_HOME;
  if (xdg && isAbsolute(xdg) && !network(join(xdg, 'meko'))) return join(xdg, 'meko');
  return join(o.sharedTmp ?? '/var/tmp', o.user ?? userInfo().username, 'meko');
}

/** Presenter's home in Meko's data folder. */
export function homeIn(mekoDir: string): string {
  return join(mekoDir, 'extensions', 'presenter');
}

/** The Meko data folder a home is in, or null for a home outside one (dev, tests). */
export function mekoDirOf(home: string): string | null {
  return basename(home) === 'presenter' && basename(dirname(home)) === 'extensions' ? dirname(dirname(home)) : null;
}

export function presenterHome(env: NodeJS.ProcessEnv = process.env): string {
  return env.PRESENTER_HOME ? resolve(env.PRESENTER_HOME) : homeIn(mekoHome({ env }));
}

export interface Paths {
  home: string;
  /** Settings: the Meko Cloud URL and key, when there's no Meko on this computer to ask. */
  config: string;
  run: string;
  /** The server's only listener: Meko's web listener serves it to browsers at /ext/presenter/. */
  webSock: string;
  pid: string;
  logs: string;
  /** Each deck's draft, Claude session folder and saved-pieces index: decks/<datapack id>/. */
  decks: string;
  /** Pieces by SHA-256, as fetched from Meko or made here: cache/<hash>. */
  cache: string;
}

export function getPaths(home: string = presenterHome()): Paths {
  return {
    home,
    config: join(home, 'config.json'),
    run: join(home, 'run'),
    webSock: sockPath(home),
    pid: join(home, 'run', 'server.pid'),
    logs: join(home, 'logs'),
    decks: join(home, 'decks'),
    cache: join(home, 'cache'),
  };
}

/**
 * Unix socket paths are limited to 108 bytes on Linux and 104 on macOS; 100 fits both. When the
 * home path is too long, the socket goes in a private per-user folder under /tmp. Not $TMPDIR:
 * the server starts from a shell, but Meko's login service runs `_handoff`, and their TMPDIRs differ.
 */
function sockPath(home: string): string {
  const preferred = join(home, 'run', 'web.sock');
  if (Buffer.byteLength(preferred) <= 100) return preferred;
  const uid = typeof process.getuid === 'function' ? process.getuid() : 0;
  const tag = createHash('sha256').update(home).digest('hex').slice(0, 12);
  return join('/tmp', `presenter-${uid}`, `${tag}-web.sock`);
}

function samePath(a: string, b: string): boolean {
  const real = (p: string) => {
    try {
      return realpathSync(p);
    } catch {
      return p;
    }
  };
  return real(a) === real(b);
}

/** The commands Meko runs, which must be for the Meko this home is in. */
export const MEKO_COMMANDS = ['start', 'stop', 'status', 'uninstall', '_handoff'];

/**
 * Refuses a command Meko runs for another data folder than the one this home is in, so a second
 * Meko (or Meko's tests) can't stop or uninstall this Presenter.
 */
export function refuseOtherMeko(paths: Paths, env: NodeJS.ProcessEnv = process.env): void {
  const own = mekoDirOf(paths.home);
  if (!env.MEKO_HOME || !own || samePath(resolve(env.MEKO_HOME), own)) return;
  throw new Error(`This Presenter belongs to the Meko in ${own}, and MEKO_HOME names ${resolve(env.MEKO_HOME)}: nothing was done. Run it with MEKO_HOME=${own}, or without MEKO_HOME.`);
}

/** PATH with ~/.local/bin, where Meko's installer links `meko`. */
export function withLocalBin(path: string, home: string = homedir()): string {
  const bin = join(home, '.local', 'bin');
  return path.split(':').includes(bin) ? path : path ? `${path}:${bin}` : bin;
}
