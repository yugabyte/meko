# YugabyteDB as the durable layer

This page describes what the YB cluster looks like from yb-light's
perspective: databases, tables, prefixes, and the DDL the operator is
expected to provision before pointing yb-light at YB.

## Databases

All three subsystems share **one** database, `yb_storage`. Within it:

| Schema | Tables | Owner | Provisioned by |
|---|---|---|---|
| `public` | `blobs`, `blob_namespace` ENUM | yugabyte | controller migrations |
| `pageserver` | `pages`, `slru_pages`, `meta`, `timeline_state` | yugabyte | controller migrations |
| `safekeeper` | `timeline_state`, `wals` | yugabyte | controller migrations |
| `storage_controller` | 8 Diesel-managed tables (`branches`, `nodes`, `safekeepers`, `metadata_health`, `timelines`, `pending_timeline_inits`, `timeline_imports`, `endpoints`) | yugabyte | controller migrations |

`yb_light_local` only `CREATE DATABASE`s `yb_storage`
(`control_plane/src/storage_controller.rs::ensure_external_database`);
every schema, ENUM, and table — across `public`, `pageserver`,
`safekeeper`, and `storage_controller` — is created by the storage
controller binary's Diesel migrations on first start. The single
authoritative migration is
`storage_controller/migrations/00000000000001_initial_schema/up.sql`,
which `CREATE SCHEMA IF NOT EXISTS`'s the `storage_controller`,
`pageserver`, and `safekeeper` schemas (`public` is built-in) and
creates every table referenced below. The controller's connections use
`search_path = storage_controller,public` (`add_search_path_to_url`), so
unqualified DDL — and Diesel's `__diesel_schema_migrations` bookkeeping —
resolve to `storage_controller`; cross-schema objects are written
schema-qualified.

The migration SQL is **not** itself idempotent (plain `CREATE TABLE`); it
is Diesel's applied-migration tracking that makes each migration run
exactly once and be skipped on restart. For that tracking to be reliable
the bookkeeping table must always live in the same schema, so
`Persistence::migration_run` runs `CREATE SCHEMA IF NOT EXISTS
storage_controller` *before* applying migrations. Without that, on the
very first start the schema doesn't exist yet, so the unqualified
`__diesel_schema_migrations` lands in `public`; on later starts it
resolves to `storage_controller` — a different, empty table — and Diesel
re-runs every migration against already-existing objects (which then
fails). No operator DDL is required on a fresh YB cluster.

## `yb_storage.blobs`

Generic chunked blob store used by:

- The pageserver. Its `yb_storage_conn` block is the shared `YbConnConfig`
  (`host_ports` / `user` / `dbname` / …) — there is no configured `prefix`
  or `table_name`; the table (`public.blobs`) and the namespace are
  hardcoded / derived, not configured.

The safekeeper has no consumer of `blobs` today — control-file state
moved to `safekeeper.timeline_state` (one row per branch) and live
WAL goes to `safekeeper.wals`. Subsystems are segregated by the **`namespace`
column** (derived from the blob's key/path), not by separate tables.

```sql
CREATE TYPE public.blob_namespace AS ENUM ('pageserver', 'safekeeper');

CREATE TABLE public.blobs (
    branch_id        TEXT                  NOT NULL,    -- branch id, or "" if not extractable
    namespace        public.blob_namespace NOT NULL,    -- 'pageserver' or 'safekeeper'
    key              TEXT                  NOT NULL,    -- discriminating filename, generation suffix stripped
    generation       INT,                               -- pageserver attach gen; NULL for genless files
    chunk_idx        INT                   NOT NULL,
    data             BYTEA                 NOT NULL,
    size_bytes       BIGINT                NOT NULL,    -- total object size, denormalized
    etag             TEXT                  NOT NULL,    -- total-object etag, denormalized
    last_modified_us BIGINT                NOT NULL,
    metadata_json    TEXT,
    PRIMARY KEY ((branch_id, key) HASH, namespace, chunk_idx)
);
```

The three-way split (`namespace` + `branch_id` + `key`) is
populated at write time by `YbStore::parse_key`:

- **`namespace`** is the typed `blob_namespace` ENUM. Today only
  `pageserver` and `safekeeper`. Derived from the blob's key/path:
  if it contains `safekeeper` → `safekeeper`; else → `pageserver`.
- **`branch_id`** is the branch-id-shaped token in the path (32 hex
  chars, optionally with a `-<count>-<num>` shard suffix). Empty string
  when absent.
- **`key`** is the **discriminating filename only** — the path with
  `branches` and the branch_id removed
  (no configured prefix is involved). `split_key_generation`
  then peels the trailing or pre-extension `-<8hex>` generation marker
  into the separate `generation` column. `YbStore::reconstruct_key` +
  `rejoin_key_generation` rebuild the original `RemotePath` for
  `list` outputs.
- **`generation`** is the pageserver attach generation, parsed off the
  path. `NULL` for genless files (heatmap, initdb archives). Matched
  explicitly on read (`generation IS NOT DISTINCT FROM $N`) so callers
  get gen-specific lookups.

A row in this schema looks like:

| namespace | branch_id | key | generation |
|---|---|---|---|
| `pageserver` | `77ffde89…089eaa` | `000000-FFF__layer-1` | `1` |
| `pageserver` | `77ffde89…089eaa` | `index_part.json` | `1` |
| `pageserver` | `77ffde89…089eaa` | `tenant-manifest.json` | `1` |
| `pageserver` | `77ffde89…089eaa` | `heatmap-v1.json` | *(NULL)* |

The `safekeeper` ENUM variant is reserved but currently unused —
safekeeper state lives in `safekeeper.timeline_state` and live WAL in
`safekeeper.wals`.

The PK puts `(branch_id, key)` in the hash partition so
every key has its own hash tablet (no hot tablet on big branches).
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
       count(DISTINCT (branch_id, key))          AS objects,
       count(*)                                  AS chunks,
       pg_size_pretty(sum(length(data))::bigint) AS stored
FROM blobs
GROUP BY namespace
ORDER BY namespace;

-- Per-branch breakdown (fast — hits the (branch_id, key) hash partition)
SELECT branch_id,
       namespace,
       count(DISTINCT key) AS objects,
       count(*)            AS chunks,
       pg_size_pretty(sum(length(data))::bigint) AS stored
FROM blobs
GROUP BY branch_id, namespace
ORDER BY sum(length(data)) DESC;

-- Largest objects
SELECT branch_id, namespace, key,
       count(*) AS chunks, pg_size_pretty(max(size_bytes)) AS total
FROM blobs
GROUP BY branch_id, namespace, key
ORDER BY max(size_bytes) DESC
LIMIT 10;
```

## `safekeeper.wals`

Per-write log used by the safekeeper's live-WAL mirror (when
`yb_storage_conn` is configured). Schema is *different* from `blobs` — keyed by
`(branch_id, bucket_id, segno, chunk_offset)`. Each row is one coalesced
run of a consumer batch (contiguous chunks concatenated before the
INSERT; the typical batch is one row); the writer never produces
overlapping rows (`truncate_wal` aligns the next write at exactly the
truncation point), so `chunk_offset` alone identifies a row within a
segment and a same-PK INSERT is an idempotent retry of the same bytes.

```sql
CREATE TABLE safekeeper.wals (
    branch_id        TEXT     NOT NULL,
    bucket_id        SMALLINT NOT NULL,  -- per-batch placement bucket, 0..9
    segno            BIGINT   NOT NULL,
    chunk_offset     BIGINT   NOT NULL,
    chunk_size       INT      NOT NULL,
    data             BYTEA    NOT NULL,
    last_modified_us BIGINT   NOT NULL,
    PRIMARY KEY ((branch_id, bucket_id) HASH, segno, chunk_offset)
) SPLIT INTO 10 TABLETS;
```

`bucket_id` exists purely for tablet placement: the `(branch_id,
bucket_id)` hash key spreads a branch's WAL across all
`WALS_BUCKETS = 10` tablets. The consumer stamps each write BATCH with one
bucket, rotating through the buckets one batch at a time — each batch lands
on ONE tablet (single-shard INSERT fast path) while sustained load spreads
across all tablets. Readers can't derive a
chunk's bucket, so index-tight reads name every bucket with a
`bucket_id IN (0..9)` hash-key list — YSQL executes that as batched
per-bucket index probes, not a scan.

Schema documented in detail in [`safekeeper.md`](safekeeper.md).

### Useful diagnostic queries

```sql
-- Per-segment summary for one branch
SELECT segno, count(*) AS chunks,
       max(chunk_offset + chunk_size) AS end_offset,
       pg_size_pretty(sum(chunk_size)::bigint) AS bytes
FROM safekeeper.wals
WHERE branch_id = '...'
GROUP BY segno
ORDER BY segno;
```

## `pageserver` schema

Holds the page store that replaces the upstream layer-file machinery
with row-per-version storage; see
[`yb-pageserver.md`](yb-pageserver.md) for the design. Provisioned
alongside `public.blobs` / `safekeeper.wals` by `yb_light_local` on first
start.

### `pageserver.pages`

One row per `(block, LSN)`. Holds **relation pages only** —
`Key::is_rel_block_key()` (`field1 == 0x00`, `field4 != 0`,
`field6 != 0xffffffff`). Non-page keys (SLRUs, metadata, dir listings,
aux files, repl origins, rel-size sentinels) continue through
`public.blobs`. Hash partition by `(branch_id, spcnode, dbnode,
relnode, forknum)` so each fork's whole block-set lives on one tablet.
Clustering by `(blknum, lsn DESC, is_image DESC)` walks a block's rows
newest-first; `is_image` is DESC so the stored order matches the
chain-read `ORDER BY lsn DESC, is_image DESC` (image before delta at
a tie).

```sql
CREATE TABLE pageserver.pages (
    branch_id    TEXT     NOT NULL,
    spcnode      BIGINT   NOT NULL,
    dbnode       BIGINT   NOT NULL,
    relnode      BIGINT   NOT NULL,
    forknum      SMALLINT NOT NULL,
    blknum       BIGINT   NOT NULL,
    is_image     BOOLEAN  NOT NULL,
    lsn          BIGINT   NOT NULL,    -- u64 stored as i64 (real LSNs << 2^63)
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((branch_id, spcnode, dbnode, relnode, forknum) HASH,
                 blknum, lsn DESC, is_image DESC)
);
```

Read query — image-anchored CTE (fast path: anchor seek + small range scan
above the anchor):

```sql
WITH anchor AS (
    SELECT lsn AS anchor_lsn FROM pageserver.pages
    WHERE branch_id=$1
      AND spcnode=$2 AND dbnode=$3 AND relnode=$4
      AND forknum=$5 AND blknum=$6
      AND is_image=TRUE AND lsn <= $7
    ORDER BY lsn DESC LIMIT 1
)
SELECT lsn, is_image, will_init, payload
FROM pageserver.pages p, anchor
WHERE p.branch_id=$1
  AND p.spcnode=$2 AND p.dbnode=$3 AND p.relnode=$4
  AND p.forknum=$5 AND p.blknum=$6
  AND p.lsn >= anchor.anchor_lsn AND p.lsn <= $7
ORDER BY p.lsn DESC;
```

If no image exists for the block (`anchor` CTE empty), `read_chain`
falls back to a plain LSN-descending scan and walks rows until it hits
a `will_init=true` delta. This is the cold path; a background image
materialiser ([Phase 3e](yb-pageserver.md#background-image-materialiser-planned))
keeps it rare.

### `pageserver.timeline_state`

One row per branch. Replaces the upstream split of `metadata`,
`index_part.json`, and `tenant-manifest.json` with a single
transactionally-updatable record. `current_generation` is the gate
that fences a stale pageserver — every flush is wrapped in a
transaction that conditionally updates this row. The `lineage` JSONB
is a forward-looking slot for ancestor-detach state (always `Default`
in yb-light today).

```sql
CREATE TABLE pageserver.timeline_state (
    branch_id              TEXT      NOT NULL,
    consistent_lsn         BIGINT    NOT NULL,
    applied_gc_cutoff_lsn  BIGINT    NOT NULL,
    ancestor_branch_id     TEXT,
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
    PRIMARY KEY ((branch_id) HASH)
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

Tables documented in [`storage-controller.md`](../../storage_controller/architecture/storage-controller.md).

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

### `yb_disable_transactional_writes`

A second per-session GUC `SET` alongside `yb_enable_upsert_mode` on the
fast-path-upsert (write) connections. When `true`, DocDB drops the
transactional write fast path's read-modify-write guarantees — safe for
our write paths because they're append-only `INSERT`s whose ordering
comes from the higher-level LSN scheme, not YB transaction semantics.

**Off by default.** It is gated by a config field on the TOML connection
block — `disable_transactional_writes` in the `yb_storage_conn` /
`[storage_controller.yb]` inline table
([`YbConnConfig::disable_transactional_writes`]). Omit it (or set
`false`) and the GUC is never sent; set `disable_transactional_writes =
true` to re-enable it.

It defaults to off because of a 500-conn-workload WAL-durability
incident: under tablet rebalancing / RPC retries, batch `INSERT`s
reported as success appeared to lose individual rows, leaving holes in
`safekeeper.wals` that wedge pageserver restart. With the GUC off, those
`INSERT`s take the standard transactional path — slower, but atomic.
Operators can opt back in per-component once the incident is resolved.

### `yb_enable_read_committed_isolation = true`

Set on the dev cluster. Required for the storage
controller's transactional updates to behave as expected.

## Direct-to-tserver hot path (`hot_path = "tsclient"`)

The shared `yb_storage_conn` block accepts two optional keys:

```toml
yb_storage_conn = { host_ports = "...", hot_path = "tsclient", tserver_rpc_addrs = "host:9100,..." }
```

- `hot_path` — `"tsclient"` (default) runs the hot paths over a pure-Rust
  thin RPC client (`compute/libs/yb_tsclient`) speaking the pggate RPC protocol
  directly to a tserver's RPC port (9100), skipping the PG backend (wire
  framing, parse/plan, pggate); `"ysql"` is the Postgres-wire escape hatch.
  The safekeeper honors it for the WAL write/read path on `safekeeper.wals`
  (see `compute/architecture/safekeeper.md` §"Direct-to-tserver hot path")
  and the pageserver for the page read/write hot path on
  `pages`/`slru_pages`/`meta` (see `compute/architecture/yb-pageserver.md`
  §"Direct-to-tserver hot path"). Cold paths
  (DDL, GC, rehydration, control file, `timeline_state`) always use YSQL.
  Old binaries fail to parse a config that sets these keys
  (`deny_unknown_fields`).
- `tserver_rpc_addrs` — yb-tserver RPC endpoints for the `tsclient`
  transport. When unset, the `host_ports` hosts on port 9100; set it
  explicitly to reach every tserver when `host_ports` is only a subset
  (e.g. a single YSQL VIP).

## Client-side retry contract — every YB query must be idempotent

`YsqlClient::run_with_retries` (the wrapper every component goes
through — `PageStore`, `YbStore`, safekeeper WAL backup,
storage_controller metadata schema) retries three classes of failure
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
[`storage_controller/migrations/00000000000001_initial_schema/up.sql`](../../storage_controller/migrations/00000000000001_initial_schema/up.sql).
`yb_light_local` only `CREATE DATABASE`s `yb_storage` (see
`control_plane/src/storage_controller.rs::ensure_external_database`);
the controller binary applies the migration on first start, which
creates every schema, ENUM, and table in one shot. Listed here for
reference / disaster-recovery; you do **not** need to run any of it
by hand.

| Schema | Object | Purpose |
|---|---|---|
| `public` | `blob_namespace` ENUM | `pageserver` / `safekeeper` discriminator on `blobs`. |
| `public` | `blobs` | Chunked blob store (pageserver yb_storage_conn); PK `((branch_id, key) HASH, namespace, chunk_idx)`. |
| `pageserver` | `pages` | Relation pages, one row per (block, LSN); PK `((branch_id, spc, db, rel, fork) HASH, blknum, lsn DESC, is_image DESC)`. |
| `pageserver` | `slru_pages` | SLRU blocks (pg_xact, pg_multixact_*); same shape as `pages`, keyed by `(branch_id, kind, segno)`. |
| `pageserver` | `meta` | Metadata blobs (relsize sentinels, dir listings, aux, two-phase, repl origins); full-overwrite semantics; PK `((branch_id, key) HASH, lsn DESC)`. |
| `pageserver` | `timeline_state` | Per-branch state (replaces `metadata` + `index_part.json` + `tenant-manifest.json`); generation-fenced via `current_generation`; PK `((branch_id) HASH)`. |
| `safekeeper` | `timeline_state` | Per-branch safekeeper control file (replaces upstream's local-FS `safekeeper.control` blob); PK `((branch_id) HASH)`. |
| `safekeeper` | `wals` | Live-WAL rows (coalesced consumer batches); PK `((branch_id, bucket_id) HASH, segno, chunk_offset)`, 10 per-batch placement buckets across 10 tablets. |
| `storage_controller` | 8 tables | `branches`, `nodes`, `safekeepers`, `metadata_health`, `timelines`, `pending_timeline_inits`, `timeline_imports`, `endpoints`. |

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
