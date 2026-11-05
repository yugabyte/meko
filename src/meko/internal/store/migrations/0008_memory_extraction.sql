-- Memory extraction through the user's own agent CLI: the turns waiting for it, and each
-- datapack's say in it. An extracted memory names its turn in its own metadata (meko_message_id).
-- Migrations are append-only: never edit one that has shipped.

-- A datapack can opt out, pick a profile (general or coding) and add its own instructions.
ALTER TABLE datapacks ADD COLUMN memory_extraction_opt_out INTEGER NOT NULL DEFAULT 0;
ALTER TABLE datapacks ADD COLUMN memory_extraction_profile TEXT NOT NULL DEFAULT 'general';
ALTER TABLE datapacks ADD COLUMN memory_extraction_instructions TEXT NOT NULL DEFAULT '';

-- A turn stored while extraction was on, until a run extracts it. attempts and last_error say how
-- its last runs went; a turn stays queued through failures.
CREATE TABLE extraction_queue (
  message_rowid INTEGER PRIMARY KEY REFERENCES messages(rowid) ON DELETE CASCADE,
  queued_at     TEXT NOT NULL,                   -- RFC 3339, UTC
  attempts      INTEGER NOT NULL DEFAULT 0,
  last_error    TEXT NOT NULL DEFAULT ''
) STRICT;
