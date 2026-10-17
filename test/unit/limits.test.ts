import { execFileSync } from 'node:child_process';
import { chmodSync, closeSync, mkdirSync, mkdtempSync, openSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { afterAll, afterEach, describe, expect, it } from 'vitest';
import { statusLineScriptPath, userStatusLine, withoutMekoSuffix, writeHookFiles } from '../../src/server/claude.js';
import { getPaths } from '../../src/server/paths.js';
import { openDb } from '../../src/server/db.js';
import { PlanLimitsStore, currentLimits, parseMeasuredLimits, parseRateLimits } from '../../src/server/limits.js';

const NOW = 1_790_000_000_000;
const OURS = '/home/x/.agent-k-local/run/statusline.sh';
/** The status JSON the status line tests pipe in, written once. */
const STATUS_IN = join(mkdtempSync(join(tmpdir(), 'akl-sl-in-')), 'in.json');
writeFileSync(STATUS_IN, '{"model":{}}');
afterAll(() => rmSync(dirname(STATUS_IN), { recursive: true, force: true }));

/**
 * Runs a status line command with the status JSON on stdin, as Claude Code does. Stdin is a file,
 * not a pipe: a command that never reads it can exit before Node writes the input, which fails
 * with EPIPE now and then.
 */
function runStatusLine(command: string, env: Record<string, string> = {}): string {
  const fd = openSync(STATUS_IN, 'r');
  try {
    return execFileSync('sh', ['-c', command], { stdio: [fd, 'pipe', 'pipe'], env: { PATH: process.env.PATH!, ...env } }).toString();
  } finally {
    closeSync(fd);
  }
}

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

describe("plan limits from AgentK's mod", () => {
  it('reads both windows by kind, with ISO reset times', () => {
    const list = [
      { kind: 'five_hour', percentUsed: 2, resetsAt: '2026-09-21T15:00:00.000Z' },
      { kind: 'seven_day', percentUsed: 42, resetsAt: '2026-09-24T20:00:00.000Z' },
      { kind: 'spend_limit', percentUsed: 5 },
    ];
    expect(parseMeasuredLimits(list, NOW)).toEqual({
      fiveHour: { usedPercent: 2, resetsAt: Date.parse('2026-09-21T15:00:00.000Z') },
      sevenDay: { usedPercent: 42, resetsAt: Date.parse('2026-09-24T20:00:00.000Z') },
      updatedAt: NOW,
    });
  });

  it('ignores a window without a usable reset time, and anything that is not a list', () => {
    expect(parseMeasuredLimits([{ kind: 'five_hour', percentUsed: 2 }, { kind: 'seven_day', percentUsed: 4, resetsAt: 'soon' }], NOW)).toBeNull();
    expect(parseMeasuredLimits([], NOW)).toBeNull();
    expect(parseMeasuredLimits({ five_hour: 2 }, NOW)).toBeNull();
    expect(parseMeasuredLimits(undefined, NOW)).toBeNull();
  });

  it('merges into the same store as the status line', () => {
    const db = openDb(':memory:');
    let changes = 0;
    const store = new PlanLimitsStore(db, () => changes++);
    store.report('s1', input({ used_percentage: 10, resets_at: 1_790_003_600 }, undefined), NOW);
    store.reportMeasured('s1', [{ kind: 'five_hour', percentUsed: 12, resetsAt: new Date(1_790_003_600_000).toISOString() }], NOW + 1000);
    expect(store.current(NOW + 1000)?.fiveHour).toEqual({ usedPercent: 12, resetsAt: 1_790_003_600_000 });
    expect(changes).toBe(2);
    db.close();
  });
});

describe('PlanLimitsStore', () => {
  it('broadcasts changes, stays quiet for repeats within a minute, and survives a restart', () => {
    const db = openDb(':memory:');
    let changes = 0;
    const store = new PlanLimitsStore(db, () => changes++);
    const a = input({ used_percentage: 10, resets_at: NOW / 1000 + 3600 }, { used_percentage: 5, resets_at: NOW / 1000 + 86_400 });
    store.report('s1', a, NOW);
    store.report('s1', a, NOW + 5_000);
    expect(changes).toBe(1);
    store.report('s1', input({ used_percentage: 11, resets_at: NOW / 1000 + 3600 }, { used_percentage: 5, resets_at: NOW / 1000 + 86_400 }), NOW + 6_000);
    expect(changes).toBe(2);
    const b = input({ used_percentage: 11, resets_at: NOW / 1000 + 3600 }, { used_percentage: 5, resets_at: NOW / 1000 + 86_400 });
    store.report('s1', b, NOW + 70_000);
    expect(changes).toBe(3);
    store.report('s1', { model: {} }, NOW + 80_000);
    expect(changes).toBe(3);
    expect(new PlanLimitsStore(db, () => undefined).current(NOW + 80_000)).toMatchObject({ fiveHour: { usedPercent: 11 }, updatedAt: NOW + 70_000 });
    db.close();
  });

  it('merges reports per window, so an idle session with an old reading changes nothing', () => {
    const db = openDb(':memory:');
    let changes = 0;
    const store = new PlanLimitsStore(db, () => changes++);
    const s = NOW / 1000;
    store.report('idle', input({ used_percentage: 90, resets_at: s - 3600 }, { used_percentage: 15, resets_at: s + 86_400 }), NOW - 3_600_000);
    store.report('busy', input({ used_percentage: 40, resets_at: s + 3600 }, { used_percentage: 20, resets_at: s + 86_400 }), NOW);
    expect(changes).toBe(2);
    // Stale: the five-hour window reset an hour ago and the week reads lower. Nothing moves, even after a minute.
    store.report('idle', input({ used_percentage: 90, resets_at: s - 3600 }, { used_percentage: 15, resets_at: s + 86_400 }), NOW + 120_000);
    // Only one window: the other one is kept.
    store.report('busy', input(undefined, { used_percentage: 20, resets_at: s + 86_400 }), NOW + 240_000);
    expect(changes).toBe(2);
    expect(store.current(NOW + 240_000)).toEqual({
      fiveHour: { usedPercent: 40, resetsAt: (s + 3600) * 1000 },
      sevenDay: { usedPercent: 20, resetsAt: (s + 86_400) * 1000 },
      updatedAt: NOW,
    });
    // A new five-hour window replaces the old one, whatever its use.
    store.report('busy', input({ used_percentage: 2, resets_at: s + 7200 }, { used_percentage: 20, resets_at: s + 86_400 }), NOW + 250_000);
    expect(changes).toBe(3);
    expect(store.current(NOW + 250_000)).toMatchObject({ fiveHour: { usedPercent: 2 }, sevenDay: { usedPercent: 20 }, updatedAt: NOW + 250_000 });
    // An earlier window that's still live is an old reading, even when it's new to the session.
    store.report('busy', input({ used_percentage: 50, resets_at: s + 3600 }, null), NOW + 400_000);
    expect(changes).toBe(3);
    expect(new PlanLimitsStore(db, () => undefined).current(NOW + 400_000)).toMatchObject({ fiveHour: { usedPercent: 2 }, sevenDay: { usedPercent: 20 } });
    db.close();
  });

  it('takes a lower use in the same window once a session reads it, as after an early reset', () => {
    const db = openDb(':memory:');
    const store = new PlanLimitsStore(db, () => undefined);
    const week = (pct: number) => [{ kind: 'seven_day', percentUsed: pct, resetsAt: new Date(NOW + 86_400_000).toISOString() }];
    const weekly = (t: number) => store.current(t)?.sevenDay?.usedPercent;
    store.reportMeasured('idle', week(100), NOW);
    store.report('busy', input(undefined, { used_percentage: 99, resets_at: NOW / 1000 + 86_400 }), NOW + 1000);
    // A session's first reading only raises the number: it may be an old one.
    expect(weekly(NOW + 1000)).toBe(100);
    // The use dropped with the reset time unchanged: Anthropic reset the week early.
    store.report('busy', input(undefined, { used_percentage: 5, resets_at: NOW / 1000 + 86_400 }), NOW + 2000);
    expect(weekly(NOW + 2000)).toBe(5);
    // The idle session repeating what it read before the reset changes nothing.
    store.reportMeasured('idle', week(100), NOW + 3000);
    expect(weekly(NOW + 3000)).toBe(5);
    // After its Claude restarts, the idle session's first reading counts as a first one again.
    store.forget('idle');
    store.reportMeasured('idle', week(4), NOW + 4000);
    store.reportMeasured('idle', week(6), NOW + 5000);
    expect(weekly(NOW + 5000)).toBe(6);
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
    expect(userStatusLine(wt, OURS)).toBeNull();
    write(join(dir, 'config', 'settings.json'), { statusLine: { type: 'command', command: 'user-sl', padding: 1 } });
    expect(userStatusLine(wt, OURS)).toEqual({ type: 'command', command: 'user-sl', padding: 1 });
    write(join(wt, '.claude', 'settings.json'), { statusLine: { type: 'command', command: 'project-sl' } });
    expect(userStatusLine(wt, OURS)?.command).toBe('project-sl');
    write(join(wt, '.claude', 'settings.local.json'), { statusLine: { type: 'command', command: 'local-sl' } });
    expect(userStatusLine(wt, OURS)?.command).toBe('local-sl');
    expect(userStatusLine(null, OURS)?.command).toBe('user-sl');
  });

  it('skips broken files and never wraps its own script', () => {
    dir = mkdtempSync(join(tmpdir(), 'akl-sl-'));
    process.env.CLAUDE_CONFIG_DIR = join(dir, 'config');
    const wt = join(dir, 'wt');
    mkdirSync(join(wt, '.claude'), { recursive: true });
    writeFileSync(join(wt, '.claude', 'settings.local.json'), '{ not json');
    write(join(wt, '.claude', 'settings.json'), { statusLine: { type: 'command', command: `'${OURS}'` } });
    write(join(dir, 'config', 'settings.json'), { statusLine: { type: 'command', command: 'user-sl' } });
    expect(userStatusLine(wt, OURS)?.command).toBe('user-sl');
    // A user's own script with a similar name is theirs, not ours.
    write(join(dir, 'config', 'settings.json'), { statusLine: { type: 'command', command: "'/home/x/.claude/statusline.sh'" } });
    expect(userStatusLine(wt, OURS)?.command).toBe("'/home/x/.claude/statusline.sh'");
  });

  it("drops Meko's datapack suffix but keeps the command it wraps", () => {
    dir = mkdtempSync(join(tmpdir(), 'akl-sl-'));
    process.env.CLAUDE_CONFIG_DIR = join(dir, 'config');
    const meko = join(dir, 'meko-statusline.sh');
    writeFileSync(meko, '#!/bin/sh\ncat >/dev/null; printf "Meko: pinned"\n');
    chmodSync(meko, 0o755);
    const run = (command: string, env: Record<string, string> = {}) => {
      write(join(dir, 'config', 'settings.json'), { statusLine: { type: 'command', command, padding: 1 } });
      const sl = userStatusLine(null, OURS)!;
      expect(sl.padding).toBe(1);
      return runStatusLine(sl.command!, env);
    };
    expect(run(`'${meko}'`)).toBe('');
    expect(run(`"${meko}" --inner-cmd 'printf "inner:"; cat'`)).toBe('inner:{"model":{}}');
    expect(run(`bash ${meko}`, { MEKO_INNER_STATUSLINE: 'printf env:; cat' })).toBe('env:{"model":{}}');
    // Another script that only ends in the same name runs as it is.
    const mine = join(dir, 'my-meko-statusline.sh');
    writeFileSync(mine, '#!/bin/sh\nprintf mine\n');
    chmodSync(mine, 0o755);
    expect(run(`'${mine}'`)).toBe('mine');
    expect(withoutMekoSuffix('user-sl --meko-statusline.sh')).toBe('user-sl --meko-statusline.sh');
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
      const run = (env: Record<string, string>) => runStatusLine(statusLineScriptPath(paths), env);
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
