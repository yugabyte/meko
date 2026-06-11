# yb-light architecture

`yb-light` is a fork of [Neon](https://github.com/neondatabase/neon) that pushes
every Neon-internal durable byte into [YugabyteDB](https://github.com/yugabyte/yugabyte-db)
instead of an external object store and a local filesystem. The Postgres
compute layer is untouched; the storage layer underneath it is the one that
changed.

These docs describe what the system looks like end-to-end. Each subsystem has
its own page with two sections: **inherited from Neon** (so a reader who knows
upstream Neon can skim) and **changes in yb-light**.

## Contents

| File | Scope |
|---|---|
| [`high-level.md`](high-level.md) | Cluster topology, components, end-to-end data flows |
| [`compute.md`](compute.md) | The user-facing Postgres + Neon smgr hooks |
| [`compute_ctl.md`](compute_ctl.md) | The per-endpoint supervisor: spec, lifecycle, control API |
| [`safekeeper.md`](safekeeper.md) | Single-SK WAL durability, control file, WAL storage |
| [`safekeeper-lsns.md`](safekeeper-lsns.md) | SK-side LSN inventory + flows |
| [`pageserver.md`](pageserver.md) | Inherited Neon pageserver design |
| [`yb-pageserver.md`](yb-pageserver.md) | yb-light pageserver fork: page-store, materialiser, write path |
| [`pageserver-lsns.md`](pageserver-lsns.md) | PS-side LSN inventory + flows |
| [`storage-controller.md`](storage-controller.md) | Cluster control plane (relational state) |
| [`remote-storage.md`](remote-storage.md) | The `RemoteStorage` abstraction + `YbStore` |
| [`yugabyte.md`](yugabyte.md) | What lives in YB: databases, schemas, tables |

## Quick mental model

```
                     +---------------------------+
                     |  Compute (Postgres 17.5)  |   user-facing PG;
                     |     NEON_SMGR + LFC       |   unchanged in yb-light
                     +---------------------------+
                        |               |
                   WAL  |               |  getpage RPC
                  push  v               v
              +----------------+   +----------------+
              |   Safekeeper   |   |   Pageserver   |
              |   (single)     |-->|  (per-tenant)  |
              +----------------+   +----------------+
                        \                 |
                         \                |  YsqlClient
                          \               |  (tokio_postgres)
                           \              |
                            v             v
              +-----------------------------------------+
              |          YugabyteDB (db: yb_storage)    |
              |   schema  storage_controller (7 tables) |
              |   schema  pageserver (5 tables)         |
              |   schema  safekeeper (timeline_state)   |
              |   schema  public (blobs, wals)          |
              +-----------------------------------------+
```

What's on local FS in yb-light: the pageserver's layer-file cache, the
compute's `pgdata`, and operational files (logs, pidfiles). Everything
that has to *survive a wipe of that disk* lives in YB. Authoritative
schema is `storage_controller/migrations/00000000000001_initial_schema/up.sql`.
