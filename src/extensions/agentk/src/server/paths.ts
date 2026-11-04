import { createHash } from 'node:crypto';
import { existsSync, realpathSync, statSync } from 'node:fs';
import { homedir, hostname, tmpdir, userInfo } from 'node:os';
import { basename, dirname, isAbsolute, join, resolve } from 'node:path';
import { fsType, isNetworkFs } from './fstype.js';

/** The home's folder in the home directory before it moved into Meko's data folder (#236). */
export const OLD_HOME_NAME = '.agent-k-local';

/** The home of versions before the move: `~/.agent-k-local`. */
export function oldHome(home: string = homedir()): string {
  return join(home, OLD_HOME_NAME);
}

/** AgentK's home in Meko's data folder: `<data folder>/extensions/agentk`. */
export function homeIn(mekoDir: string): string {
  return join(mekoDir, 'extensions', 'agentk');
}

/** Meko's data folder can't be used: a MEKO_HOME (or `[meko] home`) on a network filesystem. */
export class MekoHomeError extends Error {}

export interface MekoHomeOptions {
  env?: NodeJS.ProcessEnv;
  /** The folder Meko last told AgentK (`[meko] home`), used when MEKO_HOME isn't set. */
  configured?: string;
  home?: string;
  user?: string;
  /** The shared local folder of the last fallback; tests replace it. */
  sharedTmp?: string;
  type?: (dir: string) => string | null;
}

/**
 * Meko's data folder, by Meko's own rule (`Resolve` in src/meko/internal/paths/paths.go; both are
 * tested against src/meko/internal/paths/testdata/resolve-cases.json): MEKO_HOME (or the folder
 * Meko last told AgentK), refused on a network filesystem; else ~/.meko on a local disk; else, for
 * a home folder on a network filesystem, $XDG_STATE_HOME/meko when that's local, else
 * /var/tmp/<user>/meko. Makes nothing: `makeHomeParent` in movehome.ts does that.
 */
export function mekoHome(o: MekoHomeOptions = {}): string {
  const env = o.env ?? process.env;
  const type = o.type ?? fsType;
  const network = (p: string) => isNetworkFs(existingAncestor(p), type);
  const named = env.MEKO_HOME || o.configured;
  if (named) {
    const abs = resolve(named);
    if (network(abs)) throw new MekoHomeError(`Meko's data folder ${abs} is on a network filesystem, which Meko refuses. Choose a folder on a local disk with MEKO_HOME.`);
    return abs;
  }
  const home = o.home ?? homedir();
  const def = join(home, '.meko');
  if (!network(def)) return def;
  const xdg = env.XDG_STATE_HOME;
  if (xdg && isAbsolute(xdg) && !network(join(xdg, 'meko'))) return join(xdg, 'meko');
  return join(o.sharedTmp ?? SHARED_TMP, o.user ?? userInfo().username, 'meko');
}

/** The shared local folder of Meko's last fallback (`paths.SharedTmp` in Meko). */
export const SHARED_TMP = '/var/tmp';

function existingAncestor(p: string): string {
  for (let d = p; ; d = dirname(d)) {
    if (existsSync(d) || dirname(d) === d) return d;
  }
}

/**
 * The home of a release install: the folder its running release is in (`<home>/app/<version>`,
 * `AK_APP_DIR` as the launcher sets it), or null when this isn't one (a source checkout).
 */
export function releaseHome(appDir: string | undefined = process.env.AK_APP_DIR): string | null {
  if (!appDir) return null;
  const app = dirname(appDir);
  return basename(app) === 'app' ? dirname(app) : null;
}

/**
 * Where an install without a release keeps its home: `<Meko's data folder>/extensions/agentk` when
 * it's there, else `~/.agent-k-local` when that's there (an install the move hasn't reached), else
 * the new one. With Meko's data folder refused, the old home if there is one.
 */
export function defaultHome(o: MekoHomeOptions = {}): string {
  const old = oldHome(o.home);
  let next: string;
  try {
    next = homeIn(mekoHome(o));
  } catch (e) {
    if (existsSync(old)) return old;
    throw e;
  }
  return existsSync(next) || !existsSync(old) ? next : old;
}

/**
 * Root of all AgentK state. A release install's is the folder its release is in, whatever AK_HOME
 * says: users don't pick where AgentK keeps its data, it's always in Meko's data folder (the move
 * brings older homes there, movehome.ts). AK_HOME names it only without a release (tests, dev).
 */
export function akHome(): string {
  return releaseHome() ?? process.env.AK_HOME ?? defaultHome();
}

export interface Paths {
  home: string;
  config: string;
  defaultDb: string;
  certs: string;
  /**
   * Installed app versions: app/<version>/, app/current -> <version>. Meko installs, updates and
   * prunes them, and keeps its update records beside them (update-pending.json, bad-versions).
   */
  app: string;
  worktrees: string;
  /** Per-session design files written during the Design stage: designs/<session>/design.md. */
  designs: string;
  /** Images pasted into a session's terminal from the browser: pastes/<session>/<time>-<random>.<ext>. */
  pastes: string;
  /** Per-session launcher scripts and the Claude hooks settings file. */
  run: string;
  hookSock: string;
  /**
   * The server's socket, its only listener: Meko's web listener serves it to browsers at /ext/agentk/
   * (`me_koder _handoff --json` tells Meko where it is). Only this user can connect to it.
   */
  webSock: string;
  /** The session host's socket (host/host.ts): every Claude runs in a pty the host holds. */
  hostSock: string;
  /** What the running session host wrote about itself: pid, version, app dir, protocol. */
  hostState: string;
  /** The running session host's lease on this home (instance.ts): one host per home. */
  hostLock: string;
  /** The running server's lease on this home (instance.ts): one server per home, even across hosts. */
  lock: string;
  logs: string;
}

export function getPaths(home: string = akHome()): Paths {
  return {
    home,
    config: join(home, 'config.toml'),
    defaultDb: join(home, 'state.db'),
    certs: join(home, 'certs'),
    app: join(home, 'app'),
    worktrees: join(home, 'worktrees'),
    designs: join(home, 'designs'),
    pastes: join(home, 'pastes'),
    run: join(home, 'run'),
    hookSock: sockPath(home, 'hook'),
    webSock: sockPath(home, 'web'),
    hostSock: sockPath(home, 'host'),
    hostState: join(home, 'run', 'host.json'),
    hostLock: join(home, 'host.lock'),
    lock: join(home, 'server.lock'),
    logs: join(home, 'logs'),
  };
}

/**
 * Unix socket paths are limited to 108 bytes on Linux and 104 on macOS; 100
 * fits both. When the home path is too long, use a short socket in a private
 * per-user directory under the temp dir.
 */
function sockPath(home: string, name: 'hook' | 'host' | 'web'): string {
  const preferred = join(home, `${name}.sock`);
  if (Buffer.byteLength(preferred) <= 100) return preferred;
  const uid = typeof process.getuid === 'function' ? process.getuid() : 0;
  const tag = createHash('sha256').update(home).digest('hex').slice(0, 12);
  // The hook socket kept its old name here, so a running Claude's hooks still find it.
  return join(tmpdir(), `agent-k-local-${uid}`, name === 'hook' ? `${tag}.sock` : `${tag}-${name}.sock`);
}

export function expandHome(p: string): string {
  if (p === '~') return homedir();
  if (p.startsWith('~/')) return join(homedir(), p.slice(2));
  return p;
}

/**
 * A repo's key: its real path, so a symlink or another path to it names the same repo. Projects
 * store it, and the worktree pool keys on it. A path that can't be resolved is its own key.
 */
export function repoKey(repo: string): string {
  try {
    return realpathSync(repo);
  } catch {
    return repo;
  }
}

type StatIds = (p: string) => { dev: number; ino: number };

const NET_ALIASES = new Map<string, string | null>();

/**
 * The path Claude should be given for `p`: without the automounter's `/net/<host>` prefix when
 * this machine has that folder at the same path without it (the same device and inode, as when
 * `/net/<this host>/data` mounts the local `/data`), else `p` itself. Claude Code (2.1.292 and
 * later) asks before every read or edit under `/net`, even in auto mode, and no permission rule
 * lifts that, so a home under `/net` would stop sessions on every file. The export root is the
 * shortest prefix that matches, so the rest of `p` needn't exist yet. Each prefix is checked once
 * per process; a stat that fails means no alias. Only this machine's own `/net/<host>` is looked
 * at (by host name): a stat into another host's mount can hang while its server is down.
 */
export function claudePath(p: string, stat: StatIds = statSync, cache = NET_ALIASES, self = hostname()): string {
  const m = /^\/net\/([^/]+)(?=\/)/.exec(p);
  if (!m || !sameHost(m[1], self)) return p;
  const host = m[0];
  const parts = p.slice(host.length).split('/').filter(Boolean);
  for (let i = 1; i <= parts.length; i++) {
    const local = `/${parts.slice(0, i).join('/')}`;
    const net = host + local;
    let alias = cache.get(net);
    if (alias === undefined) {
      alias = sameFolder(net, local, stat) ? local : null;
      cache.set(net, alias);
    }
    if (alias !== null) return alias + p.slice(net.length);
  }
  return p;
}

/**
 * Whether `/net/<name>` names the machine called `self`: the same name, or the same first label
 * when one of them is a short name (`box` and `box.example.com`). Two full names must match whole.
 */
function sameHost(name: string, self: string): boolean {
  const a = name.toLowerCase();
  const b = self.toLowerCase();
  if (!a || !b) return false;
  if (a.includes('.') && b.includes('.')) return a === b;
  return a.split('.')[0] === b.split('.')[0];
}

function sameFolder(a: string, b: string, stat: StatIds): boolean {
  try {
    const x = stat(a);
    const y = stat(b);
    return x.dev === y.dev && x.ino === y.ino;
  } catch {
    return false;
  }
}

/** Inverse of expandHome, for display. */
export function tildify(p: string): string {
  const h = homedir();
  return p === h ? '~' : p.startsWith(h + '/') ? '~' + p.slice(h.length) : p;
}

/** PATH with ~/.local/bin appended, unless it's already there. */
export function withLocalBin(path: string, home: string): string {
  const local = join(home, '.local', 'bin');
  const parts = path.split(':').filter(Boolean);
  return parts.includes(local) ? path : [...parts, local].join(':');
}

/**
 * Where gh and the agents usually are when a service's PATH lacks them: Homebrew's folders (Intel
 * and Apple silicon), where gh's macOS installer puts it too. Meko looks for agents there as well.
 */
const USUAL_BINS = ['/usr/local/bin', '/opt/homebrew/bin'];

/** PATH with ~/.local/bin and then USUAL_BINS appended, each unless it's already there. */
export function withUsualBins(path: string, home: string): string {
  const withLocal = withLocalBin(path, home);
  const parts = withLocal.split(':');
  return [withLocal, ...USUAL_BINS.filter((d) => !parts.includes(d))].join(':');
}
