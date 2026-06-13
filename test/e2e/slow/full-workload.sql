-- Corruption-repro workload used to catch the 2026-06-02 Btree/INSERT_UPPER
-- walredo PANIC. Drives the materialiser through enough chain growth to
-- surface mid-flush partial-YB races: 2M-row INSERT (heap + index churn),
-- secondary-index build (page splits → INSERT_UPPER WAL records),
-- ~100k UPDATEs (chain growth on existing pages), then two range SELECTs
-- (zero-page reads would surface here).
--
-- Output-validated (#19): `indexed_val` is a deterministic Knuth
-- multiplicative hash of the serial `g` (was `random()`), and the
-- UPDATE pass hits a fixed `id % 20` (5%) slice (was TABLESAMPLE), so
-- the aggregate/checksum footers below reproduce run-to-run and a
-- golden `.expected` capture can diff them. `created_at` defaults to
-- now() but is never projected, so it doesn't perturb the output.
--
-- Run via `test/repro/run-e2e.sh` for the full bring-up + verdict. Manual
-- usage against an already-running endpoint:
--   psql -h 127.0.0.1 -p 55432 -U cloud_admin postgres -f test/repro/workload.sql

\timing on
DROP TABLE IF EXISTS big_table;
CREATE TABLE big_table (
    id          BIGSERIAL PRIMARY KEY,
    payload     TEXT      NOT NULL,
    indexed_val INT       NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT now()
);

-- 2M rows via generate_series — large enough to push many page
-- versions through the WAL + materializer. Deterministic payloads
-- (md5 of g) and deterministic indexed_val (hash of g).
INSERT INTO big_table (payload, indexed_val)
SELECT
    repeat(md5(g::text), 4),
    ((g::bigint * 2654435761) % 1000000)::int
FROM generate_series(1, 2000000) g;

CREATE INDEX big_idx ON big_table (indexed_val);

-- Post-insert verification: count + value/byte checksums.
SELECT count(*) AS n, sum(indexed_val) AS val_sum, sum(length(payload)) AS payload_bytes
  FROM big_table;

-- Fixed 5% slice update to drive walredo over existing image pages.
UPDATE big_table SET indexed_val = indexed_val + 1
 WHERE id % 20 = 0;

-- Post-update verification: val_sum rises by exactly `bumped`.
SELECT count(*) AS n, sum(indexed_val) AS val_sum FROM big_table;
SELECT count(*) AS bumped FROM big_table WHERE id % 20 = 0;

-- Force index reads (zero-page would surface here)
SELECT count(*) FROM big_table WHERE indexed_val BETWEEN 100 AND 200;
SELECT count(*) FROM big_table WHERE indexed_val BETWEEN 500000 AND 500500;
