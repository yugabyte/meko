/*
 * YCP fleet registry -- control-plane-mode migration set.
 *
 * The successor of system/000008_environments.up.sql, in portable SQL. The system
 * set cannot be used here: it is a single ordered sequence, so reaching 000008
 * means first applying 000002/000004/000006, which use YugabyteDB-only DDL
 * (`PRIMARY KEY (col HASH, ...)`, `SPLIT INTO n TABLETS`) that PostgreSQL rejects.
 * A control plane needs only the fleet registry, so it gets its own set and can
 * run against a plain Postgres.
 *
 * Tracked in its own schema_migrations_controlplane table, independent of the
 * system set. The two are not interchangeable: system/000008 is frozen without an
 * api_key_id column, so over a database that already ran it the CREATE TABLE below
 * is a no-op and the unique index on that column then has nothing to build on.
 * Point control-plane mode at its own database (see connect.go).
 *
 * The dropped YB clauses are not load-bearing: on YB the first primary-key column
 * hashes by default, and SPLIT INTO only presets tablet count.
 *
 * id is the operator-facing handle -- slug(name), baked into the install hint's
 * --set agent.envName=. It is unique per account, not fleet-wide: two accounts
 * may each hold an environment called "prod".
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
    -- The YBM API key the environment's agent authenticates with. YBM validates the
    -- key and forwards its id; the id alone identifies the environment, so it is
    -- unique fleet-wide: one key drives one environment.
    api_key_id         UUID NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (uuid)
);

-- Composite, so two accounts can each hold an environment called "prod". A repeat
-- within one account violates this index, which is what makes registration refuse
-- it as a conflict.
CREATE UNIQUE INDEX IF NOT EXISTS environments_account_id_uniq
    ON meko_system.environments (account_id, id);

CREATE UNIQUE INDEX IF NOT EXISTS environments_api_key_id_uniq
    ON meko_system.environments (api_key_id);

-- Keyed by the parent's uuid rather than its id: id is only unique per account, so
-- keying on it would let two accounts' "prod" environments share one resource set.
CREATE TABLE IF NOT EXISTS meko_system.environment_desired_resources (
    env_uuid UUID NOT NULL REFERENCES meko_system.environments (uuid) ON DELETE CASCADE,
    kind     TEXT NOT NULL,
    name     TEXT NOT NULL,
    spec     JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (env_uuid, kind, name)
);
