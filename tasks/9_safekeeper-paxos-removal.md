---
status: pending
severity: high
area: safekeeper
opened: 2026-05-20
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
