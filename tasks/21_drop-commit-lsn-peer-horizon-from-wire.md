---
status: done
severity: low
area: safekeeper
opened: 2026-06-05
closed: 2026-06-05
---

# Phase 21 — Drop `commit_lsn` / `peer_horizon_lsn` from walproposer + broker wire formats

After the in-tree commit_lsn / peer_horizon_lsn cleanup, the fields
are now zero-or-flush-lsn pass-throughs on the wire. yb-light is a
private fork with no legacy peers to maintain compat with, so the
wire fields themselves can go.

## Coordinated Rust + C changes

Both the Rust SK (`safekeeper/src/safekeeper.rs`) and the C
walproposer (`pgxn/neon/walproposer.c` + `walproposer.h`) parse the
binary message layouts. Field removal must touch both sides
atomically — easiest path is a new `SK_PROTO_VERSION_4` that omits
the dropped fields; bump `neon.safekeepers_protocol_version` default.

### Rust SK

- Drop `commit_lsn` and `truncate_lsn` from `AppendRequestHeader`,
  `AppendRequestHeaderV2`, `AppendResponse`, `VoteResponse`.
- Drop matching reads/writes in the binary parse/serialize blocks
  (`safekeeper/src/safekeeper.rs:566-580` for parse; corresponding
  serialize block).
- Drop `commit_lsn` / `peer_horizon_lsn` fields from
  `SafekeeperTimelineInfo` (broker proto) — pairs with task #99
  (storage_broker removal) where the whole proto goes away.
- Drop the field from HTTP-debug `TimelineStatus` /
  `SkTimelineInfo`.

### C walproposer

Required field removals (search for `truncateLsn`, `commitLsn`):

- Message struct fields in `walproposer.h` (`VoteResponse`,
  `AppendRequest`, `AppendResponse`).
- All `truncateLsn` reads/writes in `walproposer.c` — including the
  election-time computation (`wp->truncateLsn = Max(...)` at
  line ~1148, ~1199) and the assertion at line 1367
  (`Assert(wp->truncateLsn != InvalidXLogRecPtr || syncSafekeepers)`).
- Replace `wp->commitLsn` derivation: today it's `Max` of
  `sk->appendResponse.commitLsn` across SKs; in yb-mode it can come
  from `sk->appendResponse.flushLsn` directly (single SK = no Max).
- AppendRequest no longer needs to carry `commitLsn` or `truncateLsn`
  — the SK derives commit-equivalent from its own flush watermark.

### Build / verification

- Rebuild `neon.so` for v17 (the `neon-pg-ext-v17` make target).
- Bump the bundled extension version in the deployment ship script.
- Verify e2e: a fresh compute + new SK must succeed on the new
  protocol version; the walproposer must elect, stream, and ack
  without referencing the removed fields.
- The pageserver currently consumes `commit_lsn` off the
  `PrimaryKeepAlive` message in its walreceiver. That's the standard
  Postgres replication protocol, not the walproposer protocol —
  leave it untouched (replica connections still need it).

## Notes

- Bumping the proto version means the C extension default must move
  to 4 in lockstep with the SK binary that's shipped — neither side
  can roll forward independently.
- This task is downstream of #99 (storage_broker removal); doing #99
  first removes one of the wire-format users.
