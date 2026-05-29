# yb-pageserver

`yb-pageserver` is a fork of `pageserver/` that stores **one YB row per page
version** instead of building delta/image layer files. It exists alongside
the upstream `pageserver` so the two can be benchmarked head-to-head.

This document is the design and roadmap. The corresponding upstream
component is documented in [`pageserver.md`](pageserver.md); read that
first for the parts that are inherited unchanged (Key encoding, walredo,
WAL ingest, getpage RPC, sharding, generation handshake).

## Why a separate crate

The refactor deletes the entire layer-file machinery (~25 K lines:
`storage_layer/`, `layer_map/`, `disk_btree`, `compaction.rs`,
`inmemory_layer`, `remote_timeline_client`, `secondary*`, `heatmap*`,
`eviction_task*`). Doing that destructively on the live `pageserver/`
would forfeit our ability to run head-to-head perf tests. So
`yb-pageserver/` starts as a verbatim copy and the refactor lands inside
it.

The fork shares everything that isn't storage-implementation-specific:
- `pageserver_api` (Key, Lsn, configs, shard identity, controller
  protocol)
- `pageserver_compaction`, `pageserver_client`, `pageserver_client_grpc`,
  `pageserver_page_api` (subcrates under `pageserver/`)
- `wal_decoder` (Value, NeonWalRecord)
- `remote_storage::YsqlClient` (reconnecting YB Postgres-protocol
  wrapper)
- `walredo` (Postgres-binary subprocess for WAL replay)

The binary name is `yb-pageserver`. Builds with
`cargo build -p yb_pageserver --bin yb-pageserver`.

## What replaces what

| Upstream artifact | yb-pageserver replacement |
|---|---|
| `(Key, LSN) → Value` stored as B-tree leaves in delta/image layer files | One row per version in `pageserver.pages` (YB) |
| Open in-memory layer + frozen layer + flush loop | In-RAM write buffer (5 s / 100 MB defaults), flushed as `INSERT … VALUES (…),(…),…` into `pageserver.pages` |
| L0 → L1 compaction, image-layer creation | **Image-anchored reads** + background image materialiser (this file) |
| `index_part.json` (per-timeline layer list) | The `pageserver.timelines` state row |
| `metadata` (local file with ancestor/LSN bookkeeping) | Columns on the `pageserver.timelines` row |
| `tenant-manifest.json` (per-gen layer tracking) | `current_generation` column on the timeline row |
| Local layer cache + secondary cache + heatmap | None — every read hits YB |
| Disk eviction + heatmap downloader | None |

The `getpage` gRPC, basebackup, walreceiver, walingest, walredo, and the
storage-controller handshake are unchanged.

## Schema

Two tables live in the `pageserver` schema inside `yb_storage`,
auto-provisioned by `neon_local` alongside `public.blobs` and
`public.wals`. See [`yugabyte.md`](yugabyte.md) for the full DDL.

### `pageserver.pages`

One row per (Key, LSN). The PK partitions by the **(spcnode, dbnode,
relnode, forknum)** fork identity (each fork's whole block-set lives on
one tablet), then clusters by `(blknum, is_image, lsn DESC)` so a
per-block image lookup is a single seek, not a scan.

```sql
CREATE TABLE pageserver.pages (
    tenant_id    TEXT     NOT NULL,
    timeline_id  TEXT     NOT NULL,
    spcnode      BIGINT   NOT NULL,
    dbnode       BIGINT   NOT NULL,
    relnode      BIGINT   NOT NULL,
    forknum      SMALLINT NOT NULL,
    blknum       BIGINT   NOT NULL,
    is_image     BOOLEAN  NOT NULL,
    lsn          BIGINT   NOT NULL,    -- u64 stored as i64 (real LSNs are << 2^63)
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id, spcnode, dbnode, relnode, forknum) HASH,
                 blknum, is_image, lsn DESC)
);
```

Only **relation-page keys** live here — keys where
`Key::is_rel_block_key()` holds (`field1 == 0x00`, `field4 != 0`,
`field6 != 0xffffffff`). Non-page keys (SLRUs, metadata, dir listings,
aux files, repl origins, rel-size sentinels) continue through the
layer-file path in `public.blobs`.

`is_image` is whether the payload is an 8 KiB Postgres page (vs. a
serialised `NeonWalRecord`). `will_init` mirrors `Value::will_init()` —
true for every image and for `WalRecord` entries that act as a chain
anchor (e.g. `XLOG_HEAP_INIT_PAGE`). With `is_image` in the clustering
key, a single block's rows are split into two sub-clusters; reads anchor
on the latest `is_image=TRUE` row at or below the target LSN and walk
the deltas above it.

### `pageserver.timelines`

One row per timeline. Replaces the upstream split of `metadata` +
`index_part.json` + `tenant-manifest.json`.

```sql
CREATE TABLE pageserver.timelines (
    tenant_id              TEXT    NOT NULL,
    timeline_id            TEXT    NOT NULL,
    last_record_lsn        BIGINT  NOT NULL,
    disk_consistent_lsn    BIGINT  NOT NULL,
    applied_gc_cutoff_lsn  BIGINT  NOT NULL,
    ancestor_timeline_id   TEXT,
    ancestor_lsn           BIGINT,
    current_generation     INT     NOT NULL,
    pg_version             INT     NOT NULL,
    PRIMARY KEY ((tenant_id) HASH, timeline_id)
);
```

- `last_record_lsn` — latest WAL the pageserver has ingested.
- `disk_consistent_lsn` — highest LSN durably present in `pageserver.pages`.
  Advances on every successful flush.
- `applied_gc_cutoff_lsn` — same meaning as upstream; reads below this
  LSN aren't supported.
- `current_generation` — bumped by the storage controller on every
  attach. A stale pageserver's generation-checked write hits a row with
  a newer generation and aborts (see "Generation fencing" below).

## Read path: image-anchored CTE

`Timeline::get_vectored_impl` queries `PageStore` instead of walking
`LayerMap`:

1. For each requested key range at `target_lsn`, call
   `PageWriter::scan_range(range, target_lsn)` (blends in-RAM buffer +
   `PageStore::scan_range`).
2. Per-key reconstruction: chain comes back LSN-descending, pre-truncated
   at the first `will_init=true` row. The bottom row of that chain is
   the anchor — image → walredo base, will_init delta → first record.
3. Drive walredo as today (`Timeline::reconstruct_value`).

The SQL that `PageStore::read_chain` issues (single-key path) is a
two-step CTE:

```sql
-- Fast path: anchor on the latest image at or before target.
WITH anchor AS (
    SELECT lsn AS anchor_lsn
    FROM pageserver.pages
    WHERE <key_filter> AND is_image=TRUE AND lsn <= $target
    ORDER BY lsn DESC LIMIT 1
)
SELECT … FROM pageserver.pages p, anchor
WHERE <key_filter>
  AND p.lsn >= anchor.anchor_lsn AND p.lsn <= $target
ORDER BY p.lsn DESC;
```

With `is_image` in the clustering key the anchor seek is `O(log B)`
where `B` is the # of image rows for that block — typically 1. If no
image exists (`anchor` CTE empty), `read_chain` falls back to a plain
LSN-descending scan and walks rows until it hits a `will_init=true`
delta. Correct in all cases; the fallback is the only "slow" path and
only fires for cold blocks the background image materialiser hasn't
visited yet.

Reads above `last_record_lsn` block on `wait_lsn`. Reads below
`applied_gc_cutoff_lsn` return an error.

## Write path: pure passthrough

Replaces `InMemoryLayer` + `flush_loop` + L0/L1 compaction.

1. `walingest` produces `(Key, LSN, Value)` triples via
   `pgdatadir_mapping::DatadirModification.put`. Unchanged.
2. `DatadirModification.commit` calls `TimelineWriter::put_batch`, which
   forwards each batch to `PageWriter::put(entries)`.
3. `PageWriter::put` filters to relation-page keys (`is_rel_block_key`),
   converts each `Value` into a `PageRow` (`Value::Image` →
   `is_image=true, will_init=true`; `Value::WalRecord` → `is_image=false`
   with `will_init` taken from the record), and appends to the in-RAM
   buffer. **No walredo, no `ImageRule`** — writes are a straight
   serialise-and-buffer.
4. Buffered rows live in an in-RAM `Vec<PageRow>` (per timeline) until
   a flush threshold (default 5 s / 100 MiB) fires.
5. Flush issues one `INSERT INTO pageserver.pages VALUES (…),(…),…`
   per batch, gen-checked via a conditional UPDATE on
   `pageserver.timelines` in the same transaction (see "Generation
   fencing"). On success, `disk_consistent_lsn` advances on the
   timeline row.
6. Reads between flushes can see in-buffer rows: the read path checks
   the buffer first, then queries PageStore.

Default knobs (configurable via `pageserver.toml` under the
`yb_page_writer` block):

| Knob | Default | Meaning |
|---|---:|---|
| `flush_interval` | 5 s | Time-based flush trigger |
| `flush_size_bytes` | 100 MiB | Size-based flush trigger |
| `flush_poll_interval` | 500 ms | How often the background flusher checks the triggers |

## Background image materialiser (planned)

The write path no longer materialises images, so chain length grows
unbounded for hot blocks until the background task creates a fresh image.
Reads stay correct (slow-path walks to the latest `will_init` delta);
this is pure read-latency optimisation.

Planned behaviour:

1. Periodic sweep over `pageserver.pages` per timeline.
2. Identify keys with > N rows since their last image row (or no image
   at all).
3. For each, walredo the chain at the latest LSN and INSERT a fresh
   image row.
4. Optional follow-up: GC older images / deltas under
   `applied_gc_cutoff_lsn`.

Until this lands, expect long chains for hot blocks; reads still work,
just with more rows scanned and more walredo records to apply.

## Generation fencing

`current_generation` on the `pageserver.timelines` row is the gate.
Sequence:

1. Storage controller's attach decision bumps the generation. yb-pageserver
   learns the new gen from the attach payload.
2. yb-pageserver opens the timeline: reads the current state row, asserts
   `row.current_generation == attach_gen`.
3. Each flush transaction wraps the `INSERT INTO pages …` in a
   `BEGIN; UPDATE pageserver.timelines SET disk_consistent_lsn = $new
   WHERE tenant_id=$1 AND timeline_id=$2 AND current_generation =
   $expected; … COMMIT`. If the `UPDATE`'s row count is 0, the gen has
   moved; the flush aborts and the pageserver tears down the timeline.

This collapses the upstream "every layer file carries its gen in the
name + every delete is gen-checked" machinery into one column.

## What yb-pageserver does NOT do

- **Compaction** — none. Image-layer creation moves from synchronous
  compaction to an async background image materialiser (see above). The
  `pageserver_compaction` crate is still a build-time dep so the fork
  stays buildable; it's unused at runtime.
- **Local layer cache** — none. Every read hits YB. If profiling later
  shows a `(key, lsn) → page` LRU would help, that's a follow-up — but
  it'd live above PageStore, not as a copy of the upstream file-cache.
- **Disk eviction / heatmap / secondary cache** — none. No local files
  to evict.
- **Layer-file generations** — gone. Only the `current_generation`
  column on the timeline row matters.
- **GC** — not implemented yet. The plan is a separate cron that
  prunes `pageserver.pages` rows older than `applied_gc_cutoff_lsn`
  per key, retaining at least one image per chain. Until that lands,
  rows accumulate indefinitely.

## Implementation status

Tracked phases:

| Phase | Status | Description |
|---|---|---|
| 0 | ✅ done | Fork `pageserver/` → `yb-pageserver/`, rename crate + binary. |
| 1 | ✅ done | `pageserver.pages` + `pageserver.timelines` schema, `PageStore` Rust module (`yb-pageserver/src/page_store.rs`) with full CRUD + generation helpers and 15 integration tests against a real YB. |
| 2a | ✅ done | `ImageRule` 10-delta enforcement logic (`yb-pageserver/src/image_rule.rs`) with 12 unit tests. Pure logic, no walredo or DB dep. |
| 2b | ✅ done | `PageWriter` (`yb-pageserver/src/page_writer.rs`): buffered write path that applies `ImageRule`, calls a `Materializer` trait on `ForceImage`, blends buffered + persisted rows on read (`read_chain`), and flushes on a size/age trigger (defaults 100 MiB / 5 s). 12 integration tests including a long-workload property test that scans every LSN to confirm the LIMIT-10 invariant holds. |
| 3a | ✅ done | `Materializer` trait refactored: now takes `(key, target_lsn, prior_chain, current)` so production wrappers don't need a back-reference to `PageWriter`. `WalRedoMaterializer` (`yb-pageserver/src/page_materializer.rs`) is the production impl — converts chain rows back into `(base_img, records)` and calls `walredo::PostgresRedoManager::request_redo`. 5 tests cover the pure decode helper. Walredo-driven end-to-end test deferred to Phase 3b wiring (needs a real PG subprocess). |
| 3a+ | ✅ done | Two more primitives that Phase 3b will reach for: `PageWriter::spawn_flusher(tick)` returns a `FlushTask` (JoinHandle + CancellationToken) for the per-timeline background flusher, doing a final `flush()` on cancel; `extract_triples(SerializedValueBatch)` decodes a walingest batch into the `(Key, Lsn, Value)` triples `PageWriter::put` consumes. Both with tests. |
| 3b | ✅ done | Tenant/Timeline plumbing: `TenantSharedResources` carries `Option<Arc<PageStore>>`, `Tenant` stashes it, `TimelineResources` passes through, `Timeline` builds `PageWriter` + spawns `FlushTask` when both `page_store` and `walredo_mgr` are present. `TimelineWriter::put_batch` runs `extract_triples` and forwards to `page_writer.put` before the layer-file write (so a YB failure aborts the whole batch and walingest retries). `Timeline::shutdown` drains the FlushTask + does a final `page_writer.flush()`. **Reads still go through the layer stack** — Phase 3c is the read-flip. |
| 3c | ✅ done | `Timeline::get_vectored_impl` branches on `self.page_writer`: when yb-pageserver mode is active, reads go through `get_vectored_via_page_writer` — per-key parallel `PageWriter::read_chain` → `chain_to_value_reconstruct_state` (image-anchor → img, deltas → records) → `Timeline::reconstruct_value` (walredo). The LayerMap walk is bypassed entirely. Upstream pageserver behaviour preserved when `page_writer` is None. Layer-file path is now dead code from the read perspective; still populated by 3b dual-write, both removed by Phase 4. |
| 3c+ | ✅ done | `yb_page_writer` config block in `ConfigToml` (3 knobs: flush_interval, flush_size_bytes, flush_poll_interval). |
| 3d | ✅ done | **Schema reshape + design pivot.** `pageserver.pages` PK becomes `((tenant…forknum) HASH, blknum, is_image, lsn DESC)`: typed columns + per-block image clustering. Reads become an image-anchored CTE (fast path: latest image ≤ target + deltas above; slow path: walk to `will_init` delta). Writes drop all walredo at write time — pure passthrough. `image_rule.rs` + `page_materializer.rs` deleted. The "10-delta invariant" stops being a write-time contract; chain bounding moves to a background image materialiser (Phase 3e). End-to-end verified with `CREATE TABLE / INSERT 1000 / UPDATE×3 / SELECT` against compute; long chains (400+ rows on hot blocks) read correctly via the slow path. |
| 3e | ⏳ pending | Background image materialiser: per-timeline task that scans `pageserver.pages`, picks keys with long chains, walredos to a fresh image at the latest LSN, INSERTs the image row. Pure optimisation — correctness is already guaranteed without it. |
| 4 | ⏳ pending (user said "test before deleting") | Delete the layer stack: `storage_layer/`, `layer_map/`, `disk_btree`, `compaction.rs`, `inmemory_layer`, `secondary*`, `heatmap*`, `eviction_task*`, `init.rs`, most of `remote_timeline_client/`. ~25 K LOC removal. After 3c, the layer-file path is dead code from the read perspective; 3b's dual-write to layer files keeps it populated until this phase removes it. |
| 5 | ⏳ pending | Replace `metadata` / `index_part.json` / `tenant-manifest.json` with reads/writes against `pageserver.timelines`. Rewrite attach. |
| 6 | ⏳ deferred | GC: cron that prunes old `pages` rows. User said "figure out separately later". |

## Testing dual-write (Phase 3b verification)

Phase 3c isn't done yet — reads still go through the layer stack. The
goal of testing now is to confirm `pageserver.pages` is being populated
correctly, so when Phase 3c flips reads they Just Work.

### Run it

`yb-pageserver` is a drop-in replacement for `pageserver` as far as
config goes — point a `neon_local` config at the
`target/release/yb-pageserver` binary instead of
`target/release/pageserver`, with `remote_storage.kind = "YbStore"`
pointing at the same YB cluster the rest of yb-light uses.

On startup you should see:

```
PageStore connected; dual-write enabled
```

If you don't, the binary fell back to upstream behaviour — check that
`remote_storage` is `YbStore` (LocalFs / S3 / etc. won't trigger
dual-write).

### Per-tenant verification queries

After a few minutes of WAL ingest:

```sql
\c yb_storage

-- Row count by tenant/timeline.
SELECT tenant_id, timeline_id,
       count(*) AS rows,
       count(*) FILTER (WHERE is_image)   AS images,
       count(*) FILTER (WHERE will_init)  AS anchors,
       pg_size_pretty(sum(length(payload))::bigint) AS bytes
FROM pageserver.pages
GROUP BY tenant_id, timeline_id
ORDER BY rows DESC;

-- Top 10 hottest blocks (chain length per block).
SELECT tenant_id, timeline_id, spcnode, dbnode, relnode, forknum, blknum,
       count(*) AS chain_len,
       sum(CASE WHEN is_image THEN 1 ELSE 0 END) AS imgs,
       sum(CASE WHEN will_init THEN 1 ELSE 0 END) AS anchors
FROM pageserver.pages
GROUP BY tenant_id, timeline_id, spcnode, dbnode, relnode, forknum, blknum
ORDER BY chain_len DESC
LIMIT 10;

-- Blocks with no image at all — these hit the slow read path
-- (full chain walk to the latest will_init delta). Expected for
-- freshly-written hot blocks until the background image materialiser
-- (Phase 3e) processes them.
SELECT tenant_id, timeline_id, spcnode, dbnode, relnode, forknum, blknum,
       count(*) AS chain_len
FROM pageserver.pages
GROUP BY tenant_id, timeline_id, spcnode, dbnode, relnode, forknum, blknum
HAVING sum(CASE WHEN is_image THEN 1 ELSE 0 END) = 0
ORDER BY chain_len DESC
LIMIT 10;
```

### Compare layer files vs. PageStore

Quick spot-check: pick a key from a delta layer in
`<workdir>/pageserver_*/tenants/<tenant>-…/timelines/<timeline>/…` and
look up the same `(tenant, timeline, key)` in `pageserver.pages`. The
delta layer's `(Key, LSN) → Value` entries should appear as
`pageserver.pages` rows with matching LSN and `is_image=false`. Image
layers correspond to rows with `is_image=true`.

### Smoke-test outside neon_local

To validate end-to-end without a full dev cluster: write a small
script that uses `tokio-postgres` to scan `pageserver.pages` while
pgbench runs against the compute endpoint. Row count + max LSN should
both climb steadily.

## Code anchors

- `yb-pageserver/src/page_store.rs` — `PageStore` row store; the
  image-anchored `read_chain` CTE + `scan_range`.
- `yb-pageserver/src/page_writer.rs` — `PageWriter` buffered write path;
  pure passthrough (no walredo at write time); `read_chain` /
  `scan_range` blend in-RAM buffer + PageStore.
- `yb-pageserver/src/tenant/timeline.rs` — `get_vectored_via_page_writer`
  drives the row-store read path; `chain_to_value_reconstruct_state`
  converts a chain into walredo inputs.
- `control_plane/src/storage_controller.rs::ensure_yb_storage_schema`
  — DDL bootstrap for `pageserver` schema.
- `architecture/yugabyte.md` — full DDL reference for `pageserver.pages`
  and `pageserver.timelines`.
