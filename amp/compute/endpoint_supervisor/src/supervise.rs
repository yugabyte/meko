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

/// Set when the shutdown arrived as SIGQUIT: stop the children without waiting
/// out the drain.
static IMMEDIATE: AtomicBool = AtomicBool::new(false);

extern "C" fn on_term(_sig: libc::c_int) {
    SHUTDOWN.store(true, Ordering::SeqCst);
}

extern "C" fn on_quit(_sig: libc::c_int) {
    IMMEDIATE.store(true, Ordering::SeqCst);
    SHUTDOWN.store(true, Ordering::SeqCst);
}

/// Installs handlers for SIGTERM/SIGINT/SIGQUIT so a delete stops the children
/// instead of having them killed out from under their datadirs.
///
/// Failure is an error, not a warning: without a handler a pod delete would kill
/// this process outright and leave its children running against a datadir nobody
/// is closing. For PID 1 that is the whole point of installing them.
///
/// SIGQUIT is what `yb_amp_local` sends for an immediate stop, and its default
/// action is to terminate — so leaving it unhandled would make that path the one
/// that orphans a pageserver.
pub fn install_signal_handlers() -> Result<()> {
    for (sig, handler) in [
        (libc::SIGTERM, on_term as libc::sighandler_t),
        (libc::SIGINT, on_term as libc::sighandler_t),
        (libc::SIGQUIT, on_quit as libc::sighandler_t),
    ] {
        // SAFETY: both handlers only store to an AtomicBool, which is
        // async-signal-safe.
        let prev = unsafe { libc::signal(sig, handler) };
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

/// Whether the shutdown in flight asked for speed over a clean drain.
pub fn shutdown_is_immediate() -> bool {
    IMMEDIATE.load(Ordering::SeqCst)
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
    /// Set on the next successful spawn only; see [`Self::env_once`].
    env_once: Vec<(String, String)>,
    /// Inherited variables this child must not see, such as the YugabyteDB
    /// password for a child that runs as the compute user.
    env_remove: Vec<String>,
    run_as: Option<crate::identity::RunAs>,
    health: Option<HealthCheck>,
    child: Option<Child>,
    /// Restart instants inside the rolling window, oldest first.
    restarts: VecDeque<Instant>,
    /// Whether this service is *supposed* to be running.
    ///
    /// The monitor loop restarts anything it finds dead, which is right for a
    /// crash and wrong for a deliberate stop — without this, asking the
    /// supervisor to stop the pageserver just starts a fight it wins. Set false
    /// by an operator stop and true again by an operator start; a crash never
    /// touches it.
    wanted: bool,
    /// Where this child's output goes. `None` inherits, which in the pod means
    /// the stream `kubectl logs` shows.
    log_file: Option<PathBuf>,
    /// Which signal the last stop sent, so a test can assert the choice without
    /// depending on how promptly a loaded machine delivers it.
    #[cfg(test)]
    pub last_stop_signal: Option<libc::c_int>,
}

impl Service {
    pub fn new(name: &str, argv: Vec<String>) -> Self {
        Self {
            name: name.to_string(),
            argv,
            cwd: None,
            env: Vec::new(),
            env_once: Vec::new(),
            env_remove: Vec::new(),
            run_as: None,
            health: None,
            child: None,
            restarts: VecDeque::new(),
            wanted: true,
            log_file: None,
            #[cfg(test)]
            last_stop_signal: None,
        }
    }

    /// Is this service supposed to be running?
    pub fn wanted(&self) -> bool {
        self.wanted
    }

    pub fn cwd(mut self, dir: impl Into<PathBuf>) -> Self {
        self.cwd = Some(dir.into());
        self
    }

    pub fn env(mut self, k: &str, v: &str) -> Self {
        self.env.push((k.to_string(), v.to_string()));
        self
    }

    /// A variable for the first spawn only, not for a restart: something that
    /// describes the bring-up the service was started in.
    pub fn env_once(mut self, k: &str, v: &str) -> Self {
        self.env_once.push((k.to_string(), v.to_string()));
        self
    }

    pub fn without_env(mut self, names: impl IntoIterator<Item = String>) -> Self {
        self.env_remove.extend(names);
        self
    }

    pub fn run_as(mut self, id: Option<crate::identity::RunAs>) -> Self {
        self.run_as = id;
        self
    }

    pub fn health(mut self, check: HealthCheck) -> Self {
        self.health = Some(check);
        self
    }

    /// Send this child's output to its own file, appending across restarts.
    /// `None` inherits this process's stdout.
    ///
    /// For a caller that runs several endpoints on one host: one interleaved
    /// stream is the right answer in a pod and the wrong one on a developer's
    /// machine, where each daemon's log is read on its own.
    pub fn logging_to(mut self, path: Option<impl Into<PathBuf>>) -> Self {
        self.log_file = path.map(Into::into);
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
        for k in &self.env_remove {
            cmd.env_remove(k);
        }
        for (k, v) in self.env.iter().chain(&self.env_once) {
            cmd.env(k, v);
        }
        if let Some(id) = self.run_as {
            use std::os::unix::process::CommandExt;
            // std clears the supplementary groups when root sets a uid.
            cmd.uid(id.uid).gid(id.gid);
            // SAFETY: harden_child makes only async-signal-safe calls.
            unsafe { cmd.pre_exec(crate::identity::harden_child) };
        }
        // Seeded at every spawn, not just the first: a child restarted mid-lease
        // arms itself before the supervisor can push anything over HTTP, and one
        // that came up holding no token would refuse its first write.
        let token = crate::fence::current_token();
        if token != 0 {
            cmd.env(utils::write_fence::INITIAL_TOKEN_ENV, token.to_string());
        }
        // With no log file the child inherits this process's stdout, which in the
        // pod is the stream `kubectl logs` shows.
        if let Some(path) = &self.log_file {
            // Append, so a restart adds to the log rather than truncating the
            // evidence of why the last one died.
            let file = std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(path)
                .with_context(|| format!("open {} for {}", path.display(), self.name))?;
            let err = file
                .try_clone()
                .with_context(|| format!("duplicate the log handle for {}", self.name))?;
            cmd.stdout(file).stderr(err);
        }
        let child = cmd
            .spawn()
            .with_context(|| format!("spawn {}", self.name))?;
        tracing::info!(service = %self.name, pid = child.id(), "started");
        self.child = Some(child);
        self.env_once.clear();
        Ok(())
    }

    /// Where this child's output goes, for tests that pin the paths the launcher
    /// reads.
    #[cfg(test)]
    pub fn log_file_for_test(&self) -> Option<&PathBuf> {
        self.log_file.as_ref()
    }

    /// The argv, for tests that pin a child's command line against the flags the
    /// binary actually declares.
    #[cfg(test)]
    pub fn argv_for_test(&self) -> Vec<String> {
        self.argv.clone()
    }

    /// Who this child runs as, where, and without which inherited variables.
    #[cfg(test)]
    pub fn identity_for_test(
        &self,
    ) -> (Option<crate::identity::RunAs>, Option<&PathBuf>, &[String]) {
        (self.run_as, self.cwd.as_ref(), &self.env_remove)
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

    /// `signal`, then SIGKILL if the child does not go within `grace`, so it gets
    /// a chance to flush and close its datadir before it is killed outright.
    ///
    /// The signal is the caller's choice because the daemons distinguish them:
    /// SIGTERM drains, SIGQUIT stops now without one, and both leave a datadir
    /// that reopens cleanly. SIGKILL leaves the mess a crash leaves, so it is the
    /// last resort and never the request.
    /// Signal a running service without waiting for it to exit.
    ///
    /// Separate from [`Self::stop`] because that one is a shutdown: it waits out
    /// a grace period and escalates to SIGKILL. This is for a signal the service
    /// is meant to survive — SIGHUP telling `compute_ctl` to re-read its spec —
    /// where waiting for an exit would be waiting for the wrong thing.
    ///
    /// `false` when the service is not running, which the caller reports rather
    /// than treating as delivered.
    pub fn signal(&mut self, signal: libc::c_int) -> bool {
        let Some(child) = self.child.as_mut() else {
            return false;
        };
        let pid = child.id() as libc::pid_t;
        // SAFETY: `pid` is our own child and the signal is a valid constant.
        unsafe {
            libc::kill(pid, signal);
        }
        tracing::info!(service = %self.name, pid, signal, "signalled");
        true
    }

    pub fn stop(&mut self, signal: libc::c_int, grace: Duration) {
        let Some(child) = self.child.as_mut() else {
            return;
        };
        let pid = child.id() as libc::pid_t;
        #[cfg(test)]
        {
            self.last_stop_signal = Some(signal);
        }
        // SAFETY: `pid` is our own child, and both signals are valid.
        unsafe {
            libc::kill(pid, signal);
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
        tracing::warn!(service = %self.name, signal, "did not exit on the stop signal; killing");
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
    BudgetExceeded {
        service: String,
        restarts: usize,
    },
    /// The endpoint's write lease is gone. Every child has been stopped; the
    /// caller waits for the lease again and brings the endpoint back up.
    FenceLost,
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
            let svc = &mut self.services[i];
            let res = crate::startup::timed(&svc.name.clone(), || -> Result<()> {
                // Retried: a `fork`/`exec` that fails under momentary memory
                // pressure should not cost a whole pod restart at boot.
                crate::retry::retry("spawn", &crate::retry::Backoff::spawn(), || svc.spawn())?;
                svc.wait_healthy(timeout, interval)
            });
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
            if let Some(outcome) = self.poll_once()? {
                return Ok(outcome);
            }
            std::thread::sleep(poll);
        }
    }

    /// One pass of the monitor loop: `Some` when the loop should stop.
    ///
    /// Split out from [`Self::run`] so a caller that shares the supervisor with
    /// a control socket can take the lock per pass instead of holding it for the
    /// process's lifetime — see [`crate::control`].
    pub fn poll_once(&mut self) -> Result<Option<Outcome>> {
        if shutdown_requested() {
            self.stop_all();
            return Ok(Some(Outcome::Shutdown));
        }
        // Before the restart pass, so a child that exits because it fenced itself
        // out is not restarted into the same wall.
        if crate::fence::lost() {
            self.stop_all();
            return Ok(Some(Outcome::FenceLost));
        }
        for i in 0..self.services.len() {
            // A service an operator asked to stop stays stopped. Restarting it
            // here would make `pageserver stop` unwinnable.
            if !self.services[i].wanted() {
                continue;
            }
            match self.services[i].exited() {
                // Still running.
                Ok(None) => continue,
                Ok(Some(_)) => {}
                // Whether the child is alive is unknown, so do neither of the two
                // things that could be wrong: restarting a process that is
                // actually running, or exiting the pod over a failed `try_wait`.
                // Re-check on the next tick instead.
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
                return Ok(Some(Outcome::BudgetExceeded {
                    service: name,
                    restarts: count,
                }));
            }
            tracing::warn!(service = %name, restarts = count, "exited; restarting");
            std::thread::sleep(self.policy.backoff);
            // A daemon that fenced itself out exits before the renewal thread
            // learns the lease is gone, so the check at the top of this pass can
            // pass. Respawning it would hold this pass for the backoff and a
            // `renew_now` wait while Postgres keeps answering reads from a
            // snapshot another endpoint has moved past (AMP-76).
            if crate::fence::lost() {
                self.stop_all();
                return Ok(Some(Outcome::FenceLost));
            }
            // Same as an operator start: the replacement needs a token that
            // outlasts its startup, not the remainder of this interval.
            crate::fence::renew_now();
            if let Err(e) = self.services[i].spawn() {
                // A failed respawn must not escape this loop: propagating it
                // would exit the pod on the first transient `fork`/`exec`
                // failure, skipping the very budget that exists to bound this.
                // The child stays `None`, so the next tick counts another restart
                // and tries again — and a permanent failure still exhausts the
                // budget and exits, just by the intended route.
                tracing::error!(
                    service = %name,
                    error = %e,
                    "respawn failed; retrying against the restart budget"
                );
            } else {
                // Seeded at spawn with whatever was current, which may be nearly
                // spent by now; hand it the live one rather than leaving it to
                // write against a token that is about to lapse.
                crate::fence::push_current();
            }
        }
        Ok(None)
    }

    /// One service by name, for tests that assert on its state.
    #[cfg(test)]
    pub fn service_for_test(&mut self, name: &str) -> &mut Service {
        self.services
            .iter_mut()
            .find(|s| s.name == name)
            .expect("no such service")
    }

    /// Stop supervising one service, without signalling it.
    ///
    /// For a caller that wants to take the process down its own way: a compute is
    /// stopped through Postgres, and how Postgres goes decides what survives —
    /// `compute_ctl` turns every signal it gets into a *fast* shutdown, so asking
    /// it to stop can never produce a crash stop. Clearing `wanted` first is what
    /// keeps the monitor loop from restarting whatever the caller then kills.
    pub fn disable(&mut self, name: &str) -> Result<()> {
        let svc = self
            .services
            .iter_mut()
            .find(|s| s.name == name)
            .with_context(|| format!("no service named {name}"))?;
        svc.wanted = false;
        tracing::info!(service = name, "no longer supervised, on request");
        Ok(())
    }

    /// Stop one service and keep it stopped.
    ///
    /// Distinct from a crash: `wanted` goes false, so the monitor loop leaves it
    /// alone until [`Self::start_one`]. This is what `yb_amp_local pageserver
    /// stop` reaches.
    ///
    /// `drain` false sends SIGQUIT instead of SIGTERM: stop now, without the
    /// shutdown checkpoint. That is the launcher's `--mode immediate` — a signal
    /// the daemons implement, not a shorter deadline before SIGKILL, which would
    /// hand them a crash to recover from.
    pub fn stop_one(&mut self, name: &str, drain: bool) -> Result<()> {
        let grace = self.policy.stop_grace;
        let signal = if drain { libc::SIGTERM } else { libc::SIGQUIT };
        let svc = self
            .services
            .iter_mut()
            .find(|s| s.name == name)
            .with_context(|| format!("no service named {name}"))?;
        // Before the signal, not after: otherwise the monitor loop sees the child
        // gone and restarts the very thing being stopped.
        svc.wanted = false;
        svc.stop(signal, grace);
        tracing::info!(service = name, drain, "stopped on request");
        Ok(())
    }

    /// Start one service and gate on its health probe, so the caller knows it is
    /// serving rather than merely spawned.
    ///
    /// `budget` is how long the caller is willing to wait. It has to be the
    /// caller's, not this policy's: a caller that gives up first gets a timeout
    /// while the supervisor is still working, reports a failure that is not one,
    /// and cannot say why. `None` uses the policy's own gate.
    pub fn start_one(&mut self, name: &str, budget: Option<Duration>) -> Result<()> {
        let timeout = budget.unwrap_or(self.policy.health_timeout);
        let interval = self.policy.health_interval;
        let svc = self
            .services
            .iter_mut()
            .find(|s| s.name == name)
            .with_context(|| format!("no service named {name}"))?;
        svc.wanted = true;
        // `started()` alone is not enough: a child that has exited stays `Some`
        // until the monitor loop reaps it, so a service that crashed a moment ago
        // reads as running. `exited()` reaps it, which also keeps the replacement
        // below from orphaning a zombie.
        if svc.started() && matches!(svc.exited(), Ok(None)) {
            return Ok(());
        }
        // Before the spawn, so the child is seeded with a full-TTL token: what is
        // left of the current one may not outlast its startup writes.
        crate::fence::renew_now();
        // The monitor loop stops the endpoint on its next pass; a service
        // started now would only fence itself out, after holding the supervisor
        // lock for its whole health gate.
        anyhow::ensure!(
            !crate::fence::lost(),
            "not starting {name}: the write lease is lost"
        );
        svc.spawn()?;
        svc.wait_healthy(timeout, interval)?;
        crate::fence::push_current();
        tracing::info!(service = name, "started on request");
        Ok(())
    }

    /// One name per line, `<name> running|down|stopped`.
    ///
    /// `down` and `stopped` are both not-running, and the difference is the
    /// whole point: `down` is a service that is supposed to be up, so the
    /// monitor loop is about to restart it; `stopped` is one an operator asked
    /// to stay down.
    pub fn status(&mut self) -> String {
        let mut out = String::new();
        for s in &mut self.services {
            let alive = matches!(s.exited(), Ok(None)) && s.started();
            let state = if alive {
                "running"
            } else if s.wanted {
                "down"
            } else {
                "stopped"
            };
            out.push_str(&format!("{} {}\n", s.name, state));
        }
        out
    }

    /// Stops children in reverse start order, so a dependency outlives its
    /// dependents.
    ///
    /// A shutdown that arrived as SIGQUIT is passed on as one: the caller asked
    /// to stop now rather than drain, and the daemons implement that.
    /// Send a signal to one service by name, without stopping it.
    pub fn signal_one(&mut self, name: &str, signal: libc::c_int) -> Result<()> {
        let svc = self
            .services
            .iter_mut()
            .find(|s| s.name == name)
            .with_context(|| format!("no service named {name}"))?;
        anyhow::ensure!(svc.signal(signal), "{name} is not running");
        Ok(())
    }

    pub fn stop_all(&mut self) {
        let grace = self.policy.stop_grace;
        let signal = if shutdown_is_immediate() {
            libc::SIGQUIT
        } else {
            libc::SIGTERM
        };
        for s in self.services.iter_mut().rev() {
            s.stop(signal, grace);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Serializes the tests that touch the process-wide fence state against the
    /// ones that read it — see [`crate::fence::lost_lock`].
    ///
    /// `poll_once` and `start_one` read `fence::lost()`, which is static: a test
    /// that sets it is setting it for every test running in parallel. That is
    /// how a lease-loss test made an unrelated restart-budget test see
    /// `FenceLost` instead of the outcome it was asserting on.
    fn fence_flag() -> std::sync::MutexGuard<'static, ()> {
        crate::fence::lost_lock()
    }

    /// Declare the lease lost, and put it back on the way out — including on a
    /// panic, which a bare `set_lost_for_test(false)` at the end of a test does
    /// not cover. A leaked `true` would fail every later monitor-loop test.
    struct LeaseLost(#[allow(dead_code)] std::sync::MutexGuard<'static, ()>);

    impl LeaseLost {
        fn declare() -> Self {
            let held = fence_flag();
            crate::fence::set_lost_for_test(true);
            Self(held)
        }
    }

    impl Drop for LeaseLost {
        fn drop(&mut self) {
            crate::fence::set_lost_for_test(false);
        }
    }

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

    /// `pageserver start` on a service that crashed a moment ago has to actually
    /// start it. The crashed child is still `Some` until the monitor loop reaps
    /// it, so a check on "was it ever spawned" reports it running and returns
    /// having done nothing — and the caller goes on to probe a dead port.
    #[test]
    fn starting_a_crashed_service_replaces_it() {
        // `start_one` reads `fence::lost()`.
        let _fence = fence_flag();
        let mut sup = Supervisor::new(fast_policy());
        let mut svc = Service::new("pageserver", sh("exit 7"));
        svc.spawn().unwrap();
        // Let it die, without giving the monitor loop a chance to reap it.
        let deadline = Instant::now() + Duration::from_secs(5);
        while Instant::now() < deadline && matches!(svc.exited(), Ok(None)) {
            std::thread::sleep(Duration::from_millis(10));
        }
        // Re-spawn it as a child that stays up, so the restart is observable.
        svc.argv = sh("sleep 60");
        sup.add(svc);

        sup.start_one("pageserver", None)
            .expect("start a crashed service");
        assert!(
            matches!(sup.services[0].exited(), Ok(None)),
            "start_one returned with the service still dead"
        );
        sup.stop_all();
    }

    /// A child has to come up already holding the lease. It arms before it does
    /// anything, and an armed fence with no token refuses every write — which
    /// for a pageserver is the bootstrap import, long before a token could
    /// arrive over HTTP. Seeded at every spawn, so a restart mid-lease is
    /// covered too.
    #[test]
    fn a_spawned_child_is_handed_the_current_token() {
        // The token below is long expired, which `fence::lost` reads as a lapsed
        // lease: hold the flag so no monitor-loop test sees it.
        let _fence = fence_flag();
        let dir = camino_tempfile::tempdir().unwrap();
        let seen = dir.path().join("seen");
        let mut svc = Service::new(
            "pageserver",
            sh(&format!(
                "printf '%s' \"${{{}:-none}}\" > {seen}",
                utils::write_fence::INITIAL_TOKEN_ENV
            )),
        );

        // No lease held: nothing to hand over, and the child must not be told a
        // token it does not have.
        svc.spawn().unwrap();
        wait_for_exit(&mut svc);
        assert_eq!(std::fs::read_to_string(&seen).unwrap(), "none");

        crate::fence::set_current_for_test(Some(4_242_424_242));
        svc.spawn().unwrap();
        wait_for_exit(&mut svc);
        assert_eq!(std::fs::read_to_string(&seen).unwrap(), "4242424242");
        crate::fence::set_current_for_test(None);
    }

    /// `env_once` describes the bring-up a child was started in, so a restart
    /// must not see it — and a spawn that failed started nothing, so the retry
    /// still has to carry it.
    #[test]
    fn a_once_variable_reaches_the_first_child_and_no_restart() {
        let dir = camino_tempfile::tempdir().unwrap();
        let seen = dir.path().join("seen");
        let script = sh(&format!("printf '%s' \"${{ONCE:-none}}\" > {seen}"));
        let mut svc = Service::new("compute_ctl", vec!["/nonexistent/compute_ctl".into()])
            .env_once("ONCE", "1234");

        svc.spawn().expect_err("spawning a missing binary fails");
        svc.argv = script;
        svc.spawn().unwrap();
        wait_for_exit(&mut svc);
        assert_eq!(std::fs::read_to_string(&seen).unwrap(), "1234");

        svc.spawn().unwrap();
        wait_for_exit(&mut svc);
        assert_eq!(std::fs::read_to_string(&seen).unwrap(), "none");
    }

    fn wait_for_exit(svc: &mut Service) {
        let deadline = Instant::now() + Duration::from_secs(30);
        while Instant::now() < deadline {
            if matches!(svc.exited(), Ok(Some(_))) {
                return;
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        panic!("child never exited");
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

    /// `--mode immediate` means SIGQUIT to these daemons, and they implement it:
    /// stop now, no drain, datadir still reopenable. SIGTERM with no grace — kill
    /// it the moment it does not go — hands the daemon a crash to recover from,
    /// and it comes back slower than the caller is willing to wait.
    #[test]
    fn an_immediate_stop_asks_rather_than_kills() {
        // The signal it selects, not the child's fate: which signal ends a
        // process depends on how promptly a loaded machine delivers it, while
        // the choice is the thing this has to get right. What SIGQUIT then does
        // to a real daemon is what the integration suite covers.
        let mut svc = Service::new("safekeeper", sh("sleep 60"));
        svc.spawn().unwrap();
        let mut sup = Supervisor::new(fast_policy());
        sup.add(svc);

        sup.stop_one("safekeeper", false).unwrap();
        assert_eq!(
            sup.service_for_test("safekeeper").last_stop_signal,
            Some(libc::SIGQUIT),
            "an immediate stop must ask with SIGQUIT"
        );

        let mut svc = Service::new("pageserver", sh("sleep 60"));
        svc.spawn().unwrap();
        sup.add(svc);
        sup.stop_one("pageserver", true).unwrap();
        assert_eq!(
            sup.service_for_test("pageserver").last_stop_signal,
            Some(libc::SIGTERM),
            "a draining stop must ask with SIGTERM"
        );
    }

    #[test]
    fn health_probe_gates_startup() {
        let mut s = Service::new("sleeper", sh("sleep 30")).health(Box::new(|| true));
        s.spawn().unwrap();
        s.wait_healthy(Duration::from_secs(2), Duration::from_millis(10))
            .unwrap();
        s.stop(libc::SIGTERM, Duration::from_millis(500));
    }

    #[test]
    fn an_unhealthy_service_times_out() {
        let mut s = Service::new("never", sh("sleep 30")).health(Box::new(|| false));
        s.spawn().unwrap();
        let err = s
            .wait_healthy(Duration::from_millis(200), Duration::from_millis(10))
            .expect_err("must time out");
        assert!(format!("{err:#}").contains("did not become healthy"));
        s.stop(libc::SIGTERM, Duration::from_millis(500));
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
        // Runs the monitor loop, which reads the fence flag.
        let _fence = fence_flag();
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
        // Runs the monitor loop, which reads the fence flag.
        let _fence = fence_flag();
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

    /// Losing the lease has to stop every child, and stop them *before* the
    /// restart pass. A daemon that exited because it fenced itself out
    /// (`FENCE_LAPSED_EXIT_CODE`) looks exactly like a crash from here, and
    /// restarting it would put it straight back into the wall it just hit —
    /// against a branch some other endpoint now owns.
    #[test]
    fn losing_the_lease_stops_every_child() {
        let mut sup = Supervisor::new(fast_policy());
        for n in ["safekeeper", "pageserver", "compute_ctl"] {
            sup.add(Service::new(n, sh("sleep 30")).health(Box::new(|| true)));
        }
        sup.start_all().unwrap();
        assert!(sup.services.iter().all(|s| s.started()));

        let lost = LeaseLost::declare();
        let outcome = sup.poll_once().expect("a lost lease is not an error");
        drop(lost);

        assert!(
            matches!(outcome, Some(Outcome::FenceLost)),
            "monitor loop kept going without the lease: {outcome:?}"
        );
        for s in &mut sup.services {
            assert!(s.child.is_none(), "{} was left writing", s.name);
        }
    }

    /// Losing the lease is not the end of the endpoint: the supervisor waits
    /// for the lease and brings it back, so the stop has to leave a stack that
    /// can start again. Clearing `wanted` on the way out, or leaving a spent
    /// child handle behind, would strand the endpoint until the pod restarted.
    #[test]
    fn an_endpoint_comes_back_when_the_lease_does() {
        let mut sup = Supervisor::new(fast_policy());
        for n in ["safekeeper", "pageserver"] {
            sup.add(Service::new(n, sh("sleep 30")).health(Box::new(|| true)));
        }
        sup.start_all().unwrap();

        let lost = LeaseLost::declare();
        let outcome = sup.poll_once().unwrap();
        drop(lost);
        assert!(matches!(outcome, Some(Outcome::FenceLost)));

        sup.start_all()
            .expect("the endpoint could not be brought back after the lease returned");
        for s in &mut sup.services {
            assert!(s.started(), "{} did not come back", s.name);
        }
    }

    /// A pod thawed from a freeze holds a token whose deadline passed while it
    /// was frozen. That is a lost lease on the first tick, before the renewal
    /// thread has woken to ask the store (AMP-76).
    #[test]
    fn a_lapsed_token_stops_every_child_without_waiting_for_a_renewal() {
        let _fence = fence_flag();
        let mut sup = Supervisor::new(fast_policy());
        for n in ["safekeeper", "pageserver", "compute_ctl"] {
            sup.add(Service::new(n, sh("sleep 30")).health(Box::new(|| true)));
        }
        sup.start_all().unwrap();

        // Epoch micros one second in: long expired.
        crate::fence::set_current_for_test(Some(1_000_000));
        let outcome = sup.poll_once();
        crate::fence::set_current_for_test(None);

        assert!(
            matches!(outcome, Ok(Some(Outcome::FenceLost))),
            "monitor loop kept serving on a lapsed token: {outcome:?}"
        );
        for s in &mut sup.services {
            assert!(s.child.is_none(), "{} was left serving", s.name);
        }
    }

    /// The daemons notice a lapsed lease before the supervisor does and exit,
    /// so the pass that finds them dead starts a restart. A loss declared
    /// during its backoff has to end the pass there, not after a respawn and a
    /// wait for a renewal that is never coming (AMP-76: 7 s per daemon, with
    /// Postgres serving stale reads throughout).
    #[test]
    fn a_lease_lost_during_a_restart_backoff_stops_the_endpoint() {
        let held = fence_flag();
        let mut sup = Supervisor::new(Policy {
            backoff: Duration::from_millis(500),
            ..fast_policy()
        });
        sup.add(Service::new("pageserver", sh("exit 66")).health(Box::new(|| true)));
        sup.add(Service::new("compute_ctl", sh("sleep 30")).health(Box::new(|| true)));
        sup.start_all().unwrap();
        wait_for_exit(&mut sup.services[0]);

        let declare = std::thread::spawn(|| {
            std::thread::sleep(Duration::from_millis(100));
            crate::fence::set_lost_for_test(true);
        });
        let started = Instant::now();
        let outcome = sup.poll_once();
        declare.join().unwrap();
        crate::fence::set_lost_for_test(false);
        drop(held);

        assert!(
            matches!(outcome, Ok(Some(Outcome::FenceLost))),
            "the restart went ahead without the lease: {outcome:?}"
        );
        assert!(
            started.elapsed() < Duration::from_secs(2),
            "the pass held the endpoint up for {:?}",
            started.elapsed()
        );
        for s in &mut sup.services {
            assert!(s.child.is_none(), "{} was left serving", s.name);
        }
    }

    /// A renewal still in flight at the local deadline may come back as a valid
    /// continuation — it is judged by the database's clock — so a lapse waits for
    /// its answer instead of ending every session first.
    #[test]
    fn a_lapse_waits_for_a_renewal_in_flight() {
        let _fence = fence_flag();
        let mut sup = Supervisor::new(fast_policy());
        sup.add(Service::new("compute_ctl", sh("sleep 30")).health(Box::new(|| true)));
        sup.start_all().unwrap();

        crate::fence::set_current_for_test(Some(1_000_000));
        crate::fence::set_renewing_for_test(true);
        let in_flight = sup.poll_once();
        crate::fence::set_renewing_for_test(false);
        let answered = sup.poll_once();
        crate::fence::set_current_for_test(None);

        assert!(
            matches!(in_flight, Ok(None)),
            "stopped the endpoint under a renewal in flight: {in_flight:?}"
        );
        assert!(
            matches!(answered, Ok(Some(Outcome::FenceLost))),
            "a lapse with no renewal in flight is a loss: {answered:?}"
        );
    }

    /// An operator start after the lease is gone would only fence itself out,
    /// holding the supervisor lock through its health gate.
    #[test]
    fn a_service_is_not_started_into_a_lost_lease() {
        let lost = LeaseLost::declare();
        let mut sup = Supervisor::new(fast_policy());
        sup.add(Service::new("pageserver", sh("sleep 30")));

        let err = sup
            .start_one("pageserver", None)
            .expect_err("started a service without the lease");
        drop(lost);

        assert!(format!("{err:#}").contains("lease is lost"), "{err:#}");
        assert!(!sup.services[0].started(), "pageserver was spawned anyway");
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
