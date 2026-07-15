-- GiST index WAL redo through the pageserver. Build a GiST index over a
-- geometric column, churn it with a delete, VACUUM to emit GiST vacuum
-- WAL, then drop the buffer cache and run index scans so the
-- reconstructed GiST pages are walredo-applied on read. Ported from Neon's
-- test_gist.
CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;

CREATE TABLE gist_pvactst (i int, a int[], p point) WITH (autovacuum_enabled = off);
INSERT INTO gist_pvactst SELECT i, array[1,2,3], point(i, i+1) FROM generate_series(1,1000) i;
CREATE INDEX gist_pvactst_idx ON gist_pvactst USING gist (p);
DELETE FROM gist_pvactst WHERE i % 2 = 0;
VACUUM gist_pvactst;

SELECT clear_buffer_cache();
SET enable_seqscan = off;
SELECT count(*) FROM gist_pvactst WHERE p <@ box(point(0,0), point(2000,2000));
SELECT i FROM gist_pvactst ORDER BY p <-> point(1,2) LIMIT 1;
RESET enable_seqscan;

DROP TABLE gist_pvactst;
