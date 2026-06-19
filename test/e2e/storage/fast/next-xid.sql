-- Next-XID / CLOG SLRU advancement through the pageserver. Burn a large
-- block of XIDs so nextXid and the commit-log SLRU cross page boundaries,
-- commit visible rows past the jump, drop the buffer cache, and confirm
-- the rows' xmin still resolves as committed/visible from the
-- reconstructed CLOG. A single-session analogue of Neon's restart-driven
-- test_next_xid (the restart-persistence angle is covered by the
-- restart_wal_replay scenarios).
CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;

CREATE TABLE nextxid_t (x integer);
SET statement_timeout = 0;
SELECT test_consume_xids(500000);
INSERT INTO nextxid_t SELECT generate_series(1, 1000);
SELECT test_consume_xids(500000);

SELECT clear_buffer_cache();
SELECT count(*) FROM nextxid_t;

DROP TABLE nextxid_t;
