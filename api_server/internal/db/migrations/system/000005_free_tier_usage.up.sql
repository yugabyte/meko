/*
 * Free-tier request quota tracking.
 *
 * Tracks per-tool lifetime request counts for free-tier users so the
 * MCP server can enforce a 1,000-request-per-tool cap.
 *
 * Two-table design:
 *   free_tier_usage      — summary counters; the hot path for limit
 *                          enforcement (single upsert per MCP call).
 *   free_tier_audit_log  — append-only event log; one row per request
 *                          for analytics and per-tool trend dashboards.
 *                          Never read on the enforcement hot path.
 *
 * free_tier_usage PK is (user_id, metric_name) so each tool has an
 * independent counter and new metrics can be added as rows with no
 * schema change.
 *
 * free_tier_audit_log writes are best-effort (bounded background
 * worker in the MCP server); the summary table is the source of truth
 * for enforcement.
 */
CREATE TABLE IF NOT EXISTS meko_system.free_tier_usage (
    user_id       UUID        NOT NULL,
    metric_name   TEXT        NOT NULL,   -- e.g. 'knowledgebase_search', 'memory_search'
    request_count BIGINT      NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, metric_name)
);

CREATE TABLE IF NOT EXISTS meko_system.free_tier_audit_log (
    id          UUID        DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL,
    metric_name TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id HASH)
);

CREATE INDEX IF NOT EXISTS idx_free_tier_audit_user_metric
    ON meko_system.free_tier_audit_log (user_id, metric_name, created_at DESC);
