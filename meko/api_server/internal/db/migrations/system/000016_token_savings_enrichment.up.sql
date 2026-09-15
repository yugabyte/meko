/*
Enrichment columns for the per-datapack analytics dashboard.

tokens_if_raw_total: the context-token baseline this datapack would have
  consumed without Meko — request_count × 10,000 (BASELINE_TOKENS_PER_REQUEST
  in meko_ui/src/lib/tokenSavings.ts). Pre-migration rows are backfilled
  from request_count so savings_pct (tokens_saved_total / tokens_if_raw_total)
  is valid from day one. New rows are written directly by meko-mcp-server's
  _record_token_savings rather than derived, because the per-call baseline may
  vary as the product evolves.

score_high_total / score_mid_total / score_low_total: lifetime counts of KB
  results in each score band from context_search calls, written by
  meko-mcp-server. Nullable: pre-migration rows have no band history (the
  raw log carries no per-result scores), so NULL means "no data" rather than
  "zero high-scoring results". Bands: high ≥ 0.7, mid 0.4–0.7, low < 0.4.
*/
ALTER TABLE meko_system.token_savings_summary
  ADD COLUMN IF NOT EXISTS tokens_if_raw_total BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS score_high_total    BIGINT,
  ADD COLUMN IF NOT EXISTS score_mid_total     BIGINT,
  ADD COLUMN IF NOT EXISTS score_low_total     BIGINT;

-- Backfill tokens_if_raw_total for pre-migration rows so savings_pct is
-- computable. Uses the same 10,000-token baseline as the UI.
UPDATE meko_system.token_savings_summary
SET tokens_if_raw_total = request_count * 10000
WHERE tokens_if_raw_total = 0 AND request_count > 0;
