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

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::Duration;

use anyhow::{Context, Result, bail};
use utils::write_fence::{
    DEFAULT_TOKEN_TTL, MAX_TOKEN_TTL, TokenGrant, TokenStore, WriteToken, branch_resource_id,
    token_remaining,
};

/// How long a connect to the token store may take. No lease is held yet, so
/// this cannot be derived from one.
const CONNECT_BUDGET: Duration = Duration::from_secs(15);

/// Bounds on the lease connection itself, tighter than the client's defaults
/// because a renewal has a deadline nothing else here has: miss enough of them
/// and the endpoint stops writing.
///
/// - `statement_timeout` bounds a live-but-slow YB. `token_acquire` is one
///   indexed upsert, so a second is already generous; failing fast and retrying
///   inside the TTL beats one long wait that spends it.
/// - `TCP_USER_TIMEOUT` bounds a YB that is not there at all. A tserver whose
///   node dies without closing its sockets sends no FIN and no RST: the
///   statement bound never fires, because nothing is left to fire it, and the
///   connection looks open. Without this the kernel waits on its keepalive
///   idle, which the driver leaves at two hours, and every renewal until the
///   lease runs out goes to the same dead socket.
/// - `connect_timeout` bounds the reconnect that follows.
///
/// All three are what make a dead node cost a retry rather than the endpoint.
/// Only the detection is actually spent on one: `run_with_retries` reconnects
/// inside the same call, and both that connect and the retried statement go to
/// a live node. Summing the three describes a sick replacement instead, which
/// is a cluster-wide outage rather than the failure these are sized for.
/// How often the lease is renewed, absent `WRITE_TOKEN_RENEW_SECONDS`.
///
/// A third of the TTL, so two renewals can fail outright before anything stops
/// writing — the margin #813 did not have. Named rather than inline because the
/// bounds below are sized against it, and a test that pinned a fixture instead
/// of this would pass while the real budget stopped adding up.
pub const DEFAULT_RENEW_SECONDS: u64 = 5;

const LEASE_STATEMENT_TIMEOUT_MS: u64 = 2_000;
const LEASE_TCP_USER_TIMEOUT_MS: u64 = 3_000;
const LEASE_CONNECT_TIMEOUT_MS: u64 = 3_000;

/// The token store's connection: the endpoint's own, bounded for renewal.
fn lease_conn(yb: &ysql_client::YbConnConfig) -> ysql_client::YbConnConfig {
    ysql_client::YbConnConfig {
        statement_timeout_ms: LEASE_STATEMENT_TIMEOUT_MS,
        tcp_user_timeout_ms: LEASE_TCP_USER_TIMEOUT_MS,
        connect_timeout_ms: LEASE_CONNECT_TIMEOUT_MS,
        ..yb.clone()
    }
}

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

/// Declare the lease lost (or held again), for tests that exercise what the
/// monitor loop does about it without standing up a token store.
#[cfg(test)]
pub fn set_lost_for_test(lost: bool) {
    LOST.store(lost, Ordering::Relaxed);
}

/// The lease timings in force right now, in milliseconds.
///
/// Seeded from the environment and then, once the registry has been read, from
/// the branch's `write_token_settings` — which is why they are not simply the
/// `Config` the environment produced.
///
/// Read on each use rather than captured, so the branch's values apply to the
/// first token taken rather than only to a later one: the renewal loop takes the
/// interval at the top of every tick, and `acquire` takes the TTL per call.
/// Seeded with the defaults rather than zero, so anything that reads them
/// before `Config::from_env` has run gets the lease this binary would take
/// rather than "no lease at all".
static TTL_MS: AtomicU64 = AtomicU64::new(DEFAULT_TOKEN_TTL.as_millis() as u64);
static RENEW_MS: AtomicU64 = AtomicU64::new(DEFAULT_RENEW_SECONDS * 1_000);

/// Serializes the tests that retune the live timings against each other.
///
/// They are process-wide, so a test that changes them changes them for every
/// test running in parallel — the same shape of bug as the fence flag in #805.
#[cfg(test)]
pub(crate) fn timings_lock() -> std::sync::MutexGuard<'static, ()> {
    static LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    LOCK.lock().unwrap_or_else(|e| e.into_inner())
}

/// Put the timings back to what this binary starts with.
#[cfg(test)]
pub(crate) fn reset_timings_for_test() {
    TTL_MS.store(DEFAULT_TOKEN_TTL.as_millis() as u64, Ordering::Relaxed);
    RENEW_MS.store(DEFAULT_RENEW_SECONDS * 1_000, Ordering::Relaxed);
}

/// The lease timings a token would be taken with now.
pub fn timings() -> (Duration, Duration) {
    (
        Duration::from_millis(TTL_MS.load(Ordering::Relaxed)),
        Duration::from_millis(RENEW_MS.load(Ordering::Relaxed)),
    )
}

/// Set the lease timings. Rejected as a pair, so a half-applied change can never
/// leave the renewal interval outside the lease it renews.
///
/// A TTL set after a token is held applies to the next one taken; the one
/// already held keeps the deadline it was granted with, since the daemons were
/// pushed that value and enforce it against their own clocks.
pub fn set_timings(ttl: Duration, renew_every: Duration) -> Result<()> {
    validate_timings(ttl, renew_every)?;
    // TTL first: the renewal loop reads the interval at the top of a tick, so
    // ordering it this way means a tick that races this call cannot take the
    // new interval against the old TTL.
    TTL_MS.store(ttl.as_millis() as u64, Ordering::Relaxed);
    RENEW_MS.store(renew_every.as_millis() as u64, Ordering::Relaxed);
    tracing::info!(
        ttl_ms = ttl.as_millis() as u64,
        renew_ms = renew_every.as_millis() as u64,
        "write token: lease timings changed"
    );
    Ok(())
}

/// Apply the branch's `write_token_settings` to the live timings.
///
/// Absent keys keep what is in force, so a row naming only `ttl_ms` does not
/// silently reset the interval to a default nobody asked for. The resulting
/// pair still has to be legal together, and an illegal one is refused whole — a
/// registry row is not a reason to run a lease that expires before anything
/// renews it.
///
/// Returns whether anything changed, so a caller polling this can log a retune
/// without logging every poll.
pub fn apply_overrides(settings: &BTreeMap<String, crate::config::OverrideValue>) -> Result<bool> {
    let (live_ttl, live_renew) = timings();
    let (mut ttl, mut renew) = (live_ttl, live_renew);
    for (key, value) in settings {
        match (key.as_str(), value) {
            ("ttl_ms", crate::config::OverrideValue::Int(n)) if *n > 0 => {
                ttl = Duration::from_millis(*n as u64);
            }
            ("renew_seconds", crate::config::OverrideValue::Int(n)) if *n > 0 => {
                renew = Duration::from_secs(*n as u64);
            }
            (k, v) => tracing::warn!(
                key = %k,
                "ignoring write_token setting: want ttl_ms / renew_seconds as positive \
                 integers, got {v:?}"
            ),
        }
    }
    if (ttl, renew) == (live_ttl, live_renew) {
        return Ok(false);
    }
    set_timings(ttl, renew)?;
    Ok(true)
}

/// The one rule both the startup read and a runtime change have to satisfy.
fn validate_timings(ttl: Duration, renew_every: Duration) -> Result<()> {
    if ttl.is_zero() || ttl > MAX_TOKEN_TTL {
        bail!(
            "lease TTL {ttl:?} must be in (0, {MAX_TOKEN_TTL:?}] — the bound `token_acquire` \
             itself enforces, so anything past it is refused by the store rather than by us"
        );
    }
    if renew_every.is_zero() {
        bail!("renewal interval must be non-zero, or the renewal loop would spin");
    }
    if renew_every >= ttl {
        bail!(
            "renewal interval {renew_every:?} must be under the lease TTL {ttl:?}: renewing no \
             sooner than the lease expires leaves no slack to retry a failed renewal in"
        );
    }
    Ok(())
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
        let renew_every = Duration::from_secs(
            crate::env_u64("WRITE_TOKEN_RENEW_SECONDS")?.unwrap_or(DEFAULT_RENEW_SECONDS),
        );
        validate_timings(ttl, renew_every)
            .context("WRITE_TOKEN_TTL_MS / WRITE_TOKEN_RENEW_SECONDS are not a usable pair")?;
        // The startup values are also the first live ones.
        TTL_MS.store(ttl.as_millis() as u64, Ordering::Relaxed);
        RENEW_MS.store(renew_every.as_millis() as u64, Ordering::Relaxed);
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
                TokenStore::connect(&lease_conn(yb), "endpoint-supervisor"),
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
            let ttl = timings().0;
            match self.acquire_with_ttl(ttl) {
                Ok(g) if g.granted => {
                    if let Some(held) = self.token
                        && !granted_while_live(held, g.token, ttl)
                    {
                        // The store grants the holder a renewal however long
                        // ago its token lapsed, so a grant is not proof the
                        // lease was never lost: after a pause longer than the
                        // lease — a frozen VM, a stopped pod — another endpoint
                        // may have taken the branch and given it back. The
                        // children kept this tenure's in-memory state and must
                        // not carry on under a new one, so this is a loss, and
                        // the new token is never pushed to them.
                        tracing::error!(
                            held,
                            granted = g.token,
                            "write token: the lease lapsed before this renewal was granted"
                        );
                        return false;
                    }
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
    /// The backstop under [`lease_conn`]'s bounds, not a substitute for them: it
    /// is one timeout around the whole call, so it cannot tell a slow answer
    /// from a dead socket and leaves the connection to be reused either way.
    /// What it guarantees is that `renew` always reaches the branch that gives
    /// the lease up — an unbounded one would leave the supervisor believing it
    /// still held a lease that had lapsed. Bounded by what is left on the lease
    /// (a call cannot usefully outlive the thing it renews), with a floor so the
    /// paths that hold no token still get a real attempt.
    fn call_budget(&self) -> Duration {
        self.remaining().max(Duration::from_secs(5))
    }

    fn acquire(&self) -> Result<TokenGrant> {
        self.acquire_with_ttl(timings().0)
    }

    fn acquire_with_ttl(&self, ttl: Duration) -> Result<TokenGrant> {
        let budget = self.call_budget();
        self.rt.block_on(async {
            tokio::time::timeout(
                budget,
                self.store.acquire(&self.resource, &self.cfg.client_id, ttl),
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

/// Was `granted` — a token just issued for `ttl` — granted while `held` was
/// still live?
///
/// `token_acquire` issues `now_us() + ttl`, so `granted - ttl` is the moment it
/// granted, read off the database's clock, the same clock `held` came from. A
/// grant before `held` expired continues the lease: no one else could have
/// taken it, since a takeover waits for `held` plus the skew grace. A grant at
/// or after it starts a new one, which the daemons must not be pushed —
/// `WriteFence::write_fenced` resubmits a refused write under any newer token,
/// on the strength of that token extending the same lease.
fn granted_while_live(held: WriteToken, granted: WriteToken, ttl: Duration) -> bool {
    granted.saturating_sub(ttl.as_micros() as u64) < held
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
        while !crate::supervise::shutdown_requested() && !STOP_RENEWALS.load(Ordering::Relaxed) {
            // Per tick, not captured once: that is what makes a retune apply
            // without a restart.
            renewal_sleep(timings().1);
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
            renew_every: Duration::from_secs(DEFAULT_RENEW_SECONDS),
            client_id: "test-client".into(),
        }
    }

    const TTL: Duration = Duration::from_secs(15);

    fn us(secs: u64) -> WriteToken {
        secs * 1_000_000
    }

    /// The steady state: a renewal every few seconds, well inside the lease.
    #[test]
    fn a_renewal_inside_the_lease_continues_it() {
        let held = us(1_000_015); // expires at t = 1_000_015
        let granted = us(1_000_010) + TTL.as_micros() as u64; // granted at t = 1_000_010
        assert!(granted_while_live(held, granted, TTL));
    }

    /// A pod frozen past its lease comes back and the store grants its old
    /// client id again. That grant starts a new tenure, not a renewal: someone
    /// may have held the branch in between.
    #[test]
    fn a_grant_after_the_lease_lapsed_is_a_new_tenure() {
        let held = us(1_000_015);
        let granted = us(1_000_600) + TTL.as_micros() as u64; // ten minutes on
        assert!(!granted_while_live(held, granted, TTL));
    }

    /// The deadline is the end of the lease, not its last instant — the same
    /// rule `WriteFence` applies to a write at the expiry time.
    #[test]
    fn a_grant_at_the_deadline_is_a_new_tenure() {
        let held = us(1_000_015);
        let granted = held + TTL.as_micros() as u64;
        assert!(!granted_while_live(held, granted, TTL));
        assert!(granted_while_live(held, granted - 1, TTL));
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
        // `{:#}` for the chain: `from_env` names the env vars, and the rule it
        // broke is in the cause underneath.
        let err = format!(
            "{:#}",
            got.expect_err("a renewal at the TTL must be refused")
        );
        assert!(err.contains("must be under"), "{err}");
        assert!(err.contains("WRITE_TOKEN_RENEW_SECONDS"), "{err}");
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

    /// A renewal has to fail faster than the lease it renews, on every axis.
    /// Any of these exceeding the TTL means one stalled call spends the whole
    /// lease and the endpoint fences itself out over a single node.
    #[test]
    fn the_lease_connection_fails_faster_than_the_lease() {
        let yb: ysql_client::YbConnConfig =
            toml_edit::de::from_str("host_ports = \"h:5433\"\nuser = \"u\"\ndbname = \"d\"\n")
                .unwrap();
        let c = lease_conn(&yb);

        let ttl = DEFAULT_TOKEN_TTL.as_millis() as u64;
        for (what, ms) in [
            ("statement", c.statement_timeout_ms),
            ("tcp_user", c.tcp_user_timeout_ms),
            ("connect", c.connect_timeout_ms),
        ] {
            // Zero is "no bound" for all three, which is the state the client
            // defaults to and the one a renewal must never inherit.
            assert_ne!(ms, 0, "{what} bound is off");
            assert!(ms < ttl, "{what} bound {ms}ms is not under the {ttl}ms TTL");
        }
        // Detection is what a dead node actually costs, and it has to fit in the
        // slack between a renewal starting and the lease expiring. What follows
        // it does not: `run_with_retries` reconnects inside the same call, and
        // that connect and the retried statement land on a live node, so they
        // take milliseconds rather than their ceilings. Those ceilings bind only
        // when the replacement is sick too — a cluster-wide outage, where the
        // endpoint is going down whatever these numbers say.
        //
        // Against the production default rather than the fixture: a fixture
        // would let this pass while the real budget stopped adding up.
        let slack = (DEFAULT_TOKEN_TTL.as_millis() as u64) - DEFAULT_RENEW_SECONDS * 1_000;
        assert!(
            c.tcp_user_timeout_ms < slack,
            "detecting a dead node ({}ms) outlasts the {slack}ms a renewal has \
             before the lease expires",
            c.tcp_user_timeout_ms
        );
        // The rest of the connection block is the operator's.
        assert_eq!(c.host_ports.to_string(), "h:5433");
    }

    /// The pair is validated as a pair. A renewal interval at or past the TTL
    /// means the lease expires before anything tries to renew it, which is the
    /// endpoint fencing itself out on a timer.
    #[test]
    fn a_retune_cannot_put_the_interval_outside_the_lease() {
        let _live = timings_lock();
        let ten = Duration::from_secs(10);
        assert!(set_timings(ten, Duration::from_secs(5)).is_ok());
        assert_eq!(timings(), (ten, Duration::from_secs(5)));

        for (ttl, renew) in [
            (ten, ten),                               // equal
            (ten, Duration::from_secs(11)),           // past it
            (ten, Duration::ZERO),                    // would spin
            (Duration::ZERO, Duration::from_secs(1)), // no lease at all
            (
                MAX_TOKEN_TTL + Duration::from_secs(1),
                Duration::from_secs(5),
            ), // store refuses it
        ] {
            assert!(
                set_timings(ttl, renew).is_err(),
                "accepted ttl={ttl:?} renew={renew:?}"
            );
        }
        // Every rejection left the working pair in place.
        assert_eq!(timings(), (ten, Duration::from_secs(5)));
        reset_timings_for_test();
    }

    /// The default id must differ between two processes on the same pod, or two
    /// supervisors would share a lease and both be granted it.
    #[test]
    fn default_client_ids_differ() {
        assert_ne!(client_id("branch-a"), client_id("branch-a"));
    }
}
