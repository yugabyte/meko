-- The call log (milestone 3): one row per MCP tool call, what metrics are computed from, as Meko
-- Cloud computes them from its traces. It holds no arguments and no text: names, ids, counts,
-- scores and timings only. Rows older than 90 days are pruned.
-- Migrations are append-only: never edit one that has shipped.

CREATE TABLE call_log (
  id              INTEGER PRIMARY KEY,
  at              TEXT NOT NULL,                 -- RFC 3339, UTC: when the call started
  tool            TEXT NOT NULL,
  datapack_id     TEXT NOT NULL DEFAULT '',      -- no foreign key: history outlives a deleted datapack
  agent_id        TEXT NOT NULL DEFAULT '',
  conversation_id TEXT NOT NULL DEFAULT '',
  credential      TEXT NOT NULL DEFAULT '',      -- admin, client or none
  ok              INTEGER NOT NULL,              -- 1, or 0 with error_code set
  error_code      TEXT NOT NULL DEFAULT '',
  ms              INTEGER NOT NULL,
  memory_n        INTEGER NOT NULL DEFAULT 0,    -- results per source, for searches
  kb_n            INTEGER NOT NULL DEFAULT 0,
  conversation_n  INTEGER NOT NULL DEFAULT 0,
  score_high      INTEGER NOT NULL DEFAULT 0,    -- results by match quality: >= 0.75, >= 0.5, below
  score_mid       INTEGER NOT NULL DEFAULT 0,
  score_low       INTEGER NOT NULL DEFAULT 0,
  tokens_sent     INTEGER NOT NULL DEFAULT 0,    -- context_search's token estimate
  tokens_saved    INTEGER NOT NULL DEFAULT 0,
  input_tokens    INTEGER NOT NULL DEFAULT 0,    -- what track_token_usage reports
  output_tokens   INTEGER NOT NULL DEFAULT 0,
  steps           TEXT NOT NULL DEFAULT '[]'     -- JSON: [{step, ms, n}], the call's parts
) STRICT;
CREATE INDEX call_log_datapack ON call_log(datapack_id, at);
CREATE INDEX call_log_at ON call_log(at);
