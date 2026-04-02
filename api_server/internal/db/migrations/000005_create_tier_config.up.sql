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
