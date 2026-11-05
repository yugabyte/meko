-- context_search's baseline per call: the tokens the datapack's whole knowledge base would send,
-- which Meko Cloud's context efficiency sums. 0 on calls recorded before it.
-- Migrations are append-only: never edit one that has shipped.
ALTER TABLE call_log ADD COLUMN tokens_if_raw INTEGER NOT NULL DEFAULT 0;
