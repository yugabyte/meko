# Safekeeper

The safekeeper is the durable, fault-tolerant landing zone for the compute's
WAL. It's a Raft acceptor — N safekeepers form a quorum-replicated WAL log
that the compute writes to synchronously and the pageserver reads from
asynchronously.

In yb-light, **all of the safekeeper's durable state lives in YugabyteDB**.
The local filesystem holds only a log file and a pidfile.

## Inherited from Neon

### Raft acceptor

`safekeeper/src/safekeeper.rs` implements a single-decree Paxos / Raft-ish
acceptor:

- A `term` and a `vote` are maintained per timeline.
- Each new walproposer (the compute's WAL push side) starts an election
  bumping the term; the safekeeper votes once per term.
- After election the proposer sends `START_REPLICATION` from a chosen LSN
  and the safekeeper appends.

### Wire protocol

- **In**: walproposer connects via Postgres replication protocol on
  `--listen-pg`; uses the `START_WAL_PUSH` extended query plus a custom
  binary stream of WAL records framed by message type.
- **Out**: pageservers (and any read replica) connect via the same port and
  issue `START_REPLICATION PHYSICAL <lsn>` to stream WAL forward.

### Per-timeline state

- **Control file**: 388 B binary blob holding the consensus state (`term`,
  `vote`, peer set, `commit_lsn`, `backup_lsn`, `peer_horizon_lsn`,
  `remote_consistent_lsn`, `eviction_state`, etc.). Atomically renamed
  (`safekeeper.control.partial` → `safekeeper.control`) per update.
- **WAL segments**: vanilla Postgres XLog 16 MiB segment files named
  `XLogFileName(PG_TLI, segno, wal_seg_size)` → e.g.
  `000000010000000000000001`.

### Background tasks

- `wal_backup`: once a segment is sealed (and the offloader is elected via
  the storage_broker), uploads it through `GenericRemoteStorage` for
  long-term retention.
- `wal_backup_partial`: uploads the in-flight last segment periodically.
- Periodic control-file flush every `control_file_save_interval`
  (default 300 s) plus on-state-change flushes via `finish_change()`.

### Code layout (unchanged module names)

- `safekeeper/src/control_file.rs` — control-file `Storage` trait + impls.
- `safekeeper/src/wal_storage.rs` — WAL `Storage` trait + `PhysicalStorage` impl + `WalReader`.
- `safekeeper/src/wal_backup.rs` — sealed-segment backup, partial-backup task,
  `WalBackup` struct (now also holds the live-WAL YB connection).
- `safekeeper/src/timeline.rs` — `SharedState`, `Timeline::load_timeline`.
- `safekeeper/src/timelines_global_map.rs` — `GlobalTimelines`, startup scan.
- `safekeeper/src/safekeeper.rs` — the consensus state machine.
- `safekeeper/src/bin/safekeeper.rs` — entry point + CLI parsing.

## Changes in yb-light

### 1. Control file → YB-backed (commit `c4c7c1350`)

New `control_file::ControlFileStorage` enum dispatches between the existing
`FileStorage` and a new `YbControlFileStorage`:

```
+- enum ControlFileStorage
   +- File(FileStorage)      <-- existing local-FS impl
   +- Yb(YbControlFileStorage) <-- new: stores the 388B blob in yb_storage.blobs
                                  under the key
                                  safekeeper-control/<tenant>/<timeline>
```

The on-the-wire bytes are *bit-identical* to what `FileStorage` writes —
magic (4 B) + version (4 B) + serialized `TimelinePersistentState` + crc32c
(4 B). So a per-timeline migration between backends is a copy of the blob.

- `SafeKeeperConf::control_file_backend: ControlFileBackend { File | RemoteStorage }`
- CLI: `--control-file-backend file|remote-storage` (default `file`).
- When `remote-storage` is selected, the control file uses the same
  `GenericRemoteStorage` constructed from `--remote-storage` (a `YbStore`
  config in our setup).
- `StateSK::Loaded` / `Offloaded` and `TimelineState<CTRL>` now use
  `ControlFileStorage` as their `CTRL` type parameter (was `FileStorage`).
- `SharedState::restore` and `Timeline::load_timeline` became `async` to
  support the YB load path.

### 2. Live WAL → YB-mirrored (Phase 1, commit `35e610db6`)

New `yb_wal_storage.rs` module with `YbWalStorage` (a wrapper) and a
`WalStore` dispatch enum:

```
+- enum WalStore
   +- Physical(PhysicalStorage)  <-- unchanged path; no YB
   +- Yb(YbWalStorage)           <-- writes every WAL chunk into yb_storage.wals
                                     inside a wrapped PhysicalStorage
```

Schema (operator-provisioned in `yb_storage` DB):

```sql
CREATE TABLE wals (
    tenant_id        TEXT    NOT NULL,
    timeline_id      TEXT    NOT NULL,
    segno            BIGINT  NOT NULL,
    chunk_seq        INT     NOT NULL,   -- monotonic per (tenant, timeline, segno)
    chunk_offset     BIGINT  NOT NULL,   -- byte offset of `data` within the segment
    chunk_size       INT     NOT NULL,
    data             BYTEA   NOT NULL,
    last_modified_us BIGINT  NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id) HASH, segno, chunk_seq)
);
```

The write path is **producer / consumer** (commits
`485a76ece`, `fba8410e7`):

```
write_wal()                     consumer task                YB
─────────                       ─────────────                ──
parse byte range into
PendingChunks at segment
boundaries; stamp each
with monotonic chunk_seq
and end_lsn

push onto mpsc::Sender ─────►   rx.recv() (block)
  (bounded, cap 1024)               │
                                    │
update submitted_max_lsn            │
return                              │
                                    │
                                ┌── batch drain loop:
                                │    try_recv ×N until
                                │    FLUSH_ROWS_THRESHOLD (50)
                                │    or FLUSH_BYTES_THRESHOLD (4 MiB)
                                │    or channel empty
                                │
                                │   if under threshold:
                                │    select! on rx.recv,
                                │    flush_notify, sleep(2 ms)
                                │    — see "force drain" below
                                │
                                │    build one
                                │    INSERT INTO wals
                                │      VALUES (...), (...), ...   ───►
                                │                                       │
                                │    watch_tx.send(DurableState::         │ atomic
                                │      Healthy(highest_end_lsn))        │ batch
                                └── back to drain loop                   │
```

- **`write_wal()` does not wait for YB** — it pushes onto the producer
  channel and returns. The walproposer-facing latency is just an enqueue
  + LSN bump; durability isn't observed yet.
- **`flush_wal()` is the durability sync point.** It pings
  `flush_notify` (cuts the consumer's accumulation wait short), then
  awaits the `watch<DurableState>` watermark catching up to the highest
  LSN `write_wal` has pushed (`submitted_max_lsn`). Returns only after
  every chunk submitted before this point is durable in YB. The
  walproposer treats the post-`flush_wal` LSN as the durable-commit
  boundary, so any crash between `write_wal` returning and the
  consumer's INSERT landing in YB is a no-op for the compute (no
  commit ack was sent).
- **`chunk_seq`** is allocated by the producer at push time, so the
  consumer's batch INSERTs preserve write order. On restart, rehydrated
  via `SELECT max(chunk_seq) … GROUP BY segno`.
- **Force drain on flush.** Between the greedy `try_recv` drain and the
  INSERT, the consumer waits up to `ACCUMULATION_MAX` (2 ms) for more
  chunks via `tokio::select!`. The wait is short-circuited by
  `flush_notify` (pinged by `flush_wal` / `truncate_wal` /
  `initialize_first_segment`). Net effect: bursty arrivals accumulate
  into larger batches; commit-driven workloads (pgbench / TPC-C) skip
  the wait and INSERT immediately.
- **`PendingChunk.data` is `Arc<Vec<u8>>`.** The same allocation is
  shared with the read-side `WalCache` entry inserted alongside the
  producer's `tx.send` — no double-allocation per write.
- **Backpressure.** Bounded mpsc (capacity 1024). If the consumer
  stalls, `write_wal`'s `tx.send().await` blocks → walproposer stalls
  rather than safekeeper memory growing unboundedly.
- **`truncate_wal(end_pos)`** waits on the watermark before issuing
  the DELETE; otherwise a buffered chunk past `end_pos` could land
  after the truncate. The DELETE itself drops rows whose
  `chunk_offset + chunk_size` lies past `end_pos`. After a
  partial-segment truncate, `chunk_seq` state is reloaded from YB.
- **`remove_up_to(segno)`** deletes all rows with smaller `segno`. Does
  *not* wait on the watermark — by construction the producer's current
  segno is well above any segno targeted by `remove_up_to`.
- **Failure handling.** On a batch INSERT error (after `YsqlClient`'s
  own auto-retry has given up), the consumer publishes
  `DurableState::Failed(msg)` onto the watch and exits. The producer's
  next `tx.send` then fails, `write_wal` propagates the captured error,
  the walproposer reconnects.

CLI: `--wal-yb-config <YbStoreConfig TOML>` (optional). When absent, the
safekeeper uses `WalStore::Physical(...)` and the wals table is untouched.

### 3. WalReader falls back to YB (Phase 2, commit `77553419b`, lazy/cached in `d7eef8275`+)

`WalReader::open_segment` tries two sources, in order:

1. Local FS (`<workdir>/<tenant>/<timeline>/<XLogFileName>`). With YB-WAL
   mode + `no_local_writes` this always misses, so we fall through.
2. **YB `wals` table** — via `YbSegmentReader`, a lazy chunk-streaming
   `AsyncRead` that consults the in-memory [`WalCache`](#wal-cache--lazy-yb-reader)
   first and falls through to a single-chunk YB `SELECT` only on miss.

A safekeeper with `--wal-yb-config` set can lose its entire local WAL
directory and still serve every past-LSN streaming request from YB.

#### WAL cache & lazy YB reader

The first implementation of (2) above eagerly reassembled the full 16 MiB
segment into a `VecAsyncRead` buffer at `open_segment` time. That buffer
became *stale* as the WAL sender advanced: chunks INSERT'd into `wals`
after the snapshot were invisible, so the leading zeros from
`vec![0u8; 16 MiB]` were served instead. On the pageserver side this
decoded as `xlp_magic=0` / `xl_tot_len=0`, hundreds of KB into each
session. (Found by query (C) of `cell_db<table>` during the TPC-C run:
the bytes were correct *in* `yb_storage.wals` but stale *in the
in-memory segment buffer*.)

Replaced with two pieces, both in `safekeeper/src/yb_wal_storage.rs` +
`safekeeper/src/wal_cache.rs`:

1. **`WalCache`** (`safekeeper/src/wal_cache.rs`) — bounded in-memory
   FIFO cache of recently-written WAL chunks, keyed by `(segno,
   chunk_offset)`. Populated by `YbWalStorage::upsert_writes` *before*
   the chunk is pushed onto the consumer channel, so a read-after-write
   from the WAL sender hits memory without waiting for the YB INSERT.
   Capacity controlled by `--wal-cache-size-mib` (default 256, set to 0
   to disable). FIFO eviction once the cache outgrows its byte budget.
   The init-first-segment 16 MiB blob is intentionally **not** cached
   (it would chew the entire capacity for a one-time read at boot).

   The cache is invalidated alongside YB DELETEs by `truncate_after` /
   `remove_segnos_below`, mirroring `YbWalStorage::delete_after` and
   `YbWalStorage::remove_up_to`. A single `std::sync::Mutex` is fine:
   writes target high offsets, reads target low offsets, so the BTreeMap
   entries they touch never overlap, and the lock is held only for
   metadata ops (no I/O).

2. **`YbSegmentReader`** (`safekeeper/src/yb_wal_storage.rs`) replaces
   `VecAsyncRead`. Its `poll_read` walks forward through the segment
   one chunk at a time:

   1. Serve from `current` if its `[chunk_offset, end_offset)` covers
      `self.pos`.
   2. Otherwise `cache.lookup(segno, pos)`; on hit, set `current`,
      retry.
   3. Otherwise spawn a YB fetch task and `tokio::sync::oneshot`-await
      it. The fetch issues:
      ```sql
      SELECT chunk_offset, chunk_size, data FROM wals
      WHERE tenant_id=$1 AND timeline_id=$2 AND segno=$3
        AND chunk_offset <= $4 AND chunk_offset + chunk_size > $4
      ORDER BY chunk_seq DESC LIMIT 1
      ```
      `ORDER BY chunk_seq DESC LIMIT 1` gives last-write-wins overlay
      semantics (segno-0 init blob vs. later `write_wal` chunks at the
      same offset).
   4. If neither cache nor YB has a covering chunk, return
      `io::ErrorKind::UnexpectedEof` — failing loudly is much better
      than serving zeros and letting the pageserver hit `xlp_magic=0`
      three layers downstream.

   The fetch is dispatched via `tokio::spawn` + oneshot because
   `open_segment` returns `Pin<Box<dyn AsyncRead + Send + Sync>>` and
   `tokio_postgres` query futures aren't `Sync` — the oneshot
   `Receiver` is, so we can hold it inside the reader without losing
   `Sync`.

### 4. No local-FS writes when YB is on (commit `8ea570aeb`)

A new `PhysicalStorage::no_local_writes: bool` field gates every file/fs
operation:

- `write_in_segment` early-returns; LSN/segment tracking still runs so the
  decoder + `write_record_lsn` advance correctly.
- `flush_wal` skips fdatasync; just updates `flush_lsn`/`flush_record_lsn`
  watermarks (data is already durable in YB).
- `truncate_wal` skips the open/set_len/fsync/rename/remove sequence.
- `remove_up_to` returns immediately.
- `initialize_first_segment` skips the local segment-bootstrap write — the
  wrapper (`YbWalStorage::initialize_first_segment`) instead generates the
  16 MiB segment header via `postgres_ffi::generate_wal_segment` and INSERTs
  it into the `wals` table itself.

A new constructor, `PhysicalStorage::new_no_local_writes`, skips
`find_end_of_wal` (which would scan an empty local dir) and takes
`write_lsn` from the caller. `YbWalStorage::new` rehydrates that
`write_lsn` from YB:

```sql
SELECT segno, max(chunk_offset + chunk_size) AS end_off FROM wals
WHERE tenant_id = $1 AND timeline_id = $2
GROUP BY segno ORDER BY segno DESC LIMIT 1;
```

Plus a defensive `delete_after(write_lsn)` at construction to drop any
rows past the durable end-of-WAL (left over from a crash that INSERTed but
didn't return).

### 5. Single-safekeeper constraint in YB WAL mode

YB itself is a quorum-replicated store. The safekeeper's Raft acceptor
exists to give Neon's local-FS storage durability + replication. Once WAL
lives in YB, layering Paxos on top of YB would be double consensus — two
protocols racing on different LSN views, harder semantics, no extra
safety.

The yb-light build therefore **requires exactly one safekeeper** when
`--wal-yb-config` is set. The enforcement is in two places:

- **Boot**: `SharedState::restore` loads the control file, and if YB WAL
  mode is on (`SafeKeeperConf::is_yb_wal_mode()`), calls
  `require_single_safekeeper(&control_store.mconf)`. Fails fast at startup
  if the persisted membership has > 1 member or a joint configuration
  with > 1 new members.
- **Runtime**: `SafeKeeper::process_greeting` (which handles every
  walproposer `ProposerGreeting`) calls the same check on `msg.mconf`
  before passing it to `state.membership_switch`. A walproposer that
  tries to add a sibling safekeeper gets a hard error response.

The check itself is `require_single_safekeeper` in `safekeeper/src/lib.rs`:
allows `len == 0` (pre-generations / empty configs) and `len == 1`; rejects
anything larger with a descriptive error pointing at the redundant-consensus
issue.

Unit tests in `yb_mode_tests` cover: empty config accepted, single-member
accepted, two-member rejected, joint config with two new members rejected.

### 6. Node id from CLI, not from `safekeeper.id` (commit `8027e06c4`)

The 50-line `set_id()` helper that read/wrote `<workdir>/safekeeper.id` is
gone. `--id` is now a required CLI flag; the binary fails fast at startup
if missing. The id file is no longer created.

This was the last *durable* byte the safekeeper wrote to local FS in
YB-backed mode. After this commit, a freshly-wiped workdir works fine on
restart as long as `--id` is passed.

### 7. WAL acceptor batching ticker (commit `95088ba54`)

`WalAcceptor::run` in `receive_wal.rs` decides *when* `FlushWAL` fires.
Upstream Neon's policy was: rewrite each incoming `AppendRequest` to
`NoFlushAppendRequest`, then fire `FlushWAL` either on the periodic
`flush_ticker` (1 s) or whenever the message channel empties out
(immediate flush on lull). That's right for the local-FS backend where
`fsync` overhead is roughly batch-size-independent and latency wins.

For YB-WAL mode every flush is a separate `INSERT` statement with a
fixed per-statement DocDB + Raft cost, so we want writes to coalesce.
In YB-WAL mode (`Timeline::is_yb_wal_mode()`):

- `flush_ticker` runs at `FLUSH_INTERVAL_YB_WAL` = 2 ms instead of 1 s.
- The "queue empty → flush immediately" branch is **disabled**.

Result: flushes are driven by the 2 ms ticker, so multiple
`AppendRequest`s collected within one window all roll into the same
`YbWalStorage` producer push pattern. Commit latency rises by at most
~2 ms; throughput stops being capped by single-AppendRequest INSERTs.

### 8. `wal_backup` task disabled in YB-WAL mode (commit `9bca7c2ef`)

The `wal_backup` task offloads sealed local-FS WAL segments into the
`blobs` table for long-term retention. In YB-WAL mode the live WAL is
already mirrored row-by-row into `yb_storage.wals` and the per-segment
local files never exist (`no_local_writes`), so the offload loop has
no source — every iteration would `open()` a non-existent file, fail,
retry. Under sustained WAL pressure the retry loop back-pressured
SyncRep enough to stall compute commits (observed with HikariCP 180 s
timeouts, 0 successful TPC-C transactions).

`wal_backup::update_task` now short-circuits `should_task_run = false`
when `mgr.conf.is_yb_wal_mode()` is true. The existing kill-if-running
branch handles teardown of any task left over from before a config
change.

### 9. `calc_horizon_lsn` skips `backup_lsn` in YB-WAL mode (commit `e7eb6acf4`)

`remove_wal::calc_horizon_lsn` returns the oldest LSN the safekeeper
must keep on durable storage; segments below it are eligible for
`remove_up_to` (which in YB-WAL mode is a DELETE against
`yb_storage.wals`). It takes the min of:

- `cfile_remote_consistent_lsn` — pageserver-consumed.
- `cfile_backup_lsn` — sealed-segment offloader caught up.
- `cfile_commit_lsn`.
- `flush_lsn`.

In YB-WAL mode the `wal_backup` task is disabled (see §8), so
`backup_lsn` never advances past the initial `commit_lsn` at boot.
Keeping `backup_lsn` in the min would pin the WAL removal horizon and
the `wals` table would grow forever. The function takes a
`yb_wal_mode: bool` parameter that skips the `backup_lsn` clamp when
true. The other horizon contributors (`remote_consistent_lsn`,
`commit_lsn`, `flush_lsn`) still apply.

### 10. Auto-reconnecting YB clients via `YsqlClient` (commits `8b5e2b2b4`, `27f1e354c`, `7a424639e`)

Both the control-file connection (via `GenericRemoteStorage` → `YbStore`)
and the live-WAL mirror connection used to hold an
`Arc<tokio_postgres::Client>` for the lifetime of the process. When the
underlying TCP socket dies — YB tserver crash, network blip, YBA rolling
restart — `tokio_postgres`'s background connection task ends but the
`Arc<Client>` stays, and every subsequent `execute()` lands on a corpse.

`remote_storage::YsqlClient` (in `libs/remote_storage/src/ysql_client.rs`)
wraps `tokio_postgres::Client` with a stored connection config + reconnect
closure. On every `execute` / `query` / `query_opt` it:

1. Checks `is_closed()` and lazily reconnects if the client died.
2. Runs the op.
3. On a connection-closed error, forces a reconnect and retries **once**
   (covers in-flight statements during a reconnect window).

Plus `current()` exposes the live `Arc<Client>` for streaming reads /
`execute_raw` / `query_raw` paths that don't auto-retry cleanly; the
next `current()` call after a failed stream picks up a fresh connection.

`YsqlClient::from_yb_config(&YbStoreConfig, application_name)` is the
shared factory used by `YbStore::new`, `WalBackup::new` (the live-WAL
mirror), and any other YB-Postgres connection in yb-light. It handles
`password_env` lookup and the sslmode / sslrootcert dispatch.

### 11. TLS support (commit `7a424639e`)

`YbStoreConfig` carries an optional `sslmode` field (`"disable"` /
`"require"` / `"verify-ca"` / etc.) and `sslrootcert` (path to a PEM-
encoded CA bundle). The safekeeper's WAL-mirror connection honours
both via the shared `YsqlClient::from_yb_config` factory:

- `sslmode = "disable"` (or unset) → plain TCP, `tokio_postgres::NoTls`.
- `sslmode = "require"` (or any non-`disable`) with no `sslrootcert`
  → rustls + the AcceptAll verifier (logs but ignores cert validation
  errors). Suitable for YBA universes with self-signed per-universe CAs.
- `sslmode = "verify-ca"` with `sslrootcert = "/path/to/ca.pem"` →
  rustls verifying against the supplied CA bundle.

YBA-managed universes default to TLS-mandatory `hostssl` rules in
`pg_hba.conf`. Before this commit the safekeeper's WAL-mirror
hard-coded NoTls regardless of `sslmode` and could only reach such
universes through a TLS-bridging proxy.

## What's on local FS after all changes (YB-backed mode)

```
<workdir>/safekeepers/sk<id>/
├── safekeeper-N.log          (rolling log — operational)
├── safekeeper.pid            (running-process lock)
└── <tenant>/<timeline>/
    └── (empty — no control file, no segments, no .partial file)
```

Delete the entire `<workdir>` and the safekeeper rebuilds from YB on next
start. Logs and pidfile are recreated.

## Run / configure

```
safekeeper \
    -D <workdir> \
    --id 1 \
    --listen-pg 127.0.0.1:5474 \
    --listen-http 127.0.0.1:7686 \
    --broker-endpoint http://127.0.0.1:50061/ \
    --availability-zone sk-1 \
    --control-file-backend remote-storage \
    --remote-storage   "{host = '127.0.0.1', port = 5433, user = 'yugabyte', dbname = 'yb_storage', table_name = 'blobs', prefix = 'safekeeper-yb/', sslmode = 'require'}" \
    --wal-yb-config    "{host = '127.0.0.1', port = 5433, user = 'yugabyte', dbname = 'yb_storage', table_name = 'wals', sslmode = 'require'}"
```

The two YB configs intentionally point at the *same* DB but *different*
tables: `blobs` is the chunked blob store (shared with the pageserver and
control file), `wals` is the per-write append log.

`sslmode = "require"` is needed against YBA-managed universes (they
default to TLS-mandatory `hostssl` rules). Add
`sslrootcert = '/path/to/ca.pem'` for stricter (verify-ca) checks
instead of the AcceptAll default.

## Performance characteristics

- **`write_wal()`** is non-blocking — it parses chunks, stamps them, and
  pushes onto the producer channel. No YB round-trip on the hot path
  (unless backpressure: bounded mpsc, capacity 1024). Walproposer-facing
  latency is microseconds.
- **`flush_wal()`** awaits the consumer's watermark catching up to the
  highest LSN `write_wal` pushed. The wait is exactly one consumer
  batch's worth of work: at most `ACCUMULATION_MAX` (2 ms) wait time +
  one INSERT round-trip (~0.6 ms single-row, ~2.5 ms 50-row batch against
  a local YB; tens of ms across a real network). `flush_wal` pings
  `flush_notify` so the accumulation wait is normally short-circuited on
  the commit path.
- **Steady-state throughput** under TPC-C / pgbench is bounded by the
  single-connection batch INSERT round-trip on the consumer. With the
  producer keeping the next batch filling while the previous one's
  INSERT is in flight, single-connection YB write capacity approaches
  full saturation (~14K rows/sec at 50-row batches against local YB).
- **`truncate_wal()`** waits the same way + one DELETE; effectively
  constant time aside from row count.
- **`WalReader::open_segment()` from YB**: one SELECT pulling all chunks
  of a segment + reassembly in Rust. ~16 MiB per segment, batched as N
  rows (typically 1–N where N is `wal_seg_size / 100 MiB ≤ 1`, but for
  typical per-write rows, N can be hundreds).
- **Restart**: rehydrates `write_lsn` with one indexed SQL query against
  YB. No `find_end_of_wal` scan, no segment opens.
- **Connection failures** are absorbed by `YsqlClient` — one in-flight
  statement may be retried automatically; a sustained YB outage produces
  a `DurableState::Failed` watermark that propagates as a `flush_wal`
  error, the walproposer reconnects, and the consumer task exits cleanly.
