# transferwise LRU findings

What the workload has shown on amp, newest first. One dated entry per finding,
with the evidence and the issue it became.


## 2026-09-17 - latency is drifting up with nothing else moving

`latency_vs_7d_baseline` has climbed steadily since the rule was added: 1.87 on
14 Sep, 2.09 on 15 Sep, 2.20 on 16 Sep, 2.28 on 17 Sep. Write workflow latency
went 25.1 ms to 27.7 ms over the last of those days, and throughput reads 0.92
of yesterday's.

No step change, and nothing else moved. Reads are 1.8 ms, the outbox holds
around 27k rows so retention is keeping up, pageserver GC drain lag has been 0
since the 15 Sep drain (see below), and no absolute floor is near its threshold.
Dataset is about 4.3x the endpoint's file cache and still growing.

Unexplained. It is the exact shape the 7d baseline rule was added for after
`latency_vs_24h_ago` read 1.21 through a four-day decay, so the rule is doing its
job; what it is pointing at is not yet known. Worth separating growth from the
15 Sep build, which are confounded so far.

## 2026-09-14 - pageserver GC lag is a long duty cycle, not a stall

`pageserver_gc_drain_lag_bytes` on transferwise, the only endpoint that has ever
shown it, read off a 6-day range query at 10-minute steps:

    13 Sep 13:27   rises to 0.8 GB, drained ten minutes later
    14 Sep 11:27   rises, peaks at 20.3 GB
    15 Sep 05:47   back to ~0, so 18h20m from rise to drained
    since          flat 0

I first reported this as GC having stopped draining. That was wrong: it drained
itself, `gc_applied_cutoff_lsn` never plateaued, and the shape is a slow duty
cycle on the largest and busiest endpoint. A correction went to the thread.

Two things it leaves open. The rule fails above 10 GB, chosen before any cycle
had been observed, and the one full cycle peaked at twice that, so the threshold
is still unmeasured. And nothing has cycled since 15 Sep 05:47, against a prior
cadence of roughly a day. That is nearly three days quiet. The 15 Sep endpoint
roll landed at ~22:30, *after* that drain finished, so the roll is not evidence
for the quiet period and neither is retention reaching steady state; they are
confounded and no cycle has come along to separate them.

## 2026-09-14 - two of our own rules were wrong

Both found while assessing the LRUs after a quiet weekend, both ours rather than
amp's.

**`checkpoint_write_busy_ratio` measured a constant.** It read 0.9037 on three
LRUs with completely different workloads, identical to four decimal places. The
endpoints run stock `checkpoint_completion_target` 0.9 and `checkpoint_timeout`
300s, so Postgres spreads a timed checkpoint's writes over 270 of every 300
seconds by design and `pg_stat_checkpointer.write_time` counts that whole window,
sleeps included. The rule warned above 0.5, so every healthy endpoint warned
forever - three of the five warnings on the board. Removed. `checkpoints_forced_ratio`
is the rule that discriminates: 54% on transferwise during the 11 Sep incident,
0% everywhere now. The `pgstats` metric stays, because it documents what amp's
default spread does.

**A 24h comparison cannot see a decay slower than 24h.** `latency_vs_24h_ago`
read 1.21 on transferwise while write latency had gone 10.4 -> 21.4 ms over four
days. Added `latency_vs_7d_baseline`: current latency against
`quantile_over_time(0.1, ...[7d:1h])`. It read 1.93 immediately.

The baseline is a low percentile at hourly steps rather than the minimum at ten
minute steps, for two reasons found in review. An absolute minimum is a noise
floor, and it showed: solidgate read 1.22 against its minimum and 1.07 against
its tenth percentile, the difference being one outlier sample. And a 7d/10m
subquery is 1008 evaluations of the inner expression - over small_temporal's
1,720-series `persistence_latency_bucket` that already took 5.2s against `q()`'s
20s timeout, growing with retained history. Hourly steps cost 2.9s. The rule also
carries its own `try/except`, because `loop()` turns any uncaught failure into
`{"checker": ERR}` and drops every other rule with it.

Comparing with the best rather than with a fixed offset matters twice. It needs
no data at any particular age, so a young LRU gets the rule on day one instead of
day eight - three of the four had no 7d history at all when this was written. And
it is not fooled by a window that happens to land inside a previous incident:
transferwise's 72h offset read 0.85, better than now, because three days ago it
was in the pool-starvation dip.

**Latency only.** The same rule on throughput punishes any deliberate rate change
for a week - solidgate reads 0.09 of its best purely because its concurrency was
cut. Throughput keeps the 24h comparison, which is the right shape for a step
change.

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

The cycle went to 60m on that reading and came back to 15m once the pool was
fixed, which is the honest end state: with four connections the same ANALYZE pass
costs about 6% of throughput rather than all of it, and a short cycle keeps the
app-side summary the portal rules read from going stale. Measured across one
pass with the pool sized properly: 8,200-9,100 select/s during, 9,100-9,300
after.

**Why it stopped rather than slowed: our pool was one connection.** Mark's "it
should not stop" was right. ANALYZE takes `ShareUpdateExclusiveLock`, which does
not block DML, and not-granted locks were 0 throughout. The block was ours.
`runOneShotAnalyze` shares the driver's `pgxpool`, and cutting `THREADS` 4 -> 1
cut `POOL_MIN/MAX` with it. That left four things on one connection: one write
worker, two `non-tx-readers`, and ANALYZE. `pool.Exec` holds the connection for
each table, so everything else sat in `Acquire`. `pg_stat_activity` showed
exactly one `ledger_bench` session, running ANALYZE, which is all one connection
can show. At threads=4/pool=4 the same pass cost a quarter of the throughput and
nobody noticed.

**Pool size is not a rate knob.** `-threads` is. The pool needs one connection
per worker plus headroom for the ANALYZE goroutine: `threads + non-tx-readers +
1`. The driver's own default is `max(threads*4, 16)`; we override it, so the
override has to move whenever `THREADS` does.

**ANALYZE really is slow on amp, and that part is not ours.** Timed on a
dedicated connection with nothing else running:

    ANALYZE ledger_bench.fin_txn      30.8 s
    ANALYZE ledger_bench.op_job       28.8 s
    ANALYZE ledger_bench.out_msg_0    25.1 s

`default_statistics_target` is 100, so each of those samples 30,000 rows and
nothing more. Fifteen tables at that rate is about six minutes, which is why the
driver's five-minute budget never completes. On local storage this is a second or
two a table; here every sampled page is a pageserver read (`YbAmp/PS_ReadIO`) and
random sampling across 20M rows is the worst case for a remote page store. Worth
raising with the amp team: autovacuum's own analyze pays the same cost.

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
  by `max_wal_size`, one every 4.7 minutes. (The "25.4 hours of checkpoint write
  time inside a 28.3 hour run" originally cited here is 0.897, which is
  `checkpoint_completion_target` and not evidence of anything - see the 14 Sep
  entry. The forced-vs-timed count is the part that stood up.)
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
