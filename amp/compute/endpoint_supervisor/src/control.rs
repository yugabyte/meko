//! The supervisor's control socket: per-service lifecycle, for a caller that
//! launched this supervisor and still wants to stop one daemon.
//!
//! A caller cannot stop one of these daemons by signalling it: the monitor loop
//! sees the death and restarts it. It asks here instead, and the supervisor
//! records the service as not *wanted* until asked again
//! ([`crate::supervise::Service`]). `yb_amp_local` exposes this as `pageserver
//! stop|start|restart` and the same for the safekeeper; the integration suite
//! restarts individual daemons mid-scenario.
//!
//! A **Unix socket** rather than an HTTP port: there is no port to allocate or
//! collide, and filesystem permissions are the whole access-control story for
//! something that can stop a database. It sits beside the endpoint's other
//! run-time state where the path allows — see [`socket_path`].
//!
//! The protocol is one request line in, one response line out:
//!
//! ```text
//! disable <service>              -> ok | err: <reason>
//! start <service> [<seconds>]    -> ok | err: <reason>
//! stop <service> [immediate]     -> ok | err: <reason>
//! restart <service> [immediate]  -> ok | err: <reason>
//! status                         -> <name> running|down|stopped   (one per line, then a blank line)
//! ```
//!
//! `immediate` is the launcher's `--mode immediate`: SIGQUIT rather than
//! SIGTERM, so the daemon stops now instead of taking a shutdown checkpoint
//! first. Both are signals it implements; neither is a kill.
//!
//! `disable` stops supervising without signalling, for a caller that will take
//! the process down its own way — a compute is stopped through Postgres, and
//! `compute_ctl` turns every signal it gets into a *fast* shutdown, so asking it
//! to stop can never produce a crash stop.
//!
//! `<seconds>` on a start is how long the caller will wait for the service's
//! health probe. The caller has to say, because it is the one that gives up: a
//! shorter deadline on its side turns a start that is still in progress into a
//! reported failure with nothing to explain it.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::{Arc, Mutex};

use anyhow::{Context, Result};
use camino::{Utf8Path, Utf8PathBuf};

use crate::supervise::Supervisor;

/// The socket's name inside the run directory.
pub const SOCKET_NAME: &str = "supervisor.sock";

/// The most a Unix socket path can be. `sockaddr_un.sun_path` is 108 bytes on
/// Linux and the kernel wants a NUL in there, so 107 is the real ceiling — a
/// longer path fails `bind` with `path must be shorter than SUN_LEN` rather than
/// being truncated.
const SUN_PATH_MAX: usize = 107;

/// Where the control socket lives: beside the endpoint's other run-time state,
/// unless that path is too long to be a socket.
///
/// The pod's run directory is `/data`, so it always fits. `yb_amp_local` puts one
/// per endpoint under the repo directory, wherever the developer or the CI runner
/// checked the tree out, which is deep enough to blow the limit — and a bind that
/// fails there fails at the end of a successful startup, with everything else
/// already up.
///
/// The fallback is derived from the run directory, so both ends compute the same
/// name without passing it around, and the uid keeps two users on one host from
/// colliding on a path neither can remove.
pub fn socket_path(run_dir: &Utf8Path) -> Utf8PathBuf {
    let beside = run_dir.join(SOCKET_NAME);
    if beside.as_str().len() <= SUN_PATH_MAX {
        return beside;
    }
    // SAFETY: getuid cannot fail and touches no memory.
    let uid = unsafe { libc::getuid() };
    Utf8PathBuf::from(format!(
        "/tmp/yb-amp-{uid}-{:x}.sock",
        md5::compute(run_dir.as_str())
    ))
}

/// The supervisor, shared with the control thread.
///
/// The monitor loop holds this lock only for the length of one poll pass, and a
/// control request only for the length of one stop or start, so neither starves
/// the other in practice — and a request that has to wait for a health probe is
/// waiting on the thing it asked for.
pub type Shared = Arc<Mutex<Supervisor>>;

/// Bind the control socket, replacing a stale one left by a previous run.
///
/// A leftover socket file is the normal case after a hard kill: the path exists,
/// nothing is listening, and `bind` fails with `AddrInUse`. Removing it is safe
/// because the run directory belongs to exactly one supervisor.
pub fn bind(run_dir: &Utf8Path) -> Result<UnixListener> {
    let path = socket_path(run_dir);
    if path.exists() {
        std::fs::remove_file(&path)
            .with_context(|| format!("removing a stale control socket at {path}"))?;
    }
    let listener = UnixListener::bind(&path)
        .with_context(|| format!("binding the control socket at {path}"))?;
    tracing::info!(socket = %path, "control socket listening");
    Ok(listener)
}

/// Serve control requests until the process stops. One connection, one command.
pub fn serve(listener: UnixListener, sup: Shared) {
    for conn in listener.incoming() {
        if crate::supervise::shutdown_requested() {
            return;
        }
        match conn {
            Ok(stream) => {
                if let Err(e) = handle(stream, &sup) {
                    tracing::warn!("control request failed: {e:#}");
                }
            }
            // A failed accept is not worth taking the pod down for; the daemons
            // are running and only the control surface is affected.
            Err(e) => tracing::warn!("control socket accept failed: {e}"),
        }
    }
}

/// The monitor loop, taking the lock one pass at a time so a control request is
/// never queued behind the whole life of the process.
pub fn run_shared(sup: &Shared, poll: std::time::Duration) -> Result<crate::supervise::Outcome> {
    loop {
        {
            let mut guard = sup
                .lock()
                .map_err(|_| anyhow::anyhow!("the supervisor lock is poisoned"))?;
            if let Some(outcome) = guard.poll_once()? {
                return Ok(outcome);
            }
        }
        std::thread::sleep(poll);
    }
}

fn handle(stream: UnixStream, sup: &Shared) -> Result<()> {
    let mut reader = BufReader::new(stream.try_clone().context("cloning the control stream")?);
    let mut line = String::new();
    reader.read_line(&mut line).context("reading the request")?;

    let response = dispatch(line.trim(), sup);
    let mut stream = stream;
    stream
        .write_all(response.as_bytes())
        .context("writing the response")?;
    Ok(())
}

fn dispatch(line: &str, sup: &Shared) -> String {
    let mut parts = line.split_whitespace();
    let verb = parts.next().unwrap_or_default();
    let target = parts.next();

    let mut sup = match sup.lock() {
        Ok(s) => s,
        // A poisoned lock means the monitor thread panicked holding it. Say so
        // rather than panicking this thread too.
        Err(_) => return "err: supervisor lock poisoned\n".into(),
    };

    // A third word, when present, qualifies the verb: the stop mode for a stop,
    // and how many seconds the caller will wait for a start. Anything else is
    // ignored — a typo must not turn a drain into a kill, or a wait into a
    // no-wait.
    let qualifier = parts.next();
    let drain = qualifier != Some("immediate");
    let budget = qualifier
        .and_then(|q| q.parse().ok())
        .map(std::time::Duration::from_secs);

    let need_target = |t: Option<&str>| t.map(str::to_string);

    match (verb, need_target(target)) {
        ("status", _) => {
            let mut out = sup.status();
            out.push('\n');
            out
        }
        ("disable", Some(name)) => reply(sup.disable(&name)),
        ("stop", Some(name)) => reply(sup.stop_one(&name, drain)),
        ("start", Some(name)) => reply(sup.start_one(&name, budget)),
        ("restart", Some(name)) => reply(
            sup.stop_one(&name, drain)
                .and_then(|()| sup.start_one(&name, budget)),
        ),
        ("stop" | "start" | "restart" | "disable", None) => {
            format!("err: {verb} needs a service name\n")
        }
        ("", _) => "err: empty request\n".into(),
        _ => format!("err: unknown command {verb:?}\n"),
    }
}

fn reply(res: Result<()>) -> String {
    match res {
        Ok(()) => "ok\n".into(),
        // `{e:#}` so the caller gets the context chain on one line — this is the
        // only place the failure is explained to whoever asked.
        Err(e) => {
            format!("err: {e:#}\n")
                .replace('\n', " ")
                .trim()
                .to_string()
                + "\n"
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::supervise::{Policy, Service};

    fn sh(script: &str) -> Vec<String> {
        vec!["/bin/sh".into(), "-c".into(), script.into()]
    }

    fn shared_with(svc: Service) -> Shared {
        shared_with_policy(svc, Policy::default())
    }

    fn shared_with_policy(svc: Service, policy: Policy) -> Shared {
        let mut sup = Supervisor::new(policy);
        sup.add(svc);
        Arc::new(Mutex::new(sup))
    }

    #[test]
    fn unknown_commands_and_missing_names_are_errors_not_panics() {
        let sup = shared_with(Service::new("x", sh("sleep 60")));
        assert!(dispatch("frobnicate x", &sup).starts_with("err: unknown command"));
        assert!(dispatch("stop", &sup).starts_with("err: stop needs a service name"));
        assert!(dispatch("", &sup).starts_with("err: empty request"));
        assert!(dispatch("stop nosuch", &sup).starts_with("err:"));
    }

    /// The point of the whole module: a service stopped through the socket must
    /// report `stopped`, not `down`, so the monitor loop leaves it alone.
    #[test]
    fn a_stopped_service_is_not_merely_dead() {
        let mut svc = Service::new("pageserver", sh("sleep 60"));
        svc.spawn().unwrap();
        let sup = shared_with(svc);

        assert_eq!(dispatch("stop pageserver", &sup), "ok\n");
        let status = dispatch("status", &sup);
        assert!(
            status.contains("pageserver stopped"),
            "want a deliberate stop, got: {status}"
        );
    }

    /// The third word reaches the supervisor as the stop mode, all the way to
    /// the signal the stop sends.
    #[test]
    fn the_stop_mode_reaches_the_supervisor() {
        let mut svc = Service::new("pageserver", sh("sleep 60"));
        svc.spawn().unwrap();
        let policy = Policy {
            stop_grace: std::time::Duration::from_secs(20),
            ..Policy::default()
        };
        let sup = shared_with_policy(svc, policy);

        assert_eq!(dispatch("stop pageserver immediate", &sup), "ok\n");

        let mut guard = sup.lock().unwrap();
        assert_eq!(
            guard.service_for_test("pageserver").last_stop_signal,
            Some(libc::SIGQUIT),
            "the request's mode must reach the signal the stop sends"
        );
        drop(guard);
        assert!(dispatch("status", &sup).contains("pageserver stopped"));
    }

    #[test]
    fn status_reports_a_running_service() {
        let mut svc = Service::new("safekeeper", sh("sleep 60"));
        svc.spawn().unwrap();
        let sup = shared_with(svc);
        let status = dispatch("status", &sup);
        assert!(status.contains("safekeeper running"), "{status}");
        let _ = dispatch("stop safekeeper", &sup);
    }

    /// The caller's budget has to reach the supervisor, or the two give up at
    /// different moments: the caller reports a timeout it cannot explain while
    /// the supervisor is still starting the service, and succeeds a moment later.
    #[test]
    fn a_start_budget_reaches_the_health_gate() {
        // Never healthy, so the only way out is the budget expiring.
        let svc = Service::new("pageserver", sh("sleep 60")).health(Box::new(|| false));
        let sup = shared_with_policy(
            svc,
            Policy {
                // The supervisor's own gate is long; the caller's is what should
                // decide, and it is much shorter.
                health_timeout: std::time::Duration::from_secs(300),
                health_interval: std::time::Duration::from_millis(10),
                ..Policy::default()
            },
        );

        let started = std::time::Instant::now();
        let reply = dispatch("start pageserver 1", &sup);
        assert!(
            started.elapsed() < std::time::Duration::from_secs(30),
            "waited {:?}: the caller's budget was ignored",
            started.elapsed()
        );
        // And the reason comes back, rather than the caller inventing one from a
        // socket timeout.
        assert!(reply.contains("did not become healthy"), "{reply}");
    }

    /// `disable` has to leave the process alone. The caller uses it to take the
    /// process down its own way — a compute is stopped through Postgres, and only
    /// that decides whether a clean shutdown's state survives — so a `disable`
    /// that signalled would make the mode it is reaching for unreachable.
    #[test]
    fn disable_stops_supervising_without_signalling() {
        let mut svc = Service::new("compute_ctl", sh("sleep 60"));
        svc.spawn().unwrap();
        let sup = shared_with(svc);

        assert_eq!(dispatch("disable compute_ctl", &sup), "ok\n");

        // Still running: the caller has not killed it yet.
        let mut guard = sup.lock().unwrap();
        assert!(
            matches!(guard.service_for_test("compute_ctl").exited(), Ok(None)),
            "disable must not signal the child"
        );
        // And no longer wanted, so the monitor loop leaves the caller's kill alone.
        assert!(!guard.service_for_test("compute_ctl").wanted());
        guard.stop_all();
    }

    /// Binding over a socket file left by a hard kill is the normal restart
    /// path, not an error.
    #[test]
    fn bind_replaces_a_stale_socket() {
        let dir = camino_tempfile::tempdir().unwrap();
        let path = socket_path(dir.path());
        std::fs::write(&path, b"stale").unwrap();
        let listener = bind(dir.path()).expect("a stale socket must not block bind");
        drop(listener);
    }

    /// A run directory deep enough to exceed `sun_path` must still get a
    /// bindable socket. Without this the endpoint comes all the way up and then
    /// loses its control surface on the last step, which is how it first showed
    /// up: `path must be shorter than SUN_LEN`, after every service was healthy.
    #[test]
    fn a_long_run_directory_still_gets_a_bindable_socket() {
        let deep = Utf8PathBuf::from(format!("/{}/endpoints/main", "x".repeat(120)));
        let path = socket_path(&deep);
        assert!(
            path.as_str().len() <= SUN_PATH_MAX,
            "{path} is still too long to bind"
        );
        // Derived from the run directory alone, so the launcher computes the
        // same name without being told.
        assert_eq!(path, socket_path(&deep));
        assert_ne!(path, socket_path(Utf8Path::new(&format!("{deep}2"))));

        // A short one stays where an operator would look for it.
        assert_eq!(
            socket_path(Utf8Path::new("/data")),
            Utf8PathBuf::from("/data/supervisor.sock")
        );
    }

    /// The fallback has to actually bind, not merely be short: it is a real path
    /// in a real directory, and `/tmp` is the one place both a pod and a
    /// developer's machine can be relied on to have.
    #[test]
    fn the_fallback_socket_binds_and_answers() {
        let deep = Utf8PathBuf::from(format!("/{}/endpoints/main", "y".repeat(120)));
        let path = socket_path(&deep);
        let _ = std::fs::remove_file(&path);

        let listener = bind(&deep).expect("the fallback path must bind");
        let sup = shared_with(Service::new("pageserver", sh("sleep 60")));
        std::thread::spawn(move || serve(listener, sup));

        let mut stream = UnixStream::connect(&path).expect("connect to the fallback socket");
        stream.write_all(b"status\n").unwrap();
        let mut reply = String::new();
        BufReader::new(&stream).read_line(&mut reply).unwrap();
        assert!(reply.contains("pageserver"), "{reply}");
        let _ = std::fs::remove_file(&path);
    }
}
