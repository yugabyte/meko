# yb-pageserver

The pageserver materialises the WAL stream into pages and serves them
to compute Postgres. Its storage model is **one YugabyteDB row per page
version**: every page write becomes a row in `pageserver.pages` (or its
SLRU / metadata siblings) inside the shared `yb_storage` database.

**YugabyteDB is the system's durable store.** The pageserver holds only
transient in-RAM state (a bounded write buffer and a read cache); the
pageserver and the safekeeper both write to YugabyteDB, and it is
YugabyteDB — Raft-replicated DocDB tablets — that provides durability
and resilience. If a pageserver process dies, everything it needs to
resume is in YB: the page rows, the SLRU rows, the metadata rows, and a
single `pageserver.timeline_state` row per branch. Local disk is
scratch space only (initdb tarball, basebackup cache, pid file) and is
never authoritative.

The crate is `compute/pageserver/`; the shipped binary is named
`yb-pageserver` so the storage backend is obvious in `ps`, logs, and
deployments. Build with `cargo build -p pageserver --bin yb-pageserver`.

## Single-branch process model

One yb-pageserver process serves exactly **one `branch_id` for its
entire lifetime**. The identity is baked into `pageserver.toml` by the
launcher before spawn:

```toml
branch_id   = "77ffde89..."   # required
generation  = 7               # required; launcher bumps on EVERY spawn

[timeline_init]               # consulted only when the timeline_state row is absent
mode       = "bootstrap"      # or "branch" + ancestor_branch_id + ancestor_lsn
pg_version = 18
```

`PageServerConf::parse_and_validate` (`compute/pageserver/src/config.rs`)
rejects `branch_id` without `generation` (and vice versa), and rejects a
`[timeline_init]` block without an identity. `main()` also refuses to
start on any unknown key in `pageserver.toml` — the config is
launcher-generated against the same build, so a misspelled key is an
operator mistake worth failing loudly on.

Startup is **open-or-create** and runs SYNCHRONOUSLY in `main()` before
the HTTP/libpq/gRPC listeners accept traffic
(`mgr::init_tenant_mgr` → `PageServer::open` → `preload` + `attach` +
`activate`). A connecting compute either gets ECONNREFUSED (process
still opening; the compute's reconnect loop covers it) or talks to a
fully-active branch. There is no branch state machine and no
in-protocol wait-for-active. Failure to open is fatal to the process.

- Row exists in `pageserver.timeline_state` → open it (restarts are
  trivially idempotent; `[timeline_init]` is ignored). A row with
  `deleted_at` set refuses to open.
- Row absent + `mode = "bootstrap"` → run initdb + import, then insert
  the row (`UninitializedTimeline::finish_creation`). If the initdb tar
  for this branch already exists in remote storage it is REUSED —
  initdb output is not bit-deterministic, and a crash-retry must
  overwrite any orphan page rows with identical payloads.
- Row absent + `mode = "branch"` → validate against the ancestor's
  persisted row only (`PageStore::create_branch_row`: ancestor not
  deleted/archived, branch point not below the ancestor's own branch
  point, branch point durable — `anc.consistent_lsn >= ancestor_lsn` —
  and not below the ancestor's GC cutoff) and insert a row with
  `consistent_lsn = ancestor_lsn` (the walreceiver resume point — never
  0). The durability check is retried every 500 ms for up to 60 s,
  because the launcher picks the branch LSN from the ancestor's ingest
  position, which can run ahead of the ancestor's durable
  `consistent_lsn` by up to one flush interval. No in-process ancestor
  object exists; reads reach ancestor data through the lineage walk
  (below).
- Row absent + no `[timeline_init]` → fatal.

Attach also pushes the launcher-issued generation into YB via
`PageStore::cas_bump_generation_on_attach` (see "Generation fencing").
Inbound page-service requests (libpq query string / gRPC metadata) are
validated against the configured identity and rejected as misrouted on
mismatch (`MISROUTED_PAGESTREAM_REQUESTS`).

The management HTTP surface is small: status / metrics / profiling /
utilization, the single branch's list + detail,
`get_lsn_by_timestamp`, `get_timestamp_of_lsn`, `lsn_lease`, `do_gc`,
`flush` (`PUT /v1/branch/:branch_id/flush?lsn=` — waits for ingest to
reach `lsn`, flushes pages to YB, then forces the durable
`consistent_lsn` UPDATE through immediately, bypassing the per-`max_age`
throttle; the launcher calls it on the ancestor at branch time so the
new branch's `create_branch_row` durability barrier clears in ~0.1 s
instead of a full flush interval), and test plumbing (failpoints,
panic, tracing/event, reload_auth_validation_keys). The full route
inventory is in [`pageserver-apis.md`](pageserver-apis.md).

## Connections to YugabyteDB

`bin/yb-pageserver.rs` opens two `PageStore` handles at startup, each
with its own YSQL connection pool, so compute-facing reads never queue
behind internal writes:

- **`compute_page_store`** (`application_name =
  "yb-pageserver:compute_reads"`, pool of 4 × `scale_factor`) — the
  GetPage hot path (`PageWriter::scan_range` / `read_chain`). Compute
  backends issue parallel prefetches, and each lands on its own YSQL
  connection instead of serialising on one backend.
- **`fpu_page_store`** (`application_name = "yb-pageserver:fpu"`, pool
  of 4 × `scale_factor`) — walreceiver flushes, the materialiser's
  durable chain reads, and the materialiser's image INSERTs. Each
  connection runs `SET yb_enable_upsert_mode = true`
  (`YsqlClient`, `compute/libs/ysql_client/`), so a chunk retried after
  a partial-flush failure upserts instead of PK-conflicting.

With `hot_path = "tsclient"` in the shared connection block — **the
default** (`YbConnConfig::hot_path`, `HotPath::TsClient`) — `PageStore`
additionally opens pure-Rust thin-RPC table handles
(`PageStore::tsclient`, `libs/yb_tsclient` — pggate RPC to a tserver's
RPC port, no native library, routing stays server-side) to
`pages`/`slru_pages`/`meta` at startup with a fail-fast health check.
Set `hot_path = "ysql"` to run everything over the Postgres wire.

## Schema

Four tables live in the `pageserver` schema inside `yb_storage`:
`pages` (relation pages), `slru_pages` (SLRU blocks), `meta`
(everything else), and `timeline_state` (one state row per branch).
DDL is owned by the migration
`controller/migrations/0002_compute_storage.up.sql`; the daemons
never create their own schema. Per-branch tuning is NOT in YB: the
effective config comes from `pageserver.toml`. See
[`yugabyte.md`](yugabyte.md) for the full DDL.

### `pageserver.pages`

One row per (Key, LSN). The PK hash-partitions on the **(branch_id,
spcnode, dbnode, relnode, forknum)** identity (a relation forknum's
whole block-set lives on one tablet), then clusters by
`(blknum, lsn DESC, is_image DESC)` so a per-block scan walks rows in
LSN-descending order; `is_image` is DESC so the stored order matches
the chain-read `ORDER BY lsn DESC, is_image DESC` (image before delta
at a tie).

```sql
CREATE TABLE pageserver.pages (
    branch_id    TEXT     NOT NULL,
    spcnode      BIGINT   NOT NULL,
    dbnode       BIGINT   NOT NULL,
    relnode      BIGINT   NOT NULL,
    forknum      SMALLINT NOT NULL,
    blknum       BIGINT   NOT NULL,
    is_image     BOOLEAN  NOT NULL,
    lsn          BIGINT   NOT NULL,    -- u64 stored as i64 (real LSNs are << 2^63)
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((branch_id, spcnode, dbnode, relnode, forknum) HASH,
                 blknum, lsn DESC, is_image DESC)
);
```

Only **relation-page keys** live here — keys where
`Key::is_rel_block_key()` holds (`field1 == 0x00`, `field4 != 0`,
`field6 != 0xffffffff`); `PageStore::put_pages` rejects anything else.
`is_image` says whether the payload is an 8 KiB Postgres page (vs. a
serialised `NeonWalRecord`). `will_init` mirrors `Value::will_init()` —
true for every image and for records that fully initialise the page
(e.g. `XLOG_HEAP_INIT_PAGE`); it is the read side's chain-anchor
marker.

### `pageserver.slru_pages`

SLRU blocks (pg_xact, pg_multixact offsets/members). `kind` is 0=Clog,
1=MultiXactMembers, 2=MultiXactOffsets (`slru_kind_to_i16`,
`page_store.rs`). PK: `((branch_id, kind, segno) HASH, blknum,
lsn DESC, is_image DESC)` — a whole SLRU segment lives on one tablet.
Same image/delta chain semantics as `pages`.

### `pageserver.meta`

Everything that is neither a relation page nor an SLRU block: rel-size
sentinels, directory listings, aux files (`yb_amp.signal`,
`postgresql.auto.conf`, replication-slot state), two-phase-commit
files, replication origins, SLRU segment-size sentinels. `key` is the
18-byte BYTEA Key encoding; PK `((branch_id, key) HASH, lsn DESC)`.
**Full-overwrite semantics**: a read returns the single latest row at
LSN ≤ target — no chains, no walredo.

### `pageserver.timeline_state`

One row per branch carries every load-bearing piece of per-branch
state:

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

- `consistent_lsn` — the durability *frontier*: every row at lsn ≤ this
  is durably present in `pageserver.pages`/`slru_pages`/`meta`. It
  advances on successful `PageWriter::flush` to the **ingest
  frontier** — the highest record `finish_write` had noted *before* the
  flush snapshot was taken (a noted record's rows are all buffered by
  then, so the snapshot provably contains every unflushed row ≤
  frontier). It is deliberately NOT max(lsn) of the flushed rows: the
  snapshot races concurrent puts and can hold a row at lsn X while
  missing a sibling at lsn < X, so a max-derived checkpoint would
  silently lose records across a crash (the same frontier rule governs
  `flush_horizon_lsn`, the materialiser's durable-read bound). It
  drives safekeeper WAL truncation and ingest resume on restart.
  `last_record_lsn` is intentionally not persisted — on restart the
  walreceiver streams from `consistent_lsn` forward and the in-memory
  value re-derives within seconds.
- `applied_gc_cutoff_lsn` — reads below this LSN are not supported.
- `current_generation` — bumped by the launcher/controller on every
  attach; the fence column (see "Generation fencing").
- `prev_record_lsn` — the `xl_prev` pointer paired with
  `consistent_lsn`, persisted best-effort so restart can hand the
  safekeeper the right value; `COALESCE` on update keeps a good value
  when a flush window has no fresh pairing.
- `lineage` (JSONB) — detach-ancestor bookkeeping, always `Default` in
  yb-amp; the ancestry chain is derived from `ancestor_branch_id` /
  `ancestor_lsn`, never from this column.

## Read path: image-anchored chain reads

`Timeline::get_vectored_impl` queries the row store via `PageWriter`
(`page_server/timeline.rs`, `get_vectored_via_page_writer`):

1. Classify each requested key range by its start key: relation pages →
   `pages`, SLRU blocks → `slru_pages`, everything else → `meta`
   (payload returned directly, no walredo).
2. Scattered single-block relation reads are bucketed by
   (spcnode, dbnode, relnode, forknum); a bucket of ≥2 keys is fetched
   in ONE multi-key query (`scan_scattered_set`, `unnest`-driven) —
   one YB round trip instead of a per-block loop. Contiguous ranges use
   `scan_range`; a lone single block (the getpage hot path) dispatches
   to `read_chain`.
3. Each per-key chain comes back LSN-descending, pre-truncated at the
   first `will_init=true` row. The bottom row is the anchor — image →
   walredo base, `will_init` delta → first record.
4. `chain_to_value_reconstruct_state` converts the chain and
   `Timeline::reconstruct_value` drives walredo, fanned out across keys
   via `FuturesUnordered`.

Reads above the applied ingest frontier block on `wait_lsn` (bounded by
`wait_lsn_timeout`, default 300 s); reads below `applied_gc_cutoff_lsn`
return an error.

### The SQL shape

`PageStore::read_chain` (single-key path) uses a scalar-subquery anchor
with `COALESCE(..., 0)` as the lower bound so both ends of the LSN
range push into a single DocDB scan:

```sql
SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum,
       p.lsn, p.is_image, p.will_init, p.payload
FROM pageserver.pages p
WHERE <key_filter>
  AND p.lsn <= $target
  AND p.lsn >= COALESCE((
        SELECT a.lsn FROM pageserver.pages a
        WHERE <key_filter>
          AND a.is_image = TRUE AND a.lsn <= $target
        ORDER BY a.lsn DESC LIMIT 1), 0)
ORDER BY p.lsn DESC, p.is_image DESC;
```

With `is_image` in the clustering key the anchor seek is `O(log B)`
where `B` is the number of image rows for that block — typically 1. If
no image exists the subquery returns NULL and `COALESCE(..., 0)` turns
the lower bound into `p.lsn >= 0` — a plain LSN-descending scan of the
whole chain; the Rust-side trim stops at the first `will_init=true`
row, so both cases shorten correctly. The no-image case is the only
slow path and only fires for cold blocks the background image
materialiser has not visited yet.

Multi-block scans (`build_scan_one_fork_sql`,
`build_scan_one_fork_scattered_sql`, `build_scan_cross_fork_sql`) wrap
the same per-block anchor in a `CROSS JOIN LATERAL` driven by either a
`SELECT DISTINCT blknum` CTE (contiguous range) or
`unnest($6::bigint[])` (scattered set), so each block gets the pushdown
individually. The cross-forknum variant keys the LATERAL on the full
(spcnode, dbnode, relnode, forknum, blknum) tuple; it scatters across
hash tablets and is rare in practice.

On the tsclient lane (the default), the single-key readers —
`read_chain`, `read_slru_chain`, `read_meta` — run as paged clustered
scans from the target LSN downward that stop at the anchor client-side
(`ChainFetchState`); `normalize_chain` re-orders equal-LSN image/delta
ties into the SQL result order (the PK's trailing `is_image` clusters
ASC on the storage side, the SQL readers order it DESC). One-forknum
multi-block scans run as a single clustered range scan with per-block
anchor-stop collection (`BlockChainCollector`); the tsclient paging
envelope pins every continuation to the first page's snapshot.
Cross-forknum `scan_range` and multi-key `scan_meta_range` always stay
on YSQL — they scatter across hash buckets and are not hot. Per-op
latency is split by lane in `pageserver_yb_store_op_seconds{op,path}`.

### Chain invariants

Every chain is validated as it is assembled (`assert_chain_step`):
LSN non-increasing, image-before-delta on ties, nothing below a
`will_init` anchor. Blends of cache + YB rows are deduplicated by LSN
(`dedup_page_chain_by_lsn`); two same-LSN collisions are legitimate (a
materialised image stamped at the LSN of the delta it folded, or the
same flushed delta seen in both cache and YB), but two *distinct*
non-image payloads at one LSN mean a real record is being dropped —
that trips `correctness_errors{chain_dedup_dropped_delta}` plus a
detailed rate-limited log with backtrace, because the symptom
downstream is a walredo PANIC.

## Lineage-aware reads (branches)

A branch's pre-branch-point rows live in YB under the **ancestor's**
`branch_id`; nothing is copied at branch time (pages, SLRU, and meta
alike). At startup the pageserver walks the
`ancestor_branch_id`/`ancestor_lsn` links in `timeline_state` once
(`PageStore::load_lineage`; cycle-checked, depth-capped at 64) and
builds an ordered segment list
`[(self, MAX), (parent, cap), (grandparent, min(caps)), …]` — static
for the process lifetime, since ancestry never changes for a live
branch. Every PageStore read path (`read_chain`, `scan_range`,
`scan_scattered_set`, `read_slru_chain`, `scan_slru_range`,
`scan_meta_range`) has a `*_lineage` variant that walks the segments
nearest-first, capping each segment's read LSN at
`min(target_lsn, cap)`. The walk stops only at an anchor (`will_init`)
whose LSN is **at or above the next segment's cap** (for meta: once the
key is found — nearest segment wins). An anchor *below* the cap — e.g.
a materialiser copy-up image — does not end the walk: ancestor deltas
in `(anchor_lsn, cap]` still apply and must be layered on top.

Iterative per-segment queries (not one UNION ALL) on purpose:
`branch_id` is in the hash partition key, so segments live on different
tablets. The walk touches ONE tablet in the steady state — always for
unbranched branches, and per-key once the materialiser copies an anchor
image up into the descendant's own branch_id. A chain that crossed a
segment boundary force-dispatches a read-side materialise job
(`chain_crossed_segment`), so each cold branch key converges to
tablet-local reads after one slow read. Writes (`put_*`, materialiser
installs, `advance_consistent_lsn`) always
target the own branch_id, never an ancestor.

**Boundary.** The per-segment `branch_id` binding (the read SQL's `$1`,
rebound for each ancestor — never baked as a literal; see the
`read_chain` comment in `page_store.rs`) plus the `lineage` vector are
the one place reads are genuinely multi-branch. Writes are
single-branch — write SQL bakes `branch_id` in as a literal and is
memoized per branch (`BakedSql`). A simplification that hard-codes the
write branch_id on the read path, or bakes it into the read SQL, would
silently break branch reads — keep the lineage walk and its per-segment
binding intact. `PageWriter` carries the lineage everywhere, and its
cache fast paths only short-circuit when the cached anchor covers the
lineage (`cache_anchor_covers_lineage`: anchor at or above the first
ancestor cap).

## Write path: PageWriter buffering + flush

The write path is a pure passthrough — no walredo at write time, no
image synthesis, just serialise-and-buffer (`page_writer.rs`):

1. `walingest` produces `(Key, LSN, Value)` triples via
   `pgdatadir_mapping::DatadirModification.put`.
2. `DatadirModification.commit` calls `TimelineWriter::put_batch`,
   which forwards the serialized batch to `PageWriter::put_serialized`.
   With `ingest_parallelism > 1` and a batch of at least
   `ingest_parallelism_min_batch` entries, the deserialize + row-build
   work is partitioned by page key across blocking workers; the same
   key always lands on one worker (chain order and materialiser tallies
   stay race-free), and `applied_lsn` advances only after the whole
   batch lands, so a partially-applied batch is never reader-visible.
3. Each entry is routed by key type: relation-page keys → `PageRow`
   (`Value::Image` → `is_image=true, will_init=true`;
   `Value::WalRecord` → `is_image=false` with `will_init` from the
   record); SLRU block keys → `SlruPageRow`; everything else →
   `MetaRow` (full-overwrite payload).
4. Relation rows land in `UnifiedPages` (`unified_pages.rs`) — one
   in-RAM structure that is both the pre-flush buffer (rows pinned
   until durable) and the post-flush read cache. Per-key state lives in
   a lock-free ordered `SkipMap<Key, Mutex<KeyState>>` keyed on the
   `Copy` fixed-size `Key` (no per-row heap alloc, integer-compare
   ordering); the skiplist's ordered `range()` backs `lookup_range`.
   Eviction is per-key and whole-chain (LRU order, keys with unflushed
   rows pinned), so a cache hit either has the complete chain for a key
   or nothing — never a partial hole; that is what makes the cache-only
   fast paths (`lookup_chain_if_complete`) sound. Aggregate counters
   are atomics; concurrent getpage reads go straight to per-key mutexes
   and never serialise on a global lock.
5. SLRU and meta rows sit in simpler `Vec` buffers behind a small
   `parking_lot::Mutex<AuxBuffers>`. Meta writes also populate a
   write-through per-key LRU (`meta_cache`, default 8192 entries) that
   serves single-key `scan_meta_range` hits without a YB round trip.
6. **Flush** (`PageWriter::flush`, serialised end-to-end by
   `flush_serialize` so the aux snapshot and its count-based drain see
   the same buffer): capture the ingest frontier, snapshot the
   unflushed prefix of `UnifiedPages` (an `Arc` bump per row, no data
   copy — rows stay visible to concurrent reads during the YB round
   trip) plus the SLRU/meta buffers, then dispatch chunked multi-row
   INSERTs against `pageserver.pages` / `slru_pages` / `meta` on the
   fpu store. Chunks are 500 rows for pages/SLRU (`ROWS_PER_INSERT`;
   bind-parameter budget with `branch_id` baked as a literal) and 2000
   for meta, dispatched in parallel via
   `stream::iter(...).buffer_unordered(N)` with base N = 4/2/3
   (× `scale_factor`). On the tsclient lane the page/SLRU writes run as
   PGSQL_UPSERT batches with per-row response verification (a dropped
   row surfaces as a batch error, not a short count); `put_meta` always
   stays on YSQL. Because every connection runs in upsert mode, a retry
   after partial-chunk success cannot PK-conflict.
7. On success, `mark_flushed` flips the snapshot's rows from unflushed
   → durable in place (the rows do NOT move; they stay cached for
   reads), `evict_to_capacity` sheds cold keys if `total_bytes` exceeds
   `read_cache_bytes`, `flush_horizon_lsn` advances to the ingest
   frontier, the materialiser trigger runs (next section), and the
   durable `consistent_lsn` UPDATE fires if it moved and the
   per-`max_age` throttle allows (at most one row UPDATE per 5 s window
   by default — safekeeper truncation lagging by one window is
   trivial). `consistent_lsn_observed` always tracks the real durable
   frontier; a clean shutdown catches up any throttled tail via
   `force_upsert_consistent_lsn_if_advanced`.
8. Reads between flushes (and DURING a flush) see all in-RAM rows —
   pre-flush and durable cache — blended with PageStore via
   `read_chain` / `scan_range` (sort, dedup by LSN, truncate at the
   first `will_init=true` row). Meta reads sample the aux buffer
   BEFORE the YB read (a row leaves aux only after it is committed to
   YB, so aux-first closes the window where a concurrent flush makes a
   row momentarily invisible to both sources; on an LSN tie the durable
   YB row wins, because a walreceiver reconnect can re-push a stale
   same-LSN payload into aux).

On flush error the buffers are left untouched and the next periodic
flush retries.

### Flusher task and flush triggers

`PageWriter::spawn_flusher` runs a periodic task
(`flush_poll_interval`, default 500 ms) that flushes when either
trigger crosses threshold: buffer age ≥ `flush_interval` (5 s) or
buffered bytes ≥ `flush_size_bytes` (25 MiB). It also waits on
`materialize_wakeup`: the instant a key's delta tally crosses its
materialise threshold, the put path pulses the notify and the flusher
flushes immediately — so materialise dispatch runs within milliseconds
of the crossing instead of at the next 5 s / 25 MiB cadence.
`notify_one` coalesces a burst of crossings, and `put()` never blocks
on it, so this paces the materialiser without throttling ingest. On
shutdown the task does a final flush plus the throttle-bypassing
`consistent_lsn` catch-up.

### Backpressure

The walreceiver ingests as fast as the safekeeper sends; two points in
`PageWriter` block it when a downstream consumer is the bottleneck,
keeping heap usage bounded:

- **Flush-side.** If `unflushed_bytes + bytes_in_aux_buffers` exceeds
  `backpressure_bytes` (default 50 MiB), `put` awaits a `Notify` pulsed
  at the end of every successful flush. The 25 MiB size trigger fires
  well before the cap, so this path only engages when YB INSERTs slow
  down (CPU contention, network spikes, tablet split). Pre-flush rows
  are pinned in `UnifiedPages` until durable, so without the cap the
  buffer grows linearly with the YB backlog — multi-GiB RSS growth on
  co-located deployments under CPU pressure. `backpressure_bytes = 0`
  disables the cap (unbounded growth).
- **Materialiser-side (queue depth).** The write-time materialise
  dispatch does a **blocking** `mpsc::send` into the materialiser channel
  (capacity `materialize_queue_capacity`, default 2000). When the
  materialiser cannot drain, the channel fills and the walreceiver blocks
  at the send, bounding in-flight materialisation work. The read-side
  dispatch uses `try_send` instead — GetPage latency must not depend on
  materialiser progress; a dropped hint re-fires on the next read of
  the same key.
- **Materialiser-side (redo-pipeline bytes).** Before redoing each batch the
  materialiser holds its outstanding `commit_phase` image bytes under
  `materialize_inflight_bytes_limit` (default 512 MiB, not scaled; `0` disables)
  by draining outstanding INSERTs first. This stops draining the job queue, so it
  composes with the queue-depth path above to backpressure the walreceiver. (Those
  images are fixed-size and already bounded by the count cap, so this gate is a
  defence-in-depth ceiling — see the materialiser-RAM section for the two pools.)

The wait kinds are counted in
`pageserver_yb_pagewriter_backpressure_waits_total{kind}` (`flush`,
`materializer`, `materializer_inflight_bytes`); buffer, queue, and
redo-pipeline depth are the `pageserver_yb_pagewriter_unflushed_bytes`,
`pageserver_yb_materializer_queue_depth`, and
`pageserver_yb_materializer_inflight_bytes` gauges. When both counters are
active, the flush path is almost always the root cause: a slow
`put_pages` blocks the materialiser's own writes, which holds
materialiser slots, which fills the channel.

## The image rule and the background materialiser

Chain length is not a write-time invariant. Reads are always correct —
a chain with no image simply walks back to the latest `will_init`
delta — but walredo cost grows with chain length, so a background
**image materialiser** (`image_materializer.rs`) collapses long chains
into fresh image rows. Materialisation is pure read-latency
optimisation, deliberately off the walreceiver's hot path (walredo is a
synchronous subprocess RPC; doing it at write time would serialise
ingest).

**The rule.** `PageWriter::put` keeps a per-key tally
(`delta_counts: DashMap<Key, DeltaTally>` — count, payload bytes, and
last LSN; reset whenever an image or `will_init` row arrives for the
key). A key is queued for materialisation when, since its last anchor,
it accumulates either

- `image_chain_limit` deltas (default **20**) or
- `image_chain_payload_bytes_limit` combined delta payload bytes
  (default **4 KiB** — a few near-full-page deltas are already a lot of
  walredo work even when the count is low),

and for SLRU block keys, `image_chain_limit_slru` deltas (default
**1000**; the payload trigger does not apply — clog deltas are a couple
of bytes each, so the relation-page thresholds would fire absurdly
often). Setting a limit to 0 disables that dimension. Threshold
crossings are recorded in a `delta_ready` set so the flush-side walk
touches only crossers, not every cached key.

**Dispatch is post-flush, so targets are durable by construction.**
After every successful flush, `flush()` walks `delta_ready` and
enqueues one `MaterializeJob { key, target_lsn }` per crossed key:

- keys with rows in the just-flushed snapshot dispatch at their
  snapshot max LSN (just made durable);
- cold residue (crossed in a past flush, quiet since) dispatches at its
  tally's `last_lsn` once `last_lsn ≤ flush_horizon_lsn` — and this
  path also runs on empty flushes, so residue drains when compute goes
  idle instead of accumulating forever;
- the **actively-growing SLRU page is imaged at the flush horizon**:
  its newest delta's LSN always races ahead of the durable horizon, so
  the cold-residue rule would skip it every flush and only collapse it
  once cold (a full ~32 K-delta page). Dispatching at `horizon` images
  the durable prefix; the deltas above the horizon stay a short tail,
  re-imaged as the horizon advances.

A per-key `in_flight` set prevents a hot key from queueing a job per
threshold-multiple during a materialiser stall: the counter stays ≥
threshold across the stall and one fresh job fires at the *current* LSN
when the previous one completes — a newer image for one walredo pass.

**The worker** (`ImageMaterializer::spawn`, one per branch) drains the
queue in batches of up to `BATCH_DRAIN_LIMIT` (500) jobs through a
two-phase pipeline:

1. `redo_phase` — on the worker task, up to `redo_concurrency`
   (8 × `scale_factor`) concurrent `redo_one` futures. Each reads its
   chain from **durable sources only** — `read_chain_durable` for
   relation keys (durable cache rows + YB via the fpu store, pre-flush
   rows excluded), `read_slru_chain` for SLRU keys — then `parse_chain`
   splits `(base_img, records)` and
   `WalRedoManager::request_redo(key, actual_lsn, base_img, records,
   pg_version, RedoAttemptType::LegacyCompaction)` produces the 8 KiB
   image. `actual_lsn` is the chain head actually read (may trail
   `target_lsn`; partial progress is fine — the key re-triggers).
   Skips: empty chain, or head already an image at ≥ `target_lsn`.
   A chain read error logs and skips (best-effort); a **walredo error
   fatally aborts the process** — the chain does not legally produce a
   page, and continuing risks persisting a bad image. A merged durable
   chain with no `will_init` anchor is refused with
   `correctness_errors{materialize_unanchored_chain}` rather than
   letting walredo fold deltas onto a zero page and making that
   corruption durable.
2. `commit_phase` — spawned onto a separate task (bounded by
   `BASE_MAX_INFLIGHT_INSERTS` = 4 × `scale_factor`, matching the fpu
   pool so concurrent batches land on distinct YB backends), so batch
   N+1's redo overlaps batch N's INSERT round trip. Relation images go
   to `pages` via `put_pages` and are then offered to the cache;
   SLRU images go to `slru_pages` and need no cache install — there is
   no long-lived in-RAM SLRU read cache, and the image row anchors
   subsequent `read_slru_chain` reads on its own. `in_flight` is
   released per batch on success or failure. With `enable_gc` on, the
   commit also records a `pending_gc` trim candidate per installed
   image (buffered, best-effort — see
   [`pageserver-gc.md`](pageserver-gc.md)), so the superseded rows
   below the new anchor are later reclaimed by the GC drain; with it
   off (the default) the row count grows monotonically and
   materialisation pays only for read-latency wins.

Materialiser RAM has two pools, bounded differently:

- **Commit-pipeline images** (redone-but-not-yet-INSERTed): fixed-size (one
  ~8 KiB image per job), so `max_inflight_inserts × BATCH_DRAIN_LIMIT` (the
  count cap) already bounds this to tens of MiB. `materialize_inflight_bytes_limit`
  (default 512 MiB, **not** scaled by `scale_factor` — a memory ceiling, `0`
  disables) is a redundant defence-in-depth ceiling over it: before redoing each
  batch the worker holds an exact **local** running sum of its own outstanding
  `commit_phase` image bytes (paired with each queued INSERT future, `+=` on
  spawn / `-=` on drain) under the limit, draining oldest commits first. The
  gate uses this local counter, **not** the process-global
  `pageserver_yb_materializer_inflight_bytes` gauge (which sums across every
  timeline's materialiser and is observability only). At realistic
  `scale_factor` the count cap binds first, so this gate rarely engages.
- **Redo-input chains** (`read_chain_durable` payloads held during redo — the
  pool that actually grows with deep SLRU chains): bounded by `redo_concurrency`
  (a count) × chain depth, released by `chain_guard` before commit. This is the
  variable pool, and it is **not** byte-gated today — #373 will extend the byte
  cap to cover it (chain sizes are only known after the read, so the count cap is
  the current bound).

When the write side stalls, the local image-byte gate (or the count cap) blocking
before redo stops draining the job queue, which fills it and backpressures the
walreceiver through the write-side blocking send — the same end-to-end stall path
as `backpressure_bytes`, keyed on materialiser RAM rather than the pre-flush
buffer.

Why durable-only reads: if the materialiser built an image from
pre-flush rows and the process crashed, WAL replay would re-emit those
deltas — but the image is already in YB at the same LSN, and reads
above it would walredo the re-emitted deltas on top, double-applying
them. `read_chain_durable` restricts inputs to data that survives
crash + replay, and its `target_lsn` is contractually ≤
`flush_horizon_lsn` so a mid-flush partial-YB view (parallel chunks
commit out of order) can never feed walredo a chain with a hole.

**Guarded cache install (`install_image_if_extends`).** The cache fast
path trusts any cached `will_init` row as an anchor with a complete
chain above it, so an install must never seed an anchor that breaks
"present key ⇒ complete suffix". An image landing below the cached head
with rows above it living only in YB would do exactly that — a later
read at an intermediate LSN would anchor on it and silently drop
committed writes (a lost-update; reachable via historical-LSN reads or
an eviction between fold and install). `install_image_if_extends`
therefore installs only under one hold of the key's shard mutex, only
when the key is present and `image.lsn ≥ min(cached lsns)`; otherwise
it skips — the image is already durable in YB, so reads fall through to
the YB blend, correct but un-accelerated. Safe historical installs
(suffix above fully cached) are permitted, preserving the speedup for
static/branch readers. The `direct_getpage` integration scenario
reproduces the lost-update deterministically under tiny cache/chain
limits.

**Failure / restart.** Failed jobs are logged and dropped; the next
delta past the (reset) counter re-enqueues. On process restart the
per-key counters start at zero, so restart-survivor keys with long
chains read via the slow path until the threshold re-fires — the
read-side dispatch (a read that walredoes a chain longer than
`image_chain_limit`, or one that crossed a lineage segment) self-heals
them after one slow read.

## Replay correctness rules

The walreceiver resumes from durable `consistent_lsn`, so after a crash
(or a safekeeper reconnect under a live pageserver) it re-applies a
window of WAL whose effects are already partly in YB. Three rules keep
that window from corrupting state:

**Replay reads pre-record state exclusively.** Rows the first pass
already upserted — including `rel_size_to_key` values written *at the
extending record's own LSN* — are visible to the replaying record. Any
ingest decision that asks "what was the state *before* this record"
(relation existence and size in `create_relation_if_required` /
`put_rel_extend`) therefore reads committed state **strictly below**
the record's LSN (`DatadirModification::get_committed_before`,
`Version::at(lsn-1)`), while same-record pending writes stay visible
through the modification's pending map and the rel-size cache. An
inclusive read is identical on the first pass but makes replay see its
own prior-pass effects: `put_rel_extend` no-ops, the rel-size cache
pins at the replay-start size, and gap-fill stamps all-zero images over
live tail blocks. Regression test:
`test/integration/compute/slow/restart_relsize_zero_shadow.rs`.

**Replay windows are quarantined.** A walreceiver connection that
starts below the safekeeper's WAL end closes a per-window latch
(`Timeline::begin_replay_window`); catch-up — or an empty window at
connect — opens it (`mark_replay_caught_up`). While the latch is
closed:

- **Image materialiser folds are suppressed.** All three dispatch paths
  (flush trigger, cold residue, read-side) check
  `MaterializerHandle::folds_allowed`; a fold dispatched mid-window can
  anchor on state that predates deltas it has not seen and stamp the
  image at the chain tip — a durable lie every later read anchors on
  (observed shape: a clog image at the basebackup LSN missing the final
  ~30 durable commits' bits). Tallies keep accumulating; folding
  resumes after catch-up.
- **Basebackup waits.** `handle_basebackup_request` blocks (bounded,
  60 s) on the same latch via `wait_replay_caught_up`, so no compute is
  born from mid-window state.

**The rel-size cache is horizon-gated and forward-only.**
`rel_size_latest_cache` serves any read at `request_lsn ≥ cached_lsn`
on the premise that the entry is the relation's *current* size — false
mid-window. External (`Version::LsnRange`) reads are served only from
entries stamped at or above `rel_size_cache_horizon` (the
construction-time durable frontier); mid-window entries fall through to
the YB read, which is durably correct at any LSN
(`latest_rel_size_cache_hit`). Ingest reads (`Version::Modified`) are
exempt — the pre-record-read semantics above *depend* on the cache
tracking the replay position. And `set_cached_rel_size` never rewinds a
shared entry to the replay position: the replayer's own reads tolerate
a newer entry (they miss and fall through to an LSN-correct YB read),
while an external `nblocks` read arriving mid-window must never pin a
mid-window size into a compute for its lifetime.

**FSM/VM tolerance.** The rel-directory can list an FSM or
visibility-map forknum at a record's pre-record LSN while that
forknum's `rel_size_to_key` first appears only at the record's own
LSN — recovery resuming at a `consistent_lsn` between the two writes.
`get_rel_size_in_reldir` treats a `MissingKey` on an FSM/VM size read
as size 0, so an `SMGR_TRUNCATE_FSM`/`_VM` ingest no-ops instead of
wedging the walreceiver on `could not find data for key`. MAIN-forknum
size reads still error on a missing key. FSM/VM are advisory
(VACUUM-rebuilt), so an imprecise post-recovery size is harmless.
Exercised by the `torn_wal_write` integration scenario.

## Generation fencing

`current_generation` on the `pageserver.timeline_state` row is the
fence. The launcher bumps the generation on every spawn and bakes it
into `pageserver.toml`; the YB-side sequence is:

1. **Attach:** `PageStore::cas_bump_generation_on_attach` runs
   `UPDATE ... SET current_generation = $ours WHERE branch_id = $1 AND
   current_generation < $ours`. Zero rows → probe: stored == ours is an
   idempotent re-attach; stored > ours means a newer pageserver owns
   the branch and the attach is refused (fatal at startup).
2. **Data writes are not individually fenced.** Row INSERTs run as
   independent parallel chunks with no `BEGIN/COMMIT` wrapper. The
   fence is the **subsequent standalone UPDATE** in
   `PageStore::advance_consistent_lsn`:
   ```sql
   UPDATE pageserver.timeline_state
   SET consistent_lsn = $2,
       prev_record_lsn = COALESCE($4, prev_record_lsn)
   WHERE branch_id = $1 AND current_generation = $3
   ```
   Zero rows → a one-shot probe SELECT disambiguates "row missing"
   (wiring bug, hard error) from `StaleGeneration { live_generation }`;
   on the latter the flush path calls
   `task_mgr::shutdown_for_stale_generation` and the process shuts
   down. A stale pageserver's data rows are either redundant (the live
   pageserver overwrites them with identical upserts) or harmless — the
   damage a stale writer could do is advancing `consistent_lsn` (which
   drives safekeeper truncation), and exactly that write is fenced.
3. `update_timeline_state` (the metadata-field UPDATE) carries the same
   `current_generation = $gen` predicate; a fenced call logs and
   proceeds, leaving the shutdown to the next flush.

Generation state lives entirely in this one column — there is no
per-file or per-object generation stamping anywhere.

## Page services: libpq and gRPC

Computes fetch pages over two equivalent surfaces
(`page_service.rs`):

**libpq** (`listen_pg_addr`, default `127.0.0.1:64000`). Commands:
`pagestream_v2` / `pagestream_v3` (COPY-both subprotocol carrying
Exists / Nblocks / DbSize / GetSlruSegment / GetPage / Test messages),
`basebackup`, `fullbackup`, `lease lsn`, `set`. The pipelined executor
(`page_service_pipelining`, defaults: `max_batch_size` 32,
`pipeline_depth` 16) batches GetPage requests and runs them through a
`FuturesOrdered`; on
client disconnect (read EOF/reset) it **aborts** in-flight reads by
dropping the pipeline rather than draining responses to a gone peer —
without that, a getpage read stalled on a slow YB round trip pins the
socket fd in `CLOSE-WAIT`, and under a stall storm the accept loop dies
with `EMFILE`. As defense in depth against peers that vanish without a
FIN (host death, partition), every page-service socket gets TCP
keepalive (30 s idle + 3 × 10 s probes).

**gRPC** (`listen_grpc_addr`, off unless configured; conventional port
51051). `proto::PageService` (`pageserver/page_api/proto/page_service.proto`)
serves six RPCs: `GetBaseBackup` (streamed 256 KiB chunks), `GetDbSize`,
`GetPages` (bidirectional stream), `GetRelSize`, `GetSlruSegment`,
`LeaseLsn`. A metadata interceptor extracts the branch identity and an
auth interceptor validates the JWT; a reflection service is registered
for `grpcurl`. Responses are sent uncompressed (pageserver and compute
are co-located; compression burns CPU for no bandwidth win) while
gzip/zstd request compression is accepted.

Both surfaces authenticate with `AuthType::Trust` or
`AuthType::YbAmpJWT` (EdDSA JWT; scopes `PageServerApi` or `Tenant`,
where `Tenant` must match the branch — `auth.rs::check_permission`),
and both reject requests for any other `branch_id` as misrouted.

**Basebackup** builds the tarball from the row store at the requested
LSN. Before reading it (a) waits for the replay-quarantine latch (see
above) and (b) flushes the PageWriter (`flush_for_basebackup`) so the
read sees a self-consistent durable snapshot rather than a mid-recovery
durable+buffer blend. **`lazy_slru_download` defaults to `true`**
(`default_tenant_conf`, `compute/libs/pageserver_api/src/config.rs`),
so basebackup ships no SLRU (clog/multixact) pages — the compute faults
them in on demand via `communicator_read_slru_segment`. With
`lazy_slru_download = false` basebackup eagerly packs the entire SLRU
into the tarball; under a heavy workload (a 4000-warehouse TPC-C run
bloats clog/multixact to hundreds of thousands of pages) that makes
compute startup read the whole SLRU one page per RPC and overrun the
Init timeout. A `BasebackupCache` can serve prepared tarballs from
local disk for restart-shaped requests.

## Walreceiver and walingest

The walreceiver (`page_server/timeline/walreceiver/`) maintains one
libpq replication connection to the single safekeeper configured in
`safekeeper_pg_addr`, reconnecting with backoff
(`connection_manager.rs`); it runs on a dedicated Tokio runtime
(`WALRECEIVER_RUNTIME`). That safekeeper is not necessarily on the same
node or the same build — the stream carries the version-portable
**interpreted-WAL** protocol (`InterpretedWalRecords`,
`libs/wal_decoder`): the safekeeper filters and decodes raw WAL and
ships per-record `SerializedValueBatch`es, which the pageserver ingests
without re-parsing raw WAL (see
[`high-level.md`](high-level.md) "PS/SK co-location is NOT an
invariant").

On connect, the stream starts at durable `consistent_lsn` — NOT the
in-memory applied position — because everything above `consistent_lsn`
might not be in YB yet, and starting higher would lose the gap while
the already-advertised truncation point lets the safekeeper discard it.
If the start point is below the safekeeper's WAL end the connection
closes the replay-quarantine latch first (see "Replay correctness
rules"). `walingest.rs` digests each interpreted record into
`DatadirModification` puts; `Timeline::finish_write` advances the
applied frontier and notes `(last_record_lsn → prev_record_lsn)` for
the flush path's `prev_record_lsn` pairing.

The periodic feedback message (`PageserverFeedback`) reports the wire
field `disk_consistent_lsn` populated with the durable
`consistent_lsn`, and `remote_consistent_lsn` from the slot that
`PageWriter` bumps after every successful generation-fenced
`advance_consistent_lsn` — that value bounds safekeeper WAL truncation,
and it only ever echoes a fact already durable in YB.
`validate_wal_contiguity` optionally cross-checks the raw-WAL start/end
LSNs of each batch against the expected stream position.

## Walredo

`walredo.rs` runs Postgres binaries in a dedicated wal-redo mode
(`compute/pgxn/yb_amp_walredo/walredoproc.c` is the other side): the pageserver
passes a base image (if any) plus the WAL records to apply over
stdin/stdout and gets the reconstructed page back. The subprocess drops
privileges before replaying, so a malicious record cannot escape it.
`PostgresRedoManager` keeps a per-branch pool of subprocess slots
(`walredo_pool_size` = 2 × `scale_factor`, round-robin); a single
request is serialised through one slot's stdin/stdout, but concurrent
requests pipeline across slots and within a slot's IPC. Requests are
bounded by `wal_redo_timeout` (default 60 s). Bespoke non-FPI record
types (e.g. visibility-map bit clears, clog membership updates) are
applied in Rust by `walredo/apply_neon.rs` without the subprocess.
`RedoAttemptType` labels the caller (`ReadPage` for the read path,
`LegacyCompaction` for the materialiser) in metrics and error handling:
read-path walredo failures surface as `PageReconstructError::WalRedo`
on that one read, materialiser walredo failures abort the process.

## Background tasks

`page_server/tasks.rs` starts exactly two per-branch loops, plus
on-demand logical-size calculation:

- **GC loop** (`gc_period`): refreshes GC cutoffs (space horizon from
  `gc_horizon`, time cutoff from `pitr_interval` via
  `find_gc_cutoffs`), culls expired LSN leases, and updates
  PITR-related metrics. With `enable_gc` on for the branch, each tick
  also persists `applied_gc_cutoff_lsn = max(old, fresh cutoff)` to
  `timeline_state` (generation-fenced, monotone, durable **before**
  anything is deleted) and drains the due `pending_gc` candidates,
  deleting the superseded `pages` rows they name. With `enable_gc` off
  (the default) the tick deletes nothing and superseded deltas and old
  images accumulate in YB. Full design:
  [`pageserver-gc.md`](pageserver-gc.md).
- **Housekeeping loop** (fixed 20 s cadence,
  `TENANT_HOUSEKEEPING_PERIOD`): ingest housekeeping plus getpage
  throttle accounting.

Both share a semaphore sized at 3/4 of the Tokio worker threads
(`CONCURRENT_BACKGROUND_TASKS`).

## Configuration

Write-path knobs live under the `[yb_page_writer]` block of
`pageserver.toml` (`YbPageWriterConfig`,
`compute/libs/pageserver_api/src/config.rs`). Fields left unset take a
default; memory/queue budgets among those defaults are multiplied by
the global `scale_factor` (explicitly-set values are used verbatim):

| Knob | Default | Meaning |
|---|---:|---|
| `flush_interval` | 5 s | Time-based flush trigger |
| `flush_size_bytes` | 25 MiB | Size-based flush trigger (scaled) |
| `flush_poll_interval` | 500 ms | Background flusher poll cadence |
| `image_chain_limit` | 20 | Relation-page deltas-since-anchor before a key is queued for materialisation (`0` disables the count trigger) |
| `image_chain_limit_slru` | 1000 | Same, for SLRU (clog/multixact) blocks; the payload trigger does not apply to SLRU. `0` disables SLRU materialisation |
| `image_chain_payload_bytes_limit` | 4 KiB | Combined-delta-payload trigger for relation-page materialisation (`0` disables the size trigger) |
| `materialize_queue_capacity` | 2000 | Bounded mpsc depth (scaled); write-side dispatch **blocks** on it, read-side dispatch uses `try_send` |
| `materialize_inflight_bytes_limit` | 512 MiB | Hard ceiling on redone-but-not-INSERTed materialiser image bytes (`pageserver_yb_materializer_inflight_bytes`); the worker drains outstanding INSERTs before redoing more, backpressuring the walreceiver. **Not scaled** (memory ceiling); `0` disables |
| `read_cache_bytes` | 1 GiB | Soft cap on `UnifiedPages.total_bytes` (scaled); whole cold keys are evicted LRU-order, keys with unflushed rows pinned |
| `backpressure_bytes` | 50 MiB | Hard ceiling on the pre-flush buffer (scaled); `PageWriter::put` blocks above it; `0` disables |
| `meta_cache_entries` | 8192 | In-process LRU for `pageserver.meta` point reads; `0` disables |
| `ingest_parallelism` | 4 | Workers for `put_serialized` batch apply; `1` = serial. Same key always lands on one worker |
| `ingest_parallelism_min_batch` | 256 | Minimum batch size before the parallel apply engages; `1` forces it (tests) |

Other relevant settings: `scale_factor` (global multiplier for pool
sizes, worker counts, and the budgets above; 1.0 default, 0.25 fits a
small dev box), `wait_lsn_timeout` (300 s), `wal_redo_timeout` (60 s),
`safekeeper_pg_addr`, `listen_pg_addr` / `listen_http_addr` /
`listen_https_addr` / `listen_grpc_addr`, `http_auth_type` /
`pg_auth_type` / `grpc_auth_type`, and the `hot_path` /
`tserver_rpc_addrs` fields of the shared YB connection block
(`YbConnConfig`, `compute/libs/ysql_client/`).

## Compute ↔ pageserver timeouts

The compute opens libpq connections to the pageserver per backend
(`compute/pgxn/yb_amp/libpagestore.c`):

| Stage | GUC / location | Default | Notes |
|---|---|---|---|
| TCP connect | libpq; no `connect_timeout=` is appended by `controller/local/cfg` | Unset → OS TCP SYN-retry (~127 s on Linux) | |
| First WARN log on a stalled response | `yb_amp.pageserver_response_log_timeout` | 10 s | Cosmetic; the request keeps waiting |
| Disconnect + reconnect on a stalled response | `yb_amp.pageserver_response_disconnect_timeout` | 30 s | The pageserver is a same-DC, low-latency hop; a stuck request is far more likely a pageserver-side stall than packet loss — drop and reopen fast rather than waiting out TCP retry backoff |
| Reconnect attempts before erroring to the query | `max_reconnect_attempts` | 60 (1 s backoff each) | A hard-down pageserver surfaces to the compute query in roughly 30 s + 60 × 1 s ≈ 90 s |
| Pageserver-side activation wait | none | — | The branch opens synchronously before listeners start; until then computes get ECONNREFUSED and retry via the loop above |

## What yb-pageserver does not do

- **Compaction** — none. Image creation is the async background
  materialiser; there is no L0/L1 layer work because there are no
  layers.
- **Layer files** — none. There is no `storage_layer` stack in use, no
  layer map, no on-disk B-trees, no in-memory layers
  (`TimelineWriterState` is a unit struct), no remote layer client.
  `layer_size_sum()` is always 0.
- **Local read cache on disk / eviction / heatmaps / secondary
  locations** — none. The only caches are the in-RAM `UnifiedPages` and
  meta LRU; local disk holds only the initdb tarball and basebackup
  cache.
- **Row GC by default** — `enable_gc` is off unless a branch opts in,
  and then `Timeline::gc` deletes nothing; row counts in YB grow
  monotonically. With it on, superseded page versions are reclaimed via
  the `pending_gc` candidate queue + periodic drain
  ([`pageserver-gc.md`](pageserver-gc.md)).
- **S3 / object storage** — the pageserver's `remote_storage` backend
  is `YbStore` (required at startup); there is no S3 dependency and no
  object-versioning time-travel path. Point-in-time recovery composes
  from existing pieces — `find_lsn_for_timestamp` resolves a wall-clock
  time to an LSN, and branch creation at that LSN provides the
  recovered timeline — but the operator-facing command and the GC story
  for cross-branch row retention are open work.
- **Multi-branch hosting** — one process, one branch. Branch lifecycle
  (create/delete/migrate) is driven by the launcher and controller, not
  by pageserver HTTP APIs.
- **Detach-ancestor** — unsupported; the `lineage` JSONB column is
  always `Default`.
- **Consumption metrics / synthetic size workers** — none.

## Code anchors

- `compute/pageserver/src/page_store.rs` — `PageStore`: the four-table
  row store, image-anchored read SQL, lineage walks, timeline-state
  CRUD, generation CAS + fenced UPDATEs, tsclient lane.
- `compute/pageserver/src/page_writer.rs` — `PageWriter`: buffered
  write path, flush + `consistent_lsn` advance, backpressure,
  materialise triggers, blended reads.
- `compute/pageserver/src/gc_trim_candidates.rs` — `GcTrimCandidateEmitter`: the
  shared best-effort GC trim-candidate buffer both producers feed
  (see [`pageserver-gc.md`](pageserver-gc.md)).
- `compute/pageserver/src/unified_pages.rs` — the combined pre-flush
  buffer / read cache with per-key whole-eviction and the guarded image
  install.
- `compute/pageserver/src/image_materializer.rs` — the background
  materialiser worker pipeline.
- `compute/pageserver/src/page_server/timeline.rs` —
  `get_vectored_via_page_writer`, `chain_to_value_reconstruct_state`,
  the replay-quarantine latch, `force_flush`.
- `compute/pageserver/src/page_server/timeline/walreceiver/` —
  safekeeper connection management and interpreted-WAL ingest.
- `compute/pageserver/src/bin/yb-pageserver.rs` — process wiring:
  config load, listeners, the two PageStore pools, synchronous open.
- `compute/libs/pageserver_api/src/config.rs` — `ConfigToml`,
  `YbPageWriterConfig`, `TimelineInitConfig`, `scale_factor` helpers.
- `controller/migrations/0002_compute_storage.up.sql` — the
  authoritative DDL for the `pageserver.*` tables.
- [`pageserver-apis.md`](pageserver-apis.md) — HTTP/gRPC/libpq
  API inventory; [`pageserver-metrics.md`](pageserver-metrics.md)
  — metrics; [`yugabyte.md`](yugabyte.md) — full `yb_storage` DDL and
  the tsclient lane details; [`pageserver-lsns.md`](pageserver-lsns.md)
  — LSN vocabulary.

## Testing

The integration suite (`test/integration/compute/`, a Rust harness —
see
[`test/integration/compute/README.md`](../../test/integration/compute/README.md))
is the cross-seam backstop: it runs scenario tests and golden `.sql`
workloads against a real `yb_amp_local` + YB and scans the pageserver
log for corruption markers after every test. The `.sql` workloads diff
their result sets against checked-in `.out` goldens, so silently-wrong
reads fail as long as no statement errors out. Run
`test/integration/compute/run.sh fast|slow|all` before claiming any
corruption-class fix (the `integration-before-corruption-claim` skill
enforces this).
