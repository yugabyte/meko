# transferwise LRU

The anonymised TransferWise ledger payment transaction running around the clock
on amp. One write workflow is a single transaction that records a job, a
transaction, idempotence rows, financial steps, balance changes, six ledger legs,
an audit event, a balance snapshot and an outgoing message. Concurrent readers do
account, balance, ledger, transaction and job lookups, some transactional and
some not.

```
transferwise/
  workload/    the load test, copied verbatim from yugabyte/ledger_bench,
               plus to_amp_ddl.py
  runner.yaml  the driver pod: convert + seed-gate init containers, driver,
               retention sweeper, exporter
  checker.yaml the verdict pod
  exporter.py  driver summary -> Prometheus, because the driver has no /metrics
  up.sh        creates pool/cluster/endpoint and applies everything
  register.sh  creates or refreshes the report portal row from lru.env
```

## Rate and retention

It runs **one thread**, not four. At four it wrote ~86 GB a day, crossed the
endpoint's 43 GB file cache inside a day, and lost 44% of its throughput over the
following fourteen hours while staying green on the portal. One thread is ~50
payments a second and ~21 GB a day, which buys about a week before the same
crossing. `ledger_bench` has no pacing flag, so `-threads` is the only rate knob
there is.

A **retention sweeper** deletes exactly what TransferWise deletes and nothing
more: the outbox (`out_msg_0`, fifteen minutes) and processed jobs (`op_job`
and `op_job_blob`, a day). That is 585 bytes of the 5.0 KB a payment writes, so
it slows growth by 12%. The other 88% accumulates on purpose - real customers do
not delete it either, and watching a database get large on amp is the experiment.

Both statements pick their victims through an index and delete through a primary
key. The obvious `delete ... where id in (select ...)` does not: the planner
hash-joins the subquery against a sequential scan of every row in the table, once
a minute, forever.

## Two things shape this LRU

**The driver prints only an end-of-run summary.** No interval rows, no progress
lines, so the app-side series move once a cycle. Continuous liveness comes from
the endpoint's own Postgres metrics, amp-time and `pgstats`.

The cycle is an hour. It was fifteen minutes and the driver's per-cycle `ANALYZE`
pass turned that into a third of the workload's time; FINDINGS has the numbers.
`summary_age_s` in the checker is tied to `CYCLE_DURATION` and moves with it.

**Its `-init` drops and recreates the workload tables.** It runs exactly once,
gated on `core_account` being absent rather than on a flag someone has to
remember, because an LRU exists to accumulate history and a pod restart must not
throw it away.

## Refreshing the workload from source

`workload/` is byte-for-byte, so a refresh is a `cp` from a fresh clone. Do not
fix the copy in place: fix it upstream and copy again.

`to_amp_ddl.py` is the one deviation, and here it runs over **`main.go`** rather
than a `.sql` file, because this driver carries its schema in Go string
literals. The converted copy still compiles. It strips `SPLIT INTO n TABLETS`,
HASH primary keys and `CREATE INDEX NONCONCURRENTLY`, and exits non-zero if any
survive.

The driver also sets four `yb_*` session and database GUCs at startup. On amp
these do not exist and it logs `warning: skipping session prep statement` for
each, which is the documented behaviour and not a problem.

## Operating it

```bash
KUBE_CONTEXT=meko_dev_eks ./up.sh          # idempotent, safe to re-run
./register.sh                              # portal row; needs lru.env from up.sh
kubectl -n amp-lru get all -l lru=transferwise
kubectl -n amp-lru logs deploy/transferwise-runner -c ledger --tail=30
kubectl -n amp-lru logs deploy/transferwise-runner -c retention --tail=10
yb amp lru sync transferwise --yes
```

Dashboard: <https://grafana-lru.tail5955.ts.net/d/lru-transferwise>

To start over, drop the `transferwise_lru` database on the endpoint and delete
the pod; the seed gate will initialise again.

## Metrics and rules

Totals are cumulative across cycles: the driver counts from zero every cycle, so
each summary is added to a running base rather than replacing it, which would
make `rate()` see a reset. Rates and latencies from the last summary are exposed
as gauges.

The checker's throughput floors are absolute, because this driver runs unpaced.
They start deliberately low and should be tightened from the first days recorded
in `FINDINGS.md`. Conflicts get their own rule rather than counting as errors:
they are expected on a ledger running `SELECT ... FOR UPDATE`, and what matters
is whether the ratio jumps.

## Tuning the load

Everything is in the `transferwise-runner` ConfigMap: `THREADS`, the pool
bounds, `NON_TX_READERS`, `CYCLE_DURATION`, `WRITE_LOCK_MODE` (`for-update` or
`advisory-xact`), and the seed account counts. The driver wants the same account
counts on a run as it was seeded with, so `run.sh` passes them on both.
