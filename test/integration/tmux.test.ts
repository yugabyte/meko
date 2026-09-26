import { execFileSync, spawn } from 'node:child_process';
import { rmSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { ServiceManager } from '../../src/server/service.js';
import { KEEPALIVE, Tmux, writeTmuxConf } from '../../src/server/tmux.js';
import { getPaths } from '../../src/server/paths.js';
import { tempDir, waitFor } from '../helpers.js';

const tmux = (...a: string[]) => execFileSync('tmux', a, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();

function tmuxVersion(): [number, number] {
  const m = /(\d+)\.(\d+)/.exec(tmux('-V'));
  return m ? [Number(m[1]), Number(m[2])] : [0, 0];
}
const [major, minor] = tmuxVersion();
const hasForeground = major > 3 || (major === 3 && minor >= 2);

describe('tmux server (real tmux)', () => {
  let home: string;
  let socket: string;
  let conf: string;
  beforeEach(() => {
    home = tempDir('akl-tmux-');
    socket = `akl-tmux-${process.pid}-${Math.random().toString(36).slice(2, 8)}`;
    const paths = getPaths(join(home, 'ak'));
    writeTmuxConf(paths);
    conf = paths.tmuxConf;
  });
  afterEach(() => {
    try {
      tmux('-L', socket, 'kill-server');
    } catch {
      /* not running */
    }
    rmSync(home, { recursive: true, force: true });
  });

  it('starts its own server with the keepalive session when no service is installed', async () => {
    const t = new Tmux(socket, conf, 'tmux', () => null);
    await t.ensureServer();
    expect(await t.serverRunning()).toBe(true);
    const cmd = tmux('-L', socket, 'list-panes', '-t', `=${KEEPALIVE}:`, '-F', '#{pane_dead} #{pane_start_command}');
    expect(cmd).toMatch(/^0 .*sleep 2147483647/);
  });

  // What launchd does on macOS: the service runs `tmux -D` in the foreground with no sessions.
  it.skipIf(!hasForeground)('adds the keepalive to a foreground tmux started by the service, not a second server', async () => {
    let pid = 0;
    const launchd = {
      tmuxForeground: true,
      installed: () => true,
      start: () => {
        const p = spawn('tmux', ['-D', '-L', socket, '-f', conf], { detached: true, stdio: 'ignore' });
        pid = p.pid ?? 0;
        p.unref();
      },
    } as unknown as ServiceManager;
    const t = new Tmux(socket, conf, 'tmux', () => launchd);
    await t.ensureServer();
    expect(await t.serverRunning()).toBe(true);
    const serverPid = await waitFor(() => Number(tmux('-L', socket, 'list-sessions', '-F', '#{pid}').split('\n')[0]), (v) => v > 0);
    expect(serverPid).toBe(pid);
  });
});
