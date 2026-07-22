# Safekeeper

The safekeeper is the landing zone for the compute's WAL. The compute's
walproposer writes to it synchronously over a libpq replication connection;
pageservers read from it asynchronously over the same port. The safekeeper
persists every byte of WAL into **YugabyteDB** — durability and resilience
come from YB, the underlying quorum-replicated store, not from any local
state. A commit is durable exactly when the safekeeper has persisted its WAL
bytes into YB. The local filesystem holds only a rolling log file, a pidfile,
and a tmp directory.

One safekeeper process serves exactly **one branch** for its lifetime, and
each branch is served by exactly one safekeeper. That is the design, not a
degraded mode: replication, quorum, and crash durability all live inside YB,
so a second safekeeper would add no safety — only a second protocol racing
the first on a different view of the LSNs. There is no consensus between
safekeepers: no terms, no voting, no membership configuration. The
proposer↔acceptor handshake alone "elects" the walproposer (see
[Walproposer protocol](#walproposer-protocol)).

## Wire protocol

- **In**: the walproposer (`compute/pgxn/yb_amp/walproposer.c`) connects via
  the Postgres replication protocol on `listen_pg` and issues
  `START_WAL_PUSH (proto_version '3', ...)`, then streams a binary
  message-tagged WAL feed (protocol v3 — the only supported version;
  `SK_PROTO_VERSION_3` in `compute/safekeeper/src/safekeeper.rs`).
- **Out**: pageservers (and read replicas) connect via the same port and
  issue `START_REPLICATION [SLOT ..] [PHYSICAL] <lsn>`. The consuming
  pageserver(s) may be on a different node and a different build than this
  safekeeper (see [`high-level.md`](high-level.md) "PS/SK co-location is NOT
  an invariant"), so WAL leaves the safekeeper as the version-portable
  **interpreted-WAL** stream (`compute/safekeeper/src/send_interpreted_wal.rs`);
  the raw-WAL wire format is not offered to consumers.

The full command set on the replication port and the HTTP control surface are
documented in [`safekeeper-apis.md`](safekeeper-apis.md).

## Durable state in YugabyteDB

All per-branch durable state lives in two tables of the shared `yb_storage`
database, provisioned by
`controller/migrations/0002_compute_storage.up.sql`:

### Control file: `safekeeper.timeline_state`

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

`compute/safekeeper/src/control_file.rs` defines `YbControlFileStorage`, the
sole implementation of the control-file `Storage` trait
(`pub type ControlFileStorage = YbControlFileStorage;`). One row per branch,
one typed column per scalar of `TimelinePersistentState`
(`compute/safekeeper/src/state.rs`):

- `timeline_start_lsn` — where the branch's WAL history begins.
- `local_start_lsn` — where **this safekeeper's** persisted WAL begins
  (a guaranteed record boundary; can sit above `timeline_start_lsn`).
- `remote_consistent_lsn` — the pageserver's durable ingest frontier,
  advertised via `PageserverFeedback`; WAL below it is eligible for GC.
- `pg_version` / `system_id` / `wal_seg_size` — server identity, validated
  against every walproposer greeting.

There is no consensus state in the row — no term, no vote, no term history,
no membership — because the acceptor runs none.

Persistence paths:

- `Storage::persist()` — full-row `UPDATE`, run synchronously under the
  timeline `shared_state` write lock by state transitions that must be
  durable before replying (`handle_elected`, `handle_greeting`'s system-id
  update, `map_control_file`, the HTTP checkpoint route).
- `AdvisoryPersist::persist_remote_consistent_lsn()` — the periodic save
  (every `control_file_save_interval`, default 300 s) persists **only**
  `remote_consistent_lsn`, as a monotonic single-column
  `UPDATE ... SET remote_consistent_lsn = GREATEST(remote_consistent_lsn, $2)`.
  The timeline manager runs it as a background task **without** the
  shared_state lock, then folds the result back into the in-memory row
  mirror under a brief lock (`note_advisory_persisted`). Rationale: the
  manager loop is the sole servicer of residence-guard requests, and WAL
  appends need the shared_state write lock — holding that lock across a slow
  YB round trip freezes commits and times out every new WAL receiver.
  `GREATEST` makes the advisory write safe under any interleaving with the
  full-row persists: a late-landing advisory update can never regress the
  row. A failed save retries at most once per interval (the manager gates on
  both the persisted clock and the last-attempt clock, so a YB outage does
  not turn into a 300 ms retry storm).

### WAL: `safekeeper.wals`

```sql
CREATE TABLE safekeeper.wals (
    branch_id        TEXT     NOT NULL,
    bucket_id        SMALLINT NOT NULL,
    segno            BIGINT   NOT NULL,
    chunk_offset     BIGINT   NOT NULL,   -- byte offset of `data` within the segment
    chunk_size       INT      NOT NULL,
    data             BYTEA    NOT NULL,
    last_modified_us BIGINT   NOT NULL,
    PRIMARY KEY ((branch_id, bucket_id) HASH, segno, chunk_offset)
) SPLIT INTO 10 TABLETS;
```

`compute/safekeeper/src/yb_wal_storage.rs` owns this table. The table name is
hardcoded (`WALS_TABLE = "safekeeper.wals"`, always schema-qualified so it is
independent of the connection's `search_path`).

- **Chunk identity** is `(segno, chunk_offset)`. The writer never produces
  overlapping chunks (`truncate_wal` aligns the next write at exactly the
  truncation point), so offset order is write order and a same-key re-INSERT
  is by construction an idempotent replay of the same bytes — which is what
  makes it safe to run the hot-path INSERTs with
  `yb_enable_upsert_mode = true` (skip YB's read-before-write check).
- **`bucket_id` is placement, not identity** (`WALS_BUCKETS = 10`). The
  `(branch_id, bucket_id)` hash key spreads a branch's WAL across all ten
  tablets. The consumer stamps each assembled **batch** with one bucket,
  rotating one bucket per batch: every batch targets ONE tablet (atomic as a
  group on both write lanes, and the single-shard transaction fast path on
  YSQL — no distributed transaction), while sustained load rotates across
  all tablets. Readers cannot derive a chunk's bucket, so index-tight reads
  carry a `bucket_id IN (0..9)` hash-key list, which YSQL executes as
  batched per-bucket index probes rather than a scan.

## YB connections: `YbWalConn`

`YbWalConn::from_conf` opens three independent YSQL connections at boot
(auto-reconnecting `ysql_client::YsqlClient` wrappers, each with its own
`application_name` for attribution in `pg_stat_activity`):

- **`fpu_client`** (`safekeeper:yb_wal_fpu`) — the only lane with
  `yb_enable_upsert_mode = true`. Carries the hot-path WAL chunk INSERTs and
  the one-shot `timeline_state` INSERT of `control_file::create_new` (the
  non-idempotent INSERT needs upsert semantics so the client's
  retry-on-connection-drop loop can blindly replay it without a
  duplicate-key failure).
- **`read_client`** (`safekeeper:yb_wal_reads`) — exclusively the WAL
  sender's chunk SELECTs (`YbSegmentReader`). Keeping streaming reads off
  the system lane means a long control-file write or GC DELETE cannot stall
  the read-back path that walproposer commits depend on.
- **`system_client`** (`safekeeper:yb_wal_system`) — everything else:
  startup rehydration SELECTs, the GC DELETE, and the idempotent
  control-file ops (restore / persist / delete / list).

With `hot_path = "tsclient"` in `yb_storage_conn` (see
[`yugabyte.md`](yugabyte.md) §"Direct-to-tserver hot path"), `YbWalConn`
additionally opens a pure-Rust thin-RPC client (`compute/libs/yb_tsclient`,
pggate RPC to a tserver's RPC port) to `safekeeper.wals` — fail-fast
health-checked at startup. The hot-path WAL write batch then runs as ONE
Perform per batch (a single-tablet atomic write batch — a crash mid-flush
cannot leave a hole), and the WAL sender's fetch issues all
2×`WALS_BUCKETS` (arm, bucket) read ops as ops of ONE Perform served at a
single read time — one snapshot by construction. The tsclient lane retries
across tservers round-robin with no YSQL fallback. Control file,
`timeline_state`, GC, truncation, and rehydration always stay on the YSQL
lanes. `PGSQL_UPSERT` write ops carry skip-read-before-write semantics
natively, so the `yb_enable_upsert_mode` GUC has no counterpart on this
lane.

## Write path: `YbWalStorage` and the flush protocol

`YbWalStorage` implements the WAL `Storage` trait
(`compute/safekeeper/src/wal_storage.rs`) and is the only WAL storage
(`pub type WalStore = YbWalStorage;`). It wraps a `WalBookkeeping` — a pure
in-memory tracker with **no filesystem I/O** — and drives a
producer/consumer pipeline into `safekeeper.wals`:

```
write_wal()                     consumer task                     YB
─────────                       ─────────────                     ──
split byte range into
PendingChunks at segment
boundaries; stamp each with
chunk_offset and end_lsn

push onto mpsc::Sender ─────►   rx.recv() (block)
  (bounded, cap 1024)               │
update submitted_max_lsn            │
return                              │
                                ┌── batch drain loop:
                                │    try_recv ×N until
                                │    FLUSH_ROWS_THRESHOLD (50)
                                │    or FLUSH_BYTES_THRESHOLD (4 MiB)
                                │    or channel empty
                                │
                                │   if under threshold:
                                │    select! on rx.recv,
                                │    flush_notify, sleep(2 ms)
                                │
                                │   stamp batch with ONE bucket
                                │   coalesce contiguous chunks
                                │   into single rows
                                │
                                │   flush_batch: one multi-row
                                │   INSERT (ysql) or one Perform
                                │   (tsclient)                 ───►  atomic
                                │                                    batch
                                │   populate WalCache
                                │   durable_lsn.store(highest)
                                │   watch_tx.send(Healthy(highest))
                                └── back to drain loop
```

Key properties:

- **`write_wal()` never waits for YB.** It parses the range into one
  `PendingChunk` per segment (chunks never span segments), pushes them onto
  the bounded channel, advances `submitted_max_lsn`, and returns. Each
  chunk's `data` is an `Arc<Vec<u8>>` shared with the read-side `WalCache`
  entry — no double allocation.
- **Backpressure.** The channel is bounded (`CHUNK_CHANNEL_CAPACITY = 1024`).
  If the consumer stalls, `tx.send().await` blocks and the walproposer
  stalls, rather than safekeeper memory growing without bound.
- **Batching.** The consumer drains greedily, then waits up to
  `ACCUMULATION_MAX` (2 ms) for more chunks — a Nagle-style window that lets
  bursty arrivals amortize the fixed per-statement DocDB + Raft cost
  (single-row ≈ 0.6 ms; 50-row batch ≈ 2.5 ms against local YB). The wait is
  short-circuited by `flush_notify` (pinged by every flush/truncate), so
  commit-driven workloads skip it. Contiguous chunks of a batch are
  coalesced into single rows (`coalesce_runs`; runs break at segment
  boundaries and byte discontinuities), so the typical batch INSERTs one
  row.
- **Durability watermark.** After each successful batch the consumer stores
  the batch's highest `end_lsn` into the `durable_lsn` atomic and publishes
  it on a `watch<DurableState>` channel. `YbWalStorage::flush_lsn()` returns
  `min(bookkeeping flush_lsn, durable_lsn)` — **nothing is ever advertised
  past what has landed in YB.** The `AppendResponse.flush_lsn` the
  walproposer treats as the commit boundary is therefore always a
  YB-durable LSN.
- **Durability acks are asynchronous.** The steady-state flush
  (`yb_flush(YbFlush::Notify)`, driven by the acceptor's `FlushWAL`
  handling) only wakes the consumer; it does not wait. The WAL acceptor
  subscribes to the durability watch and emits an `AppendResponse` each time
  the watermark advances, so ingest never blocks on the YB round trip and
  synchronous commits release as soon as their bytes are durable.
  The `YbFlush` modes are:
  - `NotifyAndWait` — wake, wait for the watermark to reach
    `submitted_max_lsn`, then advance the bookkeeping watermarks. Used where
    a synchronous durable point is required: the greeting
    (`handle_greeting` flushes so the advertised `flush_lsn` is exact),
    segment initialization, and truncation.
  - `Notify` — wake only. The steady-state commit path.
  - `LocalAdvanceOnly` — advance the bookkeeping watermarks to the already
    published durable point. The durability-ack path
    (`yb_emit_append_response`).
- **Bounded blocking waits.** `wait_for_durable` logs progress every 10 s
  and gives up after 60 s. Every caller sits inside `process_msg`, holding
  the timeline shared_state write lock; an unbounded wait there starves the
  manager (fair RwLock — its reads queue behind the writer), which then
  cannot issue residence guards, and the whole timeline wedges. Timing out
  converts that wedge into a dropped connection: the lock releases and the
  walproposer retries.
- **`truncate_wal(end_pos)`** first drains the consumer (notify + wait), so
  a buffered chunk past `end_pos` cannot land after the cut. It refuses an
  `end_pos` above `durable_lsn` (accepting one would advertise WAL the row
  store cannot return). It then physically trims the table
  (`truncate_wals_above`: DELETE chunks starting at/after `end_pos`, and an
  UPDATE that shortens the one chunk straddling it via `substring`), drops
  the affected `WalCache` range, and resets the bookkeeping watermarks.
- **`remove_up_to(segno)`** is WAL GC: a DELETE of all rows with
  `segno < segno_up_to`, bounded below by `delete_lower_bound` (seeded from
  `min(segno)` at construction, advanced after each successful delete) so
  the range predicate stays tight instead of scanning tombstones from zero.
  It does not wait on the watermark — by construction it only targets
  segments far below the writer's position. The matching `WalCache` ranges
  are dropped first. The GC horizon is
  `min(cfile remote_consistent_lsn, flush_lsn, slowest active walsender)`
  (`compute/safekeeper/src/remove_wal.rs` + the manager's laggard clamp) —
  WAL lives only in these rows, so trimming past a reader would be
  unrecoverable data loss for it.
- **Failure handling.** On a batch write error (after `YsqlClient`'s own
  reconnect-and-retry-once has given up), the consumer publishes
  `DurableState::Failed(msg)` and exits. Every durability surface then
  fails loudly: blocking waiters propagate the error, the producer's next
  `tx.send` fails and `write_wal` returns the captured error, and the
  acceptor's durability-watch arm bails so the connection terminates rather
  than hanging on a frozen watermark. The walproposer reconnects.
- **`initialize_first_segment(init_lsn)`** generates the standard 16 MiB
  segment header via `postgres_ffi::generate_wal_segment` and pushes it
  through the consumer like a regular write, waiting for durability before
  returning. Run once per timeline (from `handle_elected` when
  `flush_lsn == INVALID`); it makes the first segment `pg_waldump`-able,
  since the compute streams no header for it. The 16 MiB blob is
  deliberately not cached.

### `WalBookkeeping`

The in-memory half (`compute/safekeeper/src/wal_storage.rs`): it tracks
`write_lsn` / `write_record_lsn` / `flush_lsn` / `flush_record_lsn` and
feeds every written byte through a `WalStreamDecoder` so the record-boundary
watermarks follow actual WAL records. `write_wal` rejects rewrites
(`startpos < write_lsn`) and gaps (`startpos > write_lsn`); the same guards
exist one level up in `handle_append_request`. `flush_wal` aligns the flush
watermarks with the write watermarks (durability is `YbWalStorage`'s job);
`truncate_wal` resets all watermarks to the cut; `remove_up_to` is a no-op
at this layer.

## Read path: `WalReader` and the fallback chain

`WalReader` (`compute/safekeeper/src/wal_storage.rs`) serves WAL bytes to
the interpreted-WAL sender. There are no local segment files; every byte
comes from memory or YB. For a read at position `pos` the chain is:

1. **Synthesized pre-history.** If `pos < timeline_start_lsn` (same
   segment), serve bytes from a generated empty segment — the compute never
   streamed anything below the timeline start.
2. **`YbSegmentReader`** (`compute/safekeeper/src/yb_wal_storage.rs`), a
   lazy chunk-streaming `AsyncRead` opened per segment:
   1. Serve from the currently held chunk if it covers `pos` — but only up
      to the flush frontier that was live when the chunk was acquired
      (`current_flush_bound`, converted into the segment's frame). Bytes
      above that frontier were uncommitted at fetch time and a walproposer
      reconnect can truncate-and-rewrite them; serving them from a held
      chunk after the frontier advances would splice old bytes into the
      stream (the "invalid xlog page header" torn read, #139). Past the
      bound the chunk is dropped and re-acquired.
   2. **`WalCache` lookup** (`compute/safekeeper/src/wal_cache.rs`) — a
      bounded in-memory FIFO cache keyed `(segno, chunk_offset)`, capacity
      `wal_cache_size_mib` (default 256 MiB, 0 disables). It is populated
      only with **committed** chunks: by the consumer after a successful
      batch write, and by reader fetches for chunks lying fully below the
      flush frontier at fetch-issue time. Committed WAL is immutable, so a
      cached chunk can never be shadowed by a rewrite. The cache is
      invalidated in step with the YB DELETEs (`truncate_after`,
      `remove_segnos_below`). A single `std::sync::Mutex` suffices: writes
      touch high offsets, reads low offsets, and the lock covers only
      O(log n) metadata operations.
   3. **YB fetch** — one round trip fetching the covering chunk **and** a
      forward prefetch window (`FETCH_FORWARD_CHUNKS = 64` /
      `FETCH_FORWARD_BYTES = 4 MiB`) in a single snapshot: on YSQL, one
      `UNION ALL` statement (covering arm: highest `chunk_offset < pos`,
      `LIMIT 1`; forward arm: ascending from `pos`); on tsclient, all
      (arm, bucket) ops in one Perform at one read point. A single snapshot
      is load-bearing — two separate autocommit SELECTs would let a
      truncate+rewrite commit between them and splice old-term and new-term
      bytes (#139). Fetched committed chunks warm the cache; the fetch runs
      on a spawned task bridged by a `oneshot` (the query futures are not
      `Sync`, and `open_segment` must return a `Sync` reader).
   4. **`XLOG_SWITCH` padding synthesis.** If neither cache nor YB has a
      covering chunk but the committed frontier has already passed the end
      of this segment, the gap is the zero padding a `pg_switch_wal()`
      leaves: the compute streams WAL only up to the switch record, never
      the padding, so no rows exist for it. The reader synthesizes zeros,
      which the decoder — in its skip-to-next-segment state after the
      switch record — consumes (#181).
   5. Otherwise: `io::ErrorKind::UnexpectedEof` ("YB WAL hole"). Failing
      loudly beats serving zeros and letting the pageserver decode
      `xlp_magic=0` three layers downstream.

The flush frontier flows in from `StreamingWalReader`
(`compute/safekeeper/src/wal_reader_stream.rs`), which publishes the
committed end (`commit_lsn ≡ flush_lsn`) into a shared `AtomicU64` before
every read.

On the sender side, a WAL **decode** error in the interpreted reader
(`compute/safekeeper/src/send_interpreted_wal.rs`) is treated as
recoverable: the reader exits cleanly, the pageserver walreceiver reconnects
at its last applied record boundary and re-reads authoritative bytes from
YB. A decode-error **tripwire** then classifies the failure by re-reading
the failing range on a fresh snapshot: a clean re-decode means a transient
read-time splice; a repeat failure means durable corruption (see the
`safekeeper_wal_reader_decode_tripwire_total` metric in
[`safekeeper-metrics.md`](safekeeper-metrics.md)).

## Walproposer protocol

Protocol v3 only, on both ends (`compute/pgxn/yb_amp/` and
`compute/safekeeper/src/safekeeper.rs`); the protocol is used exclusively
between these two components. Message flow on every (re)connect:

- **`ProposerGreeting`** (P→A, tag `g`): `branch_id`, `pg_version`,
  `system_id`, `wal_seg_size`. The acceptor validates all four against the
  control file (major-version or `wal_seg_size` mismatch is fatal; a
  nonzero, differing `system_id` is adopted and persisted —
  `sync-safekeepers` sends 0). It then flushes
  (`flush_wal`, the synchronous mode) so its advertised end-of-WAL is exact.
- **`AcceptorGreeting`** (A→P, tag `g`): just **`flush_lsn`** — the
  safekeeper's durable end-of-WAL. There is no voting round; the greeting
  alone elects the walproposer, which derives its resume point from this
  LSN.
- **`ProposerElected`** (P→A, tag `e`): `start_streaming_at`. The acceptor
  asserts `start_streaming_at >= flush_lsn` (never truncate committed data
  — with `commit_lsn ≡ flush_lsn` the two invariants coincide), initializes
  the first segment if this is the timeline's first WAL, truncates its WAL
  to `start_streaming_at`, seeds `timeline_start_lsn` / `local_start_lsn`
  on the very first election (where `start_streaming_at` is the timeline's
  WAL start), floors `remote_consistent_lsn` at the timeline start, and
  force-persists the control file so `postgres --sync-safekeepers` polls a
  durable, aligned LSN.
- **`AppendRequest`** (P→A, tag `a`): `begin_lsn`, `end_lsn`, WAL bytes
  (bounded by `MAX_SEND_SIZE`). Gap/rewrite guards reject non-sequential
  writes — a reconnect must go through `ProposerElected` to move the
  pointer.
- **`AppendResponse`** (A→P, tag `a`): `flush_lsn` (durability-capped) +
  hot-standby feedback (aggregated from standby walsenders) + the latest
  `PageserverFeedback` when one exists.

**`flushLsn` semantics**: the walproposer treats `AppendResponse.flush_lsn`
as the durable-commit boundary. Because `flush_lsn()` is capped at the
YB-confirmed `durable_lsn`, an ack can never run ahead of YB; a crash
between `write_wal()` returning and the batch landing in YB is a no-op for
the compute — no commit ack was sent, and the compute re-streams from the
greeting's `flush_lsn` on reconnect.

### Acceptor scheduling (`WalAcceptor`, `receive_wal.rs`)

Network reads and WAL processing run on separate tasks connected by a
bounded queue (`MSG_QUEUE_SIZE = 256` messages). The acceptor rewrites each
incoming `AppendRequest` to `NoFlushAppendRequest` and drives flushes
itself:

- A `FlushWAL` fires on a 2 ms ticker (`FLUSH_INTERVAL_YB_WAL`). Every flush
  is a candidate YB statement with a fixed per-statement cost, so writes are
  allowed to pile up between ticks and coalesce into larger batches; 2 ms
  adds at most ~2 ms to commit latency while unlocking multi-row batches.
  There is no flush-immediately-on-empty-queue path — that would defeat the
  batching.
- The **durability-watch arm** emits an `AppendResponse` whenever the
  consumer publishes a watermark advance
  (`Timeline::yb_emit_append_response` → `YbFlush::LocalAdvanceOnly`),
  releasing waiting synchronous commits without ever blocking ingest. Two
  guards keep this protocol-legal: the arm is gated on the connection having
  processed `ProposerElected` (an `a` on the wire before the `g` reply is a
  handshake violation the walproposer kills the connection over — unguarded,
  a freshly cloned watch's stale seen-version fires `changed()` immediately
  and produces a permanent reconnect storm; regression-covered by the
  `sk_split_churn` integration scenario), and the current watch value is
  marked seen at subscription. If the watch reports `Failed` or the sender
  drops unexpectedly, the acceptor bails so the failure surfaces instead of
  hanging.
- WAL is flushed once more on disconnect if appends are pending.

`handle_append_request` returns an `AppendResponse` only when `flush_lsn`
actually advanced (e.g. crossing a durability publish); otherwise the
periodic flush ticks and the durability watch carry the acks.

## Backpressure and feedback

- **Ingest backpressure**: bounded acceptor queue (256 messages) in front of
  a bounded chunk channel (1024 chunks). A slow YB propagates cleanly:
  consumer stalls → `write_wal` blocks → acceptor queue fills → network
  reader blocks → the walproposer's TCP connection backpressures the
  compute.
- **Pageserver feedback**: pageservers reply on the replication stream with
  `PageserverFeedback` (keyed by the `NEON_STATUS_UPDATE_TAG_BYTE` message
  tag, `compute/safekeeper/src/send_wal.rs`). The safekeeper records it
  per-walsender, advances the in-memory `remote_consistent_lsn`, and
  broadcasts the feedback to all connected walproposers, which use it for
  compute-side backpressure. `corruption_detected` in the feedback latches
  the `safekeeper_ps_corruption_detected` gauge.
- **Standby feedback**: hot-standby feedback and standby status replies from
  read replicas are aggregated (min over xmins, min over reply LSNs) and
  carried back to the compute in every `AppendResponse`.

## Timeline lifecycle

`GlobalTimelines` (`compute/safekeeper/src/timelines_global_map.rs`) is the
registry. Because one process serves one branch, it is a single
`Option<(BranchId, slot)>` plus one tombstone slot — not a map. The external
API keeps collection shapes (`get`, `get_all() -> Vec`, `timelines_count`)
so call sites read uniformly.

- **Identity**: `branch_id` in `safekeeper.toml` bakes the process's single
  branch (`SafeKeeperConf::yb_branch_id()`, required whenever
  `yb_storage_conn` is set). It matters because `safekeeper.timeline_state`
  is shared by the whole cluster — without a baked identity the process
  could not tell its own branch from everyone else's. Consequences:
  - `GlobalTimelines::init()` enumerates the table's branch ids and loads
    **only** the baked branch's row (if it exists yet — a fresh bootstrap
    creates it on demand), never the whole table.
  - `SafekeeperPostgresHandler::process_query` rejects any replication-port
    connection whose `branch_id` differs from the baked identity before it
    can touch the registry, so a misrouted walproposer cannot create or
    mutate a foreign branch here.
- **Creation**: either explicitly via `POST /v1/branch`, or implicitly by
  the first walproposer `START_WAL_PUSH` handshake
  (`allow_timeline_creation`, default true). Creation writes the
  `timeline_state` row (`create_new`, on the upsert lane) and loads the
  timeline; the two-phase `CreationInProgress` marker in the registry keeps
  concurrent creators out.
- **Deletion** (`DELETE /v1/branch/:branch_id`): cancel + gate-close the
  timeline, delete its (empty) local directory, and — unless `only_local` —
  delete the `safekeeper.timeline_state` row in a background task. An
  in-memory tombstone then blocks on-demand re-creation of the just-deleted
  branch; tombstones expire after 24 h (`housekeeping`) and do not survive
  restart.
- **Activity**: the per-timeline manager task
  (`compute/safekeeper/src/timeline_manager.rs`) recomputes `is_active`
  (live computes, or pageserver `remote_consistent_lsn` behind `flush_lsn`)
  on every iteration, drives the periodic control-file save and WAL GC, and
  services residence-guard requests. The active set feeds the per-timeline
  metrics collector.

## Recovery and restart

Startup rebuilds everything from YB; wiping the safekeeper's local directory
loses nothing but logs.

1. **Control file**: `SharedState::restore` loads the branch's
   `timeline_state` row (`YbControlFileStorage::restore_new`).
2. **End-of-WAL rehydration** (`YbWalStorage::new`):
   - `load_write_lsn` reads the raw end of the byte stream with one
     **max-end probe per bucket** (metadata only, never `data`):
     `SELECT segno, chunk_offset + chunk_size ... ORDER BY segno DESC,
     chunk_offset DESC LIMIT 1` per bucket; the global max across buckets is
     the end of WAL. Within a segno the top-offset row necessarily has the
     highest end because rows never overlap. No hole detection is needed:
     every consumer batch carries one bucket and lands atomically (one write
     batch on tsclient, one single-shard statement on YSQL), and batches are
     written sequentially — so the durable row set is always a contiguous
     prefix of the WAL stream. If either property is ever relaxed (batches
     spanning tablets, or concurrent in-flight batches), hole detection must
     come back.
   - The raw probe can land **mid-record** (an ungraceful compute shutdown
     leaves a torn trailing page) or **short of the true end** (after an
     `XLOG_SWITCH`, whose zero padding is never stored as rows). A decoder
     replay refines it: `find_anchor` walks back to the nearest fresh-record
     page — floored at `local_start_lsn`, a guaranteed record boundary, so a
     record spanning all the way back to a mid-page branch start cannot
     drive the walk below the store (#181) — then `find_end_of_wal` scans
     forward to the last complete record boundary.
   - **Torn tail** (`end < raw`): truncate back to the boundary; the compute
     re-streams from there. The torn rows are physically deleted
     (`truncate_wals_above`) so they are not re-counted next restart;
     best-effort, since the reader is bounded by `write_lsn` anyway.
   - **Trailing `XLOG_SWITCH`** (`end > raw`): `pg_switch_wal()` rounds the
     record's end up to the next segment boundary, past padding the compute
     never streams. The decoder's answer is honored — clamping to the raw
     end would put `write_lsn` inside the padding, where the compute
     resumes, the reader reads zeros, and the safekeeper wedges (#181).
   - On any scan failure the raw max is used, so startup never gets worse
     than the probe.
3. **GC lower bound**: `load_min_segno` seeds `delete_lower_bound` so the
   first post-restart GC DELETE is range-bounded.
4. The durability watermark and `durable_lsn` start at the rehydrated
   `write_lsn`, so an immediate flush with nothing in flight returns at
   once. Committed-but-unacked rows from a crashed run are absorbed as
   durable by the max-end probe rather than discarded.
5. On the first walproposer reconnect, the greeting advertises the
   rehydrated `flush_lsn` and `ProposerElected` truncates/resumes there.

## Run / configure

The binary is `yb-safekeeper`
(`compute/safekeeper/src/bin/yb-safekeeper.rs`). All configuration lives in
`<datadir>/safekeeper.toml`
(`compute/libs/safekeeper_api/src/config.rs::SafekeeperConfigToml`), written
by the launcher (`SafekeeperNode::start` in yb_amp_local) and read once at
startup. The schema is `deny_unknown_fields`: a misspelled or stale key
aborts startup instead of being silently ignored. The only CLI flags are:

```
yb-safekeeper -D <datadir> [--init] [--dump-control-file <path>]
```

`--init` parses config, logs, and exits. `--dump-control-file` always exits
with an error directing the operator to read the state from YB — there is no
local control file to dump.

A representative `safekeeper.toml`:

```toml
branch_id = "77ffde89..."
listen_pg = "127.0.0.1:5474"
listen_http = "127.0.0.1:7686"
yb_storage_conn = "{ host_ports = '127.0.0.1:5433', user = 'yugabyte', dbname = 'yb_storage', sslmode = 'require' }"
# tuning knobs (heartbeat_timeout, control_file_save_interval,
# wal_cache_size_mib, scale_factor, ssl_*, ...) default when omitted.
```

- `yb_storage_conn` is the single YB connection block (a string-encoded
  inline TOML table, the same `[*.yb]` shape the pageserver and
  endpoint_storage consume) used for both `safekeeper.timeline_state` and
  `safekeeper.wals` — same DB, two tables; the WAL table name is not
  configurable. Setting it is what puts the safekeeper in YB-WAL mode
  (`SafeKeeperConf::is_yb_wal_mode`); the deployment always sets it.
- `sslmode` / `sslrootcert` in that block select TLS for the YB connections:
  `disable` → plain TCP; `require` (no root cert) → rustls with an
  accept-all verifier (suitable for YBA universes with self-signed
  per-universe CAs, which default to TLS-mandatory `hostssl` rules);
  `verify-ca` + `sslrootcert` → verification against the supplied CA bundle.
- `branch_id` bakes the single-branch identity (see
  [Timeline lifecycle](#timeline-lifecycle)); yb_amp_local stamps it at
  provision time (`allocate_safekeeper_conf`).
- Defaults: `listen_pg` 127.0.0.1:5454, `listen_http` 127.0.0.1:7676,
  `control_file_save_interval` 300 s, `wal_cache_size_mib` 256 scaled by
  `scale_factor`, `heartbeat_timeout` 5 s.

The process runs three additional tokio runtimes (`WAL_SERVICE_RUNTIME`,
`HTTP_RUNTIME`, `BACKGROUND_RUNTIME`); `SIGUSR2` dumps suspended tasks from
all of them — the WAL acceptors, the usual suspects in a wedge, live on the
WAL service runtime and would otherwise be invisible in a dump. A background
watcher samples filesystem usage of the workdir every
`global_disk_check_interval` and latches `GLOBAL_DISK_LIMIT_EXCEEDED` when
usage exceeds `max_global_disk_usage_ratio × capacity`; `process_msg` fails
appends while the flag is set. `max_timeline_disk_usage_bytes` (default 0 =
disabled) bounds the retained WAL span per timeline the same way.

What's on the local filesystem:

```
<workdir>/
├── safekeeper.toml           (config, rewritten by the launcher on start)
├── safekeeper-N.log          (rolling log)
├── safekeeper.pid            (running-process lock)
├── tmp/                      (timeline-creation scratch)
└── <branch_id>/              (empty — no control file, no segments)
```

In the dev cluster the safekeeper is co-located under the endpoint that owns
its branch — a launcher convenience, not a design invariant; the consuming
pageserver(s) may live elsewhere (see [`high-level.md`](high-level.md)).
Deleting the whole directory and restarting rebuilds everything from YB.

## Code layout

- `compute/safekeeper/src/safekeeper.rs` — the acceptor: message
  parsing/serialization (v3), greeting handshake, truncate-on-elected,
  append/flush handling.
- `compute/safekeeper/src/receive_wal.rs` — walproposer connection handling,
  the `WalAcceptor` loop (flush ticker, durability-watch acks).
- `compute/safekeeper/src/control_file.rs` — `YbControlFileStorage`
  (= `ControlFileStorage`) + `AdvisoryPersist`.
- `compute/safekeeper/src/wal_storage.rs` — WAL `Storage` trait,
  `WalBookkeeping`, `WalReader`.
- `compute/safekeeper/src/yb_wal_storage.rs` — `YbWalConn`, `YbWalStorage`
  (producer/consumer writer, restart rehydration), `YbSegmentReader`.
- `compute/safekeeper/src/wal_cache.rs` — the committed-chunk read cache.
- `compute/safekeeper/src/send_wal.rs` / `send_interpreted_wal.rs` /
  `wal_reader_stream.rs` — the WAL sender stack (interpreted protocol,
  optional reader fan-out via `wal_reader_fanout`).
- `compute/safekeeper/src/timeline.rs` — `Timeline`, `SharedState`,
  residence guards.
- `compute/safekeeper/src/timeline_manager.rs` — per-timeline background
  manager (control-file saves, WAL GC, guard issuance).
- `compute/safekeeper/src/timelines_global_map.rs` — the single-slot
  `GlobalTimelines` registry.
- `compute/safekeeper/src/bin/yb-safekeeper.rs` — entry point; reads
  `safekeeper.toml` into `SafeKeeperConf`.
- `compute/libs/safekeeper_api/src/config.rs` — `SafekeeperConfigToml`, the
  on-disk schema shared by the binary and yb_amp_local.

## Performance characteristics

- **`write_wal()`** — microseconds: chunk parsing plus a channel push, no YB
  round trip (unless the bounded channel backpressures).
- **Commit latency** — one consumer batch's worth of work: at most the 2 ms
  flush tick + accumulation window, plus one batched write round trip
  (~0.6 ms single-row, ~2.5 ms 50-row against local YB; tens of ms across a
  real network). Acks ride the durability watch, so ingest and durability
  overlap.
- **Steady-state throughput** — bounded by the consumer's batched write
  round trip; the producer fills the next batch while the previous one is in
  flight, approaching single-connection YB write saturation (~14 K rows/s at
  50-row batches against local YB).
- **WAL sends** — cache hits are memory reads; misses amortize one YB round
  trip over up to 64 chunks / 4 MiB of prefetch.
- **Restart** — ten single-row index probes + a bounded record-boundary
  replay over the trailing chunks; independent of total WAL size.
- **YB connection failures** — absorbed by `YsqlClient` (lazy reconnect,
  one in-flight retry); a sustained outage surfaces as
  `DurableState::Failed`, a dropped walproposer connection, and clean
  consumer exit.
