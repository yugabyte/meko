-- Exercise the multixact offsets + members SLRUs through the pageserver.
-- A single backend can build one multixact per row: the top transaction
-- takes a FOR KEY SHARE lock on every row, then a per-row plpgsql
-- subtransaction adds a FOR SHARE lock, combining two member XIDs into a
-- MultiXactId on each tuple's xmax. 10k rows crosses SLRU page boundaries
-- on both the offsets (2048/page) and members SLRUs. After committing we
-- drop the buffer cache and read the rows back, forcing the heap pages
-- (with multixact xmax) and the multixact SLRU pages to be reconstructed
-- from YB and walredo-replayed. Ported from Neon's test_multixact.
CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;

CREATE TABLE mxact_t (id int PRIMARY KEY, n int);
INSERT INTO mxact_t SELECT g, 0 FROM generate_series(1, 10000) g;

DO $$
DECLARE
  i int;
BEGIN
  -- Top-level locker on every row.
  PERFORM 1 FROM mxact_t FOR KEY SHARE;
  -- Per-row subtransaction adds a second locker -> a 2-member multixact
  -- on each tuple's xmax.
  FOR i IN 1..10000 LOOP
    BEGIN
      PERFORM 1 FROM mxact_t WHERE id = i FOR SHARE;
    EXCEPTION WHEN OTHERS THEN
      RAISE EXCEPTION 'lock failed at row %', i;
    END;
  END LOOP;
END $$;

-- Force the heap + multixact SLRU pages to come back from the pageserver.
SELECT clear_buffer_cache();
SELECT count(*) FROM mxact_t;
SELECT count(*) FROM mxact_t WHERE n = 0;

DROP TABLE mxact_t;
