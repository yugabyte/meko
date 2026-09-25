import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { statusLineScriptPath, userStatusLine, writeHookFiles } from '../../src/server/claude.js';
import { getPaths } from '../../src/server/paths.js';
import { openDb } from '../../src/server/db.js';
import { PlanLimitsStore, currentLimits, parseRateLimits } from '../../src/server/limits.js';

const NOW = 1_790_000_000_000;
const input = (five: unknown, week: unknown) => ({ model: { id: 'x' }, rate_limits: { five_hour: five, seven_day: week } });

describe('plan limits from the status line', () => {
  it('reads both windows, with reset times in seconds', () => {
    expect(parseRateLimits(input({ used_percentage: 42.5, resets_at: 1_790_003_600 }, { used_percentage: 18, resets_at: 1_790_400_000 }), NOW)).toEqual({
      fiveHour: { usedPercent: 42.5, resetsAt: 1_790_003_600_000 },
      sevenDay: { usedPercent: 18, resetsAt: 1_790_400_000_000 },
      updatedAt: NOW,
    });
  });

  it('keeps a window on its own, and clamps odd percentages', () => {
    const l = parseRateLimits(input(undefined, { used_percentage: 130, resets_at: 1_790_400_000 }), NOW);
    expect(l).toMatchObject({ fiveHour: null, sevenDay: { usedPercent: 100 } });
  });

  it('ignores inputs without usable limits', () => {
    expect(parseRateLimits({ model: {} }, NOW)).toBeNull();
    expect(parseRateLimits(null, NOW)).toBeNull();
    expect(parseRateLimits(input({ used_percentage: '5', resets_at: 1 }, { resets_at: 1_790_400_000 }), NOW)).toBeNull();
  });

  it('drops windows once they reset', () => {
    const l = parseRateLimits(input({ used_percentage: 90, resets_at: NOW / 1000 + 60 }, { used_percentage: 20, resets_at: NOW / 1000 + 86_400 }), NOW);
    expect(currentLimits(l, NOW + 120_000)).toMatchObject({ fiveHour: null, sevenDay: { usedPercent: 20 } });
    expect(currentLimits(l, NOW + 2 * 86_400_000)).toBeNull();
  });
});

describe('PlanLimitsStore', () => {
  it('broadcasts changes, stays quiet for repeats within a minute, and survives a restart', () => {
    const db = openDb(':memory:');
    let changes = 0;
    const store = new PlanLimitsStore(db, () => changes++);
    const a = input({ used_percentage: 10, resets_at: NOW / 1000 + 3600 }, { used_percentage: 5, resets_at: NOW / 1000 + 86_400 });
    store.report(a, NOW);
    store.report(a, NOW + 5_000);
    expect(changes).toBe(1);
    store.report(input({ used_percentage: 11, resets_at: NOW / 1000 + 3600 }, { used_percentage: 5, resets_at: NOW / 1000 + 86_400 }), NOW + 6_000);
    expect(changes).toBe(2);
    store.report(a, NOW + 70_000);
    expect(changes).toBe(3);
    store.report({ model: {} }, NOW + 80_000);
    expect(changes).toBe(3);
    expect(new PlanLimitsStore(db, () => undefined).current(NOW + 80_000)).toMatchObject({ fiveHour: { usedPercent: 10 }, updatedAt: NOW + 70_000 });
    db.close();
  });
});

describe("the user's own status line", () => {
  let dir: string;
  const saved = process.env.CLAUDE_CONFIG_DIR;
  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
    if (saved === undefined) delete process.env.CLAUDE_CONFIG_DIR;
    else process.env.CLAUDE_CONFIG_DIR = saved;
  });
  const write = (file: string, v: unknown) => {
    mkdirSync(join(file, '..'), { recursive: true });
    writeFileSync(file, JSON.stringify(v));
  };

  it('prefers local project, then project, then user settings', () => {
    dir = mkdtempSync(join(tmpdir(), 'akl-sl-'));
    process.env.CLAUDE_CONFIG_DIR = join(dir, 'config');
    const wt = join(dir, 'wt');
    expect(userStatusLine(wt)).toBeNull();
    write(join(dir, 'config', 'settings.json'), { statusLine: { type: 'command', command: 'user-sl', padding: 1 } });
    expect(userStatusLine(wt)).toEqual({ type: 'command', command: 'user-sl', padding: 1 });
    write(join(wt, '.claude', 'settings.json'), { statusLine: { type: 'command', command: 'project-sl' } });
    expect(userStatusLine(wt)?.command).toBe('project-sl');
    write(join(wt, '.claude', 'settings.local.json'), { statusLine: { type: 'command', command: 'local-sl' } });
    expect(userStatusLine(wt)?.command).toBe('local-sl');
    expect(userStatusLine(null)?.command).toBe('user-sl');
  });

  it('skips broken files and never wraps its own script', () => {
    dir = mkdtempSync(join(tmpdir(), 'akl-sl-'));
    process.env.CLAUDE_CONFIG_DIR = join(dir, 'config');
    const wt = join(dir, 'wt');
    mkdirSync(join(wt, '.claude'), { recursive: true });
    writeFileSync(join(wt, '.claude', 'settings.local.json'), '{ not json');
    write(join(wt, '.claude', 'settings.json'), { statusLine: { type: 'command', command: '/home/x/.agent-k-local/run/statusline.sh' } });
    write(join(dir, 'config', 'settings.json'), { statusLine: { type: 'command', command: 'user-sl' } });
    expect(userStatusLine(wt)?.command).toBe('user-sl');
  });
});

describe('the status line script', () => {
  it("prints the session's status line, or the saved user one for older sessions", () => {
    const dir = mkdtempSync(join(tmpdir(), 'akl-sls-'));
    const saved = process.env.CLAUDE_CONFIG_DIR;
    try {
      process.env.CLAUDE_CONFIG_DIR = join(dir, 'config');
      mkdirSync(process.env.CLAUDE_CONFIG_DIR, { recursive: true });
      writeFileSync(join(process.env.CLAUDE_CONFIG_DIR, 'settings.json'), JSON.stringify({ statusLine: { type: 'command', command: 'printf user:; cat' } }));
      const paths = getPaths(join(dir, 'ak'));
      writeHookFiles(paths);
      expect(readFileSync(join(paths.run, 'user-statusline'), 'utf8')).toBe('printf user:; cat');
      const run = (env: Record<string, string>) =>
        execFileSync('sh', ['-c', statusLineScriptPath(paths)], { input: '{"model":{}}', env: { PATH: process.env.PATH!, ...env } }).toString();
      expect(run({ AK_USER_STATUSLINE: 'printf session:; cat' })).toBe('session:{"model":{}}');
      expect(run({})).toBe('user:{"model":{}}');
      expect(run({ AK_USER_STATUSLINE: '' })).toBe('');
    } finally {
      if (saved === undefined) delete process.env.CLAUDE_CONFIG_DIR;
      else process.env.CLAUDE_CONFIG_DIR = saved;
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
