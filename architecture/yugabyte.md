# YugabyteDB as the durable layer

This page describes what the YB cluster looks like from yb-neon's
perspective: databases, tables, prefixes, and the DDL the operator is
expected to provision before pointing yb-neon at YB.

## Databases

| Database | Owner | Purpose | Provisioned by |
|---|---|---|---|
| `yb_storage` | yugabyte | All blob storage for the pageserver and safekeeper. Holds `blobs` (chunked binary objects) and `wals` (per-write WAL chunks). | operator |
| `storage_controller` | yugabyte | The storage controller's 11 relational tables (Diesel-managed). | the controller binary on first start (runs migrations) |

The `storage_controller` DB is created externally (e.g. `CREATE DATABASE
storage_controller`); migrations populate the tables.
`yb_storage` and its two tables are entirely operator-provisioned —
yb-neon never runs DDL against them.

## `yb_storage.blobs`

Generic chunked blob store used by:

- The pageserver (`remote_storage` config: `prefix = 'pageserver-yb/'`).
- The safekeeper for control-file persistence (`prefix = 'safekeeper-yb/safekeeper-control/...'`).
- The safekeeper for sealed-segment WAL backup (`prefix = 'safekeeper-yb/.../<XLogFileName>'`).

Subsystems are segregated by **key prefix**, not by separate tables.

```sql
CREATE TABLE blobs (
    key              TEXT    NOT NULL,
    chunk_idx        INT     NOT NULL,
    data             BYTEA   NOT NULL,
    size_bytes       BIGINT  NOT NULL,    -- total object size, denormalized
    etag             TEXT    NOT NULL,    -- total-object etag, denormalized
    last_modified_us BIGINT  NOT NULL,
    metadata_json    TEXT,
    PRIMARY KEY ((key) HASH, chunk_idx)
);
```

Schema documented in detail in [`remote-storage.md`](remote-storage.md).

### Typical content (yb-neon dev cluster after light usage)

```
key prefix          | distinct objects | typical row count | bytes
--------------------+------------------+-------------------+-------
pageserver-yb/      |              ~30 | 1-3 chunks each   |  ~600 MB
safekeeper-yb/      |              ~30 | 1-2 chunks each   |  ~300 MB
```

(`safekeeper-yb/` contains: 1 row for the control file, plus N rows for the
sealed WAL segments that wal_backup has uploaded.)

### Useful diagnostic queries

```sql
-- Per-subsystem breakdown by key prefix
SELECT split_part(key, '/', 1) AS prefix,
       count(DISTINCT key)      AS objects,
       count(*)                 AS chunks,
       pg_size_pretty(sum(length(data))::bigint) AS stored
FROM blobs
GROUP BY 1
ORDER BY 1;

-- Largest objects
SELECT key, count(*) AS chunks, pg_size_pretty(max(size_bytes)) AS total
FROM blobs
GROUP BY key
ORDER BY max(size_bytes) DESC
LIMIT 10;
```

## `yb_storage.wals`

Per-write log used by the safekeeper's live-WAL mirror (when
`--wal-yb-config` is set). Schema is *different* from `blobs` — keyed by
`(tenant, timeline, segno, chunk_seq)` and ordered by `chunk_seq` for
last-write-wins reassembly.

```sql
CREATE TABLE wals (
    tenant_id        TEXT    NOT NULL,
    timeline_id      TEXT    NOT NULL,
    segno            BIGINT  NOT NULL,
    chunk_seq        INT     NOT NULL,
    chunk_offset     BIGINT  NOT NULL,
    chunk_size       INT     NOT NULL,
    data             BYTEA   NOT NULL,
    last_modified_us BIGINT  NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id) HASH, segno, chunk_seq)
);
```

Schema documented in detail in [`safekeeper.md`](safekeeper.md).

### Useful diagnostic queries

```sql
-- Per-segment summary for one timeline
SELECT segno, count(*) AS chunks,
       max(chunk_seq) AS max_seq,
       max(chunk_offset + chunk_size) AS end_offset,
       pg_size_pretty(sum(chunk_size)::bigint) AS bytes
FROM wals
WHERE tenant_id = '...' AND timeline_id = '...'
GROUP BY segno
ORDER BY segno;
```

## `storage_controller`

Diesel-managed schema; do **not** edit by hand. Live migrations are at
`storage_controller/migrations/`. The controller will run any pending
migration on startup. Inspect with:

```sql
\c storage_controller
\dt
```

Tables documented in [`storage-controller.md`](storage-controller.md).

## YB-side configuration that matters

### `rpc_max_message_size` (default 256 MiB)

The single most-bitten-by setting. YB's RPC layer rejects messages larger
than this. We work around it by:

- **Blobs**: chunking objects into ≤ 100 MiB rows. The single-row
  approach failed the first time we shipped a 290 MiB pageserver layer.
- **WALs**: each row is one `write_wal()` call, which is bounded by the
  WAL framing the compute sends (typically a few KiB to a few MiB). Far
  below the limit.

### `yb_enable_upsert_mode`

A per-session GUC we `SET` on every yb-neon connection (in `YbStore::new`
and in `WalBackup::new` for the live-WAL client). When `true`, plain
`INSERT` skips DocDB's read-before-write conflict check — a meaningful
speedup for the upsert-heavy workload our blob store creates.

It's harmless against vanilla Postgres (unknown GUC → SET fails → we log
and continue), so the same code path works in mixed environments.

### `yb_enable_read_committed_isolation = true`

Set on the dev cluster (`/tmp/yb-bench`). Required for the storage
controller's transactional updates to behave as expected.

## Provisioning DDL — full reference

The yb-neon system expects these to already exist when its binaries start.
A one-time setup script:

```sql
-- on a fresh YB cluster
CREATE DATABASE yb_storage;
CREATE DATABASE storage_controller;   -- controller will run migrations itself

\c yb_storage

CREATE TABLE blobs (
    key              TEXT    NOT NULL,
    chunk_idx        INT     NOT NULL,
    data             BYTEA   NOT NULL,
    size_bytes       BIGINT  NOT NULL,
    etag             TEXT    NOT NULL,
    last_modified_us BIGINT  NOT NULL,
    metadata_json    TEXT,
    PRIMARY KEY ((key) HASH, chunk_idx)
);

CREATE TABLE wals (
    tenant_id        TEXT    NOT NULL,
    timeline_id      TEXT    NOT NULL,
    segno            BIGINT  NOT NULL,
    chunk_seq        INT     NOT NULL,
    chunk_offset     BIGINT  NOT NULL,
    chunk_size       INT     NOT NULL,
    data             BYTEA   NOT NULL,
    last_modified_us BIGINT  NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id) HASH, segno, chunk_seq)
);
```

## Failure modes

- **YB unavailable on pageserver upload**: upload returns an error; the
  pageserver retries on the next `flush`. Local layer file stays on disk
  until upload succeeds.
- **YB unavailable on safekeeper write_wal** (yb-only mode): `write_wal()`
  returns an error. The walproposer doesn't ACK to the compute → compute's
  commit blocks. Once YB recovers, retries succeed.
- **YB unavailable on storage controller**: the controller's HTTP API
  returns 5xx; pageserver / safekeeper / compute keep operating until they
  need a fresh cluster decision.
- **YB unavailable for WalReader read**: only matters when the local WAL
  cache is also missing for the requested segment. In normal yb-neon
  operation the local cache exists; in the YB-only safekeeper mode this
  *is* fatal until YB recovers.

## Hosts / ports in the dev cluster

- YB master RPC: `127.0.0.1:7100`
- YB tserver YSQL: `127.0.0.1:5433` (the only port yb-neon talks to)
- YB tserver web UI: `127.0.0.1:9000`
- `yugabyted-ui`: `127.0.0.1:15433`

Started via `~/Downloads/yugabyte/yugabyte-2025.2.3.0/bin/yugabyted start
--base_dir=/tmp/yb-bench --advertise_address=127.0.0.1`.
