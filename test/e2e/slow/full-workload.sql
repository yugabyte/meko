-- Corruption-repro workload used to catch the 2026-06-02 Btree/INSERT_UPPER
-- walredo PANIC. Drives the materialiser through enough chain growth to
-- surface mid-flush partial-YB races: 2M-row INSERT (heap + index churn),
-- secondary-index build (page splits → INSERT_UPPER WAL records),
-- ~100k UPDATEs (chain growth on existing pages), then two range SELECTs
-- (zero-page reads would surface here).
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

-- 2M rows via generate_series with random payloads — large enough
-- to push many page versions through the WAL + materializer.
INSERT INTO big_table (payload, indexed_val)
SELECT
    repeat(md5(g::text), 4),
    (random() * 1000000)::int
FROM generate_series(1, 2000000) g;

CREATE INDEX big_idx ON big_table (indexed_val);

-- Some random updates to drive walredo over existing image pages
UPDATE big_table SET indexed_val = indexed_val + 1
 WHERE id IN (SELECT id FROM big_table TABLESAMPLE SYSTEM (5));

-- Force index reads (zero-page would surface here)
SELECT count(*) FROM big_table WHERE indexed_val BETWEEN 100 AND 200;
SELECT count(*) FROM big_table WHERE indexed_val BETWEEN 500000 AND 500500;
