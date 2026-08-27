/*
Reverses 000004_conversation_search_chunked_outbox.up.sql.

Lossy by necessity, in two ways:

1. Multi-chunk turns: question_text/answer_text can't be reconstructed from N
   independent chunk_text rows without re-deriving the original split
   boundaries, which aren't stored.
2. Not-yet-embedded rows: the up-migration's whole point is letting a row sit
   with embeddings IS NULL while the auto-embedding worker catches up (any
   live turn just enqueued, priority=0) -- the restored pre-outbox schema
   requires embeddings NOT NULL on every row, which such a row can't satisfy.

Both cases are dropped outright (not backfilled) rather than forced into a
shape they can't represent -- treat this as an emergency schema rollback, not
a guarantee of restoring exact prior content or an in-flight embed queue.
*/

DROP INDEX IF EXISTS datapacks.conversation_history_search_pending_embed_idx;
DROP INDEX IF EXISTS datapacks.conversation_history_search_turn_chunk_idx;

ALTER TABLE datapacks.conversation_history_search
    ADD COLUMN IF NOT EXISTS question_text TEXT,
    ADD COLUMN IF NOT EXISTS answer_text   TEXT;

UPDATE datapacks.conversation_history_search
SET question_text = '', answer_text = COALESCE(chunk_text, '')
WHERE chunk_index = 0;

-- Drop anything the restored NOT NULL schema can't represent: a multi-chunk
-- turn (chunk_index != 0, no single-row home in the old shape) or a row
-- still awaiting embedding (embeddings IS NULL, the outbox's steady state
-- for a just-enqueued live turn) -- either would fail the SET NOT NULL below.
DELETE FROM datapacks.conversation_history_search WHERE chunk_index != 0 OR embeddings IS NULL;

ALTER TABLE datapacks.conversation_history_search
    ALTER COLUMN question_text SET NOT NULL,
    ALTER COLUMN answer_text SET NOT NULL,
    ALTER COLUMN embeddings SET NOT NULL,
    DROP COLUMN IF EXISTS chunk_index,
    DROP COLUMN IF EXISTS chunk_text,
    DROP COLUMN IF EXISTS priority;
