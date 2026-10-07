-- unaccent ships in the compute image as stock contrib, with its rules file
-- under share/postgresql/tsearch_data; CREATE EXTENSION loads that file to
-- build the dictionary, and unaccent() reads it.
CREATE EXTENSION unaccent;

SELECT unaccent('Hôtel crème brûlée');
SELECT ts_lexize('unaccent', 'Hôtel');

-- Accent-insensitive full-text search through a configuration that runs
-- unaccent before simple.
CREATE TEXT SEARCH CONFIGURATION unaccent_fts (COPY = simple);
ALTER TEXT SEARCH CONFIGURATION unaccent_fts
    ALTER MAPPING FOR hword, hword_part, word WITH unaccent, simple;
SELECT to_tsvector('unaccent_fts', 'Hôtel crème brûlée');
SELECT to_tsvector('unaccent_fts', 'Hôtel crème brûlée')
    @@ to_tsquery('unaccent_fts', 'hotel & creme');

-- Leave the shared suite database as found.
DROP TEXT SEARCH CONFIGURATION unaccent_fts;
DROP EXTENSION unaccent;
