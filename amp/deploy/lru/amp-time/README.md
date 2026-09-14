# amp-time

[amp-time](https://github.com/yugabyte/amp-time) is Mark Peacock's browser UI for
`pg_stat_statements`. It samples the cumulative PGSS counters every ten seconds,
stores reset-aware deltas as navigable timeslices, and inspects an individual
statement with a live generic plan plus catalog-derived DDL and statistics. It
has no YugabyteDB dependencies, which is why it suits an amp endpoint.

It answers the question the `pgstats` sidecar cannot. `pgstats` says which tables
are growing and what the checkpointer is doing; amp-time says which statements
are slow and what plan they are using.

One instance per LRU, because a process reads one DSN:

| LRU | URL |
|---|---|
| transferwise | https://amp-time-transferwise.tail5955.ts.net |
| solidgate | https://amp-time-solidgate.tail5955.ts.net |
| newblack | https://amp-time-newblack.tail5955.ts.net |
| small_temporal | https://amp-time-small-temporal.tail5955.ts.net |

```bash
KUBE_CONTEXT=meko_dev_eks ./up.sh          # idempotent
kubectl -n amp-lru logs deploy/amp-time -c transferwise --tail=20
```

## Why it is shaped like this

amp-time is a plain Python package in a private repo. There is no image, no
chart, no manifests, only a systemd unit for its sibling `amp-showcase`. So the
package is vendored under `vendor/`, shipped as two ConfigMaps, and `psycopg` is
installed into a shared volume at startup, the same way the runner pods build
their drivers on the way up.

Four containers in one pod rather than four Deployments: they differ only in a
DSN and a port, and they share the one `psycopg` install and the one volume.

The ConfigMap names are stable, so **editing `vendor/` does not by itself reach a
running pod** - the package is copied in by an init container. `up.sh` restarts
the Deployment for that reason.

The package is copied *out* of its ConfigMaps rather than mounted where it runs
because a ConfigMap entry is a symlink into a timestamped directory, and amp-time
locates its web assets through `Path(__file__).resolve().parent`, which lands
there instead of next to `web/`.

## Refreshing from source

`vendor/amp_time/` is a `cp` from `src/amp_time/` of a fresh clone, then
re-apply the deviation below. Vendored at
[`88caa53`](https://github.com/yugabyte/amp-time/commit/88caa534b0f0e72f58da672ba9c04b777178e2d7)
(0.5.9).

### Deviation: `aclexplode` on a table with no explicit grants

`inspector.py` read table grants through
`aclexplode(COALESCE(relation.relacl, '{}'::aclitem[]))`. A table created by its
owner with no `GRANT` has `relacl IS NULL`, and `'{}'::aclitem[]` is a
zero-dimensional array, which `aclexplode` rejects with *ACL arrays must be
one-dimensional*. That is stock PostgreSQL behaviour, not an amp one. Every
table in all four LRU databases is in that state, so every inspection failed.
`aclexplode` is strict, so dropping the `COALESCE` returns no rows for a NULL
`relacl`, which is what the `COALESCE` was reaching for.

### Deviation: the statement table mixed units without saying so

`total_exec_time` is a total for the timeslice; `queries/s` and `mean_exec_time`
next to it are per second and per call. Nothing on the row said which, so
`queries/s * mean_exec_time` misses `total_exec_time` by the interval and reads
as a bug. The header now carries the window, taken from the slice's own
`duration_seconds` so it stays right under a different `--interval`.

The value stays a per-slice total rather than becoming a rate: `time_pct` beside
it is a share, and a share only means anything inside one window.

## What it needs from the endpoint

`pg_stat_statements` is preloaded on every amp endpoint but created in no
database, so each instance runs `CREATE EXTENSION IF NOT EXISTS
pg_stat_statements` before it starts sampling.

PGSS does not record `search_path`, so a statement written against a schema
other than `public` cannot be planned without one. `LiveInspector` passes its own
`options` on connect, which overrides anything in the DSN, so the only place a
search_path survives is the database default. transferwise's `seed.sh` sets it:

```sql
ALTER DATABASE transferwise_lru SET search_path = ledger_bench, public;
```

`BEGIN` and `COMMIT` appear in the statement list and cannot be inspected. That
is correct - they have no plan.
