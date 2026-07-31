-- Visibility-map bit correctness through the pageserver. VACUUM FREEZE
-- sets the all-visible VM bits; a later DELETE/UPDATE must clear them so
-- an index-only scan rechecks the heap instead of trusting a stale bit.
-- After dropping the buffer cache the VM fork is reconstructed from YB, so
-- a wrong VM page would make the index-only scan return deleted/moved
-- rows. Ported from Neon's test_vm_bit_clear.
CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;

CREATE TABLE vmtest_delete (id integer PRIMARY KEY);
INSERT INTO vmtest_delete VALUES (1);
VACUUM FREEZE vmtest_delete;

CREATE TABLE vmtest_hot_update (id integer PRIMARY KEY, filler text);
INSERT INTO vmtest_hot_update VALUES (1, 'x');
VACUUM FREEZE vmtest_hot_update;

CREATE TABLE vmtest_cold_update (id integer PRIMARY KEY);
INSERT INTO vmtest_cold_update SELECT g FROM generate_series(1, 1000) g;
VACUUM FREEZE vmtest_cold_update;

CREATE TABLE vmtest_cold_update2 (id integer PRIMARY KEY, filler text) WITH (fillfactor=100);
INSERT INTO vmtest_cold_update2 SELECT g, '' FROM generate_series(1, 1000) g;
VACUUM FREEZE vmtest_cold_update2;

-- Clear VM bits via a delete and three updates (HOT and cold).
DELETE FROM vmtest_delete WHERE id = 1;
UPDATE vmtest_hot_update SET filler='x' WHERE id = 1;
UPDATE vmtest_cold_update SET id = 5000 WHERE id = 1;
-- Clear the VM bit on the last page with an INSERT, then on block 0 with a
-- cold UPDATE that moves the row off-page.
INSERT INTO vmtest_cold_update2 VALUES (9999, 'x');
UPDATE vmtest_cold_update2 SET id = 5000, filler=repeat('x', 200) WHERE id = 1;

-- Drop the cache so the VM fork is fetched from the pageserver, then force
-- index-only scans so a stale all-visible bit would skip the heap recheck.
SELECT clear_buffer_cache();
SET enable_seqscan=off;
SET enable_indexscan=on;
SET enable_bitmapscan=off;

SELECT id FROM vmtest_delete WHERE id = 1;
SELECT id FROM vmtest_hot_update WHERE id = 1;
SELECT id FROM vmtest_cold_update WHERE id = 1;
SELECT id FROM vmtest_cold_update2 WHERE id = 1;

RESET enable_seqscan;
RESET enable_indexscan;
RESET enable_bitmapscan;

DROP TABLE vmtest_delete, vmtest_hot_update, vmtest_cold_update, vmtest_cold_update2;
