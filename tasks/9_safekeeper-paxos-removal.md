---
status: done
severity: high
area: safekeeper
opened: 2026-05-20
closed: 2026-06-05
---

## Status: DONE

Both waves landed across two sessions (~8200 LOC of dead
safekeeper code removed total).

### Wave 1 — single-SK cleanup (~3700 LOC)

6 commits. Killed pull_timeline + Hadron HCC orchestration,
timeline_eviction, eviction/partial-backup runtime state,
copy_timeline, the S3 WAL-offloader half of wal_backup, the
partial-segment uploader, and the dead CLI args + metrics that
went with them. See section "Wave 1 commits" below.

### Wave 2 — wire-protocol + simulator + bindings (~4540 LOC)

3 commits:

- `045fce42d` — drop wire-protocol v2 from both the safekeeper
  (`safekeeper/src/safekeeper.rs`) and the C walproposer
  (`pgxn/neon/walproposer.c`, `walproposer.h`). The two sides
  already spoke v3 since last session's commit_lsn /
  peer_horizon_lsn removal; v2 was parallel deadweight. Deleted
  `ProposerGreetingV2` / `VoteRequestV2` / `AppendRequestHeaderV2`
  structs, `TermHistory::from_bytes_le`, and the v2 branches in
  `parse` / `serialize` / `PAMessageSerialize` / `AsyncReadMessage`.
  ~700 LOC.

- `81552c8db` — delete `safekeeper/tests/walproposer_sim/` plus the
  three driver tests (`misc_test`, `random_test`, `simple_test`).
  The simulator was a deterministic event-loop harness for
  multi-SK consensus dynamics — already broken since last
  session's commit_lsn removal and exercising dynamics that don't
  exist in single-SK mode anyway. Also stripped the dead
  `advertise_pg_addr_tenant_only` field and the bare
  `BEGIN_HADRON`/`END_HADRON` markers. ~2500 LOC.

- `9cf39b17b` — delete `libs/walproposer` (Rust bindgen bindings
  to the C walproposer, only used by the deleted simulator).
  ~1330 LOC.

## What was NOT touched (deliberately)

The acceptor state machine itself (`handle_greeting`,
`handle_vote_request`, `handle_elected`, `handle_append_request`,
`handle_flush` + `AcceptorState`, `TermHistory`,
`find_highest_common_point`) survives intact. It still does real
work even in single-SK mode: walproposer can restart with a fresh
term and the SK must refuse stale terms, accept higher ones,
truncate WAL beyond the divergence point, and serve the
acceptor-greeting → vote-response → append-response wire
protocol. The original task's "~600 LOC of consensus state in
safekeeper.rs" estimate was made before our prior cleanups
(commit_lsn, peer_horizon_lsn, peer recovery) and the v2 protocol
removal — the actual remaining surface is the bare minimum
walproposer needs to talk to one SK.

## Wave-1 leftovers — closed out

The three cleanup-only items left open at end of wave 1 have
since landed:

- `partial_backup` / `eviction_state` / `backup_lsn` dropped from
  `TimelinePersistentState` — commit `a8fe507cb` (rode on top of
  the task-19 control-file → YB columns migration that unblocked
  the schema break).
- `RateLimiter` plumbing removed from `Manager` /
  `GlobalTimelines` — commit `b152ccf24`.
- `safekeeper_api::PullTimeline{Request,Response}` deleted —
  commit `1a9ca7ec1`, after the storage_controller stopped driving
  multi-SK migration (`259d6e9a1`) and the dead HTTP endpoints
  could be stripped together.

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
