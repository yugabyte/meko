CREATE TABLE IF NOT EXISTS meko_system.langfuse_org_mapping (
    account_id UUID PRIMARY KEY REFERENCES meko_system.account_registry(id) ON DELETE CASCADE,
    langfuse_org_id TEXT NOT NULL UNIQUE,
    langfuse_org_name TEXT NOT NULL,
    langfuse_org_public_key TEXT NOT NULL,
    langfuse_org_secret_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS meko_system.langfuse_project_mapping (
    account_id UUID NOT NULL REFERENCES meko_system.account_registry(id) ON DELETE CASCADE,
    datapack_id UUID PRIMARY KEY REFERENCES meko_system.datapack_registry(id) ON DELETE CASCADE,
    langfuse_project_id TEXT NOT NULL UNIQUE,
    langfuse_project_name TEXT NOT NULL,
    langfuse_public_key TEXT NOT NULL,
    langfuse_secret_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_langfuse_project_mapping_datapack_id
    ON meko_system.langfuse_project_mapping (datapack_id);

CREATE INDEX IF NOT EXISTS idx_langfuse_project_mapping_account
    ON meko_system.langfuse_project_mapping (account_id);
