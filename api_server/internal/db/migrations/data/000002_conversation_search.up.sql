/*
conversation_history_search — shared conversation cache for a datapack.

Stores substantive chat turns so a later query (by anyone sharing the datapack)
can be answered by summarizing prior turns instead of a fresh full-model call.
The embedding is of the COMBINED turn (question + answer) for recall; on a query
we retrieve top-K chunks within a loose cutoff and a small model summarizes them.
question_text / answer_text are kept verbatim for display and summarization.

Mirrors datapacks.knowledge_bases on purpose: same tenant_id partition key,
same vector(1536) + HNSW cosine index, so existing search SQL patterns apply.
*/

CREATE TABLE IF NOT EXISTS datapacks.conversation_history_search (
    tenant_id        UUID NOT NULL,                       -- = datapack_id (isolation + partition key)
    id               UUID DEFAULT gen_random_uuid(),
    conversation_id  UUID NOT NULL,                       -- Langfuse trace id (provenance / deep-link)
    message_id       UUID NOT NULL,                       -- Langfuse observation id (exact turn)
    user_id          UUID,                                -- owner / author of the turn (for provenance + purge)
    question_text    TEXT NOT NULL,
    answer_text      TEXT NOT NULL,
    embeddings       vector(1536) NOT NULL,               -- embedding of (question_text + answer_text)
    agent_id         TEXT,
    metadata_filters JSONB NOT NULL DEFAULT '{}'::jsonb,  -- model, langfuse_project_id, feedback, hit_count, source
    created_at       TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY ((tenant_id, id) HASH)
) PARTITION BY HASH (tenant_id);

-- 128 hash partitions on tenant_id, same layout as knowledge_bases.
DO $$
BEGIN
    FOR i IN 0..127 LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS datapacks.conversation_history_search_%s
             PARTITION OF datapacks.conversation_history_search
             FOR VALUES WITH (MODULUS 128, REMAINDER %s)',
            LPAD(i::text, 3, '0'),
            i
        );
    END LOOP;
END $$;

CREATE INDEX IF NOT EXISTS conversation_history_search_embeddings_idx
    ON datapacks.conversation_history_search
    USING hnsw (embeddings vector_cosine_ops)
    WITH (m = 32, ef_construction = 64);

-- Fast "is this conversation already indexed?" lookups: prune to the tenant's
-- partition then seek by conversation_id (no scan).
CREATE INDEX IF NOT EXISTS conversation_history_search_conv_idx
    ON datapacks.conversation_history_search (tenant_id, conversation_id);
