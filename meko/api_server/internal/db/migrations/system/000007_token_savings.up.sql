/*
Token savings for contextual_prominence_search — a two-table design
mirroring free_tier_usage / free_tier_audit_log (system/000005_free_tier_usage.up.sql):

  - token_savings_log     — append-only audit trail, one row per call.
                             Never read on the query path.
  - token_savings_summary — running counters, upserted atomically at write
                             time (meko-mcp-server's _record_token_savings).
                             This is the hot read path: a plain point/range
                             lookup by datapack_id instead of a SUM/COUNT
                             aggregation over the growing log table.
                             YugabyteDB's distributed storage has no cheap
                             index-only count/aggregate path the way
                             single-node Postgres does, so aggregating the
                             raw log on every read doesn't scale the way an
                             upsert-maintained counter does.

Both are control-plane usage/analytics data — not per-datapack application
content — so they live in meko_system rather than the tenant-partitioned
datapacks schema (conversation_history_search / knowledge_bases).

token_savings_log: no manual tenant partitioning (unlike datapacks.*
tables) — `id HASH` lets YugabyteDB hash-distribute the table directly,
same as free_tier_audit_log. datapack_id rides along as a range/clustering
column in the composite key (id HASH, datapack_id ASC) so rows for the
same datapack sort together within a hash bucket, without making
datapack_id itself the distribution key (which would let one very active
datapack skew the shard distribution).

token_savings_summary: keyed by (datapack_id, source) — same granularity
the by-source / by-datapack breakdowns in the API response need. This is
a lifetime counter, like free_tier_usage — no per-user or time-range
filtering (that level of detail is still in token_savings_log for
ad-hoc/audit queries).
*/

CREATE TABLE IF NOT EXISTS meko_system.token_savings_log (
    id              UUID        DEFAULT gen_random_uuid(),
    datapack_id     UUID        NOT NULL,
    user_id         UUID,                   -- owner/author of the call (nullable — stdio callers may not resolve to a UUID)
    conversation_id UUID        NOT NULL,
    source          TEXT        NOT NULL,   -- 'conversation' | 'memory' | 'knowledge_base+memory'
    tokens_sent     INT         NOT NULL,
    tokens_if_raw   INT         NOT NULL,
    tokens_saved    INT GENERATED ALWAYS AS (tokens_if_raw - tokens_sent) STORED,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id HASH, datapack_id ASC)
);

-- Aggregation/reporting queries filter by datapack and often by
-- conversation or user within it (ad-hoc/audit queries; the API's hot
-- read path uses token_savings_summary below, not these indexes).
CREATE INDEX IF NOT EXISTS idx_token_savings_datapack_conv
    ON meko_system.token_savings_log (datapack_id, conversation_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_token_savings_datapack_user
    ON meko_system.token_savings_log (datapack_id, user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS meko_system.token_savings_summary (
    datapack_id        UUID        NOT NULL,
    source             TEXT        NOT NULL,   -- 'conversation' | 'memory' | 'knowledge_base+memory'
    request_count      BIGINT      NOT NULL DEFAULT 0,
    tokens_saved_total BIGINT      NOT NULL DEFAULT 0,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (datapack_id, source)
);
