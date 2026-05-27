/*
 * Captures product feedback from any client surface (portal, MCP server,
 * Discord, email, CLI). Lives in the control-plane schema so it can be queried
 * across users / datapacks for triage; the datapack reference is optional
 * because feedback can be unscoped (general product direction, etc.).
 *
 * status / internal_notes / resolved_at are internal tracking fields and must
 * not be exposed on the user-facing feedback list.
 */

CREATE TYPE meko_system.feedback_source AS ENUM (
    'web_app',
    'docs',
    'discord',
    'email',
    'cli',
    'mcp_server'
);

CREATE TYPE meko_system.feedback_category AS ENUM (
    'bug',
    'confusion',
    'feature_request',
    'other'
);

CREATE TYPE meko_system.feedback_status AS ENUM (
    'new',
    'triaged',
    'in_progress',
    'resolved',
    'wontfix'
);

CREATE TABLE IF NOT EXISTS meko_system.feedback (
    -- BIGINT IDENTITY: smaller than UUID; no separate SEQUENCE in this migration (YB/YSQL manages it).
    id                 BIGINT GENERATED ALWAYS AS IDENTITY,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Nullable until first update; aligns with other meko_system tables (e.g. users.organizations).
    updated_at         TIMESTAMPTZ DEFAULT NOW(),

    -- Nullable so anonymous / non-portal sources (mcp_server, cli, email,
    -- discord) without an authenticated user can still submit feedback.
    user_id            UUID,                                    -- meko_system.users.user_id
    source             meko_system.feedback_source NOT NULL,
    source_identifier  TEXT,                                    -- e.g. discord handle, email address, mcp client id

    -- Optional datapack scope. Both columns are denormalized so a deleted
    -- datapack doesn't blank out historical feedback rows.
    datapack_id        UUID,                                    -- meko_system.datapacks.datapack_id
    datapack_name      TEXT,

    -- Where in the product the user was when they submitted (e.g. /datapacks/foo/learnings).
    context_route      TEXT,

    category           meko_system.feedback_category NOT NULL,
    body               TEXT NOT NULL,
    meko_version       TEXT,

    -- Internal triage fields (not exposed in the user-facing feedback list API).
    status             meko_system.feedback_status NOT NULL DEFAULT 'new',
    internal_notes     TEXT,
    resolved_at        TIMESTAMPTZ,

    -- Composite PK: id is the surrogate key; user_id in the key supports
    -- co-location / access patterns with user in the routing key. Secondary
    -- indexes below still cover user_id- and status-led lists (PK leads with id).
    PRIMARY KEY (user_id HASH, id ASC)
);

-- Per-user lookups ("show me my submitted feedback"), newest first.
CREATE INDEX IF NOT EXISTS idx_feedback_user_id
    ON meko_system.feedback (user_id HASH, created_at DESC);

-- Triage queue: list new / in-progress items newest first.
CREATE INDEX IF NOT EXISTS idx_feedback_status_created_at
    ON meko_system.feedback (status HASH, created_at DESC);
