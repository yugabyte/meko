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
| `index_part.json` (per-timeline layer list) | The `pageserver.timeline_state` state row |
| `metadata` (local file with ancestor/LSN bookkeeping) | Columns on the `pageserver.timeline_state` row |
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

### `pageserver.timeline_state`

One row per timeline. Replaces the upstream split of `metadata` +
`index_part.json` + `tenant-manifest.json`.

```sql
CREATE TABLE pageserver.timeline_state (
    tenant_id              TEXT    NOT NULL,
    timeline_id            TEXT    NOT NULL,
    disk_consistent_lsn    BIGINT  NOT NULL,
    applied_gc_cutoff_lsn  BIGINT  NOT NULL,
    ancestor_timeline_id   TEXT,
    ancestor_lsn           BIGINT,
    current_generation     INT     NOT NULL,
    pg_version             INT     NOT NULL,
    PRIMARY KEY ((tenant_id) HASH, timeline_id)
);
```

- `disk_consistent_lsn` — highest LSN durably present in `pageserver.pages`/
  `slru_pages`/`meta`. Advances on every successful `PageWriter::flush`.
  Drives both safekeeper WAL truncation and ingest resume on restart.
  `last_record_lsn` is intentionally not persisted — on restart, the
  walreceiver streams from `disk_consistent_lsn` forward and the
  in-memory value re-derives within seconds.
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
4. Rel-page rows live in `UnifiedPages` — one in-RAM structure that
   indexes both pre-flush (pinned in RAM until durable) and post-flush
   (LRU-ish cache for read-latency) entries. SLRU and meta entries
   stay in their own simpler `Vec` buffers for now. Everything is
   under one `RwLock<WriterState>`: `put` and the flush cursor flip
   take `.write()`; all readers (`read_chain`, `read_chain_durable`,
   `scan_range`, `read_meta`, `should_flush`, `stats`) take `.read()`.
   Concurrent get_page handlers filter the cache in parallel. Flushes
   fire when either trigger crosses threshold (default 5 s / 25 MiB).
5. Flush snapshots the unflushed prefix of `UnifiedPages` (no copy of
   row data — just clones the rows to a Vec) plus the SLRU / meta
   buffers, drops the lock, and dispatches chunked multi-row INSERTs
   against `pageserver.pages` / `slru_pages` / `meta`. Chunks within
   each table go in parallel via `stream::iter(...).buffer_unordered(N)`
   with N=4/2/3. `yb_enable_upsert_mode = true` is `SET` once per
   connection in `YsqlClient::open_*`, so a chunk retry after a
   partial-flush failure can't PK-conflict. On success, `UnifiedPages`
   flips the snapshot's rows from unflushed → durable in one cursor
   pass (the rows do NOT move; they stay in the cache for reads) and
   then evicts durable entries FIFO if `total_bytes > read_cache_bytes`.
   SLRU / meta drain as before.
6. Reads between flushes (and DURING a flush) see all in-RAM rows —
   both pre-flush and durable cache — blended with PageStore via
   `read_chain` / `scan_range` (dedup by LSN, truncate at the first
   `will_init=true` row). Because flushes don't move the data, in-flight
   INSERTed rows remain visible.
7. Background image materialiser: when `PageWriter::put` observes a
   key crossing `image_chain_limit` consecutive non-image, non-`will_init`
   deltas, it `try_send`s a `MaterializeJob { key, target_lsn }` to a
   bounded mpsc. The materialiser task drains the queue: per job it
   calls `read_chain_durable` (cache + YB, **never** pre-flush — see
   below), walredos to a fresh page image, `put_pages` the image to
   YB, then `install_materialized_image` to trim the cache for that
   key up to the image LSN and push the new image in as a durable
   cache entry. Subsequent reads at LSN ≥ image_lsn anchor on the
   image instead of walking back to the latest `will_init` delta.

Why the materialiser uses **durable-only** reads: if it built an
image from pre-flush rows and the process crashed, WAL replay would
re-emit those deltas — but the image is already in YB at the same
LSN. Reads at LSN > image_lsn would then walredo the re-emitted
deltas on top of the image, double-applying them. `read_chain_durable`
restricts the materialiser's inputs to data that survives crash +
replay.

Default knobs (configurable via `pageserver.toml` under the
`yb_page_writer` block):

| Knob | Default | Meaning |
|---|---:|---|
| `flush_interval` | 5 s | Time-based flush trigger |
| `flush_size_bytes` | 25 MiB | Size-based flush trigger |
| `flush_poll_interval` | 500 ms | How often the background flusher checks the triggers |
| `backpressure_bytes` | 50 MiB | Hard ceiling on the pre-flush buffer. `PageWriter::put` blocks until a flush drains below it. `0` disables (legacy unbounded growth) |
| `image_chain_limit` | 20 | Deltas-since-anchor before a key is queued for materialisation (`0` disables count trigger) |
| `image_chain_payload_bytes_limit` | 4 KiB | Combined-delta-payload trigger for materialisation (`0` disables size trigger) |
| `materialize_queue_capacity` | 5000 | Bounded mpsc depth; the write-side dispatch uses **blocking** send (backpressures walreceiver); the read-side dispatch uses `try_send` (drops on overflow — the next read of the same key re-evaluates) |
| `read_cache_bytes` | 1 GiB | Soft cap on `UnifiedPages.total_bytes`. Durable entries are evicted FIFO when crossed; pre-flush entries are pinned regardless |

## Backpressure

The walreceiver has no built-in throttle: it ingests as fast as the
safekeeper sends. Two places in `PageWriter::put` block the walreceiver
when a downstream consumer is the bottleneck. Both keep heap usage
bounded under sustained CPU/IO pressure that would otherwise grow the
in-RAM buffer without limit.

**Flush-side backpressure.** If `unflushed_bytes + bytes_in_aux_buffers`
exceeds `FlushPolicy::backpressure_bytes` (operator knob
`backpressure_bytes`, default 50 MiB), `put` awaits a Tokio `Notify`
pulsed at the end of every successful `flush()`. The size-based flush
trigger (`max_bytes`, default 25 MiB) fires well before the
backpressure cap is reached, so under steady state the wait path is
not exercised — it only kicks in when YB INSERTs against
`pageserver.pages` / `slru_pages` / `meta` slow down (CPU contention,
network spikes, tablet split). Pre-flush rows are **pinned** in
`UnifiedPages` until durable, so without this cap the buffer would
grow linearly with the YB backlog — we've observed multi-GiB RSS
growth on co-located deployments under CPU pressure.

Set `backpressure_bytes = 0` to opt out (legacy unbounded growth).

**Materialiser-side backpressure.** The write-time materialisation
trigger does a **blocking** `mpsc::send` into the materialiser channel
(capacity `materialize_queue_capacity`, default 5000). When the
materialiser can't drain — usually because its own `read_chain_durable`
+ walredo + `put_pages` chain is bottlenecked on the dedicated YSQL
connection — the channel fills and the walreceiver blocks at the
send. This bounds in-flight materialisation work to ~5000 keys ×
(chain payload + image), keeping walredo memory under control even
when materialisation lags badly.

The read-side materialisation trigger uses `try_send` instead: a
GetPage that just walredo'd a long chain is allowed to enqueue an
image materialisation hint, but if the channel is full the hint is
silently dropped — the next read of the same hot key will re-trigger.
GetPage latency must not depend on materialiser progress.

### Observability

Three Prometheus metrics expose the backpressure state:

| Metric | Type | What to watch for |
|---|---|---|
| `pageserver_yb_pagewriter_backpressure_waits_total{kind="flush"}` | counter | sustained non-zero rate ⇒ YB INSERT throughput is below ingest. Either raise `backpressure_bytes` (delays the symptom, doesn't fix it), scale YB, or shed load |
| `pageserver_yb_pagewriter_backpressure_waits_total{kind="materializer"}` | counter | sustained non-zero rate ⇒ materialiser is the bottleneck. Either raise `materialize_queue_capacity` (only if memory permits) or relax `image_chain_limit` / `image_chain_payload_bytes_limit` to dispatch less aggressively |
| `pageserver_yb_pagewriter_unflushed_bytes` | gauge | should hover well below `backpressure_bytes`; persistent climb toward the cap = flush is lagging |
| `pageserver_yb_materializer_queue_depth` | gauge | should hover well below `materialize_queue_capacity`; climb toward the cap = materialiser is lagging |

When both `flush` and `materializer` counters are active, the flush path is
almost always the root cause: a slow `put_pages` blocks the
materialiser's own writes, which holds materialiser slots, which fills
the channel.

## Background image materialiser

Triggered **at write time**: when `PageWriter::put` observes a key
accumulating `image_chain_limit` consecutive non-image, non-`will_init`
deltas (counter resets on any image / will_init for the key, per-key
state under the same RwLock as the buffer). On cross, a
`MaterializeJob { key, target_lsn }` is `try_send`'d to a bounded mpsc.

The worker task (`ImageMaterializer::spawn`, one per timeline) drains
the queue:

1. `PageWriter::read_chain_durable(key, target_lsn)` — durable sources
   only (cache + PageStore), excluding pre-flush rows. Returns the
   LSN-descending chain truncated at the newest `will_init=TRUE`.
2. If chain is empty (deltas not yet flushed to YB), skip; the next
   post-flush trigger picks the key up.
3. If the chain head is already an image at `lsn ≥ target_lsn`,
   someone else materialised — skip.
4. `parse_chain` splits the chain into `(base_img, records)`. Set
   `actual_lsn = chain.first().lsn` (highest durable LSN we actually
   have — may be < `target_lsn` if some triggering deltas aren't yet
   durable; we make partial progress and re-trigger later).
5. `WalRedoManager::request_redo(key, actual_lsn, base_img, records,
   pg_version, RedoAttemptType::LegacyCompaction)` → 8 KiB image.
6. `PageStore::put_pages` the image row to YB. Durable.
7. `PageWriter::install_materialized_image` trims the cache for that
   key up to `actual_lsn` (dropping the now-superseded deltas — they
   stay in YB until GC) and `push_durable`s the new image into the
   cache. Future reads anchor on the image directly.

Failure / restart behaviour: on any error the job is logged and
dropped; the next delta past the (now-reset) counter re-enqueues. On
process restart, per-key counters start at zero — restart-survivor
keys with long chains keep reading via the slow path until 10 more
deltas trigger materialisation. A periodic backstop scan could close
that gap later; not in v1.

GC of the superseded deltas in YB is Phase 6 — until then, the row
count grows. The materialiser pays only for read-latency wins; size
on disk grows monotonically.

## Generation fencing

`current_generation` on the `pageserver.timeline_state` row is the gate.
Sequence:

1. Storage controller's attach decision bumps the generation. yb-pageserver
   learns the new gen from the attach payload.
2. yb-pageserver opens the timeline: reads the current state row, asserts
   `row.current_generation == attach_gen`.
3. Each flush transaction wraps the `INSERT INTO pages …` in a
   `BEGIN; UPDATE pageserver.timeline_state SET disk_consistent_lsn = $new
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
| 1 | ✅ done | `pageserver.pages` + `pageserver.timeline_state` schema, `PageStore` Rust module (`yb-pageserver/src/page_store.rs`) with full CRUD + generation helpers and 15 integration tests against a real YB. |
| 2a | ✅ done | `ImageRule` 10-delta enforcement logic (`yb-pageserver/src/image_rule.rs`) with 12 unit tests. Pure logic, no walredo or DB dep. |
| 2b | ✅ done | `PageWriter` (`yb-pageserver/src/page_writer.rs`): buffered write path that applies `ImageRule`, calls a `Materializer` trait on `ForceImage`, blends buffered + persisted rows on read (`read_chain`), and flushes on a size/age trigger (defaults 100 MiB / 5 s). 12 integration tests including a long-workload property test that scans every LSN to confirm the LIMIT-10 invariant holds. |
| 3a | ✅ done | `Materializer` trait refactored: now takes `(key, target_lsn, prior_chain, current)` so production wrappers don't need a back-reference to `PageWriter`. `WalRedoMaterializer` (`yb-pageserver/src/page_materializer.rs`) is the production impl — converts chain rows back into `(base_img, records)` and calls `walredo::PostgresRedoManager::request_redo`. 5 tests cover the pure decode helper. Walredo-driven end-to-end test deferred to Phase 3b wiring (needs a real PG subprocess). |
| 3a+ | ✅ done | Two more primitives that Phase 3b will reach for: `PageWriter::spawn_flusher(tick)` returns a `FlushTask` (JoinHandle + CancellationToken) for the per-timeline background flusher, doing a final `flush()` on cancel; `extract_triples(SerializedValueBatch)` decodes a walingest batch into the `(Key, Lsn, Value)` triples `PageWriter::put` consumes. Both with tests. |
| 3b | ✅ done | Tenant/Timeline plumbing: `TenantSharedResources` carries `Option<Arc<PageStore>>`, `Tenant` stashes it, `TimelineResources` passes through, `Timeline` builds `PageWriter` + spawns `FlushTask` when both `page_store` and `walredo_mgr` are present. `TimelineWriter::put_batch` runs `extract_triples` and forwards to `page_writer.put` before the layer-file write (so a YB failure aborts the whole batch and walingest retries). `Timeline::shutdown` drains the FlushTask + does a final `page_writer.flush()`. **Reads still go through the layer stack** — Phase 3c is the read-flip. |
| 3c | ✅ done | `Timeline::get_vectored_impl` branches on `self.page_writer`: when yb-pageserver mode is active, reads go through `get_vectored_via_page_writer` — per-key parallel `PageWriter::read_chain` → `chain_to_value_reconstruct_state` (image-anchor → img, deltas → records) → `Timeline::reconstruct_value` (walredo). The LayerMap walk is bypassed entirely. Upstream pageserver behaviour preserved when `page_writer` is None. Layer-file path is now dead code from the read perspective; still populated by 3b dual-write, both removed by Phase 4. |
| 3c+ | ✅ done | `yb_page_writer` config block in `ConfigToml` (3 knobs: flush_interval, flush_size_bytes, flush_poll_interval). |
| 3d | ✅ done | **Schema reshape + design pivot.** `pageserver.pages` PK becomes `((tenant…forknum) HASH, blknum, is_image, lsn DESC)`: typed columns + per-block image clustering. Reads become an image-anchored CTE (fast path: latest image ≤ target + deltas above; slow path: walk to `will_init` delta). Writes drop all walredo at write time — pure passthrough. `image_rule.rs` + `page_materializer.rs` deleted. The "10-delta invariant" stops being a write-time contract; chain bounding moves to a background image materialiser (Phase 3e). End-to-end verified with `CREATE TABLE / INSERT 1000 / UPDATE×3 / SELECT` against compute; long chains (400+ rows on hot blocks) read correctly via the slow path. |
| 3e | ✅ done | **Background image materialiser** (at-write trigger, not periodic scan). `PageWriter::put` counts consecutive non-anchor deltas per key under the same RwLock as the buffer; on crossing `image_chain_limit` (default 10) `try_send`s `MaterializeJob { key, target_lsn }` to a bounded mpsc. Worker (`ImageMaterializer::spawn`, one per timeline) reads via `PageWriter::read_chain_durable` (cache + PageStore, EXCLUDES pre-flush — see write-path §7 for the correctness reason), walredos, `PageStore::put_pages` the image, then `PageWriter::install_materialized_image` trims the cache for that key up to the image LSN and inserts the new image as a durable entry. Five trigger-logic unit tests cover threshold dispatch, image/will_init reset, per-key independence, and limit=0 disable. |
| 3f | ✅ done | **`UnifiedPages` — one in-RAM structure for the rel-page buffer + post-flush cache.** Replaces the prior `Vec<PageRow>` buffer that was drained on flush. Each entry has a `durable` bit; flush is a cursor flip (`mark_first_n_flushed`) — no copy, no move. Durable entries linger in a FIFO until `total_bytes` exceeds `read_cache_bytes` (default 1 GiB), at which point they evict FIFO. Pre-flush entries are pinned regardless. Two lookup APIs: `lookup_chain` (durable + pre-flush, used by compute reads) and `lookup_chain_durable` (cache + YB only, used by the materialiser to dodge the crash + replay double-apply hazard). 10 unit tests covering insert/lookup/mark/evict/trim/snapshot. |
| 4a | ✅ done | **Stop dual-writing rel pages to layer files.** `TimelineWriter::put_batch` now filters relation-page entries out of the batch handed to `layer.put_batch`; only non-rel keys (SLRU, metadata, dir listings, aux, repl origins, rel-size sentinels) still flow into the layer stack. Batches with no non-rel entries skip the layer call entirely. **Compaction loop disabled** — `start_background_loops` no longer spawns `compaction_loop`; image-layer creation has nothing to do against `pageserver.pages` anyway. |
| 4b | ✅ done | **SLRU pages → `pageserver.slru_pages`.** Same image-anchored design as `pages` (hash partition on `(tenant, timeline, kind, segno)`, cluster on `(blknum, is_image, lsn DESC)`); same LATERAL LIMIT 1 reads; same future bg materialiser. `kind` matches `Key::field2`: 0=Clog, 1=MultiXactMembers, 2=MultiXactOffsets. |
| 4c | ✅ done | **Full-overwrite blobs → `pageserver.meta`.** Rel-size sentinels, dir listings (TenantDir/DbDir/RelDir/SlruDir), aux files, repl origins, two-phase, SLRU dir + segment-size sentinels — everything that isn't a rel-page or SLRU block. Single row per `(tenant, timeline, key, lsn)`; reads = LIMIT 1 at lsn DESC; no walredo. `TimelineWriter::put_batch` now sends the entire batch to PageWriter — the layer-file write is skipped unconditionally. End-state: a fresh tenant with INSERT 500 + UPDATE×2 + multixact + CREATE INDEX produces pages=2889, slru_pages=3, meta=968, blobs=2 (just initdb.tar.zst + tenant-manifest.json — bootstrap artifacts handled outside put_batch). |
| 4d | 🟡 runtime done; code deletion in progress | **Runtime disable** of every layer-stack-touching path is done as a side-effect of 4a/4b/4c: compaction loop never spawns, `layer.put_batch` is never called, `InMemoryLayer` is never created, layer reads are never dispatched, disk-usage eviction returns `None` (disabled by config), secondary downloader/uploader sit idle (no secondary tenants), heatmaps are empty, GC is a no-op. **Code deletion** is incremental, compile-driven, one module per commit. Done so far: `disk_usage_eviction_task.rs` (−1515 LOC), per-timeline `eviction_task.rs` (−579), `heatmap_layers_downloader.rs` (−264), `timeline/analysis.rs` (−123), `timeline/compaction.rs` (−5154 including cascades), `disk_btree_test_data.rs` (−2061), `tenant/debug.rs` (−369), `filter_iterator + merge_iterator` (−915), layer/tests + failpoints (−1297), inline test blocks across `layer_map / batch_split_writer / image_layer / delta_layer / blob_io / disk_btree / vectored_blob_io / inmemory_layer / vectored_dio_read / ephemeral_file / upload_queue / remote_timeline_client / mgr / deletion_queue` and `IndexPart / TenantManifest` parser tests (~7.5K LOC of TenantHarness-driven test scaffolding for layer-file machinery), `statvfs.rs` and assorted dead helpers (−586). **Cumulative ~24.5K LOC removed.** Added this session: `timeline/init.rs` and `Timeline::load_layer_map` (−407 — the legacy directory-scan that yb-mode never hits, since pages live in `pageserver.pages`); `Timeline::{finish_compact_batch,rewrite_layers,upload_new_image_layers}` + matching RTC `schedule_layer_file_deletion / schedule_compaction_update / schedule_barrier` and the `LayerManager` helpers that fed them (−220 — the L0→L1 compaction wiring); the `/scan_disposable_keys` test endpoint plus the whole `Layer::load_keys` → `ImageLayerInner::filter` → `ImageLayerWriter::put_image_raw` chain and `VectoredBlob::raw_with_header` / `header_start` (−253 — used only by the shard-split image copy, no-op in yb-mode); `ResidentLayer::get_as_{delta,image}` + `Delta/ImageLayer` size accessors (−54); five `Timeline::get_*` compaction-tuning getters + the `DurationRecorder` timing helper + `LayerName::kind` + the unused `access_stats_behavior` setter (−90); `CompactRequest` (deserialised input for a removed admin route), `ImageLayerCreationMode::Force` (test-only variant never constructed), `BatchWriterResult::Discarded` + the `finish_with_discard_fn` split (−85); `disk_usage_based_eviction::METRICS` and the `compact_time_histo + load_layer_map_histo` fields on `TimelineMetrics` (−87, six counters with no incrementing call sites left); `DeltaLayerIterator` and `ImageLayerIterator` end-to-end (−154 — their `iter_with_options` constructors went dead with the compaction iterator path). Also done: `tenant/secondary/` (downloader / heatmap_uploader / scheduler / secondary.rs root) + `TenantSlot::Secondary` + `LocationMode::Secondary` + `TenantStartupMode::Secondary` and ~70 match-arm unwinds across mgr.rs / routes.rs / tenant.rs / metrics.rs / lib.rs / yb-pageserver.rs; secondary HTTP handlers + route bindings; `TaskKind::Secondary{Uploads,Downloads}`; `SECONDARY_*` metric statics (−3.3K). `LocationConfigMode::Secondary` stays in libs/pageserver_api for storcon-upcall wire compat but `LocationConf::try_from` rejects it. Plus the residual heatmap chain: `Timeline::generate_heatmap` / `generate_unarchival_heatmap` / `should_keep_previous_heatmap` / the `previous_heatmap` field / the `PreviousHeatmap` enum / `TenantShard::read_on_disk_heatmap` / the unarchival-heatmap seeding in `timeline_init_and_sync` / `HeatMap{Tenant,Timeline,Layer}` types / `tenant/heatmap.rs` module / `tenant_heatmap_path` + `remote_heatmap_path` + `TENANT_HEATMAP_BASENAME` + `Scope::SecondaryTenant`/`SecondaryTimeline` + `Scope::new_secondary_*` + `RequestContext::with_scope_secondary_*` + `load_previous_heatmap` / `generate_unarchival_heatmap` / `heatmap_upload_concurrency` / `secondary_download_concurrency` config fields (−700). Plus dead stragglers: `LastEviction` enum + `happened_after` + `Layer::last_evicted_at` + `Layer::latest_activity` + `LayerAccessStats::latest_activity` + `LayerManager::visible_layers` + `LayerManagerLockHolder::{GenerateHeatmap, Eviction}` + `StorageTimeOperation::{Compact, LoadLayerMap}` + `is_ephemeral_file` + `is_temp_download_file` + `TenantManager::{get_conf, get_attached_active_tenant_shards}` + `VirtualFile::read_to_string` (−170). **Remaining (each its own session)**: `storage_layer/` production code + `layer_map.rs` + `disk_btree.rs` (~12K — the `Timeline.layers` field is the gate, ~70 internal methods read it), `timeline/offload.rs` (~160 LOC but 321 mentions across the codebase — `OffloadedTimeline`, `TimelineOrOffloaded`, `MaybeOffloaded`, manifest-driven state), most of `remote_timeline_client/`, dead compaction tests in `tenant.rs` that exercise no-op stubs (~3K LOC of `test_simple_bottom_most_compaction_*`). Each unwinds carefully. |
| 4e | ✅ done | **Write-path hardening + perf tuning.** Chunked INSERTs in `put_pages` / `put_slru_pages` / `put_meta` to stay under tokio-postgres's 65535-parameter / "value too large to transmit" caps (500 / 500 / 2000 rows per chunk). `PageWriter::flush()` no longer `mem::take`s the buffer — clones a snapshot under the lock, drops the lock, INSERTs, then `drain(0..N)` under the lock on success. Fixes a race where a concurrent walreceiver `read_chain` on a rel-size sentinel saw `MissingKey` while the buffer was "in transit" to YB, corrupting walingest. Parallel chunk dispatch via `buffer_unordered(4/2/3)`. `yb_enable_upsert_mode = true` set per-connection so chunk retries can't PK-conflict. `state: Mutex` → `RwLock` so concurrent reads parallelise. Default `flush_size_bytes` lowered from 100 MiB → 25 MiB to bound `wait_lsn` tail latency after bulk inserts. End-to-end perf workload (200k rows, 50 MB shared_buffers): UPDATE 19k rows 20.4s → 2.6s; CREATE TABLE AS update_ids 9.0s → 0.55s; zero "slow wait_lsn" warnings. |
| 4f | ⏳ pending | **Per-key LRU eviction in `UnifiedPages`.** Today eviction is per-row FIFO; mid-chain gaps are possible when stale FIFO refs let a delta be popped while its image-anchor remains. That forces the `evicted_below` watermark + completeness check. With the materialiser keeping every key's chain ≤ `image_chain_limit`+1, the natural eviction unit is the whole key — drop `KeyState` entirely, no holes possible. Replaces both FIFOs with a single key-keyed LRU; deletes `evicted_below` and stale-ref machinery; pins keys with unflushed rows. |
| 4g | ✅ done | **Cache-only read fast path + materialiser batched INSERT + per-key completeness.** `UnifiedPages` gains `KeyState { chain, evicted_below }`; reads call `lookup_chain_if_complete` first (anchor present + `anchor_lsn > evicted_below` → serve from RAM, skip YB). `PageWriter::scan_range` dispatches the single-key range (the GetPage hot path) to `read_chain` so it gets the same fast path. The materialiser worker drains up to 64 jobs per iteration, walredoes each, dumps all resulting images in one parameterised batched INSERT, then installs each into cache. Slow `scan_range` log lines on a 20k-UPDATE workload drop **8397 → 213** (40×). Microbench best-of-3 vs pre-3e/3f baseline: count_heap 5.2s → 1.1s (4.9×), seqscan 5.5s → 1.1s (5.0×), pk_point_lookup_10k 117s → 21s (5.5×), idx_range_scan 243ms → 26ms (9.3×). |
| 4h | ✅ done | **Dedicated YSQL connection for internal traffic vs compute reads.** Two YsqlClients per pageserver: `yb-pageserver:compute_reads` serves GetPage reads (scan_range, read_chain, scan_meta_range, read_slru_chain) — idle when the cache absorbs them. `yb-pageserver:internal` serves walreceiver `PageWriter::flush` writes, materialiser `read_chain_durable` cache miss, and materialiser image INSERT. Visible in `pg_stat_activity` as two distinct `application_name`s. Compute getPage stays isolated from a 25 MiB flush or a multi-job materialiser INSERT and vice versa. No measurable perf delta on the single-client microbench (cache already absorbed contention) — the structural isolation is the point. |
| 4i | ✅ done | **Hybrid materialiser threshold — count OR combined-delta-size.** `MaterializerHandle` carries `count_limit` AND `payload_bytes_limit`; per-key `DeltaTally { count, payload_bytes }` accumulates on each non-anchor `put` and resets on `is_image`/`will_init`. Whichever threshold lands first dispatches. Either zero disables that trigger; both zero disables the materialiser. Default raised: `image_chain_limit` 10 → 20; new `image_chain_payload_bytes_limit` 4096 (4 KiB). Catches small-delta accumulation (count) and large-delta bursts (bytes — a single near-full-page rewrite already represents 8 KB of walredo work to apply on every subsequent read). Tunable per-deployment via `[yb_page_writer]` in `pageserver.toml`. Existing 25 unit tests pass. Microbench tuning across the (count × bytes) matrix deferred until we have a workload that exercises both regimes. |
| 4j | ⏳ pending | **Microbench tuning matrix for 4i thresholds.** Pick a workload that exercises both axes — small-delta accumulation (lots of `ClearVisibilityMapFlags`/visibility-map updates) and large-delta bursts (heap rewrites, near-full pages). Sweep `image_chain_limit` ∈ {10, 20, 40} × `image_chain_payload_bytes_limit` ∈ {2 KiB, 4 KiB, 8 KiB, 0 (disabled)}. Track: materialiser firings per workload, read-side walredo cost on chains, INSERT throughput in `pageserver.pages`. Today's defaults (20 / 4 KiB) are intuition-based. Likely outcome: tune separately per workload-class via tenant config rather than a single global default. |
| 4k | ⏳ pending | **`neon_local start` wedge investigation.** Observed during Phase 5 testing: after several drop-database / recreate cycles, `neon_local start` hangs ~25 min without spawning `storage_controller`, even though all sibling services (broker, pageserver, safekeeper, endpoint_storage) come up. The `--start-timeout 10s` default triggers cleanup before storage_controller's migrations finish on a fresh DB — bumping to `--start-timeout 120s` worked around it. Root cause unknown: storage_controller migrations against a freshly-dropped YB database take ≥60 s; neon_local may be waiting on pageserver health which can't pass until storage_controller comes up (circular). Fix shapes: (a) bump default `--start-timeout` to 120s in neon_local, (b) drop the cross-service health-check coupling so storage_controller can start in parallel with pageserver, (c) pre-create the storage_controller schema during `neon_local init` so first-start avoids the migration burst. |
| 4j | ⏳ pending | **Per-pageserver `s3_upload_limit_bytes` config.** Already shipped (commit `bdb1d133d`); promote `S3_UPLOAD_LIMIT` const → `ConfigToml` field, default preserved at 4_500_000_000. Operators on stores without the 5 GB single-PUT cap can lift the per-file ceiling. Tracked here for completeness. |
| 5 | ✅ done | **Pageserver restart survives off `pageserver.timeline_state`** — runtime hookup of the per-timeline state row that replaces the index_part.json blob. Four sub-phases. **5a**: `PageWriter` keeps an in-RAM `dc_lsn_watermark`; flush success path issues a column-scoped `UPDATE timeline_state SET disk_consistent_lsn = $1 WHERE pk` on the internal conn when the flush's max LSN exceeds the watermark. In-memory monotonicity guard, no server-side conditional. **5b**: `UninitializedTimeline::finish_creation` calls `Timeline::write_timeline_row_to_yb` (full-row upsert via `internal_page_store` + `seed_dc_lsn_watermark`) BEFORE taking the timelines mutex and before `maybe_spawn_flush_loop`. The row is guaranteed to exist by the time the flusher starts hitting it, so Phase 5a's UPDATE always lands. **5c**: when `attach`'s `index_part.json` fetch returns `NotFound`, the `compute_page_store` path queries `get_timeline(tenant, timeline)` and synthesises an `IndexPart::empty(metadata)` with `TimelineMetadata` built from the row's columns. `last_record_lsn`/`initdb_lsn` aren't persisted (re-derived on walreceiver resume / stub-set to `disk_consistent_lsn`). **5d**: `timeline_init_and_sync`'s "no ancestor + no layer files" sanity check skips in yb-mode (that *is* the normal post-restart state); `ensure_schema` swallows YB's "type X already exists" so a stale orphan `pg_type` entry after `DROP DATABASE WITH (FORCE)` doesn't break startup. End-to-end smoke: cluster up → create tenant → INSERT 5k rows → flush → `dc_lsn` 22283440→23216088 in `pageserver.timeline_state` → stop+start pageserver → `Phase 5c: synthesising IndexPart` fires → attach completes (activated_timelines=1) → reads return correct values. Schema simplified: `last_record_lsn` column removed from `pageserver.timeline_state` (re-derived on resume); table renamed `pageserver.timelines` → `pageserver.timeline_state` to avoid YB type-name collision with `storage_controller.timelines`. |
| 6 | ⏳ pending | **Startup perf.** Cold-start time-to-first-getPage is dominated by sequential setup the layer-stack design baked in but yb-mode no longer needs. Items to investigate: (a) drop the `list_remote_timelines` blob-prefix scan entirely in yb-mode and source the timeline list from `pageserver.timeline_state` (one indexed query vs N blob LIST + GET round-trips); (b) parallelise per-timeline attach (`load_remote_timeline` is already async but is called serially in `sorted_timelines` loop); (c) skip the local-disk dentry-purge in yb-mode (no layer files to reconcile); (d) shorten the walreceiver catch-up from `disk_consistent_lsn` — currently a tenant restart re-applies the entire post-flush window even though most of it is cache-resident on the previous pageserver; could be near-instant after Phase 4f's per-key LRU lands. Target: sub-second time-to-first-getPage on a clean restart for a tenant with one timeline + a few hundred MB of recent writes. |
| 7 | ⏳ deferred | GC: cron that prunes old `pages` rows. User said "figure out separately later". |

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
  and `pageserver.timeline_state`.
