# newblack LRU

The anonymised NewBlack checkout workload running around the clock on amp. It is
the customer's own schema and their own SQL, replayed at the query weights their
production system actually executes, which is the reason this LRU exists rather
than another synthetic generator: the throughput and latency it reports are the
numbers a customer application would see.

```
newblack/
  workload/    the load test, copied verbatim from yugabyte/commerce-loadtest
  bomber.yaml  the driver pod: seed init containers, driver, metrics exporter
  checker.yaml the verdict pod
  exporter.py  driver stdout -> Prometheus, because the driver has no /metrics
  up.sh        creates pool/cluster/endpoint and applies everything
  register.sh  creates or refreshes the report portal row from lru.env
```

## What it runs

`workload/` is a copy of [yugabyte/commerce-loadtest](https://github.com/yugabyte/commerce-loadtest),
the cut-down bundle QA already uses for the YBM multitenancy scale tests. 51
tables and 145 indexes across the schema, 22 query snippets, and two mix files
whose weights come from the customer's production execution counts. The heavy
ones are point selects on `backgroundjobs` and `pricediscounts`, a counter
`UPDATE` on `backgroundjobs`, and CTE transactions that upsert `eventchains_1`
and insert a ledger row, or insert `itemledger_1` from an array of composite
values and roll it up into `inventory_1`.

Two connection lanes, as the customer's application has them: four read-only
workers on a multiplexed Npgsql pool and eight read-write workers on a normal
one, paced to `TARGET_TOTAL_TPS` in the `newblack-bomber-run` ConfigMap. The
driver runs in cycles of `CYCLE_DURATION_SECONDS` and starts the next one when
one ends, forever.

It is a happy path. It is the checkout process working, at production query
proportions, and it will not by itself surface anything that needs an unexpected
event. Anything adversarial belongs in a nemesis test, not here.

## Refreshing the workload from source

The copy in `workload/` is byte-for-byte, so a refresh is a `cp` from a fresh
clone of `commerce-loadtest` plus a re-read of `to_amp_ddl.py`. One file is
deliberately not copied: `tx_bundle_rw.sql` is a comment and a `SELECT 1` that
nothing reads, since the driver builds that transaction from q09/q11/q14/q17/q18
in C#. Do not fix the
copy in place: fix it upstream and copy again, or the LRU stops testing what QA
runs elsewhere.

`to_amp_ddl.py` is the one deliberate deviation. The schema is dumped from
YugabyteDB and carries YB-only DDL that amp's Postgres rejects: `SPLIT INTO n
TABLETS`, `PRIMARY KEY((a) HASH, b ASC)`, `USING lsm`, `CREATE INDEX
NONCONCURRENTLY`, and a leading `yb_hash_code(id) % n` index column that exists
only to spread an index over tablets. The converter strips exactly those and
fails loudly if any survive. Keeping it separate means every difference between
what the customer runs and what amp runs is in one readable file.

## Operating it

```bash
KUBE_CONTEXT=meko_dev_eks ./up.sh          # idempotent, safe to re-run
./register.sh                              # portal row; needs lru.env from up.sh
kubectl -n amp-lru get all -l lru=newblack
kubectl -n amp-lru logs deploy/newblack-bomber -c bomber --tail=40
kubectl -n amp-lru logs deploy/newblack-bomber -c exporter
yb amp lru sync newblack --yes             # run the hourly verdict now
```

Dashboard: <https://grafana-lru.tail5955.ts.net/d/lru-newblack>

The first rollout is slow and that is expected: an init container converts and
loads the schema, another seeds it, and the driver builds itself from source
before the first query. Seeding is skipped on later restarts, since reloading it
would wipe the history the LRU exists to accumulate. To start over, drop the
`newblack_lru` database on the endpoint and delete the pod.

## Metrics

The driver has no metrics endpoint, so `exporter.py` reads its log and exports
`bomber_ops_total{lane}`, `bomber_errors_total`, `bomber_tx_retries_total`,
`bomber_tps`, `bomber_mean_latency_seconds{lane}`, `bomber_qid_ops_total{qid}`
and `bomber_qid_errors_total{qid}`. Counters are cumulative across cycles: the
driver resets its own counts every cycle, and exporting that reset directly
would make `rate()` spike. Each cycle's summary block, not its last progress
line, is what closes a cycle, because the summary carries the final totals.

Disk is covered separately, because a workload that stops when something fills up
looks exactly like one that stopped for an interesting reason. The dashboard has a
"Disk and storage" row and the checker has four rules for it: the node filesystem
behind the driver's volume, `pg_wal` against `max_wal_size`, safekeeper disk, and
pageserver GC drain lag. The node one is the eviction risk, and it is the node's
disk rather than the emptyDir's `sizeLimit`, which `statvfs` cannot see.

The checker's rules are app-side first. A green verdict needs the driver's own
throughput near the paced target, its error and retry ratios low, and its mean
latencies sane, before anything about the endpoint is considered. An endpoint
that is up while the application gets nothing done is not a healthy LRU.

## Tuning the load

Everything is in the `newblack-bomber-run` ConfigMap: `TARGET_TOTAL_TPS`,
`CYCLE_DURATION_SECONDS`, the worker and pool counts, `RO_MULTIPLEXING`. The
checker reads `TARGET_TOTAL_TPS` from that same ConfigMap, so raising the load
moves the throughput rule with it and nothing has to be edited twice. It starts
at 300 TPS on a 4 vCPU endpoint, well under what the endpoint will do, so the
first weeks measure stability rather than a ceiling.
