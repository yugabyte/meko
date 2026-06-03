---
status: pending
severity: high
area: yb-pageserver
opened: 2026-06-02
---

# `pageserver.meta` single-key read hotness

## Symptom

On a non-test cluster the per-key meta SELECT is the top-N query
in pg_stat_statements:

```
query_id=-7082480392124032465; dbname=yb_storage
query:
  SELECT key, lsn, payload
  FROM "pageserver".meta
  WHERE tenant_id=$1 AND timeline_id=$2 AND key=$3 AND lsn <= $4
  ORDER BY lsn DESC LIMIT $5
calls/s:       535.39
distribution:  100.0 % across 3 nodes  (lands on one node)
total time:    20836 ms  (14.37 %)
mean time:     0.65 ms
rows/call:     1.0
```

`LIMIT $5` is pg_stat_statements normalising the literal `LIMIT 1`
that our code issues.

## Where it comes from

Single call site, single SQL: `PageStore::read_meta`
(`pageserver/src/page_store.rs:1097-1127`). Caller chain on the
read path:

- compute getpage RPC
- → `Timeline::get_vectored` (`pageserver/src/tenant/timeline.rs`)
- → `PageWriter::scan_meta_range` (`pageserver/src/page_writer.rs:1146`)
- → `PageStore::scan_meta_range` (`pageserver/src/page_store.rs:1132`)
- → single-key fast path (`page_store.rs:1149`) → `read_meta`

Every get_vectored that touches a meta key — i.e. anything not
routed to `pageserver.pages` or `pageserver.slru_pages` — turns
into one synchronous round-trip to YB. No caching on the
pageserver side.

## What keys land here

The router in `page_writer.rs:437-443` puts a row in
`pageserver.meta` when the key is **not** `is_rel_block_key` and
**not** `is_slru_block_key`. That covers (constructors in
`libs/pageserver_api/src/key.rs`):

- **`rel_size_to_key`** (`:565`) — per-relfork nblocks. **Hottest
  candidate.** Postgres calls `smgrnblocks` constantly during
  INSERT/UPDATE; compute-side relsize cache invalidates on every
  WAL replay landing on that rel.
- **`rel_dir_to_key`** (`:511`) / **`DBDIR_KEY`** (`:470`) — per-DB
  rel list, global DB list. Catalog/relation-open path.
- **`relmap_file_key`** (`:499`) — pg_filenode.map per DB.
- **`TWOPHASEDIR_KEY`** (`:712`) + per-xid twophase entries.
- **`CONTROLFILE_KEY`** (`:756`) — pg_control.
- **`CHECKPOINT_KEY`** (`:765`) — checkpoint record.
- **`slru_dir_to_key`** (`:605`) — per-SLRU-kind segment directory.
- **`slru_segment_size_to_key`** (`:656`) — per-SLRU-segment size
  (excluded from `is_slru_block_key` via field6 = `0xffffffff`).
- **`AUX_FILES_KEY`** (`:774`) + the sparse aux / replorigin range
  (`AUX_KEY_PREFIX 0x60` … `REPL_ORIGIN_KEY_PREFIX 0x63`,
  `:786`-`:818`) — logical-replication slots, replorigin state,
  custom aux files.

## Hypothesis

Almost all of the 535 cps is **relsize lookups**. The 100 %-on-one-node
distribution is consistent with the hash partition key
(`tenant_id, timeline_id, key`) routing all meta keys for a
single timeline to one tablet leader, so every backend in that
timeline funnels its meta reads to the same YB node.

The fact that mean latency is 0.65 ms and rows/call is 1.0 means
the query itself is doing the right thing per-call — the problem
is volume, not per-call cost.

## Plan (scoping)

1. **Confirm the key distribution.** Add a sampled log
   (`tracing::trace!` gated by an env var, or a counter
   bucketed by `(field1, is_relsize)`) in `read_meta` for the
   first N seconds of a session. Expected to show relsize as the
   dominant kind.

2. **Quantify the redundancy.** Two flavors:
   - Same `(tenant, timeline, key, target_lsn)` re-read by
     concurrent backends within seconds — pure cacheable.
   - Same `(tenant, timeline, key)` re-read at monotonically
     advancing `target_lsn` with no intervening writes to that
     key — also cacheable, with last-write-LSN invalidation.

3. **Read-through cache on the writer side.** Small per-timeline
   LRU keyed by `key_bytes`, value = (latest durable lsn, payload,
   horizon at which we observed it). Invalidate on next put for
   the key (we already have the put hook). Should collapse
   relsize 535→~0 cps under steady-state write traffic.

4. **Coalesce concurrent in-flight reads.** Even without a
   durable cache, `singleflight`-style dedup of in-flight
   `read_meta` calls for the same `(key, target_lsn)` would
   eliminate the thundering-herd case across backends.

5. **Schema angle.** Goes hand-in-hand with task 14
   ([data-modeling review](14_data-modeling-review.md)) — the
   "drop `lsn DESC` clustering and maintain a per-key latest
   row" proposal is the same problem from the storage side. If
   the cache fix lands first, the schema decision may change.

## Open questions

- Are read-side meta calls ever issued at an arbitrary historical
  LSN, or always at "latest available"? If always-latest, a
  simpler `read_meta_latest(key)` API drops the LSN bound
  entirely and avoids the range-scan clustering altogether.
- For relsize specifically: can the compute-side relsize cache
  be made authoritative against pageserver via a streamed
  invalidation channel instead of polled lookups? This is the
  upstream Neon design question, not just a YB-fork concern.

## Notes / links

- Same physical table as task 14's section on `pageserver.meta`;
  this task focuses on read-path traffic, that one on schema
  shape. Cross-link.
- The 100 %-on-one-node skew is a separate hot-tablet finding
  worth feeding into task 14's partition-key discussion.
