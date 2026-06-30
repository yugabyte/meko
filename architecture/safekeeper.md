# Safekeeper

The safekeeper is landing zone for the compute's WAL. The compute writes to it
synchronously and the pageserver reads from asynchronously.

In yb-light, **all of the safekeeper's durable state lives in YugabyteDB**.
The local filesystem holds only a log file and a pidfile.


### Wire protocol

- **In**: walproposer connects via Postgres replication protocol on
  `--listen-pg`; uses the `START_WAL_PUSH` extended query plus a custom
  binary stream of WAL records framed by message type.
- **Out**: pageservers (and any read replica) connect via the same port and
  issue `START_REPLICATION PHYSICAL <lsn>` to stream WAL forward. The
  consuming pageserver(s) may be on a different node and a different build
  than this safekeeper (read endpoints especially — see
  [`high-level.md`](high-level.md) "PS/SK co-location is NOT an invariant"),
  so the WAL the SK sends out is the version-portable **interpreted-WAL**
  protocol (`send_interpreted_wal`); no same-host/same-build fast-path may be
  assumed on this link without a negotiated capability (see #69).

### Per-branch state

- **Control file**: 388 B binary blob holding the consensus state (`term`,
  `vote`, peer set, `commit_lsn`, `backup_lsn`, `peer_horizon_lsn`,
  `remote_consistent_lsn`, `eviction_state`, etc.). Atomically renamed
  (`safekeeper.control.partial` → `safekeeper.control`) per update.
- **WAL segments**: vanilla Postgres XLog 16 MiB segment files named
  `XLogFileName(PG_TLI, segno, wal_seg_size)` → e.g.
  `000000010000000000000001`.

In yb-light both are replaced — see "Changes in yb-light" §1
(control file → typed columns in `safekeeper.timeline_state`) and §2
(WAL chunks mirrored row-by-row into `safekeeper.wals`). The local FS holds
nothing per-branch.

### Background tasks

Upstream Neon also ran `wal_backup` (sealed-segment S3 offload via
`GenericRemoteStorage`) and `wal_backup_partial` (in-flight last-segment
upload). Both modules are deleted in yb-light — durability is the
`safekeeper.wals` mirror, not S3 — see "Changes in yb-light" §8 below.

Surviving background work:
- Periodic control-file flush every `control_file_save_interval`
  (default 300 s) plus on-state-change flushes via `finish_change()`.

### Code layout

- `safekeeper/src/control_file.rs` — control-file `Storage` trait + impls.
- `safekeeper/src/wal_storage.rs` — WAL `Storage` trait + `PhysicalStorage` impl + `WalReader`.
- `safekeeper/src/yb_wal_storage.rs` — YB mirror (`safekeeper.wals`),
  producer/consumer writer, `load_write_lsn` restart rehydrate.
- `safekeeper/src/timeline.rs` — `SharedState`, `Timeline::load_timeline`.
- `safekeeper/src/timelines_global_map.rs` — `GlobalTimelines`. One process
  serves one branch (see §5), so the registry is a single `Option<(branch_id,
  slot)>` plus one branch-tombstone slot, not `HashMap`s;
  `init()` loads only the baked branch, never the whole shared table. The
  external API (`get`, `get_all` → `Vec`, `timelines_count`, …) is unchanged.
- `safekeeper/src/safekeeper.rs` — the consensus state machine.
- `safekeeper/src/bin/yb-safekeeper.rs` — entry point; reads
  `<datadir>/safekeeper.toml` into `SafeKeeperConf` (only `-D`/`--init`/
  `--dump-control-file` are CLI flags).
- `libs/safekeeper_api/src/config.rs` — `SafekeeperConfigToml`, the on-disk
  `safekeeper.toml` schema shared by the binary and yb_light_local.

## Changes in yb-light

### 1. Control file → YB columns (commits `c4c7c1350`, `840e56aae`)

`YbControlFileStorage` is the sole `Storage` impl (`safekeeper/src/control_file.rs`).
The local-FS `FileStorage` path, the `ControlFileStorage` dispatch enum,
the bincode + crc32c blob framing, and `control_file_upgrade.rs` (V1..V9
legacy upgrade paths, 778 LOC) were all deleted in `840e56aae` once the
intermediate blob-in-`yb_storage.blobs` form was replaced with a typed row.

Each branch owns one row in `safekeeper.timeline_state`:

```sql
CREATE TABLE safekeeper.timeline_state (
    branch_id              TEXT     NOT NULL,
    timeline_start_lsn     BIGINT   NOT NULL,
    local_start_lsn        BIGINT   NOT NULL,
    remote_consistent_lsn  BIGINT   NOT NULL,
    pg_version             INT      NOT NULL,
    system_id              BIGINT   NOT NULL,
    wal_seg_size           INT      NOT NULL,
    term                   BIGINT   NOT NULL,
    term_history           JSONB    NOT NULL,        -- Vec<{term, lsn}>
    proposer_uuid          BYTEA    NOT NULL,        -- 16 bytes
    mconf                  JSONB    NOT NULL,        -- membership Configuration
    creation_ts            TIMESTAMPTZ NOT NULL,
    PRIMARY KEY ((branch_id) HASH)
);
```

The `commit_lsn` / `backup_lsn` / `peer_horizon_lsn` / `partial_backup` /
`eviction_state` columns from the original #10 design were dropped
once their in-memory counterparts went away (`a8fe507cb`, plus the
`commit_lsn` / `peer_horizon_lsn` removals in earlier sessions).

- `persist()` is an UPSERT against the row, scoped via `YsqlClient`.
- `SharedState::restore` and `Timeline::load_timeline` are `async` to
  await the YB load.
- No CLI flag selects the backend — there's nothing to dispatch over.

### 2. Live WAL → YB-mirrored (Phase 1, commit `35e610db6`)

New `yb_wal_storage.rs` module with `YbWalStorage` (a wrapper) and a
`WalStore` dispatch enum:

```
+- enum WalStore
   +- Physical(PhysicalStorage)  <-- unchanged path; no YB
   +- Yb(YbWalStorage)           <-- writes every WAL chunk into safekeeper.wals
                                     inside a wrapped PhysicalStorage
```

Schema (operator-provisioned in `yb_storage` DB):

```sql
CREATE TABLE safekeeper.wals (
    branch_id        TEXT    NOT NULL,
    segno            BIGINT  NOT NULL,
    chunk_offset     BIGINT  NOT NULL,   -- byte offset of `data` within the segment
    chunk_size       INT     NOT NULL,
    data             BYTEA   NOT NULL,
    last_modified_us BIGINT  NOT NULL,
    PRIMARY KEY ((branch_id) HASH, segno, chunk_offset)
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
with its chunk_offset
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
                                │    INSERT INTO safekeeper.wals
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
- **`chunk_offset`** (the chunk's byte offset within its segment) is the
  in-segment identity — the writer never produces overlapping chunks
  (`truncate_wal` aligns the next write at the truncation point), so offset
  order is write order and a same-offset re-INSERT is an idempotent replay.
  On restart, the per-segment high-water position is rehydrated via
  `SELECT segno, max(chunk_offset + chunk_size) … GROUP BY segno`.
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
  partial-segment truncate, the per-segment write position is reloaded
  from YB.
- **`remove_up_to(segno)`** deletes all rows with smaller `segno`. Does
  *not* wait on the watermark — by construction the producer's current
  segno is well above any segno targeted by `remove_up_to`.
- **Failure handling.** On a batch INSERT error (after `YsqlClient`'s
  own auto-retry has given up), the consumer publishes
  `DurableState::Failed(msg)` onto the watch and exits. The producer's
  next `tx.send` then fails, `write_wal` propagates the captured error,
  the walproposer reconnects.

Config: the `yb_storage_conn` inline table in `safekeeper.toml` (optional,
the shared YB connection block — the same key the pageserver and
endpoint_storage use). When absent, the safekeeper uses
`WalStore::Physical(...)` and the wals table is untouched.

### 3. WalReader falls back to YB (Phase 2, commit `77553419b`, lazy/cached in `d7eef8275`+)

`WalReader::open_segment` tries two sources, in order:

1. Local FS (`<workdir>/branches/<branch_id>/<XLogFileName>`). With YB-WAL
   mode + `no_local_writes` this always misses, so we fall through.
2. **YB `wals` table** — via `YbSegmentReader`, a lazy chunk-streaming
   `AsyncRead` that consults the in-memory [`WalCache`](#wal-cache--lazy-yb-reader)
   first and falls through to a single-chunk YB `SELECT` only on miss.

A safekeeper with `yb_storage_conn` configured can lose its entire local WAL
directory and still serve every past-LSN streaming request from YB.

#### WAL cache & lazy YB reader

The first implementation of (2) above eagerly reassembled the full 16 MiB
segment into a `VecAsyncRead` buffer at `open_segment` time. That buffer
became *stale* as the WAL sender advanced: chunks INSERT'd into `wals`
after the snapshot were invisible, so the leading zeros from
`vec![0u8; 16 MiB]` were served instead. On the pageserver side this
decoded as `xlp_magic=0` / `xl_tot_len=0`, hundreds of KB into each
session. (Found by query (C) of `cell_db<table>` during the TPC-C run:
the bytes were correct *in* `safekeeper.wals` but stale *in the
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
      SELECT chunk_offset, data FROM safekeeper.wals
      WHERE branch_id=$1 AND segno=$2 AND chunk_offset <= $3
      ORDER BY chunk_offset DESC LIMIT 1
      ```
      `ORDER BY chunk_offset DESC LIMIT 1` returns the chunk whose offset
      most-closely precedes the requested byte — the one covering it.
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

A new constructor, `PhysicalStorage::new_no_local_writes`, skips the
upstream local-directory `find_end_of_wal` scan and takes `write_lsn` from
the caller. `YbWalStorage::new` computes that `write_lsn` from YB in two
steps. First, `load_write_lsn` reads the raw end of the byte stream:

```sql
SELECT segno, max(chunk_offset + chunk_size) AS end_off FROM safekeeper.wals
WHERE branch_id = $1
GROUP BY segno ORDER BY segno DESC LIMIT 1;
```

That raw maximum can land mid-record (an ungraceful compute shutdown leaves
a torn trailing page) or, after an `XLOG_SWITCH`, short of the true end. So
a second step replays the decoder over the YB-backed WAL: `find_anchor`
walks back to the nearest fresh-record page — validating `xlp_magic`, so it
steps over the zero padding a switch leaves — then `find_end_of_wal` scans
forward to the last complete record boundary. The result refines `write_lsn`:

- **Torn tail** (`end < raw max`): truncate back to the last valid boundary;
  the compute re-streams from there.
- **Trailing `XLOG_SWITCH`** (`end > raw max`): `pg_switch_wal()` rounds the
  record's end up to the next segment, past padding the compute never
  streams, so the decoder reports a boundary *beyond* the persisted bytes.
  Honor it — clamping back to the raw max would leave `write_lsn` inside the
  padding, where the compute resumes, the reader reads zeros, and the
  safekeeper wedges (#181).

Plus a defensive `delete_after(write_lsn)` at construction to drop any
rows past the durable end-of-WAL (left over from a crash that INSERTed but
didn't return).

The same switch padding shows up on the **read** path: a cold reader
catching up across a *historical* switch (a pageserver whose durable
`consistent_lsn` lagged behind it) reads into the un-streamed segment tail.
Once the committed frontier has passed that segment, `YbSegmentReader`
synthesizes the missing padding as zeros — which the decoder skips to the
next segment — rather than failing with a WAL-hole; a gap that is *not*
switch padding still surfaces as a decode error, never silently zero-filled
(#181).

### 5. Single-safekeeper constraint in YB WAL mode

YB itself is a quorum-replicated store. The safekeeper's Raft acceptor
exists to give Neon's local-FS storage durability + replication. Once WAL
lives in YB, layering Paxos on top of YB would be double consensus — two
protocols racing on different LSN views, harder semantics, no extra
safety.

The yb-light build therefore **requires exactly one safekeeper per
branch** when `yb_storage_conn` is configured (the membership configuration
must have a single member). Multiple safekeeper *processes* may run on
a host — the per-endpoint topology gives each branch its own dedicated
safekeeper — but each branch's membership is always one safekeeper.
The enforcement is in two places:

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

Unit tests in `yb_light_tests` cover: empty config accepted, single-member
accepted, two-member rejected, joint config with two new members rejected.

**Single-branch identity (one branch per process).** The
single-safekeeper constraint above is about *membership* (one acceptor per
branch); this is its dual on the *process* side: one safekeeper process
serves exactly one `branch_id` for its lifetime, baked into
`safekeeper.toml`'s `branch_id` (`SafeKeeperConf::yb_branch_id`,
required in YB-WAL mode). It matters because `safekeeper.timeline_state` is shared across the
whole cluster — without a baked identity a process can't tell its own
branch from everyone else's. Two consequences:

- **Scoped load.** `GlobalTimelines::init()` loads only the baked branch's
  row (if it exists yet — a fresh bootstrap creates it on demand via the
  walproposer handshake), never the whole table. The in-memory registry is
  therefore a single `Option`, not a `HashMap`.
- **Connection gating.** `SafekeeperPostgresHandler::process_query` rejects
  any connection whose `branch_id` doesn't match the baked identity
  before it can touch the registry, so a misrouted walproposer can't create
  or mutate a foreign branch in this process.

### 6. Node id from config, not from `safekeeper.id` (commit `8027e06c4`)

The 50-line `set_id()` helper that read/wrote `<workdir>/safekeeper.id` is
gone. The id now comes from the `id` key in `safekeeper.toml` (originally a
`--id` CLI flag; folded into the config file when the safekeeper's whole CLI
surface moved to `safekeeper.toml`). The id file is no longer created.

This removed the last *durable* byte the safekeeper wrote to local FS in
YB-backed mode. A freshly-wiped workdir works fine on restart as long as the
launcher rewrites `safekeeper.toml` (it does, on every `SafekeeperNode::start`).

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

### 8. `wal_backup*` and `backup_lsn` removed (commits `1a32d69b4`, `a8fe507cb`)

Durability in yb-light is the `safekeeper.wals` mirror written
synchronously by `flush_wal()` — there is no S3 offload tier.
Consequently:

- `safekeeper/src/wal_backup.rs` and `wal_backup_partial.rs` are
  deleted (`1a32d69b4` slimmed `wal_backup.rs` 921 → 197 LOC keeping
  only the YB-WAL plumbing + `delete_timeline`; later commits dropped
  the file entirely once `WalBackup` no longer existed). The
  intermediate "disable the task in YB-WAL mode" patch
  (`9bca7c2ef`) is now obsolete history.
- `backup_lsn` / `partial_backup` / `eviction_state` were dropped from
  `TimelinePersistentState` in `a8fe507cb`.
- `remove_wal::calc_horizon_lsn` is now `min(cfile_remote_consistent_lsn,
  flush_lsn, extra_horizon_lsn?)` — the `backup_lsn` clamp and the
  `yb_wal_mode` skip parameter are gone (`remove_wal.rs`).

### 9. Auto-reconnecting YB clients via `YsqlClient` (commits `8b5e2b2b4`, `27f1e354c`, `7a424639e`)

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

`YsqlClient::from_yb_config(&YbConnConfig, application_name, ...)` is the
shared factory used by `YbStore::new`, `WalBackup::new` (the live-WAL
mirror), and any other YB-Postgres connection in yb-light. It handles
`password_env` lookup and the sslmode / sslrootcert dispatch.

### 10. TLS support (commit `7a424639e`)

`YbConnConfig` carries an optional `sslmode` field (`"disable"` /
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
<workdir>/endpoints/<endpoint_id>/safekeeper/
├── safekeeper-N.log          (rolling log — operational)
├── safekeeper.pid            (running-process lock)
└── branches/<branch_id>/
    └── (empty — no control file, no segments, no .partial file)
```

In the dev cluster the safekeeper is co-located under the endpoint that owns
its branch (provisioned at `endpoint create`) — a launcher convenience,
not a design invariant; the consuming pageserver(s) may live elsewhere (see
[`high-level.md`](high-level.md) "PS/SK co-location is NOT an invariant").
Delete the entire
`endpoints/<endpoint_id>/safekeeper` dir and the safekeeper rebuilds from
YB on next start. Logs and pidfile are recreated.

## Run / configure

All configuration lives in `<datadir>/safekeeper.toml` — the safekeeper's
analogue of `pageserver.toml`, written by yb_light_local
(`SafekeeperNode::start`) and read once at startup
(`safekeeper_api::config::SafekeeperConfigToml`). Config is **fail-fast**:
`SafekeeperConfigToml` is `deny_unknown_fields`, so a misspelled or stale key
aborts startup rather than being silently ignored (alongside the usual
wrong-type / bad-value / duplicate-key deserialize failures). The only CLI
flags are the datadir locator and two operational probes:

```
yb-safekeeper -D <datadir> [--init] [--dump-control-file <path>]
```

A representative `<datadir>/safekeeper.toml`:

```toml
id = 1
branch_id = "77ffde89..."
listen_pg = "127.0.0.1:5474"
listen_http = "127.0.0.1:7686"
availability_zone = "sk-1"
yb_storage_conn = "{ host_ports = '127.0.0.1:5433', user = 'yugabyte', dbname = 'yb_storage', sslmode = 'require' }"
# … tuning knobs (heartbeat_timeout, control_file_save_interval, ssl_*, …)
# fall back to defaults when omitted; unknown keys are ignored.
```

`yb_storage_conn` is the single YB connection (a string-encoded inline table,
like `pageserver.toml`'s `yb_storage_conn`) used for both the
`safekeeper.timeline_state` row (control file) and the `safekeeper.wals` chunk
stream — same DB, two tables. The WAL chunk table is hardcoded to
`safekeeper.wals` (not configurable); `safekeeper.timeline_state` is reached
via the same `YsqlClient`.

`branch_id` bakes this process's single branch identity
(required whenever `yb_storage_conn` is set) — the safekeeper's analogue of
`pageserver.toml`'s identity key. yb_light_local stamps it at provision time
(`allocate_safekeeper_conf`). It scopes the startup load to this one
branch and gates connections to it (see §5).

`sslmode = "require"` is needed against YBA-managed universes (they
default to TLS-mandatory `hostssl` rules). Add
`sslrootcert = '/path/to/ca.pem'` for stricter (verify-ca) checks
instead of the AcceptAll default.

The storage broker was removed wholesale, so there is no
`--broker-endpoint` flag.

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
  YB, then a bounded record-boundary replay back from that raw end (no
  local segment files; reads only the trailing chunks needed to find the
  last valid boundary / resolve a trailing `XLOG_SWITCH`).
- **Connection failures** are absorbed by `YsqlClient` — one in-flight
  statement may be retried automatically; a sustained YB outage produces
  a `DurableState::Failed` watermark that propagates as a `flush_wal`
  error, the walproposer reconnects, and the consumer task exits cleanly.
