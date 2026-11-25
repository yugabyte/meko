/**
 * The runs of `meko kode review`, counted with Kode's other code reviews (`cli_reviews`). The CLI
 * never opens `state.db`: it could migrate it under an older server, or be a second writer on a
 * home shared over NFS. It leaves one owner-only JSON record per run in `run/cli-reviews/` instead
 * (`writeCliRecord`), and the server takes the records in (`ingestCliRecords`, at start and every
 * minute), deleting each once its row is in. Counts, not content: no finding's text and no
 * description. It does hold the repo's checkout path (how the server finds its project), its branch
 * and why a run failed, which stay on this computer: the usage report sends only the counts.
 */
import { readdirSync, readFileSync, realpathSync, renameSync, rmSync, statSync, writeFileSync, mkdirSync } from 'node:fs';
import { join } from 'node:path';
import { noCounts, type SeverityCounts } from '../../shared/codereview.js';
import { all, run, tx, type Db } from '../db.js';
import type { Paths } from '../paths.js';
import type { StepUsage } from './headless.js';

/** The record's version: a server takes the fields it knows of a newer one. */
export const CLI_RECORD_V = 1;

/** A record left this long (no server took it in) is deleted by the next run. */
export const CLI_RECORD_MAX_AGE_MS = 30 * 24 * 60 * 60 * 1000;

/**
 * A run's own folder left this long belongs to a run that was killed before it could remove it
 * (its `mcp.json` holds the Meko token): the next run deletes it. A live run is far younger.
 */
export const CLI_RUN_DIR_MAX_AGE_MS = 24 * 60 * 60 * 1000;

export type CliReviewState = 'done' | 'failed' | 'cancelled';

/** One run of `meko kode review`, as the CLI leaves it. */
export interface CliReviewRecord {
  v: number;
  id: string;
  /** The repo's GitHub `owner/name`, or its folder's name. */
  repo: string;
  /** The repo's main checkout, its real path, which the server matches to a project. */
  checkout: string;
  branch: string;
  base: string;
  strategy: string;
  depth: number;
  state: CliReviewState;
  /** Why it failed or stopped, or why there was nothing to review; null otherwise. */
  detail: string | null;
  /** `--pr` was given. */
  pr: boolean;
  /** Sweeps that ran: 0 when it stopped (or found nothing changed) before the passes. */
  sweeps: number;
  candidates: number;
  /** Kept findings by severity. */
  counts: SeverityCounts;
  usage: StepUsage;
  createdAt: number;
  finishedAt: number;
}

/** Where the records (and each run's own folder, while it runs) are. */
export function cliReviewsDir(paths: Paths): string {
  return join(paths.run, 'cli-reviews');
}

/** Leaves a run's record for the server, owner-only, renamed into place so the server never reads half of one. */
export function writeCliRecord(paths: Paths, rec: CliReviewRecord): void {
  const dir = cliReviewsDir(paths);
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const tmp = join(dir, `.${rec.id}.json.tmp`);
  writeFileSync(tmp, `${JSON.stringify(rec)}\n`, { mode: 0o600 });
  renameSync(tmp, join(dir, `${rec.id}.json`));
}

/**
 * Deletes records older than CLI_RECORD_MAX_AGE_MS (no server has taken them in), and run folders
 * older than CLI_RUN_DIR_MAX_AGE_MS (a run killed before it removed its own).
 */
export function sweepOldRecords(paths: Paths, now = Date.now()): void {
  const dir = cliReviewsDir(paths);
  let names: string[];
  try {
    names = readdirSync(dir);
  } catch {
    return;
  }
  for (const f of names) {
    const path = join(dir, f);
    try {
      const st = statSync(path);
      if (st.isDirectory() && f.startsWith('cli-') && now - st.mtimeMs > CLI_RUN_DIR_MAX_AGE_MS) rmSync(path, { recursive: true, force: true });
      else if (st.isFile() && f.endsWith('.json') && now - st.mtimeMs > CLI_RECORD_MAX_AGE_MS) rmSync(path, { force: true });
    } catch {
      /* gone already */
    }
  }
}

function listRecords(dir: string): string[] {
  try {
    return readdirSync(dir).filter((f) => f.endsWith('.json') && !f.startsWith('.'));
  } catch {
    return [];
  }
}

const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : 0);
const text = (v: unknown, max = 500) => (typeof v === 'string' ? v.slice(0, max) : '');

/** A record as read from its file, or null when it isn't one. */
export function readCliRecord(raw: string): CliReviewRecord | null {
  let v: Record<string, unknown>;
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null;
    v = parsed as Record<string, unknown>;
  } catch {
    return null;
  }
  const id = text(v.id, 100);
  const state = v.state === 'done' || v.state === 'failed' || v.state === 'cancelled' ? v.state : null;
  if (!/^cli-[a-z0-9-]+$/.test(id) || !state || typeof v.createdAt !== 'number') return null;
  const c = (v.counts && typeof v.counts === 'object' ? v.counts : {}) as Record<string, unknown>;
  const u = (v.usage && typeof v.usage === 'object' ? v.usage : {}) as Record<string, unknown>;
  return {
    v: num(v.v), id, repo: text(v.repo), checkout: text(v.checkout, 4096), branch: text(v.branch), base: text(v.base), strategy: text(v.strategy, 100),
    depth: num(v.depth), state, detail: typeof v.detail === 'string' ? v.detail.slice(0, 1000) : null, pr: v.pr === true, sweeps: num(v.sweeps),
    candidates: num(v.candidates), counts: { ...noCounts(), blocker: num(c.blocker), major: num(c.major), minor: num(c.minor) },
    usage: { input: num(u.input), output: num(u.output), cacheRead: num(u.cacheRead), cacheWrite: num(u.cacheWrite), costUsd: num(u.costUsd) },
    createdAt: num(v.createdAt), finishedAt: num(v.finishedAt) || num(v.createdAt),
  };
}

const real = (p: string): string => {
  try {
    return realpathSync(p);
  } catch {
    return p;
  }
};

/**
 * The project a run's checkout belongs to: the open project on that repo, else the open project
 * whose sessions worked in it most lately, else the closed project on it closed last (closing a
 * project keeps its row, and the Usage page still lists it); null for a repo no project, open or
 * closed, has used.
 */
export function projectForCheckout(db: Db, checkout: string): string | null {
  if (!checkout) return null;
  const want = real(checkout);
  const same = (p: string) => p === checkout || real(p) === want;
  for (const p of all<{ id: string; repo_path: string }>(db, 'SELECT id, repo_path FROM projects WHERE deleted_at IS NULL')) {
    if (same(p.repo_path)) return p.id;
  }
  const rows = all<{ project_id: string; repo_path: string }>(db,
    `SELECT s.project_id, s.repo_path FROM sessions s JOIN projects p ON p.id = s.project_id
     WHERE p.deleted_at IS NULL AND s.repo_path IS NOT NULL ORDER BY s.created_at DESC`);
  const seen = new Set<string>();
  for (const r of rows) {
    if (seen.has(r.repo_path)) continue;
    seen.add(r.repo_path);
    if (same(r.repo_path)) return r.project_id;
  }
  for (const p of all<{ id: string; repo_path: string }>(db, 'SELECT id, repo_path FROM projects WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC')) {
    if (same(p.repo_path)) return p.id;
  }
  return null;
}

/**
 * Takes every record in `run/cli-reviews/` into `cli_reviews` and deletes its file once its row is
 * in (by id, so a record read twice is one row). A file that isn't a record is deleted too, with a
 * warning. Returns how many it took in.
 */
export function ingestCliRecords(db: Db, paths: Paths, warn: (msg: string, detail?: string) => void): number {
  const dir = cliReviewsDir(paths);
  let n = 0;
  for (const f of listRecords(dir)) {
    const file = join(dir, f);
    let raw: string;
    try {
      raw = readFileSync(file, 'utf8');
    } catch {
      continue;
    }
    const rec = readCliRecord(raw);
    if (!rec) {
      warn(`a code review run's record couldn't be read, so it isn't counted: ${f}`);
      rmSync(file, { force: true });
      continue;
    }
    tx(db, () => run(db, `INSERT OR IGNORE INTO cli_reviews(id, project_id, repo, branch, base, strategy, depth, state, detail, pr, sweeps, candidates,
                            counts_json, input, output, cache_read, cache_write, cost_usd, created_at, finished_at)
                          VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    rec.id, projectForCheckout(db, rec.checkout), rec.repo, rec.branch, rec.base, rec.strategy, rec.depth, rec.state, rec.detail, rec.pr ? 1 : 0,
    rec.sweeps, rec.candidates, JSON.stringify(rec.counts), rec.usage.input, rec.usage.output, rec.usage.cacheRead, rec.usage.cacheWrite,
    rec.usage.costUsd, rec.createdAt, rec.finishedAt));
    rmSync(file, { force: true });
    n++;
  }
  return n;
}
