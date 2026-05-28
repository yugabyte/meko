# Pageserver

The pageserver is the only component that holds the durable copy of user
table & index data. Computes don't have relfiles; they ask the pageserver
for pages on demand via `getpage` RPC.

In yb-light, the pageserver's "remote storage" (where it durably stashes
layer files, the index, the initdb tarball, and tenant manifests) is YB
instead of S3/Azure/GCS. The pageserver itself was barely modified —
all the YB stuff is plumbed via the existing `RemoteStorage` trait.

## Inherited from Neon

### Sharding (tenants vs shards)

A **tenant** can be horizontally striped across multiple pageservers
for scaling. The unit of striping is a **shard**: each tenant has
`ShardCount` shards, identified by `(TenantId, ShardNumber, ShardCount)`
(`libs/pageserver_api/src/shard.rs::TenantShardId`). The
`ShardIdentity` carries the `stripe_size`; keys are mapped to shard
numbers by hashing `(spcnode, dbnode, relnode)` and indexing by
`stripe_size`. The storage controller's `tenant_shards` table is the
source of truth for "which pageserver owns shard N of tenant T".

On-disk directory layout reflects shards:

```
pageserver_*/tenants/<tenant_id>-<shard_count>-<shard_number>/
                     timelines/<timeline_id>/<layer-files…>
```

In the running yb-light dev cluster we use `ShardCount = 0` (unsharded;
the tenant is a single shard hosted on a single pageserver), but the
machinery is there for production deployments to scale a single tenant
across multiple pageservers. From the layer-file machinery's
perspective, each shard is independent — it has its own layer files,
its own compactor, its own remote_storage prefix.

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

#### Concrete example: page 7 of `bench_t`'s main fork

From the live yb-light cluster:

| Source | Value |
|---|---|
| `pg_tablespace.oid` for `pg_default` | `1663` |
| `pg_database.oid` for `postgres` | `5` |
| `pg_class.relfilenode` for `bench_t` | `16384` |
| Fork (heap data) | `MAIN_FORKNUM = 0` |
| Block within fork | `7` |

`rel_block_to_key` produces:

```rust
Key {
    field1: 0x00,    // u8  — relation-page namespace
    field2: 1663,    // u32 — spcNode
    field3: 5,       // u32 — dbNode
    field4: 16384,   // u32 — relfilenode
    field5: 0,       // u8  — forknum
    field6: 7,       // u32 — blknum
}
```

`Key::write_to_byte_slice` lays the fields out big-endian, giving the
18-byte storage key:

```
offset:   0    1  2  3  4    5  6  7  8    9 10 11 12   13   14 15 16 17
        ┌────┬───────────┬───────────┬───────────┬────┬───────────┐
bytes:  │0x00│00 00 06 7F│00 00 00 05│00 00 40 00│ 00 │00 00 00 07│
        └────┴───────────┴───────────┴───────────┴────┴───────────┘
        field1   field2      field3      field4    f5     field6
        (ns)   (spcnode)   (dbnode)   (relnode)   (fork) (blknum)
```

A delta-layer B-tree search key for this page at LSN `0/1E000000`
appends the LSN as big-endian u64, producing the 26-byte `DeltaKey`:

```
00  00 00 06 7F  00 00 00 05  00 00 40 00  00  00 00 00 07  ║  00 00 00 00 1E 00 00 00
└─────────────────────── 18-byte Key ──────────────────────┘  └──── LSN (u64 be) ────┘
```

#### Sort-order properties this layout buys

Because all fields are big-endian and the encoding order is
`(spcnode, dbnode, relnode, forknum, blknum)`:

- All pages of one relation are **contiguous** in the keyspace.
- All relations of one database are contiguous.
- All databases of one tablespace are contiguous.
- Within a relation, adjacent block numbers have adjacent keys, so a
  layer covering blocks 0..10 000 of one rel is a single tight
  `Range<Key>`, not a scattered set.
- `forknum` lives between `relnode` and `blknum`, so all main-fork
  pages of a rel come first, then the FSM pages, then the VM. The
  compactor processes one fork at a time without seeking around.

This is why the layer file's `key_range: Range<Key>` in the Summary
header is expressive enough to describe its coverage in two `Key`
values — start and end — rather than an arbitrary set.

#### End-to-end: one `SELECT … WHERE id = 42`

Walking through a query that does an index lookup:

1. PG planner picks an index scan on `bench_t_pkey` (a different rel,
   say `relfilenode = 16386`).
2. Index scan needs page X of the index: `smgrread(spcnode=1663,
   dbnode=5, relnode=16386, forknum=0, blknum=X)`.
3. `NEON_SMGR` sends a `GetPageAtLsnRequest` with those four ints +
   the snapshot LSN.
4. Pageserver builds `Key { 0, 1663, 5, 16386, 0, X }` via
   `rel_block_to_key`.
5. Finds the layer whose `key_range` contains that `Key` at the
   requested LSN; walks its B-tree (`DeltaKey` or `Key` depending on
   layer kind); gets a `BlobRef`; reads the value.
6. Returns the 8 KiB page to the compute.
7. Compute decodes the index tuple, gets the heap row's `ctid`, then
   does a **second** `getpage` RPC for the heap block — but now
   `relnode = 16384` (bench_t's main relfilenode), so `field4`
   changes, a different `key_range` gets hit, possibly a different
   layer file.

So one user-level SELECT translates into a small handful of `Key`
lookups, each one a tightly-encoded 18-byte tuple whose ordering
properties drive everything from layer-file design through
compaction.

### Layer files

Two flavors:

- **Image layer**: full materialized pages (`(Key, Lsn) → page bytes`) at
  a single LSN. Read-cheap, build-expensive.
- **Delta layer**: WAL-record deltas over an LSN range
  (`(Key, Lsn) → (will_init | WalRecord | Image)`). Append-only output of
  WAL ingestion + L0→L1 compaction.

Filename encodes the layer's coverage. The format is:

```
<key_start>-<key_end>__<lsn_start>-<lsn_end>-<generation>
```

with widths:

| Field | Width | Encoding |
|---|---|---|
| `key_start` | 36 hex chars (18 bytes) | the full `Key` struct, big-endian |
| `key_end` | 36 hex chars (18 bytes) | exclusive upper bound |
| `lsn_start` | 16 hex chars (8 bytes) | `Lsn` as `u64`, big-endian |
| `lsn_end` | 16 hex chars (8 bytes) | exclusive upper bound; for image layers `lsn_start == lsn_end` |
| `generation` | 8 hex chars (4 bytes) | tenant attach generation |

A real example from `.neon-yb/pageserver_1/...`:

```
000000000000000000000000000000000000-FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF__0000000001540438-00000000015404B1-00000001
└─────────── key_start (18 B) ──────┘└──────────  key_end (18 B) ──────┘  └─ lsn_start ──┘ └── lsn_end ──┘ └─ gen ─┘
```

So a directory listing alone tells the pageserver which file covers
which `(key_range, lsn_range)`. Image layers have `lsn_start == lsn_end`
because their Summary uses a single `lsn: Lsn`, not a range — see
"Image layers vs delta layers" below.

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

### LSN-versioned MVCC and time-travel reads

The pageserver doesn't store "the current state of each page" — it stores
**every version of every page within the configured retention window**.
Every read takes an explicit LSN, and the pageserver materializes what
that page looked like at that LSN by walking the layer stack.

This is a different concept from Postgres tuple-level MVCC (`xmin` /
`xmax` / visibility maps), which operates *inside* page bytes and is
what individual transactions use to decide which row versions to see.
LSN-versioned MVCC is one level deeper: at any given LSN, a page has
*one* set of bytes, and that's what the pageserver returns. The
compute then runs its own tuple-level MVCC on those bytes.

Two LSNs that matter:

| LSN | Source | Meaning |
|---|---|---|
| **`last_record_lsn`** | timeline state | The latest WAL the pageserver has ingested. Reads at this LSN see the most recent durable state. |
| **`applied_gc_cutoff_lsn`** | timeline state | The oldest LSN you can still read. Layers below this are eligible for GC; once they're gone, history before this LSN is unrecoverable. |

Bounded by the tenant config:

| Knob | Default | Meaning |
|---|---|---|
| `pitr_interval` | 7 days | How far back time-travel reads stay supported. GC waits this long before pruning. |
| `gc_horizon` | 64 MiB | LSN window above `last_record_lsn` to retain regardless of time. |

The active window between `applied_gc_cutoff_lsn` and `last_record_lsn`
is what makes branching ("create a new timeline as of LSN L") and PITR
("restore the database as of yesterday 14:00") possible — those
operations just point a new timeline at an LSN inside the window.

### Image layers vs delta layers: one LSN vs an LSN range

This is the central asymmetry in the storage format. The `Summary`
fields tell the story:

| Layer | `lsn` field shape | What that means |
|---|---|---|
| **Image** | `lsn: Lsn` (singular) | Every entry in this file is the page bytes **at exactly this one LSN**. Many keys, one LSN. |
| **Delta** | `lsn_range: Range<Lsn>` | Every entry is a WAL record (or image) at *some* LSN inside the range. Many keys, many LSNs per key. |

So **an image layer contains many 8 KiB pages, all materialised at the
same single LSN**. The B-tree index on an image layer is keyed by `Key`
alone (18 bytes) — no LSN — because the LSN is implicit and identical
for the whole file. If you want page `K` at a different LSN, you go
find a *different* image layer (or stack of delta layers on top of
this one).

A delta layer holds the *changes* over an LSN range. One key can appear
multiple times in a delta layer — once per WAL record affecting it
within that range. The B-tree on a delta layer is keyed by
`DeltaKey = Key ++ LSN` (26 bytes) for exactly this reason.

#### Reconstruction across layer kinds (the read path)

For a read of `(K, target_lsn)`:

1. Find the most recent **image layer** that covers `K` with
   `image.lsn ≤ target_lsn`. That's the **starting state**.
2. Find all **delta layers** that cover `K` with
   `delta.lsn_range ∩ (image.lsn, target_lsn] ≠ ∅`.
3. Within each delta layer, fetch the records for `K` whose LSN is in
   `(image.lsn, target_lsn]`, in LSN order.
4. Apply those records on top of the starting image via walredo
   (a Postgres binary running in a sidecar that knows how to replay
   WAL records onto pages). Each `Value::WalRecord` mutates the
   page; a `Value::Image` (in a delta layer) or a record with
   `will_init = true` resets the page from scratch.
5. After replaying the last record at or before `target_lsn`, return
   the page bytes.

If no image layer exists for `K`, walk the delta layers backwards
until you hit a record with `will_init = true` (or an embedded
`Value::Image`), then forward-replay from there.

This is why **image layers are an optimization, not a requirement**.
Without them, every read would walk every delta record back to the
timeline's creation — fast at the start of a timeline's life,
catastrophically slow after enough churn. Compaction materializes
image layers periodically to bound replay depth.

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
| `S3_UPLOAD_LIMIT` | **4.5 GB** | Emergency ceiling, not a target. The only way to hit it is the pathological "single hot key with > 4.5 GB of WAL accumulated since the last image" case, at which point `BatchLayerWriter` **bails with an error** ("a single key is updated too often") rather than producing the layer. Set to stay under S3's 5 GB single-PUT object limit. (`pageserver/src/tenant/storage_layer/layer.rs:39`) |
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
                  │  Image layer (~128 MiB target)   │  ← materialized snapshot,
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
   (4.5 GB) — at which point it actually **errors out** with
   *"a single key is updated too often"*. The 4.5 GB number is not
   a routine split point; it's the floor at which the writer
   refuses to produce a layer at all, because the result couldn't
   be PUT to S3 in a single request.

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

#### The "single hot key" pathological case

A "hot key" means a single Postgres 8 KiB page being updated repeatedly.
Recall a `Key` corresponds to one `(rel, fork, blknum)` triple — so
many updates to *different rows* on the *same page* all stack records
on the same `Key`. Real workloads where this happens:

| Workload | Why one key stays hot |
|---|---|
| Counter table (`UPDATE counters SET n = n + 1` on a single row) | Same heap block every update |
| Small hot-pinned lookup row | Same page |
| Index leaf for a monotonic PK (`bigserial`) | Inserts target the rightmost leaf until it splits |
| Heap-page tail during a bulk-INSERT | Same block while it fills |
| `pg_class` / `pg_attribute` row in a heavily-DDL'd schema | Same catalog page |

Why this builds up: WAL records for the same key get stacked as
`Value::WalRecord` entries in delta layers. Image-layer materialization
(`image_creation_threshold = 3`) is supposed to short-circuit
reconstruction by snapshotting the page's current state, but if WAL
arrives for one key faster than image-layer creation can keep up,
delta records keep piling. L0→L1 compaction won't split mid-key, so
all the records for that key go into one output file. The file grows
past the 128 MiB target (warning at ~256 MiB), past 1 GB, …

At `S3_UPLOAD_LIMIT = 4.5 GB` the writer **errors out** with:

```
a single key is updated too often: key={…}, estimated_size={…}, and
the layer file cannot be produced
```

— because the resulting object wouldn't fit in a single S3 PUT.

**Back-of-envelope**: a NeonWalRecord is ~30–100 bytes serialized.
4.5 GB ÷ 80 B ≈ 56 M records. So you need on the order of tens of
millions of WAL records targeting one page since the last image layer
was materialized to hit this. A single-row counter table at 50K ops/s
gets there in ~20 minutes if image-layer creation is starved.

**Mitigations** when the error fires:
- Trigger a manual image layer:
  `PUT /v1/tenant/<id>/timeline/<id>/compact?force_image_layer_creation=true`.
- Tune `image_creation_threshold` lower for the affected tenant.
- Fix the application: a workload that updates one page 56 M times
  before an image layer materializes is almost certainly contention
  on a hot row that ought to be partitioned.

### Practical sizes in the running yb-light cluster

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

### What compaction actually does

A timeline that just keeps ingesting WAL forever, with no compaction,
would become useless: every `getpage` would walk an ever-growing stack
of L0 delta layers, paying the cost of every WAL record ever applied
to that key. Compaction is the background work that bounds that cost.

There are three distinct things happening under the umbrella name
"compaction", in `pageserver/src/tenant/timeline/compaction.rs`. The
entry point is `Timeline::compact_legacy` (line 1206), called on a
20-second loop by default.

#### 1. L0 → L1 compaction

L0 layers come out of WAL ingest. Each L0 is a delta layer covering
**the full key range** over a **small LSN range** (one
`checkpoint_distance` worth of WAL). After 10 L0s accumulate, you have
10 layers each spanning the entire keyspace — every page read has to
consult all of them.

`compact_level0` rewrites those N L0s into M L1 delta layers, where
each L1 covers a **smaller key range** over a **larger LSN range**.
That's a transpose:

```
Before (10 L0s — wide key range, narrow LSN range each):
  ┌───────────────────────────────────────────────────┐
  │ L0:  keyspace-wide │ lsn 100..120  │              │
  │ L0:  keyspace-wide │ lsn 120..140  │              │
  │ L0:  keyspace-wide │ lsn 140..160  │              │
  │ ...                                               │
  │ L0:  keyspace-wide │ lsn 280..300  │              │
  └───────────────────────────────────────────────────┘
  Every page read must scan all 10 layers.

After L0→L1 compaction (each L1 — narrow key range, wide LSN range):
  ┌─────────┬─────────┬─────────┬─────────┬─────────┐
  │ L1: keys│ L1: keys│ L1: keys│ L1: keys│ L1: keys│
  │  0..100 │ 100..200│ 200..300│ 300..400│ 400..500│
  │ lsn 100 │ lsn 100 │ lsn 100 │ lsn 100 │ lsn 100 │
  │  ..300  │  ..300  │  ..300  │  ..300  │  ..300  │
  └─────────┴─────────┴─────────┴─────────┴─────────┘
  A page read only touches the one L1 whose key range covers it.
```

The split happens at `compaction_target_size = 128 MiB` boundaries at
key transitions. Within one L1, all records for a given key are still
grouped together in LSN order, so a single key's history can be
replayed by reading one contiguous chunk of the file (this is what the
`VectoredReadPlanner` exploits).

L0 compaction is **read-amplification critical**: too many L0s and
every read is slow. The compaction loop runs aggressively on L0s
(default `compaction_l0_first = true` + a dedicated semaphore), and
the `--compact?force-l0-compaction` HTTP endpoint forces an immediate
pass.

#### 2. Image-layer creation

L1 deltas still require walredo to materialize each page (each read is
"start from the image, apply records"). If a key range accumulates
deep delta stacks, reads get slow even with L0→L1 done — they're
walking many L1s.

When `repartition` detects that a key range has `image_creation_threshold`
(default 3) delta layers above its last image, the compactor
materializes a new image layer:

1. Pick an LSN — typically the current `last_record_lsn` (or the L0→L1
   boundary).
2. For every key in that partition's range, do a vectored read at the
   chosen LSN: reconstruct the page once.
3. Write all those reconstructed pages into a new image-layer file
   (Summary's `lsn` = the chosen LSN; `key_range` = the partition).

After this, page reads in that key range start from the new image
instead of walking deep delta stacks. The deltas it superseded
become eligible for GC once they're older than `pitr_interval`.

Image layers are **expensive to create** (you have to reconstruct
every page in the partition), so the compactor partitions the
keyspace and only creates images for partitions that need them —
guided by `image_creation_threshold` plus heuristics in
`repartition`.

#### 3. GC and gc-compaction

After `pitr_interval` (default 7 days), data older than the retention
window can be dropped. The simple GC path drops whole layer files
whose `lsn_range.end < applied_gc_cutoff_lsn`. Layers that straddle
the cutoff stay.

`gc_compaction` (a newer pass, `DEFAULT_GC_COMPACTION_ENABLED = true`)
is the smarter variant: it rewrites partially-stale layers, dropping
records below the cutoff while preserving the still-needed tail. It
also drops obsolete delta records that have been superseded by image
layers within the PITR window. This recovers space without losing
PITR capability.

#### Compaction algorithms

There are two algorithms in the tree:

- **Legacy** (`DEFAULT_COMPACTION_ALGORITHM = Legacy`) — the one
  described above. The `compact_legacy` entry point in
  `compaction.rs:1206`.
- **Tiered** — a newer leveled algorithm with finer control over
  layer densities. Opt-in via tenant config; not the default in our
  cluster.

#### Why this matters for `YbStore`

Compaction is mostly invisible to the storage backend: it produces
new layer files and deletes old ones. From `YbStore`'s perspective:

- New layer file → `upload` (multi-row INSERT, chunked at 100 MiB).
- Compacted-away layer file → `delete`.
- Two layers that compaction merged into one → first `upload` of the
  new one, then `delete` of the two old ones. No in-place updates.

The throughput cost of compaction on the YB side is mostly DELETE
load: each compaction pass drops the layers it just superseded.
With the PK `((tenant_id, timeline_id, key) HASH, namespace,
chunk_idx)`, a `DELETE FROM blobs WHERE tenant_id=? AND
timeline_id=? AND namespace=? AND key=? AND generation IS NOT
DISTINCT FROM ?` is a hash lookup on the (tenant, timeline, key)
hash + a small range scan within the tablet — fast in YB terms but
real load if compaction is busy. The generation column is matched
explicitly so a delete at gen=N never accidentally clears a row
written at a newer gen.

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

## Changes in yb-light

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
