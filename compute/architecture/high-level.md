# High-level architecture

## Components

| Component | Role | Process binary |
|---|---|---|
| Compute | User-facing Postgres backend. Speaks Postgres wire protocol to clients. | `pg_install/v18/bin/postgres` (with Neon smgr patches) |
| `compute_ctl` | Supervisor for the compute: applies config, starts/stops PG, runs catalog migrations. | `target/debug/yb-compute-ctl` |
| Safekeeper | Raft acceptor for WAL. Receives WAL from compute, durably stores it, streams to pageserver. **Limited to one instance per cluster in YB WAL mode** — YB's own Raft already provides quorum durability. | `target/debug/yb-safekeeper` |
| Pageserver | Materializes pages from WAL and serves them to compute on demand. Holds the only durable copy of relfiles (via `remote_storage`). | `target/debug/yb-pageserver` |
| Controller | Tracks cluster topology: which branches are on which pageservers, which safekeepers hold which branches, generations, etc. | `target/debug/controller` |
| Endpoint storage | Small auxiliary service for endpoint-scoped scratch data (LFC pre-warm hints, etc.). One instance **per endpoint**, co-located under `endpoints/<name>/endpoint_storage`. | `target/debug/yb-endpoint-storage` |
| YugabyteDB | Durable storage backend for everything yb-amp-internal. Replaces S3/Azure/GCS *and* the controller's local Postgres. | external (`yugabyted`) |

## Write path (one INSERT from a client)

```
client
  |  SQL
  v
Compute (PG backend)
  |  WAL record buffer-flushed by walwriter / commit
  v
walproposer  ────────────►  single Safekeeper
                              |  write_wal()  →  INSERT into safekeeper.wals
                              |                  (synchronous, one row per call)
                              |  flush_wal()  → mark flush_lsn (YB is durable)
                              v
                            ACK to walproposer  (compute can return commit to client)
```

The compute's commit latency is bounded by the safekeeper's `flush_wal()`,
which on yb-amp's YB-backed safekeeper is a YB INSERT round-trip (~ms over
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
disk. See [`yb-pageserver.md`](yb-pageserver.md) for details.

## What's durable where

(The `Path / key` column below describes the **logical** RemoteStorage
key the pageserver/safekeeper use. In YB the path is split across the
`namespace` / `branch_id` / `key` columns — see
[`yugabyte.md`](yugabyte.md). The mapping is round-trip lossless.)

| Subsystem state | yb-amp backend | Path / key |
|---|---|---|
| Pageserver layer files | YB `yb_storage.blobs` | `pageserver-yb/branches/<branch_id>/<layer>` (one BYTEA chunk per row, ≤ 100 MiB each); PK `((branch_id, key) HASH, namespace, chunk_idx)` |
| Pageserver `index_part.json` | YB `yb_storage.blobs` | `pageserver-yb/branches/<branch_id>/index_part.json-<gen>` |
| Pageserver `initdb.tar.zst` | YB `yb_storage.blobs` | `pageserver-yb/branches/<branch_id>/initdb.tar.zst` |
| Pageserver branch manifest | YB `yb_storage.blobs` | `pageserver-yb/branches/<branch_id>/tenant-manifest-<gen>.json` |
| Safekeeper control file | YB `safekeeper.timeline_state` schema | one row per branch, typed columns |
| Safekeeper live WAL | YB `safekeeper.wals` | `((branch_id, bucket_id) HASH, segno, chunk_offset)` |
| Controller relational state | YB `yb_storage` DB, `registry` schema | 8 Diesel-managed tables (alongside pageserver/safekeeper/public schemas) |
| Compute endpoint registry | YB `yb_storage` DB, `registry.endpoints` | one row per endpoint: endpoint_id, branch_id + opaque `EndpointConf` JSON (`spec`). Replaces the old per-endpoint `endpoint.json` folder |
| Pageserver local FS | local FS | `.yb_amp/endpoints/<endpoint>/pageserver/` — `pageserver.toml`, `identity.toml`, `metadata.json`, log + pidfile (no layer files; pages live in YB) |
| Safekeeper local FS | local FS | `.yb_amp/endpoints/<endpoint>/safekeeper/` — only logs + pidfile (no WAL, no control file, no id) |
| Compute `pgdata` + runtime | local FS | `.yb_amp/endpoints/<endpoint>/` — runtime working state only (`pgdata/` catalogs + ephemeral WAL pre-ship buffer, logs, pidfile, `config.json`), plus the branch's co-located `pageserver/` + `safekeeper/`; created on `endpoint create`, removed on destroy |

## What did *not* change from upstream Neon

These subsystems run unmodified upstream Neon code in yb-amp:

- The Postgres compute itself (`pg_install/v18/bin/postgres` with Neon smgr
  patches from `compute/vendor/postgres-v18`).
- `compute_ctl` and the compute spec. (The endpoint *registry* — what
  `yb_amp_local` persists per endpoint — moved out of `endpoint.json` folders
  into the `registry.endpoints` table; `compute_ctl` itself is
  unchanged.)
- The pageserver's `RemoteStorage` *trait* — we added `YbStore` as the
  primary backend (with `Unreliable` retained for tests); the upstream
  S3, Azure, GCS, and LocalFs variants have been removed, and yb-pageserver
  rejects non-YbStore configs.
- The pageserver's getpage RPC, WAL receiver, layer-file format, compaction,
  GC, and branch attach logic.
- The safekeeper's Raft acceptor logic, WAL stream decoder, walproposer wire
  protocol.
- The controller's Rust code: nearly untouched when we pointed it
  at YB. The only addition is a `--yb-config` flag that takes the shared
  `YbConnConfig` block and builds the DB connection URL internally via
  `YbConnConfig::to_database_url()`; everything else (diesel, migrations,
  queries) runs unchanged against YB's Postgres-compatible YSQL frontend.

## Topology in our current dev cluster

Single host, single YB instance, one pageserver + one safekeeper **per
branch**, co-located under the endpoint that owns the branch, plus one
endpoint_storage **per endpoint**. The PS+SK pair is provisioned at
`endpoint create` — `branch create` only records the
branch's init recipe (branch id, branch name, pending init) in the storage
controller's `pending_timeline_inits` table in YB; the PS+SK processes come
up under `endpoints/<endpoint>/{pageserver,safekeeper}` when the first
endpoint for that branch is created, which reads the recipe back and then
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
        |                              PK ((branch_id, key) HASH,
        |                                  namespace, chunk_idx)
        |                              key column holds the bare filename — the
        |                              branches/ literal + the branch id value are
        |                              not stored, they live in their own columns
        +-- schema `controller`  Diesel-managed; 7 tables
        +-- schema `pageserver`
        +-- schema `safekeeper`          `timeline_state` (typed control-file rows)
                                         + `wals` (per-write WAL chunks,
                                         PK ((branch_id) HASH, segno, chunk_offset))

yb-amp binaries (target/debug/)
  controller --yb-config "{ host_ports = '127.0.0.1:5433', user = 'yugabyte', dbname = 'yb_storage' }" --listen 127.0.0.1:<port>
  yb-pageserver   (config from on-disk pageserver.toml; yb_amp_local defaults :64100 pg, :9988 http, :51151 grpc — base ports 64000/9898/51051 + i offset)
  yb-safekeeper --id 1   (config from on-disk safekeeper.toml: yb_storage_conn; yb_amp_local defaults :5474 pg, :7686 http — base ports 5454/7676 + 20 offset)
  yb-endpoint-storage   (config from endpoints/<name>/compute/endpoint_storage/endpoint_storage.json; one per endpoint, base port 9993 + 2*k)
  yb-compute-ctl --pgdata .yb_amp/endpoints/main/pgdata --connstr postgresql://cloud_admin@127.0.0.1:55440/postgres
    +-- postgres (18.2 + Neon/yb_amp patches)  :55440
```

A production deployment would have M pageservers partitioned by branch
(one branch per pageserver — yb-amp has no sharding) and a YB cluster
that's actually distributed. **The safekeeper count stays at 1**
when `yb_storage_conn` is configured — YB already provides the quorum durability
that a multi-safekeeper Raft would. Running two protocols (YB's Raft + the
safekeeper's Paxos) on top of each other is rejected at boot and at
runtime; see [`safekeeper.md`](safekeeper.md) §5.

### PS/SK co-location is NOT an invariant

The dev cluster above co-locates each branch's pageserver and safekeeper
on one host, same build, provisioned together — but that is a dev-cluster
convenience, **not** something the design or the wire protocol may assume. A
pageserver and the safekeeper it streams WAL from can run on **different
nodes** and at **different versions** (e.g. during a rolling upgrade). This
is most visible with **read endpoints**: a read-only / hot-standby replica
can be served by a *separate* pageserver — potentially on another node and a
different build — that streams interpreted WAL from a safekeeper elsewhere.
A given safekeeper may also fan its WAL out to more than one such
pageserver. (The current `yb_amp_local` launcher happens to point a read
endpoint at the primary's pair via `127.0.0.1`, but nothing downstream may
rely on that.)

Consequence: the **SK→PS interpreted-WAL wire format must stay
version-portable** — no encoding fast-path may assume the two ends are the
same build or the same host. There is currently no version/capability
handshake on that link beyond the `protocol=` format selector, so any
leaner-but-version-sensitive framing (see #69) must be negotiated, with the
portable protobuf encoding remaining the default/fallback.

### The hot path needs direct tserver RPC access

With `hot_path = "tsclient"` (the default), the safekeeper (WAL
write/read on `safekeeper.wals`) and the pageserver (page read/write on
`pages`/`slru_pages`/`meta`) speak YB's native pggate RPC **straight to
the yb-tserver RPC port** (9100), not through the YSQL frontend (5433).
The topology diagram above lists only `:7100`/`:5433` because that is
what `yugabyted` advertises, but the tserver RPC port is just as
load-bearing.

**Deployment invariant** — every host running a safekeeper or pageserver
must reach the RPC port of the tservers it targets: the endpoints in
`tserver_rpc_addrs`, or the `host_ports` hosts on 9100 when that is
unset. (Tablet routing is server-side, so the client needs only those
configured endpoints, not every tserver.) A firewall / security group /
network policy that opens only 5433 (YSQL) and 7100 (master) lets the
control plane and cold paths keep working while breaking the hot path —
it surfaces as `hot_path = "tsclient"` startup health-check failures
(each daemon opens its table handles at boot) or terminal getpage/WAL
errors. Keep 9100 (and any ports named in `tserver_rpc_addrs`) reachable
from every SK/PS host. See [`yugabyte.md`](yugabyte.md)
§"Direct-to-tserver hot path".
