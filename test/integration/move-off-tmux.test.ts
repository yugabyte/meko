import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { moveOffTmux, retireTmuxService } from '../../src/server/legacytmux.js';
import { silentLogger } from '../../src/server/log.js';
import { run } from '../../src/server/db.js';
import type { ServiceManager } from '../../src/server/service.js';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';

// Remove with src/server/legacytmux.ts after 2026-10-14.

const hasTmux = (() => {
  try {
    execFileSync('tmux', ['-V'], { stdio: 'ignore' });
    return true;
  } catch {
    return false;
  }
})();

describe.skipIf(!hasTmux)('moving off tmux (real tmux, session host + git, fake claude)', () => {
  let env: TestEnv;
  const tmux = (...a: string[]) => execFileSync('tmux', ['-L', env.socket, ...a], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
  const hasSession = (name: string) => {
    try {
      tmux('has-session', '-t', `=${name}`);
      return true;
    } catch {
      return false;
    }
  };
  const argvLog = () => {
    const f = join(env.fakeDir, 'argv.jsonl');
    return existsSync(f) ? readFileSync(f, 'utf8').trim().split('\n').map((l) => JSON.parse(l) as { args: string[] }) : [];
  };
  afterEach(async () => {
    try {
      tmux('kill-server');
    } catch {
      /* not running */
    }
    await env.cleanup();
  });

  it("ends this home's sessions still in tmux at start and resumes them in the session host, a cut-off turn auto-continued", async () => {
    env = await makeEnv();
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Old one', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');

    // As a version before the session host left it: Claude in tmux, mid-turn, the host not running.
    await env.core.stop();
    await env.killHost();
    tmux('new-session', '-d', '-s', s.paneName, 'sleep 300');
    tmux('new-session', '-d', '-s', 'ak-elsewhere--theirs', 'sleep 300'); // another home's session
    run(env.core.db, "UPDATE sessions SET status = 'working' WHERE id = ?", s.id);
    const launches = argvLog().length;

    await env.core.start();
    expect(hasSession(s.paneName)).toBe(false);
    expect(hasSession('ak-elsewhere--theirs')).toBe(true);
    await waitFor(() => env.core.host.panes(), (panes) => panes.get(s.paneName)?.dead === false, 10000, 'resumed in the host');
    await waitFor(() => argvLog().length, (n) => n > launches, 10000, 'relaunch');
    const relaunch = argvLog().at(-1)!.args;
    expect(relaunch).toEqual(expect.arrayContaining(['--resume', s.claudeSessionId]));
    expect(relaunch.at(-1)).toMatch(/host restarted/);
  }, 30_000);

  it('ends a tmux server left with no sessions when no tmux service owns it', async () => {
    env = await makeEnv({ start: false });
    mkdirSync(env.core.paths.home, { recursive: true });
    writeFileSync(env.core.paths.tmuxConf, '# old\n');
    tmux('new-session', '-d', '-s', 'ak-demo--mine', 'sleep 300');
    const moved = await moveOffTmux({ paths: env.core.paths, log: silentLogger, services: null, ours: new Set(['ak-demo--mine']), socket: env.socket });
    expect(moved).toEqual(['ak-demo--mine']);
    expect(() => tmux('list-sessions')).toThrow();
    expect(existsSync(env.core.paths.tmuxConf)).toBe(false);
  });

  it('removes the tmux service only once the update is final', async () => {
    env = await makeEnv({ start: false });
    let removed = 0;
    const svc = { legacyTmuxInstalled: () => removed === 0, removeLegacyTmux: () => void removed++ } as unknown as ServiceManager;
    const paths = env.core.paths;
    mkdirSync(paths.home, { recursive: true });
    writeFileSync(paths.updatePending, '{}');
    expect(retireTmuxService({ paths, log: silentLogger, services: svc })).toBe(false);
    expect(removed).toBe(0);
    rmSync(paths.updatePending);
    expect(retireTmuxService({ paths, log: silentLogger, services: svc })).toBe(true);
    expect(removed).toBe(1);
    expect(retireTmuxService({ paths, log: silentLogger, services: svc })).toBe(true);
    expect(removed).toBe(1);
  });
});
