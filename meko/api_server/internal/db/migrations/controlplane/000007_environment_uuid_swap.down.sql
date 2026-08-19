-- Undoes 000007, and runs first of the three downs: recreate the pre-uuid tables under
-- the _old names and move the live ones aside as _new, which is what 000006's copy and
-- 000005's drop then expect. Schema only -- 000006 does the row copy.
--
-- Rolling back is lossy and can fail by design. Once (account_id, id) is unique, two
-- accounts may each hold an environment named "prod" -- and PRIMARY KEY (id) cannot
-- represent both. The copy in this file fails on the duplicate key rather than silently
-- discarding one account's environment. Resolve the collision first; there is no answer
-- a migration can pick on its own. The uuids are dropped either way, so rolling forward
-- again mints new ones.
DROP TABLE IF EXISTS meko_system.environment_desired_resources_old;
DROP TABLE IF EXISTS meko_system.environments_old;

CREATE TABLE meko_system.environments_old (
    id                 TEXT NOT NULL,
    account_id         TEXT,
    placement          TEXT NOT NULL DEFAULT 'cloud',
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
    kind               TEXT NOT NULL DEFAULT 'meko',
    resources          JSONB NOT NULL DEFAULT '[]'::jsonb,
    live               BOOLEAN NOT NULL DEFAULT FALSE,
    enroll_token_hash        TEXT NOT NULL DEFAULT '',
    enroll_token_expires_at  TIMESTAMPTZ,
    status_token_hash        TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id)
);

CREATE TABLE meko_system.environment_desired_resources_old (
    env_id  TEXT NOT NULL,
    kind    TEXT NOT NULL,
    name    TEXT NOT NULL,
    spec    JSONB NOT NULL DEFAULT '{}'::jsonb,
    generation BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (env_id, kind, name)
);

ALTER TABLE meko_system.environments RENAME TO environments_new;
ALTER TABLE meko_system.environment_desired_resources RENAME TO environment_desired_resources_new;
