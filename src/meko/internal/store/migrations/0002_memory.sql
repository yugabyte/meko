-- Memories, the entities mem0 links them to, and their history (milestone 2). The vector tables
-- (memories_vec, entities_vec) aren't here: their width comes from the embedding model, so the
-- store makes them when the engine first reports it (vectors.go).
-- Migrations are append-only: never edit one that has shipped.

-- One row per memory. mem0's payload keys that searches filter on or the UI shows are columns;
-- the rest of the payload is kept as JSON. Absent ids are '' (vec0 metadata columns take no NULL).
CREATE TABLE memories (
  rowid           INTEGER PRIMARY KEY,           -- vec0 and FTS5 key on it
  id              TEXT NOT NULL UNIQUE,          -- a UUID, made by mem0
  datapack_id     TEXT NOT NULL REFERENCES datapacks(id) ON DELETE CASCADE,
  user_id         TEXT NOT NULL DEFAULT '',
  agent_id        TEXT NOT NULL DEFAULT '',
  conversation_id TEXT NOT NULL DEFAULT '',
  role            TEXT NOT NULL DEFAULT '',
  actor_id        TEXT NOT NULL DEFAULT '',
  text            TEXT NOT NULL,
  text_lemmatized TEXT NOT NULL DEFAULT '',
  hash            TEXT NOT NULL DEFAULT '',
  payload         TEXT NOT NULL DEFAULT '{}',    -- the rest of mem0's payload, a JSON object
  source          TEXT NOT NULL DEFAULT 'add',   -- provenance: 'add' (extraction comes later)
  created_at      TEXT NOT NULL,                 -- RFC 3339, UTC
  updated_at      TEXT NOT NULL
) STRICT;
CREATE INDEX memories_scope ON memories(datapack_id, user_id, agent_id);
CREATE INDEX memories_conversation ON memories(datapack_id, conversation_id);

-- Keyword search over the lemmatized text, with the datapack as a column so its filter runs inside
-- MATCH. Contentless: only rowids and the index are kept, which is all bm25() needs.
CREATE VIRTUAL TABLE memories_fts USING fts5(
  datapack_id, text_lemmatized, content='', contentless_delete=1
);
-- How many rows hold each term in each column, so keyword search can drop terms too common to weigh
-- anything. Per column ('col', not 'row'): bm25() counts a term's rows in text_lemmatized only, and
-- the datapack ids' UUID segments mustn't count.
CREATE VIRTUAL TABLE memories_fts_vocab USING fts5vocab(memories_fts, 'col');
CREATE TRIGGER memories_fts_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, datapack_id, text_lemmatized)
  VALUES (new.rowid, new.datapack_id, CASE WHEN new.text_lemmatized = '' THEN new.text ELSE new.text_lemmatized END);
END;
CREATE TRIGGER memories_fts_ad AFTER DELETE ON memories BEGIN
  DELETE FROM memories_fts WHERE rowid = old.rowid;
END;
CREATE TRIGGER memories_fts_au AFTER UPDATE OF datapack_id, text, text_lemmatized ON memories BEGIN
  DELETE FROM memories_fts WHERE rowid = old.rowid;
  INSERT INTO memories_fts(rowid, datapack_id, text_lemmatized)
  VALUES (new.rowid, new.datapack_id, CASE WHEN new.text_lemmatized = '' THEN new.text ELSE new.text_lemmatized END);
END;

-- The entities mem0 extracts from memories (names, quoted terms, identifiers) and the memories
-- that mention each, for the entity boost in search: mem0's "<collection>_entities" store.
CREATE TABLE entities (
  rowid             INTEGER PRIMARY KEY,
  id                TEXT NOT NULL UNIQUE,
  datapack_id       TEXT NOT NULL REFERENCES datapacks(id) ON DELETE CASCADE,
  user_id           TEXT NOT NULL DEFAULT '',
  agent_id          TEXT NOT NULL DEFAULT '',
  conversation_id   TEXT NOT NULL DEFAULT '',
  text              TEXT NOT NULL,
  entity_type       TEXT NOT NULL DEFAULT '',
  linked_memory_ids TEXT NOT NULL DEFAULT '[]',  -- JSON array of memory ids
  payload           TEXT NOT NULL DEFAULT '{}',
  created_at        TEXT NOT NULL,
  updated_at        TEXT NOT NULL
) STRICT;
CREATE INDEX entities_scope ON entities(datapack_id, user_id, agent_id);

-- What mem0 records about each change to a memory. It outlives the memory, but not its datapack.
CREATE TABLE memory_history (
  id             INTEGER PRIMARY KEY,
  datapack_id    TEXT NOT NULL REFERENCES datapacks(id) ON DELETE CASCADE,
  memory_id      TEXT NOT NULL,
  previous_value TEXT,
  new_value      TEXT,
  action         TEXT NOT NULL,                  -- ADD, UPDATE, DELETE
  is_deleted     INTEGER NOT NULL DEFAULT 0,
  created_at     TEXT NOT NULL,
  updated_at     TEXT
) STRICT;
CREATE INDEX memory_history_memory ON memory_history(memory_id, id);
