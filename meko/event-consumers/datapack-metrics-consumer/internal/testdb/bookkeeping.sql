-- The tables the search.completed handler writes, as api_server migrations 000005, 000007, 000016 and
-- 000017 define them, minus the YugabyteDB-only key syntax (HASH, ASC) so they run on PostgreSQL. Shared
-- by the Go tests and testdata/gen_golden.py; the schema name is `meko_system` and the Go tests rename it.
CREATE TABLE meko_system.free_tier_audit_log (
    id          uuid        DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id     uuid        NOT NULL,
    metric_name text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE meko_system.token_savings_log (
    id              uuid        DEFAULT gen_random_uuid(),
    datapack_id     uuid        NOT NULL,
    user_id         uuid,
    conversation_id uuid        NOT NULL,
    source          text        NOT NULL,
    tokens_sent     int         NOT NULL,
    tokens_if_raw   int         NOT NULL,
    tokens_saved    int GENERATED ALWAYS AS (tokens_if_raw - tokens_sent) STORED,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, datapack_id)
);

CREATE TABLE meko_system.token_savings_summary (
    datapack_id         uuid        NOT NULL,
    source              text        NOT NULL,
    request_count       bigint      NOT NULL DEFAULT 0,
    tokens_saved_total  bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    tokens_if_raw_total bigint      NOT NULL DEFAULT 0,
    score_high_total    bigint,
    score_mid_total     bigint,
    score_low_total     bigint,
    PRIMARY KEY (datapack_id, source)
);

CREATE TABLE meko_system.datapack_daily_activity (
    datapack_id   uuid        NOT NULL,
    activity_date date        NOT NULL,
    search_type   text        NOT NULL,
    query_count   bigint      NOT NULL DEFAULT 0,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (datapack_id, activity_date, search_type)
);
