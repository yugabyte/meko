# solidgate LRU findings

What the workload has shown on amp, newest first. One dated entry per finding,
with the evidence and the issue it became.


## 2026-09-17 — about to cross its file cache, which makes it the clean test

`days_until_file_cache_full` warned for 226 minutes on 17 Sep, the first time this
LRU has warned on capacity. Logical size is 0.59x the endpoint's file cache and
growing at roughly 6.8 days to full. Throughput is unchanged, pinned at its 50
txn/s pace with 6.8 ms average transaction latency.

This matters beyond solidgate. Crossing the file cache is the one thing that moved
across the 11 Sep transferwise inflection, but it is not sufficient on its own:
small_temporal has run past its own cache since 12 Sep with no measurable effect,
at a much lower write rate. What else is required has never been established, and
the thread cut from 4 threads to 1 was transferwise's response, not a measured
cause. solidgate is rate-limited by design, so it crosses at a fixed write rate
and is the cleanest available test of the crossing on its own.

Nothing to do but watch. Do not raise the endpoint size to avoid the crossing.

## 2026-09-11 — one read grew with the data, and the wire with it

Twenty-two hours in, network out of the endpoint had climbed steadily while
commits fell from 288/s to 178/s and average read latency went from 0.7ms to
2.2ms. `pg_stat_database` showed 272k tuples fetched a second against 178
commits, about 1,500 rows per transaction.

`pg_stat_user_indexes` named it: `order_events_pay_profile_id_pay_profile_scope_idx`
had read 10.0 billion tuples over 22.2 million scans, 453 a scan and rising.
That is `r_profile_by_event`, which joined `order_events` a second time on
`pay_profile_id` alone and so returned every event a profile ever had. Every
inserted event landed on one of the 1,200 seeded profiles (`i_order_event` used
a seeded order id where its siblings use the new synthetic one), so each key
held 4,542 rows: one execution returned 2.0 MB, at ~46 executions a second,
about 90 MB/s of result traffic from one read. Not an amp fault: `blks_read`
stayed near zero, WAL flat at 1 MB/s, and the plan was already an index-only
scan on the right index. The result set was the problem.

Fixed in the driver, which the LRU owns from here: the second join now takes
the 20 latest events of the profile through a `created_at DESC` index (34
buffers and 0.56ms on the same key, against 4,464 and 46ms), `i_order_event`
references its own order, and a `-target-tps` flag paces the workers. The index
was built online on the live database and nothing was dropped or reseeded: the
data is the point of this LRU. It now runs at `TARGET_TPS=50`, ~3 GB a day, and
keeps growing until something gives.

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
