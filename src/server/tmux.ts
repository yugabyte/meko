import { execFile, spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { mkdirSync, writeFileSync } from 'node:fs';
import { promisify } from 'node:util';
import type { Paths } from './paths.js';
import { hostServices, type ServiceManager } from './service.js';

const exec = promisify(execFile);

/** Name of the keepalive session that holds the tmux server open. Never shown in the UI. */
export const KEEPALIVE = '_ak';
/** What the keepalive session runs: a long sleep any shell and any `sleep` understand (BSD sleep has no `infinity`). */
export const KEEPALIVE_CMD = 'sleep 2147483647';

// Works on tmux 2.7 (RHEL 8). Window options need set-window-option there.
// No prefix key: Ctrl+B belongs to Claude Code (it backgrounds a command), and tmux's own
// commands (detach, kill pane) must not be reachable from the terminal or the phone key bar.
const TMUX_CONF = `# Written by agent-k-local. Changes are overwritten on start.
set -g prefix None
set -g prefix2 None
unbind-key C-b
set -g default-terminal "screen-256color"
set -ga terminal-overrides ",xterm-256color:Tc"
set -g status off
set -g mouse on
set -g history-limit 50000
set -sg escape-time 0
set -g focus-events on
set -g set-clipboard on
set -g destroy-unattached off
set-window-option -g remain-on-exit on
set-window-option -g aggressive-resize on
`;

export function writeTmuxConf(paths: Paths): void {
  mkdirSync(paths.home, { recursive: true });
  writeFileSync(paths.tmuxConf, TMUX_CONF);
}

export interface PaneInfo {
  session: string;
  dead: boolean;
  pid: number;
  exitStatus: number | null;
}

export class Tmux {
  constructor(
    private socket: string,
    private confPath: string,
    private bin = 'tmux',
    private services: () => ServiceManager | null = hostServices,
  ) {}

  /** Args for a client attaching to a session (used by the terminal bridge). */
  attachArgs(name: string): string[] {
    return ['-L', this.socket, 'attach-session', '-t', `=${name}`];
  }

  get binary(): string {
    return this.bin;
  }

  async run(...args: string[]): Promise<string> {
    const { stdout } = await exec(this.bin, ['-L', this.socket, ...args], { maxBuffer: 16 * 1024 * 1024 });
    return stdout;
  }

  async serverRunning(): Promise<boolean> {
    try {
      await this.run('has-session', '-t', `=${KEEPALIVE}`);
      return true;
    } catch {
      return false;
    }
  }

  /**
   * Makes sure the tmux server is up. When the tmux service is installed it owns
   * the server (so server restarts never kill sessions); otherwise start it directly.
   */
  async ensureServer(): Promise<void> {
    if (await this.serverRunning()) return;
    const svc = this.services();
    if (svc?.installed('tmux')) {
      svc.start('tmux');
      for (let i = 0; i < 20; i++) {
        if (await this.serverRunning()) return;
        // launchd runs tmux in the foreground with no sessions: add the keepalive to that server.
        if (svc.tmuxForeground && (await this.addKeepalive())) return;
        await sleep(100);
      }
      // The service's server is up but its keepalive session is gone (killed by hand): put it back.
      // Only into a server that answers, so this never starts one in our own service.
      if (!svc.tmuxForeground && (await this.answers()) && (await this.addKeepaliveTo2_7())) return;
      // Never start tmux ourselves when the service owns it: it would run in the server's own
      // service, and every server restart or update would end every session.
      throw new Error(`The tmux service (${svc.id('tmux')}) isn't running. Check: ${svc.statusHint('tmux')}`);
    }
    await new Promise<void>((resolve, reject) => {
      const p = spawn(this.bin, ['-L', this.socket, '-f', this.confPath, 'new-session', '-d', '-s', KEEPALIVE, KEEPALIVE_CMD], {
        detached: true,
        stdio: 'ignore',
      });
      p.on('error', reject);
      p.on('exit', (code) => (code === 0 ? resolve() : reject(new Error(`tmux exited with ${code}`))));
      p.unref();
    });
    // Re-read the config in case the server predates this version of it.
    await this.run('source-file', this.confPath).catch(() => undefined);
  }

  /** True when a tmux server is listening on our socket, whatever sessions it has. */
  private async answers(): Promise<boolean> {
    try {
      await this.run('list-sessions');
      return true;
    } catch (e) {
      // A server with no sessions left exits, so "no sessions" still means it answered.
      return !isNoServer(e);
    }
  }

  /**
   * Creates the keepalive session in a server that was just seen answering. tmux 2.7 has no -N,
   * so this could only start a server of its own if that one exited in the moment between.
   */
  private async addKeepaliveTo2_7(): Promise<boolean> {
    try {
      await this.run('new-session', '-d', '-s', KEEPALIVE, KEEPALIVE_CMD);
      return true;
    } catch {
      return false;
    }
  }

  /** Creates the keepalive session in a running server. -N (tmux 3.2+) never starts a server of its own. */
  private async addKeepalive(): Promise<boolean> {
    try {
      await exec(this.bin, ['-N', '-L', this.socket, 'new-session', '-d', '-s', KEEPALIVE, KEEPALIVE_CMD]);
      return true;
    } catch {
      return false;
    }
  }

  /**
   * Every session's first pane. Empty when no tmux server is running; any other failure
   * throws, so callers never mistake a tmux hiccup for every pane being gone.
   */
  async panes(): Promise<Map<string, PaneInfo>> {
    const out = new Map<string, PaneInfo>();
    let text: string;
    try {
      text = await this.run('list-panes', '-a', '-F', '#{session_name}\t#{pane_dead}\t#{pane_pid}\t#{pane_dead_status}');
    } catch (e) {
      if (isNoServer(e)) return out;
      throw e;
    }
    for (const line of text.split('\n')) {
      if (!line.trim()) continue;
      const [session, dead, pid, status] = line.split('\t');
      if (session === KEEPALIVE || out.has(session)) continue;
      out.set(session, { session, dead: dead === '1', pid: Number(pid), exitStatus: status ? Number(status) : null });
    }
    return out;
  }

  async newSession(name: string, cwd: string, command: string): Promise<void> {
    await this.ensureServer();
    await this.run('new-session', '-d', '-s', name, '-c', cwd, '-x', '200', '-y', '50', command);
  }

  /** Restarts the command in a (usually dead) pane. */
  async respawn(name: string, cwd: string, command: string): Promise<void> {
    await this.run('respawn-pane', '-k', '-t', `=${name}:`, '-c', cwd, command).catch(async (e) => {
      // tmux 2.7 respawn-pane has no -c; the launcher script cds itself.
      if (String(e).includes('unknown option')) await this.run('respawn-pane', '-k', '-t', `=${name}:`, command);
      else throw e;
    });
  }

  async kill(name: string): Promise<void> {
    await this.run('kill-session', '-t', `=${name}`).catch(() => undefined);
  }

  /** Pastes text into the pane as a bracketed paste, then presses Enter. */
  async submit(name: string, text: string): Promise<void> {
    // A buffer of its own, so two pastes at once never overwrite each other.
    const buf = `ak-${name}-${randomUUID()}`;
    await new Promise<void>((resolve, reject) => {
      const p = spawn(this.bin, ['-L', this.socket, 'load-buffer', '-b', buf, '-'], { stdio: ['pipe', 'ignore', 'pipe'] });
      let err = '';
      p.stderr.on('data', (d) => (err += d));
      p.on('error', reject);
      p.on('exit', (code) => (code === 0 ? resolve() : reject(new Error(err || `load-buffer exited ${code}`))));
      p.stdin.end(text);
    });
    await this.run('paste-buffer', '-p', '-d', '-b', buf, '-t', `=${name}:`);
    await sleep(300);
    await this.run('send-keys', '-t', `=${name}:`, 'Enter');
  }

  async sendKeys(name: string, ...keys: string[]): Promise<void> {
    await this.run('send-keys', '-t', `=${name}:`, ...keys);
  }

  async capture(name: string): Promise<string> {
    return this.run('capture-pane', '-p', '-t', `=${name}:`);
  }
}

export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** What tmux says when there's no server on the socket (not running, or its socket is gone). */
export function isNoServer(e: unknown): boolean {
  const x = e as { stderr?: unknown; message?: unknown } | null;
  return /no server running|error connecting to/i.test(`${String(x?.stderr ?? '')} ${String(x?.message ?? '')}`);
}

/**
 * tmux session names can't contain '.' or ':'. Project ids and slugs never contain "--"
 * (see slugify), so the double dash keeps names of two projects on one repo apart
 * ("api" + "v2-fix" vs. "api-v2" + "fix"). Existing sessions keep their stored name.
 */
export function tmuxName(projectId: string, slug: string): string {
  return `ak-${projectId}--${slug}`.replace(/[.:]/g, '-');
}
