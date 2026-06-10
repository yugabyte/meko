# Runbook: Troubleshooting throughput issues (yb-neon)

Diagnoses low throughput (TPC-C load or execute, or any OLTP workload) on the **yb-neon** stack:
a Postgres compute, a yb-pageserver, a safekeeper, a storage_controller, and a YugabyteDB backing
store. The components may be co-located on one box or spread across boxes — this runbook does not
assume a particular topology. Substitute the conventions below for your deployment.

## Conventions (fill these in for your deployment)
Set these once per shell; every command below uses them. Get the values from the deployment's
service configs (`pageserver.toml`, the safekeeper/SC start scripts, the compute's
`postgresql.conf` / `neon.pageserver_connstring`).

```bash
# --- Postgres compute (the benchmark connects here) ---
export COMPUTE_HOST=...        # e.g. the compute box IP/host
export COMPUTE_PORT=...        # neon compute port
export COMPUTE_USER=cloud_admin
export COMPUTE_DB=tpcc         # or whatever DB the workload uses
export PGPASSWORD=...          # compute password

# --- yb-pageserver (wherever it runs) ---
export PS_HOST=...             # pageserver host
export PS_HTTP_PORT=...        # pageserver HTTP/metrics port

# --- storage_controller ---
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
| `Extension/Neon/PS_ReadIO` | getpage round-trips to the pageserver | **Step 3** |
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

## Step 3 — getpage read latency (`Extension/Neon/PS_ReadIO`)
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

**Levers:** bigger LFC (`neon.file_cache_size_limit`), bigger `page_cache_size` in `pageserver.toml`,
more concurrent terminals (amortize latency), keep working set warm.

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
- `delta_counts_entries` **net-growing and unbounded** = materializer not keeping up → long delta
  chains → slow getpage. (A cold-key drain for this landed in build `6d7510942`.)
- `queue_depth=0, inflight_bytes=0` = materializer is keeping up; backlog is fine.
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
