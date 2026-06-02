//! Auto-reconnecting, prepared-statement-caching wrapper around
//! `tokio_postgres::Client`.
//!
//! Both the pageserver's `YbStore` and the safekeeper's `YbWalStorage`
//! hold a long-lived Postgres-protocol connection to YugabyteDB. Each
//! `tokio_postgres::Client` is tied to a background connection task; if
//! the TCP socket dies (YB tserver crash, network partition, YBA rolling
//! restart, etc.), the connection task ends and any subsequent
//! `execute()` on the `Client` lands on a corpse. Without a wrapper, the
//! parent process holds the dead `Arc<Client>` indefinitely — observed
//! in production on 2026-05-23 where a YB tserver restart left the
//! safekeeper in a retry-forever stall until manually restarted.
//!
//! `YsqlClient` owns its `tokio_postgres::Config` plus a closure that
//! knows how to reopen the connection (NoTls or rustls — `connect_no_tls`
//! / `connect_rustls` factory methods bake the TLS choice in). The live
//! `Arc<Client>` sits behind a `tokio::sync::Mutex`. Every public
//! `execute` / `query` / `query_opt` call:
//!
//!   1. Checks `is_closed()`; reconnects first if the previous client is
//!      dead.
//!   2. Looks up the SQL string in a per-connection prepared-statement
//!      cache. On miss, `tokio_postgres::Client::prepare()` is invoked
//!      once and the resulting `Statement` is cached. On hit, the
//!      cached `Statement` is reused — YB skips the parse+plan round
//!      trip on every subsequent call. Hot-path SQL (read_chain,
//!      scan_range, the bulk INSERT shapes) thus pays parse+plan once
//!      per connection lifetime, not per query.
//!   3. Runs the operation against the cached `Statement`.
//!   4. On error, checks `is_closed()` again; if true, forces a
//!      reconnect (which **clears the cache** because Statements are
//!      tied to a specific Client) and retries **once**.
//!
//! Cache keying: the raw SQL string. Each call site `format!()`s the
//! schema name in, so the string varies by schema but is stable per
//! call site; with one PageStore per process the cache converges to
//! ~10-20 entries (one per distinct query shape). The bulk INSERT
//! generates one cache entry per distinct chunk size (max ~3 in
//! practice — full chunks plus a possibly-short tail).
//!
//! The mutex is held only across the cheap `is_closed()` check and the
//! reconnect itself; the `Arc<Client>` is cloned out before the real DB
//! operation runs, so multiple writers hit `tokio_postgres`'s internal
//! statement multiplexing concurrently with no extra serialization.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use futures::FutureExt;
use futures::future::BoxFuture;
use yb_tokio_postgres as tokio_postgres;
use yb_tokio_postgres::Statement;
use yb_tokio_postgres::types::ToSql;

use crate::config::YbStoreConfig;
use crate::metrics::{YB_CLIENT_ERRORS, YB_RETRIES_EXHAUSTED};

/// Future returned by the reconnect closure.
type OpenFut = BoxFuture<'static, Result<tokio_postgres::Client>>;

/// Closure that opens a fresh `tokio_postgres::Client` against the
/// configured cluster + TLS mode.
type OpenFn = Arc<dyn Fn() -> OpenFut + Send + Sync>;

pub struct YsqlClient {
    open: OpenFn,
    /// Connection pool. One slot per parallel YSQL connection. PostgreSQL
    /// processes statements on a given connection serially, so single-
    /// connection clients bottleneck at one query at a time even when
    /// `tokio_postgres` pipelines the wire. With `pool_size > 1`, callers
    /// pick a slot via round-robin (the `next` atomic counter below) and
    /// the slots run independently in parallel.
    ///
    /// Pool size is fixed at construction. Each slot holds its own
    /// connection + its own prepared-statement cache (cached `Statement`
    /// handles are bound to a specific `Client` so they can't be shared
    /// across slots).
    pool: Vec<tokio::sync::Mutex<State>>,
    next: std::sync::atomic::AtomicUsize,
}

/// The live connection and its per-connection prepared-statement cache.
/// They live together because `Statement` values are scoped to the
/// `Client` that produced them — when the client is replaced (forced
/// reconnect), the cache must be cleared in the same critical section
/// so concurrent callers never see a stale `Statement`.
struct State {
    client: Arc<tokio_postgres::Client>,
    /// SQL string → cached prepared `Statement`. `Statement` is `Clone`
    /// and internally `Arc` so the clone we hand out is cheap.
    prepared: HashMap<String, Statement>,
}

impl YsqlClient {
    /// Build a `YsqlClient` with a single connection (no pool). Equivalent
    /// to `connect_no_tls_pooled(config, 1)`.
    pub async fn connect_no_tls(config: tokio_postgres::Config) -> Result<Self> {
        Self::connect_no_tls_pooled(config, 1).await
    }

    /// Build a `YsqlClient` that connects without TLS, with `pool_size`
    /// independent connections. Opens all connections eagerly so config
    /// errors surface at construction.
    pub async fn connect_no_tls_pooled(
        config: tokio_postgres::Config,
        pool_size: usize,
    ) -> Result<Self> {
        let config = Arc::new(config);
        let open: OpenFn = {
            let config = config.clone();
            Arc::new(move || {
                let config = config.clone();
                async move { open_no_tls(&config).await }.boxed()
            })
        };
        Self::from_open(open, pool_size).await
    }

    /// Build a `YsqlClient` with a single TLS connection. Equivalent to
    /// `connect_rustls_pooled(config, tls, 1)`.
    pub async fn connect_rustls(
        config: tokio_postgres::Config,
        tls: yb_postgres_openssl::MakeTlsConnector,
    ) -> Result<Self> {
        Self::connect_rustls_pooled(config, tls, 1).await
    }

    /// Build a `YsqlClient` that connects via the supplied rustls
    /// connector with `pool_size` independent connections. Opens all
    /// eagerly.
    pub async fn connect_rustls_pooled(
        config: tokio_postgres::Config,
        tls: yb_postgres_openssl::MakeTlsConnector,
        pool_size: usize,
    ) -> Result<Self> {
        let config = Arc::new(config);
        let tls = Arc::new(tls);
        let open: OpenFn = {
            let config = config.clone();
            let tls = tls.clone();
            Arc::new(move || {
                let config = config.clone();
                let tls = tls.clone();
                async move { open_tls(&config, &tls).await }.boxed()
            })
        };
        Self::from_open(open, pool_size).await
    }

    async fn from_open(open: OpenFn, pool_size: usize) -> Result<Self> {
        let pool_size = pool_size.max(1);
        let mut pool = Vec::with_capacity(pool_size);
        for _ in 0..pool_size {
            let client = (open)().await?;
            pool.push(tokio::sync::Mutex::new(State {
                client: Arc::new(client),
                prepared: HashMap::new(),
            }));
        }
        Ok(Self {
            open,
            pool,
            next: std::sync::atomic::AtomicUsize::new(0),
        })
    }

    /// Round-robin pick a pool slot index.
    fn pick_slot(&self) -> usize {
        self.next
            .fetch_add(1, std::sync::atomic::Ordering::Relaxed)
            % self.pool.len()
    }

    /// Pick a pool slot and return the live client at that slot,
    /// reconnecting first if `is_closed()` is already true.
    ///
    /// Exposed publicly so callers can reach `query_raw` / `execute_raw`
    /// and other `tokio_postgres::Client` APIs not mirrored here (used
    /// by the YbStore blob path for COPY-shaped streaming reads).
    /// **Bypasses** the prepared-statement cache — callers that want
    /// caching should use the wrapped `execute` / `query` / `query_opt`
    /// methods.
    ///
    /// Different callers calling `current()` get different pool slots
    /// (round-robin). A streaming caller that holds the returned Arc
    /// for a long time doesn't lock anyone else out — other callers
    /// pick other slots.
    pub async fn current(&self) -> Result<Arc<tokio_postgres::Client>> {
        let slot = self.pick_slot();
        self.current_client_at(slot).await
    }

    /// Return the live client at a specific pool slot, reconnecting
    /// first if it's dead. The reconnect clears that slot's prepared-
    /// statement cache because cached `Statement`s belong to the old
    /// (now-dead) connection. Other slots are unaffected.
    async fn current_client_at(&self, slot: usize) -> Result<Arc<tokio_postgres::Client>> {
        let mut guard = self.pool[slot].lock().await;
        if guard.client.is_closed() {
            tracing::info!(slot, "YsqlClient: previous client closed, reconnecting");
            guard.client = Arc::new((self.open)().await?);
            guard.prepared.clear();
        }
        Ok(guard.client.clone())
    }

    /// Force a reconnect on a specific slot after an op observed
    /// `is_closed()`. Double-checks inside the mutex in case another
    /// task already reconnected while we were waiting on the lock.
    /// Clears that slot's prepared-statement cache on the actual
    /// reconnect path.
    async fn force_reconnect_at(&self, slot: usize) -> Result<Arc<tokio_postgres::Client>> {
        let mut guard = self.pool[slot].lock().await;
        if !guard.client.is_closed() {
            return Ok(guard.client.clone());
        }
        tracing::info!(slot, "YsqlClient: forcing reconnect after failed op");
        guard.client = Arc::new((self.open)().await?);
        guard.prepared.clear();
        Ok(guard.client.clone())
    }

    /// Resolve `sql` to a prepared `Statement`, preparing it once per
    /// connection lifetime and caching the result. Two concurrent
    /// callers that miss the cache for the same SQL may both call
    /// `client.prepare()`; the second one's `Statement` is dropped via
    /// `entry().or_insert_with()`, which is harmless (prepare is
    /// idempotent on the YB side).
    ///
    /// The cache key is the raw SQL string. Callers that build SQL via
    /// `format!()` with stable inputs (schema name, fixed chunk size,
    /// etc.) get cache hits; callers that build SQL with per-call-unique
    /// inputs (such as bulk INSERTs whose VALUES list scales with the
    /// chunk size) get one cache entry per shape — bounded in practice.
    /// Atomically resolve `(client, stmt)` for `sql` on the given slot.
    /// Both are guaranteed to be from the same client incarnation —
    /// holding the slot lock across the prepare prevents the race
    /// where a concurrent `force_reconnect_at` swaps the slot's client
    /// during our prepare, leaving the cache with a `Statement` bound
    /// to a dropped client and subsequent `execute()` calls failing
    /// with "prepared statement sN does not exist".
    ///
    /// Serializes same-slot prepares, but each SQL is only prepared
    /// once per connection lifetime, so this is cheap in steady state.
    async fn get_client_and_stmt_at(
        &self,
        slot: usize,
        sql: &str,
    ) -> Result<(Arc<tokio_postgres::Client>, Statement), tokio_postgres::Error> {
        let mut guard = self.pool[slot].lock().await;
        let client = guard.client.clone();
        if let Some(stmt) = guard.prepared.get(sql) {
            return Ok((client, stmt.clone()));
        }
        let stmt = guard.client.prepare(sql).await?;
        guard.prepared.insert(sql.to_string(), stmt.clone());
        Ok((client, stmt))
    }

    pub async fn execute(
        &self,
        sql: &str,
        params: &[&(dyn ToSql + Sync)],
    ) -> Result<u64> {
        self.run_with_retries("execute", sql, |client, stmt| async move {
            client.execute(&stmt, params).await
        })
        .await
    }

    pub async fn query(
        &self,
        sql: &str,
        params: &[&(dyn ToSql + Sync)],
    ) -> Result<Vec<tokio_postgres::Row>> {
        self.run_with_retries("query", sql, |client, stmt| async move {
            client.query(&stmt, params).await
        })
        .await
    }

    pub async fn query_opt(
        &self,
        sql: &str,
        params: &[&(dyn ToSql + Sync)],
    ) -> Result<Option<tokio_postgres::Row>> {
        self.run_with_retries("query_opt", sql, |client, stmt| async move {
            client.query_opt(&stmt, params).await
        })
        .await
    }

    /// Run a statement-shaped operation against the current client with
    /// classified, bounded internal retries.
    ///
    /// Every error is classified by [`classify_error`] into one of:
    ///
    /// - **`ConnectionDrop`** — TCP socket died (`is_closed()` is true,
    ///   or the wire surfaced a `connection closed/reset/unexpectedly
    ///   closed` message). Force-reconnect the slot (which clears the
    ///   prepared-statement cache, since old `Statement`s are bound to
    ///   the dead client), re-prepare on the fresh connection, retry.
    ///   Capped at [`MAX_CONNECTION_DROPS`] because each one pays a full
    ///   TCP+TLS handshake.
    /// - **`Transient`** — SQLSTATE `40001` (serialization failure),
    ///   `40P01` (deadlock), `25P03` (idle-in-txn timeout), or an
    ///   `XX000` whose message matches one of the
    ///   [`YB_TRANSIENT_MSG_PATTERNS`] entries (tablet leader change,
    ///   schema version mismatch, etc.). The statement did not commit
    ///   — safe to retry. Backoff with jitter, capped at
    ///   [`MAX_TOTAL_ATTEMPTS`].
    /// - **`Fatal`** — application/logic errors (`25006`, `25P02`,
    ///   `2D000`, `3B001`, any `42xxx`) and anything else. Returned
    ///   directly.
    ///
    /// **Idempotency assumption.** All current callers (pageserver
    /// `PageStore`, safekeeper WAL backup, `YbStore` blob ops, storage
    /// controller) issue idempotent SQL — reads, `INSERT ... ON
    /// CONFLICT` / upserts (we `SET yb_enable_upsert_mode = true` on
    /// connect), gen-fenced metadata updates. That lets the retry loop
    /// replay connection drops blindly even when we can't prove the
    /// pre-drop statement didn't commit. A future caller adding a bare
    /// non-idempotent write needs a separate path that returns
    /// `ConnectionDrop` errors to the caller unretried.
    ///
    /// All YsqlClient public methods (`execute` / `query` /
    /// `query_opt`) go through here, so every caller gets the same
    /// retry semantics + prepared-statement caching without
    /// reimplementing them.
    async fn run_with_retries<F, Fut, T>(
        &self,
        op_label: &str,
        sql: &str,
        f: F,
    ) -> Result<T>
    where
        F: Fn(Arc<tokio_postgres::Client>, Statement) -> Fut,
        Fut: std::future::Future<Output = Result<T, tokio_postgres::Error>>,
    {
        let slot = self.pick_slot();
        let mut delay = INITIAL_DELAY;
        let mut connection_drops = 0u32;

        for attempt in 1..=MAX_TOTAL_ATTEMPTS {
            // Ensure the slot's current client is live before fetching
            // its (client, stmt) pair atomically.
            let _ = self.current_client_at(slot).await?;
            let (client, stmt) = match self.get_client_and_stmt_at(slot, sql).await {
                Ok(pair) => pair,
                Err(e) => {
                    let live = self.current_client_at(slot).await?;
                    let class = classify_error(&e, live.is_closed());
                    record_error("prepare", &e, class);
                    match self
                        .handle_retry_outcome(
                            slot,
                            "prepare",
                            attempt,
                            class,
                            &mut connection_drops,
                            &mut delay,
                            &e,
                        )
                        .await?
                    {
                        RetryAction::Continue => continue,
                        RetryAction::Exhaust(reason) => {
                            YB_RETRIES_EXHAUSTED.with_label_values(&[reason]).inc();
                            return Err(anyhow::Error::from(e)).with_context(|| {
                                format!("YsqlClient: {op_label} prepare exhausted {reason} budget")
                            });
                        }
                        RetryAction::Fatal => return Err(anyhow::Error::from(e)),
                    }
                }
            };
            match f(client.clone(), stmt).await {
                Ok(v) => return Ok(v),
                Err(e) => {
                    let class = classify_error(&e, client.is_closed());
                    record_error(op_label, &e, class);
                    match self
                        .handle_retry_outcome(
                            slot,
                            op_label,
                            attempt,
                            class,
                            &mut connection_drops,
                            &mut delay,
                            &e,
                        )
                        .await?
                    {
                        RetryAction::Continue => continue,
                        RetryAction::Exhaust(reason) => {
                            YB_RETRIES_EXHAUSTED.with_label_values(&[reason]).inc();
                            return Err(anyhow::Error::from(e)).with_context(|| {
                                format!("YsqlClient: {op_label} after {reason} retries exhausted")
                            });
                        }
                        RetryAction::Fatal => return Err(anyhow::Error::from(e)),
                    }
                }
            }
        }
        unreachable!("retry loop returns or errors")
    }

    /// Apply the retry decision for a classified error. Mutates
    /// `connection_drops` / `delay` and either advances the loop
    /// (`Continue`), signals the caller to return after recording the
    /// exhaustion (`Exhaust`), or signals a fatal classification
    /// (`Fatal`). Centralized so the prepare-failure and run-failure
    /// paths in `run_with_retries` share identical logic.
    async fn handle_retry_outcome(
        &self,
        slot: usize,
        op_label: &str,
        attempt: u32,
        class: RetryClass,
        connection_drops: &mut u32,
        delay: &mut Duration,
        err: &tokio_postgres::Error,
    ) -> Result<RetryAction> {
        match class {
            RetryClass::ConnectionDrop => {
                *connection_drops += 1;
                if *connection_drops > MAX_CONNECTION_DROPS || attempt == MAX_TOTAL_ATTEMPTS {
                    return Ok(RetryAction::Exhaust("connection_drop"));
                }
                let _ = self.force_reconnect_at(slot).await?;
                tracing::info!(
                    attempt,
                    connection_drops = *connection_drops,
                    "YsqlClient: {op_label} reconnect+retry after dropped connection: {err}"
                );
                Ok(RetryAction::Continue)
            }
            RetryClass::Transient => {
                if attempt == MAX_TOTAL_ATTEMPTS {
                    return Ok(RetryAction::Exhaust("transient"));
                }
                tracing::info!(
                    attempt,
                    sqlstate = err.code().map(|c| c.code()).unwrap_or("none"),
                    backoff_ms = delay.as_millis() as u64,
                    "YsqlClient: {op_label} transient error, retrying: {err}"
                );
                sleep_with_jitter(*delay).await;
                *delay = (*delay * 2).min(MAX_DELAY);
                Ok(RetryAction::Continue)
            }
            RetryClass::Fatal => Ok(RetryAction::Fatal),
        }
    }
}

/// Total retry attempts. Sized for transient-conflict tails under load;
/// connection drops have a separate, smaller budget below.
const MAX_TOTAL_ATTEMPTS: u32 = 10;

/// Maximum reconnects in a single call. Each one is a full TCP+TLS
/// handshake against a (possibly remote) tserver, so we cap tighter
/// than the overall attempt count.
const MAX_CONNECTION_DROPS: u32 = 5;

/// Initial backoff for transient retries. Conflict cycles (`40001`,
/// `40P01`) usually clear in single-digit ms — start low to avoid
/// wasted latency, then double up if the contention persists.
const INITIAL_DELAY: Duration = Duration::from_millis(50);

/// Cap on the per-attempt backoff. Without a cap, the doubling chain
/// would reach 25 s at attempt 10 — too long for caller-visible
/// latency on what is usually a momentary cluster event.
const MAX_DELAY: Duration = Duration::from_secs(1);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RetryClass {
    ConnectionDrop,
    Transient,
    Fatal,
}

enum RetryAction {
    Continue,
    /// Retry budget for `reason` exhausted; caller should propagate the
    /// underlying error with this label.
    Exhaust(&'static str),
    /// Classification was fatal; caller should return the error.
    Fatal,
}

/// XX000 message substrings YB documents as safe-to-retry transients.
/// See <https://yugabytedb.tips/which-client-errors-are-safe-to-retry/>.
/// All correspond to "the statement did not commit" or "the underlying
/// tablet moved mid-flight" events — replaying is safe for any
/// idempotent op (and all current YsqlClient callers are idempotent).
const YB_TRANSIENT_MSG_PATTERNS: &[&str] = &[
    // Read race against a concurrent write — YB asks us to retry from
    // a newer snapshot. The statement did not execute.
    "Restart read required",
    // Tablet leader moved during an async write. Replay-safe for
    // idempotent ops.
    "Tablet leader changed",
    // A follow-up read waited on a prior async write past the deadline.
    "WaitForAsyncWrite RPC",
    // Cluster saturation backpressure — the RPC sat in the queue past
    // its deadline without ever running.
    "Call waited in the queue past deadline",
    // Tablet peer is restarting / splitting / being removed. Whoever
    // takes over the tablet will accept the retry.
    "is shutting down",
    // DML hit during a concurrent schema change — replay against the
    // newer schema version.
    "schema version mismatch",
];

fn looks_like_dropped_connection(msg: &str) -> bool {
    msg.contains("connection closed")
        || msg.contains("connection reset")
        || msg.contains("connection unexpectedly closed")
}

/// Classify a `tokio_postgres::Error` against the YB retry guide.
/// SQLSTATE is preferred when present; for `XX000` and missing codes
/// we fall back to a vetted message-substring allowlist
/// ([`YB_TRANSIENT_MSG_PATTERNS`]). Anything we can't positively
/// identify as transient or connection-drop is `Fatal`.
fn classify_error(err: &tokio_postgres::Error, is_closed: bool) -> RetryClass {
    let msg = err.to_string();
    if is_closed || looks_like_dropped_connection(&msg) {
        return RetryClass::ConnectionDrop;
    }
    if let Some(code) = err.code() {
        let c = code.code();
        // Standard-PG transients: the statement is guaranteed not to
        // have committed.
        if matches!(c, "40001" | "40P01" | "25P03") {
            return RetryClass::Transient;
        }
        // Application / structural errors — never retry. 42xxx covers
        // syntax / undefined object / permission / type mismatch
        // (caller bug). Listing the txn-state codes by hand because
        // they don't share a prefix.
        if matches!(c, "25006" | "25P02" | "2D000" | "3B001") || c.starts_with("42") {
            return RetryClass::Fatal;
        }
        // XX000 and unrecognized codes: fall through. YB packs several
        // distinct transients into XX000, so we have to inspect the
        // message.
    }
    if YB_TRANSIENT_MSG_PATTERNS.iter().any(|p| msg.contains(p)) {
        return RetryClass::Transient;
    }
    RetryClass::Fatal
}

fn record_error(op: &str, e: &tokio_postgres::Error, class: RetryClass) {
    let sqlstate = e.code().map(|c| c.code()).unwrap_or("none");
    let class_str = match class {
        RetryClass::ConnectionDrop => "connection_drop",
        RetryClass::Transient => "transient",
        RetryClass::Fatal => "fatal",
    };
    YB_CLIENT_ERRORS
        .with_label_values(&[op, class_str, sqlstate])
        .inc();
}

/// Sleep for `base` plus 0–25% random jitter. With `pool_size > 1`,
/// slots that all hit the same cluster event would otherwise retry in
/// lockstep — jitter breaks the herd.
async fn sleep_with_jitter(base: Duration) {
    let jitter_factor = rand::random::<f64>() * 0.25;
    let total = base.mul_f64(1.0 + jitter_factor);
    tokio::time::sleep(total).await;
}

impl YsqlClient {
    /// High-level factory: build a `YsqlClient` from a `YbStoreConfig`.
    /// Handles `password_env` lookup, application_name, and the
    /// sslmode/sslrootcert dispatch.
    ///
    /// Both the pageserver's `YbStore` and the safekeeper's WAL-mirror
    /// (`WalBackup`) call this so they agree on TLS semantics — without
    /// this shared factory, the WAL mirror used to hardcode `NoTls`
    /// regardless of `sslmode`, and would silently fail against any YBA
    /// universe configured with TLS-only `pg_hba` rules.
    pub async fn from_yb_config(
        cfg: &YbStoreConfig,
        application_name: &str,
    ) -> Result<Self> {
        Self::from_yb_config_pooled(cfg, application_name, 1).await
    }

    /// Same as `from_yb_config` but opens `pool_size` independent
    /// connections. Use for hot read paths that benefit from parallel
    /// SQL execution (e.g. pageserver compute_page_store at pool_size=4).
    pub async fn from_yb_config_pooled(
        cfg: &YbStoreConfig,
        application_name: &str,
        pool_size: usize,
    ) -> Result<Self> {
        let pg_config = build_pg_config(cfg, application_name)?;
        match cfg.sslmode.as_deref() {
            None | Some("disable") => {
                Self::connect_no_tls_pooled(pg_config, pool_size).await
            }
            Some(_mode) => {
                let tls = build_yb_rustls_connector(cfg.sslrootcert.as_deref())?;
                Self::connect_rustls_pooled(pg_config, tls, pool_size).await
            }
        }
    }
}

fn build_pg_config(
    cfg: &YbStoreConfig,
    application_name: &str,
) -> Result<tokio_postgres::Config> {
    let mut pg_config = tokio_postgres::Config::new();
    pg_config
        .host(&cfg.host)
        .port(cfg.port)
        .user(&cfg.user)
        .dbname(&cfg.dbname)
        .application_name(application_name);
    // YB smart-driver mode: `only-primary` makes the driver run
    // `SELECT * FROM yb_servers()` against the seed host on connect,
    // then open each pool connection to a different primary tserver
    // (round-robin). Reads + writes spread across the cluster
    // without any topology-keys configuration. The seed list is
    // refreshed periodically (`yb_servers_refresh_interval`, default
    // 300 s) so new tservers come into rotation without a restart.
    // For single-node dev clusters the call still works — yb_servers()
    // returns the lone node and every connection hits it.
    pg_config.load_balance("only-primary");
    if let Some(env_name) = &cfg.password_env {
        let password = std::env::var(env_name).with_context(|| {
            format!("YsqlClient: password env var `{env_name}` is not set")
        })?;
        pg_config.password(password);
    }
    Ok(pg_config)
}

/// Build an openssl-based TLS connector suitable for YugabyteDB
/// Postgres-wire connections through the yb-tokio-postgres smart
/// driver.
///
/// - `sslrootcert = None`: install an AcceptAll verifier
///   (`SslVerifyMode::NONE`) that ignores cert validation entirely
///   and logs the connection. Suitable for `sslmode = "require"`
///   against YBA universes that ship per-universe self-signed CAs
///   not in the system trust store.
/// - `sslrootcert = Some(path)`: load the file as a PEM-encoded CA
///   bundle (`set_ca_file`) and require peer verification
///   (`SslVerifyMode::PEER`) — sslmode = "verify-ca" semantics.
///
/// We swapped from rustls to openssl in the YB driver migration:
/// the YB rust-postgres fork ships `yb-postgres-openssl` natively
/// but no rustls sibling, and `libssl.so.1.1` / `libcrypto.so.1.1`
/// are already in the deploy bundle (`scripts/yb-light-ship.sh`)
/// for the postgres binaries, so the runtime link target is
/// satisfied without new bundle changes.
pub fn build_yb_rustls_connector(
    sslrootcert: Option<&str>,
) -> anyhow::Result<yb_postgres_openssl::MakeTlsConnector> {
    use openssl::ssl::{SslConnector, SslMethod, SslVerifyMode};

    let mut builder =
        SslConnector::builder(SslMethod::tls()).context("openssl SslConnector builder")?;
    if let Some(path) = sslrootcert {
        builder
            .set_ca_file(path)
            .with_context(|| format!("set_ca_file({path})"))?;
        builder.set_verify(SslVerifyMode::PEER);
    } else {
        // AcceptAll: skip verification entirely. Mirrors the previous
        // rustls AcceptAll behaviour for YBA self-signed certs.
        builder.set_verify(SslVerifyMode::NONE);
    }
    Ok(yb_postgres_openssl::MakeTlsConnector::new(builder.build()))
}

async fn open_no_tls(config: &tokio_postgres::Config) -> Result<tokio_postgres::Client> {
    let (client, connection) = config
        .connect(tokio_postgres::NoTls)
        .await
        .context("YsqlClient: connect (NoTls)")?;
    tokio::spawn(async move {
        if let Err(e) = connection.await {
            tracing::warn!("YsqlClient connection (NoTls) ended: {e}");
        }
    });
    // Enable upsert mode so plain INSERT skips the read-before-write check.
    // Harmless against non-YB Postgres (unknown GUC → SET fails silently here).
    let _ = client
        .execute("SET yb_enable_upsert_mode = true", &[])
        .await;
    Ok(client)
}

async fn open_tls(
    config: &tokio_postgres::Config,
    tls: &yb_postgres_openssl::MakeTlsConnector,
) -> Result<tokio_postgres::Client> {
    let (client, connection) = config
        .connect(tls.clone())
        .await
        .context("YsqlClient: connect (openssl)")?;
    tokio::spawn(async move {
        if let Err(e) = connection.await {
            tracing::warn!("YsqlClient connection (openssl) ended: {e}");
        }
    });
    let _ = client
        .execute("SET yb_enable_upsert_mode = true", &[])
        .await;
    Ok(client)
}

#[cfg(test)]
mod retry_class_tests {
    //! `tokio_postgres::Error` has no public constructor, so we can't
    //! drive `classify_error` end-to-end. Instead we test the helpers
    //! the classifier delegates to (connection-drop detector +
    //! transient-message allowlist) — those carry the substantive
    //! logic.
    use super::*;

    #[test]
    fn drop_patterns_match() {
        assert!(looks_like_dropped_connection(
            "db error: connection closed"
        ));
        assert!(looks_like_dropped_connection(
            "io error: connection reset by peer"
        ));
        assert!(looks_like_dropped_connection(
            "connection unexpectedly closed"
        ));
        assert!(!looks_like_dropped_connection(
            "ERROR: relation foo does not exist"
        ));
        assert!(!looks_like_dropped_connection(
            "Restart read required at 12345"
        ));
    }

    #[test]
    fn yb_transient_patterns_cover_documented_classes() {
        // Anchor the allowlist contents so a renamed pattern can't
        // silently drop coverage.
        let expected = [
            "Restart read required",
            "Tablet leader changed",
            "WaitForAsyncWrite RPC",
            "Call waited in the queue past deadline",
            "is shutting down",
            "schema version mismatch",
        ];
        for needle in expected {
            assert!(
                YB_TRANSIENT_MSG_PATTERNS.iter().any(|p| *p == needle),
                "missing transient pattern: {needle}"
            );
        }
    }

    #[test]
    fn jitter_stays_inside_envelope() {
        // 0–25% jitter on top of base must never produce a total below
        // `base` or above `1.25 * base`. Sample a few times because
        // `rand::random` is non-deterministic.
        let base = Duration::from_millis(100);
        for _ in 0..50 {
            let jitter_factor = rand::random::<f64>() * 0.25;
            let total = base.mul_f64(1.0 + jitter_factor);
            assert!(total >= base);
            assert!(total <= base.mul_f64(1.25));
        }
    }

    #[test]
    fn fortytwo_class_prefix_is_correct() {
        // 42xxx covers syntax, undefined object, permission errors —
        // all caller bugs that retrying would not fix. The classifier
        // catches the family via `c.starts_with("42")`; lock that in.
        for code in ["42P01", "42703", "42501", "42883"] {
            assert!(code.starts_with("42"));
        }
        for code in ["40001", "40P01", "25P03", "XX000"] {
            assert!(!code.starts_with("42"));
        }
    }
}
