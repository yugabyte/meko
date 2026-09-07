/*
 * Per-environment amp task history -- control-plane-mode migration set.
 *
 * The reconcile agent relays the amp controller's task log on its status heartbeat
 * (a delta per tick, not a snapshot); this is where those deltas accumulate, so
 * GET /environments/{id}/tasks can answer "what is running" and "what failed"
 * without the console reaching into a customer cluster it has no path to.
 *
 * A TABLE, unlike environments.resources (and unlike the datapack_phases JSONB that
 * 000002 added and 000003 dropped), because this is the one thing the agent reports
 * that has to OUTLIVE its source. The agent's memory of what it has relayed dies
 * with its pod, and there is no inbound path to a BYOC data plane, so the control
 * plane cannot re-read the log to rebuild what it missed -- it is the only durable
 * holder. A paginated, status-filtered console tab is also a real reader for real
 * rows, which is exactly what 000002 said its JSONB snapshot did not have.
 *
 * Keyed by env_uuid, not the id slug: id is unique only per account, so keying on it
 * would let two accounts' "prod" environments share one task history (the same
 * reason environment_desired_resources gives in 000001).
 *
 * Portable SQL only -- this set has to run against a plain PostgreSQL.
 */
CREATE TABLE IF NOT EXISTS meko_system.environment_tasks (
    env_uuid   UUID NOT NULL REFERENCES meko_system.environments (uuid) ON DELETE CASCADE,
    task_id    UUID NOT NULL,
    kind       TEXT NOT NULL,                -- e.g. create_cluster, resize_endpoint
    status     TEXT NOT NULL,                -- pending | running | succeeded | failed
    cluster_id UUID,                         -- the task's target, as the controller reports it
    target_ref TEXT,
    error      TEXT,                         -- the controller's failure reason; set when failed
    result     JSONB,                        -- the task's final step result, opaque

    -- The desired-state generation the task applies, or NULL for a task an operator
    -- or the amp UI planned directly. It is what distinguishes "the console did
    -- this" from "someone went around the control plane".
    ycp_generation BIGINT,

    -- The controller's own timestamps, kept distinct from reported_at below: those
    -- two answer different questions ("when did this task run" vs "how stale is what
    -- we know about it"), and a console that conflated them would read an agent
    -- outage as a task that had not moved.
    task_created_at TIMESTAMPTZ NOT NULL,
    task_updated_at TIMESTAMPTZ NOT NULL,
    reported_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (env_uuid, task_id)
);

-- The default page: one environment's history, newest first. The task_id tiebreak
-- is load-bearing rather than cosmetic -- several tasks can share a created_at, and
-- without a total order the same row can appear on two pages of one listing.
CREATE INDEX IF NOT EXISTS environment_tasks_by_created_at
    ON meko_system.environment_tasks (env_uuid, task_created_at DESC, task_id DESC);

-- "What is running right now", the console's primary query. Partial, because the
-- non-terminal rows are a tiny and shrinking fraction of the history.
CREATE INDEX IF NOT EXISTS environment_tasks_active
    ON meko_system.environment_tasks (env_uuid, status)
    WHERE status IN ('pending', 'running');

-- Retention prune (see EnvironmentTaskRepo.PruneTerminal), which deletes terminal
-- rows past the window across every environment.
CREATE INDEX IF NOT EXISTS environment_tasks_by_updated_at
    ON meko_system.environment_tasks (task_updated_at);
