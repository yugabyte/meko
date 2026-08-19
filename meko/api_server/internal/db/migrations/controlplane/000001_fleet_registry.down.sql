/*
 * Drops only what the up migration created. The meko_system schema is left in
 * place: on a shared database it predates this set and other tables live in it.
 */
DROP TABLE IF EXISTS meko_system.environment_desired_resources;
DROP INDEX IF EXISTS meko_system.environments_enroll_token_hash_idx;
DROP INDEX IF EXISTS meko_system.environments_account_id_uniq;
DROP TABLE IF EXISTS meko_system.environments;
