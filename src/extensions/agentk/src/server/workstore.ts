// The work queue's items and links in Postgres: the AMP database in Meko Cloud that every computer of the
// user's reads (docs/design/work-queue.md#where-its-kept). Queries only: the rules, the sync and the
// in-memory list are WorkQueue's (workqueue.ts), and finding or making the database is WorkCloud's (workcloud.ts).
import pg from 'pg';
import { moveItem } from '../shared/workorder.js';
import type { WorkItemSource, WorkLink, WorkMove } from '../shared/types.js';
import { HttpError } from './projects.js';

/** The version of the `kode` schema this Kode writes. A newer one in the database makes the queue read-only here. */
export const WORK_SCHEMA_VERSION = 1;

/** A connection to the queue's database: Postgres over pg's pool here, PGlite in the tests. */
export interface WorkSql {
  query<T = Record<string, unknown>>(sql: string, params?: unknown[]): Promise<T[]>;
  /** Runs `fn` in one transaction on one connection, rolled back when it throws. */
  tx<T>(fn: (q: WorkSql) => Promise<T>): Promise<T>;
  close(): Promise<void>;
}

/** Where the database is and how to log in (the connection record's part a connection needs). */
export interface WorkConn {
  host: string;
  port: number;
  database: string;
  user: string;
  password: string;
}

/**
 * Connects over TLS with the server's certificate checked, and channel binding when the server offers it.
 * The pool connects when a query needs it and closes a connection after 30 s unused, so the database's
 * compute can scale to zero between refreshes (D3).
 */
export function pgSql(c: WorkConn): WorkSql {
  const pool = new pg.Pool({
    host: c.host, port: c.port, database: c.database, user: c.user, password: c.password,
    ssl: { rejectUnauthorized: true, servername: c.host },
    enableChannelBinding: true,
    max: 4,
    idleTimeoutMillis: 30_000,
    // A compute that scaled to zero takes a few seconds to wake.
    connectionTimeoutMillis: 20_000,
    query_timeout: 30_000,
  });
  // An idle connection the server drops errors on the pool; the next query connects again.
  pool.on('error', () => undefined);
  const over = (run: (sql: string, params?: unknown[]) => Promise<pg.QueryResult>): Pick<WorkSql, 'query'> => ({
    async query<T>(sql: string, params?: unknown[]) {
      return (await run(sql, params)).rows as T[];
    },
  });
  return {
    ...over((sql, params) => pool.query(sql, params)),
    async tx<T>(fn: (q: WorkSql) => Promise<T>): Promise<T> {
      const client = await pool.connect();
      try {
        await client.query('BEGIN');
        const q: WorkSql = { ...over((sql, params) => client.query(sql, params)), tx: (f) => f(q), close: async () => undefined };
        const r = await fn(q);
        await client.query('COMMIT');
        return r;
      } catch (e) {
        await client.query('ROLLBACK').catch(() => undefined);
        throw e;
      } finally {
        client.release();
      }
    },
    close: () => pool.end(),
  };
}

/**
 * Whether an error means the database couldn't be reached (or let Kode in), rather than a query that's
 * wrong: a connection, auth, resource or shutdown SQLSTATE, or a socket, DNS, TLS or timeout error.
 */
export function unreachable(e: unknown): boolean {
  if (e instanceof HttpError) return false;
  const code = (e as { code?: unknown } | null)?.code;
  if (typeof code === 'string' && /^[0-9A-DF-Z][0-9A-Z]{4}$/.test(code)) return ['08', '28', '53', '57', '3D'].includes(code.slice(0, 2));
  // ECONNREFUSED, ETIMEDOUT, ENOTFOUND, CERT_HAS_EXPIRED, ERR_TLS_CERT_ALTNAME_INVALID…
  if (typeof code === 'string' && code) return true;
  return /timeout|timed out|terminated|connect|socket|ssl|tls|certificate/i.test(e instanceof Error ? e.message : String(e));
}

/** The schema and kode.meta first: migrate() reads the version before it makes the rest. */
const SCHEMA = [
  'CREATE SCHEMA IF NOT EXISTS kode',
  'CREATE TABLE IF NOT EXISTS kode.meta (key text PRIMARY KEY, value text NOT NULL)',
  `CREATE TABLE IF NOT EXISTS kode.work_items (
     id uuid PRIMARY KEY,
     title text NOT NULL,
     notes text NOT NULL DEFAULT '',
     position integer NOT NULL,
     status text NOT NULL DEFAULT 'open',
     source text NOT NULL DEFAULT 'manual',
     source_key text UNIQUE,
     unplaced boolean NOT NULL DEFAULT false,
     github_done_at bigint,
     created_at bigint NOT NULL,
     closed_at bigint,
     updated_at bigint NOT NULL)`,
  `CREATE TABLE IF NOT EXISTS kode.work_item_links (
     item_id uuid NOT NULL REFERENCES kode.work_items(id) ON DELETE CASCADE,
     ord integer NOT NULL,
     key text NOT NULL,
     kind text NOT NULL,
     url text,
     label text NOT NULL,
     PRIMARY KEY (item_id, key))`,
  'CREATE INDEX IF NOT EXISTS work_item_links_key ON kode.work_item_links(key)',
  'CREATE INDEX IF NOT EXISTS work_items_open ON kode.work_items(status, position)',
];

/** What the database keeps of an item (its session is on this computer: work_item_sessions). */
export interface StoredItem {
  id: string;
  title: string;
  notes: string;
  links: WorkLink[];
  position: number;
  status: 'open' | 'closed';
  source: WorkItemSource;
  sourceKey: string | null;
  unplaced: boolean;
  githubDoneAt: number | null;
  createdAt: number;
  closedAt: number | null;
}

interface ItemRow {
  id: string;
  title: string;
  notes: string;
  position: number;
  status: 'open' | 'closed';
  source: WorkItemSource;
  source_key: string | null;
  unplaced: boolean;
  github_done_at: string | number | null;
  created_at: string | number;
  closed_at: string | number | null;
}

interface LinkRow {
  item_id: string;
  key: string;
  kind: WorkLink['kind'];
  url: string | null;
  label: string;
}

/** bigint comes back as text from pg (and as a number from PGlite). */
const num = (v: string | number | null): number | null => (v == null ? null : Number(v));

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
/** An item id the database can look up: anything else is no item (a uuid column refuses it with an error). */
export const isItemId = (id: unknown): id is string => typeof id === 'string' && UUID.test(id);

/** The queue's queries, on one database. Every write sets updated_at. */
export class AmpWorkStore {
  constructor(readonly sql: WorkSql) {}

  /**
   * Makes the `kode` schema, or brings it up to this version, under a lock so two computers connecting at
   * once don't race. True when the database is a newer Kode's: it's read, and not written, here.
   */
  async migrate(): Promise<{ readOnly: boolean }> {
    return this.sql.tx(async (q) => {
      await q.query('SELECT pg_advisory_xact_lock(7343060201)');
      // The version first: a newer Kode's tables are left exactly as they are, not made again as this one knows them.
      for (const s of SCHEMA.slice(0, 2)) await q.query(s);
      const v = Number((await q.query<{ value: string }>("SELECT value FROM kode.meta WHERE key = 'schema_version'"))[0]?.value ?? 0);
      if (v > WORK_SCHEMA_VERSION) return { readOnly: true };
      for (const s of SCHEMA.slice(2)) await q.query(s);
      if (v < WORK_SCHEMA_VERSION) {
        await q.query(`INSERT INTO kode.meta(key, value) VALUES ('schema_version', $1)
                       ON CONFLICT (key) DO UPDATE SET value = excluded.value`, [String(WORK_SCHEMA_VERSION)]);
      }
      return { readOnly: false };
    });
  }

  private async withLinks(q: WorkSql, rows: ItemRow[]): Promise<StoredItem[]> {
    if (!rows.length) return [];
    const links = await q.query<LinkRow>('SELECT item_id, key, kind, url, label FROM kode.work_item_links WHERE item_id = ANY($1::uuid[]) ORDER BY ord',
      [rows.map((r) => r.id)]);
    const by = new Map<string, WorkLink[]>();
    for (const l of links) {
      const list = by.get(l.item_id) ?? [];
      list.push({ kind: l.kind, key: l.key, url: l.url, label: l.label });
      by.set(l.item_id, list);
    }
    return rows.map((r) => ({
      id: r.id, title: r.title, notes: r.notes, links: by.get(r.id) ?? [], position: r.position, status: r.status, source: r.source,
      sourceKey: r.source_key, unplaced: r.unplaced, githubDoneAt: num(r.github_done_at), createdAt: num(r.created_at)!, closedAt: num(r.closed_at),
    }));
  }

  /** The open items, in the queue's order. */
  async open(): Promise<StoredItem[]> {
    return this.withLinks(this.sql, await this.sql.query<ItemRow>("SELECT * FROM kode.work_items WHERE status = 'open' ORDER BY position, created_at"));
  }

  /** The closed items, last closed first. */
  async closed(limit: number): Promise<StoredItem[]> {
    return this.withLinks(this.sql, await this.sql.query<ItemRow>(
      "SELECT * FROM kode.work_items WHERE status = 'closed' ORDER BY closed_at DESC LIMIT $1", [limit]));
  }

  async get(id: string): Promise<StoredItem | null> {
    if (!isItemId(id)) return null;
    return (await this.withLinks(this.sql, await this.sql.query<ItemRow>('SELECT * FROM kode.work_items WHERE id = $1', [id])))[0] ?? null;
  }

  /** The items holding a link key, open first, then last closed. */
  async byLink(key: string, limit: number): Promise<StoredItem[]> {
    return this.withLinks(this.sql, await this.sql.query<ItemRow>(
      `SELECT w.* FROM kode.work_items w JOIN kode.work_item_links l ON l.item_id = w.id WHERE l.key = $1
        ORDER BY w.status = 'closed', w.closed_at DESC, w.position LIMIT $2`, [key, limit]));
  }

  /** Whether an item (open or closed) already has this key: synced from it, or holding it as a link. */
  async seen(key: string): Promise<boolean> {
    return (await this.sql.query(
      'SELECT 1 FROM kode.work_items WHERE source_key = $1 UNION ALL SELECT 1 FROM kode.work_item_links WHERE key = $1 LIMIT 1', [key])).length > 0;
  }

  /** The item a sync made from this source (a PR, a Slack message), or null. */
  async idBySource(key: string): Promise<string | null> {
    return (await this.sql.query<{ id: string }>('SELECT id FROM kode.work_items WHERE source_key = $1', [key]))[0]?.id ?? null;
  }

  private async writeLinks(q: WorkSql, id: string, links: WorkLink[]): Promise<void> {
    await q.query('DELETE FROM kode.work_item_links WHERE item_id = $1', [id]);
    for (const [i, l] of links.entries()) {
      await q.query('INSERT INTO kode.work_item_links(item_id, ord, key, kind, url, label) VALUES ($1, $2, $3, $4, $5, $6)', [id, i, l.key, l.kind, l.url, l.label]);
    }
  }

  /**
   * Adds an item at the bottom of the open ones. False when one with its id or `sourceKey` is there already
   * (another computer synced the same PR meanwhile): nothing is written then.
   */
  async insert(o: {
    id: string; title: string; notes: string; links: WorkLink[]; source: WorkItemSource; sourceKey: string | null; unplaced: boolean;
    createdAt: number; status?: 'open' | 'closed'; closedAt?: number | null; githubDoneAt?: number | null;
  }): Promise<boolean> {
    return this.sql.tx(async (q) => {
      const added = await q.query(`INSERT INTO kode.work_items(id, title, notes, position, status, source, source_key, unplaced, github_done_at, created_at, closed_at, updated_at)
        VALUES ($1, $2, $3, (SELECT COALESCE(MAX(position), 0) + 1 FROM kode.work_items WHERE status = 'open'), $4, $5, $6, $7, $8, $9, $10, $11)
        ON CONFLICT DO NOTHING RETURNING id`,
      [o.id, o.title, o.notes, o.status ?? 'open', o.source, o.sourceKey, o.unplaced, o.githubDoneAt ?? null, o.createdAt, o.closedAt ?? null, Date.now()]);
      if (!added.length) return false;
      await this.writeLinks(q, o.id, o.links);
      return true;
    });
  }

  /** Changes an item: what's given, in one transaction. 404 when it's gone. */
  async update(id: string, c: {
    title?: string; notes?: string; links?: WorkLink[]; status?: 'open' | 'closed'; move?: WorkMove; placed?: boolean;
  }): Promise<void> {
    await this.sql.tx(async (q) => {
      const r = (await q.query<{ status: string }>('SELECT status FROM kode.work_items WHERE id = $1 FOR UPDATE', [id]))[0];
      if (!r) throw new HttpError(404, 'That work item no longer exists.');
      const now = Date.now();
      if (c.title != null) await q.query('UPDATE kode.work_items SET title = $2 WHERE id = $1', [id, c.title]);
      if (c.notes != null) await q.query('UPDATE kode.work_items SET notes = $2 WHERE id = $1', [id, c.notes]);
      if (c.links) await this.writeLinks(q, id, c.links);
      if (c.status && c.status !== r.status) {
        if (c.status === 'closed') await q.query("UPDATE kode.work_items SET status = 'closed', closed_at = $2, unplaced = false WHERE id = $1", [id, now]);
        else await q.query(`UPDATE kode.work_items SET status = 'open', closed_at = NULL,
                              position = (SELECT COALESCE(MAX(position), 0) + 1 FROM kode.work_items WHERE status = 'open') WHERE id = $1`, [id]);
      }
      if (c.move) {
        const ids = (await q.query<{ id: string }>("SELECT id FROM kode.work_items WHERE status = 'open' ORDER BY position, created_at FOR UPDATE")).map((x) => x.id);
        if (ids.includes(id)) await this.writeOrder(q, moveItem(ids, id, c.move));
      }
      if (c.move || c.placed) await q.query('UPDATE kode.work_items SET unplaced = false WHERE id = $1', [id]);
      await q.query('UPDATE kode.work_items SET updated_at = $2 WHERE id = $1', [id, now]);
    });
  }

  private async writeOrder(q: WorkSql, ids: string[]): Promise<void> {
    if (!ids.length) return;
    await q.query(`UPDATE kode.work_items w SET position = o.n, updated_at = $2
                     FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, n) WHERE w.id = o.id AND w.position IS DISTINCT FROM o.n`,
    [ids, Date.now()]);
  }

  /** Puts the open items in this order: 409 when `ids` isn't exactly them (one was added or closed meanwhile, here or elsewhere). */
  async reorder(ids: string[], moved: string | null): Promise<void> {
    await this.sql.tx(async (q) => {
      const open = new Set((await q.query<{ id: string }>("SELECT id FROM kode.work_items WHERE status = 'open' FOR UPDATE")).map((x) => x.id));
      if (ids.length !== open.size || new Set(ids).size !== ids.length || !ids.every((x) => open.has(x))) {
        throw new HttpError(409, 'The work queue changed meanwhile. Try again on the queue as it is now.');
      }
      await this.writeOrder(q, ids);
      if (moved && isItemId(moved)) await q.query('UPDATE kode.work_items SET unplaced = false WHERE id = $1', [moved]);
    });
  }

  /** Closes the open items among `ids`: how many it closed. */
  async close(ids: string[]): Promise<number> {
    const valid = [...new Set(ids)].filter(isItemId);
    if (!valid.length) return 0;
    const now = Date.now();
    return (await this.sql.query(`UPDATE kode.work_items SET status = 'closed', closed_at = $2, unplaced = false, updated_at = $2
                                    WHERE id = ANY($1::uuid[]) AND status = 'open' RETURNING id`, [valid, now])).length;
  }

  /** The open review requests a GitHub sync added, with whether each is marked done. */
  async githubOpen(): Promise<{ id: string; sourceKey: string; done: boolean }[]> {
    return (await this.sql.query<{ id: string; source_key: string; github_done_at: unknown }>(
      "SELECT id, source_key, github_done_at FROM kode.work_items WHERE source = 'github' AND status = 'open' AND source_key IS NOT NULL",
    )).map((r) => ({ id: r.id, sourceKey: r.source_key, done: r.github_done_at != null }));
  }

  async setGithubDone(id: string, at: number | null): Promise<void> {
    await this.sql.query('UPDATE kode.work_items SET github_done_at = $2, updated_at = $3 WHERE id = $1', [id, at, Date.now()]);
  }
}
