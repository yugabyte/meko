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

**That client does not exist yet** — nothing in this stack shifts the value, and
nothing needs to: the daemons compare the token against their own wall clock,
where micros are the natural unit. The encoding lands with the thin-client
change that introduces `ignore_after_hybrid_time`. Keeping it in one place, on
the client, is what stops two implementations drifting apart; sending raw micros
to the server would be read as a 1970 timestamp and reject every write —
fail-closed, but broken.

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
| **TTL** | 10 s |
| **renewal** | every 8 s, leaving 2 s of slack before expiry |

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
        every 8s
supervisor ──── token_acquire(branch:<id>, <instance>, ttl=10s) ──> YSQL
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
byte passes through on its way into `amp_storage.wals`, on both the
thin-client and YSQL lanes. If the token has expired against the local
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
  nothing; a real partition costs at most the 2 s of slack.

On loss the supervisor SIGTERMs every child immediately — safekeeper,
pageserver, `compute_ctl` and its Postgres — then goes
back to blocking on `token_acquire`. When it wins the token again it
re-runs the whole bring-up. It never exits on token loss; a pod that
cannot get the token is a pod waiting its turn, not a failed pod.

On a clean shutdown (SIGTERM) the supervisor calls `token_release`, so a
replacement starts immediately instead of waiting out the TTL.

### The timing budget

```
t+0s    token granted, expires at t+10s
t+8s    renewal -> new token, expires at t+18s
        (if this fails, retry at t+9s, t+10s, ...)
t+10s   old token expires:
          - safekeeper stops writing WAL and exits
          - another client may take over at t+10.5s (10s + 500ms skew)
```

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

1. **The token is not yet a fence at the Raft layer.** Nothing on the YB write
   path is checked against it server-side, so the fence is cooperative: it stops
   a *well-behaved* writer that notices its lease lapsed, not a wedged one that
   resumes mid-write after a long stall. Closing it needs a tserver-side check
   before the op reaches the preparer — tracked upstream in
   [yugabyte-db#33175](https://github.com/yugabyte/yugabyte-db/issues/33175).
   The precedent exists but only for object locks: `ignore_after_hybrid_time` on
   `AcquireObjectLockRequestPB` (`src/yb/tserver/tserver.proto`), enforced in
   `TSLocalLockManager::CheckRequestForDeadline`
   (`src/yb/tserver/ts_local_lock_manager.cc`). There is no equivalent field on
   `PgPerformOptionsPB` / `PgPerformRequestPB` (`src/yb/tserver/pg_client.proto`)
   or `PgsqlWriteRequestPB` (`src/yb/common/pgsql_protocol.proto`).

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
