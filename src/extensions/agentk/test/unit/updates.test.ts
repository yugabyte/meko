import { execFileSync } from 'node:child_process';
import { chmodSync, copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { kvGet, kvSet, openDb } from '../../src/server/db.js';
import { restartServer } from '../../src/server/health.js';
import { getPaths, type Paths } from '../../src/server/paths.js';
import type { ServiceManager } from '../../src/server/service.js';
import {
  compareSemver,
  HttpsCompareSource,
  notesRange,
  onProbation,
  pruneSnapshots,
  snapshotBeforeMigrating,
  switchedAway,
  Updates,
  type CompareSource,
} from '../../src/server/updates.js';

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

describe('notesRange', () => {
  it.each([
    ['0.0.9', '0.1.0', true],
    ['0.1.0-beta.1', '0.1.0', true],
    ['0.1.0', '0.1.0', false],
    ['0.2.0', '0.1.0', false],
    ['main', '0.1.0', false],
    ['0.0.9', '0.1.0/../x', false],
    ['v0.0.9', '0.1.0', false],
    [' 0.0.9', '0.1.0', false],
  ])("What's new from %s to %s is a range: %s", (from, to, ok) => {
    expect(notesRange(from, to)).toBe(ok);
  });
});

describe('Updates', () => {
  const make = (updatedFrom: string | null, kv = new Map<string, string>(), source?: CompareSource) => ({
    kv,
    u: new Updates({ version: '0.1.0', kvGet: (k) => kv.get(k) ?? null, kvSet: (k, v) => void kv.set(k, v), updatedFrom, source }),
  });

  it("says which update this version came from while it's on probation, and keeps it for What's new after", () => {
    const { u, kv } = make('0.0.9');
    expect(u.status()).toEqual({ current: '0.1.0', justUpdatedFrom: '0.0.9', lastUpdatedFrom: '0.0.9' });
    // A later start, probation over: no notice, but Settings still offers What's new in this version.
    expect(make(null, kv).u.status()).toEqual({ current: '0.1.0', justUpdatedFrom: null, lastUpdatedFrom: '0.0.9' });
    // A newer "from" (a rollback, or a switch back by hand) is no update.
    expect(make('0.2.0').u.status()).toEqual({ current: '0.1.0', justUpdatedFrom: null, lastUpdatedFrom: null });
  });

  it("reads What's new from the commits between two tags, newest first, asking GitHub once per range", async () => {
    // Across the switch from v… to agentk-v… tags: GitHub knows only v0.0.9 and agentk-v0.1.0.
    let down = true;
    const notFound = () => Object.assign(new Error('Command failed: gh api'), { stderr: 'gh: Not Found (HTTP 404)\n' });
    const compare = vi.fn(async (_repo: string, base: string, head: string) => {
      if (down || base !== 'v0.0.9' || head !== 'agentk-v0.1.0') throw notFound();
      return {
        totalCommits: 300,
        url: 'https://github.com/o/r/compare/v0.0.9...agentk-v0.1.0',
        commits: [
          { sha: '1111111aaa', message: 'fix: the older fix', url: 'c1' },
          { sha: '2222222bbb', message: 'docs: not a change', url: 'c2' },
          { sha: '3333333ccc', message: 'fix: the newer fix', url: 'c3' },
        ],
      };
    });
    const { u } = make(null, new Map(), { compare });
    await expect(u.notes('0.0.9', '0.1.0')).rejects.toThrow("GitHub couldn't compare 0.0.9 and 0.1.0: gh: Not Found (HTTP 404)");
    // A failed ask isn't kept: the next one asks again, and its answer is kept.
    down = false;
    const n = await u.notes('0.0.9', '0.1.0');
    expect(await u.notes('0.0.9', '0.1.0')).toBe(n);
    expect(compare).toHaveBeenLastCalledWith('yugabyte/meko', 'v0.0.9', 'agentk-v0.1.0');
    expect(n).toMatchObject({ from: '0.0.9', to: '0.1.0', commits: 3, totalCommits: 300, compareUrl: 'https://github.com/o/r/compare/v0.0.9...agentk-v0.1.0' });
    expect(n.sections).toEqual([{ title: 'Fixes', items: [expect.objectContaining({ summary: 'the newer fix' }), expect.objectContaining({ summary: 'the older fix' })] }]);
  });

  it.each([
    { status: 404, token: undefined, hint: true },
    { status: 403, token: undefined, hint: true },
    { status: 404, token: 't', hint: false },
    { status: 500, token: undefined, hint: false },
  ])('names the sign-in only when it can be why GitHub refused a comparison ($status, token $token)', async ({ status, token, hint }) => {
    vi.stubEnv('GITHUB_TOKEN', token);
    vi.stubGlobal('fetch', vi.fn(async () => new Response('{}', { status })));
    try {
      const err = await new HttpsCompareSource().compare('o/r', 'v0.0.9', 'v0.1.0').catch((e: Error) => e);
      expect(String(err)).toContain(`GitHub API returned ${status}`);
      expect(String(err).includes('gh auth login')).toBe(hint);
    } finally {
      vi.unstubAllGlobals();
      vi.unstubAllEnvs();
    }
  });
});

describe('after an update', () => {
  let root: string;
  let paths: Paths;
  beforeEach(() => {
    root = mkdtempSync(join(tmpdir(), 'ak-upd-'));
    paths = getPaths(join(root, 'home'));
    for (const v of ['0.1.0', '0.2.0']) mkdirSync(join(paths.app, v), { recursive: true });
    symlinkSync('0.2.0', join(paths.app, 'current'));
  });
  afterEach(() => rmSync(root, { recursive: true, force: true }));

  it('keeps a copy of the database as the old version left it, once, before the new one migrates it', () => {
    const file = join(paths.home, 'state.db');
    const db = openDb(file);
    kvSet(db, 'k', 'before');
    db.close();
    expect(snapshotBeforeMigrating(file, null)).toBeNull();
    expect(snapshotBeforeMigrating(file, '0.1.0')).toBe(`${file}.bak-0.1.0`);
    const copy = openDb(`${file}.bak-0.1.0`);
    expect(kvGet(copy, 'k')).toBe('before');
    copy.close();
    // A later start on probation keeps the first copy.
    const again = openDb(file);
    kvSet(again, 'k', 'after');
    again.close();
    expect(snapshotBeforeMigrating(file, '0.1.0')).toBeNull();
    const kept = openDb(`${file}.bak-0.1.0`);
    expect(kvGet(kept, 'k')).toBe('before');
    kept.close();
  });

  it('deletes the database copies of versions no longer installed, and only those', () => {
    const file = join(paths.home, 'state.db');
    for (const v of ['0.0.5', '0.1.0', 'notes']) writeFileSync(`${file}.bak-${v}`, 'x');
    expect(pruneSnapshots(paths, file)).toEqual(['state.db.bak-0.0.5']);
    expect(existsSync(`${file}.bak-0.1.0`)).toBe(true);
    expect(existsSync(`${file}.bak-notes`)).toBe(true);
  });

  it('is on probation only with MEKO_UPDATED_FROM, for its first 5 minutes', () => {
    expect(onProbation(1000, { MEKO_UPDATED_FROM: '0.1.0' })).toBe(true);
    expect(onProbation(5 * 60_000, { MEKO_UPDATED_FROM: '0.1.0' })).toBe(false);
    expect(onProbation(1000, {})).toBe(false);
  });

  it('tells a stop into an update Meko switched to from any other stop', () => {
    expect(switchedAway(join(paths.app, '0.2.0'))).toBe(false);
    expect(switchedAway(join(paths.app, '0.1.0'))).toBe(true);
    expect(switchedAway(undefined)).toBe(false);
  });

  describe('meko kode restart', () => {
    const services = (installed = true) => {
      const restarted: string[] = [];
      const svc = { installed: () => installed, restart: (s: string) => void restarted.push(s) } as unknown as ServiceManager;
      return { svc, restarted };
    };

    it("restarts the server's service, never the host's, and waits for the version app/current names", async () => {
      const { svc, restarted } = services();
      const answers = [{ ok: false, error: 'connect ENOENT' }, { ok: true, version: '0.1.0' }, { ok: true, version: '0.2.0' }] as const;
      const probe = vi.fn(async () => answers[Math.min(probe.mock.calls.length - 1, answers.length - 1)]!);
      expect(await restartServer(paths, svc, { probe })).toBe('0.2.0');
      expect(restarted).toEqual(['server']);
      expect(probe).toHaveBeenCalledTimes(3);
    });

    it("says why when nothing answers in time, or there's no service to restart", async () => {
      const { svc } = services();
      const probe = async () => ({ ok: false as const, error: 'connect ENOENT' });
      await expect(restartServer(paths, svc, { probe, waitMs: 0 })).rejects.toThrow("The server didn't answer within 0 s (connect ENOENT)");
      await expect(restartServer(paths, services(false).svc, { probe })).rejects.toThrow(/No background services run this home/);
    });
  });
});

describe('the launcher', () => {
  const launcher = resolve(__dirname, '../../packaging/me_kode');
  let root: string;
  let bin: string;
  let log: string;

  beforeEach(() => {
    // Resolved: the launcher names itself with links followed, and macOS's tmpdir is under /var, a link to /private/var.
    root = realpathSync(mkdtempSync(join(tmpdir(), 'ak-launch-')));
    bin = join(root, 'path');
    log = join(root, 'meko.log');
    const dir = join(root, 'home', 'app', '0.2.0');
    for (const d of ['bin', 'node/bin', 'lib/dist/server']) mkdirSync(join(dir, d), { recursive: true });
    copyFileSync(launcher, join(dir, 'bin', 'me_kode'));
    chmodSync(join(dir, 'bin', 'me_kode'), 0o755);
    // Its names before, as a release carries them, which services installed then and older Mekos run.
    symlinkSync('me_kode', join(dir, 'bin', 'agentk'));
    symlinkSync(process.execPath, join(dir, 'node', 'bin', 'me_kode'));
    writeFileSync(join(dir, 'VERSION'), '0.2.0\n');
    writeFileSync(join(dir, 'lib', 'dist', 'server', 'cli.js'), 'console.log("node", process.argv.slice(2).join(" "))\n');
    symlinkSync('0.2.0', join(root, 'home', 'app', 'current'));
    mkdirSync(bin);
  });
  afterEach(() => rmSync(root, { recursive: true, force: true }));

  /** A meko on PATH that can (or can't) launch, recording what it's asked to. */
  const meko = (supported: boolean) => {
    writeFileSync(join(bin, 'meko'), `#!/bin/sh\necho "meko $*" >> "${log}"\n[ "$1 $2" = "_launch --supported" ] && exit ${supported ? 0 : 2}\necho launched\n`);
    chmodSync(join(bin, 'meko'), 0o755);
  };
  const run = (args: string[], env: Record<string, string> = {}, command = 'me_kode') =>
    execFileSync(join(root, 'home', 'app', 'current', 'bin', command), args, {
      encoding: 'utf8',
      env: { PATH: `${bin}:/usr/bin:/bin`, HOME: join(root, 'h'), ...env },
      stdio: ['ignore', 'pipe', 'pipe'],
    }).trim();
  const calls = () => (existsSync(log) ? readFileSync(log, 'utf8').trim().split('\n') : []);

  it.each(['serve', 'host'])('hands %s to Meko, naming its own resolved path, whichever name ran it', (cmd) => {
    meko(true);
    expect(run([cmd])).toBe('launched');
    expect(run([cmd], {}, 'agentk')).toBe('launched');
    const launch = `meko _launch agentk ${join(root, 'home', 'app', '0.2.0', 'bin', 'me_kode')} ${cmd}`;
    expect(calls()).toEqual(['meko _launch --supported', launch, 'meko _launch --supported', launch]);
  });

  it("runs the server itself when Meko's launcher has started it, when no Meko can, and for other commands", () => {
    meko(true);
    expect(run(['serve'], { MEKO_LAUNCHED: '1' })).toBe('node serve');
    expect(run(['version', '--json'])).toBe('node version --json');
    expect(calls()).toEqual([]);
    meko(false);
    expect(run(['serve'])).toBe('node serve');
    expect(calls()).toEqual(['meko _launch --supported']);
  });
});
