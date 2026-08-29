/*
 * Agent deployments to a serverless runtime (AWS Bedrock AgentCore today;
 * `runtime` stays open-ended TEXT so a second real runtime is a Go-side
 * addition, not a migration).
 *
 * account_mode = managed: deploys into Meko's own AWS account using ambient
 * credentials (extends marketplace_service.go's RoleARN-empty fallback).
 * account_mode = byoc: deploys into the CALLER's AWS account via
 * sts:AssumeRole(role_arn) with external_id as the confused-deputy token --
 * both are NULL until the two-step BYOC onboarding flow fills them in.
 *
 * PK design mirrors mcp_personal_access_tokens: ((user_id) HASH, id ASC).
 * List-by-user is single-partition; row lookup is `id + user_id` so a
 * caller can never address another user's deployment by id guess.
 */
CREATE TABLE IF NOT EXISTS meko_system.deployments (
    id                  UUID NOT NULL DEFAULT gen_random_uuid(),
    user_id             UUID NOT NULL,                        -- meko_system.users(user_id)
    -- Loosely-typed per design: no agents table. "adlc-review-agent" today; a
    -- second wired agent adds a second value here, no schema change.
    agent_slug          VARCHAR(100) NOT NULL,
    runtime             VARCHAR(20) NOT NULL,                  -- 'agentcore' today; 'vercel' | 'lyzr' reserved
    account_mode        VARCHAR(10) NOT NULL,                  -- 'managed' | 'byoc'
    region              VARCHAR(30) NOT NULL,
    -- BYOC only, NULL under managed mode. role_arn is also passed as
    -- AgentCore's execution RoleArn -- see agentcore_client.go.
    role_arn            VARCHAR(2048),
    external_id         VARCHAR(64),
    -- Filled in once CreateAgentRuntime succeeds.
    agent_runtime_arn   VARCHAR(2048),
    agent_runtime_id    VARCHAR(200),
    container_uri       VARCHAR(2048),
    status              VARCHAR(20) NOT NULL DEFAULT 'pending', -- pending|deploying|active|failed|deleting|deleted
    status_message      TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY ((user_id) HASH, id ASC)
) SPLIT INTO 4 TABLETS;

-- The status poller's hot path: WHERE status IN ('pending', 'deploying').
CREATE INDEX IF NOT EXISTS deployments_status_idx
    ON meko_system.deployments (status) WHERE status IN ('pending', 'deploying');
