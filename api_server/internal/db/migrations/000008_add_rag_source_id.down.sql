ALTER TABLE meko_system.datapack_knowledge_base_registry
    DROP COLUMN IF EXISTS rag_index_name,
    DROP COLUMN IF EXISTS rag_source_id;
