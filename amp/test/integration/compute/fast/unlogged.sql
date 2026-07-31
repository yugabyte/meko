-- Unlogged relation init-fork handling across every index AM. CREATE
-- writes an init fork the pageserver must store for each relation; after
-- dropping the buffer cache the table and all its indexes must still
-- answer correctly. Ported from Neon's test_unlogged.
CREATE EXTENSION IF NOT EXISTS yb_amp_test_utils;

CREATE UNLOGGED TABLE iut (id int);
CREATE UNIQUE INDEX iut_idx ON iut (id);
CREATE INDEX iut_gist ON iut USING gist (int4range(id, id, '[]'));
CREATE INDEX iut_spgist ON iut USING spgist (int4range(id, id, '[]'));
CREATE INDEX iut_gin ON iut USING gin ((id::text::jsonb));
CREATE INDEX iut_brin ON iut USING brin (id);
CREATE INDEX iut_hash ON iut USING hash (id);
ALTER TABLE iut ADD COLUMN seq int GENERATED ALWAYS AS IDENTITY;
INSERT INTO iut (id) VALUES (42);
INSERT INTO iut (id) VALUES (43);

SELECT clear_buffer_cache();
SELECT id, seq FROM iut ORDER BY id;

DROP TABLE iut;
