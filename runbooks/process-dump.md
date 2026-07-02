# Diagnosing async hangs with a tokio task dump

Use this whenever a yb-light process **stops making progress but isn't
crashing, CPU-bound, or blocked on YB/network** — i.e. an async deadlock or a
lost wakeup. Applies to every daemon: pageserver, safekeeper,
endpoint_storage, storage_controller, compute_ctl.

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
pgrep -a yb-pageserver              # or yb-safekeeper / yb-compute-ctl / ...
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

### getpage / wait_lsn stall
Symptom: `compute_request` tasks all parked in `Timeline::wait_lsn`
(`last_record_lsn.wait_for_timeout`). That's usually a *symptom* of ingest
having stopped (advance `applied_lsn`/`last_record_lsn` again by unwedging the
walreceiver) — check the `walreceiver` runtime in the same dump for the real
cause before touching the read path.

### Flush / materializer stall
Symptom: writes stall with `unflushed_bytes` pinned high. In the dump, the
`background_op` flush task parked in `PageWriter::flush` — e.g. on the bounded
materializer channel `send().await` (`dispatch_materialize_jobs`), while the
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
cargo yb_light <component> restart      # or: yb <component> restart
```

Always capture the task dump **before** restarting — it is the only artifact
that names the cycle.

## Related

- `utils::tokio_dump` — the SIGUSR2 dumper wired into every daemon's `main`.
- `runbooks/throughput-troubleshooting.md`, `runbooks/perf-investigation.md`.
- `architecture/yb-pageserver.md` — flush / materializer / backpressure model.
