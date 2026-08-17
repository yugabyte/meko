//! The endpoint's write lease, and pushing it to the daemons.
//!
//! At most one endpoint may write a branch. Nothing about "the old pod is gone"
//! is observable from the new one — only the absence of evidence that it is
//! still there — so the endpoint takes a **lease** instead, and the lease is
//! what carries the right to write.
//!
//! A token is a point in time: the epoch micros the lease expires at. That makes
//! one value both "you hold it" and "stop writing at this instant", which is why
//! it can be handed to the daemons and then enforced by them against their own
//! clocks with nothing to call back to. See
//! `compute/architecture/compute-fencing.md`.
//!
//! The store client is [`utils::write_fence::TokenStore`] — the same one the
//! live tests race against a real cluster, so the acquire/deny/release contract
//! is specified once rather than restated here. It is async and this crate is
//! not, hence the current-thread runtime. `tokio` and `ysql_client` are already
//! in this binary's dependency graph through `pageserver_api`, and every other
//! read this binary makes against YugabyteDB goes through the same client.

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::Duration;

use anyhow::{Context, Result, bail};
use utils::write_fence::{
    DEFAULT_TOKEN_TTL, TokenGrant, TokenStore, WriteToken, branch_resource_id, token_remaining,
};

/// How long a connect to the token store may take. No lease is held yet, so
/// this cannot be derived from one.
const CONNECT_BUDGET: Duration = Duration::from_secs(15);

/// Set once the lease is gone. The renewal thread cannot reach the `Supervisor`
/// — it belongs to the main thread — so it raises this and the monitor loop
/// stops the children on its next tick.
static LOST: AtomicBool = AtomicBool::new(false);

/// The token this process currently holds, readable without the `Fence` itself.
///
/// Every child is spawned with it (see `Service::spawn`), so a daemon is armed
/// and holding a live token from its first instruction — including one the
/// monitor loop restarts mid-lease, which would otherwise come up holding
/// nothing and refuse its first write.
static CURRENT: AtomicU64 = AtomicU64::new(0);

/// The token to seed a child with, or 0 when this process holds none.
pub fn current_token() -> WriteToken {
    CURRENT.load(Ordering::Relaxed)
}

/// Set the held token directly, for tests that spawn children without a lease.
#[cfg(test)]
pub fn set_current_for_test(token: Option<WriteToken>) {
    set_current(token);
}

/// Set when a caller needs the lease renewed now rather than at the next tick.
static RENEW_NOW: AtomicBool = AtomicBool::new(false);

/// Set when the lease's serving is over and the renewal thread must hand the
/// fence back. Nothing else ends that thread when the daemons stop for a spent
/// restart budget: the process is not shutting down and the lease is not lost —
/// renewals would keep succeeding against a healthy store while the join below
/// them blocks, and the lease would outlive the daemons it fenced.
static STOP_RENEWALS: AtomicBool = AtomicBool::new(false);

/// Ask the renewal thread to return, so joining it is bounded by one renewal
/// call rather than by the life of the process.
pub fn stop_renewals() {
    STOP_RENEWALS.store(true, Ordering::Relaxed);
}

/// Ask the renewal thread to renew immediately, and wait briefly for it.
///
/// Renewals run at `renew_every` against a longer TTL, so the held token is
/// nearly spent just before each tick — and a daemon seeded with it then has
/// only that remainder to get through its startup writes, with the next push
/// most of an interval away. Starting a service is therefore worth a fresh
/// token rather than whatever is left of the current one.
pub fn renew_now() {
    let before = current_token();
    // No lease held: fencing is off, or nothing has been acquired yet. There is
    // nothing to freshen, and with no renewal thread running the wait below
    // would stall every service start for its full deadline.
    if before == 0 {
        return;
    }
    RENEW_NOW.store(true, Ordering::Relaxed);
    let deadline = std::time::Instant::now() + Duration::from_secs(5);
    while std::time::Instant::now() < deadline {
        if current_token() != before {
            return;
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    tracing::warn!("write token: renewal did not land before starting a service");
}

/// Where a token is pushed, so any thread can refresh a daemon that just came
/// up without owning the `Fence`.
static TARGETS: std::sync::OnceLock<Vec<(&'static str, String)>> = std::sync::OnceLock::new();

/// Push the token this process holds to every daemon.
///
/// For a service that has just started: it was seeded at spawn with whatever was
/// current then, which can be most of a renewal interval old, and it may write
/// before the next renewal comes round. Waiting for that tick is what fences a
/// freshly restarted daemon out of its own startup.
pub fn push_current() {
    let (Some(targets), token) = (TARGETS.get(), current_token()) else {
        return;
    };
    if token == 0 {
        return;
    }
    for (name, url) in targets {
        match crate::coordinate::post_write_token(url, token) {
            Err(e) => tracing::warn!(service = name, "write token: push failed: {e:#}"),
            Ok(armed) => tracing::debug!(service = name, armed, "write token accepted"),
        }
    }
}

fn set_current(token: Option<WriteToken>) {
    CURRENT.store(token.unwrap_or(0), Ordering::Relaxed);
}

pub fn lost() -> bool {
    LOST.load(Ordering::Relaxed)
}

/// How the lease is configured, as the chart's ConfigMap delivers it.
#[derive(Debug, Clone)]
pub struct Config {
    /// Take a lease at all. False runs the endpoint unfenced, which is only safe
    /// for a topology that is single-writer by construction.
    pub required: bool,
    pub ttl: Duration,
    /// How often to renew. The gap to `ttl` is how long a failing renewal may be
    /// retried before anything downstream stops writing.
    pub renew_every: Duration,
    /// Identifies this PROCESS, never this endpoint. `token_acquire` grants
    /// unconditionally to the current holder — that is what makes renewal cheap
    /// — so two supervisors sharing an id would both be granted it and there
    /// would be no fence at all.
    pub client_id: String,
}

impl Config {
    pub fn from_env(branch_id: &str) -> Result<Self> {
        let ttl = match crate::env_u64("WRITE_TOKEN_TTL_MS")? {
            Some(ms) => Duration::from_millis(ms),
            None => DEFAULT_TOKEN_TTL,
        };
        let renew_every =
            Duration::from_secs(crate::env_u64("WRITE_TOKEN_RENEW_SECONDS")?.unwrap_or(8));
        if renew_every >= ttl {
            bail!(
                "WRITE_TOKEN_RENEW_SECONDS ({renew_every:?}) must be under WRITE_TOKEN_TTL_MS \
                 ({ttl:?}): renewing no sooner than the lease expires leaves no slack to retry \
                 a failed renewal in"
            );
        }
        Ok(Self {
            required: crate::env_bool_or("REQUIRE_WRITE_TOKEN", true),
            ttl,
            renew_every,
            client_id: client_id(branch_id),
        })
    }
}

/// A per-process client id. `WRITE_TOKEN_CLIENT_ID` overrides it, which is only
/// safe if the operator can guarantee uniqueness per process.
///
/// The default mixes the hostname (the pod), the pid and a nonce, so a restarted
/// pod with the same name and a recycled pid is still a different client.
fn client_id(branch_id: &str) -> String {
    if let Some(id) = crate::env_opt("WRITE_TOKEN_CLIENT_ID") {
        return id;
    }
    let host = std::fs::read_to_string("/etc/hostname")
        .ok()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| "endpoint".to_string());
    let nonce = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.subsec_nanos())
        .unwrap_or(0);
    format!("{host}-{}-{nonce:08x}-{branch_id}", std::process::id())
}

/// The lease, and the daemons it is pushed to.
pub struct Fence {
    cfg: Config,
    store: TokenStore,
    /// Drives the async store client from this synchronous binary.
    rt: tokio::runtime::Runtime,
    resource: String,
    /// The token currently held. `None` before the first grant and after a loss.
    token: Option<WriteToken>,
    /// `(service, url)` for `POST /v1/write_token`.
    targets: Vec<(&'static str, String)>,
}

impl Fence {
    pub fn connect(
        cfg: Config,
        yb: &ysql_client::YbConnConfig,
        branch_id: &str,
        targets: Vec<(&'static str, String)>,
    ) -> Result<Self> {
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .context("building the write-fence runtime")?;
        // Bounded like every other store call: `boot::wait_for_yb` only proves
        // something accepts on the port, and a YB that accepts and then never
        // completes the startup handshake would hang here forever — before the
        // supervisor has started anything, with no log line to say why.
        let store = rt.block_on(async {
            tokio::time::timeout(
                CONNECT_BUDGET,
                TokenStore::connect(yb, "endpoint-supervisor"),
            )
            .await
            .map_err(|_| {
                anyhow::anyhow!(
                    "connecting to the resource-token store timed out after {CONNECT_BUDGET:?}"
                )
            })?
        })?;
        // Also kept globally, so a service that comes up later can be refreshed
        // by whichever thread started it.
        let _ = TARGETS.set(targets.clone());
        Ok(Self {
            resource: branch_resource_id(branch_id),
            cfg,
            store,
            rt,
            token: None,
            targets,
        })
    }

    /// Time left on the held lease by this host's clock.
    pub fn remaining(&self) -> Duration {
        self.token.map(token_remaining).unwrap_or(Duration::ZERO)
    }

    /// Block until this process holds the lease.
    ///
    /// A pod that cannot get it is a pod waiting its turn, not a failed pod, so
    /// this waits rather than returning an error. Gives up only on shutdown, and
    /// reports whether it got one.
    pub fn acquire_blocking(&mut self) -> bool {
        LOST.store(false, Ordering::Relaxed);
        // A renew request that raced the previous lease's loss has nobody left
        // to serve it; a stale flag here would turn this loop's waits into a
        // busy spin against the token store.
        RENEW_NOW.store(false, Ordering::Relaxed);
        let mut announced = false;
        while !crate::supervise::shutdown_requested() {
            match self.acquire() {
                Ok(g) if g.granted => {
                    self.token = Some(g.token);
                    set_current(self.token);
                    tracing::info!(
                        client_id = %self.cfg.client_id,
                        lease_secs = self.remaining().as_secs_f32(),
                        "write token acquired"
                    );
                    return true;
                }
                Ok(g) => {
                    if !announced {
                        tracing::info!(
                            holder = %g.holder,
                            "write token is held elsewhere; waiting our turn"
                        );
                        announced = true;
                    }
                    // `retry_after` is how long until takeover becomes legal;
                    // floored so a near-zero wait cannot spin.
                    sleep_interruptible(g.retry_after.max(Duration::from_millis(250)));
                }
                Err(e) => {
                    tracing::warn!("write token: acquire failed: {e:#}; retrying");
                    sleep_interruptible(Duration::from_secs(1));
                }
            }
        }
        false
    }

    /// One renewal. `false` means the lease is gone and the endpoint must stop.
    ///
    /// A failed *call* is not a lost lease — YB blips, and the gap between the
    /// renewal period and the TTL exists precisely to absorb them — so it is
    /// retried until the held token's own deadline passes. A *denial* is
    /// immediate: someone else holds it.
    fn renew(&mut self) -> bool {
        while !crate::supervise::shutdown_requested() {
            match self.acquire() {
                Ok(g) if g.granted => {
                    self.token = Some(g.token);
                    set_current(self.token);
                    self.push();
                    return true;
                }
                Ok(g) => {
                    tracing::error!(holder = %g.holder, "write token was taken over");
                    return false;
                }
                Err(e) => {
                    let left = self.remaining();
                    if left.is_zero() {
                        tracing::error!(
                            "write token: renewal failing and the lease ran out: {e:#}"
                        );
                        return false;
                    }
                    tracing::warn!(
                        left_secs = left.as_secs_f32(),
                        "write token: renewal failed: {e:#}; retrying"
                    );
                    sleep_interruptible(Duration::from_secs(1));
                }
            }
        }
        true
    }

    /// The longest a single store call may take.
    ///
    /// `YsqlClient` sets neither a connect nor a statement timeout, so a YB that
    /// accepts the connection and then never answers would hang this call
    /// forever — and an unbounded `renew` never reaches the branch that gives
    /// the lease up, so the supervisor would sit there believing it still held a
    /// lease that had in fact lapsed. Bounded by what is left on the lease (a
    /// call cannot usefully outlive the thing it renews), with a floor so the
    /// paths that hold no token still get a real attempt.
    fn call_budget(&self) -> Duration {
        self.remaining().max(Duration::from_secs(5))
    }

    fn acquire(&self) -> Result<TokenGrant> {
        let budget = self.call_budget();
        self.rt.block_on(async {
            tokio::time::timeout(
                budget,
                self.store
                    .acquire(&self.resource, &self.cfg.client_id, self.cfg.ttl),
            )
            .await
            .map_err(|_| anyhow::anyhow!("token_acquire did not answer within {budget:?}"))?
        })
    }

    /// Hand the lease back on a clean shutdown, so a replacement starts
    /// immediately instead of waiting out the TTL. Best effort.
    pub fn release(&mut self) {
        if self.token.take().is_none() {
            return;
        }
        set_current(None);
        // Bounded too: this runs on the shutdown path, and a pod that hangs here
        // is a pod the kubelet ends up killing anyway. Failing to release just
        // means the replacement waits out the TTL.
        let budget = self.call_budget();
        let released = self.rt.block_on(async {
            tokio::time::timeout(
                budget,
                self.store.release(&self.resource, &self.cfg.client_id),
            )
            .await
            .map_err(|_| anyhow::anyhow!("token_release did not answer within {budget:?}"))?
        });
        match released {
            Ok(_) => tracing::info!("write token released"),
            Err(e) => {
                tracing::warn!("write token: release failed: {e:#}; it will expire on its own")
            }
        }
    }

    /// Push the held token to the daemons.
    ///
    /// A failed push is not fatal: a daemon that misses one fences itself out
    /// when the token it already holds runs out, which is the correct outcome.
    pub fn push(&self) {
        let Some(token) = self.token else {
            return;
        };
        for (name, url) in &self.targets {
            match crate::coordinate::post_write_token(url, token) {
                Err(e) => tracing::warn!(service = name, "write token: push failed: {e:#}"),
                Ok(armed) => tracing::debug!(service = name, armed, "write token accepted"),
            }
        }
    }
}

/// Sleep, waking early if a shutdown lands. Polled rather than signalled because
/// the flag is set from a signal handler, where a condvar cannot be notified —
/// the whole crate waits on it this way.
fn sleep_interruptible(total: Duration) {
    let step = Duration::from_millis(50);
    let mut slept = Duration::ZERO;
    while slept < total && !crate::supervise::shutdown_requested() {
        let d = step.min(total - slept);
        std::thread::sleep(d);
        slept += d;
    }
}

/// The renewal thread's sleep: also wakes when a service start asks for a fresh
/// token. Only that thread may use this — the acquire loop's waits also run
/// through a sleep, and a pending renew request must not turn them into a spin.
fn renewal_sleep(total: Duration) {
    let step = Duration::from_millis(50);
    let mut slept = Duration::ZERO;
    while slept < total && !crate::supervise::shutdown_requested() {
        // A service about to start wants a full-TTL token, not the remainder of
        // this interval — and a lease whose serving is over wants the thread
        // back.
        if RENEW_NOW.load(Ordering::Relaxed) || STOP_RENEWALS.load(Ordering::Relaxed) {
            return;
        }
        let d = step.min(total - slept);
        std::thread::sleep(d);
        slept += d;
    }
}

/// Renew on a background thread until the lease is lost or the pod stops,
/// handing the fence back so the caller can release or re-acquire with it.
///
/// On loss it raises [`lost`]; the monitor loop reads that on its next tick and
/// stops every child. Every child, not only the safekeeper, because the lease is
/// the endpoint's right to write — and whatever made another instance eligible
/// to take it means this instance is no longer the endpoint.
pub fn spawn_renewals(mut fence: Fence) -> std::thread::JoinHandle<Fence> {
    STOP_RENEWALS.store(false, Ordering::Relaxed);
    std::thread::spawn(move || {
        let every = fence.cfg.renew_every;
        while !crate::supervise::shutdown_requested() && !STOP_RENEWALS.load(Ordering::Relaxed) {
            renewal_sleep(every);
            if crate::supervise::shutdown_requested() || STOP_RENEWALS.load(Ordering::Relaxed) {
                break;
            }
            if !fence.renew() {
                LOST.store(true, Ordering::Relaxed);
                break;
            }
            RENEW_NOW.store(false, Ordering::Relaxed);
        }
        fence
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg() -> Config {
        Config {
            required: true,
            ttl: DEFAULT_TOKEN_TTL,
            renew_every: Duration::from_secs(8),
            client_id: "test-client".into(),
        }
    }

    /// Renewing no sooner than the lease expires leaves nothing to retry a
    /// failed renewal in, so it is refused at startup rather than discovered
    /// during an outage.
    #[test]
    fn renew_interval_must_leave_slack() {
        let vars = [
            ("WRITE_TOKEN_TTL_MS", "5000"),
            ("WRITE_TOKEN_RENEW_SECONDS", "5"),
        ];
        let saved: Vec<_> = vars
            .iter()
            .map(|(k, _)| (*k, std::env::var(k).ok()))
            .collect();
        // SAFETY: single-threaded test; restored below.
        unsafe {
            for (k, v) in vars {
                std::env::set_var(k, v);
            }
        }
        let got = Config::from_env("branch");
        unsafe {
            for (k, v) in saved {
                match v {
                    Some(v) => std::env::set_var(k, v),
                    None => std::env::remove_var(k),
                }
            }
        }
        let err = got
            .expect_err("a renewal at the TTL must be refused")
            .to_string();
        assert!(err.contains("must be under"), "{err}");
    }

    /// A YB that accepts the connection and then never answers must not hang the
    /// renewal: without the bound in `call_budget` the lease-loss path is
    /// unreachable and the supervisor sits believing it holds a lease that has
    /// lapsed. (`YbConnConfig` can carry the same bounds, but the supervisor
    /// takes the connection block as the operator wrote it, so the budget here
    /// is what makes the guarantee unconditional.)
    #[test]
    fn a_yb_that_never_answers_does_not_hang_the_call() {
        use std::io::Read;

        // Accepts, reads the startup packet, and then says nothing at all.
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        std::thread::spawn(move || {
            let (mut sock, _) = listener.accept().unwrap();
            let mut buf = [0u8; 1024];
            let _ = sock.read(&mut buf);
            std::thread::sleep(Duration::from_secs(120));
        });

        let yb: ysql_client::YbConnConfig = toml_edit::de::from_str(&format!(
            "host_ports = \"127.0.0.1:{port}\"\nuser = \"u\"\ndbname = \"d\"\n"
        ))
        .unwrap();

        let started = std::time::Instant::now();
        // Connecting may fail outright or hang; either way it must come back.
        let res =
            Fence::connect(cfg(), &yb, "branch", Vec::new()).and_then(|f| f.acquire().map(|_| ()));
        assert!(res.is_err(), "a silent server must not look like a grant");
        assert!(
            started.elapsed() < Duration::from_secs(60),
            "call was not bounded: took {:?}",
            started.elapsed()
        );
    }

    /// The default id must differ between two processes on the same pod, or two
    /// supervisors would share a lease and both be granted it.
    #[test]
    fn default_client_ids_differ() {
        assert_ne!(client_id("branch-a"), client_id("branch-a"));
    }
}
