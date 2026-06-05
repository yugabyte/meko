---
status: done
severity: medium
area: safekeeper
opened: 2026-06-05
closed: 2026-06-05
---

# Phase 20 — Remove the storage_broker crate

Done. Single-SK yb-mode made the broker's "which SK is most caught up"
selection trivial; its WAL-availability signaling was unnecessary
because the SK→PS WAL stream is long-lived. The broker was dead weight.

## What landed

- PS-side `wal_source_connconf` now comes from
  `PageServerConf::safekeeper_pg_addr` (`host:port` string sourced from
  the matching TOML field), not from a broker subscription.
- `pageserver/src/tenant/timeline/walreceiver/connection_manager.rs`
  collapsed from 1126 LOC to ~440 LOC: single-connection
  connect-with-reconnect-backoff loop, no candidate selection.
- `safekeeper/src/broker.rs` (345 LOC SK push task) deleted.
- `SafeKeeperConf::{broker_endpoint, broker_keepalive_interval,
  disable_periodic_broker_push}` removed; `--broker-endpoint` /
  `--broker-keepalive-interval` CLI flags gone.
- `Timeline::record_safekeeper_info` + the
  `/v1/record_safekeeper_info/...` HTTP route deleted.
- `LocalSkInfo` (in `safekeeper/src/timeline.rs`) replaces the old
  `storage_broker::proto::SafekeeperTimelineInfo` usage at the few
  internal call sites that still needed timeline state snapshots
  (`wal_backup_partial`'s pre-upload `(flush_lsn, commit_lsn,
  last_log_term)` capture).
- `storage_broker/` crate deleted entirely (lib + binary + benches +
  proto definitions).
- `neon_local`'s `StorageBroker` subcommand, `NeonBroker` LocalEnv
  field, `[broker]` TOML section, and broker process management
  removed.
- `control_plane/src/pageserver.rs` now emits
  `safekeeper_pg_addr='127.0.0.1:<port>'` into `pageserver.toml`
  instead of `broker_endpoint='...'`.

## Verification

- `cargo build --workspace` clean.
- `cargo test -p safekeeper --lib` green.
- `test/e2e/restart-wal-replay-yb.sh` passes end-to-end on the new
  static-SK PS↔SK plumbing.

## Notes / stragglers

- `docker-compose/pageserver_config/pageserver.toml` still has a
  legacy `broker_endpoint='...'` override line — it's a deploy
  artifact outside the workspace build; the deploy script that
  consumes it will need to flip to `safekeeper_pg_addr` if/when the
  docker-compose path is exercised.
- `Timeline.broker_active: AtomicBool` and `broker_active_set:
  Arc<TimelinesSet>` SK-internal naming survive — stale field names
  but functionally correct (they're internal book-keeping, not
  broker-coupled). Leaving for a future renaming pass.
