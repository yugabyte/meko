import { createHash } from 'node:crypto';
import { existsSync, realpathSync } from 'node:fs';
import { basename, join } from 'node:path';
import type { Core } from './core.js';
import { all, get, run } from './db.js';
import { detachWorktree, isIdleWorktree, listWorktrees, removeWorktree } from './git.js';
import { errMsg } from './log.js';
import { slugify, type SessionRow } from './store.js';

/** The most idle worktrees a repo may keep, whatever config.toml says. */
export const MAX_IDLE_PER_REPO = 20;
/** The longest an unused idle worktree may be kept before it's removed, in days. */
export const MAX_IDLE_DAYS = 365;
const DAY = 24 * 60 * 60 * 1000;

interface WorktreeRow {
  path: string;
  repo: string;
  session_id: string | null;
  released_at: number | null;
  stale: number;
}

/** The pool key of a repo: its real path, so projects that reach it by other paths share one pool. */
export function repoKey(repo: string): string {
  try {
    return realpathSync(repo);
  } catch {
    return repo;
  }
}

/**
 * The folder a repo's new worktrees go in: `<repo folder name>.<hash>`. Project ids are only
 * letters, digits and dashes, so the dot keeps it apart from the older per-project folders, and
 * the hash keeps apart two repos with the same folder name.
 */
export function repoFolder(worktreesDir: string, key: string): string {
  return join(worktreesDir, `${slugify(basename(key))}.${createHash('sha256').update(key).digest('hex').slice(0, 8)}`);
}

/**
 * Session worktrees, and the idle ones that wait for the next session on their repo with their
 * build outputs intact. Idle worktrees belong to the repo, not a project: every project on the
 * repo takes from and gives back to the same pool. A worktree in use belongs to one session.
 * Claims are single synchronous SQLite writes, so two sessions never get the same worktree, and
 * removal forgets an idle worktree in one write that fails once it has been claimed.
 *
 * A worktree git won't remove (it has files git doesn't ignore) stays recorded as stale: never
 * handed out, never forgotten while its folder exists, and reusable again once it's clean.
 */
export class WorktreePool {
  constructor(private core: Core) {}

  private get db() {
    return this.core.db;
  }

  /** How many idle worktrees each repo keeps (0: remove them when sessions end). */
  cap(): number {
    return clampInt(this.core.config.worktrees.idlePerRepo, MAX_IDLE_PER_REPO);
  }

  /** Days an idle worktree may go unused before it's removed (0: kept until trimmed). */
  idleDays(): number {
    return clampInt(this.core.config.worktrees.idleDays, MAX_IDLE_DAYS);
  }

  /**
   * Hands the repo's most recently idled worktree to a session, or returns null. Synchronous, so
   * it can run inside the transaction that adds the session.
   */
  claim(repo: string, sessionId: string): string | null {
    if (this.cap() === 0) return null;
    const r = get<{ path: string }>(this.db,
      `UPDATE worktrees SET session_id = ?
        WHERE path = (SELECT path FROM worktrees WHERE repo = ? AND session_id IS NULL AND stale = 0
                      ORDER BY released_at DESC LIMIT 1)
        RETURNING path`, sessionId, repoKey(repo));
    return r?.path ?? null;
  }

  /** Gives a claimed worktree back to the pool as it was (its reuse failed for another reason). */
  unclaim(path: string, sessionId: string): void {
    run(this.db, 'UPDATE worktrees SET session_id = NULL WHERE path = ? AND session_id = ?', path, sessionId);
  }

  /** A path for a new worktree of the repo (`wt-<n>`, lowest free n), recorded as the session's. */
  add(repo: string, sessionId: string): string {
    const key = repoKey(repo);
    const dir = repoFolder(this.core.paths.worktrees, key);
    for (let n = 1; ; n++) {
      const path = join(dir, `wt-${n}`);
      if (existsSync(path) || get(this.db, 'SELECT 1 AS x FROM worktrees WHERE path = ?', path)) continue;
      run(this.db, 'INSERT INTO worktrees(path, repo, session_id, created_at) VALUES (?, ?, ?, ?)', path, key, sessionId, Date.now());
      return path;
    }
  }

  /** Forgets a worktree (removed, or its folder is gone). */
  drop(path: string): void {
    run(this.db, 'DELETE FROM worktrees WHERE path = ?', path);
  }

  /**
   * True while the session's worktree is its own. False once the session let go of it (idle,
   * another session's, or removed). A worktree from before the pool (no row) is the session's
   * while it exists.
   */
  owns(row: SessionRow): boolean {
    if (row.worktree_released) return false;
    const r = get<WorktreeRow>(this.db, 'SELECT * FROM worktrees WHERE path = ?', row.worktree_path);
    return r ? r.session_id === row.id : existsSync(row.worktree_path);
  }

  /** Records that a session let go of its worktree, so nothing reads or removes it as the session's again. */
  released(sessionId: string): void {
    run(this.db, 'UPDATE sessions SET worktree_released = 1 WHERE id = ?', sessionId);
  }

  /**
   * Makes an ending session's worktree idle instead of removing it: HEAD detached (the branch is
   * free to delete), ignored files kept. Then trims the repo to the cap. The caller has checked
   * that the worktree exists and is clean. False, with nothing changed, when it should be removed
   * as before: the cap is 0, or the pool doesn't know it as the session's.
   */
  async park(row: SessionRow, repo: string): Promise<boolean> {
    const w = get<WorktreeRow>(this.db, 'SELECT * FROM worktrees WHERE path = ?', row.worktree_path);
    if (this.cap() === 0 || w?.session_id !== row.id) return false;
    if (!(await detachWorktree(row.worktree_path))) return false;
    run(this.db, 'UPDATE worktrees SET session_id = NULL, released_at = ?, stale = 0 WHERE path = ? AND session_id = ?',
      Date.now(), row.worktree_path, row.id);
    await this.trim(repo);
    return true;
  }

  /** Removes the repo's idle worktrees beyond <keep> (the cap), longest idle first. */
  async trim(repo: string, keep = this.cap()): Promise<void> {
    const idle = all<WorktreeRow>(this.db,
      'SELECT * FROM worktrees WHERE repo = ? AND session_id IS NULL AND stale = 0 ORDER BY released_at DESC', repoKey(repo));
    for (const w of idle.slice(keep)) await this.remove(w);
  }

  /** Trims every repo to the cap (after the setting is lowered). */
  async trimAll(): Promise<void> {
    const repos = all<{ repo: string }>(this.db, 'SELECT DISTINCT repo FROM worktrees WHERE session_id IS NULL AND stale = 0');
    for (const { repo } of repos) await this.trim(repo);
  }

  /** Removes idle worktrees no session has used for the configured number of days. */
  async expire(now = Date.now()): Promise<void> {
    const days = this.idleDays();
    if (!days) return;
    const old = all<WorktreeRow>(this.db,
      'SELECT * FROM worktrees WHERE session_id IS NULL AND stale = 0 AND released_at < ?', now - days * DAY);
    for (const w of old) {
      if (await this.remove(w)) this.core.log.info(`removed the idle worktree at ${w.path}: no session used it for ${days} days`);
    }
  }

  /**
   * Removes the repo's idle worktrees when no live project other than <exceptProject> uses the
   * repo (the last project on it is being deleted).
   */
  async releaseRepo(repo: string, exceptProject: string): Promise<void> {
    const key = repoKey(repo);
    const others = all<{ repo_path: string }>(this.db, 'SELECT repo_path FROM projects WHERE deleted_at IS NULL AND id != ?', exceptProject);
    if (others.some((p) => repoKey(p.repo_path) === key)) return;
    await this.trim(key, 0);
  }

  /**
   * At startup: re-keys rows by the repo's real path, forgets idle worktrees whose folder is gone,
   * marks stale the ones no longer clean and detached (and makes stale ones reusable again once
   * they are), removes those of repos no live project uses and those unused too long, and trims
   * to the cap. Idempotent, and safe while sessions start: every change to an idle row is
   * conditional on it still being idle.
   */
  async sweep(): Promise<void> {
    for (const w of all<WorktreeRow>(this.db, 'SELECT * FROM worktrees')) {
      const key = repoKey(w.repo);
      if (key !== w.repo) run(this.db, 'UPDATE worktrees SET repo = ? WHERE path = ?', key, w.path);
    }
    const live = new Set(all<{ repo_path: string }>(this.db, 'SELECT repo_path FROM projects WHERE deleted_at IS NULL').map((p) => repoKey(p.repo_path)));
    const idle = all<WorktreeRow>(this.db, 'SELECT * FROM worktrees WHERE session_id IS NULL');
    for (const repo of new Set(idle.map((w) => w.repo))) {
      const list = await listWorktrees(repo);
      for (const w of idle.filter((x) => x.repo === repo)) {
        if (!existsSync(w.path)) {
          run(this.db, 'DELETE FROM worktrees WHERE path = ? AND session_id IS NULL', w.path);
        } else if (!live.has(repo)) {
          await this.remove(w);
        } else {
          const usable = await isIdleWorktree(repo, w.path, list);
          if (usable === !w.stale) continue;
          run(this.db, 'UPDATE worktrees SET stale = ? WHERE path = ? AND session_id IS NULL', usable ? 0 : 1, w.path);
          if (!usable) this.core.log.info(`stopped reusing the worktree at ${w.path}: it is no longer clean and detached`);
        }
      }
    }
    await this.expire();
    await this.trimAll();
  }

  /**
   * Takes a claimed worktree that turned out not to be reusable out of the pool: removed if git
   * will (only ignored files left), else kept on disk as stale.
   */
  async discard(path: string, sessionId: string): Promise<void> {
    const w = get<WorktreeRow>(this.db, 'SELECT * FROM worktrees WHERE path = ? AND session_id = ?', path, sessionId);
    if (!w) return;
    this.unclaim(path, sessionId);
    await this.remove({ ...w, session_id: null });
  }

  /**
   * Removes an idle worktree without --force. The row goes first, in a write that does nothing
   * once a session has claimed the worktree (then it's left alone). If git refuses (files git
   * doesn't ignore), the folder stays and is recorded as stale. True when it was removed.
   */
  private async remove(w: WorktreeRow): Promise<boolean> {
    if (run(this.db, 'DELETE FROM worktrees WHERE path = ? AND session_id IS NULL', w.path).changes === 0) return false;
    let removed = false;
    try {
      removed = await removeWorktree(w.repo, w.path);
    } catch (e) {
      this.core.log.warn(`removing the idle worktree at ${w.path} failed`, errMsg(e));
    }
    if (!removed) {
      run(this.db, `INSERT OR IGNORE INTO worktrees(path, repo, session_id, released_at, stale, created_at) VALUES (?, ?, NULL, ?, 1, ?)`,
        w.path, w.repo, w.released_at, Date.now());
      this.core.log.info(`kept the idle worktree at ${w.path}: git wouldn't remove it (it has files git doesn't ignore); it isn't reused until it's clean`);
    }
    return removed;
  }
}

/** A whole number from 0 to max; anything else (not a number) is 0. */
function clampInt(v: unknown, max: number): number {
  const n = Math.floor(Number(v));
  return Number.isFinite(n) ? Math.max(0, Math.min(max, n)) : 0;
}
