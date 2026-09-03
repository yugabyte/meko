-- Name unique among LIVE pools only, so deleting a pool frees its name for reuse
-- (clusters_name_per_pool already does this for cluster names).
--
-- SEPARATE from 0015 as a precaution, following the 0013/0014 split: on
-- YugabyteDB a CREATE INDEX inside a transaction block is downgraded to a
-- non-concurrent build (yugabyte-db#6240) whose backfill does not see rows
-- written earlier in that same transaction, and golang-migrate runs each file in
-- one. 0015 writes no rows -- ADD COLUMN with no default, DROP CONSTRAINT -- so
-- unlike 0013's data repair there is nothing for the backfill to miss, and
-- TestMigrateStoragePoolNameLiveCoversExistingPools passes with the two merged.
-- The split is kept anyway because the cost is one file and the failure it
-- forecloses is silent: the DDL succeeds either way, and an index missing live
-- rows lets two live pools hold one name, which is the invariant this index IS.
CREATE UNIQUE INDEX IF NOT EXISTS storage_pools_name_live
    ON amp_registry.storage_pools (name) WHERE dropped_at IS NULL;
