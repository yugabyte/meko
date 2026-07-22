# YugabyteDB as the durable layer

Every durable byte in yb-amp lives in YugabyteDB. The pageserver,
safekeeper, endpoint_storage, and controller are stateless daemons that
write their state to YB tables and can be killed and restarted at any
point; durability, replication, and fault tolerance come from YB's own
storage layer (DocDB tablets under Raft), not from anything the daemons
do locally. This page describes what the YB cluster looks like from
yb-amp's perspective: the database, every schema and table, how the
daemons connect, and the query contract every YB-talking call site must
obey.

## The `yb_storage` database

All subsystems share **one** database, `yb_storage`. Within it:

| Schema | Tables | Provisioned by |
|---|---|---|
| `public` | `blobs` table + `blob_namespace` ENUM | migration `0002_compute_storage` |
| `pageserver` | `pages`, `slru_pages`, `meta`, `timeline_state` | migration `0002_compute_storage` |
| `safekeeper` | `timeline_state`, `wals` | migration `0002_compute_storage` |
| `controller` | the controller's product model: `app_user`, `storage_pool`, `project`, `user_pool_grant`, `user_project_grant`, `branch`, `endpoint`, `task`, `task_step` (plus the `grant_role`, `endpoint_role`, `task_status`, `step_status` ENUMs) | migration `0001_init` |
| `registry` | the controller's storage-layer registry: `branches`, `nodes`, `timelines`, `pending_timeline_inits`, `endpoints` | migration `0003_registry` |

### Provisioning — controller migrations on startup

The migrations live in [`controller/migrations/`](../../controller/migrations/)
and are embedded into the controller binary (`controller/migrations/embed.go`);
the controller applies them via `golang-migrate` on every startup
(`db.Migrate` in `controller/internal/db/migrate.go`, called from both
`controller serve` and the standalone `controller migrate` subcommand —
the latter exists for init-container / pre-deploy use). Already-applied
migrations are skipped via the `schema_migrations` bookkeeping table.

The migrator's `search_path` is pinned to the control-plane schema
(`--db-schema` / `AMP_DB_SCHEMA`, default `controller`), and `db.Migrate`
issues `CREATE SCHEMA IF NOT EXISTS controller` before the migrate driver
initialises — the bookkeeping table lives in that schema, so the schema
must exist before the very first migration runs. Unqualified DDL in the
migrations therefore lands in `controller`; every object outside it
(`public.*`, `pageserver.*`, `safekeeper.*`, `registry.*`) is written
schema-qualified. Every statement in `0002_compute_storage` and
`0003_registry` is IF-NOT-EXISTS-guarded, so those migrations are
idempotent at the SQL level too.

The data-plane migration —
[`controller/migrations/0002_compute_storage.up.sql`](../../controller/migrations/0002_compute_storage.up.sql)
— is the authoritative definition of every compute-facing schema and
table. The schema/table names are **hardcoded** by the daemons
(`compute/pageserver/src/page_store.rs` `DEFAULT_SCHEMA = "pageserver"`,
`compute/safekeeper/src/yb_wal_storage.rs` `safekeeper.wals`,
`compute/libs/remote_storage/src/yb_store.rs` `blobs`) and bound at
daemon startup — no service creates its own tables, so the controller
must have run before the pageserver/safekeeper/endpoint_storage start.

The `yb_storage` **database** itself is created outside the migrations:
`yb_amp_local` does it on controller start (`ensureExternalDatabase` in
`controller/local/launch/nodes.go`), `deploy/up.sh` does it during
cluster bring-up, and a standalone endpoint pod can do it itself with
`DEV_PROVISION_SCHEMA=true` (see
[`single-container-endpoint.md`](single-container-endpoint.md)).

## Connection paths

There are two ways yb-amp code reaches YB:

- **`YsqlClient`** ([`compute/libs/ysql_client/src/lib.rs`](../libs/ysql_client/src/lib.rs))
  — an auto-reconnecting, prepared-statement-caching wrapper around
  `tokio_postgres` speaking the Postgres wire protocol to a tserver's
  YSQL port (5433). Every component's SQL goes through it: the
  pageserver's `PageStore` and `YbStore`, the safekeeper's WAL mirror
  and control file, endpoint_storage. `from_yb_config_pooled` opens N
  independent connections round-robined per call for hot paths that
  benefit from parallel execution. `YbConnConfig` is the one canonical
  connection block (TOML `{ host_ports = "...", user = "...", dbname =
  "..." , ... }`) every daemon deserializes; it carries connection
  identity only — no table or schema names, each component hardcodes
  the tables it owns.
- **`yb_tsclient`** ([`compute/libs/yb_tsclient/`](../libs/yb_tsclient/))
  — a pure-Rust thin RPC client speaking the pggate RPC protocol
  (`yb.tserver.PgClientService`) directly to a tserver's RPC port
  (9100), skipping the PG backend entirely (wire framing, parse/plan,
  pggate). It implements exactly what the hot tables need — hash-eq +
  range-bound clustered scans with paging and snapshot pinning, and
  batched `PGSQL_UPSERT` writes — and nothing else.

### Direct-to-tserver hot path (`hot_path = "tsclient"`)

The shared `yb_storage_conn` block accepts several optional keys:

```toml
yb_storage_conn = { host_ports = "...", hot_path = "tsclient", tserver_rpc_addrs = "host:9100,...", tsclient_read_sessions = 4, tsclient_write_sessions = 4 }
```

- `hot_path` — `"tsclient"` (the default) runs the hot paths over
  `yb_tsclient`; `"ysql"` is the Postgres-wire escape hatch. The
  safekeeper honors it for the WAL write/read path on `safekeeper.wals`
  (see [`safekeeper.md`](safekeeper.md) §"Direct-to-tserver hot path")
  and the pageserver for the page read/write hot path on
  `pages`/`slru_pages`/`meta` (see
  [`pageserver.md`](pageserver.md) §"Direct-to-tserver hot
  path"). The tsclient lane keeps per-op fallback to YSQL at runtime;
  cold paths (DDL, GC, rehydration, control file, `timeline_state`)
  always use YSQL. A binary that does not know these keys fails to
  parse a config that sets them (`deny_unknown_fields`) — intentional,
  not silently ignored.
- `tserver_rpc_addrs` — yb-tserver RPC endpoints for the `tsclient`
  transport. When unset, the `host_ports` hosts on port 9100; set it
  explicitly to reach every tserver when `host_ports` is only a subset
  (e.g. a single YSQL VIP, or a K8s service that load-balances across
  the tserver pods).
- `tsclient_read_sessions` / `tsclient_write_sessions` — server-*session*
  counts for the `tsclient` pool. They set request *concurrency* (each
  server session is a serial Perform queue). Used verbatim when set (no
  scale-factor multiplication). When unset, each daemon applies its own
  default: the **pageserver** `scale_count(4, scale_factor)` for both
  (getpage + put_pages fan-out); the **safekeeper** a fixed **1 read / 1
  write** (the WAL lane is low-concurrency — one-batch-at-a-time flush +
  segment reads).
- `tsclient_sessions_per_conn` — how many `tsclient` sessions ride each
  TCP connection (default **4**). The pool opens
  `ceil((read_sessions + write_sessions) / sessions_per_conn)`
  connections and packs the sessions across them (reads and writes
  sharing connections), so the **connection count grows with the session
  count**, and it is the connections — not the sessions — that spread
  load across tserver nodes: behind a single ClusterIP VIP every
  connection dials the one VIP and the VIP lands each on a (possibly
  different) tserver, so more connections = more nodes carrying traffic.
  Each daemon logs the effective counts in its
  `thin-RPC (tsclient) hot path ready …` line at startup.

At startup each daemon resolves its hot tables' DocDB ids via a one-time
catalog lookup over YSQL (`lookup_table_oids`), failing hard if any
table is missing — a daemon never runs with a partially-enabled direct
path. The deployment consequence: every safekeeper/pageserver host must
be able to reach the tserver RPC port (9100, or whatever
`tserver_rpc_addrs` names), not just YSQL 5433 — see
[`high-level.md`](high-level.md) §"The hot path needs direct tserver
RPC access".

## `public.blobs`

Generic chunked blob store, shared by everything that needs
object-shaped storage:

- **The pageserver** (through the `RemoteStorage` trait's `YbStore`
  backend, [`remote-storage.md`](remote-storage.md)): the per-branch
  `initdb.tar.zst` archives ([`compute/libs/initdb_storage/`](../libs/initdb_storage/)).
  The pageserver runs `initdb` once when a branch is created, ships the
  tarball here, and later branch creations download it instead of
  paying the `initdb` cost again.
- **endpoint_storage**: endpoint-scoped scratch blobs (LFC prewarm
  state) that the compute and control plane read/write over its HTTP
  API.

Subsystems are segregated by the **`namespace` column** (a typed ENUM,
derived from the blob's key), not by separate tables:

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

The column split is populated at write time by `YbStore::parse_key`
(`compute/libs/remote_storage/src/yb_store.rs`):

- **`namespace`** — `safekeeper` if the key contains `safekeeper`, else
  `pageserver`. The `safekeeper` variant is reserved; safekeeper state
  lives in typed rows (`safekeeper.timeline_state`, `safekeeper.wals`),
  so current safekeeper builds write no blobs.
- **`branch_id`** — the branch-id-shaped token in the path (32 hex
  chars, optionally with a `-<count>-<num>` shard suffix). Empty string
  when absent.
- **`key`** — the discriminating filename only: the path with the
  `branches` literal and the branch id removed.
  `split_key_generation` then peels a trailing or pre-extension
  `-<8hex>` generation marker into the separate `generation` column;
  `reconstruct_key` + `rejoin_key_generation` rebuild the original
  `RemotePath` for `list` outputs (round-trip lossless).
- **`generation`** — the pageserver attach generation parsed off the
  key; `NULL` for genless files (e.g. the initdb archives). Matched
  explicitly on read (`generation IS NOT DISTINCT FROM $N`).

The PK puts `(branch_id, key)` in the hash partition so every object
gets its own hash bucket (no hot tablet on big branches) and a
`key = $x` lookup resolves to one tablet; within a bucket rows sort by
`(namespace, chunk_idx)`. Generation is a plain column, so multiple
generations of the same logical file collapse into one row by design —
newer generations overwrite older ones, and YB's ACID isolation
prevents split-brain between two writers.

**Chunking**: objects are split into rows of `YB_CHUNK_SIZE = 100 MiB`
(`yb_store.rs`), because YB's RPC layer rejects messages larger than
`rpc_max_message_size` (256 MiB default) — a single-row BYTEA above
that limit cannot be written at all. 100 MiB keeps the chunk count at 1
for typical objects. The `size_bytes` / `etag` / `last_modified_us` /
`metadata_json` columns are denormalized onto every chunk row so a
`head` lookup is a single-row read.

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

## `pageserver` schema

The pageserver's row-per-version page store; see
[`pageserver.md`](pageserver.md) for the full design.

### `pageserver.pages`

One row per `(block, LSN)`. Holds **relation pages only** — keys where
`Key::is_rel_block_key()` holds (`field1 == 0x00`, `field4 != 0`,
`field6 != 0xffffffff`). SLRU blocks go to `slru_pages`; everything
else goes to `meta`. The hash partition on `(branch_id, spcnode,
dbnode, relnode, forknum)` keeps a relation fork's whole block-set on
one tablet, so range scans within a relfork are tablet-local.
Clustering by `(blknum, lsn DESC, is_image DESC)` stores a block's rows
newest-first; `is_image` is DESC so the stored order matches the
chain-read `ORDER BY lsn DESC, is_image DESC` (image before delta at an
LSN tie).

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

The chain read is image-anchored: a correlated subquery seeks the
newest `is_image = TRUE` row at LSN ≤ target and bounds the scan from
below, so the fast path is one anchor seek plus a small range scan
above it (the anchor is a pushable lower bound on the DocDB scan):

```sql
SELECT p.lsn, p.is_image, p.will_init, p.payload
FROM pageserver.pages p
WHERE p.branch_id=$1
  AND p.spcnode=$2 AND p.dbnode=$3 AND p.relnode=$4
  AND p.forknum=$5 AND p.blknum=$6
  AND p.lsn <= $7
  AND p.lsn >= COALESCE((
        SELECT a.lsn FROM pageserver.pages a
        WHERE a.branch_id=$1
          AND a.spcnode=$2 AND a.dbnode=$3 AND a.relnode=$4
          AND a.forknum=$5 AND a.blknum=$6
          AND a.is_image=TRUE AND a.lsn <= $7
        ORDER BY a.lsn DESC LIMIT 1), 0)
ORDER BY p.lsn DESC, p.is_image DESC;
```

When no image exists for the block (the anchor subquery is empty, so
the bound falls back to 0), the reader walks rows LSN-descending until
it hits a `will_init = true` delta. That is the cold path; the
background image materialiser (see
[`pageserver.md`](pageserver.md)) compacts long delta chains into
walredo-applied images to keep it rare.

### `pageserver.slru_pages`

SLRU blocks — `pg_xact`, `pg_multixact_offsets`,
`pg_multixact_members`. `kind` matches the key encoding's `field2`
ordering: 0 = Clog, 1 = MultiXactMembers, 2 = MultiXactOffsets. Same
image/delta shape as `pages`; the hash partition keeps a whole SLRU
segment on one tablet.

```sql
CREATE TABLE pageserver.slru_pages (
    branch_id    TEXT     NOT NULL,
    kind         SMALLINT NOT NULL,
    segno        BIGINT   NOT NULL,
    blknum       BIGINT   NOT NULL,
    is_image     BOOLEAN  NOT NULL,
    lsn          BIGINT   NOT NULL,
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((branch_id, kind, segno) HASH,
                 blknum, lsn DESC, is_image DESC)
);
```

### `pageserver.meta`

Metadata blobs — everything that is neither a relation page nor an SLRU
block: relsize sentinels, directory listings (TenantDir / DbDir /
RelDir / SlruDir), aux files (`yb_amp.signal`, `postgresql.auto.conf`,
replication-slot state), two-phase commit files, replication origins,
SLRU segment-size sentinels. Full-overwrite semantics: a read is the
latest row at LSN ≤ target — no walredo, no chains.

```sql
CREATE TABLE pageserver.meta (
    branch_id    TEXT     NOT NULL,
    key          BYTEA    NOT NULL,
    lsn          BIGINT   NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((branch_id, key) HASH, lsn DESC)
);
```

### `pageserver.timeline_state`

One row per branch — the page store's durable index record, carrying
every load-bearing per-branch field in a single transactionally-updatable
row: consistent / GC-cutoff / initdb LSNs, ancestry, generation,
archive/delete timestamps, and rel-size migration state.
`current_generation` is the gate that fences a stale pageserver: every
flush runs in a transaction that conditionally updates this row
(`... WHERE current_generation = $expected`), so a pageserver holding
an old generation cannot clobber state owned by a newer attachment. The
`lineage` JSONB column is a forward-looking slot for ancestor-detach
state (always `Default` today).

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

## `safekeeper` schema

### `safekeeper.timeline_state`

The per-branch safekeeper control file as a typed row — one row per
branch, hash-partitioned on `branch_id` so it lives on a single tablet.
Hot-update LSN scalars get their own BIGINT columns so a control-file
save is a column-scoped UPDATE rather than a full-row rewrite. yb-amp
runs a single safekeeper per branch with WAL durability provided by
YB itself, so there is no consensus state (term / term history) and no
membership configuration to persist.

```sql
CREATE TABLE safekeeper.timeline_state (
    branch_id              TEXT        NOT NULL,
    timeline_start_lsn     BIGINT      NOT NULL,
    local_start_lsn        BIGINT      NOT NULL,
    remote_consistent_lsn  BIGINT      NOT NULL,
    pg_version             INT         NOT NULL,
    system_id              BIGINT      NOT NULL,
    wal_seg_size           INT         NOT NULL,
    creation_ts            TIMESTAMPTZ NOT NULL,
    PRIMARY KEY ((branch_id) HASH)
);
```

### `safekeeper.wals`

The durable copy of the WAL stream. The safekeeper keeps no WAL on
local disk: every WAL write lands here, and reads are served from here
(fronted by an in-memory hot cache for recent WAL). A commit is acked
to the compute only once its bytes are in this table.

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

Each row is one coalesced run of a consumer batch: contiguous chunks of
a segment are concatenated before the write, so the typical batch is
one row. The writer never produces overlapping rows (`truncate_wal`
aligns the next write at exactly the truncation point), so
`(segno, chunk_offset)` is a chunk's natural identity within the
branch and a same-PK INSERT is necessarily an idempotent retry of the
same bytes.

`bucket_id` exists purely for tablet placement: the
`(branch_id, bucket_id)` hash key spreads a branch's WAL across all
`WALS_BUCKETS = 10` tablets. The consumer stamps each write **batch**
with one bucket, rotating through the buckets one batch at a time —
each batch lands on ONE tablet (the tsclient lane applies it as one
atomic write batch; the YSQL lane's multi-VALUES INSERT hits the
single-shard transaction fast path), while sustained load rotates
across all tablets. Readers cannot derive a chunk's bucket, so
index-tight reads name every bucket with a `bucket_id IN (0..9)`
hash-key list — YSQL executes that as batched per-bucket index probes,
not a scan.

Schema and batching documented in detail in
[`safekeeper.md`](safekeeper.md).

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

(`yb-dump-wals --summary` renders the same view with gap detection —
see [`../README.md`](../README.md) §"Operator tools".)

### `pageserver.pending_gc`

GC candidate queue (pageserver.md §GC): one row per
garbage-producing event, inserted when the event is applied, drained
once the branch's GC cutoff passes `due_lsn`. `blknum` doubles as the
candidate kind: ≥ 0 per-block trim, −2 whole-fork drop, −1 rel-size
shrink (only trim has writers in this cut). Every stored column is in
the PK, so a candidate re-inserted by crash + WAL replay (or a chunk
retry) is a byte-identical upsert — no `ON CONFLICT` needed under
`yb_enable_upsert_mode`.

```sql
CREATE TABLE pageserver.pending_gc (
    branch_id  TEXT     NOT NULL,
    bucket_id  INT      NOT NULL,  -- writer-computed: page_store::gc_bucket_id(identity) % GC_BUCKETS
    spcnode    BIGINT   NOT NULL,
    dbnode     BIGINT   NOT NULL,
    relnode    BIGINT   NOT NULL,
    forknum    SMALLINT NOT NULL,
    blknum     BIGINT   NOT NULL,
    due_lsn    BIGINT   NOT NULL,
    PRIMARY KEY ((branch_id, bucket_id) HASH,
                 due_lsn, spcnode, dbnode, relnode, forknum, blknum)
) SPLIT INTO 3 TABLETS;
```

`bucket_id` is placement only, never identity: it spreads a branch's
candidate inserts across tablets instead of hot-spotting the single
`(branch_id)`-hashed tablet. The **writer computes it** — a fixed,
deterministic FNV-1a hash of the identity columns mod `GC_BUCKETS`
(`pageserver::page_store::gc_bucket_id`) — and supplies it in the
INSERT. It is deliberately **not** a `GENERATED ALWAYS AS
(yb_hash_code(...) % N)` column: the planned yb-client write path
bypasses the YSQL query layer, which would never evaluate a generated
expression, so the bucket must be client-supplied. Determinism (same
block → same bucket) is what lets a crash-replayed candidate land on
the same PK and dedup; the modulus must equal `GC_BUCKETS`. The drain
issues one `due_lsn` range scan per bucket (via a UNION-ALL that YB
plans as an early-terminating Merge Append — verified unaffected by
the plain vs generated column, since the read is plain `bucket_id = N`
hash-key equality, not `yb_hash_code()` in a predicate). The tablet
count (3) is deliberately below the bucket count (10): a multi-row
insert's distributed-txn fan-out is bounded by tablets touched, while
10 buckets still hash-spread evenly across them; the SPLIT is explicit
because size-based auto-splitting never triggers on a small,
write-hot, continuously-drained queue.

## `registry` schema

The controller's storage-layer bookkeeping: generation authority (the
fencing counter) plus the node / branch / endpoint / pending-init
registries behind the HTTP surface the controller serves to
`yb_amp_local`, endpoint pods, and the integration harness. Kept in its
own schema so the flat storage model (32-hex `branch_id` VARCHAR) stays
separate from the controller's product model (project / branch /
endpoint by UUID in the `controller` schema). The controller's
connection pool pins `search_path` to `controller`; the registry store
schema-qualifies every reference (`registry.*`), so both coexist on one
pool.

| Table | Purpose |
|---|---|
| `registry.branches` | Per-branch placement + generation record. Keyed by `branch_id` alone — branches are not sharded across pageservers (YB tablet-shards the keyspace itself). The `generation` counter, bumped by the attach-hook, is the fencing primitive; monotonic across the branch's lifecycle, NULL until first attach. |
| `registry.nodes` | Pageserver registry (bookkeeping / observability; nothing gates on it). |
| `registry.timelines` | Per-branch WAL start LSN + generation tracking. The branch's single co-located safekeeper is owned by the launcher, not tracked here. |
| `registry.pending_timeline_inits` | Init recipe for a branch whose id + name were allocated at `branch create` but whose pageserver + safekeeper are not provisioned yet; `endpoint create` reads it back to materialize the branch, then deletes the row. NULL `ancestor_branch_id` = Bootstrap recipe (initdb + import on first start); non-NULL = fork off that ancestor at `start_lsn`. |
| `registry.endpoints` | Durable registry of compute endpoints, keyed by endpoint id. `spec` is opaque JSONB the controller never interprets (it round-trips through `yb_amp_local`'s serde model). |

Registry semantics are documented in
[`../../controller/architecture/sc-integration.md`](../../controller/architecture/sc-integration.md).

## `controller` schema

The controller's product model — users, storage pools, projects,
grants, branches and endpoints by UUID, and the write-ahead task
framework's `task` / `task_step` tables that drive the
create/delete workflows. Managed entirely by the controller's
migration `0001_init`; do **not** edit by hand. Inspect with:

```sql
\c yb_storage
SET search_path = controller, public;
\dt
```

## YB-side configuration that matters

### `rpc_max_message_size` (default 256 MiB)

The single most-bitten-by setting. YB's RPC layer rejects messages
larger than this, so every write path bounds its message size:

- **Blobs**: chunked into ≤ 100 MiB rows (`YB_CHUNK_SIZE`).
- **WALs**: the safekeeper's consumer task flushes a batch at 50 chunks
  (`FLUSH_ROWS_THRESHOLD`) or 4 MiB (`FLUSH_BYTES_THRESHOLD`),
  whichever comes first — comfortable headroom under the limit while
  amortizing the per-statement DocDB cost (a 50-row batch costs ~12×
  less per row than single-row INSERTs).

### `yb_enable_upsert_mode`

A per-session GUC `SET` on every yb-amp write connection (centrally in
`YsqlClient::from_yb_config` / `from_yb_config_pooled` with
`set_fast_path_upsert = true` — used by `YbStore::new`, the
pageserver's `PageStore`, and the safekeeper's `YbWalStorage`). When
`true`, plain `INSERT` skips DocDB's read-before-write conflict check —
a meaningful speedup for an upsert-heavy workload, and load-bearing for
correctness of the blob path: re-PUTs of the same blob key are plain
INSERTs that rely on upsert mode to overwrite rather than PK-conflict.

It is also `SET` on every reconnect, so transient connection drops
never leave subsequent inserts paying the read-before-write cost. It is
harmless against plain PostgreSQL (unknown GUC → SET fails → logged and
ignored), so the same code path works in mixed environments.

### `yb_disable_transactional_writes`

A second per-session GUC, `SET` alongside `yb_enable_upsert_mode` on
the fast-path-upsert (write) connections **only when the config opts
in**: `disable_transactional_writes = true` in the `yb_storage_conn` /
`[controller.yb]` block (`YbConnConfig::disable_transactional_writes`).
When on, DocDB drops the transactional write path's
read-modify-write guarantees — acceptable in principle for append-only
INSERTs whose ordering comes from the LSN scheme rather than YB
transaction semantics.

**Off by default**, because of a WAL-durability incident under a
500-connection workload: with the GUC on, batch INSERTs reported as
success appeared to lose individual rows under tablet rebalancing / RPC
retries, leaving holes in `safekeeper.wals` that wedge pageserver
restart. With the GUC off those INSERTs take the standard transactional
path — slower, but atomic. Operators can opt back in per component via
the config field.

## Client-side retry contract — every YB query must be idempotent

`YsqlClient::run_with_retries` — the wrapper behind every `execute` /
`query` / `query_opt`, i.e. every SQL statement any component sends —
retries three classes of failure internally before surfacing anything
to the caller:

- **Connection drops**: TCP socket closed mid-statement (YB tserver
  restart, rolling upgrade, network blip). Force-reconnect the slot
  (clearing its prepared-statement cache — cached `Statement`s belong
  to the dead connection), re-prepare, replay. Up to
  `MAX_CONNECTION_DROPS = 5` reconnects per call.
- **SQLSTATE-confirmed transients**: `40001` serialization failure,
  `40P01` deadlock detected, `25P03` idle-in-txn timeout. The server
  guarantees the statement did not commit — replay is safe even for
  non-idempotent SQL.
- **YB `XX000` allowlist** (`YB_TRANSIENT_MSG_PATTERNS`): `Restart read
  required`, `Tablet leader changed`, `WaitForAsyncWrite RPC` timeout,
  `Call waited in the queue past deadline` (overload backpressure),
  tablet peer `is shutting down` (split/move/restart), `schema version
  mismatch`. YB packs several distinct transients into `XX000`, so the
  message text is inspected; all of these are documented by YB as
  safe-to-retry.

Total budget per call: `MAX_TOTAL_ATTEMPTS = 10`, backoff starting at
50 ms and doubling to a 1 s cap, with 0–25% random jitter (pooled
callers otherwise retry in lockstep after a shared cluster event).
Errors surface only once the budget is spent; application/logic errors
(`42xxx` and friends) are fatal immediately.

**The catch**: the connection-drop arm reconnects without any way to
know whether the pre-drop statement committed before the socket died.
The reconnect arm replays anyway. **This is only safe if the statement
is idempotent — re-executing it must produce the same end state whether
the first attempt applied, half-applied, or did nothing.**

Every YB-talking call site in this codebase satisfies that contract:

- **All reads** — `SELECT` via `query` / `query_opt` / `query_raw` —
  are intrinsically idempotent.
- **All writes** are one of: `INSERT ... ON CONFLICT (...) DO NOTHING`
  / `DO UPDATE`; plain `INSERT` running under
  [`yb_enable_upsert_mode = true`](#yb_enable_upsert_mode) (same
  semantics — the conflict path is a no-op upsert; this covers the
  `pages` / `slru_pages` / `meta` / `wals` / `blobs` write paths); or
  generation-fenced `UPDATE pageserver.timeline_state SET ... WHERE
  current_generation = $expected` (a replay sees the same generation
  and applies the same row state).
- **DELETEs** are key-scoped (`DELETE FROM ... WHERE id = $1`); replay
  against an already-deleted row is a no-op.

**New code touching YB must keep this invariant.** Concrete rules for
contributors:

- Don't write a bare `INSERT` that needs to fail on a duplicate row.
  Add `ON CONFLICT DO NOTHING` (or `DO UPDATE ... RETURNING` if you
  need to distinguish apply from replay).
- Don't issue `UPDATE foo SET counter = counter + 1`. Replay
  double-counts. Make the update absolute (`SET counter = $new`) with
  the read-modify-write in the application.
- Don't depend on a `SELECT ... FOR UPDATE` lock surviving a retry —
  a reconnect drops the implicit transaction.
- If you genuinely need a non-idempotent write through `YsqlClient`,
  add a code path that does **not** retry the connection-drop class
  (the Transient and Fatal branches stay safe — those are
  server-confirmed no-ops). Nothing needs this today.

The Prometheus series `yb_client_errors_total{op,class,sqlstate}`,
`yb_client_retries_exhausted_total{reason}`, and
`yb_client_query_seconds{op}` (exposed on both pageserver and
safekeeper `/metrics`) make the retry mix observable. A spike in
`class="transient"` is usually a tablet event; a sustained
`retries_exhausted` rate means YB is unhealthy or unreachable beyond
what the client can absorb.

## Failure modes

- **YB unavailable on safekeeper `write_wal`**: the durability
  watermark stops advancing, so the safekeeper does not ACK and the
  compute's commit blocks (this is the point of the design — a commit
  is never acknowledged before its WAL is durable in YB). Once YB
  recovers, the consumer's retries drain the backlog and commits
  proceed.
- **YB unavailable on pageserver writes**: the page writer's
  `YsqlClient` retries absorb short blips; a longer outage stalls WAL
  ingestion and `consistent_lsn` stops advancing until YB returns.
- **YB unavailable on pageserver reads**: getpage requests fail after
  the retry budget; the compute's own request retries take over.
  Recently-written WAL reads on the safekeeper are served from its
  in-memory cache and survive short YB outages.
- **YB unavailable on the controller**: its HTTP API returns 5xx;
  running pageservers / safekeepers / computes keep operating and only
  block on the controller when they need a fresh cluster decision
  (attach, branch create).

## Hosts / ports in the dev cluster

- YB master RPC: `127.0.0.1:7100`
- YB tserver YSQL: `127.0.0.1:5433` (cold paths, DDL, controller)
- YB tserver RPC: `127.0.0.1:9100` (the `tsclient` hot path)
- YB tserver web UI: `127.0.0.1:9000`
- `yugabyted-ui`: `127.0.0.1:15433`

Started via `yugabyted start --base_dir=<dir> --advertise_address=127.0.0.1`.
