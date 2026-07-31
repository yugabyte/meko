-- relfilenode/OID handling through the pageserver. Consume OIDs so the
-- next relfilenode allocation wraps up against an existing table's
-- relfilenode, then VACUUM FULL the other table to force it to take a new
-- (potentially colliding) relfilenode. After dropping the buffer cache
-- both tables must still read back their own rows — a relfilenode mix-up
-- would surface as wrong/missing data. Ported from Neon's test_oid_overflow.
CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;

CREATE TABLE oidof_t1 (x integer);
INSERT INTO oidof_t1 VALUES (1);
CREATE TABLE oidof_t2 (x integer);
INSERT INTO oidof_t2 VALUES (2);

VACUUM FULL oidof_t1;
VACUUM FULL oidof_t1;
VACUUM pg_class;

SET statement_timeout = 0;
DO $$
DECLARE
  rfn oid;
BEGIN
  SELECT relfilenode INTO rfn FROM pg_class WHERE relname = 'oidof_t1';
  -- oid has no `- int` operator; cast to int4 first (the per-run cluster is
  -- freshly initdb'd, so relfilenodes are well under the int4 ceiling).
  PERFORM test_consume_oids(rfn::int - 1);
END $$;

VACUUM FULL oidof_t2;

SELECT clear_buffer_cache();
SELECT x FROM oidof_t1;
SELECT x FROM oidof_t2;

DROP TABLE oidof_t1, oidof_t2;
