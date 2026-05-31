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

use anyhow::{Context, Result};
use futures::FutureExt;
use futures::future::BoxFuture;
use tokio_postgres::Statement;
use tokio_postgres::types::ToSql;

use crate::config::YbStoreConfig;

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
        tls: tokio_postgres_rustls::MakeRustlsConnect,
    ) -> Result<Self> {
        Self::connect_rustls_pooled(config, tls, 1).await
    }

    /// Build a `YsqlClient` that connects via the supplied rustls
    /// connector with `pool_size` independent connections. Opens all
    /// eagerly.
    pub async fn connect_rustls_pooled(
        config: tokio_postgres::Config,
        tls: tokio_postgres_rustls::MakeRustlsConnect,
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
                async move { open_rustls(&config, &tls).await }.boxed()
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
    async fn get_or_prepare_at(
        &self,
        slot: usize,
        client: &tokio_postgres::Client,
        sql: &str,
    ) -> Result<Statement, tokio_postgres::Error> {
        {
            let guard = self.pool[slot].lock().await;
            if let Some(stmt) = guard.prepared.get(sql) {
                return Ok(stmt.clone());
            }
        }
        let stmt = client.prepare(sql).await?;
        let mut guard = self.pool[slot].lock().await;
        // Another task may have prepared the same SQL on this slot
        // while we were awaiting `client.prepare()`; keep whichever
        // entry is already there (statements are interchangeable for a
        // given SQL on a given Client) and drop ours.
        let cached = guard
            .prepared
            .entry(sql.to_string())
            .or_insert_with(|| stmt.clone())
            .clone();
        Ok(cached)
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

    /// Run a statement-shaped operation against the current client
    /// with two retry classes layered on top:
    ///
    /// 1. **Connection drop** — if the op errors and `is_closed()` is
    ///    true, force a reconnect (which clears the prepared-statement
    ///    cache, since old `Statement`s are bound to the dead client),
    ///    re-prepare the SQL on the fresh connection, and retry once.
    ///    Catches the case where the TCP socket died mid-statement (YB
    ///    tserver restart, YBA rolling restart, etc.).
    /// 2. **YB "Restart read required"** — a YugabyteDB-specific
    ///    transient surfaced on read-only metadata operations that
    ///    race a schema-change side-effect (common right after
    ///    CREATE DATABASE / DDL bursts, also seen during shard
    ///    splits and rebalances). The error means "the statement did
    ///    not execute; retry it". Bounded to 5 attempts with doubling
    ///    backoff from 200 ms. Safe to retry because the statement
    ///    by definition didn't apply.
    ///
    /// All YsqlClient public methods (`execute` / `query` /
    /// `query_opt`) go through here so every caller — pageserver,
    /// safekeeper WAL backup, YbStore blob ops, storage controller
    /// — gets the same retry semantics and prepared-statement caching
    /// without each one having to reimplement it.
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
        let mut delay = std::time::Duration::from_millis(200);
        for attempt in 1..=5u32 {
            let mut client = self.current_client_at(slot).await?;
            let stmt = match self.get_or_prepare_at(slot, &client, sql).await {
                Ok(s) => s,
                Err(e) => {
                    // prepare() failed — same retry classification as a
                    // failed op below. Connection-drop path reconnects
                    // and retries; transient YB errors backoff.
                    if client.is_closed() {
                        client = self.force_reconnect_at(slot).await?;
                        match self.get_or_prepare_at(slot, &client, sql).await {
                            Ok(s) => s,
                            Err(e2) => {
                                return Err(anyhow::Error::from(e2)).with_context(|| {
                                    format!(
                                        "YsqlClient: {op_label} prepare after reconnect ({e})"
                                    )
                                });
                            }
                        }
                    } else {
                        let msg = e.to_string();
                        if attempt < 5 && msg.contains("Restart read required") {
                            tokio::time::sleep(delay).await;
                            delay = delay.saturating_mul(2);
                            continue;
                        }
                        return Err(anyhow::Error::from(e));
                    }
                }
            };
            match f(client.clone(), stmt).await {
                Ok(v) => return Ok(v),
                Err(e) => {
                    // (1) connection drop: reconnect once and retry inside
                    // this same iteration. If the retry also fails, fall
                    // through to the transient classifier below.
                    let e = if client.is_closed() {
                        let fresh = self.force_reconnect_at(slot).await?;
                        // Re-prepare on the fresh connection.
                        let stmt = self
                            .get_or_prepare_at(slot, &fresh, sql)
                            .await
                            .map_err(anyhow::Error::from)
                            .with_context(|| {
                                format!("YsqlClient: {op_label} re-prepare after reconnect")
                            })?;
                        match f(fresh, stmt).await {
                            Ok(v) => return Ok(v),
                            Err(e2) => {
                                return Err(anyhow::Error::from(e2)).with_context(|| {
                                    format!("YsqlClient: {op_label} after reconnect ({e})")
                                });
                            }
                        }
                    } else {
                        e
                    };
                    // (2) YB "Restart read required" — match by message
                    // because tokio-postgres does not surface a stable
                    // SQLSTATE for this. Cheap string contain check.
                    let msg = e.to_string();
                    if attempt < 5 && msg.contains("Restart read required") {
                        tracing::info!(
                            attempt,
                            backoff_ms = delay.as_millis() as u64,
                            "YsqlClient: {op_label} hit YB 'Restart read required', retrying"
                        );
                        tokio::time::sleep(delay).await;
                        delay = delay.saturating_mul(2);
                        continue;
                    }
                    return Err(anyhow::Error::from(e));
                }
            }
        }
        unreachable!("retry loop returns or errors")
    }
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
    if let Some(env_name) = &cfg.password_env {
        let password = std::env::var(env_name).with_context(|| {
            format!("YsqlClient: password env var `{env_name}` is not set")
        })?;
        pg_config.password(password);
    }
    Ok(pg_config)
}

/// Build a rustls connector suitable for YugabyteDB Postgres-wire
/// connections.
///
/// - `sslrootcert = None`: load the system root store and install an
///   `AcceptAll` verifier that logs but ignores cert validation errors.
///   Suitable for `sslmode = "require"` against YBA universes that ship
///   per-universe self-signed CAs not in the system trust store.
/// - `sslrootcert = Some(path)`: load the file as a PEM-encoded CA
///   bundle and verify against it (sslmode = "verify-ca"). The file
///   must contain one or more `-----BEGIN CERTIFICATE-----` blocks.
pub fn build_yb_rustls_connector(
    sslrootcert: Option<&str>,
) -> anyhow::Result<tokio_postgres_rustls::MakeRustlsConnect> {
    use rustls::client::WebPkiServerVerifier;
    use rustls::client::danger::{ServerCertVerified, ServerCertVerifier};
    use rustls::crypto::ring;

    // Build the root store.
    let roots = if let Some(path) = sslrootcert {
        let pem = std::fs::read(path)
            .with_context(|| format!("reading sslrootcert file `{path}`"))?;
        let mut store = rustls::RootCertStore::empty();
        let mut reader = std::io::Cursor::new(pem);
        let mut added = 0usize;
        for item in rustls_pemfile::certs(&mut reader) {
            let cert = item
                .with_context(|| format!("parsing PEM cert in `{path}`"))?;
            store
                .add(cert)
                .with_context(|| format!("adding cert from `{path}` to root store"))?;
            added += 1;
        }
        if added == 0 {
            anyhow::bail!("sslrootcert `{path}` contains no PEM CERTIFICATE blocks");
        }
        Arc::new(store)
    } else {
        let der_certs = rustls_native_certs::load_native_certs();
        let mut store = rustls::RootCertStore::empty();
        store.add_parsable_certificates(der_certs.certs);
        Arc::new(store)
    };

    let provider = Arc::new(ring::default_provider());
    let builder = rustls::ClientConfig::builder_with_provider(provider.clone())
        .with_safe_default_protocol_versions()
        .expect("ring should support default protocol versions");

    let client_config = if sslrootcert.is_some() {
        // Strict verification against the supplied CA bundle.
        builder.with_root_certificates(roots).with_no_client_auth()
    } else {
        // AcceptAll: log validation failures and proceed. Mirrors the
        // storage_controller's existing behaviour.
        #[derive(Debug)]
        struct AcceptAll(Arc<WebPkiServerVerifier>);
        impl ServerCertVerifier for AcceptAll {
            fn verify_server_cert(
                &self,
                end_entity: &rustls::pki_types::CertificateDer<'_>,
                intermediates: &[rustls::pki_types::CertificateDer<'_>],
                server_name: &rustls::pki_types::ServerName<'_>,
                ocsp_response: &[u8],
                now: rustls::pki_types::UnixTime,
            ) -> Result<ServerCertVerified, rustls::Error> {
                if let Err(err) = self.0.verify_server_cert(
                    end_entity,
                    intermediates,
                    server_name,
                    ocsp_response,
                    now,
                ) {
                    tracing::info!(
                        ?server_name,
                        "YsqlClient: ignoring TLS validation error: {err:?}"
                    );
                }
                Ok(ServerCertVerified::assertion())
            }
            fn verify_tls12_signature(
                &self,
                message: &[u8],
                cert: &rustls::pki_types::CertificateDer<'_>,
                dss: &rustls::DigitallySignedStruct,
            ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
                self.0.verify_tls12_signature(message, cert, dss)
            }
            fn verify_tls13_signature(
                &self,
                message: &[u8],
                cert: &rustls::pki_types::CertificateDer<'_>,
                dss: &rustls::DigitallySignedStruct,
            ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
                self.0.verify_tls13_signature(message, cert, dss)
            }
            fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
                self.0.supported_verify_schemes()
            }
        }
        let verifier = Arc::new(AcceptAll(
            WebPkiServerVerifier::builder_with_provider(roots, provider).build()?,
        ));
        builder
            .dangerous()
            .with_custom_certificate_verifier(verifier)
            .with_no_client_auth()
    };

    Ok(tokio_postgres_rustls::MakeRustlsConnect::new(client_config))
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

async fn open_rustls(
    config: &tokio_postgres::Config,
    tls: &tokio_postgres_rustls::MakeRustlsConnect,
) -> Result<tokio_postgres::Client> {
    let (client, connection) = config
        .connect(tls.clone())
        .await
        .context("YsqlClient: connect (Rustls)")?;
    tokio::spawn(async move {
        if let Err(e) = connection.await {
            tracing::warn!("YsqlClient connection (Rustls) ended: {e}");
        }
    });
    let _ = client
        .execute("SET yb_enable_upsert_mode = true", &[])
        .await;
    Ok(client)
}
