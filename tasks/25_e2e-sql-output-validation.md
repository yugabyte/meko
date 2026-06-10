---
status: done
severity: medium
area: yb-pageserver
opened: 2026-06-10
closed: 2026-06-10
---

# Validate the output of `.sql` e2e tests

## Symptom

`test/e2e/run.sh` runs `*.sql` tests via
`psql -v ON_ERROR_STOP=1 -f <file>` (`run_sql_workload`,
`test/e2e/run.sh:203`) and only checks two things: every statement
completes without a SQL error, and no corruption markers appear in
the pageserver log. The *result sets* are never checked. A read path
that silently returns wrong rows — stale page versions, missing
tuples after an index build, a lineage read that drops a delta —
passes the suite as long as no statement errors out. The `.sh` tests
(e.g. `branch-endpoint.sh`) assert counts explicitly; the four `.sql`
workloads (`fast/smoke.sql`, `fast/update-churn.sql`,
`slow/btree-splits.sql`, `slow/full-workload.sql`) do not.

## Fix sketch

pg_regress-style expected output:

- For each `test/e2e/<cat>/<name>.sql`, add
  `test/e2e/<cat>/<name>.expected` holding the wanted psql output.
- `run_sql_workload` captures psql stdout (normalised: strip
  `Time:` lines from `\timing`, libpq version-warning noise, and any
  other nondeterminism — e.g. emit counts/checksums instead of raw
  `random()` data) and diffs it against the `.expected` file;
  any diff fails the test with the diff shown.
- Workloads that use `random()` must assert through deterministic
  projections (counts, sums of deterministic columns, checksums of
  ordered ids) rather than raw rows — adjust the four `.sql` files
  to end each phase with such verification queries.
- Keep `ON_ERROR_STOP` + the corruption-marker scan; output diffing
  is an additional gate, not a replacement.

## Notes

- The `.sql` workloads were written as load generators, not
  correctness checks — most of their value is the page-version churn.
  Adding verification queries (e.g. `SELECT count(*), sum(indexed_val)
  FROM big_table` before/after a churn phase, repeated after
  restarts) is where the real signal is; the `.expected` diff is the
  enforcement mechanism.
- Related: task 12 (e2e test infra), task 24 (a correctness hole the
  current `.sql` tests cannot catch precisely because output is
  unchecked).

## Resolution

`run_sql_workload` (`test/e2e/run.sh`) now captures psql stdout,
normalises it (`normalize_psql_output`: drops `\timing` `Time:` lines,
libpq version warnings, trailing whitespace), and diffs against a
sibling `<name>.out` golden (pg_regress convention). A mismatch fails
the test with the diff; a missing golden fails with a regen hint.
`E2E_REGEN_EXPECTED=1` regenerates goldens. The corruption-marker scan
+ `ON_ERROR_STOP` are kept as additional gates.

The four workloads were made deterministic so goldens are stable:
`random()*1e6` → Knuth hash `((g::bigint*2654435761)%1000000)::int`
(still scatters index keys → still drives btree splits), `TABLESAMPLE
SYSTEM (n)` → fixed `id % k = 0` slice. Each gained count/sum/checksum
verification footers (ordered `string_agg` md5 digests for the small
tables) before and after the churn phase. Goldens checked in:
`{fast/smoke,fast/update-churn,slow/btree-splits,slow/full-workload}.out`.

Verified: `test/e2e/run.sh all` regenerates and then (without regen)
diffs clean — "output matches" on all four workloads.
