/*
Memory tables partitioned by datapack alone, and a tenant index on the KB chunk store.

meko_mem0_collection is hash-partitioned on (datapack, user, agent), but
memory_search deliberately leaves agent_id out so agents share memories within
a datapack. Without the agent the planner cannot prune, so every search runs
one HNSW scan per partition and merges 128 results: 1.6 s per query at 6k
datapacks and 43 s at 100k on a 3-node RF3 universe (MEKO-784).

Partitioned on meko_datapack_id alone, (datapack, user) prunes to one partition
whether or not the agent is given. Partitions stay, and stay at 128, because
they bound each HNSW index: a filtered vector search walks its index until it
finds enough of the tenant's rows, so its cost grows with the index. The same
data unpartitioned took 29-103 s per index search at 100k datapacks; one
partition in 128 took 16 ms. A small datapack is searched exactly by primary-key
prefix instead (2.2 ms, recall 1.00 at 100k).

Expand only: nothing reads or writes these tables until meko-mcp-server is
pointed at them (MEM0_COLLECTION_NAME), and meko_mem0_collection is untouched.
Same column shape as meko-mem0's PGVector.create_col(), so the library works
against either table by collection name alone.
*/

CREATE TABLE IF NOT EXISTS datapacks.meko_mem0_memories (
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
) PARTITION BY HASH (meko_datapack_id);

DO $$
BEGIN
    FOR i IN 0..127 LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS datapacks.meko_mem0_memories_%s
             PARTITION OF datapacks.meko_mem0_memories
             FOR VALUES WITH (MODULUS 128, REMAINDER %s)',
            LPAD(i::text, 3, '0'),
            i
        );
    END LOOP;
END $$;

CREATE INDEX IF NOT EXISTS meko_mem0_memories_hnsw_idx
    ON datapacks.meko_mem0_memories
    USING hnsw (embeddings vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

CREATE INDEX IF NOT EXISTS meko_mem0_memories_text_lemmatized_idx
    ON datapacks.meko_mem0_memories
    USING gin (to_tsvector('simple', payload->>'text_lemmatized'));

-- mem0's entity store is always {collection}_entities (see 000004).
CREATE TABLE IF NOT EXISTS datapacks.meko_mem0_memories_entities (
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
) PARTITION BY HASH (meko_datapack_id);

DO $$
BEGIN
    FOR i IN 0..127 LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS datapacks.meko_mem0_memories_entities_%s
             PARTITION OF datapacks.meko_mem0_memories_entities
             FOR VALUES WITH (MODULUS 128, REMAINDER %s)',
            LPAD(i::text, 3, '0'),
            i
        );
    END LOOP;
END $$;

CREATE INDEX IF NOT EXISTS meko_mem0_memories_entities_hnsw_idx
    ON datapacks.meko_mem0_memories_entities
    USING hnsw (embeddings vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

CREATE INDEX IF NOT EXISTS meko_mem0_memories_entities_text_lemmatized_idx
    ON datapacks.meko_mem0_memories_entities
    USING gin (to_tsvector('simple', payload->>'text_lemmatized'));

-- The KB chunk store's primary key is ((tenant_id, id) HASH), so a tenant-only
-- filter cannot use it and an exact search scans the whole partition (24.5 ms
-- at 100k datapacks, growing with the partition). This index makes it a range
-- scan of the datapack's own chunks, and makes a per-file DELETE by
-- (tenant_id, document_id) one as well. Its first version sat in
-- migrations/000005_kb_document_id_index.up.sql, outside migrations/data/, and
-- was never embedded or applied; IF NOT EXISTS covers a database where someone
-- created it by hand.
CREATE INDEX IF NOT EXISTS knowledge_bases_tenant_document_idx
    ON datapacks.knowledge_bases (tenant_id HASH, document_id ASC);
