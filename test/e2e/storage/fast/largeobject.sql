-- Large-object round-trip under the corruption/correctness gate (#140).
-- A large object is stored in pg_largeobject as ~2 KB chunks in the page
-- store, so a multi-hundred-KB LO spans many pages. We create one, verify a
-- byte-identical full read-back, overwrite the leading chunk via lo_put and
-- re-read it, then unlink. The assigned OID is nondeterministic, so it is
-- captured with \gset (never printed) to keep the golden stable. Self-cleaning
-- (unlinks the object).
\set ON_ERROR_STOP 1

-- 200 KB payload (deadbeef ×50000 hex → 200000 bytes) ≈ 100 LO pages.
SELECT lo_from_bytea(0, decode(repeat('deadbeef', 50000), 'hex')) AS loid \gset
SELECT length(lo_get(:loid)) AS len;
SELECT lo_get(:loid) = decode(repeat('deadbeef', 50000), 'hex') AS ok;

-- Overwrite the first 4 bytes, confirm the edit lands and the rest is intact.
SELECT lo_put(:loid, 0, '\xcafebabe'::bytea);
SELECT substring(lo_get(:loid) for 4) = '\xcafebabe'::bytea AS ok_after_put;
SELECT length(lo_get(:loid)) AS len_after_put;

SELECT lo_unlink(:loid);
