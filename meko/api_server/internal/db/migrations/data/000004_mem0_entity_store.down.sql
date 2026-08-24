-- Reverse of 000004_mem0_entity_store.up.sql.
-- Dropping the parent table drops all 128 partitions with it.

DROP INDEX IF EXISTS datapacks.meko_mem0_collection_text_lemmatized_idx;

DROP TABLE IF EXISTS datapacks.meko_mem0_collection_entities;
