-- GIN index WAL redo through the pageserver, including XLOG_GIN_VACUUM_PAGE.
-- Build a GIN index, delete half the rows, VACUUM to emit GIN vacuum WAL,
-- then drop the buffer cache and query via the index so the reconstructed
-- GIN pages are walredo-applied on read. Ported from Neon's test_gin_redo.
CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;

CREATE TABLE gin_test_tbl (id integer, i int4[]);
CREATE INDEX gin_test_idx ON gin_test_tbl USING gin (i);
INSERT INTO gin_test_tbl SELECT g, array[3, 1, g] FROM generate_series(1, 10000) g;
DELETE FROM gin_test_tbl WHERE id % 2 = 0;
VACUUM gin_test_tbl;

SELECT clear_buffer_cache();
SET enable_seqscan = off;
SELECT count(*) FROM gin_test_tbl WHERE i @> array[1];
SELECT count(*) FROM gin_test_tbl WHERE i @> array[3];
SELECT id FROM gin_test_tbl WHERE i @> array[9999] ORDER BY id;
RESET enable_seqscan;

DROP TABLE gin_test_tbl;
