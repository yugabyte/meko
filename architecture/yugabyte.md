# YugabyteDB as the durable layer

This page describes what the YB cluster looks like from yb-light's
perspective: databases, tables, prefixes, and the DDL the operator is
expected to provision before pointing yb-light at YB.

## Databases

All three subsystems share **one** database, `yb_storage`. Within it:

| Schema | Tables | Owner | Provisioned by |
|---|---|---|---|
| `public` | `blobs`, `wals`, `blob_namespace` ENUM | yugabyte | `neon_local` (auto) |
| `pageserver` | `pages`, `timelines` (used by `yb-pageserver`) | yugabyte | `neon_local` (auto) |
| `storage_controller` | 11 Diesel-managed tables (`tenant_shards`, `nodes`, `timelines`, …) | yugabyte | `neon_local` creates the schema; the controller binary runs migrations on first start |

`neon_local` auto-provisions everything on first start: it connects to
YB's `yugabyte` admin DB with the same credentials as the controller's
`database_url`, `CREATE DATABASE yb_storage`, then inside that DB
creates the `public.blob_namespace` ENUM + `public.blobs` /
`public.wals` tables (schema-qualified DDL so they can't drift into
`storage_controller` via search_path). The controller's URL is
amended on its side with `options=-c search_path=storage_controller,public`
so its migrations land in the `storage_controller` schema. All
idempotent — re-runs are no-ops. No operator DDL is required on a
fresh YB cluster.

## `yb_storage.blobs`

Generic chunked blob store used by:

- The pageserver (`remote_storage` config: `prefix = 'pageserver-yb/'`).
- The safekeeper for control-file persistence (`prefix = 'safekeeper-yb/safekeeper-control/...'`).
- The safekeeper for sealed-segment WAL backup (`prefix = 'safekeeper-yb/.../<XLogFileName>'`).

Subsystems are segregated by **key prefix**, not by separate tables.

```sql
CREATE TYPE public.blob_namespace AS ENUM ('pageserver', 'safekeeper');

CREATE TABLE public.blobs (
    tenant_id        TEXT                  NOT NULL,    -- Neon tenant UUID, or "" if not extractable
    timeline_id      TEXT                  NOT NULL,    -- Neon timeline UUID, or "" for tenant-scoped blobs
    namespace        public.blob_namespace NOT NULL,    -- 'pageserver' or 'safekeeper'
    key              TEXT                  NOT NULL,    -- discriminating filename, generation suffix stripped
    generation       INT,                               -- pageserver attach gen; NULL for genless files
    chunk_idx        INT                   NOT NULL,
    data             BYTEA                 NOT NULL,
    size_bytes       BIGINT                NOT NULL,    -- total object size, denormalized
    etag             TEXT                  NOT NULL,    -- total-object etag, denormalized
    last_modified_us BIGINT                NOT NULL,
    metadata_json    TEXT,
    PRIMARY KEY ((tenant_id, timeline_id, key) HASH, namespace, chunk_idx)
);
```

The four-way split (`namespace` + `tenant_id` + `timeline_id` + `key`) is
populated at write time by `YbStore::parse_key`:

- **`namespace`** is the typed `blob_namespace` ENUM. Today only
  `pageserver` and `safekeeper`. Derived from the configured `prefix`:
  if it contains `safekeeper` → `safekeeper`; else → `pageserver`.
- **`tenant_id`** and **`timeline_id`** are the first two Neon-id-shaped
  tokens in the path (32 hex chars, optionally with a `-<count>-<num>`
  shard suffix). Empty strings when absent.
- **`key`** is the **discriminating filename only** — the path with
  the configured prefix stripped *and* with `tenants`, `timelines`,
  the tenant_id, and the timeline_id removed. `split_key_generation`
  then peels the trailing or pre-extension `-<8hex>` generation marker
  into the separate `generation` column. `YbStore::reconstruct_key` +
  `rejoin_key_generation` rebuild the original `RemotePath` for
  `list` outputs.
- **`generation`** is the pageserver attach generation, parsed off the
  path. `NULL` for genless files (heatmap, initdb archives). Matched
  explicitly on read (`generation IS NOT DISTINCT FROM $N`) so callers
  get gen-specific lookups.

A row in this schema looks like:

| namespace | tenant_id | timeline_id | key | generation |
|---|---|---|---|---|
| `pageserver` | `c8178f5e…1f966a` | `77ffde89…089eaa` | `000000-FFF__layer-1` | `1` |
| `pageserver` | `c8178f5e…1f966a` | `77ffde89…089eaa` | `index_part.json` | `1` |
| `pageserver` | `c8178f5e…1f966a` | *(empty)* | `tenant-manifest.json` | `1` |
| `pageserver` | `c8178f5e…1f966a` | *(empty)* | `heatmap-v1.json` | *(NULL)* |
| `safekeeper` | `c8178f5e…1f966a` | `77ffde89…089eaa` | `safekeeper-control` | *(NULL)* |
| `safekeeper` | `c8178f5e…1f966a` | `77ffde89…089eaa` | `000000010000000000000005` | *(NULL)* |

The PK puts `(tenant_id, timeline_id, key)` in the hash partition so
every key has its own hash tablet (no hot tablet on big tenants).
`key = $x` lookups resolve to one tablet via hash; within a bucket
rows are sorted by `(namespace, chunk_idx)`. Generation lives in a
plain column — multiple gens of the same logical file collapse into
one row by design (newer gens overwrite older; ACID isolation
prevents split-brain).

Schema documented in detail in [`remote-storage.md`](remote-storage.md).

### Typical content (yb-light dev cluster after light usage)

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
-- Per-subsystem breakdown
SELECT namespace,
       count(DISTINCT (tenant_id, timeline_id, key)) AS objects,
       count(*)                                       AS chunks,
       pg_size_pretty(sum(length(data))::bigint)      AS stored
FROM blobs
GROUP BY namespace
ORDER BY namespace;

-- Per-tenant breakdown (fast — hits the (tenant_id, timeline_id) hash partition)
SELECT tenant_id,
       timeline_id,
       namespace,
       count(DISTINCT key) AS objects,
       count(*)            AS chunks,
       pg_size_pretty(sum(length(data))::bigint) AS stored
FROM blobs
GROUP BY tenant_id, timeline_id, namespace
ORDER BY sum(length(data)) DESC;

-- Largest objects
SELECT tenant_id, timeline_id, namespace, key,
       count(*) AS chunks, pg_size_pretty(max(size_bytes)) AS total
FROM blobs
GROUP BY tenant_id, timeline_id, namespace, key
ORDER BY max(size_bytes) DESC
LIMIT 10;
```

## `yb_storage.wals`

Per-write log used by the safekeeper's live-WAL mirror (when
`--wal-yb-config` is set). Schema is *different* from `blobs` — keyed by
`(tenant, timeline, segno, chunk_seq)` and ordered by `chunk_seq` for
last-write-wins reassembly.

```sql
CREATE TABLE public.wals (
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

## `pageserver` schema

Used by `yb-pageserver` (a fork of the upstream `pageserver` that
replaces the layer-file machinery with row-per-version storage). The
upstream `pageserver` does NOT touch this schema — it uses `blobs`
only. Provisioned alongside `public.blobs` / `public.wals` by
`neon_local` on first start.

### `pageserver.pages`

One row per `(Key, LSN)`. The 10-delta image rule (enforced at write
time by `yb-pageserver`'s `ImageRule`) guarantees that any chain of
≤ 10 rows for a given key contains at least one `will_init=true` entry,
so the read query plans as a single bounded range scan.

```sql
CREATE TABLE pageserver.pages (
    tenant_id    TEXT    NOT NULL,
    timeline_id  TEXT    NOT NULL,
    key          BYTEA   NOT NULL,      -- 18-byte pageserver_api::key::Key
    lsn          BIGINT  NOT NULL,      -- u64 stored as i64 (real LSNs << 2^63)
    is_image     BOOLEAN NOT NULL,
    will_init    BOOLEAN NOT NULL,
    payload      BYTEA   NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id, key) HASH, lsn DESC)
);
```

Read query (one round trip, hash to one tablet, scan ≤ 10 rows):

```sql
SELECT lsn, is_image, will_init, payload
FROM pageserver.pages
WHERE tenant_id = $1 AND timeline_id = $2 AND key = $3 AND lsn <= $4
ORDER BY lsn DESC
LIMIT 10;
```

### `pageserver.timelines`

One row per timeline. Replaces the upstream split of `metadata`,
`index_part.json`, and `tenant-manifest.json` with a single
transactionally-updatable record. `current_generation` is the gate that
fences a stale pageserver — every flush is wrapped in a transaction
that conditionally updates this row.

```sql
CREATE TABLE pageserver.timelines (
    tenant_id              TEXT    NOT NULL,
    timeline_id            TEXT    NOT NULL,
    last_record_lsn        BIGINT  NOT NULL,
    disk_consistent_lsn    BIGINT  NOT NULL,
    applied_gc_cutoff_lsn  BIGINT  NOT NULL,
    ancestor_timeline_id   TEXT,
    ancestor_lsn           BIGINT,
    current_generation     INT     NOT NULL,
    pg_version             INT     NOT NULL,
    PRIMARY KEY ((tenant_id) HASH, timeline_id)
);
```

See [`yb-pageserver.md`](yb-pageserver.md) for the design, the 10-delta
rule, and the implementation roadmap.

## `storage_controller` schema

Diesel-managed schema living inside `yb_storage`; do **not** edit by
hand. Live migrations are at `storage_controller/migrations/`. The
controller binary amends its connection URL with
`options=-c search_path=storage_controller,public` so unqualified DDL
in the migrations lands in this schema rather than `public`. A
bootstrap step (`Persistence::migration_run`) issues
`CREATE SCHEMA IF NOT EXISTS storage_controller` on a raw connection
before applying migrations, so the very first migration has a target.

Inspect with:

```sql
\c yb_storage
SET search_path = storage_controller, public;
\dt
```

Tables documented in [`storage-controller.md`](storage-controller.md).

## YB-side configuration that matters

### `rpc_max_message_size` (default 256 MiB)

The single most-bitten-by setting. YB's RPC layer rejects messages larger
than this. We work around it by:

- **Blobs**: chunking objects into ≤ 100 MiB rows. The single-row
  approach failed the first time we shipped a 290 MiB pageserver layer.
- **WALs**: each row is one chunk of one `write_wal()` call, which is
  bounded by the WAL framing the compute sends (typically a few KiB to
  a few MiB). The safekeeper's consumer task batches up to 50 chunks
  or 4 MiB into one `INSERT VALUES` statement — well below the limit.

### `yb_enable_upsert_mode`

A per-session GUC we `SET` on every yb-light connection (centrally in
`YsqlClient::open_*` — used by both `YbStore::new` and
`WalBackup::new`). When `true`, plain `INSERT` skips DocDB's
read-before-write conflict check — a meaningful speedup for the
upsert-heavy workload our blob store creates.

It's also `SET` on every reconnect, so transient connection drops
don't leave subsequent inserts paying the read-before-write cost.

It's harmless against vanilla Postgres (unknown GUC → SET fails → we log
and continue), so the same code path works in mixed environments.

### `yb_enable_read_committed_isolation = true`

Set on the dev cluster (`/tmp/yb-bench`). Required for the storage
controller's transactional updates to behave as expected.

## Provisioning DDL — full reference

`neon_local` runs this DDL itself on first start (see
`control_plane/src/storage_controller.rs::ensure_external_database`).
Listed here for reference / disaster-recovery; you do **not** need to
run any of it by hand.

```sql
-- on a fresh YB cluster (auto-run by neon_local)
CREATE DATABASE yb_storage;
\c yb_storage

-- The controller's tables live in a dedicated schema inside the
-- same DB; the bootstrap step in `Persistence::migration_run` issues
-- the CREATE SCHEMA before Diesel migrations run.
CREATE SCHEMA IF NOT EXISTS storage_controller;

CREATE TYPE public.blob_namespace AS ENUM ('pageserver', 'safekeeper');

CREATE TABLE public.blobs (
    tenant_id        TEXT                  NOT NULL,
    timeline_id      TEXT                  NOT NULL,
    namespace        public.blob_namespace NOT NULL,
    key              TEXT                  NOT NULL,
    generation       INT,
    chunk_idx        INT                   NOT NULL,
    data             BYTEA                 NOT NULL,
    size_bytes       BIGINT                NOT NULL,
    etag             TEXT                  NOT NULL,
    last_modified_us BIGINT                NOT NULL,
    metadata_json    TEXT,
    PRIMARY KEY ((tenant_id, timeline_id, key) HASH, namespace, chunk_idx)
);

CREATE TABLE public.wals (
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

CREATE SCHEMA IF NOT EXISTS pageserver;

CREATE TABLE pageserver.pages (
    tenant_id    TEXT    NOT NULL,
    timeline_id  TEXT    NOT NULL,
    key          BYTEA   NOT NULL,
    lsn          BIGINT  NOT NULL,
    is_image     BOOLEAN NOT NULL,
    will_init    BOOLEAN NOT NULL,
    payload      BYTEA   NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id, key) HASH, lsn DESC)
);

CREATE TABLE pageserver.timelines (
    tenant_id              TEXT    NOT NULL,
    timeline_id            TEXT    NOT NULL,
    last_record_lsn        BIGINT  NOT NULL,
    disk_consistent_lsn    BIGINT  NOT NULL,
    applied_gc_cutoff_lsn  BIGINT  NOT NULL,
    ancestor_timeline_id   TEXT,
    ancestor_lsn           BIGINT,
    current_generation     INT     NOT NULL,
    pg_version             INT     NOT NULL,
    PRIMARY KEY ((tenant_id) HASH, timeline_id)
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
  cache is also missing for the requested segment. In normal yb-light
  operation the local cache exists; in the YB-only safekeeper mode this
  *is* fatal until YB recovers.

## Hosts / ports in the dev cluster

- YB master RPC: `127.0.0.1:7100`
- YB tserver YSQL: `127.0.0.1:5433` (the only port yb-light talks to)
- YB tserver web UI: `127.0.0.1:9000`
- `yugabyted-ui`: `127.0.0.1:15433`

Started via `~/Downloads/yugabyte/yugabyte-2025.2.3.0/bin/yugabyted start
--base_dir=/tmp/yb-bench --advertise_address=127.0.0.1`.
