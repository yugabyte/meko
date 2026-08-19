-- Give an environment a primary key that is not its own name. Step 1 of 3: the new
-- tables. 000006 copies into them, 000007 swaps them in.
--
-- Until now the key was id, which is slug(name) -- so identity was a user-chosen string
-- in a namespace shared by every account. Two consequences this removes:
--
--   * One account's registration could take over another's environment. Register upserts
--     ON CONFLICT (id), so a second account naming its environment "prod" overwrote the
--     first account's row -- its state, and the token its agent authenticates with.
--   * The desired-resources child table keyed off that same string, so its rows were
--     addressed by a name rather than by the environment they belong to.
--
-- After 000006, uuid is the key; id stays exactly as it was, the operator-facing slug the
-- agent sends as ?envId= and the install hint bakes into --set agent.envName=; and
-- (account_id, id) is unique, so two accounts may each hold "prod".
--
-- WHY THREE FILES. golang-migrate sends each file to the server as one multi-statement
-- simple query, which is one implicit transaction. YugabyteDB cannot run this sequence
-- that way -- it fails with
--
--   ERROR: current transaction is expired or aborted (query layer retries aren't
--   supported for multi-statement queries issued via the simple query protocol)
--
-- and the control plane runs on YB as well as PostgreSQL. Every statement here is fine on
-- both engines individually; it is mixing schema changes and row copying in one query
-- that YB rejects. So the DDL and the DML are separated: 000005 and 000007 create and
-- swap, 000006 only copies rows. Do not merge them back into one file.
--
-- Create-copy-rename rather than ALTER ... ADD PRIMARY KEY, because YB stores the primary
-- key physically in DocDB, so changing it in place is a table rewrite whose availability
-- depends on the server version. Copying needs only ordinary DDL on either engine.
--
-- Left over from a half-applied run: golang-migrate marks the version dirty and refuses to
-- continue, so a retry starts from a clean slate.
DROP TABLE IF EXISTS meko_system.environment_desired_resources_new;
DROP TABLE IF EXISTS meko_system.environments_new;

-- account_id becomes NOT NULL, defaulting to the empty string. NULL and '' both meant
-- "no account" (the Go layer maps one to the other through nullStr/NullString), and NULLs
-- compare as distinct inside a unique constraint -- so leaving it nullable would let
-- unlimited unaccounted rows share a name and defeat the constraint 000007 adds.
CREATE TABLE meko_system.environments_new (
    uuid               UUID NOT NULL DEFAULT gen_random_uuid(),
    id                 TEXT NOT NULL,
    account_id         TEXT NOT NULL DEFAULT '',
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

-- The child table follows the parent's key. It kept env_id as the slug, which is only
-- unique per account from here on -- so leaving it would have let two accounts' "prod"
-- environments share one set of desired resources.
CREATE TABLE meko_system.environment_desired_resources_new (
    env_uuid  UUID NOT NULL,
    kind      TEXT NOT NULL,
    name      TEXT NOT NULL,
    spec      JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Added by 000004. Carried through the rebuild rather than recreated, so a
    -- resource's generation survives -- resetting it to 0 would make the agent
    -- treat every resource as unchanged against a warm cache.
    generation BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (env_uuid, kind, name),
    FOREIGN KEY (env_uuid) REFERENCES meko_system.environments_new (uuid) ON DELETE CASCADE
);
