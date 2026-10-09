import { createHash } from 'node:crypto';
import { realpathSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { join } from 'node:path';

/** Root of all agent-k-local state. Override with AK_HOME (tests, dev). */
export function akHome(): string {
  return process.env.AK_HOME ?? join(homedir(), '.agent-k-local');
}

export interface Paths {
  home: string;
  config: string;
  defaultDb: string;
  certs: string;
  /** Installed app versions: app/<version>/, app/current -> <version>. */
  app: string;
  worktrees: string;
  /** Per-session design files written during the Design stage: designs/<session>/design.md. */
  designs: string;
  /** Images pasted into a session's terminal from the browser: pastes/<session>/<time>-<random>.<ext>. */
  pastes: string;
  /** Per-session launcher scripts and the Claude hooks settings file. */
  run: string;
  hookSock: string;
  /** The session host's socket (host/host.ts): every Claude runs in a pty the host holds. */
  hostSock: string;
  /** What the running session host wrote about itself: pid, version, app dir, protocol. */
  hostState: string;
  /** The running session host's lease on this home (instance.ts): one host per home. */
  hostLock: string;
  /** The running server's lease on this home (instance.ts): one server per home, even across hosts. */
  lock: string;
  /** Written by versions that ran sessions in tmux; removed once a session host has taken over. */
  tmuxConf: string;
  logs: string;
  /** Written by the updater before a restart; read by the bin wrapper for rollback. */
  updatePending: string;
  /** One version per line that failed to boot and must not be retried. */
  badVersions: string;
  downloads: string;
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
    hostSock: sockPath(home, 'host'),
    hostState: join(home, 'run', 'host.json'),
    hostLock: join(home, 'host.lock'),
    lock: join(home, 'server.lock'),
    tmuxConf: join(home, 'tmux.conf'),
    logs: join(home, 'logs'),
    updatePending: join(home, 'update-pending.json'),
    badVersions: join(home, 'bad-versions'),
    downloads: join(home, 'downloads'),
  };
}

/**
 * Unix socket paths are limited to 108 bytes on Linux and 104 on macOS; 100
 * fits both. When the home path is too long, use a short socket in a private
 * per-user directory under the temp dir.
 */
function sockPath(home: string, name: 'hook' | 'host'): string {
  const preferred = join(home, `${name}.sock`);
  if (Buffer.byteLength(preferred) <= 100) return preferred;
  const uid = typeof process.getuid === 'function' ? process.getuid() : 0;
  const tag = createHash('sha256').update(home).digest('hex').slice(0, 12);
  // The hook socket kept its old name here, so a running Claude's hooks still find it.
  return join(tmpdir(), `agent-k-local-${uid}`, name === 'hook' ? `${tag}.sock` : `${tag}-host.sock`);
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
