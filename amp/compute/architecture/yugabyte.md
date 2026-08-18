# YugabyteDB as the durable layer

Every durable byte in yb-amp lives in YugabyteDB. The pageserver,
safekeeper, and controller are stateless daemons that
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
| `amp_storage` | the data plane: `pages`, `slru_pages`, `meta`, `pending_gc`, `wals`, `blobs` (+ the `blob_namespace` ENUM), and `branch_state` — the per-branch row the pageserver and safekeeper share | migration `0003_compute_storage` |
| `amp_registry` | the control plane's resource model: `users`, `storage_pools`, `clusters`, `user_pool_grants`, `user_cluster_grants`, `branches`, `endpoints` (plus the `grant_role` and `endpoint_role` ENUMs). One branch table and one endpoint table, serving both the product API and the storage-layer control surface | migration `0002_resource_model` (`0005_endpoint_daemon_settings` repairs databases that applied it before the endpoint's `pageserver_settings` / `safekeeper_settings` columns were added to it) |
| `amp_registry` | the write-fencing lease store: `resource_token` plus `token_acquire` / `token_release` / `token_holder` and the epoch-micros helpers `now_us` / `us_add_ms` / `us_to_ms`. See [`compute-fencing.md`](compute-fencing.md) | migration `0006_resource_token` |
| `controller` | the write-ahead task log: `tasks`, `task_steps` (plus the `task_status` / `step_status` ENUMs) | migration `0001_task_log` |

### Provisioning — controller migrations on startup

The migrations live in [`controller/migrations/`](../../controller/migrations/)
and are embedded into the controller binary (`controller/migrations/embed.go`);
the controller applies them via `golang-migrate` on every startup
(`db.Migrate` in `controller/internal/db/migrate.go`, called from both
`controller serve` and the standalone `controller migrate` subcommand —
the latter exists for init-container / pre-deploy use). Already-applied
migrations are skipped via the `schema_migrations` bookkeeping table.

The migrator's `search_path` is pinned to the control-plane resource
schema (`--db-schema` / `AMP_DB_SCHEMA`, default `amp_registry`), and
`db.Migrate` issues `CREATE SCHEMA IF NOT EXISTS <schema>` before the
migrate driver initialises — the bookkeeping table lives in that schema,
so the schema must exist before the very first migration runs. (The
migration connection is pinned to a single connection for the same
reason a fresh database used to fail: YB caches the catalog per backend,
so a `CREATE SCHEMA` on one pooled connection is not necessarily visible
to the next.) Unqualified DDL in the migrations therefore lands in
`amp_registry`; every object outside it (`controller.*`, `amp_storage.*`)
is written schema-qualified. Every statement in `0002_compute_storage`
is IF-NOT-EXISTS-guarded, so it is idempotent at the SQL level too.

The data-plane migration —
[`controller/migrations/0003_compute_storage.up.sql`](../../controller/migrations/0003_compute_storage.up.sql)
— is the authoritative definition of every compute-facing schema and
table. The schema/table names are **hardcoded** by the daemons
(`compute/pageserver/src/page_store.rs` `DEFAULT_SCHEMA = "amp_storage"`,
`compute/safekeeper/src/yb_wal_storage.rs` `amp_storage.wals`,
`compute/libs/remote_storage/src/yb_store.rs` `blobs`) and bound at
daemon startup — no service creates its own tables, so the controller
must have run before the pageserver/safekeeper start.

The `yb_storage` **database** itself is created outside the migrations:
`yb_amp_local` does it on controller start (`ensureExternalDatabase` in
`controller/local/launch/nodes.go`), and `deploy/up.sh` does it during
cluster bring-up. An endpoint pod never provisions its own schema (see
[`single-container-endpoint.md`](single-container-endpoint.md)).

## Connection paths

There are two ways yb-amp code reaches YB:

- **`YsqlClient`** ([`compute/libs/ysql_client/src/lib.rs`](../libs/ysql_client/src/lib.rs))
  — an auto-reconnecting, prepared-statement-caching wrapper around
  `tokio_postgres` speaking the Postgres wire protocol to a tserver's
  YSQL port (5433). Every component's SQL goes through it: the
  pageserver's `PageStore` and `YbStore`, the safekeeper's WAL mirror
  and control file. `from_yb_config_pooled` opens N
  independent connections round-robined per call for hot paths that
  benefit from parallel execution. `YbConnConfig` is the one canonical
  connection block (TOML `{ host_ports = "...", user = "...", dbname =
  "..." , ... }`) every daemon deserializes; it carries connection
  identity only — no table or schema names, each component hardcodes
  the tables it owns.
- **`yb_thin_client`** ([`compute/libs/yb_thin_client/`](../libs/yb_thin_client/))
  — the same idea over a thin C ABI to a `libyb_thin_client.so` C++ shim
  (a YugabyteDB build target linking YB's own proxy/partitioning/codec),
  so routing, partition hashing, and `pg_doc_data` row decoding come from
  YB's own code rather than being re-implemented in Rust. Its one read
  primitive is a **batch of ops = one `Perform`** (one RPC, one snapshot),
  so multi-op reads collapse to a single round trip; writes are the same
  shape. Selected by `hot_path = "yb_thin_client"` (**the default**) — the
  single hot-path client. It speaks the pggate RPC protocol
  (`yb.tserver.PgClientService`) directly to a tserver's RPC port (9100),
  skipping the PG backend entirely (wire framing, parse/plan, pggate), and
  implements exactly what the hot tables need — hash-eq + range-bound
  clustered scans with paging and snapshot pinning, and batched
  `PGSQL_UPSERT` writes — and nothing else.

### Direct-to-tserver hot path (`hot_path = "yb_thin_client"`)

The shared `yb_storage_conn` block accepts several optional keys:

```toml
yb_storage_conn = { host_ports = "...", tserver_rpc_addrs = "host:9100,...", tsclient_read_sessions = 4, tsclient_write_sessions = 4 }
```

- `hot_path` — `"yb_thin_client"` (**the default**) runs the hot paths over
  the `yb_thin_client` shim client (batch-first: a call = a batch of ops =
  one Perform); `"ysql"` is the Postgres-wire escape hatch. The
  safekeeper honors it for the WAL write/read path on `amp_storage.wals`
  (see [`safekeeper.md`](safekeeper.md) §"Direct-to-tserver hot path")
  and the pageserver for the page read/write hot path on
  `pages`/`slru_pages`/`meta` (see
  [`pageserver.md`](pageserver.md) §"Direct-to-tserver hot
  path"). The direct lanes keep per-op fallback to YSQL at runtime;
  cold paths (DDL, GC, rehydration, control file, `branch_state`)
  always use YSQL. A binary that does not know these keys fails to
  parse a config that sets them (`deny_unknown_fields`) — intentional,
  not silently ignored.
- `tserver_rpc_addrs` — yb-tserver RPC endpoints for the direct-RPC
  transport. When unset, the `host_ports` hosts on port 9100; set it
  explicitly to reach every tserver when `host_ports` is only a subset
  (e.g. a single YSQL VIP, or a K8s service that load-balances across
  the tserver pods).
- `tsclient_read_sessions` / `tsclient_write_sessions` — server-*session*
  counts for the direct-RPC pool. (The `tsclient_` prefix is historical: it
  predates `yb_thin_client` replacing `yb_tsclient`; the keys are kept so
  existing configs parse.) They set request *concurrency* (each
  server session is a serial Perform queue). Used verbatim when set (no
  scale-factor multiplication). When unset, each daemon applies its own
  default: the **pageserver** `scale_count(2, scale_factor)` for both
  (getpage + put_pages fan-out); the **safekeeper** a fixed **1 read / 1
  write** (the WAL lane is low-concurrency — one-batch-at-a-time flush +
  segment reads).
- `tsclient_sessions_per_conn` — how many hot-path sessions ride each
  TCP connection (default **4**). The pool opens
  `ceil((read_sessions + write_sessions) / sessions_per_conn)`
  connections and packs the sessions across them (reads and writes
  sharing connections), so the **connection count grows with the session
  count**, and it is the connections — not the sessions — that spread
  load across tserver nodes: behind a single ClusterIP VIP every
  connection dials the one VIP and the VIP lands each on a (possibly
  different) tserver, so more connections = more nodes carrying traffic.
  Each daemon logs the effective counts in its
  `thin-client hot path ready …` line at startup.

### Bounding a call (`connect_timeout_ms` / `statement_timeout_ms`)

Two more keys on the same block bound how long a YSQL call may take. Both
read `0` as "no bound". The connect bound is **on by default** at 5s; the
statement bound is **off** by default and opt-in per block:

```toml
yb_storage_conn = { host_ports = "...", connect_timeout_ms = 5000, statement_timeout_ms = 30000 }
```

- `connect_timeout_ms` — a ceiling on opening one connection. It covers
  the **whole** open: the driver's own `connect_timeout` is spent on
  `TcpStream::connect` alone, which leaves TLS, the startup handshake,
  authentication and the session GUCs unbounded, so `YsqlClient` applies
  the same value again around the lot. A peer that completes the TCP
  handshake and then goes quiet is the case that matters — nothing
  fails, so the retry budget below is never spent and the caller waits
  forever.
- `statement_timeout_ms` — the server-side `statement_timeout` GUC, sent
  as `-c statement_timeout=N` in the startup packet. The server enforces
  it, so a statement a wedged tablet would otherwise hold open is
  cancelled at the far end rather than waiting on the client to lose
  patience. It bounds a *slow* answer, not a *silent* peer: a backend
  that stops responding mid-statement is bounded by TCP keepalives, not
  by this. The cancellation arrives as `57014`, which is fatal — see the
  retry contract below.

**`statement_timeout_ms` is off by default, deliberately.** It applies to
**every** statement on the connection, and several the daemons issue
legitimately run for minutes:

- the GC drop/shrink deletes (`gc_act_drop_fork`, `gc_act_shrink_fork`)
  remove every version of every block of a fork in one unbatched
  statement — and because a candidate is dequeued only once its action
  succeeds, a cancelled delete is re-selected forever. GC stops and the
  storage is never reclaimed.
- `list_rels` runs an unbounded `DISTINCT` over a database's reldir range
  on the basebackup path, so a cap there fails compute startup.
- blob upload/download already carry their own 120s budget
  (`RemoteStorageConfig::DEFAULT_TIMEOUT`), which a shorter statement
  bound would silently preempt — mid-stream, for a download.

Turning it on globally wants those paths batched and made sargable first.
Until then a caller that knows its statements are short sets it on its own
block. The cancellation arrives as `57014`, which is fatal — see the retry
contract below — so this is not a knob that degrades gracefully.

The diagnostic tools in `compute/tools` set both to `0`. They are run by
hand during an incident and issue the long scans the daemons never do —
`reset-consistent-lsn` replays the WAL decoder over `amp_storage.wals` —
so the operator watching it is the timeout.

At startup each daemon resolves its hot tables' DocDB ids via a one-time
catalog lookup over YSQL (`lookup_table_oids`), failing hard if any
table is missing — a daemon never runs with a partially-enabled direct
path. The deployment consequence: every safekeeper/pageserver host must
be able to reach the tserver RPC port (9100, or whatever
`tserver_rpc_addrs` names), not just YSQL 5433 — see
[`high-level.md`](high-level.md) §"The hot path needs direct tserver
RPC access".

## `amp_storage.blobs`

Generic chunked blob store, shared by everything that needs
object-shaped storage:

- **The pageserver** (through the `RemoteStorage` trait's `YbStore`
  backend, [`remote-storage.md`](remote-storage.md)): the per-branch
  `initdb.tar.zst` archives ([`compute/libs/initdb_storage/`](../libs/initdb_storage/)).
  The pageserver runs `initdb` once when a branch is created, ships the
  tarball here, and later branch creations download it instead of
  paying the `initdb` cost again.
- **compute_ctl**: endpoint-scoped scratch blobs (LFC prewarm
  state) that the compute and control plane read/write over its HTTP
  API.

Subsystems are segregated by the **`namespace` column** (a typed ENUM,
derived from the blob's key), not by separate tables:

```sql
CREATE TYPE amp_storage.blob_namespace AS ENUM ('pageserver', 'safekeeper');

CREATE TABLE amp_storage.blobs (
    branch_id        uuid                  NOT NULL,    -- branch id; nil uuid if not extractable
    namespace        amp_storage.blob_namespace NOT NULL,    -- 'pageserver' or 'safekeeper'
    key              TEXT                  NOT NULL,    -- discriminating filename, generation suffix stripped
    generation       INT,                               -- pageserver attach gen; NULL for genless files
    chunk_idx        INT                   NOT NULL,
    data             BYTEA                 NOT NULL,
    size_bytes       BIGINT                NOT NULL,    -- total object size, denormalized
    etag             TEXT                  NOT NULL,    -- total-object etag, denormalized
    last_modified_us BIGINT                NOT NULL,
    metadata_json    TEXT,
    PRIMARY KEY ((branch_id) HASH, namespace ASC, key ASC, chunk_idx ASC)
);
```

The column split is populated at write time by `YbStore::parse_key`
(`compute/libs/remote_storage/src/yb_store.rs`):

- **`namespace`** — `safekeeper` if the key contains `safekeeper`, else
  `pageserver`. The `safekeeper` variant is reserved; safekeeper state
  lives in typed rows (`amp_storage.branch_state`, `amp_storage.wals`),
  so current safekeeper builds write no blobs.
- **`branch_id`** — the branch-id-shaped token in the path (a uuid,
  optionally with a `-<count>-<num>` shard suffix), stored as a `uuid`.
  The nil uuid when the path carries no id.
- **`key`** — the discriminating filename only: the path with the
  `branches` literal and the branch id removed.
  `split_key_generation` then peels a trailing or pre-extension
  `-<8hex>` generation marker into the separate `generation` column;
  `reconstruct_key` + `rejoin_key_generation` rebuild the original
  `RemotePath` for `list` outputs (round-trip lossless).
- **`generation`** — the pageserver attach generation parsed off the
  key; `NULL` for genless files (e.g. the initdb archives). Matched
  explicitly on read (`generation IS NOT DISTINCT FROM $N`).

The PK hashes on `branch_id` alone, so a branch's blobs live on one
tablet and every branch-scoped operation is key-bound: point lookups
seek `(namespace, key)` inside the branch's range, a prefix LIST is a
range scan on the `key` range column, and branch-drop GC is a single
hash-bound DELETE. Per-branch blob volume is small (initdb-archive
scale), which is what makes the one-tablet-per-branch bound safe.
Generation is a plain column, so multiple generations of the same
logical file collapse into one row by design — newer generations
overwrite older ones, and YB's ACID isolation prevents split-brain
between two writers.

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

-- Per-branch breakdown (fast — branch_id binds the hash partition)
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

## `amp_storage` schema

The pageserver's row-per-version page store; see
[`pageserver.md`](pageserver.md) for the full design.

### `amp_storage.pages`

One row per `(block, LSN)`. Holds **relation pages only** — keys where
`Key::is_rel_block_key()` holds (`field1 == 0x00`, `field4 != 0`,
`field6 != 0xffffffff`). SLRU blocks go to `slru_pages`; everything
else goes to `meta`. The key is all-range, led by a 6-way placement
bucket (`page_store::page_bucket_id`) so the table pre-splits into
exactly 6 tablets and writes spread from the first row. Within a bucket
the relfork identity still clusters together, so range scans within a
relfork stay tablet-local for any 2MB stretch. Clustering by
`(blknum, lsn DESC, is_image DESC)` stores a block's rows newest-first;
`is_image` is DESC so the stored order matches the chain-read
`ORDER BY lsn DESC, is_image DESC` (image before delta at an LSN tie).

```sql
CREATE TABLE amp_storage.pages (
    bucket_id    SMALLINT NOT NULL,    -- ((relnode/7)+(relnode/13)+(blknum/256))%6
    branch_id    uuid     NOT NULL,
    spcnode      BIGINT   NOT NULL,
    dbnode       BIGINT   NOT NULL,
    relnode      BIGINT   NOT NULL,
    forknum      SMALLINT NOT NULL,
    blknum       BIGINT   NOT NULL,
    is_image     BOOLEAN  NOT NULL,
    lsn          BIGINT   NOT NULL,    -- u64 stored as i64 (real LSNs << 2^63)
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    CONSTRAINT pages_bucket_id_matches_formula CHECK (
        bucket_id = ((relnode / 7) + (relnode / 13) + (blknum / 256)) % 6
    ),
    PRIMARY KEY (bucket_id ASC, branch_id ASC, spcnode ASC, dbnode ASC,
                 relnode ASC, forknum ASC, blknum ASC, lsn DESC, is_image DESC)
) SPLIT AT VALUES ((1), (2), (3), (4), (5));
```

Because `bucket_id` leads the key, **every** query against `pages` binds
it to a single value. A chain read knows its blknum and so binds the
entire 7-column prefix (one tablet); a blknum range scan touches one
bucket per 2MB stretch and loops over `page_buckets_for_range`;
whole-fork and cross-fork operations fan out over all six. On the thin
lane that is forced — its range-key bind takes one value per column, with
no set form — whereas YSQL could equally use `bucket_id = ANY(…)`, which
YB pushes into the index condition; the per-bucket shape is shared so
both lanes fan out alike. See [`pageserver.md`](pageserver.md) "Why the
bucket" for the function's design and the salt's failure mode.

`payload` here — and in `slru_pages` and `meta` below — holds a
compression envelope, not bare page bytes: a codec tag byte followed by
(by default) an LZ4 block. The Rust layer encodes on write and decodes
on read; YSQL only ever sees an opaque `BYTEA`. Consequently ad-hoc SQL
against these columns measures *stored* bytes — `octet_length(payload)`
is the compressed length and `substring(payload, 1, n)` slices envelope
bytes — so use `dump-pages`, which decodes client-side. See
[`pageserver.md`](pageserver.md) "Payload compression" for the format,
the `yb_payload_compression` knob, and why compressing above DocDB
(which does not TOAST these values) pays.

The chain read is image-anchored: a correlated subquery seeks the
newest `is_image = TRUE` row at LSN ≤ target and bounds the scan from
below, so the fast path is one anchor seek plus a small range scan
above it (the anchor is a pushable lower bound on the DocDB scan):

```sql
SELECT p.lsn, p.is_image, p.will_init, p.payload
FROM amp_storage.pages p
WHERE p.branch_id=$1
  AND p.spcnode=$2 AND p.dbnode=$3 AND p.relnode=$4
  AND p.forknum=$5 AND p.blknum=$6
  AND p.lsn <= $7
  AND p.lsn >= COALESCE((
        SELECT a.lsn FROM amp_storage.pages a
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

### `amp_storage.slru_pages`

SLRU blocks — `pg_xact`, `pg_multixact_offsets`,
`pg_multixact_members`. `kind` matches the key encoding's `field2`
ordering: 0 = Clog, 1 = MultiXactMembers, 2 = MultiXactOffsets. Same
image/delta shape as `pages`. The hash partition is `branch_id` alone,
so a branch's whole SLRU space lives on one tablet: branch-drop GC is a
single key-bound DELETE, and a whole-SLRU export is one contiguous
scan. That bound is safe because SLRU volume is protocol-capped (clog
is 2 bits per xact and vacuum's freeze horizons force truncation), and
the write hotspot — the single head page per kind — is one key, which
no partitioning scheme could spread anyway. Within the branch, the
range columns keep a `(kind, segno)` segment contiguous with `blknum`
ordered inside it.

```sql
CREATE TABLE amp_storage.slru_pages (
    branch_id    uuid     NOT NULL,
    kind         SMALLINT NOT NULL,
    segno        BIGINT   NOT NULL,
    blknum       BIGINT   NOT NULL,
    is_image     BOOLEAN  NOT NULL,
    lsn          BIGINT   NOT NULL,
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((branch_id) HASH,
                 kind ASC, segno ASC, blknum ASC, lsn DESC, is_image DESC)
);
```

### `amp_storage.meta`

Metadata blobs — everything that is neither a relation page nor an SLRU
block: relsize sentinels, per-relation reldir markers, directory listings
(TenantDir / DbDir / SlruDir), aux files (`yb_amp.signal`, `postgresql.auto.conf`,
replication-slot state), two-phase commit files, replication origins,
SLRU segment-size sentinels. Full-overwrite semantics: a read is the
latest row at LSN ≤ target — no walredo, no chains.

Like `slru_pages`, the hash partition is `branch_id` alone — sentinels
and directory blobs are small, branch-drop GC is one key-bound DELETE,
and `key` as a range column makes multi-key scans (`scan_meta_range`)
contiguous.

```sql
CREATE TABLE amp_storage.meta (
    branch_id    uuid     NOT NULL,
    key          BYTEA    NOT NULL,
    lsn          BIGINT   NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((branch_id) HASH, key ASC, lsn DESC)
);
```

### `amp_storage.branch_state`

One row per branch, **shared by the pageserver and the safekeeper**. Each
daemon owns a group of columns and writes only its own group; the other group
is NULL until its owner fills it in.

The row is created by the **controller**, at branch-create time
(`Store.RecordBranch`), before any pageserver, safekeeper, or endpoint
supervisor for the branch starts. Neither daemon ever creates it: both validate
that it exists and UPDATE their own columns. A daemon that finds no row fails
loudly — it means it started against a branch the controller never provisioned,
or one whose data has already been reclaimed.

The controller also **deletes** it, as the first step of reclaiming a dropped
branch (`controller/internal/branchgc`): it is the row that pins an ancestor's GC
cutoff at this branch's fork point, so removing it is what lets the parent's own
GC advance again, and a pageserver that somehow outlived its branch then finds no
row for its generation-fenced writes to match and fences itself out.

Because the row is shared, **its presence is not a per-daemon signal**. What
tells a daemon whether *its own* state exists is whether its group is
populated:

| Daemon | Presence predicate | Decides |
|---|---|---|
| pageserver | `pageserver_initialized` | bootstrap (initdb + import) vs load |
| safekeeper | `safekeeper_initialized` | create control file vs restore it |

Each flag is a plain `BOOLEAN NOT NULL DEFAULT false`, set by exactly one
statement — that daemon's creation write — in the same statement that fills its
group. So the flag and the group can't disagree, and "my state exists" is
recorded rather than inferred. The creation writes stay idempotent: the
safekeeper's is scoped `AND NOT safekeeper_initialized`, and the pageserver's
writes `consistent_lsn` and `prev_record_lsn` as `COALESCE(col, $n)` — the two
columns the flush path also writes, so whichever lands first wins. Its other
columns have no second writer before creation and are assigned directly.

### Why a data column can't be the discriminator

The flags replaced `consistent_lsn IS NOT NULL` and `timeline_start_lsn IS NOT
NULL`. That trick is only sound while the chosen column has exactly one writer,
and the pageserver's did not — so **on the bootstrap path its creation write
never landed**:

1. `claim_generation` (`mgr.rs`, before the branch is opened) set
   `current_generation` on the controller-seeded row, satisfying the generation
   fence even though the rest of the group was NULL. (That fence is gone — see
   [`compute-fencing.md`](compute-fencing.md) — but the shape of the bug below
   is why the flags exist, so the sequence is kept as it was.)
2. Bootstrap imports the initdb datadir. `DatadirModification::commit` calls
   `finish_write` → `note_record_lsn`, so `record_lsn_history` is non-empty.
3. `UninitializedTimeline::write` spawns the flush loop and flushes **before**
   `finish_creation`. That flush sees a non-empty ingest frontier and calls
   `advance_consistent_lsn`, which was fenced only on `current_generation` — it
   matched, and **set `consistent_lsn` alone**.
4. `finish_creation` → `write_timeline_row_to_yb` → `update_branch_initial_state`
   was `WHERE consistent_lsn IS NULL`, by then false. It matched zero rows, the
   existence probe passed, and it **silently no-oped**.

A freshly bootstrapped branch therefore carried `consistent_lsn` and the
generation and nothing else: `applied_gc_cutoff_lsn` and
`initdb_lsn` stayed NULL (#552). `enable_gc` defaults to
false, so no GC tick stamped the cutoff afterwards either — only the integration
harness, which sets `enable_gc = true`, converged the row, which is why CI never
saw it. Observed on a live cluster immediately after bootstrap:
`consistent_lsn = 22469800` with all four of those columns NULL.

Reads were mis-classified too. A crash between step 3 and step 4 left
`consistent_lsn` set on a branch whose import never finished, so the next attach
took the *load* path over a half-imported datadir. The flag is false until the
import completes, so that retry now re-bootstraps correctly.

The safekeeper's group has a single writer, so its version of the trick happened
to hold. The one write that could have put a value into the group before the
creation write ran — the unscoped advisory `remote_consistent_lsn` UPDATE, the
same shape one step short of the pageserver's bug — no longer exists: the
safekeeper does not persist a retention floor at all, and reads the
pageserver's `consistent_lsn` instead.

The flag columns are declared in `0003` rather than added by a follow-on
migration, so **a database provisioned before them has to be recreated**
(`yb_amp_local init --force remove-all-contents` locally; the GKE deploy
rebuilds `yb_storage`). `golang-migrate` records `0003` as applied and never
re-runs it, so an existing database would otherwise be left without the columns
and every daemon would fail on its first query.

**A flag is still not a non-NULL guarantee for every column**, and readers keep
deserializing the group as nullable. Reading `applied_gc_cutoff_lsn` as a plain
`i64` made a NULL a fatal deserialize error during attach — and because attach
is retried forever, a pageserver crash-loop (#531). The flag plus the
`COALESCE` creation write means a freshly bootstrapped branch now gets every
column, so that is defence in depth rather than a live requirement; keep it,
because the cost is one `unwrap_or(0)` and the failure mode it guards is a
crash-loop. Widening a predicate is the wrong repair for a NULL either way: it
would hide a branch with live data behind the *bootstrap* path.

The pageserver group is the page store's durable index record: consistent /
GC-cutoff / initdb LSNs, and ancestry. There is no fencing state here: the
endpoint's write lease fences every process in the endpoint before it writes,
not one service's columns after the fact (see
[`compute-fencing.md`](compute-fencing.md)).

The safekeeper group is its control file. Hot-update LSN scalars get their own
BIGINT columns so a control-file save is a column-scoped UPDATE rather than a
full-row rewrite. yb-amp runs a single safekeeper per branch with WAL durability
provided by YB itself, so there is no consensus state (term / term history) and
no membership configuration to persist.

`pg_version` is the one shared column, and it holds a full `PgVersionId`
(e.g. 180002) rather than a major version. What makes a single shared column
safe is that it has exactly one writer: the controller seeds it at branch create
with the full id of the PostgreSQL amp ships (180002 for PG 18; the init recipe
carries only the major version), and nothing writes it again.

Both daemons only read it. The pageserver divides by 10000 for the major version
it needs. The safekeeper loads it into `ServerInfo` and checks it against every
walproposer greeting — a check on the *major* version only, so the seeded value
needs no later refinement.

```sql
CREATE TABLE amp_storage.branch_state (
    branch_id              uuid        NOT NULL,
    pg_version             INT         NOT NULL,

    -- per-daemon "my group exists" flags; each set only by that daemon's
    -- creation write, in the same statement that fills its group
    pageserver_initialized BOOLEAN     NOT NULL DEFAULT false,
    safekeeper_initialized BOOLEAN     NOT NULL DEFAULT false,

    -- pageserver-owned; NULL until the pageserver creates its state
    consistent_lsn         BIGINT,
    applied_gc_cutoff_lsn  BIGINT,
    ancestor_branch_id     uuid,
    ancestor_lsn           BIGINT,
    initdb_lsn             BIGINT,
    prev_record_lsn        BIGINT,

    -- safekeeper-owned; NULL until the safekeeper creates its control file
    timeline_start_lsn     BIGINT,
    system_id              BIGINT,
    wal_seg_size           INT,
    creation_ts            TIMESTAMPTZ,

    PRIMARY KEY ((branch_id) HASH)
);
```

### `amp_storage.wals`

The durable copy of the WAL stream. The safekeeper keeps no WAL on
local disk: every WAL write lands here, and reads are served from here
(fronted by an in-memory hot cache for recent WAL). A commit is acked
to the compute only once its bytes are in this table.

```sql
CREATE TABLE amp_storage.wals (
    bucket_id        SMALLINT NOT NULL,  -- per-batch placement bucket, 0..5
    branch_id        uuid     NOT NULL,
    segno            BIGINT   NOT NULL,
    chunk_offset     BIGINT   NOT NULL,
    chunk_size       INT      NOT NULL,
    data             BYTEA    NOT NULL,
    last_modified_us BIGINT   NOT NULL,
    CONSTRAINT wals_bucket_id_in_range CHECK (bucket_id >= 0 AND bucket_id < 6),
    PRIMARY KEY (bucket_id ASC, branch_id ASC, segno ASC, chunk_offset ASC)
) SPLIT AT VALUES ((1), (2), (3), (4), (5));
```

All-range key led by `bucket_id`, pre-split into `WALS_BUCKETS` tablets.
Unlike `pages`, this bucket is **not** derivable from the row — it is a
rotating per-batch counter — so readers always fan out across all of them,
and any scan must page through empty-page-plus-continuation responses from
tablets holding no match. See [`safekeeper.md`](safekeeper.md) for why
`bucket_id` leads rather than `branch_id`.

`data` holds a compression envelope, not bare WAL bytes — a codec tag
byte followed by (by default) an LZ4 block, encoded and decoded by the
Rust layer. `chunk_size` stays the **logical** (decoded) length, because
`chunk_offset + chunk_size` is LSN-space arithmetic; so
`octet_length(data)` is *not* `chunk_size`, and `substring(data, …)`
slices envelope bytes. Use `dump-wals`, which decodes client-side.
See [`safekeeper.md`](safekeeper.md) "`data` is compressed" for the
`yb_wal_compression` knob and why the whole coalesced run is compressed
rather than each chunk.

Each row is one coalesced run of a consumer batch: contiguous chunks of
a segment are concatenated before the write, so the typical batch is
one row. The writer never produces overlapping rows (`truncate_wal`
aligns the next write at exactly the truncation point), so
`(segno, chunk_offset)` is a chunk's natural identity within the
branch and a same-PK INSERT is necessarily an idempotent retry of the
same bytes.

`bucket_id` exists purely for tablet placement: as the leading
range-key column it spreads a branch's WAL across all
`WALS_BUCKETS = 6` pre-split tablets. The consumer stamps each write
**batch** with one bucket, rotating through the buckets one batch at a
time — each batch lands on ONE tablet (the direct-RPC lane applies it as
one atomic write batch; the YSQL lane's multi-VALUES INSERT hits the
single-shard transaction fast path), while sustained load rotates
across all tablets. Readers cannot derive a chunk's bucket, so
index-tight reads name every bucket with a `bucket_id IN (0..5)`
key list — YSQL executes that as batched per-bucket index probes,
not a scan.

Schema and batching documented in detail in
[`safekeeper.md`](safekeeper.md).

### Useful diagnostic queries

```sql
-- Per-segment summary for one branch
SELECT segno, count(*) AS chunks,
       max(chunk_offset + chunk_size) AS end_offset,
       pg_size_pretty(sum(chunk_size)::bigint) AS bytes
FROM amp_storage.wals
WHERE branch_id = '...'
GROUP BY segno
ORDER BY segno;
```

(`dump-wals --summary` renders the same view with gap detection —
see [`../README.md`](../README.md) §"Operator tools".)

### `amp_storage.pending_gc`

GC candidate queue (pageserver.md §GC): one row per garbage-producing
event, inserted when the event is applied, drained once the branch's GC
cutoff passes `due_lsn`. `gc_kind` tags the operation (trim / shrink /
drop, one variant per target table — `page_store::GcKind`); `key` is the
18-byte object `Key` (a block for trim, an object sentinel for
drop/shrink), and `relsize` is a shrink's truncation target (`NULL`
otherwise). The PK columns `(branch_id, bucket_id, due_lsn, key,
gc_kind)` uniquely identify a candidate — `relsize` is the only non-PK
payload — so a candidate re-inserted by crash + WAL replay (or a chunk
retry) upserts onto the same PK with an identical `relsize`; no `ON
CONFLICT` needed under `yb_enable_upsert_mode`.

```sql
CREATE TABLE amp_storage.pending_gc (
    branch_id  uuid     NOT NULL,
    bucket_id  INT      NOT NULL,  -- writer-computed: page_store::gc_bucket_id(key) % GC_BUCKETS
    due_lsn    BIGINT   NOT NULL,
    key        BYTEA    NOT NULL,  -- encoded 18-byte Key: block (trim) or object (drop/shrink)
    gc_kind    SMALLINT NOT NULL,  -- page_store::GcKind (0..=6; trim/shrink/drop × table)
    relsize    BIGINT,             -- shrink's truncation target; NULL for trim/drop
    PRIMARY KEY ((branch_id, bucket_id) HASH,
                 due_lsn, key, gc_kind)
) SPLIT INTO 3 TABLETS;
```

`bucket_id` is placement only, never identity: it spreads a branch's
candidate inserts across tablets instead of hot-spotting the single
`(branch_id)`-hashed tablet. The **writer computes it** — a fixed,
deterministic FNV-1a hash of the 18-byte `key` mod `GC_BUCKETS`
(`pageserver::page_store::gc_bucket_id`) — and supplies it in the
INSERT. It is deliberately **not** a `GENERATED ALWAYS AS
(yb_hash_code(...) % N)` column: the planned yb-client write path
bypasses the YSQL query layer, which would never evaluate a generated
expression, so the bucket must be client-supplied. Determinism (same
key → same bucket) is what lets a crash-replayed candidate land on
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

## `amp_registry` schema

The controller's storage-layer bookkeeping: the branch and endpoint
bookkeeping behind the HTTP surface the controller serves to
`yb_amp_local` and the integration harness.

There is exactly **one** `branches` table and **one** `endpoints` table,
and they are the product model's — the storage layer's flat view is a
rendering of the same rows, not a second registry. The controller's
connection pool pins `search_path` here, so these are the unqualified
tables; the task log (`controller.*`) and the data plane
(`amp_storage.*`) are schema-qualified at the statement.

| Table | Purpose |
|---|---|
| `amp_registry.users`, `storage_pools`, `clusters`, `user_pool_grants`, `user_cluster_grants` | The resource ladder above a branch. `clusters` is tombstoned (`dropped_at`) rather than deleted, because its branches are. |
| `amp_registry.branches` | One row per branch, keyed by `branch_id` alone — it is the storage layer's globally-unique `BranchId`, and branches are not sharded across pageservers (YB tablet-shards the keyspace itself), so nothing needs a composite key. `cluster_id` is an ordinary scoping column, indexed by `branches_by_cluster`. Carries the branch's *creation record* (`name`, `pg_version`, `parent_branch_id`, `fork_lsn`), written at branch create and read back by the launcher to materialize the pageserver + safekeeper pair (NULL parent = bootstrap, non-NULL = fork that ancestor at `fork_lsn`). Also holds `extensions` (a JSONB array): the branch's opted-in externalized-extension set, seeded from the ancestor on fork by `RecordBranch` and unioned by the create_endpoint opt-in, so forks and read replicas inherit it (compute/architecture/extensions.md). Every `branch_id` column in `amp_storage` holds this same `uuid`. No fencing state lives here — write fencing is a lease on the branch in `amp_registry.resource_token` (compute/architecture/compute-fencing.md), held by whichever endpoint is writing it. A dropped branch is tombstoned via `dropped_at`, never deleted; the name and default-branch uniqueness indexes are partial on that, so a drop frees the name. The reaper then stamps `reap_after` (`dropped_at + --branch-gc-retention`), the deadline after which the controller's branch-drop reaper deletes the branch's rows from every table below and stamps `gc_completed_at` — the row itself still stays, because a branch id is never reused (`controller/internal/branchgc`, and controller/architecture/sc-integration.md § Branch-drop GC). The `branches_gc_due` partial index over those two columns is that reaper's due-scan. |
| `amp_registry.endpoints` | One row per compute endpoint, keyed by `id` and belonging to a branch — there is no `cluster_id` column, so an endpoint and its branch can never disagree about the cluster. Every endpoint is created the same way, through `/api/v1`, whether the instance it names is a pod the controller deploys or a local process `yb_amp_local` spawns (`--deploy-driver=local`); the only difference is that a local compute has no controller-assigned `address`. `endpoints_one_writer_per_branch` applies to both. |

Semantics are documented in
[`../../controller/architecture/sc-integration.md`](../../controller/architecture/sc-integration.md).

## `controller` schema

The write-ahead task framework's `tasks` / `task_steps` tables (plus
their status enums) that drive the create/delete workflows — the
controller's own operational machinery, not a user resource, which is why
it sits outside the resource model. Unlike `amp_registry` this schema name
is fixed, not configurable (`db.TaskSchema`), the same way `amp_storage`
is. Provisioned first (`0001_task_log`) so a fresh database can record
work from the moment the controller starts; managed entirely by that
migration, so do **not** edit by hand. Inspect with:

```sql
\c yb_storage
SET search_path = amp_registry, public;
\dt
\dt controller.*
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
retries, leaving holes in `amp_storage.wals` that wedge pageserver
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
(`42xxx` and friends) are fatal immediately, as is `57014` — the server
cancelling a statement that outran `statement_timeout`, where retrying
would spend the budget to hit the same wall.

The budget bounds *attempts*, not *duration*: nothing here fires against
a server that accepts a statement and never answers, because no attempt
ever fails. `connect_timeout_ms` / `statement_timeout_ms` above are what
bound the clock.

**The catch**: the connection-drop arm reconnects without any way to
know whether the pre-drop statement committed before the socket died.
The reconnect arm replays anyway. **This is only safe if the statement
is idempotent — re-executing it must produce the same end state whether
the first attempt applied, half-applied, or did nothing.**

Nearly every YB-talking call site in this codebase satisfies that contract:

- **All reads** — `SELECT` via `query` / `query_opt` / `query_raw` —
  are intrinsically idempotent.
- **All writes** are one of: `INSERT ... ON CONFLICT (...) DO NOTHING`
  / `DO UPDATE`; plain `INSERT` running under
  [`yb_enable_upsert_mode = true`](#yb_enable_upsert_mode) (same
  semantics — the conflict path is a no-op upsert; this covers the
  `pages` / `slru_pages` / `meta` / `wals` / `blobs` write paths); or an
  absolute `UPDATE amp_storage.branch_state SET ... WHERE branch_id =
  $1` (a replay writes the same row state, so it is a no-op).
- **DELETEs** are key-scoped (`DELETE FROM ... WHERE id = $1`); replay
  against an already-deleted row is a no-op.

There is no exception: every production write is one of the shapes
above, so a replay lands the same row state.

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
  server-confirmed no-ops). No production path needs this today. The
  case that does want it is bare DDL, where a replay raises `42P07`
  rather than doing something harmless twice — see #508.

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
- YB tserver RPC: `127.0.0.1:9100` (the `yb_thin_client` hot path)
- YB tserver web UI: `127.0.0.1:9000`
- `yugabyted-ui`: `127.0.0.1:15433`

Started via `yugabyted start --base_dir=<dir> --advertise_address=127.0.0.1`.
