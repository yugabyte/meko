# solidgate LRU findings

What the workload has shown on amp, newest first. One dated entry per finding,
with the evidence and the issue it became.

## 2026-09-10 — first run on meko_dev_eks

Deployed against pool `lru-solidgate`, one 2 vCPU / 16Gi endpoint. 13 tables and
38 indexes converted and loaded on amp's Postgres 18 with no manual step, seeded
with 1,200 orders, 40 merchants and 800 payment methods.

Concurrency was settled by measurement, not by guess. On this endpoint eight
workers and four both hold about 4.6k statements a second and pin it at ~100%
CPU, so four is already past the knee; four merely halves latency (14.5ms
transactions to 6.9ms) for the same throughput. Two workers hold ~2,707
statements a second and 338 transactions a second at 61% CPU, with 0.7ms reads,
0.5ms updates and 5.9ms transactions, and zero errors. That is where it runs, so
a regression shows up as a change rather than as more saturation.

Endpoint side at concurrency 4, for reference: 553 commits/s, 1,875 tuples
updated/s, 14,095 fetched/s, 1.6 MB/s of WAL, 136 MB logical size.

The endpoint records deadlocks at about 0.0003/s over an hour, bursting to
0.003, while the driver reports zero errors: an update-heavy transaction over
shared seeded rows deadlocks sometimes and its retry hides it from the
application. The rule's band was widened to match, because the presence of
deadlocks here is the workload's nature and only a change in the rate is a
signal.

Nothing to report on the product yet. This is the baseline the next weeks are
measured against, and the checker's throughput floors still want tightening.
