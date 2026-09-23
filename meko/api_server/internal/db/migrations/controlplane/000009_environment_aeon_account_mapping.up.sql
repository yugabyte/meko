/*
 * Sticky (storage_pool, region) -> environment assignment -- control-plane-mode
 * migration set.
 *
 * The region-addressed resource APIs name a storage pool and a region, never an
 * environment. A region matches more than one MANAGED environment, so the first call
 * for a (storage_pool, region) has to pick one and every later call has to reuse that
 * same one -- otherwise a pool's clusters would scatter across environments. This
 * table is that record: unique on (storage_pool_name, region), so a pool pins to at
 * most one environment per region while remaining free to sit in a different one in
 * another region.
 *
 * Keyed to environments(uuid), not the id slug: id is unique only per account (the
 * same reason environment_desired_resources and environment_tasks key on uuid). The
 * FK cascades on delete so a removed environment cannot leave a dangling mapping;
 * withdrawing an environment from service is a soft state change (DEREGISTERED), which
 * the resolver treats as a miss and re-pins, so a live row here is never orphaned.
 *
 * Guarded with IF NOT EXISTS -- migrations run on every boot.
 *
 * Portable SQL only -- this set has to run against a plain PostgreSQL.
 */
CREATE TABLE IF NOT EXISTS meko_system.environment_aeon_account_mapping (
    id                UUID NOT NULL DEFAULT gen_random_uuid(),
    environment_id    UUID NOT NULL REFERENCES meko_system.environments (uuid)
                        ON DELETE CASCADE ON UPDATE CASCADE,
    region            TEXT NOT NULL,
    storage_pool_name VARCHAR(128) NOT NULL,
    created_on        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_on        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_environment_aeon_account_mapping PRIMARY KEY (id)
);

-- One environment per (storage_pool, region): the uniqueness the resolver relies on,
-- and the conflict target its lookup-or-assign upsert uses.
CREATE UNIQUE INDEX IF NOT EXISTS environment_aeon_account_mapping_pool_region_uniq
    ON meko_system.environment_aeon_account_mapping (storage_pool_name, region);

-- The FK's own lookup path, and how a deleted environment finds its rows to cascade.
CREATE INDEX IF NOT EXISTS environment_aeon_account_mapping_env_idx
    ON meko_system.environment_aeon_account_mapping (environment_id);
