---
status: in_progress
severity: high
area: safekeeper
opened: 2026-05-20
---

## Wave 1 — DONE (2026-06-05)

~3700 LOC of dead consensus + S3-offloader + local-FS-eviction
code removed across 6 commits. Wave 2 (consensus state-machine
slimming in `safekeeper.rs` + `state.rs`) is the remaining work.

## Wave 1 commits

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

- `1a32d69b4` — slim `wal_backup.rs` (921 → 197 LOC: dropped the S3
  offloader half, kept the YB-WAL plumbing + delete_timeline), slim
  `wal_backup_partial.rs` (579 → 56 LOC: kept only the
  `PartialRemoteSegment` / `State` / `UploadStatus` types that are
  persisted in the control file), delete `copy_timeline.rs` (256
  LOC: S3 + local-FS WAL copy, neither substrate exists in
  yb-light), and remove `bootstrap_mgr` / `switch_to_offloaded` /
  `switch_to_present` from `timeline.rs`.

- `2db5da006` — drop dead CLI args (`--max-offloader-lag`,
  `--wal-backup-parallel-jobs`, `--disable-wal-backup`,
  `--partial-backup-timeout`, `--enable-offload`,
  `--partial-backup-concurrency`, `--eviction-min-resident`, etc.),
  the matching `SafeKeeperConf` fields, the `defaults` constants
  (`DEFAULT_PARTIAL_BACKUP_TIMEOUT`, `DEFAULT_EVICTION_MIN_RESIDENT`,
  ...), the `debug_dump::Config` fields, and the dead Prometheus
  metrics (`safekeeper_backed_up_segments_total`,
  `safekeeper_partial_backup_uploads_total`,
  `safekeeper_wal_backup_tasks_*`,
  `safekeeper_evicted_timelines`, ...).

## Wave 1 leftovers (not blocking; cleanup-only)

- **state.rs**: `partial_backup`, `eviction_state`, `backup_lsn`
  fields are dead at runtime but persisted in the control file
  format. Removing them needs either a versioned migration or a
  clean break — gated on task 19 (SK control file → YB columns).
- **RateLimiter**: still threaded through `Manager` /
  `GlobalTimelines` even though both semaphores (partial_backup +
  eviction) have no callers. Trivial to delete, just a wider edit.
- **`safekeeper_api::PullTimelineRequest`/`Response`**: kept on the
  API surface because the storage_controller's multi-SK reconciler
  still composes them. Their deletion cascades into
  `SafekeeperTimelineOpKind::Pull` and the timeline-creation flow —
  out of scope; the storage_controller slim is a separate task.

## Wave 2 — consensus state-machine slimming (PENDING)

The single-SK simplification of the acceptor state machine itself:

- ~600 LOC of consensus state in `safekeeper.rs` (currently 1815
  LOC; some of this was already trimmed when `peer_horizon_lsn` /
  `commit_lsn` came out in commits `c4b76b479` / `2a6e1eb9c` /
  `f050b9c0e` last session — actual remaining surface is smaller
  than 600 LOC).
- ~150 LOC of peer state in `state.rs`.
- Term tracking collapses to "highest term seen, accept anything
  from it, reject earlier."

Wave 2 is the harder half because it edits the same state machine
the C-side walproposer (`pgxn/neon/walproposer.c`) drives — any
state change needs paired Rust+C edits and a live-compute smoke
run.

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
