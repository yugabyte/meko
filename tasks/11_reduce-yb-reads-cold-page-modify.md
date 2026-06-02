---
status: pending
severity: medium
area: yb-pageserver
opened: 2026-06-02
---

# Reduce YB reads when cold pages are modified

## Motivation

After Phase 4f (per-key whole-eviction in `UnifiedPages`) the cache
is populated exclusively by `push(write)` and `push_durable(materialiser
image install)`. Reads NEVER insert into the cache. This is correct
but pays an avoidable YB round-trip on the cold-page-then-modify
sequence:

1. Compute reads cold key K @ LSN 50 → `read_chain` fetches
   `[image@30, delta@40, delta@50]` from YB, returns to compute.
2. Walreceiver writes K @ LSN 100 → cache holds only `[delta@100]`,
   no anchor.
3. Materialiser triggers @ LSN 100 → captured-chain fast path
   (`lookup_chain_durable_if_complete`) finds no anchor → falls
   back to `read_chain_durable` → another YB round-trip for the
   same `[image@30, delta@40, delta@50, delta@70]` we already
   fetched in step 1.

Two YB round-trips for state we already had in RAM transiently.

## Why we punted in 4f (Option 1)

The obvious approach — cache the read's chain so step 3 hits RAM —
introduces a correctness hazard whenever eviction has happened
between an earlier write and the current read. The cache cannot
remember rows it has evicted, so it cannot tell whether a YB-fetched
chain abuts cleanly with its current contents.

### Scenarios analysed

| # | Sequence | Outcome under naive read-caching |
|---|---|---|
| A | write@100 → materialize@100 | Cache `[delta@100]`. Materialiser falls back to YB for anchor. **Safe.** |
| B | read@50 → materialize@50 | Cache `[image@30, delta@40, delta@50]` from the read. Materialiser at target_lsn=50 walks anchor, finds image@30, complete. **Safe.** |
| C | read@50 → write@100 → materialize@100 | Cache `[image@30, delta@40, delta@50, delta@100]`. `lookup_chain_durable_if_complete(K, 100)` finds anchor at image@30, returns "complete". **CORRUPTION** — if walreceiver wrote K@70 in this writer instance, that row sits in YB but not in cache; walredo applies delta@40, delta@50, delta@100 to image@30 and skips delta@70. |
| D | write@1500 → read@1000 (post-restart) → materialize@2000 | Cache after restart-fed write `[delta@1500]`. Read at 1000 fetches an older chain. **Hazard** — YB may contain K-rows in `(1000, 1500)` from a previous pageserver lifetime that this instance never saw. |
| E | push@70 → evict K → read@50 → push@100 → materialize@100 | Cache after the read+push `[image@30, delta@50, delta@100]`. **CORRUPTION** — delta@70 lives in YB (we flushed it before evicting), the cache forgot, materialiser misses it. The killer case. |

### Options considered

- **Option 1 (chosen):** never populate cache from reads. Status quo
  of 4f. Simple, correct, but pays the YB round-trip for the
  scenario above.
- **Option 2:** track per-key `cache_lo` and only extend down via
  reads when the read's `target_lsn ≥ cache_lo - 1`. Killed by E:
  eviction destroys our memory of having written delta@70, so the
  contiguity check can't see the gap.
- **Option I (two-tier):** maintain a separate "read-fed" cache that
  the materialiser ignores. Reads serve subsequent reads at the
  same LSN from RAM; materialiser still goes to YB. Recovers part
  of the optimisation at the cost of double bookkeeping.

## Plan

If benchmarks show the cold-page-modify YB round-trip matters, the
likely shape is **Option I** — a separate read-fed cache scoped to
short-lived read-only use, with the materialiser strictly served
from the write-fed `UnifiedPages` it has today. Pseudocode sketch:

```
struct ReadCache {
    by_key: HashMap<Bytes, Vec<PageRow>>,  // bounded LRU
}
```

- Populate on `read_chain` success.
- Hit on subsequent `read_chain` at the same target_lsn (or
  superset that the existing chain covers).
- **Never** read by the materialiser, by `read_chain_durable`, by
  any path that produces a durable artefact.
- Invalidate the per-key entry whenever a `push(write)` lands for
  that key (the cached chain is now stale relative to writes the
  materialiser cares about).

Open questions:
- Is the cold-page-modify scenario common enough on real
  workloads to justify the extra cache? Measure first.
- What's the eviction policy? Tied to compute query lifetime,
  or just LRU bytes?
- If the same compute connection re-reads K at a different
  `target_lsn`, do we still need YB? Probably yes — chain depends
  on LSN.

## Notes

Task 4f's own README + frontmatter documents the safe end-state.
This task is the follow-on optimisation, dependent on measuring
the cost first.
