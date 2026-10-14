-- Reverse of 000007. Deliberately a no-op: the registration may predate the migration
-- (hosted dev/prod created it by hand), and removing it would silently stop conversation
-- turns being embedded there. Dropping it where only 000007 created it is a manual
-- dist_rag.column_embedding_registrations / vector_indexes cleanup.
SELECT 1;
