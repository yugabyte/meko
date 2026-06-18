-- Drop the conversation_history_search answer cache. CASCADE removes the 128 hash
-- partitions and the HNSW index along with the parent table.
DROP TABLE IF EXISTS datapacks.conversation_history_search CASCADE;
