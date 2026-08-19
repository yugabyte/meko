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
 *
 * id is the operator-facing handle -- slug(name), sent by the agent as ?envId= and baked
 * into the install hint's --set agent.envName=. It is unique per account, not
 * fleet-wide: two accounts may each hold an environment called "prod".
 */
CREATE SCHEMA IF NOT EXISTS meko_system;

CREATE TABLE IF NOT EXISTS meko_system.environments (
    uuid               UUID NOT NULL DEFAULT gen_random_uuid(),
    id                 TEXT NOT NULL,          -- slug(name); unique per account
    -- The YBM account that owns this environment. Defaults to an all-zeros sentinel so
    -- YCP can be stood up and exercised standalone -- the e2e, the local scripts, the
    -- console -- where there is no YBM and so no account to attribute an environment to.
    --
    -- A sentinel rather than NULL because NULLs compare as distinct in a unique
    -- constraint: nullable, and environments_account_id_uniq below would let any number
    -- of unattributed rows share a name.
    account_id         UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
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
    kind               TEXT NOT NULL DEFAULT 'meko',        -- amp | meko; meko id is bare slug, amp id is prefixed "amp-"
    resources          JSONB NOT NULL DEFAULT '[]'::jsonb,  -- amp controller's reported inventory ([] for meko)
    live               BOOLEAN NOT NULL DEFAULT FALSE,
    enroll_token_hash        TEXT NOT NULL DEFAULT '',   -- SHA-256 hex
    enroll_token_expires_at  TIMESTAMPTZ,                -- bootstrap token TTL
    status_token_hash        TEXT NOT NULL DEFAULT '',   -- SHA-256 hex
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (uuid)
);

-- What Register upserts on: a name another account holds is a separate row, and
-- re-registering your own name updates it.
CREATE UNIQUE INDEX IF NOT EXISTS environments_account_id_uniq
    ON meko_system.environments (account_id, id);

CREATE INDEX IF NOT EXISTS environments_enroll_token_hash_idx
    ON meko_system.environments (enroll_token_hash);

-- Keyed by the parent's uuid rather than its id: id is only unique per account, so
-- keying on it would let two accounts' "prod" environments share one resource set.
CREATE TABLE IF NOT EXISTS meko_system.environment_desired_resources (
    env_uuid UUID NOT NULL REFERENCES meko_system.environments (uuid) ON DELETE CASCADE,
    kind     TEXT NOT NULL,
    name     TEXT NOT NULL,
    spec     JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (env_uuid, kind, name)
);
