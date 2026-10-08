-- Reverse of 000006_mem0_memories_by_datapack.up.sql. Dropping a parent drops its
-- 128 partitions, and with them any memories written to the new tables: move
-- those back to meko_mem0_collection first if MCP was already pointed at them.

DROP INDEX IF EXISTS datapacks.knowledge_bases_tenant_document_idx;

DROP TABLE IF EXISTS datapacks.meko_mem0_memories_entities;
DROP TABLE IF EXISTS datapacks.meko_mem0_memories;
