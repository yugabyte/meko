# Safekeeper

The safekeeper is the durable, fault-tolerant landing zone for the compute's
WAL. It's a Raft acceptor — N safekeepers form a quorum-replicated WAL log
that the compute writes to synchronously and the pageserver reads from
asynchronously.

In yb-neon, **all of the safekeeper's durable state lives in YugabyteDB**.
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

## Changes in yb-neon

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

- One row per `write_wal()` call. If a single call spans a segment boundary,
  the write is split into one row per segment.
- `chunk_seq` is an in-memory monotonic counter per segno; on restart
  rehydrates via `SELECT max(chunk_seq) ... GROUP BY segno`.
- `flush_wal()` is a no-op for durability in YB-only mode (rows are durable
  by the time `write_wal()` returns); it just advances the in-memory
  `flush_lsn` watermark.
- `truncate_wal(end_pos)` deletes rows whose `chunk_offset + chunk_size`
  lies past `end_pos`. After truncate it reloads `chunk_seq` state from YB
  to stay monotonic.
- `remove_up_to(segno)` deletes all rows with smaller `segno`.

CLI: `--wal-yb-config <YbStoreConfig TOML>` (optional). When absent, the
safekeeper uses `WalStore::Physical(...)` and the wals table is untouched.

### 3. WalReader falls back to YB (Phase 2, commit `77553419b`)

`WalReader::open_segment` now tries three sources, in order:

1. Local FS (`<workdir>/<tenant>/<timeline>/<XLogFileName>`).
2. **YB `wals` table** — re-assembles the segment by SELECTing all chunks
   for `(tenant, timeline, segno)` ordered by `chunk_seq`, applying last-
   write-wins on overlapping byte ranges, returning a `VecAsyncRead` cursored
   at the requested `xlogoff`.
3. The existing sealed-segment `wal_backup` path through `GenericRemoteStorage`.

A safekeeper with `--wal-yb-config` set can lose its entire local WAL
directory and still serve every past-LSN streaming request from YB.

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

### 5. Node id from CLI, not from `safekeeper.id` (commit `8027e06c4`)

The 50-line `set_id()` helper that read/wrote `<workdir>/safekeeper.id` is
gone. `--id` is now a required CLI flag; the binary fails fast at startup
if missing. The id file is no longer created.

This was the last *durable* byte the safekeeper wrote to local FS in
YB-backed mode. After this commit, a freshly-wiped workdir works fine on
restart as long as `--id` is passed.

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
    --remote-storage   "{host = '127.0.0.1', port = 5433, user = 'yugabyte', dbname = 'yb_storage', table_name = 'blobs', prefix = 'safekeeper-yb/'}" \
    --wal-yb-config    "{host = '127.0.0.1', port = 5433, user = 'yugabyte', dbname = 'yb_storage', table_name = 'wals'}"
```

The two YB configs intentionally point at the *same* DB but *different*
tables: `blobs` is the chunked blob store (shared with the pageserver and
control file), `wals` is the per-write append log.

## Performance characteristics

- `write_wal()`: synchronous YB INSERT per call. Roughly one network
  round-trip + DocDB Raft write. With YB local on the same host, ~ms.
  With YB across a real network, milliseconds-to-tens-of-ms. Compute
  commit latency rises proportionally — this is the main trade-off of
  the YB-backed safekeeper.
- `flush_wal()`: in YB mode, no I/O — just advances an in-memory LSN.
- `truncate_wal()`: one DELETE; effectively constant time aside from how
  many rows YB has to delete.
- `WalReader::open_segment()` from YB: one SELECT pulling all chunks of a
  segment + reassembly in Rust. ~16 MiB per segment, batched as N rows
  (typically 1–N where N is `wal_seg_size / 100 MiB ≤ 1`, but for typical
  per-write rows, N can be hundreds).
- Restart: rehydrates `write_lsn` with one indexed SQL query against YB.
  No `find_end_of_wal` scan, no segment opens.
