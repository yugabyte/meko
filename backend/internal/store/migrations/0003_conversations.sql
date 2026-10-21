-- Conversations, their turns, and the turns' chunks for conversation search (milestone 2). The
-- chunks' vectors are in message_chunks_vec, which the store makes with the other vector tables.
-- Migrations are append-only: never edit one that has shipped.

-- A datapack that opted out of conversation search stops embedding new turns.
ALTER TABLE datapacks ADD COLUMN conversation_search_opt_out INTEGER NOT NULL DEFAULT 0;

CREATE TABLE conversations (
  rowid       INTEGER PRIMARY KEY,
  id          TEXT NOT NULL,                     -- 32 lowercase hex, derived from the session id
  datapack_id TEXT REFERENCES datapacks(id) ON DELETE CASCADE, -- NULL: a conversation in no datapack
  agent_id    TEXT NOT NULL,                     -- its owner, the agent that created it
  session_id  TEXT NOT NULL DEFAULT '',
  run_id      TEXT NOT NULL DEFAULT '',
  title       TEXT NOT NULL,
  metadata    TEXT NOT NULL DEFAULT '{}',        -- a JSON object
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
) STRICT;
-- An id is unique within a datapack, and among conversations in none.
CREATE UNIQUE INDEX conversations_id ON conversations(COALESCE(datapack_id, ''), id);
CREATE INDEX conversations_recent ON conversations(datapack_id, updated_at);

-- One turn: what the user said (input) and what the agent answered (output).
CREATE TABLE messages (
  rowid              INTEGER PRIMARY KEY,
  id                 TEXT NOT NULL,              -- 32 lowercase hex, derived from the caller's seed
  conversation_rowid INTEGER NOT NULL REFERENCES conversations(rowid) ON DELETE CASCADE,
  input              TEXT NOT NULL,
  output             TEXT NOT NULL,
  reasoning          TEXT NOT NULL DEFAULT '',
  plan               TEXT NOT NULL DEFAULT '[]', -- a JSON array of steps
  metadata           TEXT NOT NULL DEFAULT '{}',
  created_at         TEXT NOT NULL,
  UNIQUE (conversation_rowid, id)                -- a repeated seed is the same turn
) STRICT;

-- A turn's text in overlapping windows, each embedded for conversation search. embedded is 0 until
-- its vector is in message_chunks_vec (the engine may be down when the turn arrives). AUTOINCREMENT:
-- a rowid is never reused, so a vector embedded for a chunk deleted meanwhile can't land on another.
CREATE TABLE message_chunks (
  rowid         INTEGER PRIMARY KEY AUTOINCREMENT,
  message_rowid INTEGER NOT NULL REFERENCES messages(rowid) ON DELETE CASCADE,
  chunk_index   INTEGER NOT NULL,
  text          TEXT NOT NULL,
  embedded      INTEGER NOT NULL DEFAULT 0,
  UNIQUE (message_rowid, chunk_index)
) STRICT;
CREATE INDEX message_chunks_pending ON message_chunks(rowid) WHERE embedded = 0;
