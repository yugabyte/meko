//! Auto-reconnecting wrapper around `tokio_postgres::Client`.
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
//!   2. Runs the operation.
//!   3. On error, checks `is_closed()` again; if true, forces a
//!      reconnect and retries **once**. This catches the case where the
//!      connection dropped mid-statement.
//!
//! The mutex is held only across the cheap `is_closed()` check and the
//! reconnect itself; the `Arc<Client>` is cloned out before the real DB
//! operation runs, so multiple writers hit `tokio_postgres`'s internal
//! statement multiplexing concurrently with no extra serialization.
//!
//! For streaming reads (`query_raw`) and other Client APIs not mirrored
//! here, callers should use `current()` which returns the live
//! `Arc<Client>` (reconnecting first if it's dead). The retry-once
//! semantics don't compose cleanly with streams — caller handles those.

use std::sync::Arc;

use anyhow::{Context, Result};
use futures::FutureExt;
use futures::future::BoxFuture;
use tokio_postgres::binary_copy::BinaryCopyInWriter;
use tokio_postgres::types::{ToSql, Type};

use crate::config::YbStoreConfig;

/// Future returned by the reconnect closure.
type OpenFut = BoxFuture<'static, Result<tokio_postgres::Client>>;

/// Closure that opens a fresh `tokio_postgres::Client` against the
/// configured cluster + TLS mode.
type OpenFn = Arc<dyn Fn() -> OpenFut + Send + Sync>;

pub struct YsqlClient {
    open: OpenFn,
    current: tokio::sync::Mutex<Arc<tokio_postgres::Client>>,
}

impl YsqlClient {
    /// Build a `YsqlClient` that connects without TLS. Opens the
    /// initial connection eagerly so config errors surface at construction.
    pub async fn connect_no_tls(config: tokio_postgres::Config) -> Result<Self> {
        let config = Arc::new(config);
        let open: OpenFn = {
            let config = config.clone();
            Arc::new(move || {
                let config = config.clone();
                async move { open_no_tls(&config).await }.boxed()
            })
        };
        let client = (open)().await?;
        Ok(Self::from_parts(open, client))
    }

    /// Build a `YsqlClient` that connects via the supplied rustls
    /// connector (typically built with an AcceptAll verifier — see
    /// the YbStore-side helper). Opens the initial connection eagerly.
    pub async fn connect_rustls(
        config: tokio_postgres::Config,
        tls: tokio_postgres_rustls::MakeRustlsConnect,
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
        let client = (open)().await?;
        Ok(Self::from_parts(open, client))
    }

    fn from_parts(open: OpenFn, client: tokio_postgres::Client) -> Self {
        Self {
            open,
            current: tokio::sync::Mutex::new(Arc::new(client)),
        }
    }

    /// Return the live client, reconnecting first if `is_closed()` is
    /// already true. Use for streaming reads (`query_raw`) or other
    /// Client APIs not mirrored here. The next call after a failed
    /// streaming op will pick up a fresh connection automatically.
    pub async fn current(&self) -> Result<Arc<tokio_postgres::Client>> {
        let mut guard = self.current.lock().await;
        if guard.is_closed() {
            tracing::info!("YsqlClient: previous client closed, reconnecting");
            *guard = Arc::new((self.open)().await?);
        }
        Ok(guard.clone())
    }

    /// Force a reconnect after an op observed `is_closed()`. Double-checks
    /// inside the mutex in case another task already reconnected while
    /// we were waiting on the lock.
    async fn force_reconnect(&self) -> Result<Arc<tokio_postgres::Client>> {
        let mut guard = self.current.lock().await;
        if !guard.is_closed() {
            return Ok(guard.clone());
        }
        tracing::info!("YsqlClient: forcing reconnect after failed op");
        *guard = Arc::new((self.open)().await?);
        Ok(guard.clone())
    }

    pub async fn execute(
        &self,
        sql: &str,
        params: &[&(dyn ToSql + Sync)],
    ) -> Result<u64> {
        let client = self.current().await?;
        match client.execute(sql, params).await {
            Ok(n) => Ok(n),
            Err(e) if client.is_closed() => {
                drop(client);
                let client = self.force_reconnect().await?;
                client
                    .execute(sql, params)
                    .await
                    .with_context(|| format!("YsqlClient: execute after reconnect ({e})"))
            }
            Err(e) => Err(anyhow::Error::from(e)),
        }
    }

    pub async fn query(
        &self,
        sql: &str,
        params: &[&(dyn ToSql + Sync)],
    ) -> Result<Vec<tokio_postgres::Row>> {
        let client = self.current().await?;
        match client.query(sql, params).await {
            Ok(rows) => Ok(rows),
            Err(e) if client.is_closed() => {
                drop(client);
                let client = self.force_reconnect().await?;
                client
                    .query(sql, params)
                    .await
                    .with_context(|| format!("YsqlClient: query after reconnect ({e})"))
            }
            Err(e) => Err(anyhow::Error::from(e)),
        }
    }

    /// Open a binary-COPY stream against `sql` (`COPY ... FROM STDIN
    /// BINARY`). Caller writes rows via the returned writer and calls
    /// `finish().await`. No reconnect-and-retry: if the connection
    /// dies mid-stream, the error propagates; the caller logs and
    /// the next batch reopens. (Mid-stream replay is impossible —
    /// we'd have to remember every row already sent.)
    pub async fn copy_in_binary(
        &self,
        sql: &str,
        types: &[Type],
    ) -> Result<BinaryCopyInWriter> {
        let client = self.current().await?;
        let sink = client
            .copy_in(sql)
            .await
            .with_context(|| format!("YsqlClient: copy_in failed for {sql}"))?;
        Ok(BinaryCopyInWriter::new(sink, types))
    }

    pub async fn query_opt(
        &self,
        sql: &str,
        params: &[&(dyn ToSql + Sync)],
    ) -> Result<Option<tokio_postgres::Row>> {
        let client = self.current().await?;
        match client.query_opt(sql, params).await {
            Ok(row) => Ok(row),
            Err(e) if client.is_closed() => {
                drop(client);
                let client = self.force_reconnect().await?;
                client
                    .query_opt(sql, params)
                    .await
                    .with_context(|| format!("YsqlClient: query_opt after reconnect ({e})"))
            }
            Err(e) => Err(anyhow::Error::from(e)),
        }
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
        let pg_config = build_pg_config(cfg, application_name)?;
        match cfg.sslmode.as_deref() {
            None | Some("disable") => Self::connect_no_tls(pg_config).await,
            Some(_mode) => {
                let tls = build_yb_rustls_connector(cfg.sslrootcert.as_deref())?;
                Self::connect_rustls(pg_config, tls).await
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
