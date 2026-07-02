//! On-demand tokio task dumps for diagnosing async deadlocks.
//!
//! An OS-thread backtrace only shows runtime worker threads parked in the
//! scheduler; it can't show *which `.await`* each suspended task is stuck on.
//! [`Handle::dump`] captures exactly that — the suspend-point backtrace of
//! every task on a runtime — which is what you need to find an async
//! deadlock (a cycle of tasks blocked on tokio `Mutex`/`Notify`/channels).
//!
//! Entry points:
//!   - [`dump_runtimes`]: render a dump on demand (e.g. from an HTTP handler).
//!   - [`spawn_sigusr2_dumper`]: log a full dump on every `SIGUSR2`
//!     (`kill -USR2 <pid>`). It runs on its own dedicated thread + runtime,
//!     separate from every runtime it dumps, so it still works when the
//!     service runtimes are wedged (their workers only need to be *parked*,
//!     which the scheduler can interrupt to collect the trace).
//!
//! Requires building with `--cfg tokio_unstable` (set in `.cargo/config.toml`).
//! Without it the dump body compiles to a short "unavailable" note so callers
//! build in either configuration.

use tokio::runtime::Handle;

/// Render a task dump for each `(name, handle)` runtime. `async` because
/// [`Handle::dump`] is async.
pub async fn dump_runtimes(handles: &[(String, Handle)]) -> String {
    #[cfg(all(tokio_unstable, tokio_taskdump))]
    {
        use std::fmt::Write as _;
        let mut out = String::new();
        for (name, handle) in handles {
            let _ = writeln!(out, "\n===== runtime: {name} =====");
            let dump = handle.dump().await;
            for (i, task) in dump.tasks().iter().enumerate() {
                let _ = writeln!(out, "--- task {i} ---\n{}", task.trace());
            }
        }
        out
    }
    #[cfg(not(all(tokio_unstable, tokio_taskdump)))]
    {
        let _ = handles;
        "tokio task dump unavailable: built without `--cfg tokio_unstable --cfg tokio_taskdump`\n"
            .to_string()
    }
}

/// Spawn a dedicated thread that logs a full task dump of `handles` every time
/// the process receives `SIGUSR2` (`kill -USR2 <pid>`).
///
/// The thread runs its own current-thread runtime — separate from every
/// runtime it dumps — so a dump succeeds even when the service runtimes are
/// deadlocked. Output goes to both `tracing` and stderr (in case the logging
/// pipeline is itself stuck). Call once per process, after the runtime(s)
/// exist; pass one entry per runtime (a single-runtime service passes just
/// `Handle::current()`).
pub fn spawn_sigusr2_dumper(handles: Vec<(String, Handle)>) {
    let spawned = std::thread::Builder::new()
        .name("tokio-task-dumper".to_string())
        .spawn(move || {
            let rt = match tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
            {
                Ok(rt) => rt,
                Err(e) => {
                    tracing::warn!("tokio-task-dumper: failed to build runtime: {e}");
                    return;
                }
            };
            rt.block_on(async move {
                let mut sig = match tokio::signal::unix::signal(
                    tokio::signal::unix::SignalKind::user_defined2(),
                ) {
                    Ok(s) => s,
                    Err(e) => {
                        tracing::warn!(
                            "tokio-task-dumper: failed to install SIGUSR2 handler: {e}"
                        );
                        return;
                    }
                };
                tracing::info!(
                    "tokio-task-dumper ready: `kill -USR2 <pid>` for a suspended-task dump"
                );
                while sig.recv().await.is_some() {
                    let text = dump_runtimes(&handles).await;
                    tracing::info!("TOKIO TASK DUMP (SIGUSR2) BEGIN\n{text}\nTOKIO TASK DUMP END");
                    eprintln!("TOKIO TASK DUMP (SIGUSR2) BEGIN\n{text}\nTOKIO TASK DUMP END");
                }
            });
        });
    if let Err(e) = spawned {
        tracing::warn!("tokio-task-dumper: failed to spawn thread: {e}");
    }
}
