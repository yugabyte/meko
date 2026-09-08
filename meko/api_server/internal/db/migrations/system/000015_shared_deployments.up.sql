/*
 * The farm of pre-deployed AgentCore Runtimes any signed-in user can invoke
 * without deploying their own -- one row per deployable agent_slug (see
 * service.SupportedDeploymentAgentSlugs). Distinct from meko_system.deployments:
 * these runtimes are created and operated entirely outside Meko (typically
 * by hand via the AWS CLI/console against MEKO_DEPLOYMENTS_MANAGED_EXECUTION_ROLE_ARN),
 * so there is no owning user_id and no lifecycle for api_server to drive --
 * this table only tracks "which ARN is the shared instance for this agent."
 * Written by an operator (direct SQL today), read by GET/POST
 * /deployments/shared/:agent_slug*.
 */
CREATE TABLE IF NOT EXISTS meko_system.shared_deployments (
    agent_slug        VARCHAR(100) NOT NULL,
    agent_runtime_arn VARCHAR(2048) NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (agent_slug)
);
