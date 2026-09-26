import { execFile } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import { createReadStream, existsSync, mkdirSync, readFileSync, readdirSync, realpathSync, renameSync, rmSync, symlinkSync, unlinkSync, writeFileSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { promisify } from 'node:util';
import type { UpdateStatus } from '../shared/types.js';
import { dbPath, type Config } from './config.js';
import type { Paths } from './paths.js';

const run = promisify(execFile);

// ---------------------------------------------------------------------------
// Versions

export interface SemVer {
  major: number;
  minor: number;
  patch: number;
  pre: string[];
}

export function parseSemver(v: string): SemVer | null {
  const m = /^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$/.exec(v.trim());
  if (!m) return null;
  return { major: +m[1], minor: +m[2], patch: +m[3], pre: m[4] ? m[4].split('.') : [] };
}

/** Negative if a < b, positive if a > b, 0 if equal. Invalid versions sort lowest. */
export function compareSemver(a: string, b: string): number {
  const x = parseSemver(a);
  const y = parseSemver(b);
  if (!x || !y) return (x ? 1 : 0) - (y ? 1 : 0);
  for (const k of ['major', 'minor', 'patch'] as const) {
    if (x[k] !== y[k]) return x[k] - y[k];
  }
  // A release sorts above any of its prereleases.
  if (!x.pre.length || !y.pre.length) return (x.pre.length ? 0 : 1) - (y.pre.length ? 0 : 1);
  for (let i = 0; i < Math.max(x.pre.length, y.pre.length); i++) {
    const p = x.pre[i];
    const q = y.pre[i];
    if (p === undefined) return -1;
    if (q === undefined) return 1;
    const pn = /^\d+$/.test(p);
    const qn = /^\d+$/.test(q);
    if (pn && qn) {
      if (+p !== +q) return +p - +q;
    } else if (pn !== qn) {
      return pn ? -1 : 1; // numeric identifiers sort below alphanumeric ones
    } else if (p !== q) {
      return p < q ? -1 : 1;
    }
  }
  return 0;
}

/**
 * A version's lane: its first prerelease identifier (0.1.2-beta.3 → beta), "prerelease" for a
 * numeric one or a GitHub prerelease without one, else "stable". scripts/prune-releases.sh
 * uses the same rule for GitHub Releases.
 */
export function releaseLane(v: string, prerelease = false): string {
  const pre = parseSemver(v)?.pre[0];
  if (pre && !/^\d+$/.test(pre)) return pre;
  return pre || prerelease ? 'prerelease' : 'stable';
}

/** Installed versions to keep per lane under app/. */
export const KEEP_PER_LANE = 5;

/** Version dirs to delete: all but the newest `keep` in each lane, never a protected one. */
export function versionsToPrune(names: string[], keep: number, protect: Set<string>): string[] {
  const lanes = new Map<string, string[]>();
  for (const n of names) {
    if (!parseSemver(n)) continue;
    const lane = releaseLane(n);
    lanes.set(lane, [...(lanes.get(lane) ?? []), n]);
  }
  const out: string[] = [];
  for (const vs of lanes.values()) {
    vs.sort((a, b) => compareSemver(b, a));
    out.push(...vs.slice(keep).filter((v) => !protect.has(v)));
  }
  return out;
}

// ---------------------------------------------------------------------------
// Release sources (GitHub via `gh`, or plain HTTPS once the repo is public)

export interface Release {
  tag: string;
  version: string;
  prerelease: boolean;
  draft: boolean;
  url: string;
  assets: string[];
}

export interface ReleaseSource {
  listReleases(repo: string): Promise<Release[]>;
  /** Download one asset into destDir and return its path. */
  download(repo: string, tag: string, asset: string, destDir: string): Promise<string>;
}

interface GhRelease {
  tag_name: string;
  prerelease: boolean;
  draft: boolean;
  html_url: string;
  assets: { name: string; url: string; browser_download_url: string }[];
}

function toRelease(r: GhRelease): Release {
  return {
    tag: r.tag_name,
    version: r.tag_name.replace(/^v/, ''),
    prerelease: r.prerelease,
    draft: r.draft,
    url: r.html_url,
    assets: r.assets.map((a) => a.name),
  };
}

/** Uses the user's authenticated `gh`, which works for internal repos. */
export class GhReleaseSource implements ReleaseSource {
  async listReleases(repo: string): Promise<Release[]> {
    const { stdout } = await run('gh', ['api', `repos/${repo}/releases?per_page=30`], { maxBuffer: 16 << 20 });
    return (JSON.parse(stdout) as GhRelease[]).map(toRelease);
  }

  async download(repo: string, tag: string, asset: string, destDir: string): Promise<string> {
    mkdirSync(destDir, { recursive: true });
    await run('gh', ['release', 'download', tag, '--repo', repo, '--pattern', asset, '--dir', destDir, '--clobber'], {
      timeout: 10 * 60_000,
    });
    return join(destDir, asset);
  }
}

/** Plain HTTPS against api.github.com; uses GITHUB_TOKEN when set. */
export class HttpsReleaseSource implements ReleaseSource {
  private cache = new Map<string, GhRelease[]>();

  private headers(accept = 'application/vnd.github+json'): Record<string, string> {
    const h: Record<string, string> = { Accept: accept, 'User-Agent': 'agent-k-local-updater' };
    if (process.env.GITHUB_TOKEN) h.Authorization = `Bearer ${process.env.GITHUB_TOKEN}`;
    return h;
  }

  async listReleases(repo: string): Promise<Release[]> {
    const res = await fetch(`https://api.github.com/repos/${repo}/releases?per_page=30`, { headers: this.headers() });
    if (!res.ok) throw new Error(`GitHub API returned ${res.status} for ${repo} releases`);
    const list = (await res.json()) as GhRelease[];
    this.cache.set(repo, list);
    return list.map(toRelease);
  }

  async download(repo: string, tag: string, asset: string, destDir: string): Promise<string> {
    if (!this.cache.has(repo)) await this.listReleases(repo);
    const rel = this.cache.get(repo)?.find((r) => r.tag_name === tag);
    const a = rel?.assets.find((x) => x.name === asset);
    if (!a) throw new Error(`release ${tag} has no asset ${asset}`);
    // Private/internal repos need the API asset URL + token; public ones can use the browser URL.
    const res = process.env.GITHUB_TOKEN
      ? await fetch(a.url, { headers: this.headers('application/octet-stream') })
      : await fetch(a.browser_download_url, { headers: { 'User-Agent': 'agent-k-local-updater' } });
    if (!res.ok) throw new Error(`download of ${asset} failed with HTTP ${res.status}`);
    mkdirSync(destDir, { recursive: true });
    const dest = join(destDir, asset);
    writeFileSync(dest, Buffer.from(await res.arrayBuffer()));
    return dest;
  }
}

/** `gh` when it is installed and logged in, HTTPS otherwise. */
export async function defaultReleaseSource(): Promise<ReleaseSource> {
  try {
    await run('gh', ['auth', 'status'], { timeout: 15_000 });
    return new GhReleaseSource();
  } catch {
    return new HttpsReleaseSource();
  }
}

// ---------------------------------------------------------------------------
// Helpers

export async function sha256File(path: string): Promise<string> {
  const h = createHash('sha256');
  for await (const chunk of createReadStream(path)) h.update(chunk as Buffer);
  return h.digest('hex');
}

/** Parse `sha256sum` output: "<hex>  <name>" or "<hex> *<name>". */
export function parseSha256Sums(text: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const line of text.split('\n')) {
    const m = /^([0-9a-fA-F]{64})\s+\*?(.+?)\s*$/.exec(line);
    if (m) out.set(m[2], m[1].toLowerCase());
  }
  return out;
}

export async function verifySha256(file: string, sumsFile: string): Promise<void> {
  const want = parseSha256Sums(readFileSync(sumsFile, 'utf8')).get(basename(file));
  if (!want) throw new Error(`SHA256SUMS has no entry for ${basename(file)}`);
  const got = await sha256File(file);
  if (got !== want) throw new Error(`checksum mismatch for ${basename(file)}`);
}

export function readBadVersions(paths: Paths): Set<string> {
  try {
    return new Set(
      readFileSync(paths.badVersions, 'utf8')
        .split('\n')
        .map((s) => s.trim().replace(/^v/, ''))
        .filter(Boolean),
    );
  } catch {
    return new Set();
  }
}

/** The release asset for a version on this platform and CPU: agent-k-local-<v>-<linux|darwin>-<x64|arm64>.tar.gz. */
export function tarballName(version: string, arch: string = process.arch, platform: string = process.platform): string {
  return `agent-k-local-${version}-${platform}-${arch}.tar.gz`;
}

/** Pick the newest eligible release, or null. */
export function pickLatest(
  releases: Release[],
  opts: { includePrerelease: boolean; bad: Set<string>; arch?: string; platform?: string },
): Release | null {
  let best: Release | null = null;
  for (const r of releases) {
    if (r.draft || !parseSemver(r.version)) continue;
    if (r.prerelease && !opts.includePrerelease) continue;
    if (opts.bad.has(r.version)) continue;
    // Releases without a build for this platform (e.g. macOS before it was supported) are skipped.
    if (!r.assets.includes(tarballName(r.version, opts.arch, opts.platform)) || !r.assets.includes('SHA256SUMS')) continue;
    if (!best || compareSemver(r.version, best.version) > 0) best = r;
  }
  return best;
}

function writeFileAtomic(path: string, data: string): void {
  const tmp = `${path}.tmp-${randomBytes(4).toString('hex')}`;
  writeFileSync(tmp, data);
  renameSync(tmp, path);
}

/** Point `link` at `target` atomically (symlink under a temp name, then rename over). */
export function repointSymlink(link: string, target: string): void {
  const tmp = `${link}.tmp-${randomBytes(4).toString('hex')}`;
  symlinkSync(target, tmp);
  renameSync(tmp, link);
}

// ---------------------------------------------------------------------------
// Updater

export interface UpdaterDeps {
  paths: Paths;
  config: () => Config;
  version: string;
  kvGet(k: string): string | null;
  kvSet(k: string, v: string): void;
  snapshotDb(dest: string): void;
  log(msg: string): void;
  onChange(s: UpdateStatus): void;
  requestRestart(): void;
  /** Installed version dir (defaults to AK_APP_DIR). Unset = running from a source checkout. */
  appDir?: string;
  /** Release source override (tests). */
  source?: ReleaseSource;
  arch?: string;
  platform?: string;
}

interface PendingUpdate {
  from: string;
  to: string;
  attempts: number;
  at: number;
}

const DISABLED = 'Updates are disabled when running from a source checkout.';
const KV_CHECKED = 'update.lastCheckedAt';
const KV_LATEST = 'update.latest';
const KV_URL = 'update.releaseUrl';
const KV_JUST = 'update.justUpdatedFrom';

export class Updater {
  private timer: NodeJS.Timeout | null = null;
  private latest: string | null;
  private releaseUrl: string | null;
  private lastCheckedAt: number | null;
  private lastError: string | null = null;
  private applying = false;
  private justUpdatedFrom: string | null = null;
  private checking: Promise<UpdateStatus> | null = null;
  private readonly appDir: string | undefined;

  constructor(private deps: UpdaterDeps) {
    this.appDir = deps.appDir ?? process.env.AK_APP_DIR ?? undefined;
    this.latest = deps.kvGet(KV_LATEST) || null;
    this.releaseUrl = deps.kvGet(KV_URL) || null;
    const checked = Number(deps.kvGet(KV_CHECKED));
    this.lastCheckedAt = Number.isFinite(checked) && checked > 0 ? checked : null;
    if (!this.appDir) this.lastError = DISABLED;
  }

  get enabled(): boolean {
    return !!this.appDir;
  }

  start(): void {
    // "Updated to vX" is shown for one boot only; markHealthy() sets it again for this boot.
    this.deps.kvSet(KV_JUST, '');
    if (!this.enabled) return;
    void this.check();
    this.schedule();
  }

  stop(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
  }

  private schedule(): void {
    this.stop();
    const hours = Math.max(0.1, this.deps.config().update.intervalHours || 6);
    this.timer = setTimeout(() => {
      void this.check().finally(() => this.schedule());
    }, hours * 3600_000);
    this.timer.unref();
  }

  status(): UpdateStatus {
    const channel = this.deps.config().update.channel;
    return {
      current: this.deps.version,
      channel,
      latest: this.latest,
      available: this.enabled && channel !== 'off' && !!this.latest && compareSemver(this.latest, this.deps.version) > 0,
      applying: this.applying,
      lastCheckedAt: this.lastCheckedAt,
      lastError: this.lastError,
      justUpdatedFrom: this.justUpdatedFrom,
      releaseUrl: this.releaseUrl,
    };
  }

  private changed(): UpdateStatus {
    const s = this.status();
    this.deps.onChange(s);
    return s;
  }

  private async source(): Promise<ReleaseSource> {
    return this.deps.source ?? (await defaultReleaseSource());
  }

  check(): Promise<UpdateStatus> {
    if (!this.enabled) {
      this.lastError = DISABLED;
      return Promise.resolve(this.status());
    }
    if (this.deps.config().update.channel === 'off') return Promise.resolve(this.status());
    this.checking ??= this.doCheck().finally(() => {
      this.checking = null;
    });
    return this.checking;
  }

  private async doCheck(): Promise<UpdateStatus> {
    const cfg = this.deps.config();
    try {
      const releases = await (await this.source()).listReleases(cfg.update.repo);
      const best = pickLatest(releases, {
        includePrerelease: cfg.update.channel === 'prerelease',
        bad: readBadVersions(this.deps.paths),
        arch: this.deps.arch,
        platform: this.deps.platform,
      });
      this.latest = best?.version ?? null;
      this.releaseUrl = best?.url ?? null;
      this.lastError = null;
    } catch (e) {
      this.lastError = `Update check failed: ${(e as Error).message}`;
      this.deps.log(this.lastError);
    }
    this.lastCheckedAt = Date.now();
    this.deps.kvSet(KV_CHECKED, String(this.lastCheckedAt));
    this.deps.kvSet(KV_LATEST, this.latest ?? '');
    this.deps.kvSet(KV_URL, this.releaseUrl ?? '');
    return this.changed();
  }

  async apply(): Promise<UpdateStatus> {
    if (!this.enabled) {
      this.lastError = DISABLED;
      return this.status();
    }
    if (this.applying) return this.status();
    if (!this.status().available) await this.check();
    const target = this.status().available ? this.latest : null;
    if (!target) return this.status();

    this.applying = true;
    this.lastError = null;
    this.changed();
    const { paths } = this.deps;
    const cfg = this.deps.config();
    const staging = join(paths.app, `.staging-${target}-${randomBytes(4).toString('hex')}`);
    try {
      const src = await this.source();
      const tag = `v${target}`;
      const asset = tarballName(target, this.deps.arch, this.deps.platform);
      const dl = join(paths.downloads, target);
      rmSync(dl, { recursive: true, force: true });
      this.deps.log(`Downloading ${asset}`);
      const tarball = await src.download(cfg.update.repo, tag, asset, dl);
      const sums = await src.download(cfg.update.repo, tag, 'SHA256SUMS', dl);
      await verifySha256(tarball, sums);

      if (cfg.update.requireAttestation) {
        try {
          await run('gh', ['attestation', 'verify', tarball, '--repo', cfg.update.repo], { timeout: 120_000 });
        } catch (e) {
          throw new Error(`attestation verification failed: ${((e as { stderr?: string }).stderr || (e as Error).message).trim()}`, { cause: e });
        }
      }

      mkdirSync(staging, { recursive: true });
      await run('tar', ['-xzf', tarball, '-C', staging], { timeout: 5 * 60_000 });
      const extracted = join(staging, `agent-k-local-${target}`);
      if (!existsSync(join(extracted, 'bin', 'agent-k-local'))) throw new Error('unexpected tarball layout');

      const dest = join(paths.app, target);
      const currentDir = this.currentDirName();
      if (target === currentDir) throw new Error(`version ${target} is already the running version`);
      rmSync(dest, { recursive: true, force: true });
      renameSync(extracted, dest);

      try {
        // The new wrapper sets its own AK_APP_DIR/AK_VERSION; don't leak ours.
        const env = { ...process.env };
        delete env.AK_APP_DIR;
        delete env.AK_VERSION;
        await run(join(dest, 'bin', 'agent-k-local'), ['selftest'], { timeout: 60_000, env });
      } catch (e) {
        throw new Error(`self-test of ${target} failed: ${((e as { stderr?: string }).stderr || (e as Error).message).trim()}`, { cause: e });
      }

      this.deps.snapshotDb(join(dirname(dbPath(paths, cfg)), `state.db.bak-${this.deps.version}`));
      const pending: PendingUpdate = { from: currentDir, to: target, attempts: 0, at: Date.now() };
      writeFileAtomic(paths.updatePending, JSON.stringify(pending));
      repointSymlink(join(paths.app, 'current'), target);
      rmSync(dl, { recursive: true, force: true });
      this.deps.log(`Installed ${target}; restarting`);
      this.changed();
      this.deps.requestRestart();
      return this.status();
    } catch (e) {
      this.applying = false;
      this.lastError = `Update to ${target} failed: ${(e as Error).message}`;
      this.deps.log(this.lastError);
      return this.changed();
    } finally {
      rmSync(staging, { recursive: true, force: true });
    }
  }

  /** Name of the running version's directory under app/ (what the rollback wrapper switches back to). */
  private currentDirName(): string {
    try {
      return basename(realpathSync(this.appDir!));
    } catch {
      return this.deps.version;
    }
  }

  /** Called by the server once it has been healthy for 30s after boot. */
  markHealthy(): void {
    this.clearPending();
    if (this.enabled) this.pruneVersions();
  }

  /** Deletes installed versions beyond the newest few in each lane, keeping the running one. */
  private pruneVersions(): void {
    const { app } = this.deps.paths;
    let names: string[];
    try {
      names = readdirSync(app);
    } catch {
      return;
    }
    for (const v of versionsToPrune(names, KEEP_PER_LANE, new Set([this.currentDirName(), this.deps.version]))) {
      try {
        rmSync(join(app, v), { recursive: true, force: true });
        this.deps.log(`Removed old version ${v}`);
      } catch (e) {
        this.deps.log(`Removing old version ${v} failed: ${(e as Error).message}`);
      }
    }
  }

  private clearPending(): void {
    let pending: PendingUpdate;
    try {
      pending = JSON.parse(readFileSync(this.deps.paths.updatePending, 'utf8')) as PendingUpdate;
    } catch {
      return;
    }
    try {
      unlinkSync(this.deps.paths.updatePending);
    } catch {
      // already gone
    }
    if (pending?.from && pending.from !== this.deps.version) {
      this.justUpdatedFrom = pending.from;
      this.deps.kvSet(KV_JUST, pending.from);
      this.deps.log(`Updated from ${pending.from} to ${this.deps.version}`);
      this.changed();
    }
  }
}
