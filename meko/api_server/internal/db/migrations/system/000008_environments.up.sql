/*
 * YCP fleet registry (control-plane mode).
 *
 * NOT the live definition of these tables. That is the controlplane/ set, which keys
 * environments by uuid, makes (account_id, id) unique, and binds each environment to an
 * api_key_id. This copy is frozen in an older shape that the code cannot read:
 * scanEnvironment selects uuid and api_key_id and scans account_id as NOT NULL, so a
 * control plane pointed at a database that ran only this set fails on its first query
 * rather than degrading. Nor can the controlplane set run over this one -- the table
 * survives its IF NOT EXISTS create without an api_key_id column, and the unique index
 * on that column then fails. Control-plane mode migrates with --only-controlplane-migrate
 * against its own database, so that pairing is a misconfiguration and not a path anything
 * takes.
 *
 * Inert rather than wrong in data-plane mode: envSvc is constructed only under
 * --control-plane-mode, so nothing here reads or writes these tables. Kept rather than
 * dropped because removing them means a new system migration running destructive DDL
 * against every data-plane database.
 *
 * One row per registered customer data-plane environment. Populated by the
 * agent enrollment handshake and status reports; read by the console/fleet API.
 * Holds metadata only -- never customer data.
 *
 * Auth material is stored hashed: enroll_token_hash (a bootstrap token accepted for a
 * TTL) and status_token_hash (env-scoped bearer the agent presents on
 * status/desired-state). Both belong to this frozen shape; the live set binds an
 * api_key_id instead.
 *
 * PK design: ((id) HASH). id is a slug (e.g. "acme-prod"); list is a small scan.
 * Secondary index on enroll_token_hash is the enroll hot path.
 */
CREATE TABLE IF NOT EXISTS meko_system.environments (
    id                 TEXT NOT NULL,
    account_id         TEXT,
    placement          TEXT NOT NULL DEFAULT 'cloud',   -- cloud | onprem
    region             TEXT,
    state              TEXT NOT NULL DEFAULT 'PENDING',  -- §7 state machine
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
    kind               TEXT NOT NULL DEFAULT 'meko',        -- amp | meko; meko id is bare slug, amp id is prefixed "amp-"
    resources          JSONB NOT NULL DEFAULT '[]'::jsonb,  -- amp controller's reported inventory ([] for meko)
    live               BOOLEAN NOT NULL DEFAULT FALSE,
    enroll_token_hash        TEXT NOT NULL DEFAULT '',   -- SHA-256 hex
    enroll_token_expires_at  TIMESTAMPTZ,                -- bootstrap token TTL; reusable until then
    status_token_hash        TEXT NOT NULL DEFAULT '',   -- SHA-256 hex
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY ((id) HASH)
) SPLIT INTO 4 TABLETS;

CREATE INDEX IF NOT EXISTS environments_enroll_token_hash_idx
    ON meko_system.environments (enroll_token_hash);

/*
 * Generic desired-state resources per environment (control-plane intent). Each row
 * is a typed resource (kind + name + kind-specific spec) the agent reconciles toward;
 * reported/actual state comes back on status reports. Kind-agnostic so new resource
 * types (datapack, operator, ingress, …) need no schema change.
 */
CREATE TABLE IF NOT EXISTS meko_system.environment_desired_resources (
    env_id  TEXT NOT NULL,
    kind    TEXT NOT NULL,
    name    TEXT NOT NULL,
    spec    JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY ((env_id) HASH, kind ASC, name ASC)
) SPLIT INTO 4 TABLETS;
