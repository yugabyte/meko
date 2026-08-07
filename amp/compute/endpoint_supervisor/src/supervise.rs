//! Process supervision: the PID 1 behaviour.
//!
//! Children are started **in order**, each gated on a health probe before
//! the next begins, because the dependency chain is real: `compute_ctl`'s first
//! act is to fetch a basebackup from the pageserver, and the pageserver streams
//! WAL from the safekeeper. Starting them concurrently produces a compute that
//! fails its own startup.
//!
//! A crashed child is restarted with backoff, up to a budget within a rolling
//! window; past the budget the supervisor exits non-zero so the kubelet recreates
//! the whole pod rather than leaving a half-running endpoint behind. The budget is
//! what turns a child that cannot stay up into a pod restart instead of an
//! endless local restart loop.
//!
//! Reaping orphans is **not** this process's job: the image runs it under tini,
//! which is PID 1 proper and reaps. This process owns its own children only.

use std::collections::VecDeque;
use std::path::PathBuf;
use std::process::{Child, Command};
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

use anyhow::{Context, Result, bail};

/// Set by the signal handler; polled by the monitor loop. A handler may only
/// touch async-signal-safe state, so it does nothing but flip this.
static SHUTDOWN: AtomicBool = AtomicBool::new(false);

extern "C" fn on_term(_sig: libc::c_int) {
    SHUTDOWN.store(true, Ordering::SeqCst);
}

/// Installs handlers for SIGTERM/SIGINT so a pod delete stops the children
/// instead of having them killed out from under their datadirs.
///
/// Failure is an error, not a warning: without a handler a pod delete would kill
/// this process outright and leave its children running against a datadir nobody
/// is closing. For PID 1 that is the whole point of installing them.
pub fn install_signal_handlers() -> Result<()> {
    for sig in [libc::SIGTERM, libc::SIGINT] {
        // SAFETY: `on_term` only stores to an AtomicBool, which is
        // async-signal-safe.
        let prev = unsafe { libc::signal(sig, on_term as libc::sighandler_t) };
        if prev == libc::SIG_ERR {
            bail!(
                "installing the handler for signal {sig} failed: {}",
                std::io::Error::last_os_error()
            );
        }
    }
    Ok(())
}

pub fn shutdown_requested() -> bool {
    SHUTDOWN.load(Ordering::SeqCst)
}

/// How the supervisor decides a child came up, rather than merely started. A
/// process that exits immediately would otherwise let the next one start against
/// a dependency that is not there.
pub type HealthCheck = Box<dyn Fn() -> bool + Send>;

pub struct Service {
    pub name: String,
    argv: Vec<String>,
    cwd: Option<PathBuf>,
    env: Vec<(String, String)>,
    health: Option<HealthCheck>,
    child: Option<Child>,
    /// Restart instants inside the rolling window, oldest first.
    restarts: VecDeque<Instant>,
}

impl Service {
    pub fn new(name: &str, argv: Vec<String>) -> Self {
        Self {
            name: name.to_string(),
            argv,
            cwd: None,
            env: Vec::new(),
            health: None,
            child: None,
            restarts: VecDeque::new(),
        }
    }

    pub fn cwd(mut self, dir: impl Into<PathBuf>) -> Self {
        self.cwd = Some(dir.into());
        self
    }

    pub fn env(mut self, k: &str, v: &str) -> Self {
        self.env.push((k.to_string(), v.to_string()));
        self
    }

    pub fn health(mut self, check: HealthCheck) -> Self {
        self.health = Some(check);
        self
    }

    pub fn spawn(&mut self) -> Result<()> {
        let (program, args) = self
            .argv
            .split_first()
            .context("service argv must not be empty")?;
        let mut cmd = Command::new(program);
        cmd.args(args);
        if let Some(dir) = &self.cwd {
            cmd.current_dir(dir);
        }
        for (k, v) in &self.env {
            cmd.env(k, v);
        }
        // stdout/stderr are inherited on purpose: every child logs to the pod's
        // stream, which is what `kubectl logs` shows.
        let child = cmd
            .spawn()
            .with_context(|| format!("spawn {}", self.name))?;
        tracing::info!(service = %self.name, pid = child.id(), "started");
        self.child = Some(child);
        Ok(())
    }

    /// The argv, for tests that pin a child's command line against the flags the
    /// binary actually declares.
    #[cfg(test)]
    pub fn argv_for_test(&self) -> Vec<String> {
        self.argv.clone()
    }

    /// Whether this service holds a child handle, i.e. has been started and not
    /// stopped. Lets [`Supervisor::start_all`] run again after a service is added
    /// mid-startup, without respawning what is already up.
    pub fn started(&self) -> bool {
        self.child.is_some()
    }

    /// `Some(status)` once the child has exited, `None` while it is running.
    pub fn exited(&mut self) -> Result<Option<std::process::ExitStatus>> {
        match self.child.as_mut() {
            None => Ok(Some(std::process::ExitStatus::default())),
            Some(c) => Ok(c
                .try_wait()
                .with_context(|| format!("wait {}", self.name))?),
        }
    }

    /// Blocks until the health probe passes, or the timeout expires. A service
    /// with no probe is considered up as soon as it is spawned.
    pub fn wait_healthy(&mut self, timeout: Duration, interval: Duration) -> Result<()> {
        // Taken out for the duration: holding `&self.health` would conflict with
        // the `&mut self` that the exit check below needs.
        let Some(check) = self.health.take() else {
            return Ok(());
        };
        let result = self.wait_healthy_inner(&check, timeout, interval);
        self.health = Some(check);
        result
    }

    fn wait_healthy_inner(
        &mut self,
        check: &HealthCheck,
        timeout: Duration,
        interval: Duration,
    ) -> Result<()> {
        let deadline = Instant::now() + timeout;
        while Instant::now() < deadline {
            if shutdown_requested() {
                bail!("shutdown requested while waiting for {}", self.name);
            }
            // A child that has already exited will never become healthy; fail
            // now rather than burning the whole timeout.
            if let Some(status) = self.exited()? {
                bail!("{} exited during startup with {status}", self.name);
            }
            if check() {
                tracing::info!(service = %self.name, "healthy");
                return Ok(());
            }
            std::thread::sleep(interval);
        }
        bail!("{} did not become healthy within {timeout:?}", self.name)
    }

    /// Records a restart and returns how many have happened inside `window`.
    /// Entries older than the window are dropped, so a service that flaps slowly
    /// forever is not treated the same as one crash-looping.
    pub fn record_restart(&mut self, now: Instant, window: Duration) -> usize {
        while let Some(front) = self.restarts.front() {
            if now.duration_since(*front) > window {
                self.restarts.pop_front();
            } else {
                break;
            }
        }
        self.restarts.push_back(now);
        self.restarts.len()
    }

    /// SIGTERM, then SIGKILL if it does not go quietly within `grace`, so a child
    /// gets a chance to flush and close its datadir before it is killed outright.
    pub fn stop(&mut self, grace: Duration) {
        let Some(child) = self.child.as_mut() else {
            return;
        };
        let pid = child.id() as libc::pid_t;
        // SAFETY: `pid` is our own child, and SIGTERM is a valid signal.
        unsafe {
            libc::kill(pid, libc::SIGTERM);
        }
        let deadline = Instant::now() + grace;
        while Instant::now() < deadline {
            match child.try_wait() {
                Ok(Some(_)) => {
                    tracing::info!(service = %self.name, "stopped");
                    self.child = None;
                    return;
                }
                Ok(None) => std::thread::sleep(Duration::from_millis(100)),
                Err(_) => break,
            }
        }
        tracing::warn!(service = %self.name, "did not exit on SIGTERM; killing");
        let _ = child.kill();
        let _ = child.wait();
        self.child = None;
    }
}

/// Restart and health-gate timings. [`Policy::default`] is what the pod runs
/// with; the fields are public so tests can shorten every duration.
#[derive(Debug, Clone, Copy)]
pub struct Policy {
    pub backoff: Duration,
    pub max_restarts: usize,
    pub window: Duration,
    pub health_timeout: Duration,
    pub health_interval: Duration,
    pub stop_grace: Duration,
}

impl Default for Policy {
    fn default() -> Self {
        Self {
            backoff: Duration::from_secs(2),
            max_restarts: 5,
            window: Duration::from_secs(180),
            health_timeout: Duration::from_secs(120),
            health_interval: Duration::from_millis(500),
            stop_grace: Duration::from_secs(10),
        }
    }
}

impl Policy {
    /// The default, with the three knobs the chart exposes applied. A malformed
    /// value keeps the default rather than failing the pod: these are tuning, and
    /// an endpoint that will not start is worse than one running the defaults.
    pub fn from_env() -> Self {
        let mut p = Self::default();
        if let Some(v) = env_secs("SUPERVISOR_RESTART_BACKOFF_SECONDS") {
            p.backoff = v;
        }
        if let Some(v) = env_usize("SUPERVISOR_MAX_RESTARTS") {
            p.max_restarts = v;
        }
        if let Some(v) = env_secs("SUPERVISOR_RESTART_WINDOW_SECONDS") {
            p.window = v;
        }
        p
    }
}

fn env_str(name: &str) -> Option<String> {
    std::env::var(name).ok().filter(|v| !v.is_empty())
}

fn env_secs(name: &str) -> Option<Duration> {
    match env_str(name)?.parse() {
        Ok(n) => Some(Duration::from_secs(n)),
        Err(_) => {
            tracing::warn!(name, "not a number of seconds; keeping the default");
            None
        }
    }
}

fn env_usize(name: &str) -> Option<usize> {
    match env_str(name)?.parse() {
        Ok(n) => Some(n),
        Err(_) => {
            tracing::warn!(name, "not a number; keeping the default");
            None
        }
    }
}

/// Why the monitor loop returned. `BudgetExceeded` is the one the caller must
/// turn into a non-zero exit, so the kubelet recreates the pod.
#[derive(Debug, PartialEq, Eq)]
pub enum Outcome {
    Shutdown,
    BudgetExceeded { service: String, restarts: usize },
}

pub struct Supervisor {
    services: Vec<Service>,
    policy: Policy,
}

impl Supervisor {
    pub fn new(policy: Policy) -> Self {
        Self {
            services: Vec::new(),
            policy,
        }
    }

    pub fn add(&mut self, service: Service) {
        self.services.push(service);
    }

    /// Starts every not-yet-started service in order, gating each on its health
    /// probe. A failure here stops whatever already came up, so a failed start
    /// does not leave half a stack running.
    ///
    /// Already-running services are skipped, so this can be called again after a
    /// service is added part-way through startup — which is what the
    /// pageserver/safekeeper handshake before `compute_ctl` needs.
    pub fn start_all(&mut self) -> Result<()> {
        for i in 0..self.services.len() {
            if self.services[i].started() {
                continue;
            }
            let (timeout, interval) = (self.policy.health_timeout, self.policy.health_interval);
            let res = (|| -> Result<()> {
                let svc = &mut self.services[i];
                // Retried: a `fork`/`exec` that fails under momentary memory
                // pressure should not cost a whole pod restart at boot.
                crate::retry::retry("spawn", &crate::retry::Backoff::spawn(), || svc.spawn())?;
                svc.wait_healthy(timeout, interval)
            })();
            if let Err(e) = res {
                let failed = self.services[i].name.clone();
                self.stop_all();
                return Err(e).with_context(|| format!("bringing up {failed}"));
            }
        }
        Ok(())
    }

    /// Restarts crashed children until shutdown or a service exhausts its budget.
    pub fn run(&mut self, poll: Duration) -> Result<Outcome> {
        loop {
            if shutdown_requested() {
                self.stop_all();
                return Ok(Outcome::Shutdown);
            }
            for i in 0..self.services.len() {
                match self.services[i].exited() {
                    // Still running.
                    Ok(None) => continue,
                    Ok(Some(_)) => {}
                    // Whether the child is alive is unknown, so do neither of the
                    // two things that could be wrong: restarting a process that is
                    // actually running, or exiting the pod over a failed
                    // `try_wait`. Re-check on the next tick instead.
                    Err(e) => {
                        tracing::warn!(
                            service = %self.services[i].name,
                            error = %e,
                            "could not determine status; re-checking next tick"
                        );
                        continue;
                    }
                }
                let name = self.services[i].name.clone();
                let count = self.services[i].record_restart(Instant::now(), self.policy.window);
                if count > self.policy.max_restarts {
                    tracing::error!(
                        service = %name,
                        restarts = count,
                        window_secs = self.policy.window.as_secs(),
                        "exceeded its restart budget; exiting so the pod is recreated"
                    );
                    self.stop_all();
                    return Ok(Outcome::BudgetExceeded {
                        service: name,
                        restarts: count,
                    });
                }
                tracing::warn!(service = %name, restarts = count, "exited; restarting");
                std::thread::sleep(self.policy.backoff);
                if let Err(e) = self.services[i].spawn() {
                    // A failed respawn must not escape this loop: propagating it
                    // would exit the pod on the first transient `fork`/`exec`
                    // failure, skipping the very budget that exists to bound this.
                    // The child stays `None`, so the next tick counts another
                    // restart and tries again — and a permanent failure still
                    // exhausts the budget and exits, just by the intended route.
                    tracing::error!(
                        service = %name,
                        error = %e,
                        "respawn failed; retrying against the restart budget"
                    );
                }
            }
            std::thread::sleep(poll);
        }
    }

    /// Stops children in reverse start order, so a dependency outlives its
    /// dependents.
    pub fn stop_all(&mut self) {
        let grace = self.policy.stop_grace;
        for s in self.services.iter_mut().rev() {
            s.stop(grace);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sh(script: &str) -> Vec<String> {
        vec!["/bin/sh".into(), "-c".into(), script.into()]
    }

    fn fast_policy() -> Policy {
        Policy {
            backoff: Duration::from_millis(1),
            max_restarts: 2,
            window: Duration::from_secs(60),
            health_timeout: Duration::from_millis(500),
            health_interval: Duration::from_millis(10),
            stop_grace: Duration::from_millis(200),
        }
    }

    #[test]
    fn a_service_starts_and_reports_exit() {
        let mut s = Service::new("true", sh("exit 0"));
        s.spawn().unwrap();
        let deadline = Instant::now() + Duration::from_secs(5);
        while Instant::now() < deadline {
            if s.exited().unwrap().is_some() {
                return;
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        panic!("child never reported exit");
    }

    /// The health gate is what keeps the ordering meaningful: a service that
    /// exits during startup must fail immediately rather than burn the timeout.
    #[test]
    fn a_service_that_dies_during_startup_fails_fast() {
        let mut s = Service::new("dies", sh("exit 3")).health(Box::new(|| false));
        let started = Instant::now();
        let err = s
            .wait_healthy(Duration::from_secs(30), Duration::from_millis(10))
            .expect_err("must not report healthy");
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "should fail fast, took {:?}",
            started.elapsed()
        );
        assert!(
            format!("{err:#}").contains("exited during startup"),
            "{err:#}"
        );
    }

    #[test]
    fn health_probe_gates_startup() {
        let mut s = Service::new("sleeper", sh("sleep 30")).health(Box::new(|| true));
        s.spawn().unwrap();
        s.wait_healthy(Duration::from_secs(2), Duration::from_millis(10))
            .unwrap();
        s.stop(Duration::from_millis(500));
    }

    #[test]
    fn an_unhealthy_service_times_out() {
        let mut s = Service::new("never", sh("sleep 30")).health(Box::new(|| false));
        s.spawn().unwrap();
        let err = s
            .wait_healthy(Duration::from_millis(200), Duration::from_millis(10))
            .expect_err("must time out");
        assert!(format!("{err:#}").contains("did not become healthy"));
        s.stop(Duration::from_millis(500));
    }

    /// The budget is what converts a crash-looping child into a pod restart, so
    /// the accounting is worth pinning: entries outside the window do not count.
    #[test]
    fn restart_budget_only_counts_inside_the_window() {
        let mut s = Service::new("flapper", sh("exit 1"));
        let t0 = Instant::now();
        let window = Duration::from_secs(60);
        assert_eq!(s.record_restart(t0, window), 1);
        assert_eq!(s.record_restart(t0 + Duration::from_secs(1), window), 2);
        // Well outside the window: the earlier two are forgotten.
        assert_eq!(s.record_restart(t0 + Duration::from_secs(600), window), 1);
    }

    /// End to end: a child that keeps dying trips the budget and the loop returns
    /// the outcome the caller turns into a non-zero exit.
    #[test]
    fn a_crash_looping_service_exhausts_its_budget() {
        let mut sup = Supervisor::new(fast_policy());
        sup.add(Service::new("crasher", sh("exit 1")));
        sup.start_all().unwrap();
        match sup.run(Duration::from_millis(5)).unwrap() {
            Outcome::BudgetExceeded { service, restarts } => {
                assert_eq!(service, "crasher");
                assert!(restarts > fast_policy().max_restarts, "restarts={restarts}");
            }
            other => panic!("expected the budget to trip, got {other:?}"),
        }
    }

    /// A respawn that can never succeed must still exit by the budget, not by
    /// propagating the spawn error straight out of the loop. As PID 1 the
    /// difference matters: the budget is what bounds the restarts, and an error
    /// escaping `run` skips it entirely.
    #[test]
    fn an_unspawnable_service_exits_by_the_budget_not_by_an_error() {
        let mut sup = Supervisor::new(fast_policy());
        // Spawned once so start_all succeeds, then made unspawnable, so every
        // respawn in the monitor loop fails at `exec`.
        sup.add(Service::new("gone", sh("exit 0")));
        sup.start_all().unwrap();
        sup.services[0].argv = vec!["/nonexistent/binary".into()];
        match sup
            .run(Duration::from_millis(5))
            .expect("a failed respawn must not propagate as an error")
        {
            Outcome::BudgetExceeded { service, restarts } => {
                assert_eq!(service, "gone");
                assert!(restarts > fast_policy().max_restarts, "restarts={restarts}");
            }
            other => panic!("expected the budget to trip, got {other:?}"),
        }
    }

    /// The chart sets these three, so ignoring them would silently discard
    /// operator tuning — which is what happened until the GKE run surfaced it.
    #[test]
    fn the_policy_reads_the_knobs_the_chart_sets() {
        // SAFETY: single-threaded test; names are unique to it.
        unsafe {
            std::env::set_var("SUPERVISOR_RESTART_BACKOFF_SECONDS", "7");
            std::env::set_var("SUPERVISOR_MAX_RESTARTS", "9");
            std::env::set_var("SUPERVISOR_RESTART_WINDOW_SECONDS", "600");
        }
        let p = Policy::from_env();
        assert_eq!(p.backoff, Duration::from_secs(7));
        assert_eq!(p.max_restarts, 9);
        assert_eq!(p.window, Duration::from_secs(600));

        // A malformed value must not take the pod down over a tuning knob.
        unsafe { std::env::set_var("SUPERVISOR_MAX_RESTARTS", "lots") };
        assert_eq!(
            Policy::from_env().max_restarts,
            Policy::default().max_restarts
        );
        unsafe {
            std::env::remove_var("SUPERVISOR_RESTART_BACKOFF_SECONDS");
            std::env::remove_var("SUPERVISOR_MAX_RESTARTS");
            std::env::remove_var("SUPERVISOR_RESTART_WINDOW_SECONDS");
        }
    }

    /// The handshake before the compute starts means `start_all` runs twice. The
    /// second call must start only what was added, not respawn a running child —
    /// which would orphan the first process and double-bind its port.
    #[test]
    fn a_second_start_all_only_starts_what_was_added() {
        let mut sup = Supervisor::new(fast_policy());
        sup.add(Service::new("first", sh("sleep 30")).health(Box::new(|| true)));
        sup.start_all().unwrap();
        let first_pid = sup.services[0].child.as_ref().unwrap().id();

        sup.add(Service::new("second", sh("sleep 30")).health(Box::new(|| true)));
        sup.start_all().unwrap();

        assert_eq!(
            sup.services[0].child.as_ref().unwrap().id(),
            first_pid,
            "the running service was respawned"
        );
        assert!(sup.services[1].started());
        sup.stop_all();
    }

    /// The handshake between the two `start_all` calls can fail, and when it does
    /// the caller must stop what is already up. `Supervisor` does not reap on
    /// drop, so a bare `?` there leaves three daemons running against datadirs
    /// nobody is closing — the situation the signal handlers exist to prevent.
    #[test]
    fn stop_all_reaps_a_partially_started_stack() {
        let mut sup = Supervisor::new(fast_policy());
        sup.add(Service::new("a", sh("sleep 30")).health(Box::new(|| true)));
        sup.add(Service::new("b", sh("sleep 30")).health(Box::new(|| true)));
        sup.start_all().unwrap();
        assert!(sup.services.iter().all(|s| s.started()));

        // What the caller does when the handshake errors.
        sup.stop_all();

        for s in &mut sup.services {
            assert!(!s.started(), "{} was left running", s.name);
        }
    }

    /// A failed bring-up must not leave the services that already started behind.
    #[test]
    fn a_failed_start_stops_what_already_came_up() {
        let mut sup = Supervisor::new(fast_policy());
        sup.add(Service::new("ok", sh("sleep 30")).health(Box::new(|| true)));
        sup.add(Service::new("bad", sh("sleep 30")).health(Box::new(|| false)));
        let err = sup.start_all().expect_err("second service must fail");
        assert!(format!("{err:#}").contains("bringing up bad"), "{err:#}");
        // Nothing is left running: every child handle was cleared by stop_all.
        for s in &mut sup.services {
            assert!(s.child.is_none(), "{} was left running", s.name);
        }
    }

    #[test]
    fn stop_all_is_reverse_order() {
        let mut sup = Supervisor::new(fast_policy());
        for n in ["first", "second", "third"] {
            sup.add(Service::new(n, sh("sleep 30")));
        }
        sup.start_all().unwrap();
        sup.stop_all();
        for s in &mut sup.services {
            assert!(s.child.is_none(), "{} still running", s.name);
        }
    }
}
