-- Knowledge files and the knowledge base's chunks, promoted memories among them (milestone 4).
-- A file's bytes aren't here: they live in knowledgebase-files/<datapack>/<sha256> beside the
-- database. The chunks' vectors are in kb_chunks_vec, which the store makes with the other vector
-- tables (vectors.go).
-- Migrations are append-only: never edit one that has shipped.

-- One row per file, once per datapack and content. status is QUEUED until the indexer takes it,
-- PROCESSING while it's extracted and its chunks are embedded, then COMPLETED or FAILED.
CREATE TABLE kb_documents (
  rowid         INTEGER PRIMARY KEY,
  id            TEXT NOT NULL UNIQUE,          -- a UUID: the document_id callers see
  datapack_id   TEXT NOT NULL REFERENCES datapacks(id) ON DELETE CASCADE,
  document_key  TEXT NOT NULL,                 -- <user>/<datapack>/<sha256>/<sanitized filename>, Cloud's key
  filename      TEXT NOT NULL,
  document_type TEXT NOT NULL,                 -- its MIME type
  content_hash  TEXT NOT NULL,                 -- SHA-256 of its bytes, 64 lowercase hex: its file's name
  size_bytes    INTEGER NOT NULL,
  status        TEXT NOT NULL DEFAULT 'QUEUED',
  error         TEXT NOT NULL DEFAULT '',      -- why it FAILED, for the caller; never the file's text
  chunks_total  INTEGER,                       -- NULL until its text is chunked
  uploaded_by   TEXT NOT NULL DEFAULT '',      -- the agent or client that uploaded it
  uploaded_at   TEXT NOT NULL,
  completed_at  TEXT,
  UNIQUE (datapack_id, content_hash)
) STRICT;
CREATE INDEX kb_documents_queue ON kb_documents(rowid) WHERE status = 'QUEUED';
CREATE INDEX kb_documents_recent ON kb_documents(datapack_id, uploaded_at);
CREATE INDEX kb_documents_key ON kb_documents(datapack_id, document_key);

-- The datapacks that have a knowledge base: a file was stored in it, or POST .../create asked for
-- one. Cloud's knowledge_base_file_count counts this (one knowledge base per datapack), not files.
CREATE TABLE kb_bases (
  datapack_id TEXT PRIMARY KEY REFERENCES datapacks(id) ON DELETE CASCADE,
  created_at  TEXT NOT NULL
) STRICT;

-- The knowledge base's chunks: a file's windows of text, or a promoted memory whole. embedded is 0
-- until its vector is in kb_chunks_vec and its terms in kb_chunks_fts: a chunk is searchable both
-- ways at once. AUTOINCREMENT: a rowid is never reused, so a vector embedded for a chunk deleted
-- meanwhile can't land on another.
CREATE TABLE kb_chunks (
  rowid          INTEGER PRIMARY KEY AUTOINCREMENT,
  id             TEXT NOT NULL UNIQUE,         -- a UUID: the id search results carry
  datapack_id    TEXT NOT NULL REFERENCES datapacks(id) ON DELETE CASCADE,
  document_rowid INTEGER REFERENCES kb_documents(rowid) ON DELETE CASCADE,
  promoted_rowid INTEGER REFERENCES promoted_memories(rowid) ON DELETE CASCADE,
  chunk_index    INTEGER NOT NULL DEFAULT 0,
  text           TEXT NOT NULL,
  metadata       TEXT NOT NULL DEFAULT '{}',   -- Cloud's metadata_filters: {} for a file's chunks
  embedded       INTEGER NOT NULL DEFAULT 0,
  CHECK ((document_rowid IS NULL) != (promoted_rowid IS NULL))
) STRICT;
CREATE INDEX kb_chunks_pending ON kb_chunks(rowid) WHERE embedded = 0;
CREATE INDEX kb_chunks_document ON kb_chunks(document_rowid, embedded);
CREATE INDEX kb_chunks_promoted ON kb_chunks(promoted_rowid);
CREATE INDEX kb_chunks_datapack ON kb_chunks(datapack_id);

-- Keyword search over the chunks, with the datapack as a column so its filter runs inside MATCH, as
-- for memories. FTS5's Porter stemmer matches word forms (Cloud matches spaCy lemmas). A row is
-- added when its chunk's vector lands.
CREATE VIRTUAL TABLE kb_chunks_fts USING fts5(
  datapack_id, text, content='', contentless_delete=1, tokenize='porter unicode61'
);
CREATE TRIGGER kb_chunks_fts_ad AFTER DELETE ON kb_chunks WHEN old.embedded = 1 BEGIN
  DELETE FROM kb_chunks_fts WHERE rowid = old.rowid;
END;

-- When a promoted memory's chunk became searchable; NULL while it waits for its vector.
ALTER TABLE promoted_memories ADD COLUMN promoted_to_kb_at TEXT;

-- Memories promoted before this migration were kept as text only: each gets its chunk now, to be
-- embedded by the embedder sweep. The id is a random UUID (version 4).
INSERT INTO kb_chunks(id, datapack_id, promoted_rowid, text, metadata)
SELECT lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)), 2) || '-' ||
         substr('89ab', 1 + (abs(random()) % 4), 1) || substr(hex(randomblob(2)), 2) || '-' || hex(randomblob(6))),
       datapack_id, rowid, text, json_object('source', 'memory', 'agent_id', agent_id, 'memory_id', id)
FROM promoted_memories;
