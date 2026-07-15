-- TOAST round-trip under the corruption/correctness gate (#140).
-- pg_regress.sh already checks logical correctness; this exists to route the
-- storage-heavy path through the e2e gate: SET STORAGE EXTERNAL forces large
-- values out-of-line and uncompressed, so each row spans many ~2 KB TOAST
-- chunks (hence many page-store pages). We push them through insert / update /
-- delete / VACUUM FULL (which rewrites the table and its TOAST relation) and
-- assert byte-identical round-trip at each step. Output is booleans + counts
-- only, so the golden is deterministic. Self-cleaning (drops its table).
\set ON_ERROR_STOP 1

CREATE TABLE toast_t (id int PRIMARY KEY, big text);
ALTER TABLE toast_t ALTER COLUMN big SET STORAGE EXTERNAL;

-- ~64 KB per value (md5 is 32 chars; ×2000), 50 rows ≈ 3.2 MB out-of-line.
INSERT INTO toast_t
SELECT g, repeat(md5(g::text), 2000) FROM generate_series(1, 50) g;
SELECT count(*) AS n, bool_and(big = repeat(md5(id::text), 2000)) AS ok FROM toast_t;

-- Rewrite every TOAST value at a different length.
UPDATE toast_t SET big = repeat(md5((id + 1000)::text), 2500);
SELECT bool_and(big = repeat(md5((id + 1000)::text), 2500)) AS ok_after_update FROM toast_t;

-- Delete half, then VACUUM FULL (new relfilenode for table + TOAST), read back.
DELETE FROM toast_t WHERE id % 2 = 0;
VACUUM FULL toast_t;
SELECT count(*) AS n, bool_and(big = repeat(md5((id + 1000)::text), 2500)) AS ok_after_vacuum FROM toast_t;

DROP TABLE toast_t;
