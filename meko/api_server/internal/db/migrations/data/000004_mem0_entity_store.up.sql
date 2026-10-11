-- meko-mem0 2.0.14.1 (upstream mem0 v2.0.14 base) replaces the graph store
-- with an "entity store": a companion pgvector collection named
-- {collection}_entities holding one row per named entity (person, project,
-- tool, ...) with payload.linked_memory_ids pointing at the memories that
-- mention it. mem0 uses it at search time to boost memories linked to the
-- entities named in the query.
--
-- Prod runs with MEM0_CREATE_TABLES=false (schema provisioned here, not by
-- the library), so this migration carries a copy of the DDL that
-- meko-mem0's PGVector.create_col() would emit for the entity collection.
-- Source of truth: meko-mem0 mem0/vector_stores/pgvector.py::create_col().
-- If that schema ever changes, this migration's successor must follow.
--
-- The old graph tables (meko_mem0_graph_vertex_embeddings,
-- meko_promoted_graph_vertex_embeddings) and the AGE graph are deliberately
-- left untouched (design decision 2026-07-27: deferred cleanup; the
-- promotion flow still reads legacy graph data).

CREATE EXTENSION IF NOT EXISTS vector;

-- Entity-store table for the free-tier shared collection
-- (meko_mem0_collection -> meko_mem0_collection_entities). Same shape as the
-- main collection so create_col() (dev, MEM0_CREATE_TABLES=true) and this
-- migration (prod) can never diverge. meko_promotion_user_action is unused
-- for entity rows but kept for shape parity with create_col().
CREATE TABLE IF NOT EXISTS datapacks.meko_mem0_collection_entities (
                    meko_datapack_id UUID NOT NULL,
                    meko_user_id UUID NOT NULL,
                    meko_agent_id TEXT NOT NULL,
                    id UUID NOT NULL,
                    meko_conversation_id UUID,
                    embeddings vector(1536),
                    payload JSONB,
                    meko_created_at TIMESTAMPTZ DEFAULT NOW(),
                    meko_promotion_user_action datapacks.meko_promotion_user_action NULL,
                    PRIMARY KEY (meko_datapack_id HASH, meko_user_id ASC, meko_agent_id ASC, meko_created_at DESC, id DESC)
                ) PARTITION BY HASH (meko_datapack_id, meko_user_id, meko_agent_id);

DO $$
DECLARE
    -- Set by the api-server's migrator from --hash-partitions.
    n int := COALESCE(NULLIF(current_setting('meko.hash_partitions', true), '')::int, 128);
BEGIN
    FOR i IN 0..n - 1 LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS datapacks.meko_mem0_collection_entities_%s
             PARTITION OF datapacks.meko_mem0_collection_entities
             FOR VALUES WITH (MODULUS %s, REMAINDER %s)',
            LPAD(i::text, 3, '0'),
            n,
            i
        );
    END LOOP;
END $$;

-- Entity similarity search (add-time >=0.95 dedup match, search-time >=0.5
-- boost match) runs cosine queries against this table. Index parameters
-- match meko_mem0_collection_hnsw_idx from 000001 for house consistency;
-- note create_col() only emits an HNSW index when the store is configured
-- with hnsw=true, which the MCP server does not set — acceptable for
-- dev-scale lazy-created tables, indexed here for prod.
CREATE INDEX IF NOT EXISTS meko_mem0_collection_entities_hnsw_idx
                    ON datapacks.meko_mem0_collection_entities
                    USING hnsw (embeddings vector_cosine_ops)
                    WITH (m = 16, ef_construction = 64);

-- create_col() adds this to every collection; entity payloads carry no
-- text_lemmatized so the index stays empty, but keeping it preserves
-- shape parity between migration-provisioned and lazily-created tables.
CREATE INDEX IF NOT EXISTS meko_mem0_collection_entities_text_lemmatized_idx
                    ON datapacks.meko_mem0_collection_entities
                    USING gin(to_tsvector('simple', payload->>'text_lemmatized'));

-- Hybrid (BM25-style) keyword search on the MAIN collection: mem0 2.x
-- writes payload.text_lemmatized on every new memory and keyword_search()
-- runs ts_rank_cd over this expression index. Old rows lack the field and
-- are simply never matched by the keyword signal (no backfill, design
-- decision 2026-07-27). Verified on YugabyteDB 2.31: GIN expression index
-- over a hash-partitioned table works, including multi-term tsqueries.
CREATE INDEX IF NOT EXISTS meko_mem0_collection_text_lemmatized_idx
                    ON datapacks.meko_mem0_collection
                    USING gin(to_tsvector('simple', payload->>'text_lemmatized'));
