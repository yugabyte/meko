# ledger_bench

`ledger_bench` is a Go workload for YugabyteDB YSQL that simulates ledger payment processing: coordinated financial writes in one transaction, alongside transactional and non-transactional reads.

Each write workflow records a job, transaction, idempotence records, financial steps, balance changes, ledger records, audit events, a balance snapshot, and an outgoing message. It commits once. The default locking mode uses `SELECT ... FOR UPDATE`; about half of workflows use a straight-through path that reduces job and transaction status updates.

Concurrent readers perform account, balance, ledger, transaction, and job lookups. The job status query matches `job_status = 'SETTLED'` and a transaction ID, supported by an index with `job_status HASH`. Account transaction reads also use ascending index scans.

## Files

```text
ledger_bench/
  .gitignore
  README.md
  go.mod
  go.sum
  main.go
```

`main.go` contains the schema, seed statements, workload SQL, connection pool, retries, and statistics. Initialization creates the tables and indexes before inserting seed data. Explicit table and index definitions use `SPLIT INTO 6 TABLETS`; explicit index creation uses `NONCONCURRENTLY`. The schema has no foreign keys.

## Build

Use Go 1.22 or later and a reachable YugabyteDB YSQL database. The schema uses YugabyteDB-specific SQL.

```bash
go build -trimpath -o ledger_bench .
```

## Connection

Supply your own connection details at runtime. There are no built-in credentials or database endpoints.

In Bash, enter the connection string privately, then enter the password separately:

```bash
read -r -s -p 'Connection string: ' LEDGER_BENCH_DSN
printf '\n'
export LEDGER_BENCH_DSN
read -r -s -p 'Database password: ' PGPASSWORD
printf '\n'
export PGPASSWORD
```

Use a password-free connection string with this shape, replacing the placeholders:

```text
host=HOST_A,HOST_B,HOST_C port=5433 user=DB_USER dbname=DB_NAME sslmode=verify-full sslrootcert=/path/to/root.crt connect_timeout=10 load_balance=true
```

Use the TLS settings and CA certificate required by your database. A single host is also supported. With multiple hosts and `load_balance=true`, new connections rotate across the supplied hosts, with the others retained as fallbacks. The client does not discover additional servers automatically. The database application name is `ledger_bench`.

The `-dsn` flag can override `LEDGER_BENCH_DSN`. Keep passwords in `PGPASSWORD` rather than in command arguments. Entering connection details at the prompts keeps their values out of shell history.

## Fresh Initialization

Stop any running instance before initializing. **`-init` drops and recreates the workload tables in the selected schema, deleting their existing data.** Use a dedicated schema: initialization also removes foreign keys in that schema.

```bash
./ledger_bench \
  -schema=ledger_bench \
  -init -run=false \
  -accounts=20000 -settlement-accounts=1000
```

This seeds 20,000 accounts, including 1,000 settlement accounts, plus their initial balances and ledger rows. Job, transaction, audit, and other workflow tables start empty. `-run=false` leaves the workload stopped after initialization.

## Run

Five-minute run with 12 workers and a fixed 12-connection pool:

```bash
./ledger_bench \
  -schema=ledger_bench -duration=5m \
  -threads=12 -pool-min-conns=12 -pool-max-conns=12
```

48-hour run with the same concurrency:

```bash
./ledger_bench \
  -schema=ledger_bench -duration=48h \
  -threads=12 -pool-min-conns=12 -pool-max-conns=12
```

The default two non-transactional reader goroutines share the same pool. If you change the seed account counts, pass those same counts when running. A run without `-init` uses the existing data. Press Ctrl+C to stop the process.

One `ANALYZE` pass over the workload tables is scheduled one minute after each run starts. At startup, the workload attempts these session settings and their database-level equivalents:

```sql
SET yb_max_saop_merge_streams=64;
SET yb_enable_derived_saops=true;
SET yb_enable_derived_equalities=true;
SET yb_enable_cbo=on;
SET enable_nestloop=off;
```

Unsupported or unauthorized settings are reported and skipped. Database-level settings require sufficient privileges; session settings executed through the pool are not guaranteed on every connection when database-level changes are skipped.

## Results

The final summary reports operation and statement throughput, observed read/write mix, latency histograms, conflicts, transient retries, pool resets, and errors. Lock-bearing statements count on the write side. Additional non-transactional reads affect the observed mix; it is not a fixed 1:1 ratio.

These are application counters, not storage-level request counts. Failed or retried work can generate database activity that successful-operation counters do not include.

Workers retry transaction conflicts and transient connection failures with backoff. Pool health checks and resets help replace failed connections. Retries are bounded per operation; workers continue attempting work until the configured duration expires. Startup failures can still terminate the process, and a lost commit response does not provide an exactly-once guarantee.

This is a synthetic workload for performance testing, not a payment processing service. Use `./ledger_bench -h` to inspect all available options.
