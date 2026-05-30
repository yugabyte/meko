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
| `image_chain_limit` | 10 | Deltas-since-anchor before a key is queued for materialisation (0 disables) |
| `materialize_queue_capacity` | 1024 | Bounded mpsc depth; `try_send` drops on overflow |
| `read_cache_bytes` | 1 GiB | Soft cap on `UnifiedPages.total_bytes`. Durable entries are evicted FIFO when crossed; pre-flush entries are pinned regardless |

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
| 3e | ✅ done | **Background image materialiser** (at-write trigger, not periodic scan). `PageWriter::put` counts consecutive non-anchor deltas per key under the same RwLock as the buffer; on crossing `image_chain_limit` (default 10) `try_send`s `MaterializeJob { key, target_lsn }` to a bounded mpsc. Worker (`ImageMaterializer::spawn`, one per timeline) reads via `PageWriter::read_chain_durable` (cache + PageStore, EXCLUDES pre-flush — see write-path §7 for the correctness reason), walredos, `PageStore::put_pages` the image, then `PageWriter::install_materialized_image` trims the cache for that key up to the image LSN and inserts the new image as a durable entry. Five trigger-logic unit tests cover threshold dispatch, image/will_init reset, per-key independence, and limit=0 disable. |
| 3f | ✅ done | **`UnifiedPages` — one in-RAM structure for the rel-page buffer + post-flush cache.** Replaces the prior `Vec<PageRow>` buffer that was drained on flush. Each entry has a `durable` bit; flush is a cursor flip (`mark_first_n_flushed`) — no copy, no move. Durable entries linger in a FIFO until `total_bytes` exceeds `read_cache_bytes` (default 1 GiB), at which point they evict FIFO. Pre-flush entries are pinned regardless. Two lookup APIs: `lookup_chain` (durable + pre-flush, used by compute reads) and `lookup_chain_durable` (cache + YB only, used by the materialiser to dodge the crash + replay double-apply hazard). 10 unit tests covering insert/lookup/mark/evict/trim/snapshot. |
| 4a | ✅ done | **Stop dual-writing rel pages to layer files.** `TimelineWriter::put_batch` now filters relation-page entries out of the batch handed to `layer.put_batch`; only non-rel keys (SLRU, metadata, dir listings, aux, repl origins, rel-size sentinels) still flow into the layer stack. Batches with no non-rel entries skip the layer call entirely. **Compaction loop disabled** — `start_background_loops` no longer spawns `compaction_loop`; image-layer creation has nothing to do against `pageserver.pages` anyway. |
| 4b | ✅ done | **SLRU pages → `pageserver.slru_pages`.** Same image-anchored design as `pages` (hash partition on `(tenant, timeline, kind, segno)`, cluster on `(blknum, is_image, lsn DESC)`); same LATERAL LIMIT 1 reads; same future bg materialiser. `kind` matches `Key::field2`: 0=Clog, 1=MultiXactMembers, 2=MultiXactOffsets. |
| 4c | ✅ done | **Full-overwrite blobs → `pageserver.meta`.** Rel-size sentinels, dir listings (TenantDir/DbDir/RelDir/SlruDir), aux files, repl origins, two-phase, SLRU dir + segment-size sentinels — everything that isn't a rel-page or SLRU block. Single row per `(tenant, timeline, key, lsn)`; reads = LIMIT 1 at lsn DESC; no walredo. `TimelineWriter::put_batch` now sends the entire batch to PageWriter — the layer-file write is skipped unconditionally. End-state: a fresh tenant with INSERT 500 + UPDATE×2 + multixact + CREATE INDEX produces pages=2889, slru_pages=3, meta=968, blobs=2 (just initdb.tar.zst + tenant-manifest.json — bootstrap artifacts handled outside put_batch). |
| 4d | 🟡 runtime done; code deletion in progress | **Runtime disable** of every layer-stack-touching path is done as a side-effect of 4a/4b/4c: compaction loop never spawns, `layer.put_batch` is never called, `InMemoryLayer` is never created, layer reads are never dispatched, disk-usage eviction returns `None` (disabled by config), secondary downloader/uploader sit idle (no secondary tenants), heatmaps are empty, GC is a no-op. **Code deletion** is incremental, compile-driven, one module per commit. Done so far: `disk_usage_eviction_task.rs` (−1515 LOC), per-timeline `eviction_task.rs` (−579), `heatmap_layers_downloader.rs` (−264), `timeline/analysis.rs` (−123), `timeline/compaction.rs` (−5154 including cascades). Cumulative ~7.6K LOC removed. **Remaining (each its own session)**: `secondary/` (~3K, ~70 callers in mgr.rs around `TenantSlot::Secondary`), `storage_layer/` + `layer_map.rs` + `disk_btree.rs` (~15K, the `Timeline.layers` field is the gate — ~70 internal methods read it), `timeline/offload.rs` (~160 LOC but 145 mentions in tenant.rs), `init.rs` (load_layer_map), most of `remote_timeline_client/`. Each unwinds carefully. |
| 4e | ✅ done | **Write-path hardening + perf tuning.** Chunked INSERTs in `put_pages` / `put_slru_pages` / `put_meta` to stay under tokio-postgres's 65535-parameter / "value too large to transmit" caps (500 / 500 / 2000 rows per chunk). `PageWriter::flush()` no longer `mem::take`s the buffer — clones a snapshot under the lock, drops the lock, INSERTs, then `drain(0..N)` under the lock on success. Fixes a race where a concurrent walreceiver `read_chain` on a rel-size sentinel saw `MissingKey` while the buffer was "in transit" to YB, corrupting walingest. Parallel chunk dispatch via `buffer_unordered(4/2/3)`. `yb_enable_upsert_mode = true` set per-connection so chunk retries can't PK-conflict. `state: Mutex` → `RwLock` so concurrent reads parallelise. Default `flush_size_bytes` lowered from 100 MiB → 25 MiB to bound `wait_lsn` tail latency after bulk inserts. End-to-end perf workload (200k rows, 50 MB shared_buffers): UPDATE 19k rows 20.4s → 2.6s; CREATE TABLE AS update_ids 9.0s → 0.55s; zero "slow wait_lsn" warnings. |
| 4f | ⏳ pending | **Per-key LRU eviction in `UnifiedPages`.** Today eviction is per-row FIFO; mid-chain gaps are possible when stale FIFO refs let a delta be popped while its image-anchor remains. That forces the `evicted_below` watermark + completeness check. With the materialiser keeping every key's chain ≤ `image_chain_limit`+1, the natural eviction unit is the whole key — drop `KeyState` entirely, no holes possible. Replaces both FIFOs with a single key-keyed LRU; deletes `evicted_below` and stale-ref machinery; pins keys with unflushed rows. |
| 4g | ✅ done | **Cache-only read fast path + materialiser batched INSERT + per-key completeness.** `UnifiedPages` gains `KeyState { chain, evicted_below }`; reads call `lookup_chain_if_complete` first (anchor present + `anchor_lsn > evicted_below` → serve from RAM, skip YB). `PageWriter::scan_range` dispatches the single-key range (the GetPage hot path) to `read_chain` so it gets the same fast path. The materialiser worker drains up to 64 jobs per iteration, walredoes each, dumps all resulting images in one parameterised batched INSERT, then installs each into cache. Slow `scan_range` log lines on a 20k-UPDATE workload drop **8397 → 213** (40×). Microbench best-of-3 vs pre-3e/3f baseline: count_heap 5.2s → 1.1s (4.9×), seqscan 5.5s → 1.1s (5.0×), pk_point_lookup_10k 117s → 21s (5.5×), idx_range_scan 243ms → 26ms (9.3×). |
| 4h | ✅ done | **Dedicated YSQL connection for internal traffic vs compute reads.** Two YsqlClients per pageserver: `yb-pageserver:compute_reads` serves GetPage reads (scan_range, read_chain, scan_meta_range, read_slru_chain) — idle when the cache absorbs them. `yb-pageserver:internal` serves walreceiver `PageWriter::flush` writes, materialiser `read_chain_durable` cache miss, and materialiser image INSERT. Visible in `pg_stat_activity` as two distinct `application_name`s. Compute getPage stays isolated from a 25 MiB flush or a multi-job materialiser INSERT and vice versa. No measurable perf delta on the single-client microbench (cache already absorbed contention) — the structural isolation is the point. |
| 4i | ⏳ pending | **Revisit materialiser threshold.** Current `image_chain_limit=10` counts deltas only. Small WAL records (1–2 bytes) accumulate slowly and waste materialiser cycles; large deltas (near-full pages) leave reads doing many records' worth of walredo before triggering. Move to a hybrid: trigger when **20 deltas OR 4 KiB combined delta payload** since the last anchor for a key, whichever lands first. Per-key payload counter lives alongside `delta_counts` in `WriterState`, reset on `will_init`/image. Pick numbers by re-running microbench across (10/20/40 delta caps) × (2/4/8 KiB size caps). |
| 4j | ⏳ pending | **Per-pageserver `s3_upload_limit_bytes` config.** Already shipped (commit `bdb1d133d`); promote `S3_UPLOAD_LIMIT` const → `ConfigToml` field, default preserved at 4_500_000_000. Operators on stores without the 5 GB single-PUT cap can lift the per-file ceiling. Tracked here for completeness. |
| 5 | ⏳ pending | Replace `metadata` / `index_part.json` / `tenant-manifest.json` with reads/writes against `pageserver.timelines`. Rewrite attach. The table schema already exists (`tenant_id, timeline_id, last_record_lsn, disk_consistent_lsn, applied_gc_cutoff_lsn, ancestor_timeline_id, ancestor_lsn, current_generation, pg_version`) and `PageStore::{upsert_timeline,get_timeline,bump_generation}` are wired (Phase 1). What's missing: the runtime hookup that replaces the existing `RemoteTimelineClient` calls into the blob store. After 4a/4b/4c, layer files for rel/SLRU/meta keys are never created, so the upload of `index_part.json` (which is generated alongside layer-file uploads) silently stops happening — on restart, `attach` looks for `index_part.json` at the per-timeline blob key and sees nothing, then purges the local directory and fails the basebackup ("Timeline … was not found"). Fixing it means: (a) on every `disk_consistent_lsn` advance / generation bump / ancestor update, upsert the timeline row; (b) on attach, build the in-memory `IndexPart` equivalent from the row, skip the blob fetch; (c) tenant-manifest replacement is simpler — the generation column on the timeline row IS the manifest. |
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
