# High-level architecture

## Components

| Component | Role | Process binary |
|---|---|---|
| Compute | User-facing Postgres backend. Speaks Postgres wire protocol to clients. | `pg_install/v17/bin/postgres` (with Neon smgr patches) |
| `compute_ctl` | Supervisor for the compute: applies config, starts/stops PG, runs catalog migrations. | `target/debug/compute_ctl` |
| Safekeeper | Raft acceptor for WAL. Receives WAL from compute, durably stores it, streams to pageserver. **Limited to one instance per cluster in YB WAL mode** — YB's own Raft already provides quorum durability. | `target/debug/safekeeper` |
| Pageserver | Materializes pages from WAL and serves them to compute on demand. Holds the only durable copy of relfiles (via `remote_storage`). | `target/debug/yb-pageserver` |
| Storage controller | Tracks cluster topology: which tenants are on which pageservers, which safekeepers hold which timelines, generations, etc. | `target/debug/storage_controller` |
| Endpoint storage | Small auxiliary service for endpoint-scoped scratch data (LFC pre-warm hints, etc.). One instance **per endpoint**, co-located under `endpoints/<name>/endpoint_storage`. | `target/debug/endpoint_storage` |
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
                                         (yb_storage public.blobs, namespace='pageserver')
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
| Storage controller relational state | YB `yb_storage` DB, `storage_controller` schema | 8 Diesel-managed tables (alongside pageserver/safekeeper/public schemas) |
| Compute endpoint registry | YB `yb_storage` DB, `storage_controller.endpoints` | one row per endpoint: endpoint_id, tenant_id, timeline_id + opaque `EndpointConf` JSON (`spec`). Replaces the old per-endpoint `endpoint.json` folder |
| Pageserver local FS | local FS | `.yb_light/endpoints/<endpoint>/pageserver/` — `pageserver.toml`, `identity.toml`, `metadata.json`, log + pidfile (no layer files; pages live in YB) |
| Safekeeper local FS | local FS | `.yb_light/endpoints/<endpoint>/safekeeper/` — only logs + pidfile (no WAL, no control file, no id) |
| Compute `pgdata` + runtime | local FS | `.yb_light/endpoints/<endpoint>/` — runtime working state only (`pgdata/` catalogs + ephemeral WAL pre-ship buffer, logs, pidfile, `config.json`), plus the timeline's co-located `pageserver/` + `safekeeper/`; created on `endpoint create`, removed on destroy |

## What did *not* change from upstream Neon

These subsystems run unmodified upstream Neon code in yb-light:

- The Postgres compute itself (`pg_install/v17/bin/postgres` with Neon smgr
  patches from `vendor/postgres-v17`).
- `compute_ctl` and the compute spec. (The endpoint *registry* — what
  `yb_light_local` persists per endpoint — moved out of `endpoint.json` folders
  into the `storage_controller.endpoints` table; `compute_ctl` itself is
  unchanged.)
- The pageserver's `RemoteStorage` *trait* — we added `YbStore` as the
  primary backend (with `Unreliable` retained for tests); the upstream
  S3, Azure, GCS, and LocalFs variants have been removed, and yb-pageserver
  rejects non-YbStore configs.
- The pageserver's getpage RPC, WAL receiver, layer-file format, compaction,
  GC, and tenant attach logic.
- The safekeeper's Raft acceptor logic, WAL stream decoder, walproposer wire
  protocol.
- The storage controller's Rust code: nearly untouched when we pointed it
  at YB. The only addition is a `--yb-config` flag that takes the shared
  `YbConnConfig` block and builds the DB connection URL internally via
  `YbConnConfig::to_database_url()`; everything else (diesel, migrations,
  queries) runs unchanged against YB's Postgres-compatible YSQL frontend.

## Topology in our current dev cluster

Single host, single YB instance, one pageserver + one safekeeper **per
timeline**, co-located under the endpoint that owns the timeline, plus one
endpoint_storage **per endpoint**. The PS+SK pair is provisioned at
`endpoint create` — `tenant create` and `timeline branch` only record the
timeline's init recipe (ids, branch name, pending init) in the storage
controller's `pending_timeline_inits` table in YB; the PS+SK processes come
up under `endpoints/<endpoint>/{pageserver,safekeeper}` when the first
endpoint for that timeline is created, which reads the recipe back and then
deletes the pending row. The endpoint_storage instance comes up under
`endpoints/<endpoint>/endpoint_storage` for **every** endpoint (siblings
share the PS+SK pair but each gets its own endpoint_storage); the compute
talks to its own instance.

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
  storage_controller --yb-config "{ host_ports = '127.0.0.1:5433', user = 'yugabyte', dbname = 'yb_storage' }" --listen 127.0.0.1:<port>
  yb-pageserver   (config from on-disk pageserver.toml; yb_light_local defaults :64100 pg, :9988 http, :51151 grpc — base ports 64000/9898/51051 + i offset)
  safekeeper --id 1 --wal-yb-config <YbConnConfig TOML>   (yb_light_local defaults :5474 pg, :7686 http — base ports 5454/7676 + 20 offset)
  endpoint_storage   (config from endpoints/<name>/endpoint_storage/endpoint_storage.json; one per endpoint, base port 9993 + 2*k)
  compute_ctl --pgdata .yb_light/endpoints/main/pgdata --connstr postgresql://cloud_admin@127.0.0.1:55440/postgres
    +-- postgres (v17.5 + Neon patches)  :55440
```

A production deployment would have M pageservers sharded by tenant and a
YB cluster that's actually distributed. **The safekeeper count stays at 1**
when `--wal-yb-config` is set — YB already provides the quorum durability
that a multi-safekeeper Raft would. Running two protocols (YB's Raft + the
safekeeper's Paxos) on top of each other is rejected at boot and at
runtime; see [`safekeeper.md`](safekeeper.md) §5.

### PS/SK co-location is NOT an invariant

The dev cluster above co-locates each timeline's pageserver and safekeeper
on one host, same build, provisioned together — but that is a dev-cluster
convenience, **not** something the design or the wire protocol may assume. A
pageserver and the safekeeper it streams WAL from can run on **different
nodes** and at **different versions** (e.g. during a rolling upgrade). This
is most visible with **read endpoints**: a read-only / hot-standby replica
can be served by a *separate* pageserver — potentially on another node and a
different build — that streams interpreted WAL from a safekeeper elsewhere.
A given safekeeper may also fan its WAL out to more than one such
pageserver. (The current `yb_light_local` launcher happens to point a read
endpoint at the primary's pair via `127.0.0.1`, but nothing downstream may
rely on that.)

Consequence: the **SK→PS interpreted-WAL wire format must stay
version-portable** — no encoding fast-path may assume the two ends are the
same build or the same host. There is currently no version/capability
handshake on that link beyond the `protocol=` format selector, so any
leaner-but-version-sensitive framing (see #69) must be negotiated, with the
portable protobuf encoding remaining the default/fallback.
