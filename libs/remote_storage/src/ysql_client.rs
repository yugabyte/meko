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
use tokio_postgres::types::ToSql;

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
