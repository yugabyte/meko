// The work queue's database in the tests: Postgres in this process (PGlite), in place of AMP in Meko Cloud.
import { PGlite, type Transaction } from '@electric-sql/pglite';
import type { WorkSql } from '../src/server/workstore.js';

type Querier = Pick<PGlite, 'query'> | Transaction;

function over(q: Querier): Pick<WorkSql, 'query'> {
  return { query: async <T>(sql: string, params?: unknown[]) => (await q.query<T>(sql, params as unknown[])).rows };
}

/**
 * A WorkSql on a PGlite database (a new one by default). Closing it leaves `db` open when it was given,
 * so a test can connect to the same database again, as a second computer, or after a restart.
 */
export function pgliteSql(db?: PGlite): WorkSql {
  const own = !db;
  const pg = db ?? new PGlite();
  return {
    ...over(pg),
    tx: (fn) => pg.transaction(async (t) => {
      const q: WorkSql = { ...over(t), tx: (f) => f(q), close: async () => undefined };
      return fn(q);
    }),
    close: async () => {
      if (own) await pg.close();
    },
  };
}

let shared: PGlite | null = null;

/**
 * One PGlite for a whole test file (starting one takes a second or more), emptied for each test: the
 * work queue's schema is dropped, so the next connection makes it again.
 */
export async function freshPglite(): Promise<PGlite> {
  shared ??= new PGlite();
  await shared.exec('DROP SCHEMA IF EXISTS kode CASCADE');
  return shared;
}
