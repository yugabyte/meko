import { lstatSync, mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { DEFAULT_CONFIG } from '../../src/server/config.js';
import { checkMove, HomeMove, refuseOtherMeko, rememberMekoHome, type MoveCheck } from '../../src/server/movehome.js';
import { loadConfig } from '../../src/server/config.js';
import { removeOldHomeLink } from '../../src/server/install.js';
import { akHome, getPaths, homeIn, mekoHome, MekoHomeError, oldHome } from '../../src/server/paths.js';
import { MOVE_JOB, startMoveJob, type ServiceManager } from '../../src/server/service.js';
import { tempDir } from '../helpers.js';

/** Where Meko's data folder is: the cases Meko's own paths.Resolve is tested against too. */
const CASES = (JSON.parse(readFileSync(join(__dirname, '../../../../backend/internal/paths/testdata/resolve-cases.json'), 'utf8')) as {
  cases: { name: string; env: Record<string, string>; networkDirs: string[]; want?: string; error?: boolean }[];
}).cases;

let root: string;
beforeEach(() => {
  root = tempDir('ak-home-');
});
afterEach(() => rmSync(root, { recursive: true, force: true }));

/**
 * A case's paths in this test's folder: $HOME, $SHARED_TMP and $USER filled in, and any other
 * absolute path under the folder, with the network folders made (the check looks at the nearest
 * folder that exists).
 */
function setUp(c: (typeof CASES)[number]) {
  const home = join(root, 'home');
  const sharedTmp = join(root, 'var-tmp');
  const fill = (p: string) => {
    const s = p.replace('$HOME', home).replace('$SHARED_TMP', sharedTmp).replace('$USER', 'me');
    return s.startsWith(root) || !s.startsWith('/') ? s : join(root, s);
  };
  const network = c.networkDirs.map(fill);
  for (const d of [home, sharedTmp, ...network]) mkdirSync(d, { recursive: true });
  const type = (dir: string) => (network.some((n) => dir === n || dir.startsWith(n + '/')) ? 'nfs' : 'ext2/ext3');
  const env = Object.fromEntries(Object.entries(c.env).map(([k, v]) => [k, fill(v)]));
  return { opts: { env, home, user: 'me', sharedTmp, type }, want: c.want && fill(c.want) };
}

describe("Meko's data folder", () => {
  it.each(CASES)('$name', (c) => {
    const { opts, want } = setUp(c);
    if (c.error) expect(() => mekoHome(opts)).toThrow(MekoHomeError);
    else expect(mekoHome(opts)).toBe(want);
  });

  it('takes the folder Meko last named when MEKO_HOME is unset', () => {
    expect(mekoHome({ env: {}, configured: join(root, 'meko'), home: root, type: () => 'ext4' })).toBe(join(root, 'meko'));
    expect(mekoHome({ env: { MEKO_HOME: join(root, 'other') }, configured: join(root, 'meko'), home: root, type: () => 'ext4' })).toBe(join(root, 'other'));
  });
});

describe("AgentK's home", () => {
  const keys = ['AK_APP_DIR', 'AK_HOME', 'MEKO_HOME', 'HOME'] as const;
  const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
  afterEach(() => {
    for (const k of keys) {
      if (saved[k] === undefined) delete process.env[k];
      else process.env[k] = saved[k];
    }
  });

  it("is a release's own folder whatever AK_HOME says, else AK_HOME (dev, tests)", () => {
    process.env.AK_APP_DIR = join(root, 'h', 'app', '0.2.0');
    process.env.AK_HOME = join(root, 'elsewhere');
    expect(akHome()).toBe(join(root, 'h'));
    delete process.env.AK_APP_DIR;
    expect(akHome()).toBe(join(root, 'elsewhere'));
  });

  it.each([
    ['neither', [], 'new'],
    ['only the old one', ['old'], 'old'],
    ['only the new one', ['new'], 'new'],
    ['both', ['old', 'new'], 'new'],
  ])('without either set, is the one in Meko\'s data folder unless only the old one is there (%s)', (_what, there, want) => {
    delete process.env.AK_APP_DIR;
    delete process.env.AK_HOME;
    delete process.env.MEKO_HOME;
    process.env.HOME = root;
    const homes = { old: oldHome(root), new: homeIn(join(root, '.meko')) };
    for (const h of there as ('old' | 'new')[]) mkdirSync(homes[h], { recursive: true });
    expect(akHome()).toBe(homes[want as 'old' | 'new']);
  });
});

describe('whether the home moves', () => {
  const app = (home: string) => join(home, 'app', '0.2.0');
  const local = { type: () => 'ext4', env: {} as NodeJS.ProcessEnv };

  function release(home: string) {
    mkdirSync(app(home), { recursive: true });
    return getPaths(home);
  }

  const check = (paths: ReturnType<typeof getPaths>, o: Parameters<typeof checkMove>[2] = {}) =>
    checkMove(paths, structuredClone(DEFAULT_CONFIG), { appDir: app(paths.home), gitVersion: () => 'git version 2.43.7', meko: { ...local, home: root }, mekoHelp: () => null, ...o });

  it('moves a release install into Meko\'s data folder: renamed on one filesystem, copied onto another when there\'s room', () => {
    const paths = release(oldHome(root));
    const to = homeIn(join(root, '.meko'));
    expect(check(paths)).toEqual({ plan: { from: paths.home, to, method: 'rename' } });
    let dev = 0;
    const another = { device: () => ++dev, size: () => 100 * 2 ** 20 };
    expect(check(paths, { ...another, free: () => 200 * 2 ** 20 })).toEqual({ plan: { from: paths.home, to, method: 'copy' } });
    dev = 0;
    expect(check(paths, { ...another, free: () => 50 * 2 ** 20 })).toEqual({ skip: expect.stringMatching(/needs 110 MB there, and 50 MB is free/) });
  });

  it.each<[string, (paths: ReturnType<typeof getPaths>) => Parameters<typeof checkMove>[2], MoveCheck | RegExp]>([
    ['a source checkout never moves', () => ({ appDir: undefined }), null],
    ['a target with something in it', () => {
      mkdirSync(homeIn(join(root, '.meko')), { recursive: true });
      writeFileSync(join(homeIn(join(root, '.meko')), 'x'), '');
      return {};
    }, /isn't empty/],
    ['git older than 2.30', () => ({ gitVersion: () => 'git version 2.27.0' }), /needs git 2\.30 or later/],
    ['a Meko whose destroy would delete the moved home with its own', () => ({ mekoHelp: () => 'Usage of meko destroy:\n  -force' }), /older than one whose `meko destroy` keeps AgentK's data/],
    ['a Meko whose destroy keeps it', () => ({ mekoHelp: () => '  -force\n  -with-extensions' }), { plan: expect.objectContaining({ method: 'rename' }) }],
    ['MEKO_HOME on a network filesystem', () => ({ meko: { home: root, env: { MEKO_HOME: join(root, 'net') }, type: () => 'nfs' } }), /network filesystem/],
  ])('%s', (_what, setup, want) => {
    const paths = release(oldHome(root));
    const r = check(paths, setup(paths));
    if (want instanceof RegExp) expect(r).toEqual({ skip: expect.stringMatching(want) });
    else expect(r).toEqual(want);
  });

  it("an AK_HOME an install named moves too; a home in Meko's data folder, or linked to it, doesn't", () => {
    expect(check(release(homeIn(join(root, '.meko'))))).toBeNull();
    rmSync(join(root, '.meko'), { recursive: true });
    const custom = release(join(root, 'custom'));
    expect(check(custom)).toMatchObject({ plan: { from: custom.home, method: 'rename' } });
    const to = homeIn(join(root, '.meko'));
    mkdirSync(dirname(to), { recursive: true });
    rmSync(custom.home, { recursive: true });
    mkdirSync(to);
    symlinkSync(to, custom.home);
    expect(check(getPaths(custom.home))).toBeNull();
  });
});

describe('the move trigger', () => {
  function trigger(r: MoveCheck, { services = true } = {}) {
    const paths = getPaths(join(root, 'h'));
    mkdirSync(paths.home, { recursive: true });
    const warnings: string[] = [];
    const starts: unknown[] = [];
    const probation = { on: false };
    const svc = { kind: 'systemd', installed: () => services } as unknown as ServiceManager;
    const move = new HomeMove(paths, { info: () => undefined, warn: (m) => warnings.push(m) }, {
      services: () => svc,
      check: () => r,
      start: (_kind, job) => void starts.push(job),
      onProbation: () => probation.on,
    });
    return { paths, move, warnings, starts, probation };
  }

  it('starts the move job once, and never while an update is still being tried out', () => {
    const t = trigger({ plan: { from: join(root, 'h'), to: join(root, 'new'), method: 'rename' } });
    t.probation.on = true;
    t.move.check();
    expect(t.starts).toEqual([]);
    t.probation.on = false;
    t.move.check();
    t.move.check();
    expect(t.starts).toEqual([{ bin: expect.stringMatching(/bin\/agentk$/), from: join(root, 'h'), to: join(root, 'new') }]);
  });

  it("says once why a home can't move, and leaves a home without services to serve", () => {
    const t = trigger({ skip: 'git is too old' });
    t.move.check();
    t.move.check();
    expect(t.warnings).toEqual([expect.stringContaining('git is too old')]);
    const none = trigger({ plan: { from: 'a', to: 'b', method: 'rename' } }, { services: false });
    none.move.check();
    expect(none.starts).toEqual([]);
  });
});

describe('the move job', () => {
  const job = () => ({ bin: '/h/app/0.2.0/bin/agentk', from: '/h', to: '/meko/extensions/agentk' });

  it('runs as a transient systemd unit that restarts after a failure, unless it runs already', () => {
    const calls: string[][] = [];
    let active = false;
    const run = (cmd: string, args: string[]) => {
      calls.push([cmd, ...args]);
      return { ok: cmd === 'systemctl' && args[1] === 'is-active' ? active : true, out: '', err: '' };
    };
    startMoveJob('systemd', job(), run);
    const started = calls.find((c) => c[0] === 'systemd-run')!;
    expect(started).toEqual(expect.arrayContaining(['--unit', MOVE_JOB.systemd, 'Restart=on-failure', 'AK_HOME=/h', '/h/app/0.2.0/bin/agentk', '_move-home', '--to', '/meko/extensions/agentk']));
    active = true;
    calls.length = 0;
    startMoveJob('systemd', job(), run);
    expect(calls.some((c) => c[0] === 'systemd-run')).toBe(false);
  });

  it('runs as a LaunchAgent of its own on macOS', () => {
    const calls: string[][] = [];
    const run = (cmd: string, args: string[]) => {
      calls.push([cmd, ...args]);
      return { ok: args[0] !== 'print', out: '', err: '' };
    };
    startMoveJob('launchd', job(), run, root, 501);
    const plist = join(root, 'Library', 'LaunchAgents', `${MOVE_JOB.launchd}.plist`);
    const text = readFileSync(plist, 'utf8');
    expect(text).toContain('<string>_move-home</string><string>--to</string><string>/meko/extensions/agentk</string>');
    expect(text).toContain('<key>SuccessfulExit</key><false/>');
    expect(calls).toContainEqual(['launchctl', 'bootstrap', 'gui/501', plist]);
  });
});

describe('what Meko tells AgentK', () => {
  it('keeps the MEKO_HOME Meko runs it with as [meko] home, once there is a config', () => {
    const paths = getPaths(join(root, 'h'));
    rememberMekoHome(paths, { MEKO_HOME: join(root, 'meko') });
    expect(loadConfig(paths).meko.home).toBe('');
    mkdirSync(paths.home);
    writeFileSync(paths.config, '');
    rememberMekoHome(paths, { MEKO_HOME: join(root, 'meko') });
    expect(loadConfig(paths).meko.home).toBe(join(root, 'meko'));
  });

  // A release's home stays where it's installed, so a Meko for another folder must not stop it.
  it.each([
    ['no MEKO_HOME', 'meko/extensions/agentk', undefined, true],
    ["the home's own Meko", 'meko/extensions/agentk', 'meko', true],
    ["a link to the home's Meko", 'meko/extensions/agentk', 'link', true],
    ['another Meko', 'meko/extensions/agentk', 'other', false],
    ["a home that isn't in a Meko folder yet", 'h', 'other', true],
  ])('refuses a command Meko runs only for another Meko: %s', (_what, home, mekoHome, allowed) => {
    mkdirSync(join(root, 'meko'));
    symlinkSync(join(root, 'meko'), join(root, 'link'));
    const run = () => refuseOtherMeko(getPaths(join(root, home)), { MEKO_HOME: mekoHome && join(root, mekoHome) });
    if (allowed) expect(run).not.toThrow();
    else expect(run).toThrow(`This AgentK belongs to the Meko in ${join(root, 'meko')}, and MEKO_HOME names ${join(root, 'other')}: nothing was done`);
  });
});

describe('uninstall --purge after the move', () => {
  it("removes ~/.agent-k-local only when it's the link the move left to this home", () => {
    const old = oldHome(root);
    symlinkSync(join(root, 'elsewhere'), old);
    removeOldHomeLink(join(root, 'h'), old);
    expect(there(old)).toBe(true);
    rmSync(old);
    symlinkSync(join(root, 'h'), old);
    removeOldHomeLink(join(root, 'h'), old);
    expect(there(old)).toBe(false);
  });
});

/** Whether a path is there, a dangling link included. */
function there(p: string): boolean {
  try {
    lstatSync(p);
    return true;
  } catch {
    return false;
  }
}
