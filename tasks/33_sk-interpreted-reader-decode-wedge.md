---
status: pending
severity: high
area: safekeeper
opened: 2026-06-11
---

# SK interpreted WAL reader wedges on xlog page-header decode error

## Symptom

During an e2e fast-suite run (2026-06-11, dev box), the main timeline's
WAL sender entered a permanent 15s retry loop, wedging the suite for
80+ minutes:

```
WAL sender:panic{... location=safekeeper/src/send_interpreted_wal.rs:379}:
interpreted wal reader: failed to decode WAL record at start_pos=0/15D9EB0:
Decode(WalDecodeError { msg: "invalid xlog page header: xlp_rem_len=127,
expected 7905", lsn: 0/15DA000 })
```

PS side: walreceiver reconnects every ~15s ("starting replication from
0/15D9EB0, safekeeper is at 0/15DA428 ... connection closed"),
`last_record_lsn` never advances, compute `wait_lsn` blocks forever.
Started right after the branch_ancestor/branch_endpoint scenarios
(endpoint/PS stop-start churn); the `going_away` scenario then hung on
its seed step.

## Hypothesis

`xlp_rem_len` mismatch at a WAL page boundary (`0/15DA000`) on a
re-read from the cache/YB path — same family as the task-23 cold-cache
mid-chunk resume bug ([see 23](23_sk-kill9-wal-hole-flake.md)): the
reader stitches a page whose continuation header doesn't match the
record it thinks it's resuming. Deterministic once entered (every retry
decodes the same bytes), so it wedges rather than flakes.

Not related to the task-28 PS-side fixes (PS rel-size cache); observed
on a build whose only delta from main was that PS change.

## Repro lead

**2 for 2 on full fast-suite runs (2026-06-11)**: both wedged at the
same point — `going_away`'s seed step, right after
branch_ancestor/branch_endpoint — with the same decode signature on
safekeeper-1. Individual scenario runs (fresh cluster each) do not
trigger it; the branch-scenario sequence appears to arm it. This
currently blocks any full `run.sh fast` pass. The wedge state is also a
detection gap: the WAL sender panics in a loop and the suite has no
timeout — consider (a) fixing the decode, (b) making the harness
fail-fast on repeated `interpreted wal reader` panics in SK logs
(the PS-side e2e marker scan doesn't read SK logs).

## Notes

Full logs were in `.e2e-tests/run_1781185124_2613489/` (cleaned);
the panic excerpt above is the complete signature.
