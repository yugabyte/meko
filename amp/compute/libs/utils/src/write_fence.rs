//! Single-writer write fencing: the YSQL resource-token store, and the
//! process-wide fence the storage daemons check before they write.
//!
//! Two instances of the same endpoint must never write the same branch at
//! once, and nothing about "the old one is gone" is ever observable from the
//! new one — only the absence of evidence that it is still there. So the
//! endpoint takes a **lease**, and the lease is what carries the right to
//! write.
//!
//! A **token** is a point in time issued by YSQL: the epoch microseconds at
//! which that lease expires. One value therefore says two things at once —
//!
//! - *a lease*: while your token is the stored one, no other client can get
//!   one for that resource;
//! - *a deadline*: the instant after which you must stop writing.
//!
//! The second property is what makes the token worth passing downstream. A
//! service handed one needs nothing else — not the store, not the supervisor:
//! it holds a number and can read its own clock.
//!
//! This module is both halves of that:
//!
//! - [`TokenStore`] — the client for `amp_registry.token_acquire` /
//!   `token_release` / `token_holder` (migration `0006_resource_token`).
//!   Whoever holds the lease (today: the endpoint supervisor) uses this.
//! - [`WriteFence`] / [`global`] — the process-wide "may I still write?"
//!   check the pageserver and safekeeper make against the token that was
//!   pushed to them.
//!
//! The full design, including why the fence is shaped this way and what it
//! does not cover, is in `compute/architecture/compute-fencing.md`.

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, bail};
use serde::{Deserialize, Serialize};
use ysql_client::{YbConnConfig, YsqlClient};

/// A write token: the epoch microseconds at which the holder's lease expires.
/// Zero means "no token", which an armed fence treats as expired.
///
/// Micros, not a HybridTime. The `<< 12` HybridTime encoding belongs to the
/// thin client, at the one point where this becomes `ignore_after_hybrid_time`
/// and the server compares it against its own `clock_->Now()`. Keeping the
/// shift there rather than here keeps it in one place instead of two that can
/// drift — and a token stays directly readable as a timestamp until then.
pub type WriteToken = u64;

/// The lease length the endpoint supervisor takes. It renews at 8s, so 2s of
/// slack absorbs a slow round trip without the fence ever lapsing in the
/// steady state.
pub const DEFAULT_TOKEN_TTL: Duration = Duration::from_secs(10);

/// The grace a NEW client waits past a lapsed token before it may take over,
/// covering disagreement between two PG nodes' wall clocks. Matches
/// YugabyteDB's own `max_clock_skew_usec` default. The holder renews with no
/// grace at all — a renewal cannot violate mutual exclusion whoever's clock is
/// right.
pub const DEFAULT_TOKEN_SKEW: Duration = Duration::from_millis(500);

/// Bounds `token_acquire` enforces. Checked here too, so a caller gets a clear
/// error without a round trip.
const MAX_TOKEN_TTL: Duration = Duration::from_secs(300);
const MAX_TOKEN_SKEW: Duration = Duration::from_secs(3);

/// The resource id convention: one lease per branch, because the branch is the
/// thing at most one writer may write.
pub fn branch_resource_id(branch_id: &str) -> String {
    format!("branch:{branch_id}")
}

// ---------------------------------------------------------------------------
// The lease store
// ---------------------------------------------------------------------------

/// The outcome of one [`TokenStore::acquire`]. Exactly one of the two shapes
/// is meaningful: granted, carrying [`Self::token`]; or denied, carrying
/// [`Self::holder`] and [`Self::retry_after`].
#[derive(Debug, Clone)]
pub struct TokenGrant {
    /// Does this caller now hold the lease?
    pub granted: bool,
    /// The new lease expiry. Zero when denied.
    pub token: WriteToken,
    /// Who holds the lease — this caller when granted, the incumbent when
    /// denied. Empty when denied and the row vanished under a concurrent
    /// release.
    pub holder: String,
    /// How long until this caller may take over. Zero when granted, and zero
    /// when denied but the lease was released meanwhile (i.e. try again now).
    pub retry_after: Duration,
}

/// A live lease, as `token_holder` reports it.
#[derive(Debug, Clone)]
pub struct TokenLease {
    pub client_id: String,
    pub token: WriteToken,
    /// The real lease budget left, zero once lapsed.
    pub expires_in: Duration,
    /// How much longer a *different* client is kept out — `expires_in` plus
    /// the remaining skew grace.
    pub takeover_in: Duration,
}

/// Client for the resource-token store.
///
/// Nothing here interprets the rule. `token_acquire` decides in one atomic
/// `INSERT ... ON CONFLICT DO UPDATE ... WHERE`; this binds arguments and
/// renders the answer.
///
/// **Idempotency.** [`YsqlClient`] can replay a statement whose commit status
/// is unknown, which the repo requires every YB write to tolerate. `acquire`
/// tolerates it by construction: a replayed grant for the same client is a
/// renewal, which is exactly what the holder is always allowed to do. It also
/// absorbs SQLSTATE `40001` internally (a racing loser occasionally gets a
/// serialization failure instead of a clean denial), so callers here see a
/// grant or a denial, never the conflict.
pub struct TokenStore {
    client: YsqlClient,
}

impl TokenStore {
    /// Open a connection to the store.
    pub async fn connect(cfg: &YbConnConfig, application_name: &str) -> anyhow::Result<Self> {
        let client = YsqlClient::from_yb_config(cfg, application_name, false)
            .await
            .context("connect to the resource-token store")?;
        Ok(TokenStore { client })
    }

    /// Wrap an existing connection.
    pub fn new(client: YsqlClient) -> Self {
        TokenStore { client }
    }

    /// Take or renew the lease on `resource_id` for `client_id`, with the
    /// default skew grace. See [`Self::acquire_with_skew`].
    pub async fn acquire(
        &self,
        resource_id: &str,
        client_id: &str,
        ttl: Duration,
    ) -> anyhow::Result<TokenGrant> {
        self.acquire_with_skew(resource_id, client_id, ttl, DEFAULT_TOKEN_SKEW)
            .await
    }

    /// Take or renew the lease on `resource_id` for `client_id`.
    ///
    /// The current holder is always granted a renewal. A different client is
    /// granted only once the stored token plus `skew` has passed; otherwise it
    /// is denied and told who holds it and how long to wait. Both outcomes are
    /// `Ok` — a denial is an answer, not a failure.
    ///
    /// `client_id` must identify one PROCESS, not one logical service. Two
    /// instances sharing a client id are both "the holder", both get granted,
    /// and the mutual exclusion is gone.
    pub async fn acquire_with_skew(
        &self,
        resource_id: &str,
        client_id: &str,
        ttl: Duration,
        skew: Duration,
    ) -> anyhow::Result<TokenGrant> {
        if resource_id.is_empty() || client_id.is_empty() {
            bail!("resource id and client id must be non-empty");
        }
        let ttl_ms = ttl.as_millis() as i64;
        if ttl_ms <= 0 || ttl > MAX_TOKEN_TTL {
            bail!("token ttl must be in (0ms, {MAX_TOKEN_TTL:?}], got {ttl:?}");
        }
        if skew > MAX_TOKEN_SKEW {
            bail!("token skew must be in [0, {MAX_TOKEN_SKEW:?}], got {skew:?}");
        }
        let skew_ms = skew.as_millis() as i64;

        let row = self
            .client
            .query_opt(
                "SELECT granted, token, holder, retry_after_ms \
                   FROM amp_registry.token_acquire($1, $2, $3, $4)",
                &[&resource_id, &client_id, &ttl_ms, &skew_ms],
            )
            .await
            .context("call amp_registry.token_acquire")?
            .context("amp_registry.token_acquire returned no row")?;

        let granted: bool = row.get(0);
        let token: Option<i64> = row.get(1);
        let holder: Option<String> = row.get(2);
        let retry_after_ms: i64 = row.get(3);

        if granted {
            // A grant carries no wait, and the holder is us. Both come back
            // that way from SQL; normalize rather than trust, so a caller can
            // sleep on `retry_after` blindly.
            return Ok(TokenGrant {
                granted: true,
                token: decode_token(token.context("granted token was NULL")?),
                holder: client_id.to_string(),
                retry_after: Duration::ZERO,
            });
        }
        Ok(TokenGrant {
            granted: false,
            token: 0,
            holder: holder.unwrap_or_default(),
            retry_after: Duration::from_millis(retry_after_ms.max(0) as u64),
        })
    }

    /// Drop the lease on `resource_id` if `client_id` holds it; reports
    /// whether a row was removed. Releasing someone else's lease is not an
    /// error — it is a no-op that reports `false`.
    ///
    /// An explicit release by the owner bypasses the skew grace: the next
    /// client is granted immediately rather than waiting out the TTL.
    pub async fn release(&self, resource_id: &str, client_id: &str) -> anyhow::Result<bool> {
        if resource_id.is_empty() || client_id.is_empty() {
            bail!("resource id and client id must be non-empty");
        }
        let row = self
            .client
            .query_opt(
                "SELECT amp_registry.token_release($1, $2)",
                &[&resource_id, &client_id],
            )
            .await
            .context("call amp_registry.token_release")?
            .context("amp_registry.token_release returned no row")?;
        Ok(row.get(0))
    }

    /// The live lease on `resource_id`, or `None` when it is free.
    ///
    /// "Free" means free to a NEW client: `token_holder` keeps reporting a
    /// lapsed lease through its skew grace, precisely so this answer and what
    /// a subsequent [`Self::acquire_with_skew`] would decide cannot disagree.
    pub async fn holder(&self, resource_id: &str) -> anyhow::Result<Option<TokenLease>> {
        self.holder_with_skew(resource_id, DEFAULT_TOKEN_SKEW).await
    }

    /// [`Self::holder`] with an explicit grace.
    pub async fn holder_with_skew(
        &self,
        resource_id: &str,
        skew: Duration,
    ) -> anyhow::Result<Option<TokenLease>> {
        if resource_id.is_empty() {
            bail!("resource id must be non-empty");
        }
        if skew > MAX_TOKEN_SKEW {
            bail!("token skew must be in [0, {MAX_TOKEN_SKEW:?}], got {skew:?}");
        }
        let skew_ms = skew.as_millis() as i64;
        let row = self
            .client
            .query_opt(
                "SELECT client_id, token, expires_in_ms, takeover_in_ms \
                   FROM amp_registry.token_holder($1, $2)",
                &[&resource_id, &skew_ms],
            )
            .await
            .context("call amp_registry.token_holder")?;
        Ok(row.map(|row| {
            let expires_in_ms: i64 = row.get(2);
            let takeover_in_ms: i64 = row.get(3);
            TokenLease {
                client_id: row.get(0),
                token: decode_token(row.get(1)),
                expires_in: Duration::from_millis(expires_in_ms.max(0) as u64),
                takeover_in: Duration::from_millis(takeover_in_ms.max(0) as u64),
            }
        }))
    }
}

/// A token crosses the wire as `bigint`. Epoch micros are ~1.79e15 today against
/// `i64::MAX` = 9.22e18, so the signed range is never in danger and the cast is
/// lossless.
fn decode_token(raw: i64) -> WriteToken {
    raw as WriteToken
}

// ---------------------------------------------------------------------------
// The process-wide fence
// ---------------------------------------------------------------------------

/// Body of `POST /v1/write_token`, the push the supervisor makes on every
/// renewal.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WriteTokenRequest {
    pub token: WriteToken,
}

/// What that push reports back: the token now held, and how long it is good
/// for by this process's own clock. `expires_in_ms` is the useful half — it is
/// how the supervisor sees the clock disagreement it is budgeting for.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WriteTokenResponse {
    pub token: WriteToken,
    pub expires_in_ms: i64,
    pub armed: bool,
}

/// The fence has lapsed: the process may no longer write.
#[derive(Debug, Clone, Copy)]
pub struct FenceLapsed {
    /// The token that lapsed. Zero when none was ever pushed.
    pub token: WriteToken,
    /// How long ago it lapsed, by the local clock.
    pub overdue: Duration,
}

impl std::fmt::Display for FenceLapsed {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        if self.token == 0 {
            write!(
                f,
                "no write token has been issued to this process (write fence is armed)"
            )
        } else {
            write!(
                f,
                "write token {} expired {:?} ago",
                self.token, self.overdue
            )
        }
    }
}

/// The process's answer to "may I still write?".
///
/// One process serves one branch, so this is a singleton rather than something
/// threaded through every call site — [`global`] is it.
///
/// Two states matter, and the difference between them is the whole safety
/// argument:
///
/// - **Disarmed**. Nobody is issuing tokens — in-process tests, and any
///   deployment that has set `require_write_token = false` to work around a
///   fencing bug — and every check passes.
/// - **Armed** ([`WriteFence::arm`], from `require_write_token` in the
///   service's config, and the default everywhere a supervisor runs). Checks
///   are real, and the fence starts out holding the zero token, which reads as
///   already expired. An armed service that never hears from its supervisor
///   therefore refuses to write rather than treating silence as permission.
#[derive(Debug, Default)]
pub struct WriteFence {
    armed: AtomicBool,
    token: AtomicU64,
}

impl WriteFence {
    pub const fn new() -> Self {
        WriteFence {
            armed: AtomicBool::new(false),
            token: AtomicU64::new(0),
        }
    }

    /// Turn checking on. Called once at startup from `require_write_token`.
    /// There is deliberately no way back: disarming a live fence would be a
    /// way to write past a lapsed lease.
    pub fn arm(&self) {
        self.armed.store(true, Ordering::Relaxed);
    }

    #[inline]
    pub fn is_armed(&self) -> bool {
        self.armed.load(Ordering::Relaxed)
    }

    /// Record a token pushed by the supervisor, and return the token now held.
    ///
    /// Monotonic: an older token than the one held is ignored. Pushes are
    /// independent HTTP requests and can be reordered by a retry, and taking
    /// the older of two would shorten the fence for no reason.
    pub fn set_token(&self, token: WriteToken) -> WriteToken {
        self.token.fetch_max(token, Ordering::Relaxed).max(token)
    }

    #[inline]
    pub fn token(&self) -> WriteToken {
        self.token.load(Ordering::Relaxed)
    }

    /// Time left on the current token by the local clock, saturating at zero.
    ///
    /// This compares the issuing PG node's `gettimeofday` against ours —
    /// deliberately, since a monotonic lease cannot be handed between
    /// processes at all — so it carries whatever the two clocks disagree by.
    /// That disagreement is what [`DEFAULT_TOKEN_SKEW`] budgets for on the
    /// takeover side.
    pub fn remaining(&self) -> Duration {
        remaining_at(self.token(), SystemTime::now())
    }

    /// Is this process still allowed to write?
    ///
    /// The armed test comes first so the disarmed default — every dev topology
    /// and every test — is one relaxed load and no clock read, on paths that
    /// run per write batch.
    #[inline]
    pub fn check(&self) -> Result<(), FenceLapsed> {
        if !self.is_armed() {
            return Ok(());
        }
        self.check_at(SystemTime::now())
    }

    /// Record a pushed token and describe what this process now holds. Shared by
    /// the daemons' `POST /v1/write_token` handlers, which differ only in their
    /// router glue.
    pub fn accept_push(&self, token: WriteToken) -> WriteTokenResponse {
        let held = self.set_token(token);
        WriteTokenResponse {
            token: held,
            expires_in_ms: self.remaining().as_millis() as i64,
            armed: self.is_armed(),
        }
    }

    fn check_at(&self, now: SystemTime) -> Result<(), FenceLapsed> {
        if !self.is_armed() {
            return Ok(());
        }
        let token = self.token();
        if remaining_at(token, now) > Duration::ZERO {
            return Ok(());
        }
        Err(FenceLapsed {
            token,
            overdue: overdue_at(token, now),
        })
    }

    /// Check, and if the fence has lapsed, **end the process**.
    ///
    /// Call this immediately before a write reaches YugabyteDB. Exiting rather
    /// than returning an error is the point: an error propagates back to a
    /// client that will reconnect and retry against this same stranded
    /// process. The fence has to remove the writer, not the write. Anything
    /// buffered goes with it, which is correct — it was never acknowledged as
    /// durable, and some other endpoint now owns the branch.
    pub fn enforce(&self, what: &str) {
        if let Err(lapsed) = self.check() {
            tracing::error!("write fence: refusing {what}: {lapsed}; exiting");
            std::process::exit(FENCE_LAPSED_EXIT_CODE);
        }
    }
}

/// Exit code a process uses when it kills itself over a lapsed fence. Distinct
/// from 1 so a supervisor or a log scan can tell "fenced out" from "crashed".
pub const FENCE_LAPSED_EXIT_CODE: i32 = 66;

/// The wall-clock instant a token expires at.
///
/// The token comes from `clock_timestamp()` on the PG node, which bottoms out in
/// the same `gettimeofday` YB's own wall clock reads — so comparing it with a
/// local clock carries whatever the two disagree by. That is deliberate: a
/// monotonic lease cannot be handed between machines at all, and the disagreement
/// is bounded by the `max_clock_skew_usec` the database already fail-stops on.
/// The skew grace on the takeover side is what budgets for it.
pub fn token_deadline(token: WriteToken) -> SystemTime {
    UNIX_EPOCH + Duration::from_micros(token)
}

/// Time left on `token` by the local clock, saturating at zero. A zero token —
/// "none held" — always reads as expired.
pub fn token_remaining(token: WriteToken) -> Duration {
    remaining_at(token, SystemTime::now())
}

fn remaining_at(token: WriteToken, now: SystemTime) -> Duration {
    if token == 0 {
        return Duration::ZERO;
    }
    token_deadline(token)
        .duration_since(now)
        .unwrap_or(Duration::ZERO)
}

fn overdue_at(token: WriteToken, now: SystemTime) -> Duration {
    now.duration_since(token_deadline(token))
        .unwrap_or(Duration::ZERO)
}

/// The environment variable a supervisor seeds a child's first token with.
///
/// A daemon arms before it does anything, and an armed fence holding no token
/// refuses every write — which for a pageserver is the bootstrap import, long
/// before the supervisor can push a token over HTTP. The supervisor holds the
/// lease before it starts any child, so it hands the token over at spawn and
/// uses the HTTP pushes for renewals.
pub const INITIAL_TOKEN_ENV: &str = "WRITE_TOKEN";

/// Arm the process's fence, seeded with the token its supervisor passed at
/// spawn.
///
/// Missing or unparseable seeds arm anyway: refusing to write until a token
/// arrives is the safe direction, and it is what an unsupervised daemon with
/// fencing switched on should do.
pub fn arm_with_initial_token() {
    let fence = global();
    fence.arm();
    match std::env::var(INITIAL_TOKEN_ENV)
        .ok()
        .and_then(|v| v.trim().parse::<WriteToken>().ok())
    {
        Some(token) => {
            fence.set_token(token);
            tracing::info!(
                token,
                remaining_ms = fence.remaining().as_millis() as u64,
                "write fence armed with the token from the supervisor"
            );
        }
        None => tracing::info!("write fence armed; waiting for a write token from the supervisor"),
    }
}

static GLOBAL: WriteFence = WriteFence::new();

/// The process's fence. One process serves one branch, so there is exactly one.
pub fn global() -> &'static WriteFence {
    &GLOBAL
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A token for `at`, the way `now_us()` would produce one.
    fn token_at(at: SystemTime) -> WriteToken {
        at.duration_since(UNIX_EPOCH).unwrap().as_micros() as u64
    }

    // --- the fence, no database needed ------------------------------------

    #[test]
    fn disarmed_fence_never_lapses() {
        let fence = WriteFence::new();
        // No token at all, and a token that expired decades ago: both fine
        // while nobody is issuing tokens.
        assert!(fence.check().is_ok());
        fence.set_token(token_at(UNIX_EPOCH + Duration::from_secs(1)));
        assert!(fence.check().is_ok());
    }

    #[test]
    fn armed_fence_without_a_token_is_closed() {
        let fence = WriteFence::new();
        fence.arm();
        let err = fence
            .check()
            .expect_err("armed fence with no token let a write through");
        assert_eq!(err.token, 0);
        assert!(err.to_string().contains("no write token"), "{err}");
    }

    #[test]
    fn armed_fence_tracks_the_deadline() {
        let now = SystemTime::now();
        let fence = WriteFence::new();
        fence.arm();
        fence.set_token(token_at(now + Duration::from_secs(10)));

        assert!(fence.check_at(now).is_ok());
        assert!(fence.check_at(now + Duration::from_secs(9)).is_ok());
        // The deadline is the end of the lease, not its last instant: a write
        // AT the expiry time is a write after it.
        assert!(fence.check_at(now + Duration::from_secs(10)).is_err());
        let err = fence.check_at(now + Duration::from_secs(11)).unwrap_err();
        assert!(err.overdue >= Duration::from_secs(1), "{:?}", err.overdue);
    }

    #[test]
    fn set_token_is_monotonic() {
        let now = SystemTime::now();
        let fence = WriteFence::new();
        fence.arm();
        let newer = token_at(now + Duration::from_secs(10));
        let older = token_at(now + Duration::from_secs(5));

        fence.set_token(newer);
        // A reordered push of an older token must not shorten the fence.
        assert_eq!(fence.set_token(older), newer);
        assert_eq!(fence.token(), newer);
        assert!(fence.check_at(now + Duration::from_secs(7)).is_ok());
    }

    /// The reply `POST /v1/write_token` sends back, which is the supervisor's
    /// only way to find out it is pushing at a service that will not act on
    /// the token. A disarmed daemon accepting a push and reporting
    /// `armed: true` would make an unfenced endpoint indistinguishable from a
    /// fenced one.
    #[test]
    fn a_push_reports_what_the_daemon_will_do_with_it() {
        let now = SystemTime::now();
        let token = token_at(now + Duration::from_secs(10));

        let disarmed = WriteFence::new();
        let reply = disarmed.accept_push(token);
        assert_eq!(reply.token, token);
        assert!(!reply.armed, "a disarmed daemon must not claim to be fenced");

        let armed = WriteFence::new();
        armed.arm();
        let reply = armed.accept_push(token);
        assert_eq!(reply.token, token);
        assert!(reply.armed);
        assert!(reply.expires_in_ms > 0, "{}", reply.expires_in_ms);
    }

    /// The enforcement decision the route feeds, without the exit: an armed
    /// daemon pushed a token that has already expired must refuse the next
    /// write. This is the state a daemon is left in when its supervisor loses
    /// the lease and stops renewing.
    #[test]
    fn an_expired_push_closes_an_armed_fence() {
        let now = SystemTime::now();
        let fence = WriteFence::new();
        fence.arm();
        fence.accept_push(token_at(now + Duration::from_secs(10)));
        assert!(fence.check_at(now).is_ok());

        // Time passes and no supervisor pushes again.
        let err = fence
            .check_at(now + Duration::from_secs(11))
            .expect_err("an armed fence past its deadline let a write through");
        assert!(err.overdue >= Duration::from_secs(1), "{:?}", err.overdue);

        // A stale push does not reopen it: `set_token` keeps the newer of the
        // two, so the reply echoes the token still held rather than the one
        // just pushed, and the fence stays shut.
        let held = fence.token();
        let reply = fence.accept_push(token_at(now - Duration::from_secs(1)));
        assert_eq!(reply.token, held);
        assert!(fence.check_at(now + Duration::from_secs(11)).is_err());
    }

    #[test]
    fn remaining_saturates_rather_than_underflowing() {
        let now = SystemTime::now();
        let fence = WriteFence::new();
        fence.set_token(token_at(now - Duration::from_secs(30)));
        assert_eq!(remaining_at(fence.token(), now), Duration::ZERO);
        assert_eq!(remaining_at(0, now), Duration::ZERO);
    }

    /// The store, against a live YugabyteDB.
    ///
    /// The mutual-exclusion guarantee lives in one SQL statement, not in Rust,
    /// so the only way to know it holds is to race it on a real cluster. These
    /// need `amp_registry` migrated (migration `0006_resource_token`) and
    /// default to a developer cluster on 5433/yb_storage;
    /// `test/integration/compute/run.sh unit` points them at its own
    /// ephemeral, freshly-migrated one instead.
    ///
    /// Held in a `live` submodule so the plain `cargo nextest run --workspace`
    /// job — which has no cluster — can exclude exactly these by path while
    /// still running the fence tests above.
    mod live {
        use super::super::*;
        use std::sync::Arc;

        async fn store() -> TokenStore {
            let host = std::env::var("YB_TEST_HOST").unwrap_or_else(|_| "127.0.0.1".into());
            let port = std::env::var("YB_TEST_PORT").unwrap_or_else(|_| "5433".into());
            let user = std::env::var("YB_TEST_USER").unwrap_or_else(|_| "yugabyte".into());
            let dbname = std::env::var("YB_TEST_DB").unwrap_or_else(|_| "yb_storage".into());
            let cfg: YbConnConfig = toml_edit::de::from_str(&format!(
                "host_ports = \"{host}:{port}\"\nuser = \"{user}\"\ndbname = \"{dbname}\"\n"
            ))
            .expect("build YbConnConfig");
            TokenStore::connect(&cfg, "write_fence_test")
                .await
                .expect("connect to local YB; set YB_TEST_HOST/PORT/USER/DB to override")
        }

        /// A resource id unique to this test run, so the live tests can run in
        /// parallel with each other against one database.
        fn scratch_resource(what: &str) -> String {
            format!("test:{what}:{}", uuid::Uuid::new_v4())
        }

        #[tokio::test]
        async fn token_grants_and_renews() {
            let store = store().await;
            let res = scratch_resource("grants_and_renews");

            let first = store
                .acquire(&res, "A", DEFAULT_TOKEN_TTL)
                .await
                .expect("acquire");
            assert!(first.granted, "free resource denied: {first:?}");
            assert_eq!(first.holder, "A");
            assert_eq!(first.retry_after, Duration::ZERO);

            // The token is a deadline, so it must sit about a TTL in the future.
            let left = remaining_at(first.token, SystemTime::now());
            assert!(
                left > Duration::ZERO && left <= DEFAULT_TOKEN_TTL + Duration::from_secs(2),
                "token {} is {left:?} from expiry",
                first.token
            );

            // The holder renews with no wait, and the deadline moves forward.
            let renewed = store
                .acquire(&res, "A", DEFAULT_TOKEN_TTL)
                .await
                .expect("renew");
            assert!(
                renewed.granted,
                "holder denied its own renewal: {renewed:?}"
            );
            assert!(
                renewed.token > first.token,
                "renewed token {} did not advance past {}",
                renewed.token,
                first.token
            );
        }

        #[tokio::test]
        async fn second_client_denied_while_lease_is_live() {
            let store = store().await;
            let res = scratch_resource("denied_while_live");

            assert!(
                store
                    .acquire(&res, "A", DEFAULT_TOKEN_TTL)
                    .await
                    .expect("A acquire")
                    .granted
            );

            let denied = store
                .acquire(&res, "B", DEFAULT_TOKEN_TTL)
                .await
                .expect("B acquire");
            assert!(!denied.granted, "B was granted a lease A holds: {denied:?}");
            assert_eq!(denied.token, 0, "a denial must not carry a token");
            assert_eq!(denied.holder, "A");
            // Must cover the rest of the lease plus the grace, and must not be
            // zero — a zero would spin the caller's retry loop.
            assert!(
                denied.retry_after > Duration::ZERO
                    && denied.retry_after
                        <= DEFAULT_TOKEN_TTL + DEFAULT_TOKEN_SKEW + Duration::from_secs(1),
                "retry_after {:?} out of range",
                denied.retry_after
            );

            // A renews while B is locked out — the grace never applies to the holder.
            assert!(
                store
                    .acquire(&res, "A", DEFAULT_TOKEN_TTL)
                    .await
                    .expect("A renew")
                    .granted
            );
        }

        #[tokio::test]
        async fn takeover_only_after_expiry() {
            let store = store().await;
            let res = scratch_resource("takeover");

            // The grace is disabled so takeover becomes legal the instant the
            // lease lapses: skew is a takeover policy knob, not part of the
            // exclusion rule, so zeroing it changes when takeover is legal and
            // nothing else.
            //
            // The TTL is generous rather than minimal on purpose. The "still
            // held" assertion below only means anything if the lease is
            // definitely live when it runs, and these tests share a machine with
            // the rest of the suite — a tight TTL turns a descheduled thread
            // into a spurious failure.
            let ttl = Duration::from_secs(3);
            assert!(
                store
                    .acquire_with_skew(&res, "A", ttl, Duration::ZERO)
                    .await
                    .expect("A acquire")
                    .granted
            );
            assert!(
                !store
                    .acquire_with_skew(&res, "B", ttl, Duration::ZERO)
                    .await
                    .expect("B acquire during lease")
                    .granted,
                "B took over a lease that had not expired"
            );

            tokio::time::sleep(ttl + Duration::from_millis(500)).await;

            let took = store
                .acquire_with_skew(&res, "B", DEFAULT_TOKEN_TTL, Duration::ZERO)
                .await
                .expect("B acquire after expiry");
            assert!(took.granted, "B denied after the lease expired: {took:?}");
            // The row is overwritten in place, so A is simply not the holder now.
            let lease = store
                .holder(&res)
                .await
                .expect("holder")
                .expect("a lease was just granted");
            assert_eq!(lease.client_id, "B");
        }

        #[tokio::test]
        async fn skew_grace_blocks_takeover_but_never_renewal() {
            let store = store().await;
            let res = scratch_resource("skew_grace");

            // A lease that is expired on arrival, with a grace still to run. The
            // grace is the maximum rather than the default: what is under test is
            // that it blocks takeover at all, and a 500ms window is small enough
            // that a descheduled thread could step past it and fail spuriously.
            let grace = MAX_TOKEN_SKEW;
            assert!(
                store
                    .acquire_with_skew(&res, "A", Duration::from_millis(1), grace)
                    .await
                    .expect("A acquire")
                    .granted
            );

            let denied = store
                .acquire_with_skew(&res, "B", DEFAULT_TOKEN_TTL, grace)
                .await
                .expect("B acquire inside grace");
            assert!(!denied.granted, "B took over inside the skew grace");
            assert_eq!(denied.holder, "A");

            // The holder renews straight through the grace.
            assert!(
                store
                    .acquire_with_skew(&res, "A", DEFAULT_TOKEN_TTL, grace)
                    .await
                    .expect("A renew inside grace")
                    .granted
            );

            // Same situation, grace disabled: an expired lease IS free. This is
            // what pins the grace as the only thing that was keeping B out.
            let res2 = scratch_resource("skew_grace_disabled");
            assert!(
                store
                    .acquire_with_skew(&res2, "A", Duration::from_millis(1), Duration::ZERO)
                    .await
                    .expect("A acquire on res2")
                    .granted
            );
            tokio::time::sleep(Duration::from_millis(20)).await;
            assert!(
                store
                    .acquire_with_skew(&res2, "B", DEFAULT_TOKEN_TTL, Duration::ZERO)
                    .await
                    .expect("B acquire with grace disabled")
                    .granted,
                "B denied with the grace disabled and the lease expired"
            );
        }

        #[tokio::test]
        async fn release_is_owner_only_and_hands_over_immediately() {
            let store = store().await;
            let res = scratch_resource("release");

            assert!(
                store
                    .acquire(&res, "A", DEFAULT_TOKEN_TTL)
                    .await
                    .expect("A acquire")
                    .granted
            );

            // A non-owner release is a no-op, not an error, and leaves the lease
            // alone.
            assert!(
                !store
                    .release(&res, "B")
                    .await
                    .expect("release by non-owner"),
                "B released a lease held by A"
            );
            assert_eq!(
                store
                    .holder(&res)
                    .await
                    .expect("holder")
                    .expect("lease still held")
                    .client_id,
                "A"
            );

            assert!(
                store.release(&res, "A").await.expect("release by owner"),
                "owner release reported no row removed"
            );
            // Releasing twice is idempotent: the second call finds nothing.
            assert!(
                !store.release(&res, "A").await.expect("second release"),
                "second release reported a row removed"
            );
            assert!(
                store
                    .holder(&res)
                    .await
                    .expect("holder after release")
                    .is_none()
            );

            // The point of an explicit release: no grace wait for the next client,
            // even though the released lease was nowhere near expiry.
            assert!(
                store
                    .acquire(&res, "B", DEFAULT_TOKEN_TTL)
                    .await
                    .expect("B acquire after release")
                    .granted,
                "B denied a released resource"
            );
        }

        #[tokio::test]
        async fn holder_agrees_with_what_acquire_would_decide() {
            let store = store().await;
            let res = scratch_resource("holder");

            assert!(
                store
                    .holder(&res)
                    .await
                    .expect("holder of a free resource")
                    .is_none()
            );

            let granted = store
                .acquire(&res, "A", DEFAULT_TOKEN_TTL)
                .await
                .expect("A acquire");
            let lease = store
                .holder(&res)
                .await
                .expect("holder")
                .expect("lease just granted");
            assert_eq!(lease.client_id, "A");
            assert_eq!(lease.token, granted.token);
            assert!(
                lease.expires_in > Duration::ZERO && lease.expires_in <= DEFAULT_TOKEN_TTL,
                "expires_in {:?} out of range",
                lease.expires_in
            );
            assert!(
                lease.takeover_in >= lease.expires_in,
                "takeover_in {:?} < expires_in {:?}; the grace can only add time",
                lease.takeover_in,
                lease.expires_in
            );

            // Once the lease AND its grace are gone the resource reads as free —
            // the same answer an acquire by a new client would give.
            let short = scratch_resource("holder_lapsed");
            assert!(
                store
                    .acquire_with_skew(&short, "A", Duration::from_millis(50), Duration::ZERO)
                    .await
                    .expect("short acquire")
                    .granted
            );
            tokio::time::sleep(Duration::from_millis(200)).await;
            assert!(
                store
                    .holder_with_skew(&short, Duration::ZERO)
                    .await
                    .expect("holder of a lapsed lease")
                    .is_none()
            );
        }

        /// The load-bearing one. Many clients storm many resources at once;
        /// afterwards each resource must have exactly one owner, and every client
        /// told "granted" must be that owner.
        ///
        /// A denial is a legitimate outcome, so the assertion is not "everyone
        /// succeeded" — it is that no two clients were ever told yes about the
        /// same resource at the same time.
        #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
        async fn mutual_exclusion_under_a_race() {
            const CLIENTS: usize = 3;
            const RESOURCES: usize = 25;

            let run = uuid::Uuid::new_v4();
            let resources: Vec<String> = (0..RESOURCES)
                .map(|i| format!("test:race:{run}:{i}"))
                .collect();

            // One connection per client, so the storm is a real race between
            // independent sessions rather than a queue behind one.
            let mut stores = Vec::new();
            for _ in 0..CLIENTS {
                stores.push(Arc::new(store().await));
            }

            let mut tasks = Vec::new();
            for (c, store) in stores.iter().enumerate() {
                for resource in &resources {
                    let store = store.clone();
                    let resource = resource.clone();
                    tasks.push(tokio::spawn(async move {
                        let client_id = format!("client-{c}");
                        let grant = store
                            .acquire(&resource, &client_id, DEFAULT_TOKEN_TTL)
                            .await
                            .expect("acquire under contention");
                        (resource, client_id, grant.granted)
                    }));
                }
            }

            // resource -> the client that was told it was granted.
            let mut owners: std::collections::HashMap<String, String> = Default::default();
            for task in tasks {
                let (resource, client_id, granted) = task.await.expect("join");
                if !granted {
                    continue;
                }
                if let Some(prev) = owners.insert(resource.clone(), client_id.clone()) {
                    assert_eq!(
                        prev, client_id,
                        "resource {resource} granted to two different clients"
                    );
                }
            }

            assert_eq!(
                owners.len(),
                RESOURCES,
                "{} of {RESOURCES} resources ended up with an owner",
                owners.len()
            );
            // And the stored row must name whoever was told yes.
            let store = &stores[0];
            for (resource, client_id) in &owners {
                let lease = store
                    .holder(resource)
                    .await
                    .expect("holder")
                    .expect("owned resource has a lease");
                assert_eq!(
                    &lease.client_id, client_id,
                    "{resource}: stored holder disagrees with the grant"
                );
            }
        }

        #[tokio::test]
        async fn argument_bounds_are_rejected() {
            let store = store().await;
            let res = scratch_resource("bounds");

            for (what, ttl, skew) in [
                ("zero ttl", Duration::ZERO, DEFAULT_TOKEN_SKEW),
                (
                    "sub-millisecond ttl",
                    Duration::from_micros(100),
                    DEFAULT_TOKEN_SKEW,
                ),
                (
                    "ttl over the cap",
                    Duration::from_secs(301),
                    DEFAULT_TOKEN_SKEW,
                ),
                (
                    "skew over the cap",
                    DEFAULT_TOKEN_TTL,
                    Duration::from_secs(4),
                ),
            ] {
                assert!(
                    store.acquire_with_skew(&res, "A", ttl, skew).await.is_err(),
                    "{what} was accepted"
                );
            }
            assert!(store.acquire(&res, "", DEFAULT_TOKEN_TTL).await.is_err());
            assert!(store.acquire("", "A", DEFAULT_TOKEN_TTL).await.is_err());
            assert!(store.release("", "A").await.is_err());

            // The SQL enforces the same bounds, reached directly — the Rust guard
            // above must not be the only thing between a caller and a bad lease.
            let raw = store
                .client
                .query_opt(
                    "SELECT granted FROM amp_registry.token_acquire($1, $2, $3, $4)",
                    &[&res.as_str(), &"A", &0i64, &500i64],
                )
                .await;
            assert!(raw.is_err(), "token_acquire accepted p_ttl_ms = 0");
        }

        /// The token is not an opaque handle: its value decodes to the instant the
        /// lease expires, which is the whole reason it can be handed to a service
        /// that then fences itself against its own clock. This is the seam between
        /// the two halves of this module.
        #[tokio::test]
        async fn a_granted_token_drives_the_fence() {
            let store = store().await;
            let res = scratch_resource("fence_seam");

            let granted = store
                .acquire(&res, "A", Duration::from_secs(2))
                .await
                .expect("acquire");
            assert!(granted.granted);

            let fence = WriteFence::new();
            fence.arm();
            fence.set_token(granted.token);
            assert!(
                fence.check().is_ok(),
                "a freshly granted token does not open the fence: {:?}",
                fence.check().err()
            );

            // The deadline is issued by YB's clock, not ours, so assert on the
            // shape rather than to the millisecond: it must be in the future, and
            // not much more than the TTL away.
            let left = fence.remaining();
            assert!(
                left > Duration::ZERO && left <= Duration::from_secs(2) + DEFAULT_TOKEN_SKEW,
                "fence has {left:?} left on a 2s lease"
            );

            // Release, take a 1ms lease, and the fence closes behind it.
            assert!(store.release(&res, "A").await.expect("release"));
            let brief = store
                .acquire(&res, "A", Duration::from_millis(1))
                .await
                .expect("brief acquire");
            assert!(brief.granted);
            fence.token.store(0, Ordering::Release);
            fence.set_token(brief.token);
            tokio::time::sleep(Duration::from_millis(50)).await;
            assert!(
                fence.check().is_err(),
                "the fence stayed open past a lapsed token"
            );
        }
    }
}
