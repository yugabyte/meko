-- VM fork truncation through the pageserver. After VACUUM truncates the
-- relation (and its VM fork), the VM page served from the local buffer
-- must match the one reconstructed from YB once the cache is dropped — a
-- truncation that didn't propagate to the stored VM fork would diverge.
-- Ported from Neon's test_vm_truncate.
CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;
CREATE EXTENSION IF NOT EXISTS pageinspect;

CREATE TABLE vmtrunc_t (pk integer PRIMARY KEY, counter integer DEFAULT 0,
                        filler text DEFAULT repeat('?', 200));
INSERT INTO vmtrunc_t (pk) VALUES (generate_series(1, 1000));
DELETE FROM vmtrunc_t WHERE pk > 10;
VACUUM vmtrunc_t;  -- truncates the relation, including its VM and FSM

DO $$
DECLARE
  pg_bitmap text;
  ps_bitmap text;
BEGIN
  pg_bitmap := substr(encode(get_raw_page('vmtrunc_t', 'vm', 0), 'hex'), 48);
  PERFORM clear_buffer_cache();
  ps_bitmap := substr(encode(get_raw_page('vmtrunc_t', 'vm', 0), 'hex'), 48);
  IF pg_bitmap IS DISTINCT FROM ps_bitmap THEN
    RAISE EXCEPTION 'VM page mismatch after truncate: local=% pageserver=%',
      pg_bitmap, ps_bitmap;
  END IF;
END $$;

DROP TABLE vmtrunc_t;
