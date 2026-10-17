import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  readlinkSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import { getPaths, type Paths } from '../../src/server/paths.js';
import {
  compareSemver,
  parseSha256Sums,
  pickLatest,
  releaseLane,
  tarballName,
  uncountStart,
  Updater,
  verifySha256,
  hostVersionDir,
  notesRange,
  versionsToPrune,
  type Release,
  type ReleaseSource,
} from '../../src/server/updater.js';

const sha = (buf: Buffer | string) => createHash('sha256').update(buf).digest('hex');

describe('compareSemver', () => {
  it.each([
    ['1.2.3', '1.2.4', -1],
    ['1.10.0', '1.9.9', 1],
    ['0.20260924.10', '0.20260924.9', 1],
    ['v2.0.0', '2.0.0', 0],
    ['1.0.0', '1.0.0-rc.1', 1],
    ['0.20260924.0', '0.0.0-dev', 1],
    ['1.0.0-rc.2', '1.0.0-rc.10', -1],
    ['1.0.0-alpha', '1.0.0-alpha.1', -1],
    ['1.0.0-1', '1.0.0-beta', -1],
    ['garbage', '0.0.1', -1],
  ] as const)('orders %s against %s', (a, b, sign) => {
    expect(Math.sign(compareSemver(a, b))).toBe(sign);
  });
});

function rel(version: string, extra: Partial<Release> = {}): Release {
  return {
    tag: `v${version}`,
    version,
    prerelease: version.includes('-'),
    draft: false,
    url: `https://example.invalid/releases/v${version}`,
    assets: [tarballName(version, 'x64'), 'SHA256SUMS'],
    ...extra,
  };
}

describe('release lanes', () => {
  it('groups versions by their prerelease identifier', () => {
    expect(releaseLane('0.20260925.9')).toBe('stable');
    expect(releaseLane('v0.20260925.9')).toBe('stable');
    expect(releaseLane('0.20260925.9', true)).toBe('prerelease');
    expect(releaseLane('1.0.0-beta.2')).toBe('beta');
    expect(releaseLane('0.20260925.6-dp-links')).toBe('dp-links');
    expect(releaseLane('1.0.0-1')).toBe('prerelease');
  });
});

describe('removing old versions', () => {
  const names = ['current', '.staging-x', '0.1.0', '0.1.1', '0.1.2', '0.1.3', '0.1.10', '0.1.11-ctrl', '0.2.0-beta.1'];
  const prune = (running: string[], from: string | null) => versionsToPrune(names, new Set(running), from).sort();

  it.each([
    ['the running version and the one it updated from', ['0.1.10'], '0.1.3', ['0.1.0', '0.1.1', '0.1.11-ctrl', '0.1.2', '0.2.0-beta.1']],
    ['the newest older version after a manual switch', ['0.1.11-ctrl'], null, ['0.1.0', '0.1.1', '0.1.2', '0.1.3', '0.2.0-beta.1']],
    ['the newest older version when the one it came from is gone', ['0.1.10'], '0.0.9', ['0.1.0', '0.1.1', '0.1.11-ctrl', '0.1.2', '0.2.0-beta.1']],
    ['the newest other version when nothing is older', ['0.1.0'], null, ['0.1.1', '0.1.10', '0.1.11-ctrl', '0.1.2', '0.1.3']],
    ['the running version under either of its names', ['0.1.10', '0.1.11-ctrl'], null, ['0.1.0', '0.1.1', '0.1.2', '0.2.0-beta.1']],
  ] as const)('keeps %s', (_name, running, from, removed) => {
    expect(prune([...running], from)).toEqual(removed);
  });

  it('never touches non-versions', () => {
    expect(versionsToPrune(['current', '.staging-x', '0.1.0'], new Set(['0.1.0']), null)).toEqual([]);
  });
});

describe('hostVersionDir', () => {
  it('names the version a running session host started from, so pruning keeps it', () => {
    const paths = getPaths(mkdtempSync(join(tmpdir(), 'akl-hostver-')));
    expect(hostVersionDir(paths)).toBeNull();
    mkdirSync(join(paths.home, 'run'), { recursive: true });
    writeFileSync(paths.hostState, JSON.stringify({ pid: process.pid, version: '0.1.2', appDir: join(paths.home, 'app', '0.1.2') }));
    expect(hostVersionDir(paths)).toBe('0.1.2');
    // A host that is gone keeps nothing.
    writeFileSync(paths.hostState, JSON.stringify({ pid: 2 ** 22 + 12345, version: '0.1.2', appDir: join(paths.home, 'app', '0.1.2') }));
    expect(hostVersionDir(paths)).toBeNull();
    rmSync(paths.home, { recursive: true, force: true });
  });
});

describe('pickLatest', () => {
  const releases = [rel('0.2.0'), rel('0.3.0'), rel('0.4.0-rc.1'), rel('0.3.1', { draft: true })];

  it.each([
    ['the newest stable release', false, [], 'x64', '0.3.0'],
    ['prereleases on the prerelease channel', true, [], 'x64', '0.4.0-rc.1'],
    ['past bad versions', false, ['0.3.0'], 'x64', '0.2.0'],
    ['nothing without an asset for this arch', false, [], 'arm64', undefined],
  ] as const)('picks %s', (_name, includePrerelease, bad, arch, version) => {
    expect(pickLatest(releases, { includePrerelease, bad: new Set(bad), arch })?.version).toBe(version);
  });

  it('skips releases without a build for this platform', () => {
    const linuxOnly = rel('0.5.0', { assets: [tarballName('0.5.0', 'arm64', 'linux'), 'SHA256SUMS'] });
    const both = rel('0.4.0', { assets: [tarballName('0.4.0', 'arm64', 'linux'), tarballName('0.4.0', 'arm64', 'darwin'), 'SHA256SUMS'] });
    const opts = { includePrerelease: false, bad: new Set<string>(), arch: 'arm64' };
    expect(pickLatest([linuxOnly, both], { ...opts, platform: 'linux' })?.version).toBe('0.5.0');
    expect(pickLatest([linuxOnly, both], { ...opts, platform: 'darwin' })?.version).toBe('0.4.0');
    expect(pickLatest([linuxOnly], { ...opts, platform: 'darwin' })).toBeNull();
  });
});

describe('tarballName', () => {
  it('names the platform and CPU, this host\'s by default', () => {
    expect(tarballName('0.20260925.1', 'x64', 'linux')).toBe('agent-k-local-0.20260925.1-linux-x64.tar.gz');
    expect(tarballName('0.20260925.1', 'arm64', 'darwin')).toBe('agent-k-local-0.20260925.1-darwin-arm64.tar.gz');
    expect(tarballName('1.0.0')).toBe(`agent-k-local-1.0.0-${process.platform}-${process.arch}.tar.gz`);
  });
});

describe('sha256 verification', () => {
  let dir: string;
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'ak-sha-'));
  });
  afterEach(() => rmSync(dir, { recursive: true, force: true }));

  it('parses both sha256sum formats', () => {
    const m = parseSha256Sums(`${'a'.repeat(64)}  one.tar.gz\n${'B'.repeat(64)} *two.tar.gz\nnoise\n`);
    expect(m.get('one.tar.gz')).toBe('a'.repeat(64));
    expect(m.get('two.tar.gz')).toBe('b'.repeat(64));
  });

  it('accepts a matching file and rejects a tampered one', async () => {
    const f = join(dir, 'x.tar.gz');
    writeFileSync(f, 'hello');
    writeFileSync(join(dir, 'SHA256SUMS'), `${sha('hello')}  x.tar.gz\n`);
    await expect(verifySha256(f, join(dir, 'SHA256SUMS'))).resolves.toBeUndefined();
    writeFileSync(f, 'hellO');
    await expect(verifySha256(f, join(dir, 'SHA256SUMS'))).rejects.toThrow(/checksum mismatch/);
  });

  it('rejects a file missing from SHA256SUMS', async () => {
    const f = join(dir, 'y.tar.gz');
    writeFileSync(f, 'x');
    writeFileSync(join(dir, 'SHA256SUMS'), `${sha('x')}  other.tar.gz\n`);
    await expect(verifySha256(f, join(dir, 'SHA256SUMS'))).rejects.toThrow(/no entry/);
  });
});

// ---------------------------------------------------------------------------

interface Fixture {
  root: string;
  paths: Paths;
  cfg: Config;
  releaseDir: string;
  kv: Map<string, string>;
  snapshot: ReturnType<typeof vi.fn>;
  restart: ReturnType<typeof vi.fn>;
  /** A source without `compare` gets one that fails. */
  makeUpdater(source: SourceStub, appDir?: string): Updater;
}

type SourceStub = Omit<ReleaseSource, 'compare'> & Partial<ReleaseSource>;
const noCompare: ReleaseSource['compare'] = async () => {
  throw new Error('no comparison');
};

/** Build a fake release tarball for `version` whose `selftest` exits with `code`. */
function buildRelease(releaseDir: string, version: string, code = 0, corrupt = false, selftestSecs = 0): void {
  const stage = mkdtempSync(join(tmpdir(), 'ak-rel-'));
  const top = join(stage, `agent-k-local-${version}`);
  mkdirSync(join(top, 'bin'), { recursive: true });
  writeFileSync(join(top, 'bin', 'agent-k-local'), `#!/bin/sh\n[ "$1" = selftest ] && { sleep ${selftestSecs}; exit ${code}; }\nexit 0\n`);
  chmodSync(join(top, 'bin', 'agent-k-local'), 0o755);
  writeFileSync(join(top, 'VERSION'), version + '\n');
  const name = tarballName(version, 'x64');
  execFileSync('tar', ['-czf', join(releaseDir, name), '-C', stage, `agent-k-local-${version}`]);
  rmSync(stage, { recursive: true, force: true });
  const digest = corrupt ? '0'.repeat(64) : sha(readFileSync(join(releaseDir, name)));
  writeFileSync(join(releaseDir, 'SHA256SUMS'), `${digest}  ${name}\n`);
}

function fakeSource(releaseDir: string, releases: Release[]): SourceStub {
  return {
    listReleases: async () => releases,
    download: async (_repo, _tag, asset, destDir) => {
      mkdirSync(destDir, { recursive: true });
      copyFileSync(join(releaseDir, asset), join(destDir, asset));
      return join(destDir, asset);
    },
  };
}

function setup(): Fixture {
  const root = mkdtempSync(join(tmpdir(), 'ak-upd-'));
  const paths = getPaths(join(root, 'home'));
  mkdirSync(join(paths.app, '0.1.0', 'bin'), { recursive: true });
  writeFileSync(join(paths.app, '0.1.0', 'VERSION'), '0.1.0\n');
  symlinkSync('0.1.0', join(paths.app, 'current'));
  const releaseDir = join(root, 'release');
  mkdirSync(releaseDir);
  const cfg: Config = structuredClone(DEFAULT_CONFIG);
  cfg.state.dbPath = join(paths.home, 'state.db');
  const kv = new Map<string, string>();
  const snapshot = vi.fn();
  const restart = vi.fn();
  return {
    root,
    paths,
    cfg,
    releaseDir,
    kv,
    snapshot,
    restart,
    makeUpdater: (source, appDir = join(paths.app, '0.1.0')) =>
      new Updater({
        paths,
        config: () => cfg,
        version: '0.1.0',
        kvGet: (k) => kv.get(k) ?? null,
        kvSet: (k, v) => void kv.set(k, v),
        snapshotDb: snapshot,
        log: () => {},
        onChange: () => {},
        requestRestart: restart,
        appDir,
        source: { compare: noCompare, ...source },
        arch: 'x64',
      }),
  };
}

describe('Updater', () => {
  let fx: Fixture;
  const savedAppDir = process.env.AK_APP_DIR;
  beforeEach(() => {
    delete process.env.AK_APP_DIR;
    fx = setup();
  });
  afterEach(() => {
    rmSync(fx.root, { recursive: true, force: true });
    if (savedAppDir === undefined) delete process.env.AK_APP_DIR;
    else process.env.AK_APP_DIR = savedAppDir;
  });

  it('is disabled when running from a source checkout', async () => {
    const u = new Updater({
      paths: fx.paths,
      config: () => fx.cfg,
      version: '0.1.0',
      kvGet: () => null,
      kvSet: () => {},
      snapshotDb: () => {},
      log: () => {},
      onChange: () => {},
      requestRestart: () => {},
      source: { compare: noCompare, ...fakeSource(fx.releaseDir, [rel('9.9.9')]) },
    });
    const s = await u.check();
    expect(s.available).toBe(false);
    expect(s.lastError).toMatch(/source checkout/);
  });

  it('does not check when the channel is off', async () => {
    fx.cfg.update.channel = 'off';
    const list = vi.fn(async () => [rel('0.2.0')]);
    const u = fx.makeUpdater({ listReleases: list, download: async () => '' });
    const s = await u.check();
    expect(list).not.toHaveBeenCalled();
    expect(s.available).toBe(false);
  });

  it('finds an update and skips versions listed in bad-versions', async () => {
    writeFileSync(fx.paths.badVersions, '0.3.0\n');
    const u = fx.makeUpdater(fakeSource(fx.releaseDir, [rel('0.2.0'), rel('0.3.0')]));
    const s = await u.check();
    expect(s.latest).toBe('0.2.0');
    expect(s.available).toBe(true);
    expect(fx.kv.get('update.latest')).toBe('0.2.0');
  });

  it('does not offer a remembered latest version that was rolled back since', async () => {
    // 0.2.0 was found, applied, and rolled back: this boot of 0.1.0 still has it cached.
    fx.kv.set('update.latest', '0.2.0');
    fx.kv.set('update.releaseUrl', 'https://example.com/0.2.0');
    writeFileSync(fx.paths.badVersions, '0.2.0\n');
    buildRelease(fx.releaseDir, '0.2.0');
    const download = vi.fn();
    const u = fx.makeUpdater({ listReleases: async () => [rel('0.2.0')], download });
    expect(u.status()).toMatchObject({ latest: null, available: false, releaseUrl: null });
    const s = await u.apply();
    expect(s).toMatchObject({ available: false, applying: false });
    expect(download).not.toHaveBeenCalled();
    expect(fx.restart).not.toHaveBeenCalled();
  });

  it('checks again no sooner than every 24 days, however long the interval', async () => {
    vi.useFakeTimers();
    try {
      fx.cfg.update.intervalHours = 1_000_000;
      const list = vi.fn(async () => []);
      const u = fx.makeUpdater({ listReleases: list, download: async () => '' });
      u.start();
      await vi.advanceTimersByTimeAsync(60_000);
      expect(list).toHaveBeenCalledTimes(1); // the check at start, not one per millisecond
      await vi.advanceTimersByTimeAsync(2 ** 31);
      expect(list).toHaveBeenCalledTimes(2);
      u.stop();
    } finally {
      vi.useRealTimers();
    }
  });

  it.each([
    { name: 'a verified release and schedules a restart', remembered: null, listed: ['0.2.0'], to: '0.2.0' },
    // A release cut since the last check is installed now, not offered after the restart.
    { name: 'the newest release, not the one found before', remembered: '0.2.0', listed: ['0.2.0', '0.3.0'], to: '0.3.0' },
    { name: 'the version found before when the check fails', remembered: '0.2.0', listed: null, to: '0.2.0' },
  ])('applies $name', async ({ remembered, listed, to }) => {
    if (remembered) fx.kv.set('update.latest', remembered);
    buildRelease(fx.releaseDir, to);
    const source = fakeSource(fx.releaseDir, (listed ?? []).map((v) => rel(v)));
    const u = fx.makeUpdater({
      listReleases: async (repo) => {
        if (!listed) throw new Error('rate limited');
        return source.listReleases(repo);
      },
      download: source.download,
    });
    // Two clicks, one install; it shows as installing while the releases are checked.
    const first = u.apply();
    expect(u.status().applying).toBe(true);
    const [s, again] = await Promise.all([first, u.apply()]);
    expect(again).toEqual(s);
    expect(s.lastError).toBeNull();
    expect(readlinkSync(join(fx.paths.app, 'current'))).toBe(to);
    expect(existsSync(join(fx.paths.app, to, 'bin', 'agent-k-local'))).toBe(true);
    const pending = JSON.parse(readFileSync(fx.paths.updatePending, 'utf8'));
    expect(pending).toMatchObject({ from: '0.1.0', to, attempts: 0 });
    expect(fx.snapshot).toHaveBeenCalledWith(join(fx.paths.home, 'state.db.bak-0.1.0'));
    expect(fx.restart).toHaveBeenCalledOnce();
  });

  it('refuses a release whose checksum does not match', async () => {
    buildRelease(fx.releaseDir, '0.2.0', 0, true);
    const u = fx.makeUpdater(fakeSource(fx.releaseDir, [rel('0.2.0')]));
    const s = await u.apply();
    expect(s.lastError).toMatch(/checksum mismatch/);
    expect(s.applying).toBe(false);
    expect(readlinkSync(join(fx.paths.app, 'current'))).toBe('0.1.0');
    expect(existsSync(fx.paths.updatePending)).toBe(false);
    expect(fx.restart).not.toHaveBeenCalled();
  });

  it('refuses a release whose self-test fails', async () => {
    buildRelease(fx.releaseDir, '0.2.0', 1);
    const u = fx.makeUpdater(fakeSource(fx.releaseDir, [rel('0.2.0')]));
    const s = await u.apply();
    expect(s.lastError).toMatch(/self-test/);
    expect(readlinkSync(join(fx.paths.app, 'current'))).toBe('0.1.0');
    expect(fx.snapshot).not.toHaveBeenCalled();
    expect(fx.restart).not.toHaveBeenCalled();
  });

  it('reports the previous version from boot, and markHealthy then clears the pending update', () => {
    writeFileSync(fx.paths.updatePending, JSON.stringify({ from: '0.0.9', to: '0.1.0', attempts: 1, at: 1 }));
    const u = fx.makeUpdater(fakeSource(fx.releaseDir, []));
    u.start();
    u.stop();
    // The "Updated to" banner doesn't wait for the healthy check; the rollback stays armed until it.
    expect(u.status()).toMatchObject({ justUpdatedFrom: '0.0.9', lastUpdatedFrom: '0.0.9' });
    expect(fx.kv.get('update.justUpdatedFrom')).toBe('0.0.9');
    expect(existsSync(fx.paths.updatePending)).toBe(true);
    u.markHealthy();
    expect(existsSync(fx.paths.updatePending)).toBe(false);
  });

  it('shows no banner on a boot that was not an update, and keeps the last update for What\'s new', () => {
    fx.kv.set('update.justUpdatedFrom', '0.0.9');
    fx.kv.set('update.lastUpdatedFrom', '0.0.9');
    const u = fx.makeUpdater(fakeSource(fx.releaseDir, []));
    u.start();
    u.stop();
    expect(u.status()).toMatchObject({ justUpdatedFrom: null, lastUpdatedFrom: '0.0.9' });
    expect(fx.kv.get('update.justUpdatedFrom')).toBe('');
    // Rolled back to an older version since: nothing to show.
    fx.kv.set('update.lastUpdatedFrom', '0.2.0');
    expect(u.status().lastUpdatedFrom).toBeNull();
  });

  it('reads What\'s new from the commits between two tags, newest first, asking GitHub once per range', async () => {
    const compare = vi.fn(async () => ({
      totalCommits: 300,
      url: 'https://github.com/o/r/compare/v0.0.9...v0.1.0',
      commits: [
        { sha: '1111111aaa', message: 'fix: the older fix', url: 'c1' },
        { sha: '2222222bbb', message: 'docs: not a change', url: 'c2' },
        { sha: '3333333ccc', message: 'fix: the newer fix', url: 'c3' },
      ],
    }));
    compare.mockRejectedValueOnce(Object.assign(new Error('Command failed: gh api'), { stderr: 'gh: Not Found (HTTP 404)\n' }));
    const u = fx.makeUpdater({ ...fakeSource(fx.releaseDir, []), compare });
    await expect(u.notes('0.0.9', '0.1.0')).rejects.toThrow("GitHub couldn't compare v0.0.9 and v0.1.0: gh: Not Found (HTTP 404)");
    // A failed ask isn't kept: the next one asks again, and its answer is kept.
    const n = await u.notes('0.0.9', '0.1.0');
    expect(await u.notes('0.0.9', '0.1.0')).toBe(n);
    expect(compare).toHaveBeenCalledTimes(2);
    expect(compare).toHaveBeenCalledWith(fx.cfg.update.repo, 'v0.0.9', 'v0.1.0');
    expect(n).toMatchObject({ from: '0.0.9', to: '0.1.0', commits: 3, totalCommits: 300, compareUrl: 'https://github.com/o/r/compare/v0.0.9...v0.1.0' });
    expect(n.sections).toEqual([{ title: 'Fixes', items: [expect.objectContaining({ summary: 'the newer fix' }), expect.objectContaining({ summary: 'the older fix' })] }]);
  });

  it.each([
    ['0.0.9', '0.1.0', true],
    ['0.1.0-beta.1', '0.1.0', true],
    ['0.1.0', '0.1.0', false],
    ['0.2.0', '0.1.0', false],
    ['main', '0.1.0', false],
    ['0.0.9', '0.1.0/../x', false],
    ['v0.0.9', '0.1.0', false],
    [' 0.0.9', '0.1.0', false],
  ])('What\'s new from %s to %s is a range: %s', (from, to, ok) => {
    expect(notesRange(from, to)).toBe(ok);
  });

  it('markHealthy leaves an update alone while it installs and after the switch', async () => {
    for (const v of ['0.0.1', '0.0.9']) mkdirSync(join(fx.paths.app, v));
    writeFileSync(fx.paths.updatePending, JSON.stringify({ from: '0.0.9', to: '0.1.0', attempts: 1, at: 1 }));
    buildRelease(fx.releaseDir, '0.2.0', 0, false, 1);
    const u = fx.makeUpdater(fakeSource(fx.releaseDir, [rel('0.2.0')]));
    const applying = u.apply();
    // The healthy timer fires during the new version's self-test.
    for (let i = 0; i < 100 && !existsSync(join(fx.paths.app, '0.2.0')); i++) await new Promise((r) => setTimeout(r, 20));
    u.markHealthy();
    await applying;
    expect(existsSync(join(fx.paths.app, '0.2.0', 'bin', 'agent-k-local'))).toBe(true);
    expect(readlinkSync(join(fx.paths.app, 'current'))).toBe('0.2.0');
    // Or it fires in the old version while it shuts down for the restart.
    u.markHealthy();
    expect(JSON.parse(readFileSync(fx.paths.updatePending, 'utf8'))).toMatchObject({ from: '0.1.0', to: '0.2.0' });
    expect(readdirSync(fx.paths.app).sort()).toEqual(['0.0.1', '0.0.9', '0.1.0', '0.2.0', 'current']);
  });

  it('markHealthy that came during an update runs when that update fails', async () => {
    for (const v of ['0.0.1', '0.0.9']) mkdirSync(join(fx.paths.app, v));
    writeFileSync(fx.paths.updatePending, JSON.stringify({ from: '0.0.9', to: '0.1.0', attempts: 1, at: 1 }));
    buildRelease(fx.releaseDir, '0.2.0', 1, false, 1);
    const u = fx.makeUpdater(fakeSource(fx.releaseDir, [rel('0.2.0')]));
    const applying = u.apply();
    for (let i = 0; i < 100 && !existsSync(join(fx.paths.app, '0.2.0')); i++) await new Promise((r) => setTimeout(r, 20));
    u.markHealthy();
    expect(existsSync(fx.paths.updatePending)).toBe(true);
    const s = await applying;
    expect(s.lastError).toMatch(/self-test/);
    // This version's own update is final now: its pending file is gone and 0.0.9 is kept to go back to.
    expect(existsSync(fx.paths.updatePending)).toBe(false);
    expect(readdirSync(fx.paths.app).sort()).toEqual(['0.0.9', '0.1.0', 'current']);
  });

  it('markHealthy drops a stale pending file and never deletes what current points at', () => {
    for (const v of ['0.0.3', '0.0.7', '0.0.9']) mkdirSync(join(fx.paths.app, v));
    writeFileSync(fx.paths.updatePending, JSON.stringify({ from: '0.0.3', to: '0.0.7', attempts: 2, at: 1 }));
    // Someone pointed current at 0.0.3 by hand without restarting.
    rmSync(join(fx.paths.app, 'current'));
    symlinkSync('0.0.3', join(fx.paths.app, 'current'));
    fx.makeUpdater(fakeSource(fx.releaseDir, [])).markHealthy();
    expect(existsSync(fx.paths.updatePending)).toBe(false);
    // Running 0.1.0 and current's 0.0.3 stay, plus the newest other older one, 0.0.9.
    expect(readdirSync(fx.paths.app).sort()).toEqual(['0.0.3', '0.0.9', '0.1.0', 'current']);
  });

  it('markHealthy removes the database snapshots of removed versions', () => {
    for (const v of ['0.0.1', '0.0.3']) mkdirSync(join(fx.paths.app, v));
    for (const f of ['state.db', 'state.db.bak-0.0.1', 'state.db.bak-0.0.3', 'state.db.bak-0.0.2', 'state.db.bak-mine', 'notes.txt']) {
      writeFileSync(join(fx.paths.home, f), '');
    }
    writeFileSync(fx.paths.updatePending, JSON.stringify({ from: '0.0.3', to: '0.1.0', attempts: 1, at: 1 }));
    fx.makeUpdater(fakeSource(fx.releaseDir, [])).markHealthy();
    const left = readdirSync(fx.paths.home).filter((f) => f.startsWith('state.db') || f === 'notes.txt').sort();
    // 0.0.3 is the kept previous version; 0.0.1 was removed, 0.0.2 wasn't installed; other files stay.
    expect(left).toEqual(['notes.txt', 'state.db', 'state.db.bak-0.0.3', 'state.db.bak-mine']);
  });

  it('markHealthy keeps only the running version and the one it updated from', () => {
    for (const v of ['0.0.1', '0.0.2', '0.0.3', '0.0.7', '0.0.1-beta.1', '0.1.1-ctrl']) mkdirSync(join(fx.paths.app, v));
    writeFileSync(fx.paths.updatePending, JSON.stringify({ from: '0.0.3', to: '0.1.0', attempts: 1, at: Date.now() }));
    fx.makeUpdater(fakeSource(fx.releaseDir, [])).markHealthy();
    expect(readdirSync(fx.paths.app).sort()).toEqual(['0.0.3', '0.1.0', 'current']);
  });

  it('a clean stop of the version being tried out takes its start back off the count', () => {
    const pending = (attempts: number, to: string) =>
      writeFileSync(fx.paths.updatePending, JSON.stringify({ from: '0.0.3', to, attempts, at: 1 }));
    const attempts = () => JSON.parse(readFileSync(fx.paths.updatePending, 'utf8')).attempts;
    const u = fx.makeUpdater(fakeSource(fx.releaseDir, []));
    pending(2, '0.1.0');
    u.stoppedCleanly();
    expect(attempts()).toBe(1);
    pending(0, '0.1.0');
    u.stoppedCleanly();
    expect(attempts()).toBe(0); // never below zero
    // The old version stopping so the new one can start changes nothing.
    pending(0, '0.2.0');
    u.stoppedCleanly();
    expect(attempts()).toBe(0);
    rmSync(fx.paths.updatePending);
    expect(() => u.stoppedCleanly()).not.toThrow(); // no update pending
  });

  it('a start refused because another server holds the home is not counted either', () => {
    // serve calls this before the Updater exists (it hasn't opened the database).
    writeFileSync(fx.paths.updatePending, JSON.stringify({ from: '0.0.3', to: '0.1.0', attempts: 2, at: 1 }));
    uncountStart(fx.paths, '0.1.0', undefined);
    expect(JSON.parse(readFileSync(fx.paths.updatePending, 'utf8')).attempts).toBe(1);
    uncountStart(fx.paths, '0.0.3', undefined);
    expect(JSON.parse(readFileSync(fx.paths.updatePending, 'utf8')).attempts).toBe(1);
  });
});

// ---------------------------------------------------------------------------

describe('bin wrapper rollback', () => {
  const wrapper = resolve(__dirname, '../../packaging/agent-k-local');
  let root: string;

  function fakeVersion(home: string, version: string, message: string): void {
    const dir = join(home, 'app', version);
    mkdirSync(join(dir, 'bin'), { recursive: true });
    mkdirSync(join(dir, 'node', 'bin'), { recursive: true });
    mkdirSync(join(dir, 'lib', 'dist', 'server'), { recursive: true });
    copyFileSync(wrapper, join(dir, 'bin', 'agent-k-local'));
    chmodSync(join(dir, 'bin', 'agent-k-local'), 0o755);
    symlinkSync(process.execPath, join(dir, 'node', 'bin', 'node'));
    writeFileSync(join(dir, 'VERSION'), version + '\n');
    writeFileSync(join(dir, 'lib', 'dist', 'server', 'cli.js'), `console.log(${JSON.stringify(message)}, process.env.AK_VERSION)\n`);
  }

  beforeEach(() => {
    root = mkdtempSync(join(tmpdir(), 'ak-wrap-'));
    fakeVersion(root, '0.1.0', 'old');
    fakeVersion(root, '0.2.0', 'new');
    symlinkSync('0.2.0', join(root, 'app', 'current'));
  });
  afterEach(() => rmSync(root, { recursive: true, force: true }));

  const runServe = () =>
    execFileSync(join(root, 'app', 'current', 'bin', 'agent-k-local'), ['serve'], {
      encoding: 'utf8',
      env: { ...process.env, AK_HOME: root },
      stdio: ['ignore', 'pipe', 'pipe'],
    }).trim();

  it('runs the current version and counts its start while an update is pending, even after two failed starts', () => {
    writeFileSync(join(root, 'update-pending.json'), JSON.stringify({ from: '0.1.0', to: '0.2.0', attempts: 2 }));
    expect(runServe()).toBe('new 0.2.0');
    expect(JSON.parse(readFileSync(join(root, 'update-pending.json'), 'utf8')).attempts).toBe(3);
    expect(existsSync(join(root, 'bad-versions'))).toBe(false);
  });

  it('rolls back after the third failed start', () => {
    writeFileSync(join(root, 'update-pending.json'), JSON.stringify({ from: '0.1.0', to: '0.2.0', attempts: 3 }));
    expect(runServe()).toBe('old 0.1.0');
    expect(readlinkSync(join(root, 'app', 'current'))).toBe('0.1.0');
    expect(readFileSync(join(root, 'bad-versions'), 'utf8').trim()).toBe('0.2.0');
    expect(existsSync(join(root, 'update-pending.json'))).toBe(false);
  });

  it('ignores the pending file for commands other than serve', () => {
    writeFileSync(join(root, 'update-pending.json'), JSON.stringify({ from: '0.1.0', to: '0.2.0', attempts: 2 }));
    const out = execFileSync(join(root, 'app', 'current', 'bin', 'agent-k-local'), ['version'], {
      encoding: 'utf8',
      env: { ...process.env, AK_HOME: root },
    }).trim();
    expect(out).toBe('new 0.2.0');
    expect(JSON.parse(readFileSync(join(root, 'update-pending.json'), 'utf8')).attempts).toBe(2);
  });
});
