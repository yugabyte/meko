# solidgate LRU

The anonymised Solidgate card-payment transaction running around the clock on
amp. Update-heavy by design: the driver's default transaction is eight
independently chosen statements, roughly 39% reads, 18% inserts and 43% updates
across orders, payment methods and payment events.

```
solidgate/
  workload/    the load test, forked from
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

## The workload is ours

`workload/` started as a copy of yugabyte/update-heavy-anon-card-workload and
has diverged: the LRU owns these files and fixes land here. `FINDINGS.md`
records each deviation from the source with its reason, so a reader can still
tell the customer's shape from ours. The shell wrappers were never copied,
because `run.sh` calls the binary directly and the wrappers' environment
variables (`CONCURRENCY`, `MINPOOL`, …) are only read by them.

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
throughput floors are absolute and sit under the paced rate, so a driver that
cannot hold its own pace shows up as a change.

Four more rules cover disk, because a workload that stops because something
filled up looks identical to one that stopped for an interesting reason.

## Tuning the load

Everything is in the `solidgate-runner` ConfigMap: `CONCURRENCY`, the pool
bounds, `STMTS_PER_TXN`, `CYCLE_DURATION`, and `TARGET_TPS`, the committed
transactions a second the driver paces all workers to. The checker reads
`CONCURRENCY` from that same ConfigMap so the two cannot drift.

The data grows forever by design and nothing deletes it: the customer does not
archive yet, and the point is to see what gives first. The reads must therefore
stay bounded whatever the size, and `FINDINGS.md` records the one that was not.
