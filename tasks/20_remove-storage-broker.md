---
status: pending
severity: medium
area: safekeeper
opened: 2026-06-05
---

# Phase 20 — Remove the storage_broker crate

Single-SK yb-mode makes the broker's "which SK is most caught up"
selection trivial, and its WAL-availability signaling is unnecessary
because the SK→PS WAL stream is long-lived and pushes new bytes
directly on the existing connection. The broker is dead weight today.

## Inventory

- `storage_broker/` crate: ~96 LOC lib + 910 LOC binary + benches.
- `safekeeper/src/broker.rs` (345 LOC) — SK push task (publisher).
- `pageserver/src/tenant/timeline/walreceiver/connection_manager.rs`
  (1126 LOC) — broker subscriber + SK selection logic. The broker is
  this file's core data source; it builds `wal_source_connconf` (the
  actual SK address to connect to) from broker-published
  `SafekeeperTimelineInfo` messages.
- `SafeKeeperConf::{broker_endpoint, broker_keepalive_interval,
  disable_periodic_broker_push}` + the `--broker-endpoint` /
  `--broker-keepalive-interval` CLI flags.
- `storage_broker = ...` deps in workspace + `control_plane`,
  `safekeeper`, `pageserver`, `libs/pageserver_api/`.
- `neon_local start` launches the broker child process.

## Plan (single commit)

1. **PS-side SK address discovery.** Verify the storage_controller
   already feeds the PS its SK set via the `/notify-safekeepers`
   upcall (`storage_controller/src/compute_hook.rs:64` — the
   "ComputeHookTimeline for safekeeper change notifications" path).
   Confirm the PS persists / caches that set, and use it as the source
   for `wal_source_connconf` instead of broker data.
2. **Rewrite connection_manager.rs** from broker-driven to a static-SK
   connect-with-reconnect loop. With single-SK the selection logic
   collapses; the core "connect, stream WAL, reconnect on disconnect
   with backoff" loop should be ~300 lines, down from 1126.
3. **Delete `safekeeper/src/broker.rs`** entirely, plus the SK conf
   fields + CLI flags.
4. **Delete the `storage_broker/` crate.**
5. **Stop launching `storage_broker` from `neon_local start`**
   (`control_plane/src/bin/neon_local.rs`).
6. **Drop `storage_broker` from workspace Cargo.toml** + the
   `storage_broker = ...` dep lines in `control_plane`, `safekeeper`,
   `pageserver`, `libs/pageserver_api/`.

## Notes

- The SK push and PS subscriber MUST be removed together, or via a
  transitional commit that introduces the static-SK connconf path
  first. Removing just the SK push would leave the PS subscriber
  blocked forever waiting for updates.
- Read-replica feedback (hot-standby xmin etc.) flows through the
  compute → SK → PS chain as `StandbyFeedback` payloads on the WAL
  streaming connection — independent of the broker. Verify the chain
  still works after the rewrite (existing e2e covers PS catch-up but
  not standby feedback specifically).
- Verify with `cargo build --workspace`, `cargo test -p safekeeper
  --lib`, and `test/e2e/restart-wal-replay-yb.sh`.
