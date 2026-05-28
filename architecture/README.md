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
| [`safekeeper.md`](safekeeper.md) | WAL Raft cluster, control file, WAL storage |
| [`pageserver.md`](pageserver.md) | Layer files, getpage, remote_storage |
| [`storage-controller.md`](storage-controller.md) | Cluster control plane (relational state) |
| [`remote-storage.md`](remote-storage.md) | The `RemoteStorage` abstraction + `YbStore` |
| [`yugabyte.md`](yugabyte.md) | What lives in YB: databases, tables, schemas |

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
              |                |-->|  (per-tenant)  |
              +----------------+   +----------------+
                        \                 |
                         \                |
                          \               |  remote_storage
                           \              |  (RemoteStorage trait)
                            v             v
              +-----------------------------------------+
              |              YugabyteDB                 |
              |  db: yb_storage         db: storage_    |
              |    table: blobs           controller    |
              |    table: wals            (11 tables)   |
              +-----------------------------------------+
```

What's on local FS in yb-light (when YB-backed): the pageserver's layer-file
cache, the compute's `pgdata`, and operational files (logs, pidfiles).
Everything that has to *survive a wipe of that disk* is in YB.
