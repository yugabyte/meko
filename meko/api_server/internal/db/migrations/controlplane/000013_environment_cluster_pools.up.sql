/*
 * Which storage pool each amp cluster belongs to -- control-plane-mode migration set.
 *
 * environment_tasks keys a task by cluster_id (an amp UUID) and has no pool column,
 * and the observed inventory, the only other place that pairs a cluster with its
 * pool, forgets a cluster as soon as the agent collects it. So scoping the task log
 * to a pool from the live inventory alone hid every task of a deleted cluster,
 * including its delete_cluster task. This table remembers the pairing past that:
 * written from each heartbeat's inventory, never removed because a cluster stopped
 * being reported.
 *
 * A row is only ever written from a cluster the agent reported under that pool, so a
 * pool still sees only its own clusters' tasks. Amp cluster UUIDs are not reused.
 *
 * observed_at is when the cluster was last reported, to within a day: a row is pruned
 * only once no task refers to it and its cluster has been gone for the task retention.
 *
 * Portable SQL only -- this set has to run against a plain PostgreSQL.
 */
CREATE TABLE IF NOT EXISTS meko_system.environment_cluster_pools (
    env_uuid     UUID NOT NULL REFERENCES meko_system.environments (uuid) ON DELETE CASCADE,
    cluster_id   UUID NOT NULL,
    storage_pool TEXT NOT NULL,
    observed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (env_uuid, cluster_id)
);

CREATE INDEX IF NOT EXISTS environment_cluster_pools_by_pool
    ON meko_system.environment_cluster_pools (env_uuid, storage_pool);
