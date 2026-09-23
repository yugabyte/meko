# Compute write fencing

Two instances of the same endpoint must never write the same branch at
the same time. That happens more easily than it sounds: Kubernetes
reschedules a pod while the old one is still alive but unreachable, a
node partitions and comes back, an operator starts a second container by
hand. Nothing about "the old pod is gone" is ever observable from the new
one — only the *absence* of evidence that it is still there.

This page describes the fence: a **resource token** issued by YSQL, held by
the endpoint supervisor, and passed down to the per-endpoint services as their
write deadline. It fences the endpoint — every process in it — rather than any
one service's rows.

## The token is a deadline

A **token is a point in time**: the microseconds since the Unix epoch at which
the lease expires. YSQL generates it, so the value comes from the database's
clock rather than the caller's, and the same value serves two purposes at once —

- **a lease**: while your token is the stored one, no other client can
  get one for that resource;
- **a deadline**: the moment it names is the instant after which you must
  stop writing.

That second property is what makes it worth passing downstream. A
service handed a token does not need to talk to the token store, or to
the supervisor, or to anything else: it holds a number, it can read its
own clock, and it knows whether it is still allowed to write.

### Getting the time from YSQL

```sql
SELECT (EXTRACT(EPOCH FROM clock_timestamp()) * 1000000)::bigint;  -- e.g. 1786161733463940
```

- `clock_timestamp()` bottoms out in the same `gettimeofday` that YB's own wall
  clock reads (`GetCurrentTimeMicros`, `src/yb/gutil/walltime.h`), so the value
  is directly comparable to hybrid times cluster-wide once encoded (below).
  `EXTRACT` returns `numeric` in PG15, so the microsecond conversion is exact
  rather than routed through a float.
- It is **wall clock**, not monotonic — an NTP step moves it. What bounds the
  disagreement between two nodes is the skew grace, below.
- Epoch micros are ~1.79e15 today against `i64::MAX` = 9.22e18, so a signed
  `bigint` is never in danger — and the value stays directly readable, via
  `to_timestamp(token / 1000000.0)` in SQL or `WriteToken`'s `token_deadline`
  in Rust.

**Use `clock_timestamp()`, not `now()` / `current_timestamp`.** Those return
*transaction start* time. `statement_timestamp()` is safe, but
`clock_timestamp()` is the true analogue.

### Encoding for the wire

The token is stored and compared in micros. It is HybridTime-encoded in **the
thin client**, at the single point where it becomes `ignore_after_hybrid_time`
(which the server compares against `clock_->Now()`):

```
HybridTime = epoch_micros << 12        (logical bits are 0)
```
`src/yb/common/hybrid_time.h`

```rust
const HT_LOGICAL_BITS: u32 = 12;
let ignore_after_ht: u64 = token << HT_LOGICAL_BITS;
```

The field now exists: `ybthin_upsert_row.ignore_after_hybrid_time`, added by the
2026.1.2 shim. **No caller sets it yet** — `TsClient::upsert` sends `0`, which
means "no fence" — so the encoding above is still unwritten, and the daemons go
on comparing the token against their own wall clock, where micros are the
natural unit. Keeping the shift in one place, on the client, is what stops two
implementations drifting apart; sending raw micros to the server would be read
as a 1970 timestamp and reject every write — fail-closed, but broken.

## Schema

The token store lives in `amp_registry` — it is control-plane state, not
data-plane state — and is defined by
[`controller/migrations/0006_resource_token.up.sql`](../../controller/migrations/0006_resource_token.up.sql),
which is the source of truth: the `resource_token` table, `token_acquire` /
`token_release` / `token_holder`, and the epoch-micros helpers.

The whole mutual-exclusion guarantee is the single
`INSERT … ON CONFLICT DO UPDATE … WHERE … RETURNING` inside `token_acquire`.
`RETURNING` yields a row only when that `WHERE` fired, which is what makes
"granted" a fact about the write rather than a conclusion drawn from a prior
read. **Do not refactor it into a separate `SELECT` and `UPDATE`** — there is
no read-then-write window today, and adding one silently removes the guarantee
this whole page is about.

### The skew grace

`p_skew_ms` (default 500 ms) exists because the token comes from a PG
node's wall clock, and two PG nodes can disagree. 500 ms matches
YugabyteDB's own `max_clock_skew_usec` default
(`src/yb/util/physical_time.cc`), and that bound is *fail-stop* for
transactional tables (`src/yb/tablet/tablet.cc` enables the `LOG(FATAL)` in
`src/yb/server/hybrid_clock.cc`). The lease inherits a bound the database
already enforces rather than adding a new assumption of its own.

It gates only **takeover by a new client**. The current holder renews
with no grace at all, because a renewal cannot violate mutual exclusion
no matter whose clock is right. So the cost of the grace is paid once, by
the replacement, at the moment it takes over — never by the steady state.

`token_release` bypasses it on purpose: an explicit handoff by the owner
needs no clock agreement.

### Semantics

| Situation | Result |
| --- | --- |
| No row for the resource | granted |
| Stored token + skew grace has passed | granted to any client |
| Token live, requester **is** the holder | granted (renew), no grace wait |
| Token live, requester is **not** the holder | denied, with holder + `retry_after_ms` |
| Explicitly released by the holder | granted immediately, grace bypassed |

## How yb-amp uses it

### Who holds a token, and for what

| | |
| --- | --- |
| **resource_id** | `branch:<branch_id>` — the branch is the thing at most one writer may write |
| **client_id** | one **instance** of the endpoint supervisor, not the endpoint: `<hostname>-<pid>-<nonce>-<branch>` (override with `WRITE_TOKEN_CLIENT_ID`) |
| **TTL** | 15 s |
| **renewal** | every 5 s, leaving 10 s of slack before expiry |

The client id being per-*instance* rather than per-endpoint is
load-bearing. `token_acquire` grants unconditionally to the current
holder — that is what makes renewal cheap — so if two supervisors for the
same endpoint shared a client id, both would be granted and there would be
no fence at all. A fresh id per process costs one takeover wait
(≤ TTL + skew) when a pod is genuinely replaced, and buys the property the
whole mechanism exists for.

### Propagation

The supervisor
([`compute/endpoint_supervisor`](../endpoint_supervisor/src/fence.rs))
acquires the token **before it starts anything**, and pushes it to each
service over that service's existing HTTP control surface:

```
POST /v1/write_token   {"token": 7316005604050857984}
 ->  200 {"token": 7316005604050857984, "expires_in_ms": 9873}
```

on the safekeeper ([`safekeeper-apis.md`](safekeeper-apis.md)) and the
pageserver ([`pageserver-apis.md`](pageserver-apis.md)). Every successful
renewal pushes again.

```
        every 5s
supervisor ──── token_acquire(branch:<id>, <instance>, ttl=15s) ──> YSQL
     │                                                          (amp_registry)
     ├── POST /v1/write_token ──> safekeeper   (enforces)
     └── POST /v1/write_token ──> pageserver   (enforces)
```

### Enforcement

Both daemons keep the token in a process-global
[`utils::write_fence`](../libs/utils/src/write_fence.rs) — one branch per
process, so a global is the honest shape. `require_write_token = true` in
the service's config arms it; left false, every check is a no-op. Today only
the single-container endpoint sets it, because it is the only launcher with a
process resident to hold the lease
([#741](https://github.com/yugabyte/meko/issues/741)).

**An armed service with no token is fenced out, not unfenced.** The fence
starts holding the zero token, which reads as already expired. A
safekeeper that never hears from its supervisor therefore kills itself at
its first write rather than treating silence as permission — the failure
mode of a supervisor that died, or of a service that came up under a
supervisor that has already lost the lease, is a dead service, not a
second writer.

**Safekeeper.** The check sits in `flush_batch`
(`compute/safekeeper/src/yb_wal_storage.rs`) — the single point every WAL
byte passes through on its way into `amp_storage.wals`. If the token has
expired against the local
wall clock, the safekeeper does not write and does not return an error to
its caller: it logs and **exits the process**.

Exiting is the point. An error would propagate back to the walproposer,
which would reconnect and retry — against the same stranded safekeeper.
The fence has to remove the writer, not the write. Whatever WAL was
buffered is dropped along with the process, which is correct: it was
never acknowledged as durable, and by definition some other endpoint now
owns the branch.

**Pageserver.** Same rule at the same kind of choke point: the token is
checked before the page/relation writes go to YB, and an expired token
exits the process.

### Losing the token

The supervisor treats loss as a whole-endpoint event, because it is:
whatever it is that made another instance eligible to take over means
this instance is no longer the endpoint.

- **Denied** (`granted = false`): another client holds it. Immediate
  loss.
- **Renewal failing** (YB unreachable, connection error): retried every
  second until the token's own deadline passes. Transient blips cost
  nothing; a real partition costs at most the 10 s of slack.

On loss the supervisor SIGTERMs every child immediately — safekeeper,
pageserver, `compute_ctl` and its Postgres — then goes
back to blocking on `token_acquire`. When it wins the token again it
re-runs the whole bring-up. It never exits on token loss; a pod that
cannot get the token is a pod waiting its turn, not a failed pod.

On a clean shutdown (SIGTERM) the supervisor calls `token_release`, so a
replacement starts immediately instead of waiting out the TTL.

### The timing budget

The renewal interval is a third of the TTL, so **two** renewals can fail
outright before the lease is spent.

The renewal path shares the endpoint with the workload it fences, so a renewal
is likeliest to be late exactly when the endpoint is busiest — which is when
stopping the safekeeper costs most. The margin is sized for that moment, not for
a healthy round trip.

The slack is also what a YugabyteDB node dying is spent on: the call fails, the
connection is replaced, and the smart driver's next connect lands somewhere
alive. See "A node that stops answering" below for what makes the failure fast
enough to fit.

```
t+0s    token granted, expires at t+15s
t+5s    renewal -> new token, expires at t+20s
        (if this fails, retry at t+6s, t+7s, ...)
t+10s   a second renewal may fail outright and there is still 5s left
t+15s   old token expires:
          - safekeeper stops writing WAL and exits
          - another client may take over at t+15.5s (15s + 500ms skew)
```

### Retuning a branch's lease

`WRITE_TOKEN_TTL_MS` and `WRITE_TOKEN_RENEW_SECONDS` are the boot defaults, and
they arrive as env vars from the pod's ConfigMap — which a running process never
sees change. Editing the chart value changes the next pod, not this one.

The live source is the branch:

```sql
UPDATE amp_registry.branches
   SET write_token_settings = '{"ttl_ms": 20000, "renew_seconds": 8}'::jsonb
 WHERE branch_id = $1;
```

**On the branch, not the endpoint.** The lease is one row per branch
(`branch:<branch_id>`), and every endpoint on the branch contends for it, so two
endpoints disagreeing about its TTL would not be an override — just whichever of
them renewed last. It sits beside `pg_settings` and `extensions`, the branch's
other interpreted properties, and like them a fork inherits it.

The supervisor applies it at startup, from the same registry read that fetches
the daemon settings — and only at startup. **A change takes effect when the
endpoint next restarts**, exactly like `pageserver_settings`, `safekeeper_settings`
and `pg_settings`, which are also read once and rendered into the config a daemon
starts from. Nothing polls: a fleet re-reading a table every minute to notice a
change nobody made is a cost paid continuously for an event that is rare.

The pair is validated as a pair and refused as a pair — an interval at or past
the TTL is the endpoint fencing itself out on a timer, and naming only one of the
two would move the slack under a value nobody set. A refused row leaves the
environment's timings in place and logs why; an endpoint that cannot read the
registry keeps them too, because failing to read an override must never be worse
than not having one.

The control socket reports what is in force, and only reports:

```
$ echo lease | socat - UNIX-CONNECT:$RUN_DIR/supervisor.sock
ttl_ms=15000 renew_ms=5000
```

Deliberately read-only. An endpoint whose live lease disagreed with its branch
row would be very hard to explain to whoever changed the row.

### A node that stops answering

The lease connection is bounded more tightly than the client's defaults,
because a renewal has a deadline nothing else on this path has — miss enough of
them and the endpoint stops writing. Three bounds, for three different
failures:

| | |
| --- | --- |
| `statement_timeout` | **2 s.** A live but slow YB. `token_acquire` is one indexed upsert, so failing fast and retrying inside the TTL beats one long wait that spends it. |
| `TCP_USER_TIMEOUT` | **3 s.** A YB that is not there at all. |
| `connect_timeout` | **3 s.** The reconnect that follows. |

The middle one is the one worth explaining. A tserver whose node dies without
closing its sockets sends no FIN and no RST. Nothing at the protocol level
notices: `is_closed()` is false, the statement bound never fires because
nothing is left to fire it, and the client waits on data that will never
arrive. Keepalives would eventually catch it, but the driver leaves the idle at
**two hours**. Without `TCP_USER_TIMEOUT` every renewal until the lease ran out
would go to the same dead socket, and the endpoint would fence itself out over
one node in a healthy cluster.

With it, the kernel fails the socket once data has gone unacknowledged for 3 s.
That surfaces as a connection error, so the retry loop reconnects and the next
connect can land on a live node — a dead node costs a retry rather than the
endpoint.

Only the detection is spent in that case. `run_with_retries` reconnects inside
the same call, and both that connect and the retried statement go to a live
node, so they take milliseconds rather than their ceilings. Adding the three
numbers up describes a different failure — the replacement being sick too,
i.e. a cluster-wide outage, where the endpoint is going down whatever these
bounds say. What has to fit in the 10 s of slack is the 3 s of detection.

The supervisor also wraps each call in a timeout of its own, bounded by what is
left on the lease. That is a backstop, not a substitute: it is one timeout
around the whole call, so it cannot tell a slow answer from a dead socket, and
it leaves the connection to be reused either way. What it guarantees is that a
renewal always *returns*, so the lease-loss path is reachable.

Two clocks are compared and neither is the other's: the token comes from
the PG backend's `gettimeofday`, and the safekeeper checks it against its
own. That is deliberate — a monotonic-clock lease cannot be transferred
between processes at all — and it is why the takeover side, and only the
takeover side, waits out the skew grace. Under a skew larger than
`p_skew_ms` the fence degrades to the same window the rest of YugabyteDB
degrades to.

## Client integration notes

**Handle SQLSTATE `40001`.** Under contention a loser occasionally gets a
transaction conflict instead of a clean `granted = false`:

```
ERROR:  could not serialize access due to concurrent update
```

Retry on `40001`; the retry then returns a normal denial carrying
`retry_after_ms`.

Rust callers get this for free — `YsqlClient` already classifies `40001`
as transient and retries with backoff — which is why
[`utils::write_fence::TokenStore`](../libs/utils/src/write_fence.rs)
has no retry loop of its own and its callers see a grant or a denial,
never the conflict. A client speaking raw SQL (the supervisor, over
`psql`) has to do it itself:

```rust
loop {
    match store.acquire(&resource, &client_id, ttl).await {
        Ok(g) if g.granted => break g.token,
        Ok(g) => sleep(g.retry_after).await,
        Err(e) => return Err(e),
    }
}
```

**Replay is safe.** `YsqlClient` can also replay a statement whose commit
status is unknown, which every YB write in this repo has to tolerate.
`token_acquire` tolerates it by construction: a replayed grant for the
same client is a renewal, which the holder is always allowed.

**`retry_after_ms` is rounded up, and never zero.** The token is micros and
this is milliseconds, so the conversion truncates; rounding *down* would tell
the caller to wait fractionally less than the grace has left and it would come
straight back for another denial. It is also floored at 1 ms rather than 0,
because the denial path re-reads the row and races the lease it is reporting on
— an expiry between the rejected upsert and that read leaves a non-positive
remainder, and a caller with no wait of its own would spin.

The durations are milliseconds while the token is microseconds, and that split
is deliberate: the token is an *instant* compared against a clock, where
rounding is a correctness question, whereas these are a sleep target and two
observability fields, where nothing below a millisecond is actionable — you
cannot sleep more precisely than the scheduler, and the whole premise of the
500 ms grace is that clocks disagree by more than that. They also pair with the
`p_ttl_ms` / `p_skew_ms` inputs, which are human-set knobs. Rust callers never
see either unit: `TokenGrant` and `TokenLease` convert both to `Duration` at the
boundary.

`retry_after_ms` is computed from the same `now_us()` reading as the denial, so
it is consistent with the answer rather than with a later clock read.

## Known gaps / follow-ups

1. **The fence is available on the write path, and amp does not ask for it.**
   The server side landed for the thin client: `ignore_after_hybrid_time` is a
   per-row field on `ybthin_upsert_row`, and per the shim's ABI header the leader
   rejects the write with `YBTHIN_FENCED` when that hybrid time has already passed
   by the time the op is assigned its own — judged per tablet, so a batch
   straddling the boundary can be partly applied. What is missing is on our side:
   `TsClient::upsert` hardcodes `0`, so no write carries a fence and the token
   stays cooperative — it stops a *well-behaved* writer that notices its lease
   lapsed, not a wedged one that resumes mid-write after a long stall. Closing it
   means threading the lease token through the page and WAL writers and encoding
   it as above. The general YSQL path is still uncovered
   ([yugabyte-db#33175](https://github.com/yugabyte/yugabyte-db/issues/33175) is
   open): no such field on `PgPerformOptionsPB` / `PgPerformRequestPB`
   (`src/yb/tserver/pg_client.proto`) or `PgsqlWriteRequestPB`
   (`src/yb/common/pgsql_protocol.proto`) — but amp's only write transport is the
   thin client, so that does not block us.

   Two things that look like they could stand in for it, and cannot:

   - **An RPC deadline cannot substitute.** Once a write is handed to Raft
     nothing cancels it — `ReplicateBatch` takes no deadline
     (`src/yb/consensus/consensus.h`). The round commits whenever a majority
     acks, and the response is dropped if the inbound call already timed out. A
     timeout-based lease would be actively misleading.
   - **The thin-client ABI has no per-call deadline.** Only a client-wide
     `rpc_timeout_ms` at construction (`src/yb/thin_client/yb_thin_client.h`,
     default 60 s), stamped on every controller. Worth adding additive per-call
     variants; note a dropped session is reopened lazily and synchronously
     before dispatch (`DoHeartbeat` in `yb_thin_client.cc`, under the session
     mutex), so one logical call can burn 2x the timeout.
