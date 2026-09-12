# transferwise LRU findings

What the workload has shown on amp, newest first. One dated entry per finding,
with the evidence and the issue it became.

## 2026-09-12 - the driver spends a third of every cycle in ANALYZE

Mark read the amp-time chart as "the wise workload stopped". It had not. It was
analysing.

`runOneShotAnalyze` walks all fifteen tables a minute into every cycle with a
five-minute budget. At 20M rows a table it spends the whole budget and still
dies on `one-shot analyze failed (ANALYZE "ledger_bench"."fin_txn"): timeout`.

amp-time, ten-second slices:

    end_utc     select/s  insert/s  update/s
    08:09:25     2076.2     720.7     136.9
    08:09:35      255.0      85.3      16.9
    08:09:45        9.8       0.0       0.0   <- writes stop
    08:11:15       10.2       0.0       0.0

`pg_stat_activity` in that window holds one workload session, `ANALYZE
"ledger_bench"."acct_link"`, waiting on `YbAmp/PS_ReadIO`. Not-granted locks: 0.
Nothing is blocked.

Over eighteen minutes, **40% of slices had zero inserts**, in stretches of 4.8
and 2.2 minutes, each beginning a minute after a cycle started. While writing,
one thread does 1515 select/s, 453 insert/s, 88 update/s, which is what it
should.

The cost is fixed per cycle, so four cycles an hour paid it four times: 33% dead
time at 15m, 8% at 60m. Hence `CYCLE_DURATION: 60m`. Fifteen minutes only ever
existed because this driver prints nothing until its end-of-run summary, and
amp-time now samples every ten seconds.

**`summary_age_s` is coupled to the cycle length.** One summary lands per cycle,
so its floor has to clear a whole cycle plus the ANALYZE inside it. Left at
1500/2700 on an hourly cycle it would read FAIL forever. Any change to
`CYCLE_DURATION` has to move it.

**Not the index.** The 24% query plans correctly:

    Limit  (cost=0.56..166.59 rows=41 width=18)
      ->  Index Scan using fin_txn_pkey on ledger_bench.fin_txn
            Index Cond: ((account_id = $1) AND (txn_id > $2))

`fin_txn_pkey` is `(account_id, txn_id)`, matching the predicate and the ORDER
BY, so no sort. The `rows=41` that reads as a bad estimate is amp-time's: it
plans with typed NULL placeholders under `force_generic_plan`, because PGSS
records no parameters. **Read the index choice from an amp-time plan, never the
row estimate.**

**`idx_fin_txn_account_created` has 0 scans** since this LRU started, maintained
on 23M rows for nothing. Upstream schema, not fixed here.

**Retention's catch-up is expensive and finite.** Switched on against a 23M-row
backlog it reached 44% of database execution time, 3,614 tup_deleted/s against
1,426 commits/s, draining ~3.3M rows/hour. It blocks nothing. Steady state is
about one 5,000-row batch a minute; the 20-batch cap exists only for catch-up.
Turning retention on for the first time against a large table costs hours of
unrepresentative throughput, which is worth knowing before doing it to another
LRU.

## 2026-09-12 - the cause was dataset growth, not max_wal_size

The `max_wal_size` reading in the entry below is wrong, and Hari said so in
`#amp-real-lru`. Checked properly with a range query: `pg_wal` sat at 100% of the
limit from the first hour while throughput held flat for twelve, so forced
checkpoints were already running at full rate through the entire healthy period
and cannot be what changed. The ratio only falls below 1.00 *after* throughput
does, because less throughput is less WAL.

The one thing that moves across the inflection is dataset size crossing the file
cache:

    UTC          commits/s  wal_ratio  blks_read/s  logical GB
    09-11 05:12      8322       1.00         373       40.9
    09-11 06:12      8477       1.00         404       44.8   <- crosses 43 GB cache
    09-11 08:12      7620       1.00         654       52.4   <- decline starts
    09-12 03:12      5343       0.80         743      106

**Where the growth goes.** One payment writes 25 rows across 13 tables, ~5.0 KB.
39% of the 107 GB is indexes.

    ledger/txn (6 tables)   60%   3000 B/payment
    audit (evt_audit)       17%    867
    plumbing (5 tables)     11%    562
    jobs (op_job+blob)      10%    490
    outbox (out_msg_0)       2%     95

Mark's answer to "is it growing in the right places" is yes: 60% is in the ledger
and transaction tables, which is where a ledger should grow.

**What changed as a result**, agreed in the thread:

- threads 4 -> 1. ~50 payments/s, ~21 GB/day against ~86. The only rate knob
  `ledger_bench` has.
- a retention sweeper deleting the outbox and processed jobs, exactly what
  TransferWise deletes. 585 B of 5.0 KB per payment, so 12% of growth and no
  more. Mark: "almost every one of our bigger RDS customers today doesn't really
  delete outside of outbox patterns much".
- no drop-and-recreate. Hari suggested running for x days and rebuilding; Mark's
  objection stands - "if we drop and recreate, how will we ever see the edges of
  bigger DBs on AMP".
- amp-time deployed against all four endpoints, so the next step (more balance
  lookups, adjusting the read/write ratio) is made against measured plans rather
  than guesses. `-write-ratio` defaults to 0.62 and is the knob for that.

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
