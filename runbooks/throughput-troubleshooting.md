# Runbook: Troubleshooting throughput issues (yb-light)

Diagnoses low throughput (TPC-C load or execute, or any OLTP workload) on the **yb-light** stack:
a Postgres compute, a yb-pageserver, a safekeeper, a controller, and a YugabyteDB backing
store. The components may be co-located on one box or spread across boxes — this runbook does not
assume a particular topology. Substitute the conventions below for your deployment.

## Conventions (fill these in for your deployment)
Set these once per shell; every command below uses them. Get the values from the deployment's
service configs (`pageserver.toml`, the safekeeper/SC start scripts, the compute's
`postgresql.conf` / `yb_amp.pageserver_connstring`).

```bash
# --- Postgres compute (the benchmark connects here) ---
export COMPUTE_HOST=...        # e.g. the compute box IP/host
export COMPUTE_PORT=...        # yb-light compute port
export COMPUTE_USER=cloud_admin
export COMPUTE_DB=tpcc         # or whatever DB the workload uses
export PGPASSWORD=...          # compute password

# --- yb-pageserver (wherever it runs) ---
export PS_HOST=...             # pageserver host
export PS_HTTP_PORT=...        # pageserver HTTP/metrics port

# --- controller ---
export SC_BASE=http://<sc-host>:<sc-port>

# --- YugabyteDB backing store ---
export YB_HOST=...; export YB_PORT=...; export YB_DB=...
export YB_STORAGE_PASSWORD=...    # YB password (also used by PS/SK)

# --- psql + a ready-made compute conn string ---
export PSQL=<path-to>/psql                     # the bundle's psql, or system psql
export C="host=$COMPUTE_HOST port=$COMPUTE_PORT user=$COMPUTE_USER dbname=$COMPUTE_DB sslmode=disable"
```

### psql shared-lib note
Some bundle psql builds want `libreadline.so.7` while the OS ships only `.so.8`. If psql errors on
the shared lib, symlink it and prepend to `LD_LIBRARY_PATH`:
```bash
mkdir -p /tmp/psqllibs; ln -sf /usr/lib64/libreadline.so.8 /tmp/psqllibs/libreadline.so.7
export LD_LIBRARY_PATH=/tmp/psqllibs:$LD_LIBRARY_PATH
```
Many bundles ship a psql that works without this — try it first.

---

## The one rule
**Find the bottleneck before changing anything.** In this stack the limiter is almost never the
component you'd guess. Walk the checklist top to bottom; each step tells you whether to keep going.

---

## Step 1 — What are the backends waiting on? (most important signal)
```bash
# sample several times — one snapshot lies
for i in $(seq 8); do
  PGPASSWORD="$PGPASSWORD" $PSQL "$C" -tAc \
   "select coalesce(wait_event_type||'/'||wait_event,'(running)') w, count(*)
    from pg_stat_activity where backend_type='client backend' and state='active'
    group by 1 order by 2 desc;"
done
```
Read the dominant wait event and jump to the matching cause below:

| Dominant wait | Likely cause | Go to |
|---|---|---|
| `IPC/SyncRep` | commit blocked on WAL durability to the safekeeper | **Step 2** |
| `Extension/YbAmp/PS_ReadIO` | getpage round-trips to the pageserver | **Step 3** |
| `LWLock/BufferContent`, `LWLock/*` | buffer/lock contention on hot pages | **Step 4** |
| `(running)` / `CPU` | CPU-bound — find which component | **Step 5** |
| `Client/ClientRead` | client/driver side (connection pool, terminal pacing) | **Step 6** |

---

## Step 2 — Commit durability latency (`IPC/SyncRep`)
The WAL path: every commit under `synchronous_commit=on` + `synchronous_standby_names=walproposer`
blocks until the **safekeeper** persists that WAL to YugabyteDB (`wals` table, over TLS). This is
**latency-bound**, not throughput-bound — the compute and the safekeeper can both sit near-idle
while WAL crawls. The hit is worse when the safekeeper and YB store are far apart (cross-box /
cross-AZ round-trips per commit).

**Confirm:**
```bash
# WAL advance rate — healthy load is tens of MB/s; ~1 MB/s = durability-bound
L1=$(PGPASSWORD="$PGPASSWORD" $PSQL "$C" -tAc "select pg_current_wal_lsn();")
sleep 4
L2=$(PGPASSWORD="$PGPASSWORD" $PSQL "$C" -tAc "select pg_current_wal_lsn();")
PGPASSWORD="$PGPASSWORD" $PSQL "$C" -tAc \
  "select pg_size_pretty((pg_wal_lsn_diff('$L2','$L1')/4)::bigint)||'/s';"
```
Cross-check the safekeeper is idle (low CPU) in Step 5. If SK CPU is low and WAL is slow → durability latency.

**Levers (load phase only — does NOT change the execute measurement):**
- `ALTER SYSTEM SET synchronous_commit = off;` then `SELECT pg_reload_conf();` — commits return
  without waiting for SK durability. Multi-fold load speedup. Re-enable before a durability test.
- Fewer/larger commits (loader batch size) reduce the number of SyncRep waits.
- Do **not** silently flip this for a run whose point is durability/latency — note it in results.

---

## Step 3 — getpage read latency (`Extension/YbAmp/PS_ReadIO`)
Each shared-buffers + LFC miss fetches the page from the pageserver. The 8k-direct PS does a
**per-page YB fetch on miss** (~100–180 ms each when cold, more if the PS is off-box from YB). With
few backends this serializes hard.

**Quantify:**
```bash
curl -s http://$PS_HOST:$PS_HTTP_PORT/metrics | awk '
 /smgr_query_seconds_sum.*get_page_at_lsn/   {s=$2}
 /smgr_query_seconds_count.*get_page_at_lsn/ {c=$2}
 END{ if(c>0) printf "mean_getpage=%.1f ms over %d calls\n",(s/c)*1000,c }'
```
- **mean > ~50 ms** → read-latency-bound. The PS is waiting on YB per page; not a compute problem.
  Check chain depth: `pageserver_deltas_per_read_global_*` (deep chains = more replay per getpage).
- **mean low (single-digit ms) AND few calls** → reads are NOT your bottleneck; most reads are
  served from LFC. Go back to Step 1 — the real limiter is elsewhere (usually Step 2).

**Attribute that ~50ms — is it YB-side or PS-internal?** The metric `yb_client_query_seconds{op=query}`
(no `pageserver_` prefix) reports per-query wall-clock at the PS YsqlClient layer. Compare:
```bash
curl -s http://$PS_HOST:$PS_HTTP_PORT/metrics | awk '
 /^yb_client_query_seconds_sum\{op="query"/   {ys=$2}
 /^yb_client_query_seconds_count\{op="query"/ {yc=$2}
 /pageserver_smgr_query_seconds_sum.*get_page_at_lsn/   {ss=$2}
 /pageserver_smgr_query_seconds_count.*get_page_at_lsn/ {sc=$2}
 END{ if(yc>0&&sc>0) printf "smgr=%.1fms  yb_query=%.1fms  share=%.0f%%\n",
         (ss/sc)*1000,(ys/yc)*1000,(ys/yc)/(ss/sc)*100 }'
```
- If `yb_query / smgr` ≈ 80–95% → the bottleneck is **YB-side** (RocksDB seek, plan execute,
  conn-pool wait). Compare against the YB tserver's own `handler_latency_yb_ysqlserver_SQLProcessor_SelectStmt`
  (typically 1–2 ms server-side). The remainder is **conn-pool queueing** in PS — see **Step 3a**.
- If `yb_query / smgr` < 50% → time is in PS-internal work (chain walk, walredo dispatch).
  Check `crossbeam_skiplist` / `UnifiedPages` in a `perf record -g -p <PS pid> -- sleep 30`.

**Levers:** bigger LFC (`yb_amp.file_cache_size_limit`), bigger `page_cache_size` in `pageserver.toml`,
more concurrent terminals (amortize latency), keep working set warm.

---

## Step 3a — PS → YB connection pool starvation (`YbAmp/PS_ReadIO` + high `yb_query` latency)

**Symptom:** PS has very few persistent TCP connections to the YB tserver (e.g. ~9) while many
compute backends are blocked on `Extension/YbAmp/PS_ReadIO`. Hikari `ConnAcq` latency on the bench
side blows up to seconds-to-tens-of-seconds even at "modest" connection counts (e.g. 100).

**Diagnose** (run wherever the PS lives, replacing `$YB_HOST` with the YB tserver host):
```bash
# How many established TCP conns does PS hold to YB?
sudo ss -tnp dst $YB_HOST 2>/dev/null | awk 'NR>1' \
  | grep -oP 'pid=\d+' | sort | uniq -c | sort -rn
```
Cross-reference each pid against `ps -o pid,comm,args -p <pid>`. The yb-pageserver pid's count is
the YB pool size. With `scale_factor=1.0` (default) it's around 9 — far too few for the typical
~100 concurrent compute backends doing getpages. The shortage shows up as long
`yb_client_query_seconds{op=query}` even when YB's own `handler_latency_yb_ysqlserver_SQLProcessor_SelectStmt`
is ~1–2 ms (the gap is queue wait inside PS).

**Fix — bump `scale_factor` in `pageserver.toml`:**
```toml
# In pageserver.toml — MUST be at the top level, NOT inside [timeline_init]
# (otherwise PS logs "ignoring unknown configuration item path=timeline_init.scale_factor")
scale_factor    = 6.0
page_cache_size = 8192    # explicit override so it isn't scaled along with everything else
```
Then `yb_light_local pageserver restart --id 1`. Verify:
- `WARN ignoring unknown configuration item` lines for `scale_factor` are absent from the PS log.
- The TCP-conn count in the `ss` command above multiplies (e.g. 9 → ~49 at `scale_factor=6.0`).

**Observed impact at scale_factor=6 on a 4 vCPU / 32 GB tier** (1000 wh TPC-C, 100 conn):
TPM-C 6,200 → 12,500 (≈ 2×); per-tx avg latency 430 ms → 138 ms (3×); StockLevel avg 3,286 ms → 763 ms (4.3×);
Hikari ConnAcq 21,000 ms → 170 ms (125×).

This is independent of any LFC / page_cache change — `scale_factor` widens the PS-side concurrency
into YB. It does **not** scale the YB tserver itself; if YB-server `SelectStmt` is also ≥ 5 ms
average, see Step 4 / 7 and consider raising YB's `db_block_cache_size_percentage`.

---

## Step 4 — Lock / buffer contention (`LWLock/BufferContent`)
Backends fighting over the same hot pages (e.g. TPC-C WAREHOUSE/DISTRICT rows, or a growing index
during bulk insert). Some `BufferContent` alongside another dominant wait is normal; if it dominates,
the workload is concentrated on a few pages.
- Check for a single hot table/index in `pg_stat_activity.query`.
- More warehouses / wider key distribution spreads contention.
- Not usually the primary limiter in this stack — confirm Steps 2/3 first.

---

## Step 5 — Who is actually burning CPU?
Look at CPU per process wherever each component runs. If components are co-located, one `top` shows
all of them; if split across boxes, check each box.
```bash
# On the box hosting the compute / safekeeper / SC:
top -bn1 -w 200 | head -15
# On the box hosting the pageserver:
ps -o %cpu,rss,comm -C yb-pageserver
```
Interpret CPU **relative to that box's core count** (a process near `N*100%` on an N-core box is
saturated):
- **Compute pegged (near full core count):** compute is the limiter — fewer loader threads won't
  help; it's genuinely CPU-bound.
- **PS pegged on ONE core (~100%) while the rest idle:** the known **single-threaded getpage replay /
  image-materializer lock starvation** — see Step 7. Adding cores won't help; it's lock-bound.
- **Everything idle:** you are latency-bound (Step 2 or 3), not CPU-bound. Stop looking for a CPU fix.

---

## Step 6 — Client side (`Client/ClientRead`, pool timeouts)
- TPC-C uses keying+think time pacing (`useKeyingTime/useThinkTime=true`) — paced runs are *meant*
  to idle. Low tpmC under pacing is expected, not a bug. See efficiency formula below.
- `HikariPool ... Connection is not available, request timed out` → a terminal couldn't grab a
  connection. Raise the connection-acquisition timeout (`TPCC_CONN_TIMEOUT_MS`; default 180000, use
  600000 for a high-latency/off-box pageserver) and/or lower the terminal count (`TPCC_CONNS`). This
  aborts the whole run, so it masks the real throughput number.

---

## Step 7 — Pageserver internals: materializer / delta_counts backlog
The 8k-direct PS mints full-page images after a delta threshold. Under concurrent OLTP the image
materializer can be **starved on the image-map lock** (`install_image acquire_ms ~200ms`), minting
only single-digit images/s while deltas pile up.

**Watch:**
```bash
curl -s http://$PS_HOST:$PS_HTTP_PORT/metrics | grep -E \
 '^pageserver_yb_pagewriter_delta_counts_entries|^pageserver_yb_materializer_(queue_depth|inflight_bytes|read_side_dispatches_total)'
```
- `delta_counts_entries` (gauge from `PageWriter::stats()`, = size of the per-key delta-tally map) is
  the **count of pages with ≥1 non-anchor delta since their last anchor (FPI/will_init)**. An entry
  is removed when its chain crosses threshold and gets re-imaged, or when an FPI resets the chain.
  Most entries are sub-threshold pages (many cold pages never cross), so a high absolute count is
  normal coverage, **NOT** a backlog. **Read the SHAPE over time, not the number:**
  - **flat / decaying** → steady state, materializer keeping up. Healthy.
  - **climbs during writes, plateaus at rest** → normal (sub-threshold + cold pages).
  - **climbs monotonically forever, even at idle** → materializer drain < arrival (the old-build bug
    that hit 8.4M; the cold-residue case is fixed in `6d7510942`, drained on every flush).
- The real "materializer falling behind" signal is `queue_depth` / `inflight_bytes` **rising and
  staying up**, plus a heavy **`deltas_per_read` tail** (a few continuously-*hot* pages with very
  deep chains → slow getpage). `queue_depth=0, inflight_bytes=0` = keeping up regardless of count.
- `6d7510942`'s cold-key drain trims cold residue but does NOT materialize continuously-hot pages —
  those deep chains show up in `deltas_per_read`, not in the entry count.
- PS log perf lines: `perf tag="materialise_batch" ... batch_size= images=` and
  `perf tag="install_image" acquire_ms= held_ms=` — high `acquire_ms` with `held_ms=0` = pure
  lock-acquire starvation, not YB latency.

---

## Quick reference

**TPC-C efficiency** = achieved tpmC / (warehouses × 12.86). 12.86 tpmC/warehouse is the spec max
under full keying+think pacing.

**Load progress** (the loader logs only "Finished!" at the end):
```bash
PGPASSWORD="$PGPASSWORD" $PSQL "$C" -tAc "select count(*) from warehouse;"   # of N warehouses
```
A large load (e.g. 1000wh) on a small, latency-bound stack can run for hours — that is the baseline,
not a stall. Confirm with the WAL-rate and wait-event checks above before declaring a problem.

**Knobs (`scripts/tpcc-run.sh`):** `TPCC_RUNTIME`(s), `TPCC_CONNS`, `TPCC_WARMUP`,
`TPCC_LOADER_THREADS`, `TPCC_CONN_TIMEOUT_MS`.

**Symptom → first check:**
| Symptom | First thing to look at |
|---|---|
| Load crawling, everything idle | Step 2 — WAL rate + SyncRep |
| Throughput dropped vs a prior run | Step 7 — delta_counts net-growing? + Step 3 getpage mean |
| Run aborted early | Step 6 — connection-pool timeout in `execute.log` |
| One PS core at 100%, rest idle | Step 7 — materializer lock starvation |
| Compute pegged at full core count | CPU-bound — genuine, not tunable away |
| All backends on `YbAmp/PS_ReadIO` + Hikari ConnAcq tens of seconds | **Step 3a** — PS → YB pool starvation, bump `scale_factor` |
| `yb_client_query` ≫ YB tserver `SelectStmt` | **Step 3a** — PS-side queueing for too-few YB conns |
