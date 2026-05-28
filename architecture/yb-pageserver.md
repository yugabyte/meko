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
| L0 → L1 compaction, image-layer creation | The **10-delta image rule** at write time (this file) |
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

One row per (Key, LSN). The PK partitions by the page identity and orders
versions DESC inside each hash tablet, so the read query is a single
range scan.

```sql
CREATE TABLE pageserver.pages (
    tenant_id    TEXT    NOT NULL,
    timeline_id  TEXT    NOT NULL,
    key          BYTEA   NOT NULL,    -- 18-byte Key (pageserver_api::key::Key)
    lsn          BIGINT  NOT NULL,    -- u64 stored as i64 (real LSNs are << 2^63)
    is_image     BOOLEAN NOT NULL,
    will_init    BOOLEAN NOT NULL,
    payload      BYTEA   NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id, key) HASH, lsn DESC)
);
```

`is_image` is whether the payload is an 8 KiB Postgres page (vs. a
serialised `NeonWalRecord`). `will_init` mirrors `Value::will_init()` —
true for every image and for `WalRecord` entries that act as a chain
anchor. The read path stops walking back at the first `will_init=true`.

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

## The 10-delta image rule

Reads are bounded by `LIMIT IMAGE_CHAIN_LIMIT` (default 10) so the query
plan is always "hash to one tablet, read at most 10 rows, stop". For
this to be correct the write path must guarantee that **every contiguous
chain of `IMAGE_CHAIN_LIMIT` rows per key contains an image** (i.e., a
row with `will_init=true`).

Implemented in `yb-pageserver/src/image_rule.rs`:

```rust
pub enum WriteAction {
    Passthrough,   // write the entry verbatim
    ForceImage,    // walredo the chain, write the result as an image
}
```

Per-key counter `deltas_since_image: HashMap<Key, usize>`:

- First time we see a key in this session:
  - `will_init` entry (`Value::Image` or `WalRecord` with
    `will_init=true`): `Passthrough`, counter := 0.
  - Plain delta: `ForceImage`. We can't tell whether PageStore already
    has a chain anchor at a recent enough LSN, so be safe.
- Subsequent entries for the same key:
  - `will_init` entry: `Passthrough`, counter := 0.
  - Plain delta with `counter + 1 < threshold`: `Passthrough`,
    counter += 1.
  - Plain delta with `counter + 1 >= threshold`: `ForceImage`,
    counter := 0.

When the rule says `ForceImage`, the caller walks the prior chain
(buffered rows + a `read_chain(key, lsn_just_below_this_one,
LIMIT 10)` against PageStore), calls walredo, and writes the result as
`is_image=true, will_init=true` at the same LSN as the delta that
triggered the materialisation. The original delta is *not* also written
— the image replaces it.

### Why "first delta forces an image"

The counter is in-memory only. After a pageserver restart we don't know
how many deltas live in YB beyond the last image; in the worst case
nine. If the next entry is a delta, applying the "counter + 1 ≥
threshold" rule with counter starting at 0 would push the chain to 11
rows, breaking the `LIMIT 10` invariant. Forcing an image on the first
delta after a restart keeps the invariant intact at the cost of one
extra walredo per key per restart.

### Why threshold = 10

`SELECT … ORDER BY lsn DESC LIMIT N` plans as a single one-direction
range scan inside one hash tablet. Smaller N → fewer rows per read but
more walredo at write time. Larger N → less write-side work but bigger
read scans. 10 is a starting point — the constant is configurable in
the `ImageRule::new(threshold)` constructor.

## Read path (planned)

`Timeline::get_vectored_impl` is rewritten to query `PageStore` instead
of walking `LayerMap`:

1. For the requested keys at `target_lsn`, batch a vectored read:
   `read_chains(tenant, timeline, keys, target_lsn, IMAGE_CHAIN_LIMIT)`.
2. For each key, the result is a Vec of up to 10 `PageRow`s in DESC LSN
   order. The chain always contains at least one `will_init=true` row
   (by construction of the 10-delta rule).
3. Find the most recent `will_init=true` row — that's the base.
   Rows newer than it are deltas that walredo will apply on top.
4. Drive walredo as today (`Timeline::reconstruct_value`).

Reads above `last_record_lsn` block on `wait_lsn`. Reads below
`applied_gc_cutoff_lsn` return an error.

## Write path (planned)

Replaces `InMemoryLayer` + `flush_loop` + L0/L1 compaction.

1. `walingest` produces `(Key, LSN, Value)` triples via
   `pgdatadir_mapping::DatadirModification.put`. Unchanged.
2. `DatadirModification.commit` calls `TimelineWriter::put_batch`. The
   new implementation:
   - For each entry, call `ImageRule::decide(key, value)`.
   - `Passthrough`: append a `PageRow` with the value's bytes + flags.
   - `ForceImage`: walk the chain (buffer + PageStore), call walredo,
     append a synthetic image row at this LSN.
3. Buffered rows live in an in-RAM `Vec<PageRow>` (per timeline) until
   a flush threshold:
   - **5 seconds** since the last flush, OR
   - **100 MB** of accumulated payload.
   Both configurable per pageserver.
4. Flush issues one `INSERT INTO pageserver.pages VALUES (…),(…),…`
   per batch, gen-checked via a conditional UPDATE on
   `pageserver.timelines` in the same transaction (see "Generation
   fencing"). On success, `disk_consistent_lsn` advances on the
   timeline row.
5. Reads between flushes can see in-buffer rows: the read path checks
   the buffer first, then queries PageStore.

Default knobs (configurable via `pageserver.toml`):

| Knob | Default | Meaning |
|---|---:|---|
| `yb_flush_interval` | 5 s | Time-based flush trigger |
| `yb_flush_size` | 100 MB | Size-based flush trigger |
| `yb_image_chain_limit` | 10 | Threshold for the 10-delta rule |

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

- **Compaction** — none. The 10-delta rule does at write time what L0→L1
  + image-layer creation do in upstream. The `pageserver_compaction`
  crate is still a build-time dep so the fork stays buildable; it's
  unused at runtime.
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
| 3a | ✅ done | `Materializer` trait refactored: now takes `(key, target_lsn, prior_chain, current)` so production wrappers don't need a back-reference to `PageWriter`. `WalRedoMaterializer` (`yb-pageserver/src/page_materializer.rs`) is the production impl — converts chain rows back into `(base_img, records)` and calls `walredo::PostgresRedoManager::request_redo`. 5 tests cover the pure decode helper. Walredo-driven end-to-end test deferred to Phase 3 wiring (needs a real PG subprocess). |
| 3b | ⏳ pending | Wire `PageWriter` (+ `WalRedoMaterializer`) into `Timeline`: initialise from pageserver.toml's YB config, hook `TimelineWriter::put_batch` to dual-write alongside `InMemoryLayer`, rewrite `Timeline::get_vectored_reconstruct_data*` to use `PageWriter::read_chain`. |
| 4 | ⏳ pending | Delete the layer stack: `storage_layer/`, `layer_map/`, `disk_btree`, `compaction.rs`, `inmemory_layer`, `secondary*`, `heatmap*`, `eviction_task*`, `init.rs`, most of `remote_timeline_client/`. ~25 K LOC removal. |
| 5 | ⏳ pending | Replace `metadata` / `index_part.json` / `tenant-manifest.json` with reads/writes against `pageserver.timelines`. Rewrite attach. |
| 6 | ⏳ deferred | GC: cron that prunes old `pages` rows. User said "figure out separately later". |

## Code anchors

- `yb-pageserver/src/page_store.rs` — `PageStore` row store.
- `yb-pageserver/src/image_rule.rs` — 10-delta rule (pure logic).
- `yb-pageserver/src/page_writer.rs` — `PageWriter` buffered write path;
  `Materializer` trait + `FlushPolicy`; `read_chain` blends buffer +
  PageStore for the eventual read path.
- `yb-pageserver/src/page_materializer.rs` — `WalRedoMaterializer`
  production impl that converts chain rows into walredo inputs and calls
  `PostgresRedoManager::request_redo`.
- `control_plane/src/storage_controller.rs::ensure_yb_storage_schema`
  — DDL bootstrap for `pageserver` schema.
- `architecture/yugabyte.md` — full DDL reference for `pageserver.pages`
  and `pageserver.timelines`.
