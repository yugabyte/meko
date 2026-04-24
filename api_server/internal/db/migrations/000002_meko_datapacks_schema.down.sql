-- Drop indexes
DROP INDEX IF EXISTS datapacks.meko_mem0_embeddings_hnsw_idx;
DROP INDEX IF EXISTS datapacks.meko_mem0_collection_hnsw_idx;
DROP INDEX IF EXISTS datapacks.knowledge_bases_embeddings_idx;

-- Drop tables (CASCADE drops partitions automatically)
DROP TABLE IF EXISTS datapacks.meko_mem0_embeddings CASCADE;
DROP TABLE IF EXISTS datapacks.meko_mem0_collection CASCADE;
DROP TABLE IF EXISTS datapacks.knowledge_bases CASCADE;

-- Drop schema
DROP SCHEMA IF EXISTS datapacks CASCADE;
