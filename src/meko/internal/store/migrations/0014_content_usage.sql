-- How much content Meko holds, by kind, for the storage limit ([storage] max_content_mb,
-- memory.Service.room): kept by the triggers below, so a check reads four integers instead of
-- summing every row, and so writes Go doesn't make itself (the memory engine's, through the store
-- API) and foreign-key cascades (deleting a datapack or a conversation) count too.
-- What counts is what callers send: memory text and payload (promoted ones too), a turn's input,
-- output, reasoning, plan and metadata, and artifact and knowledge-file bytes. Indexes, vectors,
-- chunks, memory history, the call log and its contents, and Context Gaps don't.
-- Migrations are append-only: never edit one that has shipped.
CREATE TABLE content_usage (
  kind  TEXT PRIMARY KEY,                        -- memories, conversations, knowledge, artifacts
  bytes INTEGER NOT NULL
) STRICT;

INSERT INTO content_usage(kind, bytes) VALUES
  ('memories',
   (SELECT coalesce(sum(octet_length(text) + octet_length(payload)), 0) FROM memories) +
   (SELECT coalesce(sum(octet_length(text) + octet_length(payload)), 0) FROM promoted_memories)),
  ('conversations',
   (SELECT coalesce(sum(octet_length(input) + octet_length(output) + octet_length(reasoning) +
                        octet_length(plan) + octet_length(metadata)), 0) FROM messages)),
  ('knowledge', (SELECT coalesce(sum(size_bytes), 0) FROM kb_documents)),
  ('artifacts', (SELECT coalesce(sum(size_bytes), 0) FROM artifacts));

CREATE TRIGGER content_memories_ai AFTER INSERT ON memories BEGIN
  UPDATE content_usage SET bytes = bytes + octet_length(new.text) + octet_length(new.payload)
  WHERE kind = 'memories';
END;
CREATE TRIGGER content_memories_ad AFTER DELETE ON memories BEGIN
  UPDATE content_usage SET bytes = bytes - octet_length(old.text) - octet_length(old.payload)
  WHERE kind = 'memories';
END;
CREATE TRIGGER content_memories_au AFTER UPDATE OF text, payload ON memories BEGIN
  UPDATE content_usage SET bytes = bytes + octet_length(new.text) + octet_length(new.payload)
    - octet_length(old.text) - octet_length(old.payload)
  WHERE kind = 'memories';
END;

CREATE TRIGGER content_promoted_ai AFTER INSERT ON promoted_memories BEGIN
  UPDATE content_usage SET bytes = bytes + octet_length(new.text) + octet_length(new.payload)
  WHERE kind = 'memories';
END;
CREATE TRIGGER content_promoted_ad AFTER DELETE ON promoted_memories BEGIN
  UPDATE content_usage SET bytes = bytes - octet_length(old.text) - octet_length(old.payload)
  WHERE kind = 'memories';
END;
CREATE TRIGGER content_promoted_au AFTER UPDATE OF text, payload ON promoted_memories BEGIN
  UPDATE content_usage SET bytes = bytes + octet_length(new.text) + octet_length(new.payload)
    - octet_length(old.text) - octet_length(old.payload)
  WHERE kind = 'memories';
END;

CREATE TRIGGER content_messages_ai AFTER INSERT ON messages BEGIN
  UPDATE content_usage SET bytes = bytes + octet_length(new.input) + octet_length(new.output) +
    octet_length(new.reasoning) + octet_length(new.plan) + octet_length(new.metadata)
  WHERE kind = 'conversations';
END;
CREATE TRIGGER content_messages_ad AFTER DELETE ON messages BEGIN
  UPDATE content_usage SET bytes = bytes - octet_length(old.input) - octet_length(old.output) -
    octet_length(old.reasoning) - octet_length(old.plan) - octet_length(old.metadata)
  WHERE kind = 'conversations';
END;
CREATE TRIGGER content_messages_au AFTER UPDATE OF input, output, reasoning, plan, metadata ON messages BEGIN
  UPDATE content_usage SET bytes = bytes + octet_length(new.input) + octet_length(new.output) +
    octet_length(new.reasoning) + octet_length(new.plan) + octet_length(new.metadata) -
    octet_length(old.input) - octet_length(old.output) - octet_length(old.reasoning) -
    octet_length(old.plan) - octet_length(old.metadata)
  WHERE kind = 'conversations';
END;

CREATE TRIGGER content_kb_documents_ai AFTER INSERT ON kb_documents BEGIN
  UPDATE content_usage SET bytes = bytes + new.size_bytes WHERE kind = 'knowledge';
END;
CREATE TRIGGER content_kb_documents_ad AFTER DELETE ON kb_documents BEGIN
  UPDATE content_usage SET bytes = bytes - old.size_bytes WHERE kind = 'knowledge';
END;
CREATE TRIGGER content_kb_documents_au AFTER UPDATE OF size_bytes ON kb_documents BEGIN
  UPDATE content_usage SET bytes = bytes + new.size_bytes - old.size_bytes WHERE kind = 'knowledge';
END;

CREATE TRIGGER content_artifacts_ai AFTER INSERT ON artifacts BEGIN
  UPDATE content_usage SET bytes = bytes + new.size_bytes WHERE kind = 'artifacts';
END;
CREATE TRIGGER content_artifacts_ad AFTER DELETE ON artifacts BEGIN
  UPDATE content_usage SET bytes = bytes - old.size_bytes WHERE kind = 'artifacts';
END;
CREATE TRIGGER content_artifacts_au AFTER UPDATE OF size_bytes ON artifacts BEGIN
  UPDATE content_usage SET bytes = bytes + new.size_bytes - old.size_bytes WHERE kind = 'artifacts';
END;
