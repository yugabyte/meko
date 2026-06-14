# Pageserver

The pageserver is the only component that holds the durable copy of user
table & index data. Computes don't have relfiles; they ask the pageserver
for pages on demand via `getpage` RPC.

In yb-light, the pageserver was forked into **yb-pageserver**: page data
lives in YB tables (via `PageStore` / `PageWriter`), and the upstream
layer-file machinery has been deleted. The remaining `RemoteStorage`
artifacts (basebackup, initdb tarball, tenant manifest) still go through
`YbStore`. See [`yb-pageserver.md`](yb-pageserver.md) for the fork-specific
design (PageStore, PageWriter, image_materializer, per-key LRU).

## Inherited from Neon

### Sharding (tenants vs shards)

A **tenant** can be horizontally striped across multiple pageservers
for scaling. The unit of striping is a **shard**: each tenant has
`ShardCount` shards, identified by `(TenantId, ShardNumber, ShardCount)`
(`libs/pageserver_api/src/shard.rs::TenantShardId`). The
`ShardIdentity` carries the `stripe_size`; keys are mapped to shard
numbers by hashing `(spcnode, dbnode, relnode)` and indexing by
`stripe_size`. The storage controller's `tenants` table is the
source of truth for "which pageserver owns shard N of tenant T".

On-disk directory layout reflects shards:

```
pageserver_*/tenants/<tenant_id>-<shard_count>-<shard_number>/
                     timelines/<timeline_id>/<layer-files…>
```

In yb-light only single-shard tenants are supported, and the notion of
shards is being removed entirely (epic #73). Keyspace partitioning
(`KeyspaceRange` / `KeySpace::partition`, was `ShardedRange`) no longer
consults a shard identity — it chunks purely by size. The residual
`ShardCount` / `ShardIdentity` types still exist in
`libs/pageserver_api/src/models.rs` + `libs/utils/src/id.rs` and a few
wire/DB signatures, pending their removal in later stages of #73.

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

### Image-layer materialization

`image_creation_threshold` is implemented by the `image_materializer` in
yb-pageserver — see [`yb-pageserver.md`](yb-pageserver.md).

### Practical sizes in the running yb-light cluster

`YbStore` now carries only a handful of artifact kinds. Page data is in
`yb_storage.pages` (see [`yb-pageserver.md`](yb-pageserver.md)).

| Artifact | Observed size |
|---|---|
| `initdb.tar.zst` (per timeline, written once) | ~1.6 MiB |
| `index_part.json-<gen>` | < 1 KiB |
| `tenant-manifest-<gen>.json` | < 200 B |
| Basebackup (cached per-LSN) | tens of MiB |

`YB_CHUNK_SIZE` in `libs/remote_storage/src/yb_store.rs:72` is still
100 MiB; it now only sizes basebackup and initdb writes, since layer
files no longer exist.

### `getpage` RPC

Custom Postgres-message-typed gRPC (in `pageserver/page_api`) issued by the
compute's `NEON_SMGR` for every page miss. The pageserver:

1. Look up the (tenant, timeline) tenant slot.
2. Walk the YB page-row chain for the requested `(Key, LSN)` via
   `PageStore::read_chain` (anchor image + later deltas).
3. Walredo to reconstruct the page if the chain doesn't end on an image.
4. Stream it back.

### WAL ingest

`WalReceiver` runs per-timeline, connects to the safekeeper via
`START_REPLICATION`, and decodes WAL records, routing each record
through `PageWriter` into YB. No in-memory layer is opened; the flush
loop (`Timeline::maybe_spawn_flush_loop`) is a no-op in the fork.

### Remote storage

The pageserver uses `remote_storage::GenericRemoteStorage` for durability,
but only for the basebackup / initdb / index / manifest artifacts (see
sizes table above). Page data goes through `PageStore` into
`pageserver.pages` directly, not via the `RemoteStorage` trait. The
trait contract is documented in [`remote-storage.md`](remote-storage.md).

### Code layout

- `pageserver/src/page_server/` — tenant + timeline machinery
  (the `PageServer` struct, one open timeline per process).
- `pageserver/src/page_server/timeline/walreceiver/` — WAL ingest.
- `pageserver/src/page_store.rs` — YB-backed row store for page data.
- `pageserver/src/page_writer.rs` — buffered write path; routes records
  to `PageStore` and serves blended reads from the in-RAM buffer +
  PageStore.
- `pageserver/src/image_materializer.rs` — background image
  materialiser (replaces image-layer compaction).
- `pageserver/src/unified_pages.rs` — shared page-row types.
- `pageserver/src/pgdatadir_mapping.rs` — `(Key, LSN)` ↔ Postgres
  mapping.
- `pageserver/src/walredo.rs`, `pageserver/src/walredo/` — walredo
  sidecar.
- `libs/remote_storage/` — the trait + `YbStore` (basebackup, initdb,
  manifest).

## Changes in yb-light

### 1. `YbStore` added to `GenericRemoteStorage` (commit `cb49a6557`)

A new `RemoteStorage` impl that stores objects as rows in a YugabyteDB
table. The trait stays unchanged; this is *additive*. See
[`remote-storage.md`](remote-storage.md) for the full design.

- `libs/remote_storage/src/yb_store.rs` — the impl.
- `libs/remote_storage/src/config.rs` — adds
  `RemoteStorageKind::YbStore(ysql_client::YbConnConfig)` (the shared
  connection block; no separate `YbStoreConfig` struct).
- `libs/remote_storage/src/lib.rs` — adds the `YbStore(Arc<YbStore>)`
  variant to `GenericRemoteStorage` and the dispatch arms.

The `remote_timeline_client` was subsequently unwound; only basebackup,
initdb, and tenant-manifest still flow through `YbStore`.

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

## What's on local FS vs in YB

| State | Local FS | YB |
|---|---|---|
| Page data (`(Key, LSN) → bytes`) | — | `yb_storage.pages` (durable) |
| Per-key cache (LRU, eviction-bounded) | RAM only | — |
| `index_part.json` | yes (cache) | yes (durable) |
| `tenant-manifest.json` | yes (cache) | yes (durable) |
| `initdb.tar.zst` | yes (cache) | yes (durable) |
| Timeline state (was the `metadata` file) | — | `pageserver.timeline_state` row |
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
yb_storage_conn = {
    host_ports = "127.0.0.1:5433",
    user = "yugabyte",
    dbname = "yb_storage"
}
```

Table names are not configured: this one block serves both the page
store (`pageserver.pages` / `slru_pages` / `meta`) and the object store
(`public.blobs`), each hardcoded by the component.

`yb_light_local init` accepts a top-level config with this `yb_storage_conn`
inline table; the generated `pageserver.toml` carries it through.

`YbStore` is the only `RemoteStorage` backend in yb-light; LocalFs, S3,
Azure, and GCS were removed (commit `f26243aaa` "drop LocalFs").

## Performance characteristics

- **Steady-state reads**: `PageStore` reads against `yb_storage.pages`
  for a `(Key, LSN)` chain. The per-key in-RAM cache (LRU, bounded) hits
  the cache-only fast path when warm; misses run one SQL `read_chain`
  per key.
- **Steady-state writes**: `PageWriter::put` buffers records in RAM,
  then issues batched `PageStore::put_pages` into YB. The walreceiver
  back-pressures on `backpressure_bytes` if the pre-flush buffer can't
  drain.
- **Image materialisation**: triggered at write time when a key has
  accumulated `image_creation_threshold` WAL records since the last
  image. Reconstructs once and writes the image row to YB, then trims
  the cache for that key.
- **GC**: image-row writes supersede older deltas; cleanup is a YB
  delete against `pages` keyed by `(tenant, timeline, key)`.
