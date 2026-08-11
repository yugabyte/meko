-- btree_gin ships in the compute image for apps that build multi-column GIN
-- indexes mixing scalar (btree_gin) opclasses with pg_trgm trigram ops.
-- Build such an index, vacuum after a delete to emit GIN vacuum WAL, then
-- drop the buffer cache and query via the index so the reconstructed pages
-- are walredo-applied on read.
CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;
CREATE EXTENSION IF NOT EXISTS btree_gin;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE btree_gin_tbl (ns text, run_id integer, memo text);
CREATE INDEX btree_gin_idx ON btree_gin_tbl
    USING gin (ns, run_id, memo gin_trgm_ops);
INSERT INTO btree_gin_tbl
    SELECT 'ns-' || (g % 7), g, 'workflow memo ' || g
    FROM generate_series(1, 5000) g;
DELETE FROM btree_gin_tbl WHERE run_id % 2 = 0;
VACUUM btree_gin_tbl;

SELECT clear_buffer_cache();
SET enable_seqscan = off;
SELECT count(*) FROM btree_gin_tbl WHERE ns = 'ns-3';
SELECT count(*) FROM btree_gin_tbl WHERE run_id = 4999;
SELECT count(*) FROM btree_gin_tbl WHERE memo LIKE '%memo 499%';
RESET enable_seqscan;

DROP TABLE btree_gin_tbl;
-- Leave the shared suite database as found: later workloads (gin, gist, ...)
-- bake their CREATE EXTENSION output into goldens assuming this order.
DROP EXTENSION btree_gin;
DROP EXTENSION pg_trgm;
DROP EXTENSION yb_amp_test_utils;
