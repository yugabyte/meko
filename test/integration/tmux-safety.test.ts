import { execFileSync } from 'node:child_process';
import { chmodSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { ServiceManager } from '../../src/server/service.js';
import { Tmux, writeTmuxConf } from '../../src/server/tmux.js';
import { getPaths } from '../../src/server/paths.js';
import { tempDir, waitFor } from '../helpers.js';

const tmux = (...a: string[]) => execFileSync('tmux', a, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();

describe('tmux wrapper safety (real tmux)', () => {
  let home: string;
  let socket: string;
  let conf: string;
  beforeEach(() => {
    home = tempDir('akl-tmuxsafe-');
    socket = `akl-tmuxsafe-${process.pid}-${Math.random().toString(36).slice(2, 8)}`;
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

  it('has no prefix key, so Ctrl+B reaches Claude and tmux commands stay out of reach', async () => {
    const t = new Tmux(socket, conf, 'tmux', () => null);
    await t.ensureServer();
    expect(tmux('-L', socket, 'show-options', '-g', 'prefix')).toBe('prefix None');
    expect(tmux('-L', socket, 'show-options', '-g', 'prefix2')).toBe('prefix2 None');
  });

  it('never starts tmux itself when the tmux service is installed but not running', async () => {
    const svc = {
      tmuxForeground: false,
      installed: () => true,
      start: () => undefined,
      id: () => 'agent-k-local-tmux.service',
      statusHint: () => 'systemctl --user status agent-k-local-tmux',
    } as unknown as ServiceManager;
    const t = new Tmux(socket, conf, 'tmux', () => svc);
    await expect(t.ensureServer()).rejects.toThrow(/agent-k-local-tmux\.service\) isn't running\. Check: systemctl --user status/);
    expect(await t.serverRunning()).toBe(false);
  });

  it('lists no panes when no server runs, and throws on any other tmux failure', async () => {
    expect((await new Tmux(socket, conf, 'tmux', () => null).panes()).size).toBe(0);
    const bin = join(home, 'broken-tmux');
    writeFileSync(bin, '#!/bin/sh\necho "lost server" >&2\nexit 1\n');
    chmodSync(bin, 0o755);
    await expect(new Tmux(socket, conf, bin, () => null).panes()).rejects.toThrow(/lost server/);
  });

  it('pastes two prompts at once without either one being lost', async () => {
    const t = new Tmux(socket, conf, 'tmux', () => null);
    await t.ensureServer();
    const out = join(home, 'typed');
    await t.newSession('ak-paste', home, `sh -c 'cat > ${out}'`);
    await Promise.all([t.submit('ak-paste', 'first prompt'), t.submit('ak-paste', 'second prompt')]);
    const typed = await waitFor(() => {
      try {
        return readFileSync(out, 'utf8');
      } catch {
        return '';
      }
    }, (s) => s.includes('first prompt') && s.includes('second prompt'), 5000, 'both pastes');
    expect(typed).toContain('first prompt');
  });
});
