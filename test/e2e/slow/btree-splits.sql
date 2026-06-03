-- Fast version of the corruption-repro shape: enough secondary-index
-- churn to drive btree page splits (`Btree/INSERT_UPPER` WAL records)
-- without the 2M-row cost. Same code paths that produced the
-- 2026-06-02 walredo PANIC; this finishes in ~30 s and is the
-- regression check for the materialiser flush-horizon invariant.

\timing on
DROP TABLE IF EXISTS bt_splits;
CREATE TABLE bt_splits (
    id          BIGSERIAL PRIMARY KEY,
    indexed_val INT  NOT NULL,
    payload     TEXT NOT NULL
);

INSERT INTO bt_splits (indexed_val, payload)
SELECT (random() * 1000000)::int,
       repeat(md5(g::text), 4)
  FROM generate_series(1, 200000) g;

-- Index build is where INSERT_UPPER records show up.
CREATE INDEX bt_splits_idx ON bt_splits (indexed_val);

-- Small UPDATE pass to grow per-key chains across post-index pages.
UPDATE bt_splits
   SET indexed_val = indexed_val + 1
 WHERE id IN (SELECT id FROM bt_splits TABLESAMPLE SYSTEM (10));

-- Index range scans — zero-page reads would surface here.
SELECT count(*) FROM bt_splits WHERE indexed_val BETWEEN 100 AND 200;
SELECT count(*) FROM bt_splits WHERE indexed_val BETWEEN 500000 AND 500500;
