/*
conversation_history_search: chunked, registration-based auto-embedding outbox.

Two changes bundled into one migration since they touch the same table and
neither shipped before this point (the old inline-embed-then-insert path
never left a NULL-embeddings row for anything to discover):

1. Outbox shape: embeddings becomes nullable (a row can exist before it's
   embedded); the ONLY thing the destination table needs for the
   dist_rag.column_embedding_registrations/column_embedding_progress
   auto-embedding mechanism (see yugabyte-db's pg_dist_rag extension) to
   discover it is `embeddings IS NULL` -- no status/claimed_at/attempts
   columns here, that state lives in dist_rag.column_embedding_progress.
   `priority` distinguishes live turns (0) from backfill (1) for claim
   ordering, per that mechanism's two-tier claim.

2. Chunking: a single turn can be split into multiple chunks (long turns --
   e.g. a big tool-output paste -- otherwise get silently averaged into one
   blurry vector by OpenAIEmbeddings' token-split-and-average fallback, see
   `_chunk_text` in src/tools.py). `chunk_index` distinguishes a turn's
   chunks; `chunk_text` replaces `question_text`/`answer_text` as the stored,
   embeddable unit -- each chunk is its own row, own embedding, own
   independently-searchable HNSW entry. `id`'s existing PRIMARY KEY
   ((tenant_id, id) HASH) already uses a fresh gen_random_uuid() per row
   (never derived from conversation_id/message_id), so no PK change is
   needed to support multiple rows per turn.

Existing rows (all pre-chunking, single-chunk, already-embedded turns) are
migrated forward, not discarded: `chunk_text` is backfilled from
`question_text`/`answer_text` before those columns are dropped, so no
already-cached turn silently loses its content. `chunk_index` defaults to 0
for all of them (they were always exactly one chunk).
*/

ALTER TABLE datapacks.conversation_history_search
    ADD COLUMN IF NOT EXISTS chunk_index SMALLINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS chunk_text TEXT,
    ADD COLUMN IF NOT EXISTS priority   SMALLINT NOT NULL DEFAULT 0;   -- 0 = live turn, 1 = backfill

-- Backfill chunk_text for existing rows before dropping question_text/answer_text.
-- One-time UPDATE across a 128-partition table -- fine at today's row counts
-- (this feature is still narrowly rolled out, see the companion plan). If
-- this table has grown large by the time this migration actually runs,
-- switch this to a chunked/looped UPDATE (LIMIT + COMMIT per batch) instead
-- of one unbounded statement, the same way a large-table backfill is
-- handled elsewhere in this design.
UPDATE datapacks.conversation_history_search
SET chunk_text = COALESCE(question_text, '') || E'\n\n' || COALESCE(answer_text, '')
WHERE chunk_text IS NULL;

ALTER TABLE datapacks.conversation_history_search
    ALTER COLUMN chunk_text SET NOT NULL,
    ALTER COLUMN embeddings DROP NOT NULL,
    DROP COLUMN IF EXISTS question_text,
    DROP COLUMN IF EXISTS answer_text;

-- Idempotency for the enqueue path: (tenant_id, conversation_id, message_id,
-- chunk_index) uniquely identifies one chunk. Must lead with tenant_id (the
-- partition key) for a unique index on a partitioned table. Backs the
-- enqueue INSERT's ON CONFLICT DO NOTHING (a retried/duplicate enqueue call
-- for the same turn+chunk is a no-op, not a duplicate row).
CREATE UNIQUE INDEX IF NOT EXISTS conversation_history_search_turn_chunk_idx
    ON datapacks.conversation_history_search (tenant_id, conversation_id, message_id, chunk_index);

-- Discovery signal for the auto-embedding worker -- the only index it needs
-- to avoid a full-table scan every poll cycle. Shrinks on its own as rows
-- get embedded (a done row's embeddings is no longer NULL, so it drops out).
CREATE INDEX IF NOT EXISTS conversation_history_search_pending_embed_idx
    ON datapacks.conversation_history_search (priority, created_at)
    WHERE embeddings IS NULL;
