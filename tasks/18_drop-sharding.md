---
status: pending
severity: medium
area: yb-pageserver
opened: 2026-06-03
---

# Drop tenant-sharding machinery

## Motivation

Upstream Neon shards a tenant's keyspace across multiple pageservers
to scale write throughput beyond one PS's ingest rate. The shard
count is encoded in `ShardIdentity` / `TenantShardId` and threads
through almost every API:

- `TenantShardId = (TenantId, ShardNumber, ShardCount)` — the
  identity primitive used for tenant lookup, HTTP routing, metrics
  labels, log spans, etc.
- `ShardIdentity { count, stripe_size, layout, … }` carried inside
  `LocationConf`, used to compute `key → shard` mappings.
- Shard-split / shard-migrate code paths in storage_controller.
- `is_key_local_to_shard` / `shard_for_key` checks scattered through
  the walreceiver and read paths.

The yb-pageserver runs as a single shard per tenant (the
`image_materializer` span pins `shard_id = "0000"` —
`pageserver/src/image_materializer.rs:172`) because YugabyteDB
already shards the keyspace at the tablet layer. There's no scaling
win from pageserver-level sharding on top of that; it's redundant
machinery whose only function in this fork is to drag a 4-byte
`ShardNumber + ShardCount` through every type signature.

## Scope

1. Replace `TenantShardId` with `TenantId` everywhere. Keep a
   `TenantShardId` type alias as a deprecation shim only if the
   removal touches too many crates to land in one PR; otherwise
   delete outright.
2. Delete `ShardIdentity` from `LocationConf` (see also
   [task 17](17_move-tenant-conf-to-yb.md)).
3. Delete `is_key_local_to_shard`, `shard_for_key`, `ShardStripeSize`,
   `ShardLayout`, and the rest of `libs/pageserver_api/src/shard.rs`.
4. Strip shard fields from the storage_controller schema (Diesel
   migration that drops the columns) and from its tenant/timeline
   types.
5. Strip shard fields from the page_service / compute wire protocol
   if any are exchanged (likely some HTTP routes carry shard params).
6. Audit `pageserver.timeline_state` / `pageserver.pages` /
   `pageserver.meta` schemas — none should carry a shard column,
   but verify.
7. Update / delete shard-related tests, fixtures, and `neon_local`
   CLI flags.

## Notes

This is a large mechanical refactor with a small semantic change.
Expected diff: deep negative net (hundreds of lines deleted across
storage_controller + pageserver + libs).

Risk: any code path that *still implicitly assumes* sharding (e.g.
log spans hardcoding `shard_id = "0000"`) will stop emitting that
label — fine, but worth a search for downstream Grafana / log query
consumers before landing.

Cross-link [task 17](17_move-tenant-conf-to-yb.md).
