// What Kode knows about its own updates. Meko installs them (`meko extension update kode`, its
// Extensions page, Kode's Update button through Meko's routes) and runs their probation through
// `meko _launch`: counting a new version's starts, rolling back one that keeps failing, and finishing
// the update once it has run 5 minutes. Kode only learns it was updated (MEKO_UPDATED_FROM, which
// Meko sets while the version is on probation), says so, snapshots its database before the new
// version migrates it, and shows What's new.

import { execFile } from 'node:child_process';
import { existsSync, readdirSync, realpathSync, rmSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { promisify } from 'node:util';
import type { ReleaseNotes, UpdateStatus } from '../shared/types.js';
import { snapshotFile } from './db.js';
import type { Paths } from './paths.js';
import { releaseNotes, type NoteCommit } from './releasenotes.js';

const run = promisify(execFile);

/** Time limit on asking GitHub, so a stalled call can't leave What's new loading forever. */
const COMPARE_TIMEOUT_MS = 60_000;

/** Where Kode's releases are published: the commits What's new lists are compared there. */
export const RELEASES_REPO = 'yugabyte/meko';

/**
 * How long a new version is on probation: Meko finishes the update once it has run this long. The
 * home's move into Meko's data folder waits for it (movehome.ts).
 */
export const HEALTHY_MS = 5 * 60_000;

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

/** A version as its tag names it after the prefix (`kode-v`, `agentk-v`, `v`), nothing around it. */
const BARE_VERSION = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;

/** Whether What's new can show the changes from `from` to `to`: both bare versions, `from` the older. */
export function notesRange(from: string, to: string): boolean {
  return BARE_VERSION.test(from) && BARE_VERSION.test(to) && compareSemver(from, to) < 0;
}

/**
 * The prefixes of Kode's release tags, newest scheme first, as `tag_prefixes` in
 * scripts/release/product.sh: `kode-v<version>`, `agentk-v<version>` before Meko called it Kode,
 * and `v<version>` before releases were tagged by product.
 */
export const TAG_PREFIXES = ['kode-v', 'agentk-v', 'v'] as const;

// ---------------------------------------------------------------------------
// Comparing two tags on GitHub (What's new)

/** The commits between two refs, oldest first, as GitHub's compare returns them (at most 250). */
export interface Comparison {
  totalCommits: number;
  url: string;
  commits: NoteCommit[];
}

/** What compares two tags: the user's `gh` (which works for internal repos), or plain HTTPS. */
export interface CompareSource {
  compare(repo: string, base: string, head: string): Promise<Comparison>;
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

/** Uses the user's authenticated `gh`. */
export class GhCompareSource implements CompareSource {
  async compare(repo: string, base: string, head: string): Promise<Comparison> {
    // Only what the notes need: the comparison's files carry every patch.
    const jq = '{total_commits, html_url, commits: [.commits[] | {sha, html_url, commit: {message: .commit.message}}]}';
    const { stdout } = await run('gh', ['api', compareRoute(repo, base, head), '--jq', jq], { maxBuffer: 16 << 20, timeout: COMPARE_TIMEOUT_MS });
    return toComparison(JSON.parse(stdout) as GhComparison);
  }
}

/** Plain HTTPS against api.github.com; uses GITHUB_TOKEN when set. */
export class HttpsCompareSource implements CompareSource {
  async compare(repo: string, base: string, head: string): Promise<Comparison> {
    const headers: Record<string, string> = { Accept: 'application/vnd.github+json', 'User-Agent': 'kode' };
    if (process.env.GITHUB_TOKEN) headers.Authorization = `Bearer ${process.env.GITHUB_TOKEN}`;
    const res = await fetch(`https://api.github.com/${compareRoute(repo, base, head)}`, {
      headers,
      signal: AbortSignal.timeout(COMPARE_TIMEOUT_MS),
    });
    if (!res.ok) {
      // Without gh signed in or a token, a private repo reads as missing and the rate limit is low.
      const signIn = !process.env.GITHUB_TOKEN && [401, 403, 404].includes(res.status);
      throw new Error(`GitHub API returned ${res.status}${signIn ? '. gh isn’t signed in and GITHUB_TOKEN isn’t set: run gh auth login, or set GITHUB_TOKEN' : ''}`);
    }
    return toComparison((await res.json()) as GhComparison);
  }
}

/** `gh` when it is installed and signed in, HTTPS otherwise. */
export async function defaultCompareSource(): Promise<CompareSource> {
  try {
    await run('gh', ['auth', 'status'], { timeout: 15_000 });
    return new GhCompareSource();
  } catch {
    return new HttpsCompareSource();
  }
}

// ---------------------------------------------------------------------------
// The update this start came from

/**
 * The version Meko updated from, while this one is on probation (MEKO_UPDATED_FROM, set by
 * `meko _launch` on every start until the update is done), or null.
 */
export function updatedFrom(env: NodeJS.ProcessEnv = process.env): string | null {
  const from = env.MEKO_UPDATED_FROM?.trim();
  return from || null;
}

/** Whether this version is still on probation: Meko updated to it, and it hasn't run HEALTHY_MS yet. */
export function onProbation(uptimeMs = process.uptime() * 1000, env: NodeJS.ProcessEnv = process.env): boolean {
  return !!updatedFrom(env) && uptimeMs < HEALTHY_MS;
}

/**
 * Whether app/current no longer names the version this process runs from (`appDir`, KODE_APP_DIR):
 * Meko switched it to another version, so a stop now is a restart into that update.
 */
export function switchedAway(appDir: string | undefined = process.env.KODE_APP_DIR): boolean {
  if (!appDir) return false;
  try {
    return realpathSync(join(dirname(appDir), 'current')) !== realpathSync(appDir);
  } catch {
    return false;
  }
}

/** The database's snapshot from before an update away from `version`: state.db.bak-<version>. */
export function snapshotPath(dbFile: string, version: string): string {
  return `${dbFile}.bak-${version}`;
}

/**
 * Before the database is opened (and migrated) by a version Meko just updated to: a copy of it as the
 * version it updated from left it, once per update, for going back by hand. Returns the copy made, or
 * null when there was nothing to do.
 */
export function snapshotBeforeMigrating(dbFile: string, from: string | null): string | null {
  if (!from || !parseSemver(from) || !existsSync(dbFile)) return null;
  const dest = snapshotPath(dbFile, from);
  if (existsSync(dest)) return null;
  snapshotFile(dbFile, dest);
  return dest;
}

/**
 * Deletes the database snapshots of versions no longer installed in app/ (Meko prunes old versions
 * once an update is done); the ones for installed versions stay. Returns what it deleted.
 */
export function pruneSnapshots(paths: Paths, dbFile: string): string[] {
  let installed: Set<string>;
  let files: string[];
  try {
    installed = new Set(readdirSync(paths.app).filter((n) => parseSemver(n)));
    files = readdirSync(dirname(dbFile));
  } catch {
    return [];
  }
  // A home with no versions in app/ (a source checkout) keeps every snapshot.
  if (!installed.size) return [];
  const prefix = `${basename(dbFile)}.bak-`;
  const gone: string[] = [];
  for (const f of files) {
    const v = f.startsWith(prefix) ? f.slice(prefix.length) : null;
    if (!v || !parseSemver(v) || installed.has(v)) continue;
    try {
      rmSync(join(dirname(dbFile), f), { force: true });
      gone.push(f);
    } catch {
      // stays for the next start
    }
  }
  return gone;
}

// ---------------------------------------------------------------------------
// Status and What's new

export interface UpdatesDeps {
  version: string;
  kvGet(k: string): string | null;
  kvSet(k: string, v: string): void;
  /** The version this start was updated from (defaults to `updatedFrom()`). */
  updatedFrom?: string | null;
  /** Compare source override (tests). */
  source?: CompareSource;
}

const KV_LAST_FROM = 'update.lastUpdatedFrom';

export class Updates {
  private readonly justUpdatedFrom: string | null;
  /** What's new by range: the commits between two tags never change. */
  private notesCache = new Map<string, Promise<ReleaseNotes>>();

  constructor(private deps: UpdatesDeps) {
    const from = deps.updatedFrom === undefined ? updatedFrom() : deps.updatedFrom;
    this.justUpdatedFrom = from && notesRange(from, deps.version) ? from : null;
    // Kept after probation, for Settings' What's new in this version.
    if (this.justUpdatedFrom) deps.kvSet(KV_LAST_FROM, this.justUpdatedFrom);
  }

  status(): UpdateStatus {
    return { current: this.deps.version, justUpdatedFrom: this.justUpdatedFrom, lastUpdatedFrom: this.lastUpdatedFrom() };
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
    const src = this.deps.source ?? (await defaultCompareSource());
    let c: Comparison | null = null;
    let last: unknown;
    // Each version's tag has one of TAG_PREFIXES, newest scheme first: try them in that order.
    outer: for (const a of TAG_PREFIXES) {
      for (const b of TAG_PREFIXES) {
        try {
          c = await src.compare(RELEASES_REPO, `${a}${from}`, `${b}${to}`);
          break outer;
        } catch (e) {
          last = e;
        }
      }
    }
    if (!c) {
      const why = ((last as { stderr?: string }).stderr || (last as Error).message).trim();
      throw new Error(`GitHub couldn't compare ${from} and ${to}: ${why}`, { cause: last });
    }
    return {
      from,
      to,
      sections: releaseNotes([...c.commits].reverse(), RELEASES_REPO),
      commits: c.commits.length,
      totalCommits: c.totalCommits,
      compareUrl: c.url,
    };
  }
}
