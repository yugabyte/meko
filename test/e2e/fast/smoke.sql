-- Smoke: minimum-viable round trip through the pageserver. Covers
-- CREATE TABLE → INSERT → indexed SELECT → UPDATE → SELECT. Should
-- finish in well under 10 s; if this fails, nothing else will.

\timing on
DROP TABLE IF EXISTS smoke;
CREATE TABLE smoke (
    id    BIGSERIAL PRIMARY KEY,
    label TEXT NOT NULL
);

INSERT INTO smoke (label)
SELECT 'row-' || g FROM generate_series(1, 1000) g;

CREATE INDEX smoke_label_idx ON smoke (label);

SELECT count(*) FROM smoke WHERE label LIKE 'row-1%';
SELECT count(*) FROM smoke WHERE id BETWEEN 100 AND 200;

UPDATE smoke SET label = 'updated-' || id WHERE id <= 50;
SELECT count(*) FROM smoke WHERE label LIKE 'updated-%';

-- Deterministic verification (task 25): count + a digest over the
-- ordered (id, label) pairs. Every label is a fixed function of id, so
-- a stale page or a lost UPDATE delta moves the digest.
SELECT count(*) AS n, sum(id) AS id_sum FROM smoke;
SELECT md5(string_agg(id || ':' || label, ',' ORDER BY id)) AS digest FROM smoke;
