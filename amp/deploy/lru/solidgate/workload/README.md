# update_heavy_anon

A synthetic card payment workload combining indexed reads, inserts, and frequent
updates across orders, payment methods, and payment events.

The default transaction contains 8 independently selected statements, with an
approximate mix of 39% reads, 18% inserts, and 43% updates. CTE mode defaults to off.
Updates target seeded IDs, partitioned between workers. Application reads use
`SELECT *`, including joins across related tables, and every read is bounded:
the profile read takes at most the 20 latest events of a profile, so read cost
does not grow with the data the inserts keep adding.

The driver runs unpaced by default. `TARGET_TPS` paces all workers to a total
committed-transaction rate, for long runs where the insert growth should look
like a business rather than a benchmark.

## Requirements

- Go 1.24 or later and Bash.
- `psql` and access to a YugabyteDB YSQL database.
- Database credentials supplied at runtime through environment variables.

The schema uses YugabyteDB-specific HASH keys and tablet split clauses.
All 13 base tables and hash indexes start with 3 tablets. Every index led by a
timestamp uses `ASC` and starts with 1 tablet; the one per-profile event index
ends in `created_at DESC` so the profile read can take the latest few events.
There are no triggers, partial indexes, or seed truncation statements.

## Initialize or Reseed

Run these commands in Bash. Setup drops and recreates the database named by `DB`;
stop any workload using that database first. Use the same command to reinitialize
and reseed it.

```bash
cd update_heavy_anon
export HOST='<database-host>'
export PORT=5433
export USER_NAME='<database-user>'
export DB=update_heavy_anon
export PGSSLMODE=require
read -r -s -p 'Database password: ' PGPASSWORD
printf '\n'
export PGPASSWORD
./setup_workload_db.sh
```

The seed contains 1,200 orders, 40 merchants, and 800 payment methods. Related
tables contain matching rows or deterministic subsets. Keep `SEED_ORDERS=1200`,
`SEED_SHOPS=40`, and `SEED_TOKENS=800` unless you adjust the seed to match.
`SEED_TOKENS` is the existing environment option for payment-method cardinality.

`setup_workload_db.sh` accepts `HOST`, `PORT`, `USER_NAME`, `PASSWORD` or
`PGPASSWORD`, `DB`, `SCHEMA_FILE`, and `SEED_FILE` overrides. Host, username, and
password have no saved defaults.

## Inspect Query Plans

The plan file covers all 20 active statement shapes using synthetic sample values.
It uses `EXPLAIN` without `ANALYZE`, so inserts and updates are not executed.
Inspect the plans before running load.

```bash
psql -X -h "$HOST" -p "$PORT" -U "$USER_NAME" -d "$DB" \
  -v ON_ERROR_STOP=1 -f explain_active_queries.sql
```

## Run

Keep `PGPASSWORD` exported from setup. The driver reads the password from the
environment, so it does not need to be included in `DSN`.

```bash
export DSN="host=$HOST port=$PORT dbname=$DB user=$USER_NAME sslmode=$PGSSLMODE"
set -o pipefail
CONCURRENCY=12 \
MINPOOL=12 \
MAXPOOL=12 \
DURATION=10m \
REPORT_EVERY=30s \
STMTS_PER_TXN=8 \
TARGET_TPS=0 \
CTE=false \
CTE_FIXED_SHAPE=false \
./run_workload.sh | tee run_latest.csv
```

`run_benchmark.sh` is also available with the same environment options.
The runner builds the executable when it is missing or its source has changed.
Use `DURATION=2m` for a short smoke run.

CSV interval rows report statement QPS, committed transactions per second,
average latency, error counts, and connection pool occupancy. Use interval rows
for comparisons; the final row reports the remaining counter delta at shutdown.
Successful statement counts can include work from transactions later rolled back.

For direct node connections, add `DIRECT_EVEN=true DIRECT_EVEN_NODES=3`. This
discovers primary endpoints through `yb_servers()` and divides workers and pool
budgets across them. Use budgets divisible by 3 and ensure the client can reach
the discovered endpoints. Once running, workers retry connection failures with
backoff until the run ends; initial connection and pool setup failures exit.

## Contents

- `workload.go`: workload statements, transaction execution, and reporting.
- `workload_schema.sql`: narrow tables and indexes.
- `seed_full.sql`: synthetic seed data and statistics collection.
- `setup_workload_db.sh`: database recreation, schema initialization, and seeding.
- `run_workload.sh` and `run_benchmark.sh`: benchmark entry points.
- `explain_active_queries.sql`: sample plans for all active statement shapes.
- `go.mod` and `go.sum`: dependency declarations and checksums.

Keep runtime credentials in the shell environment. Local credential files,
executables, and run output are excluded by `.gitignore`.
