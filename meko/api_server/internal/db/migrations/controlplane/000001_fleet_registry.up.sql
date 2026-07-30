/*
 * YCP fleet registry -- control-plane-mode migration set.
 *
 * Same tables as system/000008_environments.up.sql, in portable SQL. The system
 * set cannot be used here: it is a single ordered sequence, so reaching 000008
 * means first applying 000002/000004/000006, which use YugabyteDB-only DDL
 * (`PRIMARY KEY (col HASH, ...)`, `SPLIT INTO n TABLETS`) that PostgreSQL rejects.
 * A control plane needs only the fleet registry, so it gets its own set and can
 * run against a plain Postgres.
 *
 * Tracked in its own schema_migrations_controlplane table, independent of the
 * system set. Every statement is IF NOT EXISTS, so pointing control-plane mode at
 * a YugabyteDB that already ran system/000008 is a no-op rather than a conflict.
 *
 * The dropped YB clauses are not load-bearing: on YB the first primary-key column
 * hashes by default, and SPLIT INTO only presets tablet count.
 */
CREATE SCHEMA IF NOT EXISTS meko_system;

CREATE TABLE IF NOT EXISTS meko_system.environments (
    id                 TEXT NOT NULL,
    account_id         TEXT,
    placement          TEXT NOT NULL DEFAULT 'cloud',    -- cloud | onprem
    region             TEXT,
    state              TEXT NOT NULL DEFAULT 'PENDING',
    k8s_version        TEXT,
    node_count         INT NOT NULL DEFAULT 0,
    provider           TEXT,
    datapacks          INT NOT NULL DEFAULT 0,
    max_datapacks      INT NOT NULL DEFAULT 400,
    agent_version      TEXT,
    last_heartbeat_at  TIMESTAMPTZ,
    mcp_base_url       TEXT,
    cert_fingerprint   TEXT,
    components         JSONB NOT NULL DEFAULT '[]'::jsonb,
    live               BOOLEAN NOT NULL DEFAULT FALSE,
    enroll_token_hash        TEXT NOT NULL DEFAULT '',   -- SHA-256 hex
    enroll_token_expires_at  TIMESTAMPTZ,                -- bootstrap token TTL
    status_token_hash        TEXT NOT NULL DEFAULT '',   -- SHA-256 hex
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS environments_enroll_token_hash_idx
    ON meko_system.environments (enroll_token_hash);

CREATE TABLE IF NOT EXISTS meko_system.environment_desired_resources (
    env_id  TEXT NOT NULL,
    kind    TEXT NOT NULL,
    name    TEXT NOT NULL,
    spec    JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (env_id, kind, name)
);
