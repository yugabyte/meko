import { DatabaseSync, type StatementSync } from 'node:sqlite';
import { mkdirSync, rmSync } from 'node:fs';
import { dirname } from 'node:path';

export type Db = DatabaseSync;
type Param = null | number | bigint | string | Uint8Array;
export type Row = Record<string, Param>;

/**
 * Schema migrations, applied in order. PRAGMA user_version records how many
 * have run. Never edit a shipped migration; append a new one.
 */
const MIGRATIONS: string[] = [
  `
  CREATE TABLE projects (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    repo_path TEXT NOT NULL,
    default_branch TEXT NOT NULL,
    position INTEGER NOT NULL DEFAULT 0,
    settings_json TEXT NOT NULL DEFAULT '{}',
    meko_conversation_id TEXT,
    created_at INTEGER NOT NULL,
    deleted_at INTEGER
  );
  CREATE UNIQUE INDEX projects_repo ON projects(repo_path) WHERE deleted_at IS NULL;

  CREATE TABLE project_datapacks (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    datapack_id TEXT NOT NULL,
    name TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('dedicated', 'shared')),
    PRIMARY KEY (project_id, datapack_id)
  );
  CREATE UNIQUE INDEX one_dedicated ON project_datapacks(project_id) WHERE role = 'dedicated';

  CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    slug TEXT NOT NULL,
    name TEXT NOT NULL,
    claude_session_id TEXT NOT NULL,
    transcript_path TEXT,
    branch TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    tmux_name TEXT NOT NULL,
    hook_token TEXT NOT NULL,
    desired TEXT NOT NULL CHECK (desired IN ('running', 'archived')),
    status TEXT NOT NULL,
    status_detail TEXT,
    stage TEXT NOT NULL,
    pr_number INTEGER,
    pr_url TEXT,
    review_state TEXT NOT NULL DEFAULT 'none',
    ci_state TEXT NOT NULL DEFAULT 'none',
    merge_state TEXT,
    seen_review_ids TEXT NOT NULL DEFAULT '[]',
    initial_prompt TEXT,
    auto_continue_at TEXT NOT NULL DEFAULT '[]',
    last_event_at INTEGER,
    created_at INTEGER NOT NULL,
    archived_at INTEGER,
    recovered_at INTEGER
  );
  CREATE UNIQUE INDEX sessions_slug ON sessions(project_id, slug);

  CREATE TABLE session_stages (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    stage TEXT NOT NULL,
    entered_at INTEGER NOT NULL,
    left_at INTEGER,
    moved_by TEXT NOT NULL
  );

  CREATE TABLE session_events (
    id INTEGER PRIMARY KEY,
    session_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    text TEXT,
    payload_json TEXT,
    at INTEGER NOT NULL
  );
  CREATE INDEX session_events_project ON session_events(project_id, at);

  CREATE TABLE designs (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    markdown TEXT NOT NULL,
    updated_at INTEGER NOT NULL
  );

  CREATE TABLE owner (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    username TEXT NOT NULL,
    pw_scrypt TEXT NOT NULL,
    totp_secret TEXT,
    totp_pending TEXT
  );

  CREATE TABLE auth_sessions (
    token_sha256 TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    user_agent TEXT,
    ip TEXT
  );

  CREATE TABLE audit_log (
    id INTEGER PRIMARY KEY,
    at INTEGER NOT NULL,
    action TEXT NOT NULL,
    detail_json TEXT
  );

  CREATE TABLE kv (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
  );

  CREATE TABLE stage_prompts (
    stage TEXT PRIMARY KEY,
    template TEXT NOT NULL
  );
  `,
  // 2: remember that Claude died mid-turn (its SessionEnd hook can fire as tmux dies).
  `ALTER TABLE sessions ADD COLUMN interrupted INTEGER NOT NULL DEFAULT 0;`,
  // 3: token usage, one row per model response, read incrementally from transcripts.
  `
  CREATE TABLE usage_messages (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    message_id TEXT NOT NULL,
    agent TEXT,
    model TEXT NOT NULL,
    at INTEGER NOT NULL,
    input INTEGER NOT NULL,
    output INTEGER NOT NULL,
    cache_read INTEGER NOT NULL,
    cache_write INTEGER NOT NULL,
    PRIMARY KEY (session_id, message_id)
  );
  CREATE INDEX usage_messages_at ON usage_messages(at);
  CREATE TABLE usage_files (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    offset INTEGER NOT NULL,
    PRIMARY KEY (session_id, path)
  );
  `,
  // 4: designs without plan mode. Sessions created before this keep plan mode for Design;
  // a design can be a published Artifact (its HTML and claude.ai link) instead of markdown.
  `
  ALTER TABLE sessions ADD COLUMN plan_mode INTEGER NOT NULL DEFAULT 1;
  ALTER TABLE designs ADD COLUMN html TEXT;
  ALTER TABLE designs ADD COLUMN artifact_url TEXT;
  `,
  // 5: review comments on a session's diff, sent to Claude in batches.
  `
  CREATE TABLE review_comments (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    side TEXT NOT NULL CHECK (side IN ('old', 'new')),
    line INTEGER NOT NULL,
    end_line INTEGER NOT NULL,
    excerpt TEXT NOT NULL,
    body TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'draft' CHECK (state IN ('draft', 'queued', 'sent')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    sent_at INTEGER
  );
  CREATE INDEX review_comments_session ON review_comments(session_id, state);
  `,
  // 6: several projects may share a repo.
  `DROP INDEX projects_repo;`,
  // 7: Merge and Cleanup became one stage, Merge & Close.
  `UPDATE sessions SET stage = 'merge' WHERE stage = 'cleanup';`,
];

export function openDb(path: string): Db {
  if (path !== ':memory:') mkdirSync(dirname(path), { recursive: true });
  const db = new DatabaseSync(path);
  db.exec(`
    PRAGMA journal_mode = WAL;
    PRAGMA synchronous = FULL;
    PRAGMA foreign_keys = ON;
    PRAGMA busy_timeout = 5000;
  `);
  migrate(db);
  return db;
}

export function schemaVersion(db: Db): number {
  const r = db.prepare('PRAGMA user_version').get() as { user_version: number };
  return r.user_version;
}

function migrate(db: Db): void {
  const current = schemaVersion(db);
  for (let i = current; i < MIGRATIONS.length; i++) {
    tx(db, () => {
      db.exec(MIGRATIONS[i]);
      db.exec(`PRAGMA user_version = ${i + 1}`);
    });
  }
}

export function tx<T>(db: Db, fn: () => T): T {
  db.exec('BEGIN IMMEDIATE');
  try {
    const out = fn();
    db.exec('COMMIT');
    return out;
  } catch (e) {
    db.exec('ROLLBACK');
    throw e;
  }
}

const stmtCache = new WeakMap<Db, Map<string, StatementSync>>();

function stmt(db: Db, sql: string): StatementSync {
  let m = stmtCache.get(db);
  if (!m) stmtCache.set(db, (m = new Map()));
  let s = m.get(sql);
  if (!s) m.set(sql, (s = db.prepare(sql)));
  return s;
}

export function all<T = Row>(db: Db, sql: string, ...params: Param[]): T[] {
  return stmt(db, sql).all(...params) as T[];
}

export function get<T = Row>(db: Db, sql: string, ...params: Param[]): T | undefined {
  return stmt(db, sql).get(...params) as T | undefined;
}

export function run(db: Db, sql: string, ...params: Param[]): { changes: number } {
  const r = stmt(db, sql).run(...params);
  return { changes: Number(r.changes) };
}

export function kvGet(db: Db, key: string): string | null {
  return get<{ value: string }>(db, 'SELECT value FROM kv WHERE key = ?', key)?.value ?? null;
}

export function kvSet(db: Db, key: string, value: string): void {
  run(db, 'INSERT INTO kv(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value', key, value);
}

export function kvDelete(db: Db, key: string): void {
  run(db, 'DELETE FROM kv WHERE key = ?', key);
}

export function audit(db: Db, action: string, detail?: unknown): void {
  run(db, 'INSERT INTO audit_log(at, action, detail_json) VALUES (?, ?, ?)', Date.now(), action,
    detail === undefined ? null : JSON.stringify(detail));
}

/** Consistent copy of the live database (used before updates). */
export function snapshotDb(db: Db, dest: string): void {
  rmSync(dest, { force: true }); // VACUUM INTO refuses to overwrite
  db.exec(`VACUUM INTO '${dest.replace(/'/g, "''")}'`);
}
