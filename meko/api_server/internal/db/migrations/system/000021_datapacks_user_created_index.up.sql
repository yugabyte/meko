/*
Keyset index for GET /datapacks?sort=created: one owner's datapacks newest first,
with datapack_id as the tie-breaker the cursor needs. Without it every list sorts
all of the owner's rows (13 s at 1M datapacks for one account).

Alone in its file so YugabyteDB builds it online. In a multi-statement file the
batch is one implicit transaction and the build falls back to non-concurrent.

All of one owner's entries share one hash value, so they live in one tablet: a
single account's datapack creation rate is bounded by that tablet.
*/
CREATE INDEX IF NOT EXISTS idx_datapacks_user_created
    ON meko_system.datapacks (user_id HASH, created_at DESC, datapack_id DESC)
    INCLUDE (datapack_name);
