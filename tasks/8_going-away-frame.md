---
status: done
severity: high
area: page_service / compute
opened: 2026-05-20
closed: 2026-06-03
---

# Phase 8 — Page-service "going-away" frame on graceful shutdown

## Resolution

Added a new pagestream BE tag `T_NeonGoingAwayResponse = 106` /
`PagestreamBeMessageTag::GoingAway = 106` plumbed end-to-end:

- Wire format: tag + reason byte. V3 pads the echo-header trio
  with zeros so the existing parser logic stays symmetric.
- Pageserver side: `Connections` now holds a `graceful_cancel`
  token alongside the hard `cancel`. On `Connections::shutdown`
  it fires graceful first, gives handlers a 500 ms window to
  flush a `GoingAway` frame, then fires the hard cancel as a
  fallback for stragglers. Both the serial and pipelined
  pagestream loops watch `graceful_cancel`; on fire they call
  `PageServerHandler::send_going_away` to write + flush the
  frame, then return Ok cleanly.
- Compute side: `nm_unpack_response` decodes the tag into
  `NeonGoingAwayResponse`. Both `pageserver_receive` and
  `pageserver_try_receive` intercept the tag right after unpack:
  log the reason, call `pageserver_disconnect(shard_no)`, return
  NULL. The caller's existing "connection gone, retry"
  machinery handles the rest — no behaviour change on the
  reconnect path.
- Tests: `going_away_v3_roundtrip` and `going_away_v2_wire_shape`
  in `pageserver_api::pagestream_api`. New e2e
  `test/e2e/going-away.sh` boots a cluster, gracefully stops
  the pageserver, and asserts the PS log contains
  `writing GoingAway frame on graceful shutdown` entries.

## Scope note

The frame is processed only by compute backends that are
actively in `call_PQgetCopyData` (i.e. waiting on a response)
at the moment the bytes arrive. Backends idle in the postgres
main loop don't drain their pagestream recv buffer before
discarding the connection on next-query reconnect, so the
queued frame is lost for them — but idle backends were never
the problem in the first place: the bug was in-flight reads
hanging on a half-closed socket up to
`pageserver_response_disconnect_timeout` (30 s). Idle backends
already handled the next-query reconnect gracefully without
needing the frame.

## Expected outcome (now realised)

Recovery from a graceful PS restart drops from the 30 s
read-timeout safety net to immediate disconnect for any
backend that was in-flight at shutdown. The 30 s timer remains
the safety net for non-graceful cases (PS panic, kernel OOM,
network partition).
