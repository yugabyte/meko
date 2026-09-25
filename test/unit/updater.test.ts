import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
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
  tarballName,
  Updater,
  verifySha256,
  type Release,
  type ReleaseSource,
} from '../../src/server/updater.js';

const sha = (buf: Buffer | string) => createHash('sha256').update(buf).digest('hex');

describe('compareSemver', () => {
  it('orders versions', () => {
    expect(compareSemver('1.2.3', '1.2.4')).toBeLessThan(0);
    expect(compareSemver('1.10.0', '1.9.9')).toBeGreaterThan(0);
    expect(compareSemver('v2.0.0', '2.0.0')).toBe(0);
    expect(compareSemver('1.0.0', '1.0.0-rc.1')).toBeGreaterThan(0);
    expect(compareSemver('1.0.0-rc.2', '1.0.0-rc.10')).toBeLessThan(0);
    expect(compareSemver('1.0.0-alpha', '1.0.0-alpha.1')).toBeLessThan(0);
    expect(compareSemver('1.0.0-1', '1.0.0-beta')).toBeLessThan(0);
    expect(compareSemver('garbage', '0.0.1')).toBeLessThan(0);
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

describe('pickLatest', () => {
  const releases = [rel('0.2.0'), rel('0.3.0'), rel('0.4.0-rc.1'), rel('0.3.1', { draft: true })];

  it('picks the newest stable release', () => {
    expect(pickLatest(releases, { includePrerelease: false, bad: new Set(), arch: 'x64' })?.version).toBe('0.3.0');
  });

  it('includes prereleases on the prerelease channel', () => {
    expect(pickLatest(releases, { includePrerelease: true, bad: new Set(), arch: 'x64' })?.version).toBe('0.4.0-rc.1');
  });

  it('skips bad versions', () => {
    expect(pickLatest(releases, { includePrerelease: false, bad: new Set(['0.3.0']), arch: 'x64' })?.version).toBe('0.2.0');
  });

  it('skips releases without an asset for this arch', () => {
    expect(pickLatest(releases, { includePrerelease: false, bad: new Set(), arch: 'arm64' })).toBeNull();
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
  makeUpdater(source: ReleaseSource, appDir?: string): Updater;
}

/** Build a fake release tarball for `version` whose `selftest` exits with `code`. */
function buildRelease(releaseDir: string, version: string, code = 0, corrupt = false): void {
  const stage = mkdtempSync(join(tmpdir(), 'ak-rel-'));
  const top = join(stage, `agent-k-local-${version}`);
  mkdirSync(join(top, 'bin'), { recursive: true });
  writeFileSync(join(top, 'bin', 'agent-k-local'), `#!/bin/sh\n[ "$1" = selftest ] && exit ${code}\nexit 0\n`);
  chmodSync(join(top, 'bin', 'agent-k-local'), 0o755);
  writeFileSync(join(top, 'VERSION'), version + '\n');
  const name = tarballName(version, 'x64');
  execFileSync('tar', ['-czf', join(releaseDir, name), '-C', stage, `agent-k-local-${version}`]);
  rmSync(stage, { recursive: true, force: true });
  const digest = corrupt ? '0'.repeat(64) : sha(readFileSync(join(releaseDir, name)));
  writeFileSync(join(releaseDir, 'SHA256SUMS'), `${digest}  ${name}\n`);
}

function fakeSource(releaseDir: string, releases: Release[]): ReleaseSource {
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
        source,
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
      source: fakeSource(fx.releaseDir, [rel('9.9.9')]),
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

  it('applies a verified release and schedules a restart', async () => {
    buildRelease(fx.releaseDir, '0.2.0');
    const u = fx.makeUpdater(fakeSource(fx.releaseDir, [rel('0.2.0')]));
    const s = await u.apply();
    expect(s.lastError).toBeNull();
    expect(readlinkSync(join(fx.paths.app, 'current'))).toBe('0.2.0');
    expect(existsSync(join(fx.paths.app, '0.2.0', 'bin', 'agent-k-local'))).toBe(true);
    const pending = JSON.parse(readFileSync(fx.paths.updatePending, 'utf8'));
    expect(pending).toMatchObject({ from: '0.1.0', to: '0.2.0', attempts: 0 });
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

  it('markHealthy clears the pending update and reports the previous version', () => {
    writeFileSync(fx.paths.updatePending, JSON.stringify({ from: '0.0.9', to: '0.1.0', attempts: 1, at: 1 }));
    const u = fx.makeUpdater(fakeSource(fx.releaseDir, []));
    u.markHealthy();
    expect(existsSync(fx.paths.updatePending)).toBe(false);
    expect(u.status().justUpdatedFrom).toBe('0.0.9');
    expect(fx.kv.get('update.justUpdatedFrom')).toBe('0.0.9');
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

  it('runs the current version and counts attempts while an update is pending', () => {
    writeFileSync(join(root, 'update-pending.json'), JSON.stringify({ from: '0.1.0', to: '0.2.0', attempts: 0 }));
    expect(runServe()).toBe('new 0.2.0');
    expect(JSON.parse(readFileSync(join(root, 'update-pending.json'), 'utf8')).attempts).toBe(1);
  });

  it('rolls back after the third failed start', () => {
    writeFileSync(join(root, 'update-pending.json'), JSON.stringify({ from: '0.1.0', to: '0.2.0', attempts: 2 }));
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
