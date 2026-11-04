-- Learnings, promoted memories, provenance and artifacts (milestone 3).
-- Migrations are append-only: never edit one that has shipped.

-- A memory is a learning until the user decides: '' (a learning) or 'keep_as_memory'. Promoting
-- moves it out of memories into promoted_memories, so it has no state for that.
ALTER TABLE memories ADD COLUMN promotion_action TEXT NOT NULL DEFAULT '';
-- Provenance: the connector a memory came through, '' for none (connectors come later). With
-- agent_id, conversation_id and source, every memory records where it came from.
ALTER TABLE memories ADD COLUMN connector_id TEXT NOT NULL DEFAULT '';

-- The datapack's collective memory: memories promoted out of memories, keeping every column of
-- theirs but the lemmatized text (id, text, provenance, role, actor, hash and both dates), so the
-- move to Cloud loses nothing. Text only until milestone 4 brings them into the knowledge base.
CREATE TABLE promoted_memories (
  rowid           INTEGER PRIMARY KEY,
  id              TEXT NOT NULL UNIQUE,          -- the memory's id
  datapack_id     TEXT NOT NULL REFERENCES datapacks(id) ON DELETE CASCADE,
  user_id         TEXT NOT NULL DEFAULT '',
  agent_id        TEXT NOT NULL DEFAULT '',      -- the agent that wrote it
  conversation_id TEXT NOT NULL DEFAULT '',
  connector_id    TEXT NOT NULL DEFAULT '',
  source          TEXT NOT NULL DEFAULT '',
  role            TEXT NOT NULL DEFAULT '',
  actor_id        TEXT NOT NULL DEFAULT '',
  text            TEXT NOT NULL,
  hash            TEXT NOT NULL DEFAULT '',
  payload         TEXT NOT NULL DEFAULT '{}',    -- the rest of the memory's mem0 payload
  created_at      TEXT NOT NULL,                 -- the memory's
  updated_at      TEXT NOT NULL,                 -- the memory's
  promoted_at     TEXT NOT NULL,
  promoted_by     TEXT NOT NULL DEFAULT ''       -- the agent that promoted it
) STRICT;
CREATE INDEX promoted_memories_recent ON promoted_memories(datapack_id, promoted_at);

-- Files agents keep (artifact_put): the bytes themselves, once per datapack and SHA-256.
CREATE TABLE artifacts (
  rowid            INTEGER PRIMARY KEY,
  id               TEXT NOT NULL UNIQUE,         -- a UUID
  datapack_id      TEXT NOT NULL REFERENCES datapacks(id) ON DELETE CASCADE,
  content_hash     TEXT NOT NULL,                -- SHA-256, 64 lowercase hex
  filename         TEXT NOT NULL,
  content_type     TEXT NOT NULL,
  size_bytes       INTEGER NOT NULL,
  bytes            BLOB NOT NULL,
  agent_id         TEXT NOT NULL DEFAULT '',
  created_at       TEXT NOT NULL,
  last_accessed_at TEXT,
  UNIQUE (datapack_id, content_hash)
) STRICT;
CREATE INDEX artifacts_recent ON artifacts(datapack_id, created_at);
