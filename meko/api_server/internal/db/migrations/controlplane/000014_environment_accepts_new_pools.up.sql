/*
 * The agent's word that an environment can take a storage pool it does not hold yet
 * -- control-plane-mode migration set.
 *
 * Placement reads it only for a DEGRADED environment (CLOUDGA-37501): any one
 * tenant's stuck resource degrades a shared environment, and that must not turn every
 * new tenant away. Default false, so an agent that never reports it leaves placement
 * READY-only, as it was.
 *
 * Guarded with IF NOT EXISTS like every other statement in this set -- migrations run
 * on every boot.
 */
ALTER TABLE meko_system.environments
    ADD COLUMN IF NOT EXISTS accepts_new_pools BOOLEAN NOT NULL DEFAULT false;
