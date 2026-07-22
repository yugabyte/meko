# Safekeeper LSNs

Authoritative inventory of every LSN the safekeeper tracks: name, where it
lives, what advances it, what reads it, and the invariants between them.
Grounded in the Rust sources (`compute/safekeeper/`, `compute/libs/utils`,
`compute/libs/safekeeper_api`), the walproposer C sources
(`compute/pgxn/yb_amp/`), and the controller schema
(`controller/migrations/0002_compute_storage.up.sql`).

**System framing.** Durability and resilience come from YugabyteDB, the
underlying store. The safekeeper mirrors every WAL byte into the YB table
`safekeeper.wals` and persists its per-branch control state in
`safekeeper.timeline_state`; it keeps no durable local state. Throughout
this doc, "flushed" means **persisted to YB** — `flush_lsn` is the frontier
of WAL confirmed durable in `safekeeper.wals`, and a commit is acked to the
compute only once its bytes are there.

See [`pageserver-lsns.md`](pageserver-lsns.md) for the pageserver side of
the replication link (how the `START_REPLICATION` position is chosen, what
`PageserverFeedback` reports back).

## Flow overview

```mermaid
flowchart LR
    COMP[Compute / walproposer] -->|AppendRequest| SK_ING[WAL acceptor ingest]
    SK_ING -->|write_wal producer| CHAN[chunk channel]
    CHAN -->|consumer batches| YB_WAL[(safekeeper.wals)]
    YB_WAL -->|DurableState watch| DUR[durable_lsn]
    DUR -->|caps| FL["flush_lsn = min(flush_record_lsn, durable_lsn)"]
    FL -->|AppendResponse.flush_lsn| COMP
    FL -->|commit_lsn watch| WS[walsender]
    PS[Pageserver] -->|START_REPLICATION start_pos| WS
    WS -->|InterpretedWalRecords| PS
    PS -->|PageserverFeedback ps_applylsn| SK_FB[update_remote_consistent_lsn]
    SK_FB -->|inmem, then advisory persist| RCS[remote_consistent_lsn]
    RCS -->|calc_horizon_lsn| GC[remove_up_to WAL GC]
```

---

## Commit semantics: the commit frontier IS the flush frontier

One safekeeper serves each branch, and its WAL is durable in YugabyteDB the
moment the INSERT commits — so there is a single acknowledgement frontier.
`commit_lsn` everywhere in the safekeeper is defined as `flush_lsn`:

```rust
// timeline.rs — WriteGuardSharedState::drop
let commit_lsn = self.guard.sk.flush_lsn();
let _ = self.tli.commit_lsn_watch_tx.send_if_modified(...);
```

Every release of the shared-state write guard re-derives `commit_lsn` from
`flush_lsn` and broadcasts it on `commit_lsn_watch_tx`; walsenders and the
manager subscribe to that watch. HTTP status and metrics report
`commit_lsn = flush_lsn`. The compute's walproposer applies the same rule:
`GetAcknowledgedWALPosition` returns the safekeeper's reported
`flushLsn` (once it has reached `propStartLsn`), and that becomes
`wp->commitLsn` — the position at which synchronous-commit waiters are
released. There is no LSN at which WAL is "flushed but not committed";
if `flush_lsn` covers a record, YB durably holds it.

`AppendRequestHeader` carries exactly `begin_lsn` and `end_lsn` — the WAL
byte range being appended. No commit or truncate positions travel on the
append path; truncation happens only via `ProposerElected`.

---

## Timeline control LSNs (`safekeeper.timeline_state`)

Persisted in YB via `control_file::YbControlFileStorage`. Loaded at
timeline attach; written by `TimelineState::finish_change` (full row) and
the advisory single-column path (below). The current column list is
`COLUMNS` in `compute/safekeeper/src/control_file.rs`:

| Column | Meaning |
|--------|---------|
| `timeline_start_lsn` | Earliest LSN the timeline conceptually covers (branch/bootstrap point). Set on the first `ProposerElected` if still zero. |
| `local_start_lsn` | Earliest LSN this safekeeper has contiguous WAL from. Set to `start_streaming_at` on the first election. Floors the restart record-boundary back-walk (below). |
| `remote_consistent_lsn` | Oldest checkpoint the pageserver has durably ingested into YB page storage — WAL at or above this must be retained. Fed by `PageserverFeedback.remote_consistent_lsn`. Seeded to `max(current, timeline_start_lsn)` at election so a fresh timeline doesn't pin the horizon at 0. |
| `pg_version`, `system_id`, `wal_seg_size` | Server info, set at creation; immutable. |
| `creation_ts` | Row insert time. |

**In-memory overlay** (`TimelineMemState`): a single field,
`remote_consistent_lsn`, updated immediately on pageserver feedback
(`Timeline::update_remote_consistent_lsn`, monotonic max). The persisted
copy lags until the manager's periodic advisory save.
`has_unflushed_inmem_state()` is exactly
`inmem.remote_consistent_lsn > pers.remote_consistent_lsn`.

**Advisory persistence path:** the manager
(`timeline_manager.rs::update_control_file_save`) spawns a background task
— holding no shared-state lock — at most once per
`control_file_save_interval` (default 300 s,
`SafekeeperConfigToml::default`). The task runs
`AdvisoryPersist::persist_remote_consistent_lsn`:

```sql
UPDATE safekeeper.timeline_state
SET remote_consistent_lsn = GREATEST(remote_consistent_lsn, $2)
WHERE branch_id = $1
```

The `GREATEST` makes the write monotonic, so a late-landing advisory UPDATE
can never regress a concurrent full-row persist. Retries are gated on both
the persisted clock and a last-*attempt* clock, so a failing YB never
causes a per-tick retry storm. Why the lock-free design matters: this loop
is the sole servicer of walsender residence-guard requests, and WAL appends
need the shared-state write lock — an in-loop UPDATE under lock would
freeze commits whenever the `timeline_state` UPDATE stalls.

**Consensus/membership columns do not exist** in this schema: one
safekeeper serves each branch and YugabyteDB provides the durability
quorum, so the control row carries only the three LSNs plus server info.

---

## WAL storage watermarks

### `WalBookkeeping` — in-memory bookkeeping (`wal_storage.rs`)

Pure bookkeeping used by `YbWalStorage`: LSN watermarks plus the
`WalStreamDecoder` that finds record boundaries. No filesystem interaction.

| LSN | Meaning |
|-----|---------|
| `write_lsn` | End of the byte stream accepted so far (may be mid-record). |
| `write_record_lsn` | End of the last **fully parsed** WAL record. |
| `flush_lsn` | Byte-level flush watermark; `write_wal` keeps it aligned with `write_lsn` (there is no per-segment fsync barrier — durability is tracked separately by `durable_lsn`). |
| `flush_record_lsn` | End of the last fully flushed record; what `Storage::flush_lsn()` returns. |

Documented invariant: `write_lsn ≥ write_record_lsn ≥ flush_record_lsn`.
`write_wal` rejects rewrites (`startpos < write_lsn`) and gaps
(`startpos > write_lsn` when nonzero) — sequential append only; moving the
pointer requires a `ProposerElected` truncation first.

### `YbWalStorage` — durability tracking (`yb_wal_storage.rs`)

`write_wal()` is a **producer**: it splits the byte range into per-segment
`PendingChunk`s (each stamped with its `end_lsn`) and pushes them onto a
bounded channel without waiting for YB. A background **consumer** task
drains the channel into batches, coalesces contiguous chunks into single
rows, INSERTs them into `safekeeper.wals`, and publishes the batch's
highest `end_lsn` onto a `watch` channel as
`DurableState::Healthy(lsn)` — or `DurableState::Failed(msg)` on a
non-recoverable error, which fails both durability paths and terminates
the connection instead of freezing the watermark.

| LSN | Where | Meaning |
|-----|-------|---------|
| `durable_lsn` | `Arc<AtomicU64>` mirrored from the watch | Highest `end_lsn` confirmed durable in `safekeeper.wals`. |
| `submitted_max_lsn` | field | Highest `end_lsn` handed to the consumer; the target a blocking flush waits for. |

**The externally reported flush frontier is durability-capped:**

```rust
fn flush_lsn(&self) -> Lsn {
    std::cmp::min(self.inner.flush_lsn(), Lsn(self.durable_lsn.load(...)))
}
```

Nothing is ever advertised past what YB holds. Flush modes (`YbFlush`):

| Mode | Behavior | Used by |
|------|----------|---------|
| `NotifyAndWait` | wake consumer, `wait_for_durable(submitted_max_lsn)`, advance bookkeeping | `flush_wal()` — the synchronous barrier (timeline init, truncation preludes) |
| `Notify` | wake consumer, return immediately | `handle_flush` (`FlushWAL` message) — steady-state; the ack follows via the durable watch |
| `LocalAdvanceOnly` | advance bookkeeping to current `durable_lsn` | `yb_emit_append_response` — the durability-ack path |

**Ack pipeline** (`receive_wal.rs`): the WalAcceptor converts every
`AppendRequest` to `NoFlushAppendRequest` (ingest never blocks on YB), a
`flush_ticker` periodically issues `FlushWAL`, and a dedicated select arm
on `yb_durable_watch()` fires each time the consumer publishes a new
durable LSN — emitting an `AppendResponse` with the advanced,
durability-capped `flush_lsn` so the walproposer releases waiting
synchronous commits. A crash between `write_wal()` returning and the batch
landing in YB is a no-op for the compute: no ack was sent, so no commit
was acknowledged.

`wait_for_durable` is bounded (`YB_DURABLE_WAIT_MAX`, 60 s): callers hold
the timeline write lock, and an unbounded wait would wedge the whole
timeline; timing out drops the connection instead.

**`truncate_wal(end_pos)`** first drains in-flight durability
(`wait_for_durable(submitted_max_lsn)`) so a buffered chunk past `end_pos`
can't land after the DELETE, then **refuses `end_pos > durable_lsn`** —
accepting it would set `flush_record_lsn = end_pos` and advertise WAL the
row store can't return. It deletes YB rows lying entirely past the cut,
drops cached chunks past the cut (so a reader can't be served stale bytes
that a subsequent write rewrites), and resets all four bookkeeping
watermarks to `end_pos`. Rows straddling the cut stay; reads are capped at
the flush frontier so the over-kept tail is never served.

### `SafeKeeper::flush_lsn()` wrapper (`safekeeper.rs`)

```rust
max(self.wal_store.flush_lsn(), self.state.timeline_start_lsn)
```

Avoids reporting zero before the first segment exists on a branch whose
timeline starts at a nonzero LSN.

### Restart rehydration (`YbWalStorage::new`)

1. `load_write_lsn` scans `safekeeper.wals` for
   `max(segno_base + chunk_offset + chunk_size)` across all buckets;
   falls back to `timeline_start_lsn` when the table has no rows. Because
   `write_wal` is gap-free and `truncate_wal` deletes above the cut, the
   durable row set is a contiguous prefix — the max end is the end of WAL.
2. That raw byte offset can point inside a torn record or a torn trailing
   page (#61), so it is refined to a record boundary by replaying the
   decoder: `find_anchor` walks back (floored at `local_start_lsn`), then
   `find_end_of_wal` scans forward. A torn tail truncates `write_lsn`
   *back*; a trailing `XLOG_SWITCH` advances it *forward* to the next
   segment boundary, past padding the compute never streamed. Clamping the
   switch case back to the raw max — or letting the anchor walk overshoot a
   mid-page `local_start_lsn` — wedges the safekeeper (#181); see
   [`safekeeper.md`](safekeeper.md) §"Restart".
3. Rows past the refined `write_lsn` are deleted (best-effort; a failed
   cleanup is absorbed on the next restart), and `durable_lsn` +
   `DurableState::Healthy` are seeded at `write_lsn` so an immediate
   `flush_wal()` returns at once.

---

## Walproposer link (compute ⇄ safekeeper)

| LSN | Carried in | Semantics |
|-----|-----------|-----------|
| `AcceptorGreeting.flush_lsn` | greeting reply | The safekeeper's end-of-WAL. The walproposer stores it as `propStartLsn` and derives its resume point from it; on bootstrap (`flushLsn == 0`) the compute starts from the basebackup redo LSN (`GetRedoStartLsn`). |
| `ProposerElected.start_streaming_at` | election message | Where the walproposer resumes appending; the safekeeper truncates its WAL to exactly this point. **Invariant (asserted in `handle_elected`): `start_streaming_at ≥ flush_lsn()`** — truncating below the durable frontier would discard acknowledged WAL. Because the walproposer derives it from the greeting's `flush_lsn`, the assert holds in steady state; a violation means the two sides disagree about durable history. First election also latches `timeline_start_lsn` / `local_start_lsn` and seeds `remote_consistent_lsn`. |
| `AppendRequestHeader.begin_lsn` / `end_lsn` | each append | Byte range of the WAL chunk. `handle_append_request` enforces gap-free, rewrite-free sequencing against `write_lsn`. |
| `AppendResponse.flush_lsn` | acks | The durability-capped flush frontier. Drives `wp->commitLsn` (syncrep release) and `CheckGracefulShutdown`. |
| `AppendResponse.hs_feedback` | acks | Aggregated hot-standby feedback (xmin/catalog_xmin), injected by `Timeline::process_msg` / `yb_emit_append_response` from `WalSenders::get_hotstandby`. |
| `AppendResponse.pageserver_feedback` | acks (optional) | The latest `PageserverFeedback` — the WalAcceptor clones the last `AppendResponse` and injects each incoming feedback so the compute sees pageserver progress promptly. |

On receiving feedback, `walprop_pg_process_safekeeper_feedback` advances
the compute's replication slot via `ProcessStandbyReply(standby_flush_lsn,
standby_flush_lsn, standby_apply_lsn, ...)` where `standby_flush_lsn =
wp->commitLsn` (durable in the safekeeper ⇒ in YB) and `standby_apply_lsn =
ps_feedback.disk_consistent_lsn` (processed and durable at the pageserver).
Backpressure LSN handling is covered in
[`pageserver-lsns.md`](pageserver-lsns.md) §"Walreceiver LSNs and the
feedback wire format".

---

## Pageserver streaming (outbound)

### `START_REPLICATION PHYSICAL <start_pos>`

The **pageserver chooses `start_pos`** — the safekeeper does not pick a
resume point for it. The pageserver passes its durable `consistent_lsn`
(plus record padding and `normalize_lsn` page-header normalization), NOT
its eager applied frontier: after a reconnect mid-flush, the applied
frontier can point past records not yet in YB page storage, and a
subsequent pageserver crash would lose them if the safekeeper had already
trimmed that WAL.

The walsender streams from `start_pos` toward the commit frontier:

```rust
// send_wal.rs
let end_watch = EndWatch::new(tli.get_commit_lsn_watch_rx());
let end_pos = end_watch.get();
```

If `start_pos > end_pos` the walsender logs it (info) and proceeds — the
pageserver may be momentarily ahead during catch-up races; it simply waits
for the watch to advance. Every WAL read path (`WalReader` →
`YbSegmentReader`) is capped at the live flush frontier, so bytes that are
written but not yet durable — which a reconnecting walproposer may rewrite
— are never served (#139).

### Interpreted WAL batch LSNs (pageserver protocol)

Per batch (`wal_reader_stream.rs` → `send_interpreted_wal.rs`):

| Wire field | Source | Meaning |
|------------|--------|---------|
| `streaming_lsn` | `batch.wal_end_lsn` | End of the WAL chunk this message covers. |
| `commit_lsn` | `batch.available_wal_end_lsn` | End of WAL available on the safekeeper (the commit/flush frontier at read time). |

The pageserver maps these into its `WalReceiverConnectionStatus`
(`streaming_lsn` / `commit_lsn`).

### Pageserver feedback (inbound)

The pageserver sends `PageserverFeedback` as a CopyData message tagged
`NEON_STATUS_UPDATE_TAG_BYTE` (`b'z'`, `send_wal.rs`), distinguishing it
from standard standby status updates and hot-standby feedback. The reply
reader skips the tag and length bytes, parses the key/value payload
(`PageserverFeedback::parse`,
`compute/libs/utils/src/pageserver_feedback.rs`), records it in the
walsender slot, and applies exactly one field to retention:

| Field (wire key) | Safekeeper use |
|------------------|----------------|
| `remote_consistent_lsn` (`ps_applylsn`) | **`update_remote_consistent_lsn`** — the WAL retention bound. |
| `last_received_lsn` (`ps_writelsn`) | `laggard_lsn()` clamp for WAL removal (min across pageserver walsenders); `safekeeper_ps_last_received_lsn` metric. |
| `disk_consistent_lsn` (`ps_flushlsn`) | Relayed to the compute (slot apply position, backpressure); metrics. Not a truncation input. |
| `current_timeline_size`, `replytime`, `corruption_detected` | Relayed to the compute via `AppendResponse.pageserver_feedback`. |

The pageserver populates both `remote_consistent_lsn` and
`disk_consistent_lsn` from its single durable watermark
(`Timeline::get_consistent_lsn()`), so the two wire fields carry the same
value and the truncation bound is durable by construction.

`should_walsender_stop(reported_remote_consistent_lsn)` terminates an
inactive walsender once the reported value reaches `flush_lsn` — nothing
left to stream, nothing left to wait for.

---

## Standby / hot-standby LSNs (in memory)

| LSN | Where | Role |
|-----|-------|------|
| `StandbyReply.apply_lsn` | Per walsender slot (`record_standby_reply`) | Aggregated (min across senders) in `update_reply_feedback`; observability. |
| `HotStandbyFeedback.xmin` / `catalog_xmin` | `WalSenders.agg_standby_feedback` | Min-aggregated across standby senders; injected into `AppendResponse.hs_feedback` for the compute's `ProcessStandbyHSFeedback`. |

Not persisted. Not inputs to WAL truncation.

---

## WAL removal horizon

`remove_wal::calc_horizon_lsn`:

```rust
horizon_lsn = min(
    state.cfile_remote_consistent_lsn,  // PERSISTED remote_consistent_lsn
    state.flush_lsn,                    // never trim past durable end-of-WAL
    extra_horizon_lsn,                  // slowest active walsender (laggard)
)
```

The manager (`update_wal_removal`) computes
`removal_horizon_segno = horizon.segment_number() - 1` (saturating) and,
when it exceeds `last_removed_segno`, spawns
`YbWalStorage::remove_up_to(segno)` — dropping cached chunks first, then
DELETEing all `safekeeper.wals` rows with `segno < segno_up_to` (bounded on
both sides by the last-removed segno for a tight index range).

**Why each term is in the min:**
- *Persisted* `remote_consistent_lsn`, not the in-memory copy: a feedback
  bump that hasn't reached `safekeeper.timeline_state` yet must not license
  deletion — after a safekeeper restart the horizon recomputes from the
  persisted row, and WAL deleted on the strength of a lost in-memory value
  would be gone while the row still claims it is needed.
- `flush_lsn`: a stale or corrupt `remote_consistent_lsn` past end-of-WAL
  must not trim beyond durability.
- Laggard clamp (`WalSenders::laggard_lsn`, min of pageserver walsenders'
  `last_received_lsn`): WAL lives only in `safekeeper.wals` rows — there is
  no secondary copy for an active pageserver connection to re-fetch from,
  so deleting bytes a walsender still needs is one-way data loss.

**Timeline activity** (`update_is_active`): the timeline counts as active
while computes are attached or
`remote_consistent_lsn < flush_lsn` (the pageserver hasn't durably ingested
everything). Drives the `broker_active` flag and the
`safekeeper_timeline_active` gauge.

---

## LSN ordering (steady state)

```
remote_consistent_lsn (inmem)  ≥  remote_consistent_lsn (persisted)
                                   (advisory save lags by ≤ save interval)

remote_consistent_lsn  ≤  flush_lsn (= commit_lsn)

timeline_start_lsn  ≤  local_start_lsn  ≤  flush_lsn

write_lsn  ≥  write_record_lsn  ≥  flush_record_lsn

reported flush_lsn  =  min(flush_record_lsn, durable_lsn)
                       (never past confirmed YB durability)

start_streaming_at  ≥  flush_lsn      (asserted on ProposerElected)
truncate_wal end_pos ≤ durable_lsn    (refused otherwise)
```

**Why they hold / what breaks:**
- *Durability cap on `flush_lsn`*: the ack the compute releases
  synchronous commits on. If it ever exceeded `durable_lsn`, a safekeeper
  crash after the ack but before the YB INSERT would lose an acknowledged
  commit — the exact failure the producer/consumer split is designed to
  make impossible.
- *`remote_consistent_lsn ≤ flush_lsn`*: feedback describes ingested WAL,
  which the safekeeper streamed, which is ≤ its durable end. The horizon
  min() enforces it even against a misbehaving peer.
- *Truncation floor*: `start_streaming_at < flush_lsn` would delete WAL
  already acked to the compute (and possibly already streamed to the
  pageserver); the assert makes the disagreement loud instead of silent.

---

## Cross-link: pageserver restart → stream → feedback loop

1. Pageserver loads `consistent_lsn` from `pageserver.timeline_state` and
   seeds both its eager applied frontier and its durable slot from it.
2. Pageserver sends `START_REPLICATION PHYSICAL {startpoint}` with
   `startpoint` derived from `get_consistent_lsn()`.
3. Walsender streams from `startpoint` toward the commit-frontier watch
   (`= flush_lsn`).
4. Pageserver ingests; its applied frontier races ahead at WAL speed while
   its durable slot waits for flushes.
5. After each successful gen-fenced `advance_consistent_lsn`, the
   pageserver's next `PageserverFeedback.remote_consistent_lsn` advances.
6. Safekeeper bumps inmem `remote_consistent_lsn` immediately; the advisory
   save persists it within the save interval; `calc_horizon_lsn` then
   allows WAL removal up to it.

Both lags in this loop degrade toward *extra retention*, not loss: a
lagging pageserver column means WAL gets re-streamed into idempotent
UPSERTs; a lagging safekeeper column means segments are trimmed later.

---

## Persistence cheat sheet

| Field | YB `safekeeper.timeline_state` | YB `safekeeper.wals` | In memory only |
|-------|-------------------------------|----------------------|----------------|
| `timeline_start_lsn` | Yes | — | — |
| `local_start_lsn` | Yes | — | — |
| `remote_consistent_lsn` | Yes (advisory, lagging) | — | `TimelineMemState` (live) |
| `flush_lsn` / `commit_lsn` | — | implied by max chunk end (rehydrated by `load_write_lsn` + boundary walk) | `WalBookkeeping` + `durable_lsn` |
| `write_lsn` / `write_record_lsn` | — | same rehydration | `WalBookkeeping` |
| WAL bytes | — | Yes (coalesced chunk rows) | `WalCache` (hot cache) |
| Walsender positions / feedback | — | — | `WalSenders` slots |
| Standby `apply_lsn` / hs feedback | — | — | `WalSenders` aggregation |

---

## Code anchors

- `compute/safekeeper/src/state.rs` — `TimelinePersistentState`,
  `TimelineMemState`, `start_change` / `finish_change`.
- `compute/safekeeper/src/control_file.rs` — `COLUMNS`, row read/write,
  `AdvisoryPersist::persist_remote_consistent_lsn`,
  `note_advisory_persisted`.
- `compute/safekeeper/src/safekeeper.rs` — `AcceptorGreeting`,
  `ProposerElected`, `AppendRequest{,Header}`, `AppendResponse`,
  `flush_lsn()` wrapper, `handle_elected`, `handle_append_request`,
  `handle_flush`, `yb_emit_append_response`.
- `compute/safekeeper/src/wal_storage.rs` — `Storage` trait,
  `WalBookkeeping` watermarks, `WalReader`.
- `compute/safekeeper/src/yb_wal_storage.rs` — producer/consumer batching,
  `DurableState`, `YbFlush`, `durable_lsn` cap, `load_write_lsn`,
  `truncate_wal`, `remove_up_to`, `YbSegmentReader`.
- `compute/safekeeper/src/receive_wal.rs` — WalAcceptor loop:
  `NoFlushAppendRequest` conversion, `FlushWAL` ticker, durable-watch ack
  arm, feedback injection into `AppendResponse`.
- `compute/safekeeper/src/timeline.rs` — `WriteGuardSharedState::drop`
  (commit watch), `update_remote_consistent_lsn`,
  `should_walsender_stop`, hs-feedback injection.
- `compute/safekeeper/src/send_wal.rs` — `NEON_STATUS_UPDATE_TAG_BYTE`,
  feedback intake, `laggard_lsn`, standby aggregation, `EndWatch`.
- `compute/safekeeper/src/send_interpreted_wal.rs` /
  `wal_reader_stream.rs` — `wal_end_lsn` / `available_wal_end_lsn` batch
  bounds.
- `compute/safekeeper/src/remove_wal.rs` — `calc_horizon_lsn`.
- `compute/safekeeper/src/timeline_manager.rs` — advisory save scheduling,
  `update_wal_removal`, `update_is_active`.
- `compute/pgxn/yb_amp/walproposer.c` / `walproposer_pg.c` — greeting →
  `propStartLsn`, `GetAcknowledgedWALPosition`, slot advance,
  feedback shared-memory relay.
- `compute/libs/utils/src/pageserver_feedback.rs` — feedback wire format.
- `controller/migrations/0002_compute_storage.up.sql` —
  `safekeeper.timeline_state` + `safekeeper.wals` schemas.
- [`pageserver-lsns.md`](pageserver-lsns.md) — the pageserver side of the
  link.
