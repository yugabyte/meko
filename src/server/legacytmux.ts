import { execFile } from 'node:child_process';
import { existsSync, readFileSync, rmSync } from 'node:fs';
import { promisify } from 'node:util';
import { parse } from 'smol-toml';
import { errMsg, type Logger } from './log.js';
import type { Paths } from './paths.js';
import type { ServiceManager } from './service.js';

/**
 * Moving an install off tmux. Versions before the session host ran every Claude in tmux, on
 * socket `agentk` (`[tmux] socket` in config.toml), in a tmux service. On its first start, a
 * version with the session host ends those Claudes all at once; recovery then resumes each in
 * the host (`claude --resume`, with auto-continue for a turn that was cut off). The tmux service
 * is removed once the update is final: until then a rollback still needs it.
 *
 * Remove this file, its callers and `ServiceManager.removeLegacyTmux` after 2026-10-14.
 */

const exec = promisify(execFile);

/** The tmux socket name older versions used. */
export function legacySocket(paths: Paths): string {
  try {
    const cfg = parse(readFileSync(paths.config, 'utf8')) as { tmux?: { socket?: unknown } };
    if (typeof cfg.tmux?.socket === 'string' && cfg.tmux.socket) return cfg.tmux.socket;
  } catch {
    /* no config, or unreadable: the default */
  }
  return 'agentk';
}

export interface TmuxMove {
  paths: Paths;
  log: Logger;
  services: ServiceManager | null;
  /** The pane names of this home's sessions: only these are ended (another home may share the tmux server). */
  ours?: Set<string>;
  socket?: string;
  bin?: string;
}

/**
 * Ends this home's sessions still running in tmux and returns their names. Waits (up to 5 s) for
 * their Claudes to exit, so none is still running when recovery starts it again in the host.
 * Without a tmux service, a tmux server left with no AgentK sessions is ended too.
 */
export async function moveOffTmux(o: TmuxMove): Promise<string[]> {
  const socket = o.socket ?? legacySocket(o.paths);
  const tmux = (...args: string[]) => exec(o.bin ?? 'tmux', ['-L', socket, ...args]).then((r) => r.stdout);
  let text: string;
  try {
    text = await tmux('list-panes', '-a', '-F', '#{session_name}\t#{pane_pid}\t#{pane_dead}');
  } catch {
    return []; // no tmux, or no server on the socket
  }
  const moved: string[] = [];
  const pids: number[] = [];
  let others = false;
  for (const line of text.split('\n')) {
    const [name, pid, dead] = line.split('\t');
    if (!name?.startsWith('ak-') || moved.includes(name)) continue;
    if (o.ours && !o.ours.has(name)) {
      others = true;
      continue;
    }
    moved.push(name);
    if (dead !== '1' && Number(pid) > 0) pids.push(Number(pid));
  }
  for (const name of moved) await tmux('kill-session', '-t', `=${name}`).catch(() => undefined);
  if (moved.length) o.log.info(`moved off tmux: ended ${moved.length} session(s) there; they resume in the session host`);
  for (let i = 0; i < 50 && pids.some(alive); i++) await new Promise((r) => setTimeout(r, 100));
  if (!others && !o.services?.legacyTmuxInstalled()) {
    await tmux('kill-server').catch(() => undefined);
    rmSync(o.paths.tmuxConf, { force: true });
  }
  return moved;
}

/**
 * Removes the old tmux service once the update that brought the session host is final (a
 * rollback within the trial needs it), and once no other home has AgentK sessions on its tmux
 * server: removing the service ends that server, and the socket is shared by every home on the
 * account. True when there is nothing left to remove.
 */
export async function retireTmuxService(o: TmuxMove): Promise<boolean> {
  if (!o.services?.legacyTmuxInstalled()) return true;
  if (existsSync(o.paths.updatePending)) return false;
  const socket = o.socket ?? legacySocket(o.paths);
  try {
    const { stdout } = await exec(o.bin ?? 'tmux', ['-L', socket, 'list-sessions', '-F', '#{session_name}']);
    if (stdout.split('\n').some((name) => name.startsWith('ak-'))) return false;
  } catch {
    /* no tmux server on the socket: nothing on it to keep */
  }
  try {
    o.services.removeLegacyTmux();
    rmSync(o.paths.tmuxConf, { force: true });
    o.log.info('removed the tmux service: sessions run in the session host now');
    return true;
  } catch (e) {
    o.log.warn("couldn't remove the tmux service", errMsg(e));
    return false;
  }
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    return (e as NodeJS.ErrnoException).code === 'EPERM';
  }
}
