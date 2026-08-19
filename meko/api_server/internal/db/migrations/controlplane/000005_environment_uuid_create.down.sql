-- Undoes 000005, and runs last of the three downs: drop the uuid-keyed tables (moved
-- aside as _new by 000006's down, and already copied out of by 000005's) and put the
-- pre-uuid ones back under the real names.
DROP TABLE IF EXISTS meko_system.environment_desired_resources_new;
DROP TABLE IF EXISTS meko_system.environments_new;

ALTER TABLE meko_system.environments_old RENAME TO environments;
ALTER TABLE meko_system.environment_desired_resources_old RENAME TO environment_desired_resources;

ALTER TABLE meko_system.environments
    RENAME CONSTRAINT environments_old_pkey TO environments_pkey;
ALTER TABLE meko_system.environment_desired_resources
    RENAME CONSTRAINT environment_desired_resources_old_pkey TO environment_desired_resources_pkey;

-- Recreated only now: the name belonged to the table just dropped.
CREATE INDEX IF NOT EXISTS environments_enroll_token_hash_idx
    ON meko_system.environments (enroll_token_hash);
