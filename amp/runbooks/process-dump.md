# Diagnosing async hangs with a tokio task dump

Use this whenever a yb-amp process **stops making progress but isn't
crashing, CPU-bound, or blocked on YB/network** — i.e. an async deadlock or a
lost wakeup. Applies to every daemon: pageserver, safekeeper,
controller, compute_ctl.

## Why you need a task dump (OS backtraces aren't enough)

`gdb` / `pstack` / `/proc/<pid>/task/*/stack` only show **runtime worker
threads**, which in a hang are all parked in the scheduler
(`parking_lot::Condvar::wait_until_internal` → `Parker::park`). That confirms
"everything is idle" but **not which `.await` each suspended task is stuck
on** — and an async deadlock is a *cycle of suspended tasks* on tokio
`Mutex`/`Notify`/`mpsc`/copy-stream channels, which live in task state, not on
any OS-thread stack.

Rule of thumb: if the OS-thread dump shows `RawMutex::lock` frames, it's a
**sync**-mutex ABBA (findable from the thread stacks alone). If it shows only
parked workers, it's an **async** cycle — you need the task dump below.

## Capture (SIGUSR2)

Every daemon installs a `SIGUSR2` handler (`utils::tokio_dump`) that dumps the
suspend-point backtrace of **every task on every runtime**. It runs on its own
dedicated thread, so it works even when the service runtimes are wedged.

```sh
pgrep -a pageserver          # or safekeeper / compute-ctl / ...
kill -USR2 <pid>                    # dump goes to the process log AND stderr
grep -A5000 "TOKIO TASK DUMP (SIGUSR2) BEGIN" <logfile> | less
```

- **Trigger twice, ~10 s apart.** Identical dumps ⇒ hard hang (deadlock);
  changing dumps ⇒ just slow, look for the bottleneck instead.
- Output is grouped by runtime. The pageserver has four
  (`walreceiver`, `compute_request`, `mgmt_request`, `background_op`); the
  other daemons have a single `main` runtime.
- Requires a build with `--cfg tokio_unstable --cfg tokio_taskdump`
  (set in `.cargo/config.toml`; `build.sh --release-tar` bakes it in). A build
  without it prints an "unavailable" note instead of dumping.

## Read it

1. Skip tasks parked on a normal idle await (server `accept`, a timer tick,
   an empty-channel `recv`).
2. Find the tasks for the subsystem that stopped, and note the **exact
   `.await`** at the bottom of each.
3. A deadlock is two+ tasks each blocked on a resource another holds
   (a tokio `Mutex`/`Semaphore`, a `Notify` that's never fired, a bounded
   `mpsc` whose consumer is itself blocked, or a full-duplex copy stream).
4. Corroborate with the relevant gauges/sockets (see examples) to tell a
   *deadlock* from legitimate *backpressure*.

## Examples

### Ingest wedge (pageserver)
Symptom: compute `getpage`/workload wedges; `last_record_lsn` frozen; the
PS↔SK WAL socket is ESTAB with a large **Recv-Q** (PS stopped reading);
`pageserver_yb_pagewriter_unflushed_bytes` and the materializer gauges are
**0**. In the dump, look at the `walreceiver` runtime's connection task —
leading suspect is it parked writing feedback
(`zenith_status_update(...).await`) on the same `copy_both` stream it reads WAL
from (a duplex backpressure deadlock), *not* `await_backpressure` (ruled out
when the buffers read 0). Corroborate: `ss -tmi` (Recv-Q/Send-Q both full) +
the 0 gauges. Recover by restarting the pageserver — but capture the dump
first.

### Ingest dead, walreceiver `disconnected` (stale tserver addresses)
Symptom looks like the wedge above — `last_record_lsn` frozen, every
`compute_request` parked in `wait_lsn` — but nothing is parked on a lock and
`GET /v1/branch/<id>` reports `walreceiver_status: disconnected` while the
safekeeper's `flush_lsn`/`commit_lsn` run far ahead. The logs reconnect every
~5s and fail instantly:

```
Failed to establish control connection to Some("…-yb-tserver-1.…"):
  failed to lookup address information: Name or service not known
Failed to establish control connection to available servers
```

That is a connection outliving the tservers it discovered — the StatefulSet
was replaced and the per-pod DNS names went with it. Confirm from inside the
pod: `getent hosts <cached name>` returns nothing while `getent hosts
<host_ports name>` resolves. This clears on its own: on the **YSQL** lane the
smart driver refreshes its host list through the configured `host_ports` every
`yb_servers_refresh_interval` (300s by default) and falls back to a plain
connect against them when that refresh fails, so the next reconnect after the
interval lands — look for `Control connection created to one of the configured
host(s)` in the log; on the thin-client lane `connection_lifetime_ms` (default
15 min) re-resolves `tserver_rpc_addrs` (see
`compute/architecture/yugabyte.md` → "Bounding a connection's life"). A wedge
that outlasts both is not this failure: check
`yb_client_connection_recycles_total{outcome="failed"}` (YSQL lane) and
`yb_thin_client_connection_recycles_total{outcome="failed"}` (thin-client lane)
on `/metrics` and
`getent hosts <host_ports name>` from the pod to tell a stale cache apart from a
cluster that is simply unreachable. If a restart is needed anyway it is
lossless — the WAL is durable in the safekeeper and `amp_storage.wals`; the
pageserver is a materialization that fell behind (#811).

### getpage / wait_lsn stall
Symptom: `compute_request` tasks all parked in `Timeline::wait_lsn`
(`last_record_lsn.wait_for_timeout`). That's usually a *symptom* of ingest
having stopped (advance `applied_lsn`/`last_record_lsn` again by unwedging the
walreceiver) — check the `walreceiver` runtime in the same dump for the real
cause before touching the read path.

### Flush / materializer stall
Symptom: writes stall with `unflushed_bytes` pinned high. In the dump, the
`background_op` flush task parked in `PageWriter::flush` — e.g. on the bounded
materializer channel `send().await` (`enqueue_slru_materialize_jobs`), while the
materializer worker is parked in `redo_phase`/`commit_phase`. Note that
`flush_completed.notify_waiters()` runs *after* that send, so a stuck
materializer transitively wedges ingest backpressure.

### A daemon stops answering HTTP
Symptom: `/v1/status` (or the axum health route) times out but the process is
alive. `kill -USR2` still works (separate thread). Look at the `mgmt_request`
/ `main` runtime tasks for a handler parked on a lock or a downstream call
that never returns.

## Recover

Restart the wedged process; state resumes from the durable watermark
(`consistent_lsn` for the pageserver):

```sh
yb_amp_local <component> restart      # or: yb <component> restart
```

Always capture the task dump **before** restarting — it is the only artifact
that names the cycle.

## Related

- `utils::tokio_dump` — the SIGUSR2 dumper wired into every daemon's `main`.
- `runbooks/throughput-troubleshooting.md`, `runbooks/perf-investigation.md`.
- `compute/architecture/pageserver.md` — flush / materializer / backpressure model.
