# High-level architecture

## System overview

Durability and resilience live in exactly one place: **YugabyteDB**.
The pageserver and safekeeper are stateless gateways that persist
everything they own as YB rows; the compute is stateless Postgres; the
controller keeps its registries in YB too. Any daemon can be killed and
restarted on another host without data loss — recovery is "reconnect to
YB and read your state back".

```mermaid
flowchart LR
    subgraph clients["Postgres clients"]
        app["app / psql"]
    end

    subgraph compute["Compute (patched PostgreSQL, stateless)"]
        pg["postgres + yb_amp smgr"]
        wp["walproposer"]
    end

    subgraph sk["Safekeeper (stateless)"]
        skp["WAL ingress<br/>(walproposer push)"]
        sks["interpreted-WAL sender"]
    end

    subgraph ps["Pageserver (stateless)"]
        cps["compute_page_store<br/>(reads + chain reconstruct)"]
        fps["fpu_page_store<br/>(walreceiver ingest)"]
        mat["image materialiser"]
    end

    subgraph sc["Controller (stateless)"]
        sct["generation fencing<br/>node/branch/endpoint registries"]
    end

    subgraph yb["YugabyteDB cluster (stateful)"]
        direction LR
        tblPages[("pageserver.pages<br/>pageserver.slru_pages<br/>pageserver.meta")]
        tblState[("pageserver.timeline_state<br/>safekeeper.timeline_state")]
        tblWal[("safekeeper.wals")]
        tblBlob[("public.blobs<br/>initdb archives, LFC scratch")]
        tblSc[("registry + controller schemas")]
    end

    app -- "SQL: SELECT / INSERT / UPDATE" --> pg
    pg -- "GetPage @ LSN" --> cps
    wp -- "WAL stream<br/>(START_WAL_PUSH)" --> skp
    skp -- "batched INSERTs<br/>(WAL chunks)" --> tblWal
    fps -- "START_REPLICATION<br/>(walreceiver)" --> sks
    sks -- "interpreted WAL" --> fps
    cps -- "chain reads" --> tblPages
    fps -- "page-row writes" --> tblPages
    mat -- "compact long chains<br/>(materialised images)" --> tblPages
    cps -- "gen-fenced flush" --> tblState
    sct -- "attach + generation" --> cps
    sct -- "state R/W" --> tblSc
    cps -- "initdb tar" --> tblBlob
    pg -- "basebackup fetch" --> cps
```

A yb-amp install is:

- **Compute nodes**: patched PostgreSQL, stateless, configured
  against a pageserver endpoint and a safekeeper. The `yb_amp` smgr
  extension routes every `smgr_read` to a pageserver GetPage request
  and every WAL flush to the safekeeper via the walproposer.
- **Pageserver (`yb-pageserver`)**: the row-store gateway. Two YSQL
  connection pools:
  - `compute_page_store`: serves compute reads, running image-anchored
    LSN-chain reconstruction over `pageserver.pages` /
    `pageserver.slru_pages` / `pageserver.meta`.
  - `fpu_page_store`: the walreceiver ingestion path + image
    materialiser writes. Image materialisation runs as a background
    task that compacts long delta chains into walredo-applied images so
    subsequent reads don't re-walk the chain.
  With `hot_path = "tsclient"` (the default) the page read/write hot
  paths run over the direct-to-tserver RPC client instead of YSQL; the
  YSQL pools carry the cold paths.
- **Safekeeper (`yb-safekeeper`)**: stateless WAL ingress. Receives
  the WAL stream from the compute, batches it into
  `safekeeper.wals` rows, and ACKs a commit only once its bytes are
  durable in YB. No local WAL, no quorum, no leader election —
  **exactly one safekeeper per branch**. YB's own replication provides
  the quorum durability, so stacking a second consensus protocol on
  top of it would add latency and failure modes without adding safety;
  configurations that try are rejected at boot and at runtime (see
  [`safekeeper.md`](safekeeper.md) §5).
- **Controller** (`controller`, Go): the control plane — generation
  authority (attach-hook fencing), the node/branch/endpoint
  registries, endpoint provisioning, and the product API. Its state
  lives in the same YB database (`registry` + `controller` schemas),
  and it applies all schema migrations on startup. Documented under
  [`../../controller/architecture/`](../../controller/architecture/).
- **Endpoint storage (`yb-endpoint-storage`)**: a small per-endpoint
  HTTP service for endpoint-scoped scratch blobs (LFC prewarm state),
  backed by `public.blobs`.
- **YugabyteDB cluster**: the durable storage tier for everything
  above.

> **YB query contract**: every SQL statement issued through
> `YsqlClient` must be idempotent. The client retries connection drops
> internally and may replay an in-flight write whose commit status is
> unknown. Use `INSERT ... ON CONFLICT` (or upsert mode), absolute
> updates, and generation-fenced writes — never bare INSERTs or
> relative `SET col = col + 1`. See
> [`yugabyte.md`](yugabyte.md#client-side-retry-contract--every-yb-query-must-be-idempotent)
> for the full contract.

## Components

| Component | Role | Process binary |
|---|---|---|
| Compute | User-facing Postgres backend. Speaks the Postgres wire protocol to clients. | `pg_install/v18/bin/postgres` (with smgr patches) |
| `compute_ctl` | Supervisor for the compute: applies the spec, starts/stops PG, runs catalog migrations. | `target/debug/yb-compute-ctl` |
| Safekeeper | WAL durability gateway: receives WAL from the compute, persists it to `safekeeper.wals`, streams interpreted WAL to the pageserver. **One instance per branch** — YB provides the replication. | `target/debug/yb-safekeeper` |
| Pageserver | Materializes pages from WAL into `pageserver.*` rows and serves them to the compute on demand. | `target/debug/yb-pageserver` |
| Controller | Control plane: generation fencing, node/branch/endpoint registries, endpoint provisioning, schema migrations. | `controller` (Go, `controller/cmd/controller`) |
| Endpoint storage | Per-endpoint scratch-blob service (LFC prewarm state). One instance **per endpoint**. | `target/debug/yb-endpoint-storage` |
| YugabyteDB | Durable storage backend for everything yb-amp-internal. | external (`yugabyted`) |

## Write path (one INSERT from a client)

```
client
  |  SQL
  v
Compute (PG backend)
  |  WAL record buffer-flushed by walwriter / commit
  v
walproposer  ────────────►  the branch's Safekeeper
                              |  write_wal()  → chunks onto the consumer queue
                              |  consumer     → batched INSERT into safekeeper.wals
                              |                 (≤ 50 chunks / 4 MiB per batch,
                              |                  one placement bucket per batch)
                              |  durable watermark advances when the batch lands
                              v
                            ACK to walproposer  (compute returns commit to client)
```

Commit latency is bounded by the YB write round-trip for the batch
containing the commit record (~ms against local YB; more over a real
network). That round-trip is the headline cost of YB-backed WAL — and
the ingest loop never blocks on it: the producer keeps queueing the
next batch while the previous one is in flight, and ACKs ride the
durability watermark.

In parallel, the pageserver's walreceiver streams interpreted WAL from
the safekeeper (`START_REPLICATION`) and writes page rows into
`pageserver.pages` / `slru_pages` / `meta`; its flushes advance
`pageserver.timeline_state.consistent_lsn` under a generation fence.

## Read path (a page miss in the compute)

```
client
  |  SELECT
  v
Compute backend --(miss)--> shared_buffers --(miss)--> LFC --(miss)-->
                                                                |
                                                        GetPage@LSN request
                                                                v
                                                          Pageserver
                                                                |
                                                  image-anchored chain read on
                                                  pageserver.pages (one anchor
                                                  seek + range scan above it)
                                                                |
                                          one image row?  ──yes──►  return page bytes
                                                  |
                                                  no (image + deltas)
                                                  v
                                          walredo applies the delta chain
                                                  v
                                          return page bytes to compute
```

There is no per-page local cache on the pageserver: every miss in the
compute's own caches is answered from YB rows. The image materialiser
keeps chains short so the common case is a single image row (no
walredo). See [`pageserver.md`](pageserver.md).

## What's durable where

| State | Where | Key / shape |
|---|---|---|
| Relation pages | YB `pageserver.pages` | one row per (block, LSN); images + deltas |
| SLRU blocks | YB `pageserver.slru_pages` | one row per (block, LSN) |
| Metadata blobs (relsizes, dir listings, aux files, …) | YB `pageserver.meta` | latest row ≤ LSN wins |
| Per-branch pageserver state (LSNs, generation, ancestry) | YB `pageserver.timeline_state` | one generation-fenced row per branch |
| Safekeeper control file | YB `safekeeper.timeline_state` | one row per branch, typed columns |
| Live WAL | YB `safekeeper.wals` | `((branch_id, bucket_id) HASH, segno, chunk_offset)` |
| initdb archives | YB `public.blobs` | `branches/<branch_id>/initdb.tar.zst` split across the namespace/branch_id/key columns, ≤ 100 MiB chunks |
| Endpoint scratch (LFC prewarm) | YB `public.blobs` | written via endpoint_storage's HTTP API |
| Controller registries | YB `registry` schema | branches, timelines, pending inits, endpoints |
| Controller product model | YB `controller` schema | users, pools, projects, grants, branches, endpoints, tasks |
| Compute `pgdata` + runtime | local FS | `.yb_amp/endpoints/<endpoint>/` — runtime working state only (`pgdata/` catalogs + ephemeral WAL pre-ship buffer, logs, pidfile, `config.json`); created on `endpoint create`, removed on destroy |
| Pageserver / safekeeper local FS | local FS | `.yb_amp/endpoints/<endpoint>/{pageserver,safekeeper}/` — configs (`pageserver.toml`, `metadata.json`, `safekeeper.toml`), logs, pidfiles. No pages, no WAL, no control file. |

Everything in the local-FS rows is reconstructible; everything that
must survive a disk wipe is a YB row.

## Topology in the dev cluster (`yb_amp_local`)

The launcher is `yb_amp_local`
([`controller/local/cmd/yb_amp_local`](../../controller/local/cmd/yb_amp_local)),
a Go CLI that materializes a `.yb_amp/` repo (`init`) and drives the
local environment (`start`, `branch`, `endpoint`, `pageserver`,
`safekeeper`, `endpoint_storage`, `controller` subcommands). It targets
an external YugabyteDB (`controller.yb` config block) — external YB is
the only supported path — and `CREATE DATABASE`s `yb_storage` before
starting the controller, which then applies every schema migration.

Topology: single host, single YB instance, one controller, one
pageserver + one safekeeper **per branch**, co-located under the
endpoint that owns the branch, plus one endpoint_storage **per
endpoint**. The PS+SK pair is provisioned at `endpoint create` —
`branch create` only records the branch's init recipe (branch id,
branch name, pg version, optional ancestor + start LSN) in the
controller's `registry.pending_timeline_inits` table; the pair comes up
under `endpoints/<endpoint>/{pageserver,safekeeper}` when the first
endpoint for that branch is created, which reads the recipe back and
deletes the pending row. Sibling endpoints on the same branch share the
PS+SK pair but each gets its own endpoint_storage.

```
yugabyted                        :7100 (master), :5433 (ysql), :9100 (tserver RPC)
  +-- db `yb_storage`
        +-- schema `public`      blobs (chunked BYTEA) + blob_namespace ENUM
        +-- schema `pageserver`  pages, slru_pages, meta, timeline_state
        +-- schema `safekeeper`  timeline_state (control-file rows)
        |                        + wals (WAL chunks, 10 tablets)
        +-- schema `registry`    branches, nodes, timelines,
        |                        pending_timeline_inits, endpoints
        +-- schema `controller`  product model + task framework

yb-amp processes
  controller --dev --database-url postgresql://yugabyte@127.0.0.1:5433/yb_storage \
             --listen 127.0.0.1:1234           (control_plane_api http://127.0.0.1:1234/upcall/v1/)
  yb-pageserver      (endpoints/<ep>/pageserver/pageserver.toml;
                      defaults :64000 pg, :9898 http, :51051 grpc — +2 per extra node)
  yb-safekeeper      (endpoints/<ep>/safekeeper/safekeeper.toml;
                      defaults :5454 pg, :7676 http — +2 per extra node)
  yb-endpoint-storage (endpoints/<ep>/endpoint_storage/endpoint_storage.json;
                      one per endpoint, ports 9993 + 2k)
  yb-compute-ctl --pgdata .yb_amp/endpoints/main/pgdata ...
    +-- postgres (+ yb_amp patches)      ports allocated from 55432 up
```

(`yb_amp_local init --port-base N` moves all the port lanes off their
defaults for parallel environments.)

A production deployment has M pageservers partitioned by branch (one
branch per pageserver — the pageserver does not shard a branch; YB
tablet-shards the keyspace underneath) and a YB cluster that is
actually distributed. The safekeeper count per branch stays at 1 — YB
provides the quorum durability that a safekeeper group would otherwise
exist to provide, and running two consensus protocols stacked on each
other is rejected at boot and at runtime (see
[`safekeeper.md`](safekeeper.md) §5).

### PS/SK co-location is NOT an invariant

The dev cluster co-locates each branch's pageserver and safekeeper on
one host, same build, provisioned together — but that is a dev-cluster
convenience, **not** something the design or the wire protocol may
assume. A pageserver and the safekeeper it streams WAL from can run on
**different nodes** and at **different versions** (e.g. during a
rolling upgrade). This is most visible with **read endpoints**: a
read-only / hot-standby replica can be served by a *separate*
pageserver — potentially on another node and a different build — that
streams interpreted WAL from a safekeeper elsewhere. A given safekeeper
may also fan its WAL out to more than one such pageserver. (The current
`yb_amp_local` launcher happens to point a read endpoint at the
primary's pair via `127.0.0.1`, but nothing downstream may rely on
that.)

Consequence: the **SK→PS interpreted-WAL wire format must stay
version-portable** — no encoding fast-path may assume the two ends are
the same build or the same host. There is currently no
version/capability handshake on that link beyond the `protocol=` format
selector, so any leaner-but-version-sensitive framing (see #69) must be
negotiated, with the portable protobuf encoding remaining the
default/fallback.

### The hot path needs direct tserver RPC access

With `hot_path = "tsclient"` (the default), the safekeeper (WAL
write/read on `safekeeper.wals`) and the pageserver (page read/write on
`pages`/`slru_pages`/`meta`) speak YB's native pggate RPC **straight to
the yb-tserver RPC port** (9100), not through the YSQL frontend (5433).

**Deployment invariant** — every host running a safekeeper or
pageserver must reach the RPC port of the tservers it targets: the
endpoints in `tserver_rpc_addrs`, or the `host_ports` hosts on 9100
when that is unset. (Tablet routing is server-side, so the client needs
only those configured endpoints, not every tserver.) A firewall /
security group / network policy that opens only 5433 (YSQL) and 7100
(master) lets the control plane and cold paths keep working while
breaking the hot path — it surfaces as `hot_path = "tsclient"` startup
health-check failures (each daemon opens its table handles at boot) or
terminal getpage/WAL errors. Keep 9100 (and any ports named in
`tserver_rpc_addrs`) reachable from every SK/PS host. See
[`yugabyte.md`](yugabyte.md) §"Direct-to-tserver hot path".
