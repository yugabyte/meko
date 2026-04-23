ALTER TABLE meko_system.datapack_knowledge_base_registry
    ADD COLUMN IF NOT EXISTS rag_source_id UUID,
    ADD COLUMN IF NOT EXISTS rag_index_name VARCHAR(50) DEFAULT 'knowledge_base';
