# transferwise LRU findings

What the workload has shown on amp, newest first. One dated entry per finding,
with the evidence and the issue it became.

## 2026-09-10 — first run on meko_dev_eks

Deployed against pool `lru-transferwise`, one 2 vCPU / 16Gi endpoint. The schema
converted out of `main.go` and loaded on amp's Postgres 18 with no manual step,
seeded with 20,000 accounts including 1,000 settlement accounts. The four `yb_*`
GUCs the driver sets are skipped with warnings, as documented.

First 15-minute cycle, four threads and two non-transactional readers:

    successful operations: 662189 (read-ops=472172 write-workflows=190017)
    statement totals: 14381997 (read=7448594 write=6933403)
    conflicts observed: 0
    errors: 1
    workflow op/s: 735.77
    statement qps: 15979.99
    READ-OP latency: avg=3.33ms p99~10.00ms max~47.47ms
    WRITE-WORKFLOW latency: avg=10.67ms p99~20.00ms max~227.21ms

Endpoint side: 7,471 commits/s, 5,274 tuples inserted/s, 13,695 fetched/s,
2.0 MB/s of WAL, 331 MB logical size, 1,282m of 2,000m CPU. That is the heaviest
of the four LRUs by commit rate and it still has headroom.

`pg_wal` sits at exactly its 1 GiB `max_wal_size`. That is not this LRU's
doing: measured across the four endpoints the ratio reads 0.05 for newblack,
0.58 for solidgate, 0.84 for temporal and 1.00 here, so it tracks write rate and
the busiest endpoint sits at the soft target. The rule's warn threshold moved
above 1.0 for that reason, since warning on normal behaviour says nothing.

The single error is not yet explained and is worth watching: one in 662,189
operations, with no conflicts recorded. If it recurs at a steady rate it wants a
look at what the driver counts as an error.
