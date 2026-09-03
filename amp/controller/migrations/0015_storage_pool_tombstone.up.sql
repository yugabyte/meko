-- Deleting a storage pool tombstones it, the way a cluster or a branch is
-- tombstoned: a pool's clusters are themselves tombstoned and keep their
-- storage_pool_id FK, so the pool row has to outlive them.
--
-- The partial name index this column exists for is 0016, NOT this file: on
-- YugabyteDB a CREATE INDEX inside a transaction block is downgraded to a
-- non-concurrent build whose backfill does not see rows written earlier in the
-- same transaction (yugabyte-db#6240), and golang-migrate runs each file in one.
-- 0014 was split out of 0013 for exactly this reason.
ALTER TABLE amp_registry.storage_pools
    ADD COLUMN IF NOT EXISTS dropped_at timestamptz;

-- Drop the whole-table UNIQUE on name: it spans tombstones, so it would let a
-- deleted pool hold its name forever. Both forms are dropped because the down
-- migration restores it as a plain index (ADD CONSTRAINT has no IF NOT EXISTS),
-- so a down-then-up round trip has an index here rather than a constraint.
ALTER TABLE amp_registry.storage_pools
    DROP CONSTRAINT IF EXISTS storage_pools_name_key;
DROP INDEX IF EXISTS amp_registry.storage_pools_name_key;
