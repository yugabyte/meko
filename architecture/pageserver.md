# Pageserver

The pageserver is the only component that holds the durable copy of user
table & index data. Computes don't have relfiles; they ask the pageserver
for pages on demand via `getpage` RPC.

In yb-neon, the pageserver's "remote storage" (where it durably stashes
layer files, the index, the initdb tarball, and tenant manifests) is YB
instead of S3/Azure/GCS. The pageserver itself was barely modified —
all the YB stuff is plumbed via the existing `RemoteStorage` trait.

## Inherited from Neon

### Tenant / timeline layout

```
pageserver_<id>/
└── tenants/
    └── <tenant_id>/
        ├── tenant-manifest-<gen>.json   (replication policy, generations)
        └── timelines/
            └── <timeline_id>/
                ├── metadata
                ├── index_part.json-<gen>            (layer list + metadata)
                ├── initdb.tar.zst                   (basebackup for new endpoints)
                └── <key_range>__<lsn_range>-vN-<gen>   ← layer files
```

The on-disk dir is a **cache** of what's in `remote_storage`. The pageserver
serves reads from local layer files when present; on miss it downloads from
remote.

### The `Key` abstraction

`pageserver_api::key::Key` (`libs/pageserver_api/src/key.rs:19`) is the
single 18-byte identifier the pageserver uses for *everything* it stores
— not just relation pages, but also rel-size sentinels, SLRUs (`pg_xact`,
`pg_subtrans`, `pg_multixact`), aux files, replication origins, and
per-DB / per-rel directory listings.

```rust
pub struct Key {
    pub field1: u8,     // namespace / type tag
    pub field2: u32,
    pub field3: u32,
    pub field4: u32,
    pub field5: u8,
    pub field6: u32,
}
```

The first byte is the namespace:

| `field1` prefix | Meaning | Field layout |
|---|---|---|
| `0x00` | **Relation page** (the common case) | `(0, spcnode, dbnode, relnode, forknum, blknum)` |
| `0x00`, `field6 = u32::MAX` | Legacy rel-size sentinel | tracks current block count for the rel |
| `0x01..0x5F` | SLRUs, control file, twophase, etc. | encoded by `SlruKind` + segment + block |
| `0x60` | `RELATION_SIZE_PREFIX` | new-style sparse rel size |
| `0x62` | `AUX_KEY_PREFIX` | aux files (logical replication state, etc.) |
| `0x63` | `REPL_ORIGIN_KEY_PREFIX` | replication origins |
| `0x64` | `DB_DIR_KEY_PREFIX` | per-DB directory listings |
| `0x65` | `REL_DIR_KEY_PREFIX` | per-DB relation directories |
| `0x60..0x7F` | "Metadata keys" — sparse keyspace (16-byte logical encoding wrapped in the 18-byte storage key) | — |

Encoders live alongside `Key` in the same module: `rel_block_to_key`,
`rel_size_to_key`, `slru_block_to_key`, etc.

A store operation is therefore `(Key, LSN) → bytes`: "at version `LSN`,
key `K` is this blob". Reads are time-traveling — every read takes an
LSN, and the pageserver returns the value of `K` as of that LSN by
walking the layer stack.

For hot in-memory work there's also `CompactKey(i128)`, which packs all
six fields into a single 128-bit integer without loss (`field5` is only
1 byte so it fits).

### Layer files

Two flavors:

- **Image layer**: full materialized pages (`(Key, Lsn) → page bytes`) at
  a single LSN. Read-cheap, build-expensive.
- **Delta layer**: WAL-record deltas over an LSN range
  (`(Key, Lsn) → (will_init | WalRecord | Image)`). Append-only output of
  WAL ingestion + L0→L1 compaction.

Filename encodes the layer's coverage:

```
000000…0000-FFFF…FFFF__0000000001540438-00000000015404B1-00000001
└── key_start ────┘ ──┘└── lsn_start ──┘-└── lsn_end ──┘-└─ gen ─┘
```

— so a directory listing alone tells the pageserver which file covers
which `(key_range, lsn_range)` (image layers have `lsn_start == lsn_end`).

### Layer file internal layout

Both formats are 8 KiB-aligned (`PAGE_SZ = postgres_ffi::BLCKSZ = 8192`)
so they share alignment with Postgres. A layer file looks like:

```
+---------------------------+  ← block 0       (PAGE_SZ = 8192 bytes)
|        Summary            |  bincode header, zero-padded to 8 KiB
+---------------------------+  ← block 1
|                           |
|        VALUES             |  concatenated blobs (one per (key, lsn) entry)
|                           |
|         ...               |
+---------------------------+  ← index_start_blk
|                           |
|         INDEX             |  on-disk B-tree
|                           |
+---------------------------+
```

**Summary** (`pageserver/src/tenant/storage_layer/{delta,image}_layer.rs`):

```rust
struct Summary {
    magic: u16,                 // DELTA_FILE_MAGIC (0x5A61) or IMAGE_FILE_MAGIC (0x5A60)
    format_version: u16,        // STORAGE_FORMAT_VERSION = 3
    tenant_id, timeline_id,
    key_range: Range<Key>,      // what keys this file covers
    lsn_range: Range<Lsn>,      // delta only; image has a single `lsn: Lsn`
    index_start_blk: u32,       // where INDEX section starts
    index_root_blk: u32,        // root B-tree page within INDEX
}
```

**VALUES** — concatenated blobs, no padding between them. The B-tree
gives the byte offsets:

- **Image layer**: each blob is a full 8 KiB Postgres page (possibly
  compressed by the codec configured at write time).
- **Delta layer**: each blob is a serialized `Value`:
  - `Value::Image(Bytes)` — a full page snapshot.
  - `Value::WalRecord(NeonWalRecord)` — one WAL record's contribution to
    one page. To reconstruct, replay records in LSN order; a `will_init`
    record resets the chain.

**INDEX** — a disk-resident B-tree implemented in
`pageserver/src/tenant/disk_btree.rs`. Nodes are sized to `PAGE_SZ`.

- **Image layer index key**: `Key` itself — 18 bytes.
- **Delta layer index key**: `DeltaKey = Key ++ LSN` — `KEY_SIZE + 8 = 26`
  bytes. LSN appended big-endian so byte order matches `(key, lsn)` order.
- **Index leaf value**: `BlobRef(u64)` — 8 bytes, packed:

  ```text
  BlobRef = (byte_offset_in_VALUES << 1) | will_init_bit
  ```

  Putting `will_init` in the BlobRef means the reader can decide whether
  to keep walking back through older deltas *without* dereferencing the
  blob — cuts I/O during page reconstruction.

### Lookup procedure

To answer "give me page `K` as of LSN `L`":

1. Read block 0; deserialize `Summary`. Confirm `K ∈ key_range` and `L`
   is consistent with `lsn_range`/`lsn`.
2. Open the B-tree at `(index_start_blk + index_root_blk) * PAGE_SZ`.
3. Walk the B-tree:
   - Delta: largest `DeltaKey ≤ (K, L)`.
   - Image: exact `K`.
4. Decode the `BlobRef` → seek into VALUES, read the blob.
5. Decode the blob:
   - Image: that's the page, done.
   - Delta value is an `Image` or carries `will_init`: done.
   - Otherwise: keep walking *back* through delta records (across more
     layer files if needed) until an image or `will_init` is hit, then
     replay forward, applying each record on top of the previous page
     state.

For batch reads (the common case — reconstructing many pages for a
single compute query), `VectoredReadPlanner` /
`StreamingVectoredReadPlanner` (`pageserver/src/tenant/vectored_blob_io.rs`)
coalesce adjacent blobs in VALUES into single read syscalls.

### Layer file sizes & when the pageserver splits them

Layer files are not infinitely long-lived single objects — the
pageserver runs a multi-stage lifecycle that bounds their size and
periodically rewrites them. The relevant knobs (defaults from
`libs/pageserver_api/src/config.rs::tenant_conf_defaults`):

| Knob | Default | Meaning |
|---|---:|---|
| `checkpoint_distance` | **256 MiB** | An open in-memory layer ("frozen layer") gets sealed and written as an L0 delta file when its accumulated WAL crosses this. **Hard cap on L0 delta size.** |
| `checkpoint_timeout` | 10 min | Even if `checkpoint_distance` isn't hit, the layer is frozen after this much wall time. |
| `compaction_target_size` | **128 MiB** | Output size target when L0 → L1 compaction rewrites layers. **Soft cap on L1 delta and image layer size.** A single output that grows past this triggers a split. |
| `compaction_threshold` | 10 | Number of L0 layers that must accumulate before L0→L1 compaction kicks in. |
| `compaction_period` | 20 s | Background compaction loop period. |
| `image_creation_threshold` | 3 | Per key-range, after this many delta layers have stacked on top of the last image, the pageserver materializes a new image layer to short-circuit reconstruction. |
| `S3_UPLOAD_LIMIT` | **4.5 GB** | Absolute upper bound any layer is allowed to reach. `BatchLayerWriter` force-splits above this. (`pageserver/src/tenant/storage_layer/layer.rs:39`) |
| `gc_horizon` | 64 MiB | Garbage-collection horizon for stale layers below the PITR window. |
| `pitr_interval` | 7 days | How far back time-travel reads stay supported (gates GC). |

### Layer lifecycle: where each split happens

```
                          WAL records arrive
                                  │
                                  ▼
                  ┌──────────────────────────────────┐
                  │  In-memory layer (writable)      │  ← per timeline, grows as WAL ingests
                  └──────────────────────────────────┘
                                  │
                                  │  size ≥ checkpoint_distance (256 MiB)
                                  │  OR   age ≥ checkpoint_timeout (10 min)
                                  ▼
                  ┌──────────────────────────────────┐
                  │  Frozen layer (read-only, mem)   │
                  └──────────────────────────────────┘
                                  │
                                  │  flush_loop writes to disk
                                  ▼
                  ┌──────────────────────────────────┐
                  │  L0 delta file (≤ checkpoint_distance) │  ← ~256 MiB cap
                  └──────────────────────────────────┘
                                  │
                                  │  ≥ compaction_threshold (10) L0 layers accumulate
                                  ▼
                  ┌──────────────────────────────────┐
                  │  L0 → L1 compaction              │  background task, period 20s
                  └──────────────────────────────────┘
                                  │
                                  │  rewrites N L0 deltas into M L1 deltas,
                                  │  splitting at compaction_target_size (128 MiB)
                                  │  per key range
                                  ▼
                  ┌──────────────────────────────────┐
                  │  L1 delta files (~128 MiB each)  │  ← key-range partitioned
                  └──────────────────────────────────┘
                                  │
                                  │  for any key range that's been deltas-only
                                  │  for ≥ image_creation_threshold (3) layers
                                  ▼
                  ┌──────────────────────────────────┐
                  │  Image layer (~128 MiB per shard)│  ← materialized snapshot,
                  └──────────────────────────────────┘    short-circuits delta replay
                                  │
                                  │  layer is < disk_consistent_lsn - pitr_interval
                                  ▼
                                 GC
                                  │
                                  ▼
                              dropped
```

### How splits actually happen mechanically

The writer that turns a stream of (Key, Lsn, Value) entries into one or
more layer files is `BatchLayerWriter`
(`pageserver/src/tenant/storage_layer/batch_split_writer.rs`). It tracks
`estimated_size()` as the input stream advances and rotates to a new
output file when:

1. The current writer's `estimated_size() >= compaction_target_size`
   (the configured 128 MiB target), **and** the next entry would start
   a new key — i.e. we don't split mid-key.
2. **Or** the current writer's `estimated_size() >= S3_UPLOAD_LIMIT`
   (4.5 GB) — a hard split even mid-key, to keep individual upload
   objects under cloud-provider per-object limits.

Image-layer creation has its own size logic in
`pageserver/src/tenant/timeline/compaction.rs:2261` onward: it iterates
keys in order, accumulates `key_values_total_size`, and starts a new
output file whenever `written_size + key_values_total_size >
target_file_size` and we're at a key boundary.

If a single key's accumulated values are themselves bigger than
`target_file_size` (a hot rel with many WAL records), the writer just
emits that one as an oversized layer and logs a warning at
`target_file_size * 2 + 2*PAGE_SZ` — the layer is still functionally
correct, just bigger than the configured target.

### Practical sizes in the running yb-neon cluster

Snapshot from the YB table after the cold-cache stress test
(`benchmarks/yb-store/`):

| Layer kind | Observed sizes |
|---|---|
| Initial `initdb.tar.zst` (per timeline, written once) | ~1.6 MiB |
| Initial image layer (timeline bootstrap, fits the empty DB state) | ~24 MiB |
| `index_part.json-<gen>` | < 1 KiB |
| `tenant-manifest-<gen>.json` | < 200 B |
| Sealed L1 delta on a small bench | 23–294 MiB (cluster of similar sizes around the 128 MiB target, with occasional outliers near the L0 cap of 256 MiB before compaction) |
| Largest single object ever produced by our bench | ~294 MiB (a freshly-flushed L0 that hadn't yet been compacted) |

The 294 MiB case is what motivated `YbStore`'s **100 MiB chunking** —
without it, the single `INSERT` of a 294 MiB BYTEA tripped YB's
`rpc_max_message_size` (256 MiB) and the upload retry-looped. See
[`remote-storage.md`](remote-storage.md) for the chunk-size derivation.

### How layer files map to `YbStore`

From `YbStore`'s perspective, none of the internal Summary/Values/Index
structure matters: a layer file is just an opaque `Bytes` blob keyed by
its `RemotePath` (`tenants/<id>/timelines/<id>/<layer-file-name>`). The
file arrives 8 KiB-aligned, gets chunked at 100 MiB boundaries, and
lands as N rows in `yb_storage.blobs`. The `Key` abstraction never
crosses the `RemoteStorage` boundary — it lives entirely inside the
pageserver.

### Cache miss → download: it's the whole file, by design

When the pageserver doesn't have a layer locally and needs to serve a
page out of it, it does **not** issue a byte-range read for just that
page. The full layer file is downloaded from `remote_storage` into a
temp file on local disk, then renamed into place; subsequent reads of
that layer are local-disk reads.

Code path: `pageserver/src/tenant/storage_layer/layer.rs:Layer::download`
→ `pageserver/src/tenant/remote_timeline_client/download.rs::download_layer_file`
→ `download_object`, which calls

```rust
storage.download(src_path, &DownloadOpts::default(), cancel).await?;
```

`DownloadOpts::default()` has `byte_start = Unbounded`, `byte_end =
Unbounded` — the entire layer is requested.

Why the whole file, not byte ranges:

1. **The B-tree index lives at the *end* of the file**, after the
   VALUES section. Just one page lookup would need three or more
   round-trips: Summary (block 0) + several B-tree nodes near
   `index_start_blk` + the actual blob somewhere in VALUES. With a
   remote object store, each round-trip has 10–100 ms latency. One
   bulk download is cheaper.
2. **Layer files are bounded** (~128 MiB target, ~256 MiB L0 cap,
   ~294 MiB worst-case in our bench, 4.5 GB hard ceiling). A single
   sequential read amortises across every subsequent page lookup from
   that layer.

The `RemoteStorage` trait *does* support byte ranges
(`DownloadOpts { byte_start, byte_end }`) — the S3 backend implements
them via the HTTP `Range:` header — but the pageserver doesn't use them
for layer downloads. The only place ranged reads happen is the
safekeeper's WAL fetch path.

### Streaming the download (so memory stays small)

The download is **streaming on the wire**, not "load 4 GB into RAM". The
trait shape is:

```rust
pub struct Download {
    pub download_stream: Pin<Box<dyn Stream<Item = io::Result<Bytes>> + Send + Sync + 'static>>,
    pub last_modified: SystemTime,
    pub etag: Etag,
    pub metadata: Option<StorageMetadata>,
}
```

`download_layer_file` pulls chunks off `download_stream` via
`StreamExt::next` and writes them to a `BufferedWriter<TempVirtualFile>`.
Only a small fixed-size buffer ever sits in pageserver RAM.

For **S3/Azure/GCS/LocalFs**, this is true straightforwardly: each
backend's `download` returns a real byte stream backed by the upstream
HTTP body / file read syscall, so memory peaks at the buffer size
regardless of object size.

For **`YbStore`**, this required some care. The first cut bulk-fetched
all chunks with `client.query(...)`, concatenated the BYTEAs into one
`Vec<u8>`, and wrapped it in `futures::stream::once` — pageserver RAM
peak was the full layer size. Fixed in `libs/remote_storage/src/yb_store.rs::download`
by:

1. **Phase 1 (metadata):** single-row `SELECT size_bytes, etag,
   last_modified_us, metadata_json … WHERE key = $1 AND chunk_idx = 0`
   so we can serve the `If-None-Match` shortcut and compute byte-range
   math without touching the bulk data.
2. **Phase 2 (streaming chunks):** open a `tokio_postgres::RowStream`
   via `client.query_raw(...)` filtered to the chunk_idx range that
   overlaps the requested byte range. Wrap it in
   `async_stream::stream!` and yield one chunk's bytea as a `Bytes`
   per stream poll. `Bytes::from(Vec<u8>)` is zero-copy and `slice(..)`
   on it is pointer+length adjustment — no copies for the common
   whole-file case.
3. **Byte-range math:** chunks are predictably sized (each is
   `YB_CHUNK_SIZE = 100 MiB` except possibly the last), so
   `start_chunk = start_byte / YB_CHUNK_SIZE`,
   `end_chunk = (end_byte - 1) / YB_CHUNK_SIZE`, with trim offsets
   `start_byte % YB_CHUNK_SIZE` on the first chunk and
   `(end_byte - 1) % YB_CHUNK_SIZE + 1` on the last. No request
   reads chunks it doesn't need.

After the fix, `YbStore` peaks at one chunk (up to 100 MiB) in flight,
matching S3/Azure/GCS modulo chunk granularity.

### `getpage` RPC

Custom Postgres-message-typed gRPC (in `pageserver/page_api`) issued by the
compute's `NEON_SMGR` for every page miss. The pageserver:

1. Look up the (tenant, timeline) shard.
2. Find the layer-file stack covering the requested page at the requested LSN.
3. If any needed layer is not local, download from `remote_storage`.
4. Reconstruct the page.
5. Stream it back.

### WAL ingest

`WalReceiver` runs per-timeline, connects to the safekeepers via
`START_REPLICATION`, decodes WAL records, and feeds the in-memory layer.
Periodically flushes the in-memory layer to disk and uploads to remote.

### Compaction & GC

Background tasks: L0 → L1 compaction (merge many small delta layers into
fewer larger ones), image-layer generation (materialize pages at a fresh
LSN), and GC (drop layers older than `pitr_interval`).

### Remote storage

The pageserver uses `remote_storage::GenericRemoteStorage` for durability:
upload layers + index after every flush; download on demand; delete on GC.
The contract is the `RemoteStorage` trait (see
[`remote-storage.md`](remote-storage.md)).

### Code layout

- `pageserver/src/tenant/` — tenant + timeline machinery.
- `pageserver/src/tenant/remote_timeline_client/` — bridges layer events
  (created, sealed, deleted) to `RemoteStorage` operations.
- `pageserver/src/tenant/timeline/walreceiver/` — WAL ingest.
- `pageserver/src/tenant/timeline/layer_manager.rs` — in-memory layer state.
- `pageserver/src/feature_resolver.rs` — small bits of config-dependent
  logic; the only file modified for YbStore other than the variant-add
  match arms.
- `libs/remote_storage/` — the trait + all backends, including `YbStore`.

## Changes in yb-neon

### 1. `YbStore` added to `GenericRemoteStorage` (commit `cb49a6557`)

A new `RemoteStorage` impl that stores objects as rows in a YugabyteDB
table. The trait stays unchanged; this is *additive*. See
[`remote-storage.md`](remote-storage.md) for the full design.

- `libs/remote_storage/src/yb_store.rs` — the impl.
- `libs/remote_storage/src/config.rs` — adds `YbStoreConfig` struct +
  `RemoteStorageKind::YbStore(YbStoreConfig)`.
- `libs/remote_storage/src/lib.rs` — adds the `YbStore(Arc<YbStore>)`
  variant to `GenericRemoteStorage` and the dispatch arms.
- `pageserver/src/feature_resolver.rs` — adds the new arm to the
  region-inference match (returns `"yb"`).
- `pageserver/src/tenant/remote_timeline_client/upload.rs` — adds the
  new arm to the exhaustive `match storage { ... }` block.

That's the entirety of the pageserver-side change. The trait dispatch
does the rest.

### 2. Chunked, pipelined writes

The first cut of `YbStore` stored each object as one big BYTEA row. That
ran into YB's default `rpc_max_message_size` (256 MiB) when the
pageserver started uploading ~290 MiB layer files. Fix:

- Split each object into **100 MiB chunks**, keyed by
  `(key HASH, chunk_idx)`.
- `INSERT` each chunk separately, pipelined via `client.execute_raw` +
  `futures::future::try_join_all` so N chunks ride one wave of round-trips.
- `SET yb_enable_upsert_mode = true` per session so plain INSERT skips
  DocDB's read-before-write check (we always pre-DELETE the old key
  anyway, so conflicts are impossible — but the GUC lets the YB write
  path be cheaper).

See [`remote-storage.md`](remote-storage.md) for the schema.

### 3. Local layer cache still exists

This is **unchanged from upstream Neon**, but worth calling out for the
deployment model: even with YbStore configured, the pageserver still
maintains its full local layer cache under
`<workdir>/pageserver_*/tenants/.../timelines/...`. Layer files are
written *first* locally, *then* uploaded to YB. Reads prefer local; YB
is consulted on misses.

If you wipe the local cache, the pageserver re-downloads from YB on next
attach. Tested in the benchmark suite — see
`../hari-neon/benchmarks/neon-vs-yb-vs-vanilla.md` ("YbStore cold-cache
stress test").

## What's on local FS vs in YB

| State | Local FS | YB |
|---|---|---|
| Layer files | yes (cache, evictable) | yes (durable copy) |
| `index_part.json` | yes | yes |
| `tenant-manifest.json` | yes | yes |
| `initdb.tar.zst` | yes | yes |
| `metadata` | yes only | — (rebuilt from index) |
| Operational (pidfile, logs) | yes | — |

## Run / configure

```
# pageserver.toml (excerpt)
pg_distrib_dir = '...'
broker_endpoint = 'http://127.0.0.1:50061/'
control_plane_api = 'http://127.0.0.1:1244/upcall/v1/'
listen_pg_addr = "127.0.0.1:64100"
listen_http_addr = "127.0.0.1:9988"
listen_grpc_addr = "127.0.0.1:51151"
remote_storage = {
    host = "127.0.0.1",
    port = 5433,
    user = "yugabyte",
    dbname = "yb_storage",
    table_name = "blobs",
    prefix = "pageserver-yb/"
}
```

`neon_local init` accepts a top-level config with this `remote_storage`
inline table; the generated `pageserver.toml` carries it through.

## Performance characteristics

- **Steady-state reads**: served from local layer cache. Indistinguishable
  from Neon-on-S3 or Neon-on-LocalFs (whose local cache plays the same role).
- **Steady-state writes**: layer flush → local file → background upload to
  YB. Upload latency is hidden from the compute write path — `flush` to
  pageserver is async w.r.t. commit.
- **Cold pageserver** (empty layer cache, fresh re-attach): every layer
  needed for serving a query is one YB query of N chunks. The benchmark
  with 100 MB of data + cold pageserver completed `SELECT count(*)` in
  19 s on YbStore vs 19 s on LocalFs — equivalent within noise. Both pull
  the same bytes off the same local-machine disk; the abstraction layer
  costs little.
- **YB blob size**: ~24 MiB at fresh-timeline bootstrap (initdb + initial
  image layer + index_part + manifest). ~320 MiB for a 100 MiB user
  dataset after pageserver compaction. Storage growth dominated by layer
  rewrites during compaction, same as on S3.
