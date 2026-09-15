/*
Per-datapack daily search volume, broken out by search type. Written by
meko-mcp-server (_record_daily_activity) on every context_search,
knowledgebase_search, and memory_search call via an INSERT … ON CONFLICT
DO UPDATE (upsert) so each (datapack_id, activity_date, search_type)
triple is one counter row.

search_type values — a separate vocabulary from token_savings_summary.source
because these three paths map to distinct meko-mcp-server tool calls rather
than the retrieval strategy labels the token-savings pipeline uses:
  'hybrid'  — context_search (hybrid vector + KB + memory retrieval)
  'kb'      — knowledgebase_search (KB-only retrieval)
  'memory'  — memory_search (memory-only retrieval)

updated_at mirrors the pattern of token_savings_summary and free_tier_usage
so a stale-writer bug (clock skew, duplicate flush) is detectable.
*/
CREATE TABLE IF NOT EXISTS meko_system.datapack_daily_activity (
  datapack_id   UUID        NOT NULL,
  activity_date DATE        NOT NULL,
  search_type   TEXT        NOT NULL,  -- 'hybrid' | 'kb' | 'memory'
  query_count   BIGINT      NOT NULL DEFAULT 0,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (datapack_id, activity_date, search_type)
);
