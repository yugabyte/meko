---
status: pending
severity: high
area: yb-pageserver
opened: 2026-06-10
---

# PS cache-walk regression: `scan_range` `cache_ms` degrades as deltas accumulate

## Symptom

Over the course of a single `execute` run, `scan_range` per-stage
timing degrades from ~YB-bound to ~cache-bound. The in-RAM cache walk
(`cache_ms` = the `lookup_range` stage) goes from 0 ms to dominating
the request:

| When | total_ms | yb_ms | cache_ms |
|---|---|---|---|
| Early (first ~200 reqs) | 20–101 | 20–101 | 0 (cache empty, all to YB) |
| Middle of run | 404–585 | 4–42 | 394–577 (cache walk dominates) |
| Tail end | 363–530 | 3–25 | 348–513 |

At the tail: `pageserver_yb_pagewriter_delta_counts_entries` ≈ 4.5 M
(approx_bytes ≈ 364 MB of in-memory delta metadata), while
`unflushed_bytes` is only ~8.8 MB. So nearly all deltas ARE persisted
in YB delta layers — the in-memory structure is the lookup path that
got slow, not a write backlog.

Pattern:
1. PS starts cold → `cache_ms` ≈ 0, `total` ≈ `yb_ms` = 20–100 ms.
2. As the workload runs, in-RAM bookkeeping accumulates.
3. Each range scan's cache walk → `cache_ms` grows 0 → 100 → 400 ms+.
4. Steady state: every range read is dominated by the in-memory walk.

## Impact

Throughput regression. The ~78% tpmC figure was presumably on a build
*before* the in-RAM growth hurt, when `cache_ms` stayed near zero.
Recovering throughput needs the cache walk to be sub-linear in cache
size again.

## Root cause / hypothesis

Two candidates, not yet bisected apart:

1. **`UnifiedPages::lookup_range` is O(total cached keys), by design.**
   `pageserver/src/unified_pages.rs:462` iterates the *entire*
   `by_key` DashMap (`for entry in self.by_key.iter()`) and filters
   each key against `[start_key, end_key)` with a byte comparison.
   Its own doc-comment says "Iterates the full DashMap … Not on the
   getpage hot path — scan_range is called for vectored reads, which
   are rare." If the workload issues range/vectored scans at any real
   rate, every one of them now sweeps all ~millions of cached keys —
   exactly the 0 → 400 ms growth that tracks cache size. The fix here
   is structural: make the range lookup seek into an ordered index
   (e.g. range over a `BTreeMap<Bytes, …>` / a sorted key view) so the
   walk is O(log N + matched keys) instead of O(all keys).

   Note: the single-key getpage path early-returns into `read_chain`
   *before* `lookup_range` (`page_writer.rs:725`), so plain getpage is
   not the victim — this is specifically the multi-key/vectored
   `scan_range` path that falls through to `lookup_range`.

2. **`b8aa3fac4` "pageserver: Arc<PageRow> through chain reads +
   flush"** is the build commit fingered as the regression boundary
   and touches every one of these read paths (`lookup_chain`,
   `lookup_range`, `scan_range`, merge/sort/dedup over
   `Vec<Arc<PageRow>>`). The Arc change *should* reduce per-row cost
   (refcount bump vs struct copy), but it may have added per-entry
   atomic ref-count traffic on hot accesses, or a subtler shape change
   that becomes painful at 4.5 M entries. Needs confirmation that the
   slowdown predates vs. postdates this commit.

The 364 MB / 4.5 M-entry `delta_counts` figure is the other half:
deltas are persisted but the in-RAM count bookkeeping (and/or the
per-key chains in `by_key`) keeps growing. Worth checking whether
`delta_counts` and `by_key` are being trimmed (eviction / GC) at the
rate the docstrings assume, or whether something keeps them resident.

## Repro

Run the standard sustained-write workload (the one that yielded the
tpmC number) and scrape the `tag = "scan_range"` `perf` log lines
(`page_writer.rs:783`) plus `pageserver_yb_pagewriter_delta_counts_*`
over the run. Confirm `cache_ms` climbs monotonically with
`delta_counts_entries`.

## Path to recover throughput

- Bisect: confirm whether the regression boundary is `b8aa3fac4` by
  running an immediately-prior build under the same workload and
  watching `cache_ms`.
- If it's `lookup_range`'s full-DashMap sweep: replace the linear scan
  with an ordered seek so range lookups are O(log N + hits).
- If it's the Arc churn: profile per-row cost at scale; consider
  avoiding refcount traffic on the merge/sort/dedup hot loop.

## Where to look

- `pageserver/src/unified_pages.rs:462` — `lookup_range` (full DashMap
  iter, the `cache_ms` stage).
- `pageserver/src/page_writer.rs:717` — `scan_range` (the `perf`
  timing emitter; single-key fast path vs. `lookup_range` fall-through).
- `pageserver/src/page_writer.rs:202` — `delta_counts` DashMap and its
  trimming (`prune` paths around `page_writer.rs:686`, `1115`).
- Build commit `b8aa3fac4` — `Arc<PageRow>` through chain reads + flush.
