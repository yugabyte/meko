# transferwise LRU findings

What the workload has shown on amp, newest first. One dated entry per finding,
with the evidence and the issue it became.

## 2026-09-10 - first run on meko_dev_eks

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

## 2026-09-11 - throughput fell 44% over fourteen hours, every rule green

Found by reading the driver's per-cycle summaries by hand. The checker said OK
throughout, which is the reason the drift and capacity rules in this commit
exist.

    cycle (UTC)       stmt qps   write avg   w p99
    09-10 18:38         15,980     10.67 ms   20 ms
    09-11 07:49         16,549     10.49      20      last good
    09-11 08:04         15,096     11.14      20      onset
    09-11 11:10         11,839     15.28      50
    09-11 14:16         11,423     16.74      50
    09-11 18:09         10,660     18.63      50
    09-11 22:33          9,405     22.02     100

Flat for fourteen hours, then a slide with a sawtooth: every few hours throughput
partially recovers, then resumes falling.

The mechanism is write-side, and it is the endpoint's configuration meeting a
dataset that outgrew it:

- `pg_stat_checkpointer` after 28 hours: 170 timed checkpoints and **203 forced**
  by `max_wal_size`, one every 4.7 minutes, with **25.4 hours of checkpoint write
  time inside a 28.3 hour run**. The checkpointer has effectively never idled.
- `max_wal_size` is **1 GiB** and this workload writes ~2 MB/s of WAL, so it
  fills in about eight minutes. `pg_wal` sat at exactly 100% of the limit for
  twenty straight hours.
- 35 GB of b-tree indexes against **8 GiB of `shared_buffers`**. As the indexes
  grow each insert touches more leaf pages that are not resident, and
  `blks_read/s` climbed from 0 to ~700 over the run.
- Logical size crossed the 43 GB file-cache limit around 05:50 UTC; the slide is
  visible from 08:04. It is now 94 GB, 2.2x the cache.

Not the cause: stale planner statistics. The `errors: 1-3` per cycle are all
`one-shot analyze failed ... context deadline exceeded` on the three
fastest-growing tables, 36 so far. `ANALYZE` on a 6.8 GB table no longer
finishes inside the driver's deadline, which is a symptom rather than the
disease: every other table analyses each cycle and `last_analyze` stays minutes
old.

Growth is 64 GB/day and nothing deletes, which is what Mark asked for. At that
rate the next limit arrives in days.

**Worth raising with the product, not just tuning here.** A default
`max_wal_size` of 1 GiB will do this to any write-heavy customer on amp, and
nothing in the endpoint's own metrics made it visible: the checkpointer had to
be read with psql. That is why `pgstats.py` exists now.
