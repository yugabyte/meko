# solidgate LRU

The anonymised Solidgate card-payment transaction running around the clock on
amp. Update-heavy by design: the driver's default transaction is eight
independently chosen statements, roughly 39% reads, 18% inserts and 43% updates
across orders, payment methods and payment events.

```
solidgate/
  workload/    the load test, copied verbatim from
               yugabyte/update-heavy-anon-card-workload, plus to_amp_ddl.py
  runner.yaml  the driver pod: convert + seed init containers, driver, exporter
  checker.yaml the verdict pod
  exporter.py  driver CSV -> Prometheus, because the driver has no /metrics
  up.sh        creates pool/cluster/endpoint and applies everything
  register.sh  creates or refreshes the report portal row from lru.env
```

## What it runs

13 tables and 38 indexes, seeded with 1,200 orders, 40 merchants and 800 payment
methods. The mix is the driver's own and is not ours to set; concurrency is, and
it starts at 8 so the first weeks measure stability rather than a ceiling.

The driver exits when its `-duration` expires and `run.sh` starts it again, so
the workload never ends. Its `-report-every` CSV rows are what the exporter
reads.

The upstream `setup_workload_db.sh` drops the whole database, so it is not used
here: `seed.sh` creates and seeds only when the schema is absent. An LRU exists
to accumulate history, and a pod restart must not throw it away.

## Refreshing the workload from source

`workload/` is byte-for-byte, so a refresh is a `cp` from a fresh clone. Do not
fix the copy in place: fix it upstream and copy again, or the LRU stops testing
what QA runs elsewhere. The shell wrappers are deliberately not copied, because
`run.sh` calls the binary directly. Note that upstream's environment variables
(`CONCURRENCY`, `MINPOOL`, …) are read by those wrappers, not by the binary,
which takes flags.

`to_amp_ddl.py` is the one deviation, shared in shape with the other LRUs. The
schema is dumped from YugabyteDB and carries `SPLIT INTO n TABLETS`, HASH keys
and `CREATE INDEX NONCONCURRENTLY`, none of which amp's Postgres accepts. It
strips exactly those and exits non-zero if any survive.

## Operating it

```bash
KUBE_CONTEXT=meko_dev_eks ./up.sh          # idempotent, safe to re-run
./register.sh                              # portal row; needs lru.env from up.sh
kubectl -n amp-lru get all -l lru=solidgate
kubectl -n amp-lru logs deploy/solidgate-runner -c card --tail=20
yb amp lru sync solidgate --yes
```

Dashboard: <https://grafana-lru.tail5955.ts.net/d/lru-solidgate>

To start over, drop the `solidgate_lru` database on the endpoint and delete the
pod.

## Metrics and rules

The driver computes its own per-interval rates, so `exporter.py` exposes them as
gauges rather than building counters and taking `rate()` over them. That is why
the throughput panels and rules read true immediately instead of after a
five-minute window fills. Only the error columns become counters, because they
arrive as per-interval deltas.

The checker's rules are app-side first: statement QPS, transactions per second,
error rate and per-kind latency, before anything about the endpoint. Its
throughput floors are absolute, because this driver runs unpaced and there is no
target to measure against. They start deliberately low and should be tightened
from the first days recorded in `FINDINGS.md`.

Four more rules cover disk, because a workload that stops because something
filled up looks identical to one that stopped for an interesting reason.

## Tuning the load

Everything is in the `solidgate-runner` ConfigMap: `CONCURRENCY`, the pool
bounds, `STMTS_PER_TXN`, `CYCLE_DURATION`. The checker reads `CONCURRENCY` from
that same ConfigMap so the two cannot drift.
