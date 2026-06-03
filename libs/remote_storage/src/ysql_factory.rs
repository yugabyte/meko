//! High-level `YsqlClient` factory keyed on [`YbStoreConfig`].
//!
//! Lives in `remote_storage` rather than the [`ysql_client`] crate
//! itself because [`YbStoreConfig`] is owned here — keeping the
//! ysql_client crate dependency-light (just `yb-tokio-postgres` +
//! TLS).

use anyhow::{Context, Result};
use ysql_client::{YsqlClient, build_yb_rustls_connector, tokio_postgres};

use crate::config::YbStoreConfig;

/// Build a `YsqlClient` from a `YbStoreConfig`. Single connection.
/// Handles `password_env` lookup, `application_name`, and the
/// `sslmode` / `sslrootcert` dispatch.
///
/// Both the pageserver's `YbStore` and the safekeeper's WAL-mirror
/// call this so they agree on TLS semantics — without this shared
/// factory, the WAL mirror used to hardcode `NoTls` regardless of
/// `sslmode` and would silently fail against any YBA universe
/// configured with TLS-only `pg_hba` rules.
///
/// `set_upserts` enables `SET yb_enable_upsert_mode = true` on each
/// new connection — see [`YsqlClient::connect_no_tls_pooled`].
pub async fn from_yb_config(
    cfg: &YbStoreConfig,
    application_name: &str,
    set_upserts: bool,
) -> Result<YsqlClient> {
    from_yb_config_pooled(cfg, application_name, 1, set_upserts).await
}

/// Same as [`from_yb_config`] but opens `pool_size` independent
/// connections. Use for hot read paths that benefit from parallel
/// SQL execution (e.g. pageserver compute_page_store at pool_size=4).
pub async fn from_yb_config_pooled(
    cfg: &YbStoreConfig,
    application_name: &str,
    pool_size: usize,
    set_upserts: bool,
) -> Result<YsqlClient> {
    let pg_config = build_pg_config(cfg, application_name)?;
    match cfg.sslmode.as_deref() {
        None | Some("disable") => {
            YsqlClient::connect_no_tls_pooled(pg_config, pool_size, set_upserts).await
        }
        Some(_mode) => {
            let tls = build_yb_rustls_connector(cfg.sslrootcert.as_deref())?;
            YsqlClient::connect_rustls_pooled(pg_config, tls, pool_size, set_upserts).await
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
    // NOTE: smart-driver load balancing is currently disabled. Setting
    // `pg_config.load_balance("only-primary")` causes
    // `yb-tokio-postgres 0.7.10-yb-1-beta.3` to fail every connect
    // attempt with "could not create control connection" — the smart
    // driver opens an internal "control connection" to run
    // `yb_servers()` for discovery, and that path doesn't work in the
    // current YB fork. Direct queries against the same target via the
    // same TLS connector succeed (verified 2026-06-02 against
    // 172.151.17.132:5433). Filed alongside the openssl handshake
    // issue. Until both are fixed upstream we open each pool slot to
    // the configured seed host; pool size still gives us parallel SQL
    // via independent connections, just not tserver-spread.
    //
    // pg_config.load_balance("only-primary");
    if let Some(env_name) = &cfg.password_env {
        let password = std::env::var(env_name).with_context(|| {
            format!("YsqlClient: password env var `{env_name}` is not set")
        })?;
        pg_config.password(password);
    }
    Ok(pg_config)
}
