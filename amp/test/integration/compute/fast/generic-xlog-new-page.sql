-- A freshly extended page whose first WAL is a Generic delta against its
-- all-zero relation-extension image, which is how RUM (DocumentDB's index AM)
-- logs the new right page of a non-root split. With full_page_writes off that
-- image is the block's only base, and walredo used to skip the delta and serve
-- an all-zero page. The pageserver must return the page the compute wrote.
--
-- Whether the extensions already exist depends on which workloads ran first,
-- and later goldens print it, so leave them as this workload found them.
SET client_min_messages = warning;
SELECT NOT EXISTS (SELECT FROM pg_extension WHERE extname = 'yb_amp_test_utils') AS drop_test_utils,
       NOT EXISTS (SELECT FROM pg_extension WHERE extname = 'pageinspect') AS drop_pageinspect \gset
CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;
CREATE EXTENSION IF NOT EXISTS pageinspect;

-- Never scanned or vacuumed: the helper's pages are not valid heap pages.
CREATE TABLE gxlog_new_page (c int) WITH (autovacuum_enabled = false);

DO $$
DECLARE
  blk          int8;
  compute_page bytea;
  ps_page      bytea;
BEGIN
  FOR i IN 1..5 LOOP
    blk := test_generic_xlog_new_page('gxlog_new_page', i);
    compute_page := get_raw_page('gxlog_new_page', 'main', blk);
    ps_page := get_raw_page_at_lsn('gxlog_new_page', 'main', blk, NULL, NULL);
    IF ps_page = decode(repeat('00', 8192), 'hex') THEN
      RAISE EXCEPTION 'block %: pageserver returned an all-zero page', blk;
    END IF;
    IF (SELECT lsn FROM page_header(ps_page)) <> (SELECT lsn FROM page_header(compute_page)) THEN
      RAISE EXCEPTION 'block %: pageserver page LSN % differs from compute page LSN %', blk,
        (SELECT lsn FROM page_header(ps_page)), (SELECT lsn FROM page_header(compute_page));
    END IF;
    -- From pd_flags on: pd_checksum is only set when the compute writes the page out.
    IF substr(ps_page, 11) <> substr(compute_page, 11) THEN
      RAISE EXCEPTION 'block %: pageserver page differs from the compute page', blk;
    END IF;
  END LOOP;
END $$;

DROP TABLE gxlog_new_page;

\set QUIET on
\if :drop_test_utils
DROP EXTENSION yb_amp_test_utils;
\endif
\if :drop_pageinspect
DROP EXTENSION pageinspect;
\endif
