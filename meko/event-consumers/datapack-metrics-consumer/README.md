# datapack-metrics-consumer

One of the [event consumers](../README.md). Reads the durable event log, `meko_system.events`, and applies each event's effects. It replaces
bookkeeping that tools such as `context_search` used to do inline (usage counters, the search log),
so a request inserts one row and returns, and this process does the rest a few seconds later.

| Piece | Where |
|---|---|
| The tables (`events`, `event_consumers`, `event_dead_letters`, `event_applied`) | `meko/api_server/internal/db/migrations/system/000029` and `000030` |
| The producer, `emit()` | `src/events/` in `yugabyte/meko-mcp-server` |
| This consumer | here |

It is its own Go module and image, not part of the MCP server. The MCP server image is about 20 GB
(torch, the CUDA wheels, spaCy, mem0); this one is a single static binary (about 17 MB) on Alpine. The
module is independent of `api_server`: the few values the two share (16 shards, the event types) are
restated in `internal/contract`.

## Run

```bash
YUGABYTEDB_SYSTEM_URL="host=localhost port=5433 dbname=yugabyte user=yugabyte password=yugabyte" go run .
```

The database is `YUGABYTEDB_SYSTEM_URL`, else `YUGABYTEDB_URL`, as for the MCP server. The consumer uses
one connection. `/healthz` and `/metrics` are on `:8080`. `LOG_LEVEL` sets the log level.

**Handlers are separate changes.** The registry is empty until the handler tickets (MEKO-870 and
later) add theirs. An event whose type has no handler is **not skipped**: the shard waits (counted in
`meko_events_consumer_errors_total{kind="no_handler"}`, and the lag gauge climbs), so a consumer that is
older than its producer, such as a rollback or a pod not yet replaced, loses nothing and applies the
events as soon as a build with the handler runs. One such event holds every event behind it on that
shard. To give up on one deliberately, insert it into `event_dead_letters` (with its `event_created_at` and `event_schema_version`) and its id into
`event_applied`, and advance the shard's offset past it, in one transaction.

## How it works

Per shard (there are 16; one datapack's events always share a shard):

- **Lease.** An instance takes a shard's lease (`event_consumers.lease_owner`, 30 s, renewed every
  10 s, by the database's clock). A lease that is not renewed expires and another instance takes the
  shard (`meko_events_consumer_lease_takeovers_total`).
- **Read.** The next batch after the saved offset, oldest first, leaving out the newest 3 seconds so a
  row that commits a little late is not passed.
- **Apply and advance in one transaction.** The offset moves with a compare-and-swap on the old
  value. If another instance moved it first the whole batch rolls back
  (`..._offset_conflicts_total`). That, not the lease, is what keeps a batch's effects from applying
  twice.
- **Late commits (the look-back).** `created_at` is stamped when the producer's INSERT runs, but the row
  is visible only when its transaction commits. A commit that lands after the offset has passed its
  `created_at` would never be read again. So every event the consumer handles (applied, skipped or
  dead-lettered) is also recorded in `event_applied`, in the same transaction as its effects, and each
  shard is re-read every 30 s over the 5 minutes below its offset: any event there that is not recorded
  is applied, exactly once even if two instances find it together, because each one first claims it
  with an insert that the other conflicts with. The look-back never moves the offset
  (`..._late_events_total` counts what it applied; any increase means commits are stalling). The
  guarantee is "a commit lands within the window", not "within the lag"; a commit later than the window
  is still lost, but no longer silently: records are kept for **two** windows, and the look-back counts
  the events it finds in the second one with no record (`meko_events_consumer_unrecovered_events{shard}`).
  The janitor trims `event_applied` behind each shard's own offset minus two windows, not behind the
  clock, so a shard that is behind keeps the records it needs. The look-back renews the lease first and
  skips a shard it no longer holds, and an event of a type this build has no handler for is left alone
  (counted as `no_handler`, retried while it is in the window) instead of holding back the events beside it.
- **Failures.** A batch that fails is retried event by event. An event that fails because of its own
  content (bad data or a constraint violation, SQLSTATE class 22 or 23; a handler error or panic) goes
  to `event_dead_letters` with the error (and the event's own `created_at` and `event_schema_version`, so a replay counts toward the day it happened and keeps its payload version) and the offset moves past it. A failure that says nothing
  about the event (connection loss, a conflict, a timeout, a missing table, a permission error, an
  event type with no handler) is not dead-lettered. That shard is skipped for a while (1 s, doubling to
  30 s; `..._errors_total` by kind) and **every other shard, and the janitor with its lag gauges, keep
  running**. After 20 dead letters in one pass the shard pauses, for a minute the first time and twice as long
  for each consecutive trip, up to an hour (a pass that does not trip resets it). That slows a broken
  handler, it does not stop it: each trip still moves the offset past the events it dead-lettered, so
  the recovery is to fix the handler and replay from the dead letters. `errors_total{kind="poison_paused"}`
  counts the trips.
- **A batch cannot outlive its lease.** Leases are renewed between shards and a batch's open transaction
  holds the only connection, so each batch is cut off after the lease minus one renewal minus a sixth
  of the lease (15 s with the defaults, never under 2 s). A poison batch replayed event by event stops
  at that point with what it has committed and carries on in the next pass.
- **Metrics are counted after the commit**, so a rolled-back batch is not counted.

A handler is `func(ctx, tx *sql.Tx, events []store.Event) error`: all of a batch's events of its type,
in order, and the batch's transaction. It writes through `tx` and must not commit or roll back.

## Settings

All are environment variables with the prefix `MEKO_EVENTS_CONSUMER_`. Unparsable values fall back to
the default with a warning, and out-of-range values are clamped.

| Setting | Default | Meaning |
|---|---|---|
| `BATCH_SIZE` | `500` | Events per batch (1–5000). |
| `SAFETY_LAG_SECONDS` | `3` | Newest events not read yet. |
| `LOOKBACK_WINDOW_SECONDS` | `300` | How far below its offset a shard is re-read for late commits (30–86400); also how long `event_applied` rows are kept. |
| `LOOKBACK_INTERVAL_SECONDS` | `30` | How often each shard is re-read (5–3600). |
| `LEASE_SECONDS`, `RENEW_SECONDS` | `30`, `10` | Renewal is never slower than half the lease. |
| `POLL_MIN_MS`, `POLL_MAX_MS` | `500`, `2000` | Idle sleep, doubling up to the maximum. The maximum is clamped to the renewal interval, because leases are renewed inside a tick and a longer sleep would let an idle holder's leases lapse. |
| `MAX_SHARDS` | `0` | Most shards one instance holds; `0` is no limit. See below. |
| `MAX_DEAD_LETTERS_PER_PASS` | `20` | Dead letters before a shard pauses. |
| `JANITOR_SECONDS` | `60` | How often the shard 0 holder takes measurements. |
| `DEAD_LETTER_DAYS` | `14` | Dead letters older than this are deleted. |
| `HTTP_PORT` | `8080` | `/healthz` and `/metrics`; `0` turns the server off. |
| `CONSUMER`, `SCHEMA` | `analytics`, `meko_system` | Which offsets to use; the schema is for tests. |

**Parallelism.** With no `MAX_SHARDS`, the first instance to start takes all 16 shards and the others
wait as standbys that take over when its lease expires. That is one thread of work, which is enough
while volume is low. A cap spreads shards across instances but also caps failover: with a cap of 8
and two instances, one dead instance leaves 8 shards unread. Shards are not rebalanced yet.

## Operating it

**Database time zone.** The handler counts a search toward the UTC date of its `occurred_at`. The inline
path, the failure recovery in the MCP server and the readers of `datapack_daily_activity` use `CURRENT_DATE`
in the session time zone. The consumer logs the database's time zone at startup and warns if it is not UTC;
if it is not, searches near midnight land on different days in the two paths, so pin it to UTC.

| Metric | Alert |
|---|---|
| `meko_events_consumer_lag_seconds{shard}`, age of the oldest unread event, published by the shard 0 holder (zero on every other instance, and reset when shard 0 is lost) | above 60 for 5 minutes |
| `meko_events_consumer_dead_letters_total` | any increase |
| `meko_events_consumer_late_events_total` | any increase: commits are stalling |
| `meko_events_consumer_unrecovered_events{shard}` | above 0 at any point in an hour (max over time): events committed later than the look-back window and were lost |
| `meko_events_partition_runway_months` | below 3 |
| `meko_events_default_rows` | any row |
| `meko_events_consumer_lease_takeovers_total` | sustained increase |
| `meko_events_consumer_errors_total{kind}` | sustained increase |
| no `/metrics` from any instance | the lag gauge is silent if every instance is down |

The shard 0 holder also publishes the partition gauges. It **never runs DDL and never deletes
events**: partitions are created and dropped by hand (runbook in MEKO-873; automation is MEKO-883),
and the runway and default-partition gauges are what warn that someone forgot. Its only delete is
dead letters older than `DEAD_LETTER_DAYS`.

SIGTERM finishes the batch in progress, releases the leases and exits, so the Deployment's
`terminationGracePeriodSeconds` must exceed one batch.

## Tests

```bash
go test ./...                  # unit tests; the DB-backed ones are skipped
EVENTS_TEST_DSN="postgresql://postgres@127.0.0.1:5432/postgres?sslmode=disable" go test -race ./...
```

The DB tests need a PostgreSQL-compatible server where they may create and drop a schema. Each test
gets its own schema with the same tables as migration 000029 (without the YugabyteDB-only `ASC` in
the key), and they cover: effects applied once across a crash, a competing offset advance rolling the
batch back, lease expiry and takeover, poison versus environment errors, a handler panic, the safety
lag, a commit that stalls past the offset being applied by the look-back (and only once, also with two
instances racing), handled events never coming back, `event_applied` trimming behind the shard's own
offset, shutdown releasing leases, and the janitor's measurements. CI runs them against a PostgreSQL
service container.

**Do not clear `event_applied` or move an offset back by hand.** The look-back treats an event below the
offset with no record as late and applies it again, so emptying the table, or rewinding an offset
without clearing the events' records, double-counts. To replay, delete the records for the replayed
range in the same statement batch that moves the offset.

## Not yet verified on YugabyteDB

- The row comparison `(created_at, id) > (…)` is used as an index range bound on the primary key of
  the partitioned table (`store.ReadBatch` has the fallback form).
- The look-back statements: the `<=` row comparison and the `id = ANY(...)` read over a created_at range,
  the primary-key range delete on `event_applied`, and `INSERT ... ON CONFLICT DO NOTHING RETURNING` as
  the claim when two instances insert the same key (YugabyteDB may report a conflict, which is retried).
- `pg_get_expr(relpartbound)` for the partition runway gauge.
- Behaviour of the lease and compare-and-swap statements under YugabyteDB's transaction conflicts
  (a conflict is treated as transient and retried).
- That the DSN needs no extra TLS options (`sslrootcert`) that the MCP server adds from its own settings.
