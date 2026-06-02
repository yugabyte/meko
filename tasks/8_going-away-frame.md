---
status: pending
severity: medium
area: page_service / compute
opened: 2026-05-20
---

# Phase 8 — Page-service "going-away" frame on graceful shutdown

## Symptom

When the pageserver exits cleanly its libpq listener closes
per-connection TCP sockets via the standard `Connections::shutdown`
path (`pageserver/src/lib.rs:201`), delivering a normal FIN. The
compute-side `libpagestore.c` reconnect path is gated on
`pageserver_response_disconnect_timeout` (default 30 s — was 150 s
upstream, lowered in yb-light), so an in-flight `getPage` hangs in
`BufferIo`/`getpage-wait` for up to that long before the compute
gives up and reconnects.

The backend is in a kernel poll on the (now half-closed) socket and
ignores client disconnect, so `pgbench kill` doesn't free it and
operators end up bouncing the postmaster to recover.

## Plan

Add a typed "going-away" frame to the page_service wire protocol: on
graceful shutdown, before closing each socket, the PS sends
`GoingAway { reason }` and the compute treats receipt as "abandon
all in-flight requests on this connection and immediately
reconnect" — vs the 30 s safety-net timeout.

Symmetric change on the compute-side `communicator.c` /
`libpagestore.c`.

## Expected outcome

Recovery from a graceful PS restart drops from ~30 s to <1 s.

The 30 s timer stays as the safety net for the non-graceful case
(PS panic, kernel OOM, network partition).
