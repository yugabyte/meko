-- Step 3 of 3: retire the old tables and put the new ones in their place. Schema
-- changes only (see 000005's header for why this is not one file with 000006).
DROP TABLE meko_system.environment_desired_resources;
DROP TABLE meko_system.environments;

ALTER TABLE meko_system.environments_new RENAME TO environments;
ALTER TABLE meko_system.environment_desired_resources_new RENAME TO environment_desired_resources;

-- RENAME TO renames the table, not its constraints, and the names could not be chosen at
-- CREATE time: they share a namespace with the old table's, which still existed then.
-- Corrected here, once the originals are gone -- else the schema keeps an
-- environments_new_pkey that outlives the _new table entirely.
ALTER TABLE meko_system.environments
    RENAME CONSTRAINT environments_new_pkey TO environments_pkey;
ALTER TABLE meko_system.environment_desired_resources
    RENAME CONSTRAINT environment_desired_resources_new_pkey TO environment_desired_resources_pkey;
ALTER TABLE meko_system.environment_desired_resources
    RENAME CONSTRAINT environment_desired_resources_new_env_uuid_fkey
    TO environment_desired_resources_env_uuid_fkey;

-- The enroll hot path, as before.
CREATE INDEX IF NOT EXISTS environments_enroll_token_hash_idx
    ON meko_system.environments (enroll_token_hash);

-- (account_id, id) is an environment's identity, and the conflict target Register upserts
-- on: a name another account holds is a separate row, and re-registering your own name
-- updates it, as registration always did. It replaces the ownership check the upsert had
-- to make by hand while id alone was unique.
CREATE UNIQUE INDEX IF NOT EXISTS environments_account_id_uniq
    ON meko_system.environments (account_id, id);
