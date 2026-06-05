---
status: in_progress
severity: high
area: safekeeper
opened: 2026-05-20
---

## Wave 1 progress (2026-06-05)

Commits landed so far (~1907 LOC of dead code removed):

- `419e88627` — delete `pull_timeline.rs` (787 LOC) + slim `hadron.rs`
  (459 → 78 LOC: kept disk-usage helpers, dropped HCC register +
  `hcc_pull_timeline*`). Removed `--enable-pull-timeline-on-startup`
  / `--hcc-base-url` CLI args and the 4 `SK_RECOVERY_PULL_TIMELINE_*`
  metrics. SK's `POST /v1/pull_timeline` + snapshot route gone. The
  `PullTimelineRequest/Response` types are intentionally kept in
  `libs/safekeeper_api/src/models.rs` because the storage_controller's
  multi-SK reconciler still composes them (deleting them cascades into
  `SafekeeperTimelineOpKind::Pull` and the timeline-creation flow —
  out of scope for the SK-side slim).

- `34382d296` — delete `timeline_eviction.rs` (415 LOC) + remove the
  eviction block from `timeline_manager::main_task`.

- `76dc8fff8` — strip `is_offloaded`, `evict_not_before`,
  `partial_backup_task`, `partial_backup_uploaded` fields from
  `Manager` + the associated `update_partial_backup`,
  `update_partial_backup_end`, `backup_partial_reset` methods.
  `BackupPartialReset` ctl-message handler now returns
  `Ok(Vec::new())` (no-op) so the HTTP route keeps working until it
  too can be removed.

- `42cc5948c` — drop unused `wal_backup_partial` import + the
  `UpdatePartialBackup` / `EvictTimeline` `Status` enum arms.

## Wave 1 remaining

- **wal_backup.rs slim** (~700 LOC potential). The struct still holds
  the YB-WAL plumbing (`ysql_fpu_client`, `wal_yb_table`, `wal_cache`)
  used by `wal_storage.rs::WalReader.read_segment`. The
  S3-upload-task half (`WalBackupTaskHandle`, `update_task`,
  `is_wal_backup_required`, `Segment` + helpers,
  `hadron_determine_offloader`, `determine_offloader`,
  `backup_lsn_range`, `backup_single_segment`, `backup_partial_segment`,
  `read_object`, `delete_timeline`, `copy_s3_segments`,
  `remote_timeline_path`) is dead. Strip those; keep struct + getters.
- **wal_backup_partial.rs slim** (~450 LOC potential). Keep `State`,
  `PartialRemoteSegment`, `UploadStatus` (persisted in
  `TimelinePersistentState.partial_backup`). Delete `PartialBackup`
  struct, `main_task`, `needs_uploading`, `ReplaceUploadedSegment`.
- **timeline.rs cleanup**: `bootstrap_mgr`, `switch_to_offloaded`,
  `switch_to_present` are now unreachable.
- **state.rs**: `eviction_state` and `backup_lsn` fields are
  effectively dead but persisted in the control file. Removing them
  requires a versioned migration or a clean break of the on-disk
  format — out of scope for Wave 1.
- **conf**: `wal_backup_enabled`, `enable_offload`,
  `eviction_min_resident`, `partial_backup_timeout`,
  `partial_backup_concurrency`, `max_offloader_lag_bytes` are dead
  CLI args.

---


# Phase 9 — Safekeeper Paxos / multi-SK consensus removal

yb-light is single-safekeeper by design — durability comes from
YugabyteDB's internal RAFT replication of the `wals` table, not from
SK quorum. The Paxos-flavored consensus machinery in upstream Neon's
safekeeper is therefore structurally unnecessary:

- No peers ⇒ no quorum tracking.
- No leader election (the lone SK is always leader).
- No SK-to-SK recovery.
- No need to compute `commit_lsn` across peers (`commit_lsn = flush_lsn`).

The walproposer protocol on the compute side still has to keep
working, so term tracking simplifies to "highest term seen, accept
anything from it, reject earlier"; the rest of the state machine
collapses.

## Inventory of likely-dead code (~5–6 k LOC conservative, up to ~10 k aggressive)

- `recovery.rs` (563)
- `pull_timeline.rs` (786)
- `wal_backup.rs` (915) + `wal_backup_partial.rs` (578) — both dead
  post lane E since S3 is gone
- `timeline_eviction.rs` (416)
- `control_file_upgrade.rs` (788, simplifies when peer state is gone)
- ~600 LOC of consensus state in `safekeeper.rs` (1815)
- ~150 LOC of peer state in `state.rs` (299)

**`copy_timeline.rs` (262) STAYS** — the planned architecture is one
safekeeper per timeline, so cross-SK timeline copy is needed when
migrating a timeline to a new SK process.

## What survives in safekeeper

- `Acceptor::process_append_request` (accept WAL from walproposer)
- Minimal term tracking (walproposer protocol compliance)
- `yb_wal_storage.rs` (durability)
- `commit_lsn = flush_lsn`
- `send_interpreted_wal.rs` (serve WAL to PS)
- Pageserver-feedback handling for the rcLsn truncation loop

## Sequencing

Multi-session work:

- **Wave 1 (low-risk, mechanically dead, no peer-protocol entanglement):**
  `recovery.rs`, `pull_timeline.rs`, `wal_backup*`,
  `timeline_eviction.rs`.
- **Wave 2 (careful staging against walproposer protocol):**
  consensus state-machine slimming in `safekeeper.rs` + `state.rs`.
