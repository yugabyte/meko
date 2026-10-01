/*
 * Per-environment amp extension catalog -- control-plane-mode migration set.
 *
 * The reconcile agent relays its controller's GET /extensions (builtins plus every
 * image an operator registered) on PUT /agent/extensions whenever it changes, so a
 * console can offer what this environment can actually be created with.
 *
 * Its own table rather than a column on environments: a missing row is the
 * "never reported" answer (an older agent, a meko environment, an agent not yet
 * there), which the console must tell apart from a reported empty catalog -- and
 * environments, its scan and every reader of it stay as they are.
 *
 * Unlike environment_tasks this is a cache, not the only copy: the agent re-sends
 * the whole catalog on change and at least hourly, so a lost row heals itself.
 *
 * Portable SQL only -- this set has to run against a plain PostgreSQL.
 */
CREATE TABLE IF NOT EXISTS meko_system.environment_extensions (
    env_uuid    UUID PRIMARY KEY REFERENCES meko_system.environments (uuid) ON DELETE CASCADE,
    catalog     JSONB NOT NULL,
    reported_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
