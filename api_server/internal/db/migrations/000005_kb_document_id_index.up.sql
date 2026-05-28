/*
 * Secondary index: datapacks.knowledge_bases (tenant_id, document_id).
 *
 * Purpose: enable cheap per-file DELETE on the chunk store.
 *
 * Background. The base table is hash-partitioned by tenant_id with a
 * PRIMARY KEY of ((tenant_id, id) HASH). For per-file deletes the
 * caller knows (tenant_id, document_id) — `id` is the per-chunk PK and
 * is not exposed externally. Without this index, a DELETE
 *
 *     DELETE FROM datapacks.knowledge_bases
 *     WHERE  tenant_id = $1::uuid
 *       AND  document_id = $2::uuid
 *
 * partition-prunes correctly on tenant_id (one of 128 partitions) but
 * then scans the entire partition to find the matching document_id
 * rows. A 100k-chunk datapack spread evenly across 128 partitions still
 * needs ~800 row reads to find the ~10–100 chunks of one file. With
 * this index the same DELETE is a direct (tenant_id, document_id) index
 * range scan — O(chunks_for_this_file), not O(rows_in_partition).
 *
 * The index is also useful for "show chunks of document X" reads from
 * the future per-file inspection UI; same shape, same partition-pruning
 * + range-scan benefit.
 *
 * Note: this is the chunk + embedding store (one row per chunk), NOT
 * the registry meko_system.knowledge_bases. Per-file DELETE never
 * touches the registry — that table is per-source, and one source can
 * cover many files.
 */

CREATE INDEX IF NOT EXISTS knowledge_bases_tenant_document_idx
    ON datapacks.knowledge_bases (tenant_id, document_id);
