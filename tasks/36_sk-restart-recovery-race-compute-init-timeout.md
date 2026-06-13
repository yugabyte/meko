---
status: pending
severity: medium
area: e2e
opened: 2026-06-13
---

# Compute startup times out in Init when the safekeeper is mid-restart

## Symptom

In `test/e2e torn_wal_write`, a recovering compute fails with:

```
command failed: compute startup timed out 90s; still in Init state
  FAIL — F pageserver+compute: yb_light_local endpoint start main exited with exit status: 1
```

The compute_ctl loops restarting PG (~every 15-20s); PG's walproposer
can't reach the safekeeper:

```
[WP] failed to connect to node '127.0.0.1:5484': Connection refused
[WP] failed to read from node 127.0.0.1:5484 ... server closed the connection
FATAL: terminating connection due to unexpected postmaster exit
```

so it never leaves Init and neon_local times out at 90s. Once one mode
fails this way, the iteration's remaining modes cascade.

## Likely cause

Mode E (safekeeper + compute kill) immediately precedes mode F, and
mode F's compute recovery races the safekeeper coming back up — i.e. the
harness recovers/recycles the compute before the just-restarted SK is
accepting connections (16 SK restarts in one -n 10 run, under heavy YB
load). Test-infra timing, distinct from the task-34 corruption (0
corruption signatures) and the task-35 walreceiver wedge.

## Fix direction (for sign-off)

Harness-side robustness: after "restart crashed infra", wait for the
safekeeper to be reachable (pg port 5484 accepting) before
`recycle_compute`/`endpoint start`; and/or raise the compute-start
timeout for the chaotic multi-kill modes. Confirm it's not a real SK
crash (the SK process was up and serving the PS afterward — looks like a
restart/accept-readiness race, not a crash).

## Repro

`test/e2e/run.sh -n 10 --no-fail-clean torn_wal_write` — watch for
`compute startup timed out 90s; still in Init` in the harness output and
`Connection refused` to port 5484 in the compute log.
