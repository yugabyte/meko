-- GetPage@LSN validation through the pageserver. Capture a page's LSN,
-- write more to advance it, then read the page both at the old request LSN
-- and at the latest LSN: the time-travel read must return the older image
-- while the latest read reflects the new write. LSNs are asserted in a DO
-- block (never printed) so the golden stays deterministic. Ported from
-- Neon's test_read_validation.
CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;
CREATE EXTENSION IF NOT EXISTS pageinspect;

CREATE TABLE readval_foo (c int) WITH (autovacuum_enabled = false);
INSERT INTO readval_foo VALUES (1);

DO $$
DECLARE
  first_lsn  pg_lsn;
  second_lsn pg_lsn;
  hist_lsn   pg_lsn;
  latest_lsn pg_lsn;
BEGIN
  SELECT lsn INTO first_lsn
    FROM page_header(get_raw_page('readval_foo', 'main', 0));

  INSERT INTO readval_foo VALUES (2);
  SELECT lsn INTO second_lsn
    FROM page_header(get_raw_page('readval_foo', 'main', 0));
  IF NOT (second_lsn > first_lsn) THEN
    RAISE EXCEPTION 'page LSN did not advance after write: % !> %',
      second_lsn, first_lsn;
  END IF;

  PERFORM clear_buffer_cache();

  -- Time-travel read at the original request LSN: image must be no newer.
  SELECT lsn INTO hist_lsn
    FROM page_header(get_raw_page_at_lsn('readval_foo', 'main', 0, first_lsn, NULL));
  IF hist_lsn > first_lsn THEN
    RAISE EXCEPTION 'historical read returned newer image: % > %',
      hist_lsn, first_lsn;
  END IF;

  -- Read at latest: must reflect the second write.
  SELECT lsn INTO latest_lsn
    FROM page_header(get_raw_page_at_lsn('readval_foo', 'main', 0, NULL, NULL));
  IF latest_lsn <> second_lsn THEN
    RAISE EXCEPTION 'latest read returned % want %', latest_lsn, second_lsn;
  END IF;
  IF NOT (latest_lsn > hist_lsn) THEN
    RAISE EXCEPTION 'latest image not newer than historical: % !> %',
      latest_lsn, hist_lsn;
  END IF;
END $$;

SELECT count(*) FROM readval_foo;
DROP TABLE readval_foo;
