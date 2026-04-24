CREATE SCHEMA IF NOT EXISTS meko_system;

/*
 * The types (tiers) of accounts
 */
CREATE TYPE meko_system.account_type AS ENUM ('free', 'pro', 'enterprise');

/*
 * This table will store the organization name and id.
 */
CREATE TABLE IF NOT EXISTS meko_system.organizations (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_name          VARCHAR NOT NULL UNIQUE,
    email_domain      VARCHAR NOT NULL UNIQUE,
    created_by_uid    UUID,                      -- user id in the users table that created this entry
    created_at        TIMESTAMPTZ DEFAULT NOW(),
    updated_at        TIMESTAMPTZ DEFAULT NOW()
);

/*
 * The users table for each new user signing up.
 */
CREATE TABLE IF NOT EXISTS meko_system.users (
    user_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_email          VARCHAR NOT NULL,
    user_first_name     VARCHAR NOT NULL,
    user_last_name      VARCHAR NOT NULL,
    user_type           meko_system.account_type NOT NULL DEFAULT 'free',
    user_org            UUID,  -- References meko_system.organizations(id)
    user_details        JSONB DEFAULT '{"user_preferences": {}, "mcp_credentials": {"mcp_user_name": null, "mcp_user_password": null}}'::jsonb,
    cognito_sub         VARCHAR UNIQUE,
    email_verified_at   TIMESTAMPTZ,
    has_completed_onboarding BOOLEAN NOT NULL DEFAULT FALSE
    created_at          TIMESTAMPTZ DEFAULT NOW(),
    updated_at          TIMESTAMPTZ DEFAULT NOW()
) SPLIT INTO 12 TABLETS;

-- Create a unique index on email make them unique and to allow lookups by email
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email ON users(account_email);
-- TODO: review if this is needed
CREATE INDEX IF NOT EXISTS idx_account_registry_cognito_sub ON users (cognito_sub);

-- **karthik's feedback:**
-- rename to yb_universes
-- max_datapacks_for_universe 
-- rename yba_management_url to yb_management_url
-- check if we need or can drop ysql_hostname and ysql_port
-- may be add universe_details .. requires tier information for datapack provisioning .. may be add it to jsonb column

-- **changes made:**
-- renamed table from yba_universe_registry to yb_universes
-- added a new column called max_datapacks_for_universe
-- renamed yba_management_url to yb_management_url
-- cannot drop ysql_hostname and ysql_port as it being used in datapack_service.create() method.

CREATE TABLE IF NOT EXISTS meko_system.yb_universes (
    universe_id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    yb_universe_uuid                UUID,
    yb_universe_name                VARCHAR NOT NULL,
    ysql_hostname                   VARCHAR NOT NULL,
    ysql_port                       INT NOT NULL,
    ysql_admin_connection_string    TEXT NOT NULL,     -- TODO: check if we need this, is this just connection string?
    yb_management_url               TEXT NOT NULL,
    num_datapacks_created           INT NOT NULL DEFAULT 0,          -- number of datapacks created so far in this universe
    max_datapacks                   INT NOT NULL DEFAULT 1000,       -- maximum number of datapacks in this universe
    created_at                      TIMESTAMPTZ DEFAULT NOW(),
    updated_at                      TIMESTAMPTZ DEFAULT NOW()
);


-- **karthik's feedback:**
-- rename to agents
-- account_id, primary as hash. agent_id will be range shareed.
-- agent_name will be meko_agent by default. User can change or add more agents for a datapack.
-- add default value to agent_name column

-- **changes made:**
-- renamed table from agent_registry to agents
-- removed the id column as we have composite primary key
-- create a new composite primary key on account_id (hash) and agent_id (range)
-- created a new column called agent_name
-- created a new column called agent_details

-- NIkhil has a datapack foo1 and foo2
-- Nikhil has 2 unregistered agents: n1, n2 (assumption is any unregistered agent will use default memory_tenant_id)
-- Nikhil wants n1 and n2 to have access to foo1 and foo2 - same memories and knowledge bases.
-- Advanced Flow: Nikhil creates a registered agent n3 with restricted access to foo1.
-- Nikhil wants n3 have its own memory. 
-- Nikhil shares foo1 with Amogh.
-- Amogh has agent a1 which should get seperated memories for foo1.
-- agent a1 may be able to see memories of n1, n2, n3 for datapack foo1.

-- users connects cursor and claude code to meko, each workbench sets a different agent_id. 
-- If the agent_id is provided, it will be not registered and will use default memory

CREATE TABLE IF NOT EXISTS meko_system.agents (
    user_id             UUID NOT NULL,
    agent_id            VARCHAR NOT NULL DEFAULT 'meko_agent',
    agent_details       JSONB,
    created_at          TIMESTAMPTZ DEFAULT NOW(),
    updated_at          TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (user_id HASH, agent_id ASC)
);

-- **karthik's feedback:**
--  rename the table to datapacks
-- column datapack_name should be of fixed lenght 22 charecters..prefix account UUID + default or unique 7 charecter string
-- account_id and datapack_name as composite primary key. account_id should be hash and datapack_name should be range sharded.
-- rename db_user_name and db_user_password, to prefix with datapack_ similar to datapack_ysql_connection_string
-- create table with 8 shards
-- remove the idx_datapack_registry_account as we have composite primary key

-- **changes made:**
-- renamed table from datapack_registry to datapacks
-- removed the id column as we have composite primary key
-- create a new composite primary key on account_id (hash) and datapack_name (range)
-- updated datapack_name column to be of fixed length 22 characters. 
    -- datapack_name is prefixed with unique 7 charecter string (based on the account_id) + defualt(or datapack_name if provided). 
    -- Example: account_id: 123e4567-e89b-12d3-a456-426614174000, datapack_name: default
    -- datapack_name: 123e4567-default
    -- Requires code changes.
-- renamed db_user_name and db_user_password to datapack_user_name and datapack_user_password
-- removed the index idx_datapack_registry_account as we have composite primary key


/*
 * List of datapacks that each account has created. Only shows the datapacks owned by this account. In most cases,
 * at the time of creation, the ownership is assigned to the account creating it. In future, we could allow a flow
 * where ownership can be transferred.
 */
CREATE TABLE IF NOT EXISTS meko_system.datapacks (                -- todo: add agent info column to the table
    datapack_id                         UUID DEFAULT gen_random_uuid(),
    user_id                             UUID NOT NULL,            -- owning user id for this datapack
    agent_id                            VARCHAR,                  -- agent id who created this datapack, maps to agents.agent_id
    datapack_name                       VARCHAR NOT NULL,
    created_at                          TIMESTAMPTZ DEFAULT NOW(),
    updated_at                          TIMESTAMPTZ DEFAULT NOW(),
    universe_id                         UUID NOT NULL,
    datapack_details                    JSONB,                    -- contains the description and summary of the datapack
    datapack_ysql_connection_string     TEXT NOT NULL,            -- Database access could be given to non-free tier users
    datapack_ysql_username              VARCHAR NOT NULL,
    datapack_ysql_password              VARCHAR NOT NULL,
    PRIMARY KEY (account_id HASH, datapack_id ASC)
);


-- **karthik's feedback:**
-- map this to a multi-tenant RAG processing
-- create a map to id here to sources table in id in dist_rag
-- primary key is datapack_id, pg_dist_rag.source_id as composite primary key. datapack_id should be hash and pg_dist_rag.source_id should be range sharded.
   -- rename it to knoweledge_bases
-- remove the index idx_kb_registry_datapack
-- payload TBD, knolwedge_base_details jsonb column

-- **changes made:**
-- renamed table from datapack_knowledge_base_registry to knowledge_bases
-- removed the id column as we have composite primary key
-- created a new composite primary key on datapack_id (hash) and rag_source_id (range)
-- created a new column called knowledge_base_details


CREATE TABLE IF NOT EXISTS meko_system.knowledge_bases (
    datapack_id             UUID NOT NULL,
    rag_source_id           UUID NOT NULL,              -- Points to the source id entry in pg_dist_rag
    knowledge_base_url      TEXT NOT NULL,              -- Needed because the meko app needs to upload files here
    knowledge_base_details  JSONB,
    created_at              TIMESTAMPTZ DEFAULT NOW(),
    updated_at              TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (datapack_id HASH, rag_source_id ASC)
);


-- **Datapack sharing flow**:
-- User1: has datapack default_datapack(UUID1) and Agent default_agent
-- User 2: Has datapack default_datapack(UUID2) and agent default_datapack

-- user 1: Shares default_datapack(UUID1) with user 2, only knowledge bases are shared with read permission.
    -- Insert a row into agent_datapacks table with user_id = user 2, agent_id = default_agent, datapack_id = UUID1
    -- Insert a row into agent_access table with user_id = user 2, agent_id = default_agent, datapack_id = UUID1, object_type = 'knowledge_base', permisssion="read", object_id = UUID1

-- List Datapacks for User 2:


-- **Datapack Change Ownership**:
-- User 1:


-- **Datapack Unsharing**:

-- **Datapack Deletion**:

/*
 * Defines the list of datapacks that a user's agent(s) has access to.
 */
CREATE TABLE IF NOT EXISTS meko_system.agent_datapacks (
    user_id          UUID NOT NULL,
    agent_id            VARCHAR NOT NULL,
    datapack_id         UUID NOT NULL,
    created_at          TIMESTAMPTZ DEFAULT NOW(),
    updated_at          TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (user_id HASH, agent_id HASH, datapack_id ASC)
);

CREATE INDEX IF NOT EXISTS idx_agent_datapacks_datapack_id 
    ON agent_datapacks (datapack_id HASH, user_id ASC, agent_id ASC);


CREATE TYPE meko_system.access_object_type AS ENUM ('knowledge_base', 'memory');
CREATE TYPE meko_system.access_permission AS ENUM ('read', 'write', 'admin');
CREATE TYPE meko_system.datapack_shared_status AS ENUM ('active', 'inactive');

-- **karthik's feedback:**
-- create a new table called agent_access
-- account_id, agent_id (range), datapack_id(range), object_type(enum: 'memory', 'knowledge_base'..), object_id (enum: 'memory_id', 'knowledge_base_id'..)
    -- this will be the primary key
-- object_permissions, create_at, updated_at 
-- make be we should make object_type should be moved to its own table..

-- **changes made:**
-- polymorphic access control table: object_type + object_id replaces per-type join tables
-- object_id holds the UUID of a knowledge_base's rag_source_id or a memory's id depending on object_type
-- no FK on object_id since it's polymorphic; integrity enforced at the app layer
-- composite FK (account_id, agent_id) references agents composite PK
-- datapack_id removed; the datapack relationship is captured in the object tables themselves

CREATE TABLE IF NOT EXISTS meko_system.agent_access (
    account_id          UUID NOT NULL,
    agent_id            VARCHAR NOT NULL,
    datapack_id         UUID NOT NULL,
    object_type         meko_system.access_object_type NOT NULL,
    object_id           UUID NOT NULL,      -- TODO: define what this id will be for knowledge and memory (rag_source_id or memory_id)
    permission          meko_system.access_permission NOT NULL DEFAULT 'read',
    datapack_share_URL   TEXT,
    datapack_shared_at   TIMESTAMPTZ DEFAULT NOW(),
    datapack_shared_status meko_system.datapack_shared_status NOT NULL DEFAULT 'inactive',
    created_at          TIMESTAMPTZ DEFAULT NOW(),
    updated_at          TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY  (account_id HASH, agent_id HASH, datapack_id ASC, object_type ASC, object_id ASC)
);


-- Langfuse mappings
CREATE TABLE IF NOT EXISTS meko_system.langfuse_org_mapping (
    account_id              UUID PRIMARY KEY,
    langfuse_org_id         TEXT NOT NULL UNIQUE,
    langfuse_org_name       TEXT NOT NULL,
    langfuse_org_public_key TEXT NOT NULL,
    langfuse_org_secret_key TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS meko_system.langfuse_project_mapping (
    datapack_id             UUID PRIMARY KEY,
    langfuse_project_id     TEXT NOT NULL UNIQUE,
    langfuse_project_name   TEXT NOT NULL,
    langfuse_public_key     TEXT NOT NULL,
    langfuse_secret_key     TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);


/*************************************************************
TODO: check if needed
**************/
-- Tier configuration
CREATE TABLE IF NOT EXISTS meko_system.tier_config (
    tier             meko_system.account_type PRIMARY KEY,
    mcp_image        TEXT NOT NULL,
    rag_image        TEXT NOT NULL,
    langfuse_image   TEXT NOT NULL,
    mcp_replicas     INT NOT NULL DEFAULT 1,
    rag_replicas     INT NOT NULL DEFAULT 1,
    created_at       TIMESTAMPTZ DEFAULT NOW(),
    updated_at       TIMESTAMPTZ DEFAULT NOW()
);

INSERT INTO meko_system.tier_config (tier, mcp_image, rag_image, langfuse_image, mcp_replicas, rag_replicas)
VALUES
    ('free',
     '161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/meko-mcp-server:0.0.5-20260401',
     '161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/rag-agent:0.1.0-20260326',
     'langfuse/langfuse:latest',
     1, 1),
    ('pro',
     '161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/meko-mcp-server:0.0.3-24_03_2026',
     '161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/rag-agent:0.1.0-20260326',
     'langfuse/langfuse:latest',
     1, 1),
    ('enterprise',
     '161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/meko-mcp-server:0.0.3-24_03_2026',
     '161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/rag-agent:0.1.0-20260326',
     'langfuse/langfuse:latest',
     1, 1)
ON CONFLICT (tier) DO NOTHING;
/**************
TODO: check if needed
*************************************************************/


-- Revoke public connect
REVOKE CONNECT ON DATABASE yugabyte FROM PUBLIC;
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;

-- Superusers bypass privilege checks, but grant explicitly for clarity.
GRANT CONNECT ON DATABASE yugabyte TO yugabyte;

/*************************************************************
TODO: check if needed
**************/
-- Seed test data
INSERT INTO meko_system.account_registry (id, account_first_name, account_last_name, account_email, account_type)
VALUES (
    'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11',
    'Meko',
    'Admin',
    'admin@meko.dev',
    'enterprise'
) ON CONFLICT (id) DO NOTHING;

INSERT INTO meko_system.yba_universe_registry (id, yb_universe_uuid, yb_universe_name, ysql_hostname, ysql_port, ysql_admin_connection_string, yba_management_url)
VALUES (
    'b1eebc99-9c0b-4ef8-bb6d-6bb9bd380a22',
    'c2eebc99-9c0b-4ef8-bb6d-6bb9bd380a33',
    'meko-dev-universe',
    'k8s-ybdevmek-ybmekosa-c74a0a6102-e4aec974c3e8e420.elb.us-east-2.amazonaws.com',
    5433,
    'postgresql://yugabyte:Meko%40123@k8s-ybdevmek-ybmekosa-c74a0a6102-e4aec974c3e8e420.elb.us-east-2.amazonaws.com:5433/yugabyte?sslmode=disable',
    'http://a0ea6ee2d58194693870037cb841bdbe-1826583179.us-east-2.elb.amazonaws.com/'
) ON CONFLICT (id) DO NOTHING;


