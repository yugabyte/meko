-- ybLight microbench
-- INSERT/UPDATE/DELETE: single run (cold).
-- SELECT: 3 runs each so we can pick the warm (median) number.
\set N 100000
\set Q 10000


\timing on


-- Reset and create three tables --------------------------------------------
DROP TABLE IF EXISTS heap_t;
DROP TABLE IF EXISTS pk_t;
DROP TABLE IF EXISTS idx_t;
CREATE TABLE heap_t (id int, v text, n int, ts timestamp);
CREATE TABLE pk_t   (id int PRIMARY KEY, v text, n int, ts timestamp);
CREATE TABLE idx_t  (id int PRIMARY KEY, v text, n int, ts timestamp);
CREATE INDEX idx_t_n ON idx_t(n);


-- Hot path: bind random() into a variable so the planner can use indexes.
-- ANALYZE after loads so query plans aren't pessimistic.
SET random_page_cost = 1.1;


-- ============================================================
\echo
\echo '=== bench_01_insert_heap_1M ==='
INSERT INTO heap_t (id, v, n, ts)
SELECT g, md5(g::text), (random()*1000)::int, now()
FROM generate_series(1, :N) g;


\echo
\echo '=== bench_02_insert_pk_1M ==='
INSERT INTO pk_t (id, v, n, ts)
SELECT g, md5(g::text), (random()*1000)::int, now()
FROM generate_series(1, :N) g;


\echo
\echo '=== bench_03_insert_idx_1M ==='
INSERT INTO idx_t (id, v, n, ts)
SELECT g, md5(g::text), (random()*1000)::int, now()
FROM generate_series(1, :N) g;


\echo
\echo '=== bench_03b_analyze ==='
ANALYZE heap_t;
ANALYZE pk_t;
ANALYZE idx_t;


-- ============================================================
-- SELECTS: each query repeated 3 times. The 1st is cold, 2nd/3rd are warm.
-- ============================================================


\echo
\echo '=== bench_04_count_heap r1 ==='
SELECT count(*) FROM heap_t;
\echo '=== bench_04_count_heap r2 ==='
SELECT count(*) FROM heap_t;
\echo '=== bench_04_count_heap r3 ==='
SELECT count(*) FROM heap_t;


\echo
\echo '=== bench_05_count_pk r1 ==='
SELECT count(*) FROM pk_t;
\echo '=== bench_05_count_pk r2 ==='
SELECT count(*) FROM pk_t;
\echo '=== bench_05_count_pk r3 ==='
SELECT count(*) FROM pk_t;


\echo
\echo '=== bench_06_seqscan_filter_heap r1 ==='
SELECT count(*) FROM heap_t WHERE n BETWEEN 100 AND 200;
\echo '=== bench_06_seqscan_filter_heap r2 ==='
SELECT count(*) FROM heap_t WHERE n BETWEEN 100 AND 200;
\echo '=== bench_06_seqscan_filter_heap r3 ==='
SELECT count(*) FROM heap_t WHERE n BETWEEN 100 AND 200;


\echo
\echo '=== bench_07_pk_point_lookup_10k r1 ==='
DO $$
DECLARE i int; tgt int; r record;
BEGIN
   FOR i IN 1..10000 LOOP
       tgt := (random()*99999+1)::int;
       SELECT * INTO r FROM pk_t WHERE id = tgt;
   END LOOP;
END $$;
\echo '=== bench_07_pk_point_lookup_10k r2 ==='
DO $$
DECLARE i int; tgt int; r record;
BEGIN
   FOR i IN 1..10000 LOOP
       tgt := (random()*99999+1)::int;
       SELECT * INTO r FROM pk_t WHERE id = tgt;
   END LOOP;
END $$;
\echo '=== bench_07_pk_point_lookup_10k r3 ==='
DO $$
DECLARE i int; tgt int; r record;
BEGIN
   FOR i IN 1..10000 LOOP
       tgt := (random()*99999+1)::int;
       SELECT * INTO r FROM pk_t WHERE id = tgt;
   END LOOP;
END $$;


\echo
\echo '=== bench_08_pk_range_scan r1 ==='
SELECT count(*) FROM pk_t WHERE id BETWEEN 25000 AND 26000;
\echo '=== bench_08_pk_range_scan r2 ==='
SELECT count(*) FROM pk_t WHERE id BETWEEN 25000 AND 26000;
\echo '=== bench_08_pk_range_scan r3 ==='
SELECT count(*) FROM pk_t WHERE id BETWEEN 25000 AND 26000;


\echo
\echo '=== bench_09_idx_range_scan r1 ==='
SELECT count(*), avg(n) FROM idx_t WHERE n BETWEEN 100 AND 110;
\echo '=== bench_09_idx_range_scan r2 ==='
SELECT count(*), avg(n) FROM idx_t WHERE n BETWEEN 100 AND 110;
\echo '=== bench_09_idx_range_scan r3 ==='
SELECT count(*), avg(n) FROM idx_t WHERE n BETWEEN 100 AND 110;


\echo
\echo '=== bench_10_groupby_agg r1 ==='
SELECT n%10 AS bucket, count(*), avg(length(v)) FROM heap_t GROUP BY 1 ORDER BY 1;
\echo '=== bench_10_groupby_agg r2 ==='
SELECT n%10 AS bucket, count(*), avg(length(v)) FROM heap_t GROUP BY 1 ORDER BY 1;
\echo '=== bench_10_groupby_agg r3 ==='
SELECT n%10 AS bucket, count(*), avg(length(v)) FROM heap_t GROUP BY 1 ORDER BY 1;


\echo
\echo '=== bench_11_update_1k ==='
UPDATE pk_t SET n = n+1 WHERE id BETWEEN 50000 AND 51000;


\echo
\echo '=== bench_12_delete_1k ==='
DELETE FROM heap_t WHERE id BETWEEN 0 AND 1000;


\echo
\echo '=== bench_13_vacuum_heap ==='
VACUUM (ANALYZE) heap_t;


\echo
\echo '=== bench_14_table_sizes ==='
SELECT relname,
      pg_size_pretty(pg_relation_size(c.oid)) AS table_size,
      pg_size_pretty(pg_total_relation_size(c.oid)) AS total_size
FROM pg_class c
WHERE relname IN ('heap_t','pk_t','idx_t','pk_t_pkey','idx_t_pkey','idx_t_n')
ORDER BY relname;
