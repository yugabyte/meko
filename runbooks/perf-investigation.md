# Runbook: yb-light performance investigation

Broader performance investigation for the yb-light stack — memory growth, pageserver internals, WAL
ingest, restart/replay, and CPU localization. Use this when something is "slow" or "using too much
RAM" but you don't yet have a clean throughput symptom. For a *throughput* number that's too low,
start with `throughput-troubleshooting.md`; for a component that won't start, see
`startup-failures.md`.

Topology-agnostic: components may be co-located or split across boxes. Fill in the conventions.

## Conventions
```bash
export PS_HOST=...   export PS_HTTP_PORT=...     # yb-pageserver metrics/HTTP
export COMPUTE_HOST=... export COMPUTE_PORT=...   # yb-light compute
export PSQL=<path-to>/psql
export C="host=$COMPUTE_HOST port=$COMPUTE_PORT user=cloud_admin dbname=tpcc sslmode=disable"
export M="curl -s http://$PS_HOST:$PS_HTTP_PORT/metrics"   # pageserver metrics shortcut
```

---

## 1. Localize the bottleneck first
Don't tune blind. Establish which **component** and which **resource** (CPU / memory / IO / network
latency) is the limiter:

- **Backend waits** — `pg_stat_activity` dominant wait event (the Step 1 loop in
  `throughput-troubleshooting.md`). This single signal usually decides everything.
- **CPU per process**, interpreted relative to the host's core count (`top` / `ps -o %cpu,rss,comm`).
- **PS metrics** — getpage latency, materializer backlog, RSS (below).
- **WAL rate** — `pg_wal_lsn_diff` over a few seconds (Step 2 of the throughput runbook).

Rule of thumb for this stack: **everything idle + slow = latency-bound** (commit durability or
getpage), not a CPU/tuning problem.

---

## 2. getpage read latency
Cache misses fetch pages from the pageserver, which does a per-page YB fetch on miss. Off-box from
YB, that's ~100–180 ms cold.
```bash
$M | awk '
 /smgr_query_seconds_sum.*get_page_at_lsn/   {s=$2}
 /smgr_query_seconds_count.*get_page_at_lsn/ {c=$2}
 END{ if(c>0) printf "mean_getpage=%.1f ms over %d calls\n",(s/c)*1000,c }'
$M | grep -E '^pageserver_deltas_per_read_global_'   # deep delta chains = more replay per read
```
- mean > ~50 ms → read-latency-bound; the fix is caching (LFC / page_cache) or chain depth, not CPU.
- mean single-digit ms with few calls → reads served from LFC; not your bottleneck.

## 3. Materializer / delta_counts backlog
The 8k-direct PS mints full-page images after a delta threshold; under concurrent OLTP the image
materializer can be starved on the image-map lock.
```bash
$M | grep -E '^pageserver_yb_pagewriter_delta_counts_entries|^pageserver_yb_materializer_(queue_depth|inflight_bytes|read_side_dispatches_total)'
```
- `delta_counts_entries` = size of the per-key delta-tally map = **count of pages with ≥1 non-anchor
  delta since their last anchor**; entry removed on re-image (chain crosses threshold) or FPI reset.
  Most entries are sub-threshold/cold pages, so the absolute count isn't a backlog. **Read the shape:**
  flat/decaying = healthy; climbs-then-plateaus = normal; climbs monotonically even at idle =
  drain < arrival (old-build bug, hit 8.4M; cold-residue case fixed in `6d7510942`, drained per flush).
- The "falling behind" signals are `queue_depth`/`inflight_bytes` staying up and a heavy
  `deltas_per_read` tail. `queue_depth=0, inflight_bytes=0` = keeping up, regardless of entry count.
- `6d7510942`'s cold-key drain trims cold residue; it does NOT materialize continuously-hot pages,
  whose deep chains surface in `deltas_per_read`.
- PS log: `perf tag="install_image" acquire_ms= held_ms=` — high `acquire_ms` with `held_ms=0` is
  pure **lock-acquire starvation**, not YB latency. Typically appears as **one PS core pegged at
  100% while the rest idle** — adding cores won't help.

## 4. Pageserver memory / RSS
PS RSS can look alarming; decide leak vs. cache vs. genuine live heap before acting.
```bash
ps -o rss,comm -C yb-pageserver
$M | grep -E '^pageserver_yb_.*bytes'          # in-flight write/read buffers
curl -s http://$PS_HOST:$PS_HTTP_PORT/profile/heap   # jemalloc heap profile (allocated vs mapped)
```
Known patterns (see memory notes):
- **During bulk LOAD**, RSS > ~2 GB is usually **genuine live heap** (UnifiedPages map + PageWriter
  staging + walredo), not arena cache — confirm via `/profile/heap` `allocated`.
- **At scale, RSS drifting to tens of GB** has been **jemalloc arena cache** (static tikv-jemalloc,
  bg_thread off) + uncapped in-flight buffers — not a leak. Don't chase it as a leak; cap in-flight
  buffers / accept arena retention.
- `delta_counts_approx_bytes` badly **undercounts** true footprint — don't trust it for sizing.

## 5. WAL ingest / commit durability
If commits stall (`IPC/SyncRep`) the limiter is safekeeper→YB durability latency, not throughput.
See Step 2 of `throughput-troubleshooting.md` (WAL-rate check + `synchronous_commit=off` load-only
lever). Confirm the safekeeper is low-CPU before blaming it.

## 6. Restart / replay behavior
After a PS restart, watch whether it replays a large WAL backlog before serving:
- The 8k-direct PS historically **never flushed yb-light layers**, so OLD builds re-replayed **all** WAL
  on restart (long unavailability). **Fixed in `93f755767`+**: resumes from the persisted
  `pageserver.timeline_state.consistent_lsn`.
- If a restarted PS is slow to become ready, check its log for full-history replay and confirm the
  build is `93f755767` or newer.

## 7. CPU localization (which component, which box)
```bash
# wherever compute / safekeeper / SC run:
top -bn1 -w 200 | head -15
# wherever the pageserver runs:
ps -o %cpu,rss,comm -C yb-pageserver
```
Interpret relative to core count:
- compute near full core count → genuinely compute CPU-bound (loader threads won't help).
- PS one core at 100%, rest idle → materializer/replay lock starvation (§3).
- all idle → latency-bound (§2 or §5).

---

## Known build-specific gotchas (verify the build before deep investigation)
Many "perf regressions" are build defects. Quick build vetting:
- **YB TLS connect fails on startup** (`open_tls … (openssl)` / `SSL_connect`): rustls→OpenSSL
  switch in some builds breaks TLS to YB entirely. `strings <bin> | grep -i 'SSL_connect\|OpenSSL'`;
  prefer a build using rustls.
- **`put_block_size_mb` ignored** (old bundles): YB blob chunk size was hardcoded ~100 MB; a
  small-block comparison wasn't actually configurable.
- **`s3_upload_limit_bytes`** (newer bundles): a **hard per-layer-size cap**, not a chunker — set too
  low without also shrinking `checkpoint_distance`/`compaction_target_size` and creation/load wedges.

Cross-reference the auto-memory project notes for the specifics behind each of these before
spending time on a tuning theory.

---

## Quick metric cheat-sheet
| What | Command |
|---|---|
| getpage mean latency | `$M \| awk '…get_page_at_lsn…'` (§2) |
| delta backlog growing? | `$M \| grep delta_counts_entries` (§3) |
| materializer keeping up? | `$M \| grep yb_materializer_` (§3) |
| PS RSS | `ps -o rss,comm -C yb-pageserver` |
| PS heap (leak vs cache) | `curl …/profile/heap` |
| WAL rate | `pg_wal_lsn_diff` over 4 s (throughput runbook §2) |
| backend waits | `pg_stat_activity` loop (throughput runbook §1) |
| endpoint/branch create latency | `YBL_TIMING=1 yb_light_local endpoint create …` — per-phase ms to stderr (`[timing <scope> +Nms] …`); attributes create time across cplane-load / branch-resolve / SK+PS provisioning / register |
