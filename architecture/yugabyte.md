# YugabyteDB as the durable layer

This page describes what the YB cluster looks like from yb-light's
perspective: databases, tables, prefixes, and the DDL the operator is
expected to provision before pointing yb-light at YB.

## Databases

All three subsystems share **one** database, `yb_storage`. Within it:

| Schema | Tables | Owner | Provisioned by |
|---|---|---|---|
| `public` | `blobs`, `wals`, `blob_namespace` ENUM | yugabyte | controller migrations |
| `pageserver` | `pages`, `slru_pages`, `meta`, `tenant_config`, `timeline_state` | yugabyte | controller migrations |
| `safekeeper` | `timeline_state` | yugabyte | controller migrations |
| `storage_controller` | 7 Diesel-managed tables (`tenants`, `nodes`, `safekeepers`, `metadata_health`, `controllers`, `timelines`, `timeline_imports`) | yugabyte | controller migrations |

`neon_local` only `CREATE DATABASE`s `yb_storage`
(`control_plane/src/storage_controller.rs::ensure_external_database`);
every schema, ENUM, and table — across `public`, `pageserver`,
`safekeeper`, and `storage_controller` — is created by the storage
controller binary's Diesel migrations on first start. The single
authoritative migration is
`storage_controller/migrations/00000000000001_initial_schema/up.sql`,
which `CREATE SCHEMA IF NOT EXISTS`'s all four schemas and creates
every table referenced below. The controller amends its connection
URL with `options=-c search_path=storage_controller,public` so
unqualified DDL lands in `storage_controller`; cross-schema DDL is
written schema-qualified. All idempotent — re-runs are no-ops. No
operator DDL is required on a fresh YB cluster.

## `yb_storage.blobs`

Generic chunked blob store used by:

- The pageserver (`remote_storage` config: `prefix = 'pageserver-yb/'`).

The safekeeper has no consumer of `blobs` today — control-file state
moved to `safekeeper.timeline_state` (one row per timeline) and live
WAL goes to `public.wals`. Subsystems are segregated by **key
prefix**, not by separate tables.

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

The `safekeeper` ENUM variant is reserved but currently unused —
safekeeper state lives in `safekeeper.timeline_state` and live WAL in
`public.wals`.

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
```

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
`(tenant, timeline, segno, chunk_offset)`. Each row is one chunk of
one `write_wal()` call; the writer never produces overlapping chunks
(`truncate_wal` aligns the next write at exactly the truncation
point), so `chunk_offset` alone identifies a chunk within a segment
and a same-PK INSERT is an idempotent retry of the same bytes.

```sql
CREATE TABLE public.wals (
    tenant_id        TEXT    NOT NULL,
    timeline_id      TEXT    NOT NULL,
    segno            BIGINT  NOT NULL,
    chunk_offset     BIGINT  NOT NULL,
    chunk_size       INT     NOT NULL,
    data             BYTEA   NOT NULL,
    last_modified_us BIGINT  NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id) HASH, segno, chunk_offset)
);
```

Schema documented in detail in [`safekeeper.md`](safekeeper.md).

### Useful diagnostic queries

```sql
-- Per-segment summary for one timeline
SELECT segno, count(*) AS chunks,
       max(chunk_offset + chunk_size) AS end_offset,
       pg_size_pretty(sum(chunk_size)::bigint) AS bytes
FROM wals
WHERE tenant_id = '...' AND timeline_id = '...'
GROUP BY segno
ORDER BY segno;
```

## `pageserver` schema

Holds the page store that replaces the upstream layer-file machinery
with row-per-version storage; see
[`yb-pageserver.md`](yb-pageserver.md) for the design. Provisioned
alongside `public.blobs` / `public.wals` by `neon_local` on first
start.

### `pageserver.pages`

One row per `(block, LSN)`. Holds **relation pages only** —
`Key::is_rel_block_key()` (`field1 == 0x00`, `field4 != 0`,
`field6 != 0xffffffff`). Non-page keys (SLRUs, metadata, dir listings,
aux files, repl origins, rel-size sentinels) continue through
`public.blobs`. Hash partition by `(tenant, timeline, spcnode, dbnode,
relnode, forknum)` so each fork's whole block-set lives on one tablet.
Clustering by `(blknum, lsn DESC, is_image)` means a per-block image
lookup is a single seek, not a scan.

```sql
CREATE TABLE pageserver.pages (
    tenant_id    TEXT     NOT NULL,
    timeline_id  TEXT     NOT NULL,
    spcnode      BIGINT   NOT NULL,
    dbnode       BIGINT   NOT NULL,
    relnode      BIGINT   NOT NULL,
    forknum      SMALLINT NOT NULL,
    blknum       BIGINT   NOT NULL,
    is_image     BOOLEAN  NOT NULL,
    lsn          BIGINT   NOT NULL,    -- u64 stored as i64 (real LSNs << 2^63)
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id, spcnode, dbnode, relnode, forknum) HASH,
                 blknum, lsn DESC, is_image)
);
```

Read query — image-anchored CTE (fast path: anchor seek + small range scan
above the anchor):

```sql
WITH anchor AS (
    SELECT lsn AS anchor_lsn FROM pageserver.pages
    WHERE tenant_id=$1 AND timeline_id=$2
      AND spcnode=$3 AND dbnode=$4 AND relnode=$5
      AND forknum=$6 AND blknum=$7
      AND is_image=TRUE AND lsn <= $8
    ORDER BY lsn DESC LIMIT 1
)
SELECT lsn, is_image, will_init, payload
FROM pageserver.pages p, anchor
WHERE p.tenant_id=$1 AND p.timeline_id=$2
  AND p.spcnode=$3 AND p.dbnode=$4 AND p.relnode=$5
  AND p.forknum=$6 AND p.blknum=$7
  AND p.lsn >= anchor.anchor_lsn AND p.lsn <= $8
ORDER BY p.lsn DESC;
```

If no image exists for the block (`anchor` CTE empty), `read_chain`
falls back to a plain LSN-descending scan and walks rows until it hits
a `will_init=true` delta. This is the cold path; a background image
materialiser ([Phase 3e](yb-pageserver.md#background-image-materialiser-planned))
keeps it rare.

### `pageserver.timeline_state`

One row per timeline. Replaces the upstream split of `metadata`,
`index_part.json`, and `tenant-manifest.json` with a single
transactionally-updatable record. `current_generation` is the gate
that fences a stale pageserver — every flush is wrapped in a
transaction that conditionally updates this row. The `lineage` JSONB
is a forward-looking slot for ancestor-detach state (always `Default`
in yb-mode today).

```sql
CREATE TABLE pageserver.timeline_state (
    tenant_id              TEXT      NOT NULL,
    timeline_id            TEXT      NOT NULL,
    consistent_lsn         BIGINT    NOT NULL,
    applied_gc_cutoff_lsn  BIGINT    NOT NULL,
    ancestor_timeline_id   TEXT,
    ancestor_lsn           BIGINT,
    current_generation     INT       NOT NULL,
    pg_version             INT       NOT NULL,
    initdb_lsn             BIGINT,
    prev_record_lsn        BIGINT,
    archived_at            TIMESTAMP,
    deleted_at             TIMESTAMP,
    rel_size_migration     TEXT,
    rel_size_migrated_at   BIGINT,
    lineage                JSONB,
    PRIMARY KEY ((tenant_id) HASH, timeline_id)
);
```

See [`yb-pageserver.md`](yb-pageserver.md) for the design, the
image-anchored read path, and the implementation roadmap.

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
`YsqlClient::from_yb_config` / `from_yb_config_pooled` — used by
`YbStore::new`, `PageStore`, and `YbWalStorage::new`). When `true`,
plain `INSERT` skips DocDB's
read-before-write conflict check — a meaningful speedup for the
upsert-heavy workload our blob store creates.

It's also `SET` on every reconnect, so transient connection drops
don't leave subsequent inserts paying the read-before-write cost.

It's harmless against vanilla Postgres (unknown GUC → SET fails → we log
and continue), so the same code path works in mixed environments.

### `yb_enable_read_committed_isolation = true`

Set on the dev cluster. Required for the storage
controller's transactional updates to behave as expected.

## Client-side retry contract — every YB query must be idempotent

`YsqlClient::run_with_retries` (the wrapper every component goes
through — `PageStore`, `YbStore`, safekeeper WAL backup,
storage_controller metadata DB) retries three classes of failure
internally before surfacing anything to the caller:

- **Connection drops**: TCP socket closed mid-statement (YB tserver
  restart, YBA rolling upgrade, network blip). Reconnect, re-prepare,
  replay. Up to 5 reconnects per call.
- **SQLSTATE-confirmed transients**: `40001` serialization failure,
  `40P01` deadlock detected, `25P03` idle-in-txn timeout. Statement is
  guaranteed by the server not to have committed — replay is safe even
  for non-idempotent SQL.
- **YB `XX000` allowlist**: tablet leader change during async write,
  tablet peer shutting down (split/move/restart), schema version
  mismatch, queue-deadline overload, `WaitForAsyncWrite RPC` timeout,
  `Restart read required`. Documented as transient by YB.

Total budget per call: 10 attempts, 50 ms initial backoff doubling
to 1 s cap with 0–25% jitter. Surfacing only happens once the budget
is fully spent.

**The catch**: the connection-drop arm reconnects without any way to
know whether the pre-drop statement actually committed before the
socket died. The reconnect arm replays anyway. **This is only safe if
the statement is idempotent — re-executing it must produce the same
end state whether the first attempt applied, half-applied, or did
nothing.**

Every YB-talking call site in this codebase satisfies that
contract today:

- **All reads** — `SELECT`, `query_opt`, `query_raw` — are
  intrinsically idempotent.
- **All writes** are one of: `INSERT ... ON CONFLICT (...) DO NOTHING`
  / `DO UPDATE` (`pageserver.pages`, `slru_pages`, `meta`, `wals`,
  `yb_storage.blobs` chunks), plain `INSERT` running under
  [`yb_enable_upsert_mode = true`](#yb_enable_upsert_mode) (same
  semantics — the conflict path is a no-op upsert), or
  generation-fenced `UPDATE pageserver.timeline_state SET ... WHERE
  current_generation = $expected_gen` (replay sees the same gen,
  applies the same row state).
- **DELETEs** are `WHERE` -keyed deletes (`DELETE FROM ... WHERE id =
  $1`); replay against an already-deleted row is a no-op.

**New code touching YB must keep this invariant.** Concrete rules for
contributors:

- Don't write a bare `INSERT` that needs to fail on a duplicate row.
  Add `ON CONFLICT DO NOTHING` (or `DO UPDATE ... RETURNING` if you
  need to detect the apply / replay distinction).
- Don't issue an `UPDATE foo SET counter = counter + 1`. Replay
  double-counts. Make the update absolute (`SET counter = $new`)
  with whatever read-modify-write happens in the application.
- Don't depend on a `SELECT ... FOR UPDATE` lock surviving a retry —
  reconnect drops the implicit txn.
- If you genuinely need a non-idempotent write through YsqlClient,
  add a new code path that doesn't retry `ConnectionDrop` (the
  Transient + Fatal branches stay safe — those are server-confirmed
  no-ops). Right now nothing needs it.

The two Prometheus counters
`yb_client_errors_total{op,class,sqlstate}` and
`yb_client_retries_exhausted_total{reason}` (exposed on both
pageserver and safekeeper `/metrics`) let you observe the retry mix
in production. A sudden spike in `class="transient"` is usually a
tablet event; a sustained rate of `retries_exhausted{reason="..."}` is
the signal that something is wrong upstream of the client.

## Provisioning DDL — full reference

The authoritative provisioning DDL is a single Diesel migration:
[`storage_controller/migrations/00000000000001_initial_schema/up.sql`](../storage_controller/migrations/00000000000001_initial_schema/up.sql).
`neon_local` only `CREATE DATABASE`s `yb_storage` (see
`control_plane/src/storage_controller.rs::ensure_external_database`);
the controller binary applies the migration on first start, which
creates every schema, ENUM, and table in one shot. Listed here for
reference / disaster-recovery; you do **not** need to run any of it
by hand.

| Schema | Object | Purpose |
|---|---|---|
| `public` | `blob_namespace` ENUM | `pageserver` / `safekeeper` discriminator on `blobs`. |
| `public` | `blobs` | Chunked blob store (pageserver remote_storage). |
| `public` | `wals` | Live-WAL chunks; PK `((tenant, timeline) HASH, segno, chunk_offset)`. |
| `pageserver` | `pages` | Relation pages, one row per (block, LSN); PK `((tenant, timeline, spc, db, rel, fork) HASH, blknum, lsn DESC, is_image)`. |
| `pageserver` | `slru_pages` | SLRU blocks (pg_xact, pg_multixact_*); same shape as `pages`, keyed by `(kind, segno)`. |
| `pageserver` | `meta` | Metadata blobs (relsize sentinels, dir listings, aux, two-phase, repl origins); full-overwrite semantics. |
| `pageserver` | `tenant_config` | Per-tenant `TenantConfig` (GC horizon, PITR, …); authoritative copy, controller-written. |
| `pageserver` | `timeline_state` | Per-timeline state (replaces `metadata` + `index_part.json` + `tenant-manifest.json`); generation-fenced via `current_generation`. |
| `safekeeper` | `timeline_state` | Per-timeline safekeeper control file (replaces upstream's local-FS `safekeeper.control` blob). |
| `storage_controller` | 7 tables | `tenants`, `nodes`, `safekeepers`, `metadata_health`, `controllers`, `timelines`, `timeline_imports`. |

See `up.sql` for the exact column lists, PK ordering, and FK
relationships.

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

Started via `~/Downloads/yugabyte/yugabyte-2025.2.3.0/bin/yugabyted start --base_dir=~/yb-storage --advertise_address=127.0.0.1`.
