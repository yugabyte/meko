import { execFile } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import { createReadStream, existsSync, mkdirSync, readFileSync, readdirSync, realpathSync, renameSync, rmSync, symlinkSync, unlinkSync, writeFileSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { promisify } from 'node:util';
import type { ReleaseNotes, UpdateStatus } from '../shared/types.js';
import { dbPath, type Config } from './config.js';
import type { Paths } from './paths.js';
import { releaseNotes, type NoteCommit } from './releasenotes.js';

const run = promisify(execFile);

/** Time limits, so a stalled network call can't leave a check or an update running forever. */
const LIST_TIMEOUT_MS = 60_000;
const DOWNLOAD_TIMEOUT_MS = 10 * 60_000;

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
 * How long a new version must stay up before it counts as good: until then a crash loop rolls it
 * back (see packaging/agent-k-local); after it, the update is final and older versions are removed.
 */
export const HEALTHY_MS = 5 * 60_000;

/**
 * Version dirs under app/ to delete once the running version is healthy: all of them except the
 * running one and one to go back to by hand. That is the version it updated from, or else (after a
 * manual switch) the newest one older than it, or else the newest other one.
 */
export function versionsToPrune(names: string[], running: Set<string>, from: string | null): string[] {
  const others = names.filter((n) => parseSemver(n) && !running.has(n)).sort((a, b) => compareSemver(b, a));
  const current = [...running].filter((v) => parseSemver(v)).sort((a, b) => compareSemver(b, a))[0];
  const previous =
    (from && others.includes(from) ? from : null) ??
    (current ? others.find((v) => compareSemver(v, current) < 0) : undefined) ??
    others[0];
  return others.filter((v) => v !== previous);
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

/** The commits between two refs, oldest first, as GitHub's compare returns them (at most 250). */
export interface Comparison {
  totalCommits: number;
  url: string;
  commits: NoteCommit[];
}

export interface ReleaseSource {
  listReleases(repo: string): Promise<Release[]>;
  /** The commits between two tags (What's new). */
  compare(repo: string, base: string, head: string): Promise<Comparison>;
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

interface GhComparison {
  total_commits: number;
  html_url: string;
  commits: { sha: string; html_url: string; commit: { message: string } }[];
}

function toComparison(c: GhComparison): Comparison {
  return {
    totalCommits: c.total_commits,
    url: c.html_url,
    commits: c.commits.map((x) => ({ sha: x.sha, message: x.commit.message, url: x.html_url })),
  };
}

const compareRoute = (repo: string, base: string, head: string) => `repos/${repo}/compare/${base}...${head}`;

/**
 * The prefixes of AgentK's release tags, newest scheme first: `agentk-v<version>`, and `v<version>`
 * before releases were tagged by product (scripts/release/product.sh).
 * Meko's `meko-v…` releases are another product's: their versions don't parse, so they're never
 * offered.
 */
export const TAG_PREFIXES = ['agentk-v', 'v'] as const;

/** The version an AgentK tag names, or the tag itself when it isn't one (which pickLatest skips). */
export function tagVersion(tag: string): string {
  for (const p of TAG_PREFIXES) if (tag.startsWith(p) && /^\d/.test(tag.slice(p.length))) return tag.slice(p.length);
  return tag;
}

function toRelease(r: GhRelease): Release {
  return {
    tag: r.tag_name,
    version: tagVersion(r.tag_name),
    prerelease: r.prerelease,
    draft: r.draft,
    url: r.html_url,
    assets: r.assets.map((a) => a.name),
  };
}

/** Uses the user's authenticated `gh`, which works for internal repos. */
export class GhReleaseSource implements ReleaseSource {
  async listReleases(repo: string): Promise<Release[]> {
    const { stdout } = await run('gh', ['api', `repos/${repo}/releases?per_page=30`], { maxBuffer: 16 << 20, timeout: LIST_TIMEOUT_MS });
    return (JSON.parse(stdout) as GhRelease[]).map(toRelease);
  }

  async compare(repo: string, base: string, head: string): Promise<Comparison> {
    // Only what the notes need: the comparison's files carry every patch.
    const jq = '{total_commits, html_url, commits: [.commits[] | {sha, html_url, commit: {message: .commit.message}}]}';
    const { stdout } = await run('gh', ['api', compareRoute(repo, base, head), '--jq', jq], { maxBuffer: 16 << 20, timeout: LIST_TIMEOUT_MS });
    return toComparison(JSON.parse(stdout) as GhComparison);
  }

  async download(repo: string, tag: string, asset: string, destDir: string): Promise<string> {
    mkdirSync(destDir, { recursive: true });
    await run('gh', ['release', 'download', tag, '--repo', repo, '--pattern', asset, '--dir', destDir, '--clobber'], {
      timeout: DOWNLOAD_TIMEOUT_MS,
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
    const res = await fetch(`https://api.github.com/repos/${repo}/releases?per_page=30`, {
      headers: this.headers(),
      signal: AbortSignal.timeout(LIST_TIMEOUT_MS),
    });
    if (!res.ok) throw new Error(`GitHub API returned ${res.status} for ${repo} releases`);
    const list = (await res.json()) as GhRelease[];
    this.cache.set(repo, list);
    return list.map(toRelease);
  }

  async compare(repo: string, base: string, head: string): Promise<Comparison> {
    const res = await fetch(`https://api.github.com/${compareRoute(repo, base, head)}`, {
      headers: this.headers(),
      signal: AbortSignal.timeout(LIST_TIMEOUT_MS),
    });
    if (!res.ok) {
      // Without gh signed in or a token, a private repo reads as missing and the rate limit is low.
      const signIn = !process.env.GITHUB_TOKEN && [401, 403, 404].includes(res.status);
      throw new Error(`GitHub API returned ${res.status}${signIn ? '. gh isn’t signed in and GITHUB_TOKEN isn’t set: run gh auth login, or set GITHUB_TOKEN' : ''}`);
    }
    return toComparison((await res.json()) as GhComparison);
  }

  async download(repo: string, tag: string, asset: string, destDir: string): Promise<string> {
    if (!this.cache.has(repo)) await this.listReleases(repo);
    const rel = this.cache.get(repo)?.find((r) => r.tag_name === tag);
    const a = rel?.assets.find((x) => x.name === asset);
    if (!a) throw new Error(`release ${tag} has no asset ${asset}`);
    // Private/internal repos need the API asset URL + token; public ones can use the browser URL.
    // The time limit covers reading the body too.
    const signal = AbortSignal.timeout(DOWNLOAD_TIMEOUT_MS);
    const res = process.env.GITHUB_TOKEN
      ? await fetch(a.url, { headers: this.headers('application/octet-stream'), signal })
      : await fetch(a.browser_download_url, { headers: { 'User-Agent': 'agent-k-local-updater' }, signal });
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

/** The pending update the launcher's rollback reads, or null when there is none. */
function readPending(paths: Paths): PendingUpdate | null {
  try {
    const p = JSON.parse(readFileSync(paths.updatePending, 'utf8')) as PendingUpdate;
    return p && typeof p === 'object' ? p : null;
  } catch {
    return null;
  }
}

/** Name of the running version's directory under app/ (what the rollback wrapper switches back to). */
function runningDirName(version: string, appDir: string | undefined): string {
  try {
    return appDir ? basename(realpathSync(appDir)) : version;
  } catch {
    return version;
  }
}

/** Whether `v` names the running version (by its version or its directory). */
function isRunningVersion(v: string | undefined, version: string, appDir: string | undefined): boolean {
  return !!v && (v === version || v === runningDirName(version, appDir));
}

/**
 * Takes this start back off the launcher's count of starts of a version being tried out, when
 * that version is the one running: after a clean stop, or a start refused because another server
 * holds the home (instance.ts). Neither is a crash, so neither may lead to a rollback.
 */
export function uncountStart(paths: Paths, version: string, appDir: string | undefined = process.env.AK_APP_DIR): void {
  const pending = readPending(paths);
  if (!pending || !isRunningVersion(pending.to, version, appDir)) return;
  try {
    writeFileAtomic(paths.updatePending, JSON.stringify({ ...pending, attempts: Math.max(0, (Number(pending.attempts) || 0) - 1) }));
  } catch {
    // the launcher's count just stays as it was
  }
}

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
const KV_LAST_FROM = 'update.lastUpdatedFrom';

/** A version as its tag names it after the prefix (`agentk-v`, `v`), nothing around it. */
const BARE_VERSION = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;

/** Whether What's new can show the changes from `from` to `to`: both bare versions, `from` the older. */
export function notesRange(from: string, to: string): boolean {
  return BARE_VERSION.test(from) && BARE_VERSION.test(to) && compareSemver(from, to) < 0;
}

export class Updater {
  private timer: NodeJS.Timeout | null = null;
  private latest: string | null;
  private releaseUrl: string | null;
  private lastCheckedAt: number | null;
  private lastError: string | null = null;
  private applying = false;
  /** markHealthy came while an update was being checked for or installed; it runs if that installs nothing. */
  private healthyDuringApply = false;
  private justUpdatedFrom: string | null = null;
  private checking: Promise<UpdateStatus> | null = null;
  /** The update being checked for or installed: a second click while it runs gets its result. */
  private applyRun: Promise<UpdateStatus> | null = null;
  private readonly appDir: string | undefined;
  /** What's new by range: the commits between two tags never change. */
  private notesCache = new Map<string, Promise<ReleaseNotes>>();
  /** Each listed release's tag by its version, from the last check. */
  private tags = new Map<string, string>();

  constructor(private deps: UpdaterDeps) {
    this.appDir = deps.appDir ?? process.env.AK_APP_DIR ?? undefined;
    this.latest = deps.kvGet(KV_LATEST) || null;
    this.releaseUrl = deps.kvGet(KV_URL) || null;
    const checked = Number(deps.kvGet(KV_CHECKED));
    this.lastCheckedAt = Number.isFinite(checked) && checked > 0 ? checked : null;
    if (!this.appDir) this.lastError = DISABLED;
    this.dropBadLatest();
    // Recorded before the server starts (cli.ts makes the Updater first): a version that crashes
    // while starting, the case a rollback is for, still counts as the last update, and once it's
    // rolled back lastUpdatedFrom() drops it.
    const from = this.updatedFrom();
    if (from) deps.kvSet(KV_LAST_FROM, from);
  }

  /** The version this boot updated from: an update was switched to and this is its first boot. */
  private updatedFrom(): string | null {
    const p = this.readPending();
    return p && this.isRunning(p.to) && p.from && !this.isRunning(p.from) ? p.from : null;
  }

  /** A remembered latest version that was rolled back since (it's in bad-versions) is no longer offered. */
  private dropBadLatest(): void {
    if (this.latest && readBadVersions(this.deps.paths).has(this.latest)) {
      this.latest = null;
      this.releaseUrl = null;
    }
  }

  get enabled(): boolean {
    return !!this.appDir;
  }

  start(): void {
    // "Updated to vX" is shown for the first boot of a new version, from the moment it's up.
    const from = this.updatedFrom();
    this.justUpdatedFrom = from;
    this.deps.kvSet(KV_JUST, from ?? '');
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
    // setTimeout fires at once for delays over 2^31-1 ms (about 24 days).
    this.timer = setTimeout(() => {
      void this.check().finally(() => this.schedule());
    }, Math.min(hours * 3600_000, 2 ** 31 - 1));
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
      lastUpdatedFrom: this.lastUpdatedFrom(),
      releaseUrl: this.releaseUrl,
    };
  }

  /** The last update's version, while it's older than this one (not after a rollback or a switch back). */
  private lastUpdatedFrom(): string | null {
    const from = this.deps.kvGet(KV_LAST_FROM) || null;
    return from && notesRange(from, this.deps.version) ? from : null;
  }

  /**
   * What changed from `from` to `to` (checked with notesRange first): the releasing commits
   * between their tags, from GitHub. A range is asked for once; a failed ask is tried again.
   */
  notes(from: string, to: string): Promise<ReleaseNotes> {
    const key = `${from}...${to}`;
    let p = this.notesCache.get(key);
    if (!p) {
      p = this.fetchNotes(from, to);
      this.notesCache.set(key, p);
      p.catch(() => this.notesCache.delete(key));
    }
    return p;
  }

  private async fetchNotes(from: string, to: string): Promise<ReleaseNotes> {
    const { repo } = this.deps.config().update;
    let c: Comparison;
    try {
      c = await this.eachTag([from, to], ([a, b], src) => src.compare(repo, a!, b!));
    } catch (e) {
      const why = ((e as { stderr?: string }).stderr || (e as Error).message).trim();
      throw new Error(`GitHub couldn't compare ${from} and ${to}: ${why}`, { cause: e });
    }
    return {
      from,
      to,
      sections: releaseNotes([...c.commits].reverse(), repo),
      commits: c.commits.length,
      totalCommits: c.totalCommits,
      compareUrl: c.url,
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

  /** The tags a version may have: the listed one, else each prefix's (a pruned release keeps its tag). */
  private tagsOf(version: string): string[] {
    const known = this.tags.get(version);
    return known ? [known] : TAG_PREFIXES.map((p) => `${p}${version}`);
  }

  /**
   * Runs `f` with a tag for each version, trying the next possible tags while GitHub doesn't know
   * the ones tried (`tagsOf`); the last failure is thrown.
   */
  private async eachTag<T>(versions: string[], f: (tags: string[], src: ReleaseSource) => Promise<T>): Promise<T> {
    const src = await this.source();
    const combos = versions.reduce<string[][]>((acc, v) => acc.flatMap((c) => this.tagsOf(v).map((t) => [...c, t])), [[]]);
    let last: unknown;
    for (const tags of combos) {
      try {
        return await f(tags, src);
      } catch (e) {
        last = e;
      }
    }
    throw last;
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
      this.tags = new Map(releases.map((r) => [r.version, r.tag]));
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

  apply(): Promise<UpdateStatus> {
    if (!this.enabled) {
      this.lastError = DISABLED;
      return Promise.resolve(this.status());
    }
    if (this.applyRun) return this.applyRun;
    // Installed and switched to: the restart is on its way.
    if (this.applying) return Promise.resolve(this.status());
    this.applyRun = this.doApply().finally(() => {
      this.applyRun = null;
    });
    return this.applyRun;
  }

  /**
   * Checks again and installs the newest release, not the one the last check found: a release cut
   * since then would otherwise be offered right after the restart. When the check fails, the
   * version found before is installed.
   */
  private async doApply(): Promise<UpdateStatus> {
    this.dropBadLatest();
    // Shown as installing from the click on, through the check too.
    this.applying = true;
    this.changed();
    await this.check();
    const target = this.status().available ? this.latest : null;
    if (!target) return this.endApply();

    this.lastError = null;
    this.changed();
    const { paths } = this.deps;
    const cfg = this.deps.config();
    const staging = join(paths.app, `.staging-${target}-${randomBytes(4).toString('hex')}`);
    try {
      const asset = tarballName(target, this.deps.arch, this.deps.platform);
      const dl = join(paths.downloads, target);
      rmSync(dl, { recursive: true, force: true });
      this.deps.log(`Downloading ${asset}`);
      // By the release's own tag: agentk-v<version>, or v<version> from before (TAG_PREFIXES).
      const [tarball, sums] = await this.eachTag([target], async ([tag], src) => [
        await src.download(cfg.update.repo, tag!, asset, dl),
        await src.download(cfg.update.repo, tag!, 'SHA256SUMS', dl),
      ]);
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
      this.lastError = `Update to ${target} failed: ${(e as Error).message}`;
      this.deps.log(this.lastError);
      return this.endApply();
    } finally {
      rmSync(staging, { recursive: true, force: true });
    }
  }

  /** An apply that installed nothing: a markHealthy that came meanwhile runs now. */
  private endApply(): UpdateStatus {
    this.applying = false;
    if (this.healthyDuringApply) this.markHealthy();
    return this.changed();
  }

  /** Name of the running version's directory under app/ (what the rollback wrapper switches back to). */
  private currentDirName(): string {
    return runningDirName(this.deps.version, this.appDir);
  }

  /** Called by the server once it has been healthy for HEALTHY_MS after boot. */
  markHealthy(): void {
    // An update is being installed, or has been switched to while this process shuts down: the
    // pending file and the new version's directory belong to that update, so leave both alone.
    // If the install fails instead, this runs again then.
    if (this.applying) {
      this.healthyDuringApply = true;
      return;
    }
    this.healthyDuringApply = false;
    const pending = this.readPending();
    // A pending file for another version is stale (say, left by a manual switch): drop it too.
    if (pending) this.clearPending(pending);
    const from = pending && this.isRunning(pending.to) ? pending.from : null;
    if (this.enabled) this.pruneVersions(from);
  }

  /**
   * A deliberate stop (SIGTERM, as systemd sends for a restart) of the version being tried out takes
   * its start back off the launcher's count, so restarting by hand during the HEALTHY_MS window
   * can't look like a crash loop. The old version stopping for the update itself changes nothing.
   */
  stoppedCleanly(): void {
    uncountStart(this.deps.paths, this.deps.version, this.appDir);
  }

  /** Deletes installed versions except the running one and one previous version. */
  private pruneVersions(from: string | null): void {
    const { app } = this.deps.paths;
    let names: string[];
    try {
      names = readdirSync(app);
    } catch {
      return;
    }
    // Whatever app/current points at is what systemd starts next: never delete it.
    const running = new Set([this.currentDirName(), this.deps.version]);
    try {
      running.add(basename(realpathSync(join(app, 'current'))));
    } catch {
      // no current link
    }
    // A running session host keeps the version it started from: node-pty runs its helper from
    // there, and the host keeps running across updates.
    const host = hostVersionDir(this.deps.paths);
    if (host) running.add(host);
    const removed = versionsToPrune(names, running, from);
    for (const v of removed) {
      try {
        rmSync(join(app, v), { recursive: true, force: true });
        this.deps.log(`Removed old version ${v}`);
      } catch (e) {
        this.deps.log(`Removing old version ${v} failed: ${(e as Error).message}`);
      }
    }
    this.pruneSnapshots(names.filter((n) => parseSemver(n) && !removed.includes(n)));
  }

  /**
   * Deletes the database snapshots (state.db.bak-<version>, taken before updating away from that
   * version) of versions that are no longer installed; the ones for kept versions stay.
   */
  private pruneSnapshots(kept: string[]): void {
    const dir = dirname(dbPath(this.deps.paths, this.deps.config()));
    let files: string[];
    try {
      files = readdirSync(dir);
    } catch {
      return;
    }
    for (const f of files) {
      const v = /^state\.db\.bak-(.+)$/.exec(f)?.[1];
      if (!v || !parseSemver(v) || kept.includes(v)) continue;
      try {
        rmSync(join(dir, f), { force: true });
        this.deps.log(`Removed old database snapshot ${f}`);
      } catch (e) {
        this.deps.log(`Removing old database snapshot ${f} failed: ${(e as Error).message}`);
      }
    }
  }

  private readPending(): PendingUpdate | null {
    return readPending(this.deps.paths);
  }

  /** Whether `v` names this running version (by its version or its directory). */
  private isRunning(v: string | undefined): boolean {
    return isRunningVersion(v, this.deps.version, this.appDir);
  }

  /** Ends a pending update: removes the file the launcher's rollback reads. */
  private clearPending(pending: PendingUpdate): void {
    try {
      unlinkSync(this.deps.paths.updatePending);
    } catch {
      // already gone
    }
    if (this.isRunning(pending.to) && pending.from && !this.isRunning(pending.from)) {
      this.deps.log(`Updated from ${pending.from} to ${this.deps.version}`);
    }
  }
}

/** The app version directory the running session host started from (run/host.json), if any. */
export function hostVersionDir(paths: Paths): string | null {
  try {
    const st = JSON.parse(readFileSync(paths.hostState, 'utf8')) as { appDir?: unknown; version?: unknown; pid?: unknown };
    if (typeof st.pid === 'number') process.kill(st.pid, 0);
    if (typeof st.appDir === 'string' && st.appDir) return basename(st.appDir);
    return typeof st.version === 'string' ? st.version : null;
  } catch {
    return null;
  }
}
