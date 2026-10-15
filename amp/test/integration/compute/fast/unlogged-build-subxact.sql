-- AMP-74: a GIN, GiST or SP-GiST build that fails inside a subtransaction must
-- not leave its build state behind for the next build or for COMMIT. Every
-- error below is a deliberate division by zero; stock Postgres prints the same.
\set ON_ERROR_STOP 0
SET client_min_messages = warning;
CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;
RESET client_min_messages;

CREATE TABLE ubsx_t (id int, a int[], t text, b box);
INSERT INTO ubsx_t
  SELECT g, ARRAY[g % 10, g], 'k' || g, box(point(g, g), point(g + 1, g + 1))
  FROM generate_series(1, 1000) g;

-- Each call that divides by zero aborts a subtransaction of its own.
CREATE FUNCTION ubsx_div(n int) RETURNS int[] LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
  RETURN ARRAY[100 / n];
EXCEPTION WHEN division_by_zero THEN
  RETURN ARRAY[-1000];
END $$;

-- A failed build, ROLLBACK TO, then another build.
BEGIN;
SAVEPOINT s1;
CREATE INDEX ubsx_bad1 ON ubsx_t USING gin ((ARRAY[100 / (id - 500)]));
ROLLBACK TO SAVEPOINT s1;
CREATE INDEX ubsx_gin ON ubsx_t USING gin (a);
COMMIT;

-- The same through a PL/pgSQL exception block.
DO $$
BEGIN
  BEGIN
    CREATE INDEX ubsx_bad2 ON ubsx_t USING spgist (((100 / (id - 500))::text));
  EXCEPTION WHEN division_by_zero THEN
    RAISE NOTICE 'spgist build failed';
  END;
  CREATE INDEX ubsx_spgist ON ubsx_t USING spgist (t);
END $$;

-- A failed build, ROLLBACK TO, COMMIT. GiST on box has no sortsupport, so it
-- takes the buffered build.
BEGIN;
SAVEPOINT s1;
CREATE INDEX ubsx_bad3 ON ubsx_t
  USING gist (box(point(id, 100 / (id - 500)), point(id, id)));
ROLLBACK TO SAVEPOINT s1;
COMMIT;
CREATE INDEX ubsx_gist ON ubsx_t USING gist (b);

-- A temp table's build goes through the same hooks.
CREATE TEMP TABLE ubsx_tmp AS SELECT id FROM ubsx_t;
BEGIN;
SAVEPOINT s1;
CREATE INDEX ubsx_bad4 ON ubsx_tmp USING gin ((ARRAY[100 / (id - 500)]));
ROLLBACK TO SAVEPOINT s1;
COMMIT;

-- The build fails in an inner savepoint, and the outer one is rolled back.
BEGIN;
SAVEPOINT s1;
SAVEPOINT s2;
CREATE INDEX ubsx_bad5 ON ubsx_t USING gin ((ARRAY[100 / (id - 500)]));
ROLLBACK TO SAVEPOINT s1;
CREATE INDEX ubsx_gin11 ON ubsx_t USING gin ((ARRAY[id % 11]));
COMMIT;

-- A build that succeeds in a savepoint that is then rolled back.
BEGIN;
SAVEPOINT s1;
CREATE INDEX ubsx_gin7 ON ubsx_t USING gin ((ARRAY[id % 7]));
ROLLBACK TO SAVEPOINT s1;
CREATE INDEX ubsx_gin7 ON ubsx_t USING gin ((ARRAY[id % 7]));
COMMIT;

-- The index expression aborts its own subtransactions mid-build.
BEGIN;
SAVEPOINT s1;
CREATE INDEX ubsx_expr ON ubsx_t USING gin (ubsx_div(id - 500));
COMMIT;

SELECT count(*) FROM pg_class WHERE relname LIKE 'ubsx_bad%';

-- Every index built above, read back from the pageserver.
SELECT clear_buffer_cache();
SET enable_seqscan = off;
SELECT count(*) FROM ubsx_t WHERE a @> ARRAY[3];
SELECT id FROM ubsx_t WHERE t = 'k42';
SELECT count(*) FROM ubsx_t WHERE b && box(point(10.5, 10.5), point(11.5, 11.5));
SELECT count(*) FROM ubsx_t WHERE ARRAY[id % 11] @> ARRAY[3];
SELECT count(*) FROM ubsx_t WHERE ARRAY[id % 7] @> ARRAY[3];
SELECT id FROM ubsx_t WHERE ubsx_div(id - 500) @> ARRAY[-1000];
RESET enable_seqscan;

DROP TABLE ubsx_t, ubsx_tmp;
DROP FUNCTION ubsx_div(int);
