-- mem0 tables
-- rag tables
-- promotions tables
CREATE SCHEMA IF NOT EXISTS datapacks;

/*
This table is used to store the knowledge base for each tenant.
1. This schema needs to match that of dist_rag vector index table.
2. tenant_id will be mapped to datapack_id
3. For promoted memories, document_id will be mapped to conversation_id
4. Need to add entry in metadata_filters to specify source of the knowledge base. doc vs memory 
   4.a metadata_filters can also hold the memory id for promoted memories, 
        so we can filter out redudant memories retrieved from the memory search.
*/

CREATE TABLE IF NOT EXISTS datapacks.knowledge_bases (
    tenant_id UUID NOT NULL, --DEFAULT '00000000-0000-0000-0000-000000000000' 
    id UUID DEFAULT gen_random_uuid(),
    chunk_text TEXT NOT NULL,
    embeddings vector(1536) NOT NULL, -- TODO: Make the vector dimension configurable similar to dist_rag.pg_rag_default_store?
    document_id UUID NOT NULL,
    metadata_filters JSONB NOT NULL DEFAULT '{}'::jsonb,                    
    PRIMARY KEY  ((tenant_id, id) HASH)
) PARTITION BY HASH (tenant_id);

-- 128 partitions on datapack_id
DO $$
BEGIN
    FOR i IN 0..127 LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS datapacks.knowledge_bases_%s 
             PARTITION OF datapacks.knowledge_bases 
             FOR VALUES WITH (MODULUS 128, REMAINDER %s)',
            LPAD(i::text, 3, '0'),
            i
        );
    END LOOP;
END $$;

CREATE INDEX IF NOT EXISTS knowledge_bases_embeddings_idx
    ON datapacks.knowledge_bases
    USING hnsw (embeddings vector_cosine_ops)
    WITH (m = 32, ef_construction = 64);



/*************************************************************
mem0 tables
**************/
-- see this mem0 change which is currently creating these tables:
    -- https://github.com/yugabyte/meko-mem0/compare/meko-mem0/1.0.11.1...meko-mem0/1.0.11.2

-- Should be pre-created as part of datapack universe. But mem0 may create it if not present.

-- Need to review the query pattern for mem0 collection table. 
-- Will we need meko_created_at for getting id of each memory? 
-- Need to track largest promoted at timestamp for processing on the latest memories. 
    -- Assume there are 1000 memories, 900 are already processed, we need to process the latest 100 memories.
-- meko_conversation_id maps to run_id field in payload or properties columns of different mem0 tables.
-- When a memory is promoted to knowledge base, we need to update the meko_promoted_to_kb_at field
    -- When the same memory is modified, we need to reset the meko_created_at field, so it becomes visible again.

CREATE TYPE datapacks.meko_promotion_user_action AS ENUM ('promoted', 'keep_as_memory');

CREATE TABLE IF NOT EXISTS datapacks.meko_mem0_collection (
                    meko_datapack_id UUID NOT NULL,
                    meko_user_id UUID NOT NULL,
                    meko_agent_id TEXT NOT NULL,
                    id UUID NOT NULL,
                    meko_conversation_id UUID,
                    embeddings vector(1536),
                    payload JSONB,
                    meko_created_at TIMESTAMPTZ DEFAULT NOW(),
                    meko_promotion_user_action datapacks.meko_promotion_user_action NULL, -- If 'not_promoted', UI shouldn't surface this memory for promotion to KB again.
                    PRIMARY KEY (meko_datapack_id HASH, meko_user_id ASC, meko_agent_id ASC, meko_created_at DESC, id DESC)
                ) PARTITION BY HASH (meko_datapack_id, meko_user_id, meko_agent_id);

/*
1. Two table approach:
    - Dimension on: datapack_id, user_id, agent_id
    - Dimension on: datapack_id

2. id - should be a secondary index on meko_mem0_collection
*/

-- create 128 partitions on datapack_id e.g.
DO $$
BEGIN
    FOR i IN 0..127 LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS datapacks.meko_mem0_collection_%s 
             PARTITION OF datapacks.meko_mem0_collection 
             FOR VALUES WITH (MODULUS 128, REMAINDER %s)',
            LPAD(i::text, 3, '0'),
            i
        );
    END LOOP;
END $$;

CREATE INDEX IF NOT EXISTS meko_mem0_collection_hnsw_idx
                    ON datapacks.meko_mem0_collection
                    USING hnsw (embeddings vector_cosine_ops)
                    WITH (m = 16, ef_construction = 64);

/*
1. This table will soley be used for displaying the promoted memories in the UI.
*/
CREATE TABLE IF NOT EXISTS datapacks.promoted_collection (
                    meko_datapack_id UUID NOT NULL,
                    meko_user_id UUID,
                    meko_agent_id TEXT,
                    id UUID NOT NULL,
                    meko_conversation_id UUID,
                    payload JSONB,
                    meko_created_at TIMESTAMPTZ NOT NULL,
                    meko_promoted_to_kb_at TIMESTAMPTZ DEFAULT NULL,
                    PRIMARY KEY (meko_datapack_id HASH, id ASC)
                ) SPLIT INTO 3 TABLETS;



-- mem0 memory search:
-- 1. search the agents private memory in meko_mem0_collection
-- 2. Search the globally promoted meories in meko_memories_promoted_to_kb using datapack_id for filtering.
-- 3. Fetch

-- Agent1 generated memories A B C D, KB meories = Empty
-- Agent2 generated memories E F G H, KB meories = Empty
-- Agent1 promotes memory A, B to kB, KB meories = A, B

-- Agent 2 Searches for memories, Agent 2 memories = E F G H, KB memories = A B
-- Agent 2 promotes memory E, F to kB, KB meories = A B E F

-- Agent 1 Searches for memories, Agent 1 memories = C D, KB memories = A B E F


-- This will be created during datapacks initialisation.
-- CREATE TABLE IF NOT EXISTS datapacks.meko_memories_promoted_to_kb (
--                     meko_datapack_id UUID NOT NULL,
--                     meko_user_id UUID NOT NULL,
--                     meko_agent_id TEXT NOT NULL,
--                     id UUID NOT NULL,
--                     meko_conversation_id UUID,
--                     payload JSONB,                              -- todo should we just retain "data" field from the jsonb column?
--                     meko_created_at TIMESTAMPTZ DEFAULT NOW(),
--                     meko_promoted_to_kb_at TIMESTAMPTZ DEFAULT NULL,
--                     PRIMARY KEY (meko_datapack_id HASH, meko_created_at ASC, id ASC)
--                 ) SPLIT INTO 3 TABLETS;


-- graph schema
-- embeddings table: contains the embeddings for the nodes in the graph
-- Should be pre-created as part of datapack universe. But mem0 may create it if not present.
CREATE TABLE IF NOT EXISTS datapacks.meko_mem0_graph_vertex_embeddings (
                        meko_datapack_id UUID NOT NULL,
                        meko_user_id UUID NOT NULL,
                        meko_agent_id UUID NOT NULL,
                        graph_name TEXT NOT NULL,
                        node_graphid BIGINT NOT NULL,
                        node_name TEXT,
                        embeddings vector(1536) NOT NULL, -- TODO: Make the vector dimension configurable. {self.embedding_dims}
                        meko_conversation_id UUID, 
                        created_at TIMESTAMPTZ DEFAULT NOW(),
                        updated_at TIMESTAMPTZ DEFAULT NOW(),
                        meko_promotion_user_action datapacks.meko_promotion_user_action NULL,
                        PRIMARY KEY ((meko_datapack_id, meko_user_id) HASH, meko_agent_id ASC, graph_name, node_graphid)
                    ) SPLIT INTO 3 TABLETS;         -- or just 1 tablet -- verify this needs have 128 partitions.

CREATE INDEX IF NOT EXISTS meko_mem0_graph_vertex_embeddings_hnsw_idx
                    ON datapacks.meko_mem0_graph_vertex_embeddings
                    USING hnsw (embeddings vector_cosine_ops)
                    WITH (m = 32, ef_construction = 64);

CREATE TABLE IF NOT EXISTS datapacks.meko_promoted_graph_vertex_embeddings (
                        meko_datapack_id UUID NOT NULL,
                        node_graphid BIGINT NOT NULL,
                        meko_user_id UUID,
                        meko_agent_id UUID,
                        graph_name TEXT NOT NULL,
                        node_name TEXT,
                        embeddings vector(1536) NOT NULL,
                        meko_conversation_id UUID, 
                        created_at TIMESTAMPTZ NOT NULL,
                        meko_prompted_at TIMESTAMPTZ DEFAULT NULL,
                        PRIMARY KEY (meko_datapack_id HASH, node_graphid ASC)
                    ) SPLIT INTO 3 TABLETS; -- verify this needs have 128 partitions.

CREATE INDEX IF NOT EXISTS meko_mem0_promoted_graph_vertex_embeddings_hnsw_idx
                    ON datapacks.meko_promoted_graph_vertex_embeddings
                    USING hnsw (embeddings vector_cosine_ops)
                    WITH (m = 32, ef_construction = 64);

-- Need to pre-create the graph tables used for multi-tenancy storage. or prime the mem0 to create these tables as part of platform initialization.
-- Create a graph name called "meko_multi_tenant_graph"
-- This will create a schema called "meko_multi_tenant_graph"

-- Following mem0 tables will be created in the "meko_multi_tenant_graph" schema
    -- Entity
    -- CONNECTED_TO
    -- _ag_label_vertex
    -- _ag_label_label

SET search_path = ag_catalog, datapacks, public;

-- Meko multi-teant Agent Private Graph:
SELECT * FROM ag_catalog.create_graph('meko_multi_tenant_graph');

SELECT * FROM ag_catalog.create_vlabel('meko_multi_tenant_graph','Entity');

SELECT * FROM ag_catalog.create_elabel('meko_multi_tenant_graph','CONNECTED_TO');

-- Meko multi-tenant global promoted to KB graph:
SELECT * FROM ag_catalog.create_graph('meko_promoted_graph');

SELECT * FROM ag_catalog.create_vlabel('meko_promoted_graph','Entity');

SELECT * FROM ag_catalog.create_elabel('meko_promoted_graph','CONNECTED_TO');


-- TODO: Remove the need for meko_mem0_graph_vertex_embeddings table and use a vector coloum in the Entity tables.
-- TODO: After April 30th release as this will require complete re-write of the Apache age mem0 Connector.
-- ALTER TABLE meko_multi_tenant_graph."Entity" ADD COLUMN embeddings VECTOR(1536) NOT NULL;


-- Add below columns to the above tables (taken care of in mem0/Apache AGE extension):
                -- meko_datapack_id UUID NOT NULL,
                -- meko_user_id UUID NOT NULL,
                -- meko_agent_id TEXT NOT NULL,
                -- meko_conversation_id UUID NOT NULL,
    -- REVIEW: Alternatively, we can move these columns as fields of properties column in these tables.
        -- This will help keep the Apache AGE schema independent of the meko requirements
        -- What will be the performance impact of this? We do support indexes on jsonb columns.
