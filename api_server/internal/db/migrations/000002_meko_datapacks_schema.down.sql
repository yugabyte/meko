-- Drop Apache AGE graphs (these create their own schemas)
SET search_path = ag_catalog, datapacks, public;
SELECT * FROM ag_catalog.drop_graph('meko_promoted_graph', true);
SELECT * FROM ag_catalog.drop_graph('meko_multi_tenant_graph', true);

-- Drop indexes
DROP INDEX IF EXISTS datapacks.meko_mem0_promoted_graph_vertex_embeddings_hnsw_idx;
DROP INDEX IF EXISTS datapacks.meko_mem0_graph_vertex_embeddings_hnsw_idx;
DROP INDEX IF EXISTS datapacks.meko_mem0_collection_hnsw_idx;
DROP INDEX IF EXISTS datapacks.knowledge_bases_embeddings_idx;

-- Drop tables (CASCADE drops partitions automatically)
DROP TABLE IF EXISTS datapacks.meko_promoted_graph_vertex_embeddings CASCADE;
DROP TABLE IF EXISTS datapacks.meko_mem0_graph_vertex_embeddings CASCADE;
DROP TABLE IF EXISTS datapacks.promoted_collection CASCADE;
DROP TABLE IF EXISTS datapacks.meko_mem0_collection CASCADE;
DROP TABLE IF EXISTS datapacks.knowledge_bases CASCADE;

-- Drop custom types
DROP TYPE IF EXISTS datapacks.meko_promotion_user_action;

-- Drop schema
DROP SCHEMA IF EXISTS datapacks CASCADE;
