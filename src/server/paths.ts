import { createHash } from 'node:crypto';
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
  /** Per-session launcher scripts and the Claude hooks settings file. */
  run: string;
  hookSock: string;
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
    run: join(home, 'run'),
    hookSock: hookSockPath(home),
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
function hookSockPath(home: string): string {
  const preferred = join(home, 'hook.sock');
  if (Buffer.byteLength(preferred) <= 100) return preferred;
  const uid = typeof process.getuid === 'function' ? process.getuid() : 0;
  const tag = createHash('sha256').update(home).digest('hex').slice(0, 12);
  return join(tmpdir(), `agent-k-local-${uid}`, `${tag}.sock`);
}

export function expandHome(p: string): string {
  if (p === '~') return homedir();
  if (p.startsWith('~/')) return join(homedir(), p.slice(2));
  return p;
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
