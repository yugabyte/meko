# yb-amp architecture

`yb-amp` is a PostgreSQL storage/compute engine that keeps every
durable byte in [YugabyteDB](https://github.com/yugabyte/yugabyte-db).
The compute is stateless PostgreSQL; the pageserver and safekeeper are
stateless gateways that persist pages, WAL, and control state as YB
rows — durability, replication, and fault tolerance come from YB's own
storage layer.

These docs describe what the system looks like end-to-end. Each
subsystem has its own page.

## Contents

| File | Scope |
|---|---|
| [`high-level.md`](high-level.md) | Cluster topology, components, end-to-end data flows |
| [`compute.md`](compute.md) | The user-facing Postgres + smgr hooks |
| [`compute_ctl.md`](compute_ctl.md) | The per-endpoint supervisor: spec, lifecycle, control API |
| [`safekeeper.md`](safekeeper.md) | Single-SK WAL durability, control file, WAL storage |
| [`safekeeper-lsns.md`](safekeeper-lsns.md) | SK-side LSN inventory + flows |
| [`pageserver.md`](pageserver.md) | The pageserver: row-per-page store, materialiser, and write path |
| [`pageserver-lsns.md`](pageserver-lsns.md) | PS-side LSN inventory + flows |
| [`pageserver-gc.md`](pageserver-gc.md) | PageServer garbage collection: candidate queue, drain, cutoff, config, metrics |
| [`remote-storage.md`](remote-storage.md) | The `RemoteStorage` abstraction + `YbStore` |
| [`yugabyte.md`](yugabyte.md) | What lives in YB: schemas, tables, connection paths, the retry contract |
| [`single-container-endpoint.md`](single-container-endpoint.md) | Packaging an endpoint as one pod (`deploy/`): the supervisor, bootstrap |

> **Control plane docs live outside `compute/`.** These pages cover the
> storage/compute engine only. The **amp controller** (the Go control plane,
> `amp-controller` / `ac`) — which owns cluster control: generation authority,
> the node/branch/endpoint registries, and endpoint provisioning — is documented
> under [`../../controller/architecture/`](../../controller/architecture/), with its
> own subproject guide in [`../../controller/AGENTS.md`](../../controller/AGENTS.md). It
> deploys to Kubernetes via the `yb-amp-controller` Helm chart (`deploy/`), which
> also ships its MCP-server pod.

### HTTP APIs & metrics (per service)

| Service | HTTP API | Metrics |
|---|---|---|
| Pageserver | [`pageserver-apis.md`](pageserver-apis.md) | [`pageserver-metrics.md`](pageserver-metrics.md) |
| Safekeeper | [`safekeeper-apis.md`](safekeeper-apis.md) | [`safekeeper-metrics.md`](safekeeper-metrics.md) |
| compute_ctl | [`compute_ctl-apis.md`](compute_ctl-apis.md) | [`compute_ctl-metrics.md`](compute_ctl-metrics.md) |
| Endpoint storage | [`endpoint-storage-apis.md`](endpoint-storage-apis.md) | [`endpoint-storage-metrics.md`](endpoint-storage-metrics.md) |

## Quick mental model

```
                     +---------------------------+
                     |  Compute (Postgres)    |   user-facing PG;
                     |     NEON_SMGR + LFC       |   stateless
                     +---------------------------+
                        |               |
                   WAL  |               |  getpage
                  push  v               v
              +----------------+   +----------------+
              |   Safekeeper   |   |   Pageserver   |
              | (1 per branch) |-->| (1 per branch) |
              +----------------+   +----------------+
                        \                 |
                         \                |  YsqlClient (cold paths)
                          \               |  yb_tsclient (hot paths, :9100)
                           \              |
                            v             v
              +---------------------------------------------------+
              |            YugabyteDB (db: yb_storage)            |
              |   schema  public      (blobs + blob_namespace)    |
              |   schema  pageserver  (pages, slru_pages, meta,   |
              |                        timeline_state)            |
              |   schema  safekeeper  (timeline_state, wals)      |
              +---------------------------------------------------+
```

What's on local FS: the compute's `pgdata` and operational files
(configs, logs, pidfiles). Everything that has to *survive a wipe of
that disk* lives in YB. The authoritative data-plane schema is
[`controller/migrations/0002_compute_storage.up.sql`](../../controller/migrations/0002_compute_storage.up.sql)
(registries in
[`0003_registry.up.sql`](../../controller/migrations/0003_registry.up.sql)).
