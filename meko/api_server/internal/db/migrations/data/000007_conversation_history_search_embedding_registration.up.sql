-- Register datapacks.conversation_history_search for auto-embedding, as hosted dev/prod
-- have it. Two things depend on it and data/000002 + 000005 provide neither:
--
--   * the conversations worker (WORKER_TYPE=AUTO_COLUMN_EMBEDDING) only embeds a table
--     that has a dist_rag.column_embedding_registrations row, so without one the turns
--     the MCP server queues with embeddings IS NULL are never embedded;
--   * the MCP server reads the table's embedding contract (provider, model, width) from
--     the dist_rag.vector_indexes row that registration creates, so without one a
--     conversation query has no vector.
--
-- init_column_embedding inserts that vector_indexes row itself, named after the
-- registration, and its index_name is unique, so a bare INSERT of the row would make the
-- registration fail. Same embedder as knowledge_bases and hosted: Bedrock Titan v1, a
-- fixed 1536 dimensions. The guard leaves an install that already has either the
-- registration or the row, such as hosted dev/prod, untouched. One statement and no DDL,
-- because a migration that writes rows must not share a transaction with either.
--
-- Atomic: both calls are plain functions, which cannot commit, and they only INSERT and
-- UPDATE dist_rag tables. A kill partway therefore leaves nothing, a rerun starts clean, and
-- the guard cannot latch onto a half-made registration. (One made by hand outside this
-- migration, a mapping that was never initialized, is skipped: it is not ours to repair.)
DO $$
DECLARE
    v_mapping_id uuid;
    v_index_id   uuid;
BEGIN
    IF EXISTS (SELECT 1 FROM dist_rag.column_embedding_registrations
               WHERE registration_name = 'conversation_history_search')
       OR EXISTS (SELECT 1 FROM dist_rag.vector_indexes
                  WHERE index_name = 'conversation_history_search') THEN
        RETURN;
    END IF;

    v_mapping_id := dist_rag.create_column_embedding_mapping(
        r_registration_name            := 'conversation_history_search',
        r_destination_schema           := 'datapacks',
        r_destination_table            := 'conversation_history_search',
        r_destination_embedding_column := 'embeddings',
        r_destination_tenant_column    := 'tenant_id',
        r_destination_priority_column  := 'priority',
        r_source_schema                := 'datapacks',
        r_source_table                 := 'conversation_history_search',
        r_source_text_columns          := ARRAY['chunk_text'],
        r_source_filter_by_columns     := '[{"source_filter_on_column":"id","target_value_from_column":"id"}]'::jsonb
    );

    v_index_id := dist_rag.init_column_embedding(
        r_mapping_id             := v_mapping_id,
        r_ai_provider            := 'AWS_BEDROCK'::dist_rag.ai_provider_enum,
        r_embedding_model_params := '{"model": "amazon.titan-embed-text-v1", "dimensions": 1536}'::jsonb
    );

    -- init_column_embedding leaves index_options empty; describe the table's HNSW index.
    UPDATE dist_rag.vector_indexes
    SET index_options = '{"distance_metric": "cosine", "m": 32, "ef_construction": 64}'::jsonb
    WHERE id = v_index_id;
END $$;
