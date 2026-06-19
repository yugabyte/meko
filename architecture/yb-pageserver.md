# yb-pageserver

The pageserver in this repo stores **one YB row per page version**
instead of building delta/image layer files. It started as a fork of
upstream Neon's `pageserver/` so the two could be benchmarked
head-to-head; the upstream source has since been removed (lane H,
2026-06-01) and the row-per-page implementation is now the canonical
`pageserver/` crate. The shipped binary keeps the `yb-pageserver`
name to make the storage backend obvious in `ps`, logs, and
deployments.

This document is the design and roadmap. The corresponding upstream
component is documented in [`pageserver.md`](pageserver.md); read that
first for the parts that are inherited unchanged (Key encoding, walredo,
WAL ingest, getpage RPC).

## Single-timeline process model

One yb-pageserver process serves exactly **one (tenant_id,
timeline_id) for its entire lifetime** (the long-term topology gives
each compute endpoint its own pageserver + safekeeper). That safekeeper
is not necessarily on this node or at this build — the walreceiver streams
the version-portable interpreted-WAL protocol and must tolerate a
remote/older/newer safekeeper (read endpoints especially; see
[`high-level.md`](high-level.md) "PS/SK co-location is NOT an invariant").
The identity is baked into `pageserver.toml` by the launcher before spawn:

```toml
tenant_id   = "c8178f5e..."   # required
timeline_id = "77ffde89..."   # required
generation  = 7               # required; launcher bumps on EVERY spawn

[timeline_init]               # consulted only when the timeline_state row is absent
mode       = "bootstrap"      # or "branch" + ancestor_timeline_id + ancestor_lsn
pg_version = 17
```

Startup is **open-or-create**, and runs SYNCHRONOUSLY in `main()`
before the HTTP/libpq/gRPC listeners start: a connecting compute
either gets ECONNREFUSED (process still opening; client retry covers
it) or talks to a fully-active timeline. There is no tenant state
machine and no in-protocol wait-for-active. Failure to open is fatal
to the process.

- Row exists in `pageserver.timeline_state` → open it (restarts are
  trivially idempotent; `[timeline_init]` is ignored). A row with
  `deleted_at` set refuses to open.
- Row absent + `mode = "bootstrap"` → run initdb + import, then
  insert the row (`UninitializedTimeline::finish_creation`). If the
  initdb tar for this timeline already exists in remote storage, it is
  REUSED — initdb output is not bit-deterministic, and a crash-retry
  must overwrite any orphan page rows with identical payloads.
- Row absent + `mode = "branch"` → validate against the ancestor's
  persisted row only (`PageStore::create_branch_row`: branch point
  durable, not below GC cutoff, not below the ancestor's own branch
  point) and insert a row with `consistent_lsn = ancestor_lsn` (the
  walreceiver resume point — never 0). No in-process ancestor object
  is needed; reads go through the lineage walk (below).
- Row absent + no `[timeline_init]` → fatal.

**Generation handoff is launcher-owned.** The pageserver no longer
calls the storage_ctl `/upcall/v1/re-attach` API (the
`controller_upcall_client` module is deleted). Instead, every
`PageServerNode::start` in neon_local: registers the node with
storage_ctl (idempotent), calls the attach-hook API (which increments
`storage_controller.tenants.generation` against this node and returns
it), rewrites `generation` in `pageserver.toml`, and only then spawns
the process. The YB-side CAS (`cas_bump_generation_on_attach`)
enforces the fence at open regardless of how the number arrived.

The management HTTP surface is correspondingly small: status /
metrics / profile / utilization, the single timeline's list + detail
(`GET /v1/tenant/:t/timeline[/:tl]`), `get_lsn_by_timestamp`,
`get_timestamp_of_lsn`, `lsn_lease`, `do_gc`, `flush` (`PUT
…/timeline/:tl/flush?lsn=` — flushes pages to YB, optionally waiting
for ingest to reach `lsn` first, then forces the durable `consistent_lsn`
UPDATE through immediately (bypassing the per-`max_age` upsert throttle that
the periodic/bootstrap flushes use); used to flush an ancestor at branch
time, see below), and test plumbing (failpoints, panic, tracing/event,
reload_auth_validation_keys). Inbound page-service requests (libpq
query string / gRPC metadata) are validated against the configured
identity and rejected as misrouted on mismatch.

Consequences elsewhere:
- storage_ctl's pageserver reconciler/migration machinery is deleted
  wholesale (see [`storage-controller.md`](storage-controller.md)):
  there is no location_config to converge — attachment is decided at
  spawn time.
- storage_ctl's timeline-create no longer POSTs to the pageserver; it
  **observes** (polls `timeline_detail` until `Active`, generous
  timeout for debug-build initdb), then runs the safekeeper leg with
  `last_record_lsn` from the observed `TimelineInfo`.
- `yb_light_local start` skips a pageserver whose toml has no
  identity yet; `tenant create` bakes the identity in and starts it.
- `timeline branch` provisions a NEW pageserver + safekeeper pair for
  the branch: it allocates the next node ids/ports, clones the
  ancestor pageserver's toml (patching listen addresses and
  `safekeeper_pg_addr`), bakes the branch identity +
  `[timeline_init] mode="branch"`, creates the timeline on the new
  safekeeper at the branch LSN, and starts both. The default branch
  LSN is the ancestor **safekeeper's `flush_lsn`** (durable WAL end),
  not the ancestor pageserver's `last_record_lsn` — the latter is an
  ingest position that can lag a just-committed transaction; the
  branch pageserver's startup retry waits for the ancestor's
  `consistent_lsn` to catch up before writing the branch row. To avoid
  paying a full ancestor flush interval (~5 s) on that retry, the launcher
  first issues a best-effort `flush?lsn=<branch_lsn>` to the **ancestor**
  pageserver (wait-for-ingest then flush), so the durability
  barrier clears on the first try (~5 s → ~0.1 s); the retry stays as the
  correctness guarantee. Endpoints resolve
  their pageserver and safekeeper from the timeline they sit on
  (`LocalEnv::pageserver_conf_for_timeline` /
  `safekeepers_for_timeline`) — the storage_ctl `tenant_locate` path
  is no longer used. `timeline create` (a second root timeline) still
  errors; create a new tenant instead.

## Lineage-aware reads (branches)

A branch's pre-branch-point rows live in YB under the **ancestor's**
`timeline_id`; nothing is copied at branch time (pages, SLRU and meta
alike). At startup the pageserver walks the
`ancestor_timeline_id`/`ancestor_lsn` links in `timeline_state` once
(`PageStore::load_lineage`) and builds an ordered segment list
`[(self, MAX), (parent, cap), (grandparent, min(caps)), …]` — static
for the process lifetime. Every PageStore read path (`read_chain`,
`scan_range`, `read_slru_chain`, `scan_slru_range`,
`scan_meta_range`) has a `*_lineage` variant that walks the segments
nearest-first, capping each segment's read LSN at
`min(target_lsn, cap)`. The walk stops only at an anchor (`will_init`)
whose LSN is **at or above the next segment's cap** (for meta: once
the key is found). An anchor *below* the cap — e.g. a materialiser
copy-up image — does not end the walk: ancestor deltas in
`(anchor_lsn, cap]` still apply and must be layered on top. The
`PageWriter` passes the lineage through everywhere, including the
materialiser's `read_chain_durable`, and its cache fast paths only
short-circuit when the cached anchor covers the lineage the same way
(`cache_anchor_covers_lineage`).

Iterative per-segment queries (not one UNION ALL) on purpose:
`timeline_id` is in the hash partition key, so segments live on
different tablets. The walk touches ONE tablet in the steady state —
always for unbranched timelines, and per-key after the materialiser
copies an anchor image up into the descendant's own timeline_id. A
chain that crossed a segment boundary force-dispatches a read-side
materialise job, so each cold branch key converges to local after one
slow read. Writes (`put_*`, materialiser installs,
`advance_consistent_lsn`, `delete_all_for_timeline`) always target the
own timeline_id, never an ancestor.

**Boundary.** The per-segment `timeline_id` binding (the read SQL's
`$2`, rebound for each ancestor — never baked as a literal, see the
`read_chain`/`scan_range` comment in `page_store.rs`) plus the `lineage`
vector are the one place reads are genuinely multi-timeline. Writes are
single-timeline, so the rest of the pageserver state collapses to the
process's single (tenant, timeline); the read path does **not**. A
simplification that hard-codes the write timeline on the read path, or
bakes it into the read SQL, would silently break branch reads — keep the
lineage walk and its per-segment binding intact.

Note the `lineage` JSONB column on `timeline_state` is unrelated: it
carries upstream detach-ancestor state (always `Default` in yb-light);
the ancestry chain is derived from the `ancestor_*` columns.

## What was kept from upstream

The refactor deleted the entire layer-file machinery (~25 K lines:
`storage_layer/`, `layer_map/`, `disk_btree`, `compaction.rs`,
`inmemory_layer`, `remote_timeline_client`, `secondary*`, `heatmap*`,
`eviction_task*`). What stayed — because it isn't
storage-implementation-specific — is:
- `pageserver_api` (Key, Lsn, configs, controller protocol)
- `pageserver_client`, `pageserver_client_grpc`,
  `pageserver_page_api` (sibling crates now living at `pageserver/{client,client_grpc,page_api}/`)
- `wal_decoder` (Value, NeonWalRecord)
- `remote_storage::YsqlClient` (reconnecting YB Postgres-protocol
  wrapper)
- `walredo` (Postgres-binary subprocess for WAL replay)

The binary name is `yb-pageserver`. Builds with
`cargo build -p pageserver --bin yb-pageserver`.

## What replaces what

| Upstream artifact | yb-pageserver replacement |
|---|---|
| `(Key, LSN) → Value` stored as B-tree leaves in delta/image layer files | One row per version in `pageserver.pages` (YB) |
| Open in-memory layer + frozen layer + flush loop | In-RAM write buffer (5 s / 25 MiB defaults), flushed as `INSERT … VALUES (…),(…),…` into `pageserver.pages` |
| L0 → L1 compaction, image-layer creation | **Image-anchored reads** + background image materialiser (this file) |
| `index_part.json` (per-timeline layer list) | The `pageserver.timeline_state` state row |
| `metadata` (local file with ancestor/LSN bookkeeping) | Columns on the `pageserver.timeline_state` row |
| `tenant-manifest.json` (per-gen layer tracking) | `current_generation` column on the timeline row |
| Local layer cache + secondary cache + heatmap | None — every read hits YB |
| Disk eviction + heatmap downloader | None |

The `getpage` gRPC, basebackup, walreceiver, walingest, walredo, and the
storage-controller handshake are unchanged.

## Schema

Four tables live in the `pageserver` schema inside `yb_storage`:
`pages` (relation pages), `slru_pages` (SLRU blocks), `meta`
(everything else — relsize sentinels, dir listings, aux files,
two-phase commit, repl origins), and `timeline_state` (per-timeline
state row). Per-tenant config is NOT in YB: the effective config is
the `[tenant_config]` section of `pageserver.toml`.
DDL is owned by the diesel migration at
`storage_controller/migrations/00000000000001_initial_schema/up.sql`
(lines 163-250). See [`yugabyte.md`](yugabyte.md) for the full DDL.

### `pageserver.pages`

One row per (Key, LSN). The PK partitions by the **(spcnode, dbnode,
relnode, forknum)** fork identity (each fork's whole block-set lives on
one tablet), then clusters by `(blknum, lsn DESC, is_image)` so a
per-block, per-LSN scan walks rows in LSN-descending order and an
anchor lookup at a given LSN is a single seek, not a scan.

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
    lsn          BIGINT   NOT NULL,    -- u64 stored as i64 (real LSNs are << 2^63)
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id, spcnode, dbnode, relnode, forknum) HASH,
                 blknum, lsn DESC, is_image)
);
```

Only **relation-page keys** live here — keys where
`Key::is_rel_block_key()` holds (`field1 == 0x00`, `field4 != 0`,
`field6 != 0xffffffff`). SLRU blocks go to `pageserver.slru_pages`
(see `up.sql:183-195`; `PageStore::put_slru_pages` / `read_slru_chain`
in `page_store.rs`). Everything else — metadata, dir listings, aux
files, repl origins, rel-size sentinels — goes to `pageserver.meta`
(see `up.sql:203-210`; `PageStore::put_meta` / `read_meta`). Nothing
in the pageserver writes to `public.blobs` anymore.

`is_image` is whether the payload is an 8 KiB Postgres page (vs. a
serialised `NeonWalRecord`). `will_init` mirrors `Value::will_init()` —
true for every image and for `WalRecord` entries that act as a chain
anchor (e.g. `XLOG_HEAP_INIT_PAGE`). With `is_image` in the clustering
key, a single block's rows are split into two sub-clusters; reads anchor
on the latest `is_image=TRUE` row at or below the target LSN and walk
the deltas above it.

### `pageserver.timeline_state`

One row per timeline. Replaces the upstream split of `metadata` +
`index_part.json` + `tenant-manifest.json`.

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

- `consistent_lsn` — durability *frontier*: every row at lsn ≤ this is
  durably present in `pageserver.pages`/`slru_pages`/`meta`. Advances on
  successful `PageWriter::flush` to the **ingest frontier** — the
  highest record `finish_write` had noted *before* the flush snapshot
  was taken (a noted record's rows are all buffered by then, so the
  snapshot provably contains every unflushed row ≤ frontier). It is
  NOT max(lsn) of the flushed rows: the snapshot races concurrent puts
  and can hold a row at lsn X while missing a sibling at lsn < X —
  deriving the checkpoint from the max is how #22's bug B silently
  lost records across a crash (`flush_horizon_lsn`, the materialiser's
  durable-read bound, follows the same frontier rule for the same
  reason). Drives both safekeeper WAL truncation and ingest resume on
  restart.
  `last_record_lsn` is intentionally not persisted — on restart, the
  walreceiver streams from `consistent_lsn` forward and the
  in-memory value re-derives within seconds.
- `applied_gc_cutoff_lsn` — same meaning as upstream; reads below this
  LSN aren't supported.
- `current_generation` — bumped by the storage controller on every
  attach. A stale pageserver's generation-checked write hits a row with
  a newer generation and aborts (see "Generation fencing" below).

## Read path: image-anchored CTE

`Timeline::get_vectored_impl` queries `PageStore` instead of walking
`LayerMap`:

1. For each requested key range at `target_lsn`, call
   `PageWriter::scan_range(range, target_lsn)` (blends in-RAM buffer +
   `PageStore::scan_range`).
2. Per-key reconstruction: chain comes back LSN-descending, pre-truncated
   at the first `will_init=true` row. The bottom row of that chain is
   the anchor — image → walredo base, will_init delta → first record.
3. Drive walredo as today (`Timeline::reconstruct_value`).

The SQL that `PageStore::read_chain` issues (single-key path) uses a
scalar-subquery anchor with `COALESCE(..., 0)` for the lower bound so
both ends of the LSN range are pushed into a single DocDB scan (no
CTE):

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
where `B` is the # of image rows for that block — typically 1. If no
image exists, the scalar subquery returns NULL and `COALESCE(..., 0)`
turns the lower bound into `p.lsn >= 0` — a plain LSN-descending scan
of the whole chain. The Rust-side trim walks the returned rows
LSN-desc and stops at the first `will_init=true` row, so both the
image-anchored and the no-image case shorten correctly. The
no-image case is the only "slow" path and only fires for cold blocks
the background image materialiser hasn't visited yet.

Reads above `last_record_lsn` block on `wait_lsn`. Reads below
`applied_gc_cutoff_lsn` return an error.

## Write path: pure passthrough

Replaces `InMemoryLayer` + `flush_loop` + L0/L1 compaction.

1. `walingest` produces `(Key, LSN, Value)` triples via
   `pgdatadir_mapping::DatadirModification.put`. Unchanged.
2. `DatadirModification.commit` calls `TimelineWriter::put_batch`, which
   forwards each batch to `PageWriter::put(entries)`.
3. `PageWriter::put` filters to relation-page keys (`is_rel_block_key`),
   converts each `Value` into a `PageRow` (`Value::Image` →
   `is_image=true, will_init=true`; `Value::WalRecord` → `is_image=false`
   with `will_init` taken from the record), and appends to the in-RAM
   buffer. **No walredo, no `ImageRule`** — writes are a straight
   serialise-and-buffer.
4. Rel-page rows live in `UnifiedPages` — one in-RAM structure that
   indexes both pre-flush (pinned in RAM until durable) and post-flush
   (LRU-ish cache for read-latency) entries. SLRU and meta entries
   stay in their own simpler `Vec` buffers for now. There is no
   monolithic `RwLock<WriterState>` anymore: per-key state lives in a
   lock-free ordered `SkipMap<Key, Mutex<KeyState>>` inside
   `UnifiedPages`, keyed on the relation `Key` itself — a `Copy`
   fixed-size struct, not a `Bytes` byte-encoding, so cache-key clones
   are register copies and ordering is integer field-compares (no
   per-row heap alloc, no `memcmp`); the skiplist's ordered `range()`
   backs `lookup_range`. Aux (SLRU/meta) buffers sit behind a small
   `parking_lot::Mutex<AuxBuffers>`, and counters like `rows_written`
   / `consistent_lsn_watermark` are atomics
   (`page_writer.rs:175-199`, `unified_pages.rs`). The writer fan-in
   and concurrent get_page reads go straight to per-key mutexes
   instead of serialising on a global lock.
   Concurrent get_page handlers filter the cache in parallel. Flushes
   fire when either trigger crosses threshold (default 5 s / 25 MiB).
5. Flush snapshots the unflushed prefix of `UnifiedPages` (no copy of
   row data — just clones the rows to a Vec) plus the SLRU / meta
   buffers, drops the lock, and dispatches chunked multi-row INSERTs
   against `pageserver.pages` / `slru_pages` / `meta`. Chunks within
   each table go in parallel via `stream::iter(...).buffer_unordered(N)`
   with N=4/2/3. `yb_enable_upsert_mode = true` is `SET` once per
   connection in `YsqlClient::open_*`, so a chunk retry after a
   partial-flush failure can't PK-conflict. On success, `UnifiedPages`
   flips the snapshot's rows from unflushed → durable in one cursor
   pass (the rows do NOT move; they stay in the cache for reads) and
   then evicts whole cold keys (LRU order, keys with unflushed rows
   pinned) if `total_bytes > read_cache_bytes`.
   SLRU / meta drain as before.
6. Reads between flushes (and DURING a flush) see all in-RAM rows —
   both pre-flush and durable cache — blended with PageStore via
   `read_chain` / `scan_range` (dedup by LSN, truncate at the first
   `will_init=true` row). Because flushes don't move the data, in-flight
   INSERTed rows remain visible.
7. Background image materialiser: when `PageWriter::flush` observes
   keys whose per-key delta tally crossed its threshold during the
   just-flushed window, it dispatches `MaterializeJob { key, target_lsn }`
   entries onto a bounded mpsc (blocking send — backpressures the
   walreceiver if the materialiser falls behind). Relation-page keys use
   `image_chain_limit` (or the payload-bytes threshold); **SLRU
   (clog/multixact) block keys use the separate `image_chain_limit_slru`**
   — the clog page takes one tiny delta per transaction commit, so its
   chain grows orders of magnitude faster than a relation page's but each
   delta is a couple of bytes, hence a much higher threshold. Without
   materialising SLRU, the clog chain grew to a full page (~32 K deltas)
   and every basebackup re-walked it to rebuild the page (the dominant,
   commit-volume-scaling recovery cost). Two refinements keep SLRU
   chains near the threshold under a high-rate commit burst rather than
   letting them run to a full page (#115):
   - **Dispatch is woken, not just cadence-driven.** A threshold
     crossing pulses `materialize_wakeup`, on which the periodic flusher
     waits alongside its timer; the flush — and the dispatch it drives —
     then runs within ~ms of the crossing instead of at the next
     5 s / 25 MiB flush. (`notify_one` coalesces a burst of crossings
     into back-to-back flushes, and `put()` never blocks on it, so this
     paces the materialiser without throttling ingest.)
   - **The actively-growing SLRU page is imaged at the flush horizon.**
     Its newest delta's LSN always races ahead of the durable
     `flush_horizon_lsn`, so the cold-residue dispatch (which requires
     `last_lsn ≤ horizon`) would *skip* it every flush and only collapse
     it once cold (full page). Instead, an SLRU key whose `last_lsn`
     exceeds the horizon dispatches at `horizon` — imaging the durable
     prefix. The image is stamped at the actual durable chain-head LSN
     (`read_slru_chain` returns only durable rows), so there is no hole;
     using `horizon` (vs `last_lsn`) also lets a page already imaged at
     `horizon` hit the "head is image ≥ target" skip and avoid redundant
     walredo. The deltas above `horizon` stay a short tail, re-imaged as
     `horizon` advances.
   The materialiser worker drains
   the queue in batches of up to `BATCH_DRAIN_LIMIT` jobs and runs a
   two-phase pipeline: `redo_phase` (chain reads + walredo on this
   task, with `redo_concurrency`-scaled fan-out) followed by
   `commit_phase` (bulk INSERTs + cache install spawned onto a separate
   task, bounded by `BASE_MAX_INFLIGHT_INSERTS`). The pipeline lets
   batch N+1's redo run concurrently with batch N's YB INSERT round-trip
   while keeping per-batch correctness — see `image_materializer.rs`.
   Jobs are routed by key type: relation chains read via `read_chain_durable`
   and write the image via `put_pages` + `install_materialized_image`
   (which trims the in-RAM cache for the key up to the image LSN);
   SLRU chains read via `read_slru_chain` and write via `put_slru_pages`
   (no in-RAM SLRU read cache — the image row in `slru_pages` anchors
   subsequent reads on its own). Either way, subsequent reads at
   LSN ≥ image_lsn anchor on the image (`is_image=TRUE, will_init=TRUE`)
   instead of walking back to the latest `will_init` delta.

Why the materialiser uses **durable-only** reads: if it built an
image from pre-flush rows and the process crashed, WAL replay would
re-emit those deltas — but the image is already in YB at the same
LSN. Reads at LSN > image_lsn would then walredo the re-emitted
deltas on top of the image, double-applying them. `read_chain_durable`
restricts the materialiser's inputs to data that survives crash +
replay.

**Replay reads pre-record state exclusively.** After a crash, WAL
replay re-runs below YB's durable horizon: rows the first pass already
upserted (including `rel_size_to_key` values, written *at the
extending record's own LSN*) are visible to the replaying record. Any
ingest decision that asks "what was the state *before* this record" —
relation existence, relation size in
`create_relation_if_required` / `put_rel_extend` — must therefore read
committed state **strictly below** the record's LSN
(`DatadirModification::get_committed_before`,
`Version::at(lsn-1)`), while same-record pending writes stay visible
through the modification's pending map and the rel-size cache. An
inclusive read is identical on the first pass but makes replay see its
own prior-pass effects and take different decisions — the #22
corruption: `put_rel_extend` no-opped, the rel-size cache pinned at
the replay-start size, and the gap-fill zero-filled
`[pinned_size, blkno)` on every record, stamping all-zero page images
over live tail blocks. The regression test is
`test/e2e/src/scenarios/fast/restart_relsize_zero_shadow.rs` (SIGKILL
+ replay + tenant-wide zero-image shadowing assertions).

FSM and visibility-map forks are a tolerated exception to the implicit
"relation exists ⇒ its size key is readable" coupling. The rel-directory
can list an FSM/VM fork at a record's *pre-record* LSN while that fork's
`rel_size_to_key` first appears only at the record's *own* LSN — recovery
resuming at a `consistent_lsn` that fell between the directory write and
the first size write. `get_rel_size_in_reldir` therefore treats a
`MissingKey` on an FSM/VM size read as size 0 (extending the existing
"smgrcreate-then-smgrnblocks" size-0 tolerance) so an
`SMGR_TRUNCATE_FSM`/`_VM` ingest no-ops instead of wedging the
walreceiver on `could not find data for key`. MAIN-fork size reads still
error on a missing key. FSM/VM are advisory (VACUUM-rebuilt), so an
imprecise post-recovery size is harmless. Exercised by the
`torn_wal_write` e2e scenario (#36).

Default knobs (configurable via `pageserver.toml` under the
`yb_page_writer` block):

| Knob | Default | Meaning |
|---|---:|---|
| `flush_interval` | 5 s | Time-based flush trigger |
| `flush_size_bytes` | 25 MiB | Size-based flush trigger |
| `flush_poll_interval` | 500 ms | How often the background flusher checks the triggers |
| `backpressure_bytes` | 50 MiB | Hard ceiling on the pre-flush buffer. `PageWriter::put` blocks until a flush drains below it. `0` disables (legacy unbounded growth) |
| `image_chain_limit` | 20 | Relation-page deltas-since-anchor before a key is queued for materialisation (`0` disables count trigger) |
| `image_chain_limit_slru` | 1000 | Same, for SLRU (clog/multixact) blocks. Higher than the relation limit because clog takes one ~2-byte delta per commit; caps clog reconstruction (and basebackup, which rebuilds the clog page) at ~1/30th of a full ~32 K-delta page. `0` disables SLRU materialisation |
| `image_chain_payload_bytes_limit` | 4 KiB | Combined-delta-payload trigger for relation-page materialisation (`0` disables size trigger; does not apply to SLRU) |
| `materialize_queue_capacity` | 2000 | Bounded mpsc depth; the write-side dispatch uses **blocking** send (backpressures walreceiver); the read-side dispatch uses `try_send` (drops on overflow — the next read of the same key re-evaluates) |
| `read_cache_bytes` | 1 GiB | Soft cap on `UnifiedPages.total_bytes`. Whole cold keys are evicted in LRU order when crossed; keys with unflushed (pre-flush) rows are pinned regardless |

## Backpressure

The walreceiver has no built-in throttle: it ingests as fast as the
safekeeper sends. Two places in `PageWriter::put` block the walreceiver
when a downstream consumer is the bottleneck. Both keep heap usage
bounded under sustained CPU/IO pressure that would otherwise grow the
in-RAM buffer without limit.

**Flush-side backpressure.** If `unflushed_bytes + bytes_in_aux_buffers`
exceeds `FlushPolicy::backpressure_bytes` (operator knob
`backpressure_bytes`, default 50 MiB), `put` awaits a Tokio `Notify`
pulsed at the end of every successful `flush()`. The size-based flush
trigger (`max_bytes`, default 25 MiB) fires well before the
backpressure cap is reached, so under steady state the wait path is
not exercised — it only kicks in when YB INSERTs against
`pageserver.pages` / `slru_pages` / `meta` slow down (CPU contention,
network spikes, tablet split). Pre-flush rows are **pinned** in
`UnifiedPages` until durable, so without this cap the buffer would
grow linearly with the YB backlog — we've observed multi-GiB RSS
growth on co-located deployments under CPU pressure.

Set `backpressure_bytes = 0` to opt out (legacy unbounded growth).

**Materialiser-side backpressure.** The write-time materialisation
trigger does a **blocking** `mpsc::send` into the materialiser channel
(capacity `materialize_queue_capacity`, default 2000). When the
materialiser can't drain — usually because its own `read_chain_durable`
+ walredo + `put_pages` chain is bottlenecked on the dedicated YSQL
connection — the channel fills and the walreceiver blocks at the
send. This bounds in-flight materialisation work to ~5000 keys ×
(chain payload + image), keeping walredo memory under control even
when materialisation lags badly.

The read-side materialisation trigger uses `try_send` instead: a
GetPage that just walredo'd a long chain is allowed to enqueue an
image materialisation hint, but if the channel is full the hint is
silently dropped — the next read of the same hot key will re-trigger.
GetPage latency must not depend on materialiser progress.

### Observability

Three Prometheus metrics expose the backpressure state:

| Metric | Type | What to watch for |
|---|---|---|
| `pageserver_yb_pagewriter_backpressure_waits_total{kind="flush"}` | counter | sustained non-zero rate ⇒ YB INSERT throughput is below ingest. Either raise `backpressure_bytes` (delays the symptom, doesn't fix it), scale YB, or shed load |
| `pageserver_yb_pagewriter_backpressure_waits_total{kind="materializer"}` | counter | sustained non-zero rate ⇒ materialiser is the bottleneck. Either raise `materialize_queue_capacity` (only if memory permits) or relax `image_chain_limit` / `image_chain_payload_bytes_limit` to dispatch less aggressively |
| `pageserver_yb_pagewriter_unflushed_bytes` | gauge | should hover well below `backpressure_bytes`; persistent climb toward the cap = flush is lagging |
| `pageserver_yb_materializer_queue_depth` | gauge | should hover well below `materialize_queue_capacity`; climb toward the cap = materialiser is lagging |

When both `flush` and `materializer` counters are active, the flush path is
almost always the root cause: a slow `put_pages` blocks the
materialiser's own writes, which holds materialiser slots, which fills
the channel.

## Background image materialiser

Triggered **post-flush**: `PageWriter::put` keeps a per-key tally in
the sharded `delta_counts` DashMap (counter + payload-bytes; reset on
any image / `will_init` for the key — see `page_writer.rs:505-535`).
After every successful flush, `flush()` walks `delta_counts` for keys
whose tally crossed `image_chain_limit` (or
`image_chain_payload_bytes_limit`) during the flushed window and
dispatches a `MaterializeJob { key, target_lsn }` per key at the
flushed-window's max LSN — durable in YB by construction
(`page_writer.rs:571-619`, `992-1058`). Dispatch is a **blocking**
`mpsc::send`, so the walreceiver backpressures when the materialiser
falls behind.

The worker task (`ImageMaterializer::spawn`, one per timeline) drains
the queue:

1. `PageWriter::read_chain_durable(key, target_lsn)` — durable sources
   only (cache + PageStore), excluding pre-flush rows. Returns the
   LSN-descending chain truncated at the newest `will_init=TRUE`.
2. If chain is empty (deltas not yet flushed to YB), skip; the next
   post-flush trigger picks the key up.
3. If the chain head is already an image at `lsn ≥ target_lsn`,
   someone else materialised — skip.
4. `parse_chain` splits the chain into `(base_img, records)`. Set
   `actual_lsn = chain.first().lsn` (highest durable LSN we actually
   have — may be < `target_lsn` if some triggering deltas aren't yet
   durable; we make partial progress and re-trigger later).
5. `WalRedoManager::request_redo(key, actual_lsn, base_img, records,
   pg_version, RedoAttemptType::LegacyCompaction)` → 8 KiB image.
6. `PageStore::put_pages` the image row to YB. Durable.
7. `PageWriter::install_materialized_image` trims the cache for that
   key up to `actual_lsn` (dropping the now-superseded deltas — they
   stay in YB until GC) and `push_durable`s the new image into the
   cache. Future reads anchor on the image directly.

Failure / restart behaviour: on any error the job is logged and
dropped; the next delta past the (now-reset) counter re-enqueues. On
process restart, per-key counters start at zero — restart-survivor
keys with long chains keep reading via the slow path until 20 more
deltas (or 4 KiB combined delta payload) trigger materialisation. A
periodic backstop scan could close that gap later; not in v1.

GC of the superseded deltas in YB is Phase 6 — until then, the row
count grows. The materialiser pays only for read-latency wins; size
on disk grows monotonically.

## Generation fencing

`current_generation` on the `pageserver.timeline_state` row is the gate.
Sequence:

1. Storage controller's attach decision bumps the generation. yb-pageserver
   learns the new gen from the attach payload.
2. yb-pageserver opens the timeline: reads the current state row, asserts
   `row.current_generation == attach_gen`.
3. Row INSERTs run as independent chunks via
   `stream::iter(...).buffer_unordered(N)` against
   `pageserver.pages` / `slru_pages` / `meta` — there is no `BEGIN
   / COMMIT` wrapping the data INSERTs (`page_store.rs:351-457`). The
   generation fence is a **subsequent standalone UPDATE** in
   `advance_consistent_lsn` (`page_store.rs:1589-1659`):
   `UPDATE pageserver.timeline_state
    SET consistent_lsn=$3,
        prev_record_lsn=COALESCE($5, prev_record_lsn)
    WHERE tenant_id=$1 AND timeline_id=$2 AND current_generation=$4`.
   If the `UPDATE`'s row count is 0, a one-shot probe SELECT
   disambiguates "row missing entirely" (init bug) from
   `StaleGeneration { live_generation }`; on the latter the flush
   reports `StaleGeneration` to its caller and the pageserver tears
   down the timeline.

This collapses the upstream "every layer file carries its gen in the
name + every delete is gen-checked" machinery into one column.

## What yb-pageserver does NOT do

- **Compaction** — none. Image-layer creation moves from synchronous
  compaction to an async background image materialiser (see above).
  The `pageserver_compaction` crate has been deleted; no runtime or
  build-time dependency on it remains.
- **Local layer cache** — none. Every read hits YB. If profiling later
  shows a `(key, lsn) → page` LRU would help, that's a follow-up — but
  it'd live above PageStore, not as a copy of the upstream file-cache.
- **Disk eviction / heatmap / secondary cache** — none. No local files
  to evict.
- **Layer-file generations** — gone. Only the `current_generation`
  column on the timeline row matters.
- **GC** — not implemented yet. The plan is a separate cron that
  prunes `pageserver.pages` rows older than `applied_gc_cutoff_lsn`
  per key, retaining at least one image per chain. Until that lands,
  rows accumulate indefinitely.
- **S3 object-versioning time-travel recovery** — the upstream
  `time_travel_recover_tenant` path that walked an S3 bucket's
  versioned objects to roll a tenant back to a wall-clock timestamp
  is gone (along with S3 itself in lane E, 2026-06-01). **Time
  travel and branching are still on the roadmap** — we want both —
  but they need a YB-native implementation built on the row-per-LSN
  layout of `pageserver.pages` + `pageserver.timeline_state`. The
  shape: a tenant-level "recover to timestamp T" command resolves
  T → LSN via the existing `find_lsn_for_timestamp` helper (still
  intact), then creates a fresh timeline branched at that LSN. Branch
  creation is already wired in yb-light (`timeline branch` provisions a
  pageserver+safekeeper pair; see the `ancestor_timeline_id` /
  `ancestor_lsn` columns on `pageserver.timeline_state`). What's missing is the operator-facing
  command + the GC story for the source timeline's `pageserver.pages`
  rows that the branch depends on. **Planned phase, not yet
  numbered.**

## Compute ↔ pageserver timeouts

The compute opens libpq connections to the pageserver per backend
(`pgxn/yb_amp/libpagestore.c`). The relevant timers, with yb-light defaults
where they differ from upstream:

| Stage | GUC / location | Default | Notes |
|---|---|---|---|
| TCP connect | libpq, no explicit cap in `control_plane/src/endpoint.rs::local_pageserver_conf_to_conn_info` | **Unset** → OS TCP SYN-retry (~127 s on Linux) | Currently no `connect_timeout=` parameter is appended to the constructed connstring. |
| First WARN log on stalled response | `yb_amp.pageserver_response_log_timeout` (`libpagestore.c:87`) | **10 s** | Cosmetic; request keeps waiting. |
| Disconnect + reconnect on stalled response | `yb_amp.pageserver_response_disconnect_timeout` (`libpagestore.c:95`) | **30 s** *(was 150 s upstream)* | Lowered in yb-light: yb-pageserver is a same-DC, low-latency hop, so a stuck request is far more likely a pageserver-side stall than transient packet loss — drop and reopen the socket fast, don't wait for TCP retry backoff. |
| Reconnect attempts before erroring back to compute | `max_reconnect_attempts` (`libpagestore.c:83`) | **60** (1 s backoff each) | Combined with the disconnect timeout, a hard-down pageserver surfaces to the compute query in ~30 s + 60 × 1 s ≈ ~90 s. |
| Pageserver-side activation wait | none | — | The timeline is opened synchronously before listeners start; until then, computes get ECONNREFUSED and retry via the reconnect loop above. |

Pageserver doesn't apply an idle/recv timeout to inbound compute
libpq connections (the keepalive constants in `page_service.rs` are
gRPC-only).

## Implementation status

Per-phase task records live under [`../tasks/`](../tasks/). See
`tasks/README.md` for the index (id, title, status, severity, area).

> ⚠️ **Test debt** — many phases below landed without per-phase
> regression tests on the new code paths. Particularly thin
> coverage: the `consistent_lsn` UPDATE path, the
> `visible_remote_consistent_lsn` slot publish, the safekeeper-side
> WAL truncation feedback loop, the image materialiser's
> `read_chain_durable` cache-miss path, the basebackup-cache
> invalidation on LSN advance, and the generation-fence shutdown.
> Backfill is tracked alongside the relevant task files. The
> end-to-end suite (`test/e2e/`, a Rust harness — see
> [`test/e2e/README.md`](../test/e2e/README.md)) is the cross-seam
> backstop: it runs scenario tests + golden `.sql` workloads against a
> real `yb_light_local` + YB and scans the pageserver log for corruption
> markers after every test. The `.sql` workloads diff their result sets
> against checked-in `.out` goldens (#19), so silently-wrong reads
> no longer pass as long as no statement errors out. Run it with
> `test/e2e/run.sh fast|slow|all` before claiming a corruption-class fix
> (the `e2e-before-corruption-claim` skill enforces this).

## Code anchors

- `pageserver/src/page_store.rs` — `PageStore` row store; the
  image-anchored `read_chain` CTE + `scan_range`.
- `pageserver/src/page_writer.rs` — `PageWriter` buffered write path;
  pure passthrough (no walredo at write time); `read_chain` /
  `scan_range` blend in-RAM buffer + PageStore.
- `pageserver/src/page_server/timeline.rs` — `get_vectored_via_page_writer`
  drives the row-store read path; `chain_to_value_reconstruct_state`
  converts a chain into walredo inputs.
- `storage_controller/migrations/00000000000001_initial_schema/up.sql`
  — diesel-managed DDL for the `pageserver` schema (`pages`,
  `slru_pages`, `meta`, `timeline_state`) and the
  rest of `yb_storage` (#2). Replaces the earlier
  `control_plane/src/storage_controller.rs::ensure_yb_storage_schema`
  bootstrap, which has been removed.
- `architecture/yugabyte.md` — full DDL reference for
  `pageserver.pages`, `pageserver.slru_pages`, `pageserver.meta`,
  and `pageserver.timeline_state`.
