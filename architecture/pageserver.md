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

### Layer files

Two flavors:

- **Delta layer**: WAL records over an LSN range, organized by key.
- **Image layer**: full materialized pages at a specific LSN.

Layer files have names of the form
`<key_start>-<key_end>__<lsn_start>-<lsn_end>-vN-<generation>`.
File format: per-key segments + a chunked block index. The pageserver
reads them sequentially during reconstruction. A "page reconstruct" walks
the layer stack at the requested LSN, finds the most recent image layer
plus all newer delta layers, and replays them.

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
