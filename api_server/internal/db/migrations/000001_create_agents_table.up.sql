CREATE SCHEMA IF NOT EXISTS meko_system;

CREATE TYPE meko_system.account_type AS ENUM ('free', 'pro', 'enterprise');

CREATE TABLE IF NOT EXISTS meko_system.account_registry (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_first_name VARCHAR(255) NOT NULL,
    account_last_name VARCHAR(255) NOT NULL,
    account_email VARCHAR(255) NOT NULL UNIQUE,
    account_type meko_system.account_type NOT NULL DEFAULT 'free',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS meko_system.yba_universe_registry (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    yb_universe_uuid UUID NOT NULL UNIQUE,
    yb_universe_name VARCHAR(255) NOT NULL,
    ysql_hostname VARCHAR(255) NOT NULL,
    ysql_port INT NOT NULL,
    ysql_admin_connection_string TEXT NOT NULL,
    yba_management_url TEXT NOT NULL,
    num_datapacks_created INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS meko_system.datapack_registry (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES meko_system.account_registry(id) ON DELETE CASCADE,
    datapack_name VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    yb_universe_id UUID NOT NULL REFERENCES meko_system.yba_universe_registry(id),
    datapack_ysql_connection_string TEXT NOT NULL,
    mcp_user_name VARCHAR(255) NOT NULL UNIQUE,
    mcp_user_password VARCHAR(255) NOT NULL,
    db_user_name VARCHAR(255) NOT NULL UNIQUE,
    db_user_password VARCHAR(255) NOT NULL,

    -- llm_model VARCHAR(255),
    -- embedding_model VARCHAR(255),
    UNIQUE (account_id, datapack_name)
);

CREATE INDEX IF NOT EXISTS idx_datapack_registry_account
    ON meko_system.datapack_registry (account_id);

CREATE TABLE IF NOT EXISTS meko_system.datapack_knowledge_base_registry (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    datapack_id UUID NOT NULL REFERENCES meko_system.datapack_registry(id) ON DELETE CASCADE,
    knowledge_base_url TEXT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_kb_registry_datapack
    ON meko_system.datapack_knowledge_base_registry (datapack_id);

CREATE TABLE IF NOT EXISTS meko_system.agent_registry (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    datapack_id UUID NOT NULL REFERENCES meko_system.datapack_registry(id) ON DELETE CASCADE,
    agent_name VARCHAR(255) NOT NULL,
    agent_metadata JSONB,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (datapack_id, agent_name)
);

CREATE INDEX IF NOT EXISTS idx_agent_registry_datapack
    ON meko_system.agent_registry (datapack_id);

CREATE TABLE IF NOT EXISTS meko_system.agent_knowledge_base (
    agent_id UUID NOT NULL REFERENCES meko_system.agent_registry(id) ON DELETE CASCADE,
    knowledge_base_id UUID NOT NULL REFERENCES meko_system.datapack_knowledge_base_registry(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (agent_id, knowledge_base_id)
);

CREATE INDEX IF NOT EXISTS idx_akb_knowledge_base
    ON meko_system.agent_knowledge_base (knowledge_base_id);
