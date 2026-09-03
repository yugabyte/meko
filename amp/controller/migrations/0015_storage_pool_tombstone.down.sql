-- Restored as an index, not a constraint: ADD CONSTRAINT has no IF NOT EXISTS,
-- and every statement here has to be re-runnable. Fails if two tombstoned pools
-- share a name, which the partial index permits and this one cannot describe.
CREATE UNIQUE INDEX IF NOT EXISTS storage_pools_name_key
    ON amp_registry.storage_pools (name);
ALTER TABLE amp_registry.storage_pools
    DROP COLUMN IF EXISTS dropped_at;
