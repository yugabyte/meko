-- Fast version of the corruption-repro shape: enough secondary-index
-- churn to drive btree page splits (`Btree/INSERT_UPPER` WAL records)
-- without the 2M-row cost. Same code paths that produced the
-- 2026-06-02 walredo PANIC; this finishes in ~30 s and is the
-- regression check for the materialiser flush-horizon invariant.
--
-- Output-validated (#19): every value below is a deterministic
-- function of the row's serial id, so a golden `.expected` capture
-- diffs cleanly run-to-run. `indexed_val` uses a Knuth multiplicative
-- hash of `g` instead of `random()` — it still scatters keys across
-- the index (so page splits still happen) but is reproducible. The
-- UPDATE pass selects a fixed `id % 10` slice instead of TABLESAMPLE.

\timing on
DROP TABLE IF EXISTS bt_splits;
CREATE TABLE bt_splits (
    id          BIGSERIAL PRIMARY KEY,
    indexed_val INT  NOT NULL,
    payload     TEXT NOT NULL
);

INSERT INTO bt_splits (indexed_val, payload)
SELECT ((g::bigint * 2654435761) % 1000000)::int,
       repeat(md5(g::text), 4)
  FROM generate_series(1, 200000) g;

-- Index build is where INSERT_UPPER records show up.
CREATE INDEX bt_splits_idx ON bt_splits (indexed_val);

-- Post-insert verification: row count, value checksum, and a digest
-- over ordered ids. A dropped/stale page or a lineage read that loses
-- a delta moves at least one of these.
SELECT count(*) AS n, sum(indexed_val) AS val_sum, sum(length(payload)) AS payload_bytes
  FROM bt_splits;
SELECT md5(string_agg(indexed_val::text, ',' ORDER BY id)) AS val_digest FROM bt_splits;

-- Small UPDATE pass to grow per-key chains across post-index pages.
-- Fixed 10% slice (id % 10 = 0) so the +1 bump is reproducible.
UPDATE bt_splits
   SET indexed_val = indexed_val + 1
 WHERE id % 10 = 0;

-- Post-update verification: val_sum must rise by exactly the number of
-- bumped rows; the digest must change in lock-step.
SELECT count(*) AS n, sum(indexed_val) AS val_sum FROM bt_splits;
SELECT count(*) AS bumped FROM bt_splits WHERE id % 10 = 0;
SELECT md5(string_agg(indexed_val::text, ',' ORDER BY id)) AS val_digest FROM bt_splits;

-- Index range scans — zero-page reads would surface here.
SELECT count(*) FROM bt_splits WHERE indexed_val BETWEEN 100 AND 200;
SELECT count(*) FROM bt_splits WHERE indexed_val BETWEEN 500000 AND 500500;
