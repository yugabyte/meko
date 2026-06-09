# High-level architecture

## Components

| Component | Role | Process binary |
|---|---|---|
| Compute | User-facing Postgres backend. Speaks Postgres wire protocol to clients. | `pg_install/v17/bin/postgres` (with Neon smgr patches) |
| `compute_ctl` | Supervisor for the compute: applies config, starts/stops PG, runs catalog migrations. | `target/debug/compute_ctl` |
| Safekeeper | Raft acceptor for WAL. Receives WAL from compute, durably stores it, streams to pageserver. **Limited to one instance per cluster in YB WAL mode** — YB's own Raft already provides quorum durability. | `target/debug/safekeeper` |
| Pageserver | Materializes pages from WAL and serves them to compute on demand. Holds the only durable copy of relfiles (via `remote_storage`). | `target/debug/yb-pageserver` |
| Storage controller | Tracks cluster topology: which tenants are on which pageservers, which safekeepers hold which timelines, generations, etc. | `target/debug/storage_controller` |
| Endpoint storage | Small auxiliary service for endpoint-scoped scratch data (LFC pre-warm hints, etc.). | `target/debug/endpoint_storage` |
| YugabyteDB | Durable storage backend for everything Neon-internal. Replaces S3/Azure/GCS *and* the storage controller's local Postgres. | external (`yugabyted`) |

## Write path (one INSERT from a client)

```
client
  |  SQL
  v
Compute (PG backend)
  |  WAL record buffer-flushed by walwriter / commit
  v
walproposer  ────────────►  single Safekeeper
                              |  write_wal()  →  INSERT into yb_storage.wals
                              |                  (synchronous, one row per call)
                              |  flush_wal()  → mark flush_lsn (YB is durable)
                              v
                            ACK to walproposer  (compute can return commit to client)
```

The compute's commit latency is bounded by the safekeeper's `flush_wal()`,
which on yb-light's YB-backed safekeeper is a YB INSERT round-trip (~ms over
local YB; more on the network). This is the headline performance cost of
moving WAL into YB.

## Read path (a page miss in the compute)

```
client
  |  SELECT
  v
Compute backend  --(cache miss)-->  shared_buffers  --(miss)-->  LFC  --(miss)-->
                                                                          |
                                                                  getpage RPC
                                                                          v
                                                                    Pageserver
                                                                          |
                          local layer file?  ────yes────►  reconstruct page from WAL records
                                          \
                                           no
                                            \
                                             v
                                       fetch layer from YbStore
                                         (yb_storage.blobs, prefix pageserver-yb/...)
                                             |
                                       write to local layer cache
                                             v
                                       reconstruct page
                                             v
                                       return page bytes to compute
```

If the pageserver has been restarted with an empty layer cache, the first read
of each layer is a YB SELECT of multiple chunks; subsequent reads hit local
disk. See [`pageserver.md`](pageserver.md) for details.

## What's durable where

(The `Path / key` column below describes the **logical** RemoteStorage
key the pageserver/safekeeper use. In YB the path is split across the
`namespace` / `tenant_id` / `timeline_id` / `key` columns — see
[`yugabyte.md`](yugabyte.md). The mapping is round-trip lossless.)

| Subsystem state | yb-light backend | Path / key |
|---|---|---|
| Pageserver layer files | YB `yb_storage.blobs` | `pageserver-yb/tenants/<id>/timelines/<id>/<layer>` (one BYTEA chunk per row, ≤ 100 MiB each); PK `((tenant_id, timeline_id, key) HASH, namespace, chunk_idx)` |
| Pageserver `index_part.json` | YB `yb_storage.blobs` | `pageserver-yb/tenants/<id>/timelines/<id>/index_part.json-<gen>` |
| Pageserver `initdb.tar.zst` | YB `yb_storage.blobs` | `pageserver-yb/tenants/<id>/timelines/<id>/initdb.tar.zst` |
| Pageserver tenant manifest | YB `yb_storage.blobs` | `pageserver-yb/tenants/<id>/tenant-manifest-<gen>.json` |
| Safekeeper control file | YB `safekeeper.timeline_state` schema | one row per timeline, typed columns |
| Safekeeper live WAL | YB `yb_storage.wals` | `((tenant_id, timeline_id) HASH, segno, chunk_offset)` |
| Storage controller relational state | YB `yb_storage` DB, `storage_controller` schema | 7 Diesel-managed tables (alongside pageserver/safekeeper/public schemas) |
| Pageserver local layer cache | local FS | `.neon-yb/pageserver_*/tenants/.../timelines/.../<layer>` |
| Safekeeper local FS | local FS | `.neon-yb/safekeepers/sk*/` — only logs + pidfile (no WAL, no control file, no id) |
| Compute `pgdata` | local FS | `.neon-yb/endpoints/main/pgdata/` (catalogs + ephemeral WAL pre-ship buffer) |

## What did *not* change from upstream Neon

These subsystems run unmodified upstream Neon code in yb-light:

- The Postgres compute itself (`pg_install/v17/bin/postgres` with Neon smgr
  patches from `vendor/postgres-v17`).
- `compute_ctl` and the compute spec / endpoint lifecycle.
- The pageserver's `RemoteStorage` *trait* — we added `YbStore` as the
  primary backend (with `Unreliable` retained for tests); the upstream
  S3, Azure, GCS, and LocalFs variants have been removed, and yb-pageserver
  rejects non-YbStore configs.
- The pageserver's getpage RPC, WAL receiver, layer-file format, compaction,
  GC, and tenant attach logic.
- The safekeeper's Raft acceptor logic, WAL stream decoder, walproposer wire
  protocol.
- The storage controller's Rust code: not a single line was changed when we
  pointed it at YB; the swap was purely the DB connection URL.

## Topology in our current dev cluster

Single host, single YB instance, single pageserver, single safekeeper.

```
yugabyted                       :7100 (master), :5433 (ysql)
  +-- db `yb_storage`
        +-- schema `public`
        |     +-- type `blob_namespace` ENUM ('pageserver', 'safekeeper')
        |     +-- table `blobs`        chunked BYTEA + denormalized metadata,
        |     |                        PK ((tenant_id, timeline_id, key) HASH,
        |     |                            namespace, chunk_idx)
        |     |                        key column holds the bare filename — tenants/
        |     |                        and timelines/ literals + the UUID values are
        |     |                        not stored, they live in their own columns
        |     +-- table `wals`         per-write rows,
        |                              PK ((tenant_id, timeline_id) HASH, segno, chunk_offset)
        +-- schema `storage_controller`  Diesel-managed; 7 tables
        +-- schema `pageserver`
        +-- schema `safekeeper`          incl. `timeline_state` (typed control-file rows)

yb-light binaries (target/debug/)
  storage_controller --database-url postgresql://yugabyte@127.0.0.1:5433/yb_storage --listen 127.0.0.1:<port>
  yb-pageserver   (config from on-disk pageserver.toml; neon_local defaults :64100 pg, :9988 http, :51151 grpc — base ports 64000/9898/51051 + i offset)
  safekeeper --id 1 --wal-yb-config <YbStore TOML>   (neon_local defaults :5474 pg, :7686 http — base ports 5454/7676 + 20 offset)
  endpoint_storage   :9983
  compute_ctl --pgdata .neon-yb/endpoints/main/pgdata --connstr postgresql://cloud_admin@127.0.0.1:55440/postgres
    +-- postgres (v17.5 + Neon patches)  :55440
```

A production deployment would have M pageservers sharded by tenant and a
YB cluster that's actually distributed. **The safekeeper count stays at 1**
when `--wal-yb-config` is set — YB already provides the quorum durability
that a multi-safekeeper Raft would. Running two protocols (YB's Raft + the
safekeeper's Paxos) on top of each other is rejected at boot and at
runtime; see [`safekeeper.md`](safekeeper.md) §5.
