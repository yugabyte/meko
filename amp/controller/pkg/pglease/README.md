# `pglease`

A resource lease over one YugabyteDB table, with a renewal loop that kills the
process when the lease can no longer be proven held.

`github.com/yugabyte/amp/controller/pkg/pglease`, pgx/v5 its only direct
dependency. Under `pkg/`, not `internal/`, so it is importable on its own;
nothing in the controller depends on it yet.

It is a **client for migration 0006**, not a second implementation of it. The
rule lives in `amp_registry.token_acquire` / `token_release` / `token_holder`,
which this package calls and the Rust write-fence client
(`compute/libs/utils/src/write_fence.rs`) calls too, so there is one definition
for both to share rather than two that can drift apart.

**It therefore requires migration 0006 to have run**, and there is no table of
its own: leases live in `amp_registry.resource_token`, keyed by the caller's
resource id, alongside every other holder — the compute write fence keys its own
as `branch:<id>`. Pick a key prefix that will not collide.

## The contract

A lease is an **absolute expiry in epoch microseconds, read from the database
clock**. Why that one value carries both "you hold the lease" and "stop writing at
this instant", and why the grant is a fact about a write rather than a conclusion
from a read, is in `compute-fencing.md` and migration 0006 — the definitions, not
a retelling of them.

The rule:

| Caller | May write when |
|---|---|
| the current holder (renewal) | always — **no time condition at all**, even if the lease lapsed long ago, provided nobody took over |
| a different owner (takeover) | `stored_expiry + skew <= now` |
| the holder, releasing | always — `Release` is owner-scoped and skips the grace, so the next holder is granted immediately |

`Release` is the only `DELETE`, so a key whose holder died keeps its row: it
fences nobody once its lease has lapsed, but nothing reclaims it either. Keep the
key space bounded, or add a sweeper of your own.

`Holder` reports a row for as long as takeover is blocked, so "no row" means
*free to a new owner* and cannot disagree with what a subsequent `Acquire` would
decide. A denial's `Holder` and `RetryAfter` are advisory — they come from the
statement's snapshot, which a takeover committed after it can already have
superseded.

## Units and bounds

Values on the wire are microseconds; arguments and results are `time.Duration`.

| Bound | Value | Why |
|---|---|---|
| `MinTTL` | 1ms | durations reach the database in whole milliseconds; less would truncate to an already-expired lease |
| `MaxTTL` | 300s | a lease nobody renews strands the key for its whole TTL |
| `MaxSkew` | 3s | grace against clock disagreement, not a scheduling knob |
| `DefaultSkew` | 500ms | matches YugabyteDB's `max_clock_skew_usec` default, so the lease inherits a bound the database already enforces |

`StoreConfig.Skew` zero means `DefaultSkew`, not "no grace": running without one
asserts every node's clock agrees, so it takes an explicit `DisableSkew`. It gates
takeover and probe visibility only — never a renewal, never a release.

`Config`'s defaults are `TTL 15s / RenewEvery 5s / RenewTimeout 2s /
SafetyMargin 2s`. `Validate` requires `RenewEvery < TTL - SafetyMargin` strictly
and `SafetyMargin >= RenewTimeout`.

## Fail-fast

`Held` runs one renewal goroutine, which ends the lease the moment ownership
stops being provable. Three ways, all readable through `Held.Err()`:

| Cause | Means |
|---|---|
| `ErrLost` | a renewal was refused: somebody else demonstrably holds the row |
| `ErrLeaseGap` | a renewal was *granted*, but stamped at or after the expiry already held — the lease had lapsed, so ownership was not continuous |
| `ErrRenewalDeadline` | nothing was confirmed by `TTL - SafetyMargin`, less the last grant's round trip |

A failed *call* is none of these: the deadline absorbs it. `Held.Context()` is
cancelled with the cause and then `Config.Abort` runs; the default,
`AbortProcess`, dumps the cause and the goroutines to stderr and `os.Exit`s with
`AbortExitCode` (66, the same code as the compute fence). `Config.Abort` is the
test seam, and `nil` means the real thing so forgetting it is safe.

One `Held` per key per `Client`: a second `Acquire` of a held key returns
`ErrAlreadyHeld`, because the database cannot tell it from a renewal and would
hand back a second lease over the one row.

## Running the tests

```bash
go test ./... -race                       # no database needed
AMP_TEST_DATABASE_URL=postgres://... go test ./... -race
```

Live tests are named `TestPg…` and skip when `AMP_TEST_DATABASE_URL` is unset.
`TestMain` provisions ONE migrated database for the whole package (`DROP
DATABASE` costs ~70s, so one per test would dominate the run) and tests isolate
from each other by using a resource key nothing else uses. Mutual exclusion,
takeover timing, and the death of a stalled or killed holder are only covered
with a database: without one, `go test` proves the timings and the argument
checks and nothing about the contract.
