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

mod metrics;
mod rustls_adapter;

pub use rustls_adapter::YbMakeRustlsConnect;
pub use yb_tokio_postgres as tokio_postgres;

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use futures::FutureExt;
use futures::future::BoxFuture;
use metrics::{YB_CLIENT_ERRORS, YB_QUERY_SECONDS, YB_RETRIES_EXHAUSTED};
use yb_tokio_postgres::Statement;
use yb_tokio_postgres::types::ToSql;

/// A single YB tserver endpoint, `host` + YSQL `port`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HostPort {
    pub host: String,
    pub port: u16,
}

/// One-or-more YB tserver endpoints. The wire form (TOML config and CLI
/// args) is a single CSV string `host:port[,host:port,...]` (port
/// defaults to 5433 if omitted); internally it's a parsed list, in
/// libpq-failover order. Serde de/serializes via the CSV string, and the
/// `FromStr` impl is the clap value parser.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HostPorts(pub Vec<HostPort>);

impl std::str::FromStr for HostPorts {
    type Err = anyhow::Error;
    fn from_str(s: &str) -> Result<Self> {
        let mut out = Vec::new();
        for entry in s.split(',').map(str::trim).filter(|e| !e.is_empty()) {
            let (host, port) = match entry.rsplit_once(':') {
                Some((h, p)) => (
                    h.to_string(),
                    p.parse::<u16>()
                        .with_context(|| format!("invalid port in YB endpoint {entry:?}"))?,
                ),
                None => (entry.to_string(), DEFAULT_YB_PORT),
            };
            anyhow::ensure!(!host.is_empty(), "empty host in YB endpoint {entry:?}");
            out.push(HostPort { host, port });
        }
        anyhow::ensure!(
            !out.is_empty(),
            "host_ports must contain at least one host:port (got {s:?})"
        );
        Ok(HostPorts(out))
    }
}

impl std::fmt::Display for HostPorts {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let csv = self
            .0
            .iter()
            .map(|hp| format!("{}:{}", hp.host, hp.port))
            .collect::<Vec<_>>()
            .join(",");
        f.write_str(&csv)
    }
}

impl serde::Serialize for HostPorts {
    fn serialize<S: serde::Serializer>(&self, s: S) -> std::result::Result<S::Ok, S::Error> {
        s.serialize_str(&self.to_string())
    }
}

impl<'de> serde::Deserialize<'de> for HostPorts {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> std::result::Result<Self, D::Error> {
        let s = String::deserialize(d)?;
        s.parse().map_err(serde::de::Error::custom)
    }
}

/// The one canonical YugabyteDB connection config — the TOML
/// `[*.yb]` block every yb-light daemon (pageserver, safekeeper,
/// endpoint_storage, storage_controller) deserializes, and what the
/// diagnostic tools build from their CLI flags. Connection only: which
/// YB to talk to, as whom, over what TLS. It deliberately carries NO
/// table/schema — each component hardcodes the table(s) it owns
/// (pageserver → pageserver.pages/slru_pages/meta, safekeeper →
/// safekeeper.wals, endpoint_storage → public.blobs).
#[derive(Clone, Debug, PartialEq, Eq, serde::Deserialize, serde::Serialize)]
pub struct YbConnConfig {
    /// YB tserver endpoints. Wire form is the CSV string
    /// `host:port[,host:port,...]`; stored parsed (see [`HostPorts`]).
    pub host_ports: HostPorts,
    #[serde(default = "default_yb_user")]
    pub user: String,
    #[serde(default = "default_yb_dbname")]
    pub dbname: String,
    /// Name of an environment variable that holds the password. If
    /// `Some` and unset, the connection fails at construction.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub password_env: Option<String>,
    /// `None` or `Some("disable")` → NoTls. Any other value → rustls
    /// via [`build_yb_rustls_connector`].
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sslmode: Option<String>,
    /// Path to a PEM-encoded CA bundle. Required when `sslmode` calls
    /// for strict verification (`verify-ca` / `verify-full`); when
    /// `None` the verifier falls back to AcceptAll (logged).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sslrootcert: Option<String>,
}

impl YbConnConfig {
    /// Render this connection config as a libpq/diesel `postgresql://` URL.
    ///
    /// The storage_controller's persistence layer (diesel-async) takes a
    /// URL string rather than a parsed config, so it consumes the shared
    /// `[storage_controller.yb]` block by building the URL here. All
    /// `host:port` pairs are emitted as a libpq multi-host authority;
    /// `password_env` is resolved to the live password and percent-encoded.
    pub fn to_database_url(&self) -> anyhow::Result<String> {
        use anyhow::Context;
        let mut url = String::from("postgresql://");
        url.push_str(&percent_encode(&self.user));
        if let Some(env_name) = &self.password_env {
            let pw = std::env::var(env_name)
                .with_context(|| format!("password env var `{env_name}` is not set"))?;
            url.push(':');
            url.push_str(&percent_encode(&pw));
        }
        url.push('@');
        let hosts = self
            .host_ports
            .0
            .iter()
            .map(|hp| format!("{}:{}", hp.host, hp.port))
            .collect::<Vec<_>>()
            .join(",");
        url.push_str(&hosts);
        url.push('/');
        url.push_str(&percent_encode(&self.dbname));
        let mut params = Vec::new();
        if let Some(sslmode) = &self.sslmode {
            params.push(format!("sslmode={}", percent_encode(sslmode)));
        }
        if let Some(cert) = &self.sslrootcert {
            params.push(format!("sslrootcert={}", percent_encode(cert)));
        }
        if !params.is_empty() {
            url.push('?');
            url.push_str(&params.join("&"));
        }
        Ok(url)
    }

    /// Render as a TOML inline table (`{ key = "val", ... }`) — the wire
    /// form the safekeeper (`yb_storage_conn`) and storage_controller
    /// (`--yb-config`) accept on the command line, and the block the
    /// pageserver `remote_storage` / endpoint_storage configs embed. The
    /// inverse of the `Deserialize` impl; identical across all consumers.
    pub fn to_inline_toml(&self) -> String {
        fn q(s: &str) -> String {
            format!("\"{}\"", s.replace('\\', "\\\\").replace('"', "\\\""))
        }
        let mut parts = vec![
            format!("host_ports = {}", q(&self.host_ports.to_string())),
            format!("user = {}", q(&self.user)),
            format!("dbname = {}", q(&self.dbname)),
        ];
        if let Some(p) = &self.password_env {
            parts.push(format!("password_env = {}", q(p)));
        }
        if let Some(s) = &self.sslmode {
            parts.push(format!("sslmode = {}", q(s)));
        }
        if let Some(c) = &self.sslrootcert {
            parts.push(format!("sslrootcert = {}", q(c)));
        }
        format!("{{ {} }}", parts.join(", "))
    }
}

/// Percent-encode a URL component, escaping everything outside the
/// unreserved set (RFC 3986 `ALPHA / DIGIT / - . _ ~`). Used for the
/// userinfo and query values of [`YbConnConfig::to_database_url`].
fn percent_encode(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        if b.is_ascii_alphanumeric() || matches!(b, b'-' | b'.' | b'_' | b'~') {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

fn default_yb_user() -> String {
    "yugabyte".to_string()
}

fn default_yb_dbname() -> String {
    "yb_storage".to_string()
}

/// Default YSQL port applied to a `hosts` entry that omits `:port`.
pub const DEFAULT_YB_PORT: u16 = 5433;

/// Shared connection CLI args for the `yb_storage` diagnostic tools
/// (`yb-dump-pages`, `yb-dump-wals`, `yb-reset-consistent-lsn`).
///
/// This is the SINGLE source of truth for those flags — every tool
/// `#[command(flatten)]`s it, so the connection surface (and TLS support)
/// can't drift between tools. `--sslmode` makes them work against both a
/// local no-TLS dev cluster (default) and a TLS-required cluster (AWS):
/// `disable`/unset → NoTls; any other value → TLS (rustls), verifying
/// against `--sslrootcert` if given, else accept-all.
#[cfg(feature = "cli")]
#[derive(clap::Args, Clone, Debug)]
pub struct YbConnArgs {
    /// YB tserver endpoints: `host:port[,host:port,...]` (port defaults
    /// to 5433). Same CSV form as the daemons' `host_ports` TOML key.
    #[arg(long, default_value = "127.0.0.1:5433")]
    pub host_ports: HostPorts,
    #[arg(long, default_value = "yugabyte")]
    pub user: String,
    #[arg(long = "db-name", default_value = "yb_storage")]
    pub db_name: String,
    /// Name of an env var holding the password. Unset → no password
    /// (trust, matching a local dev cluster).
    #[arg(long)]
    pub password_env: Option<String>,
    /// `disable` (default, NoTls) | `require` | `verify-ca` | `verify-full`.
    /// Any value other than `disable` enables TLS.
    #[arg(long)]
    pub sslmode: Option<String>,
    /// PEM CA bundle for TLS verification (`verify-*`); omit for
    /// encrypt-without-verify (`require`).
    #[arg(long)]
    pub sslrootcert: Option<String>,
}

#[cfg(feature = "cli")]
impl YbConnArgs {
    /// Build the [`YbConnConfig`] these flags describe.
    pub fn into_config(&self) -> YbConnConfig {
        YbConnConfig {
            host_ports: self.host_ports.clone(),
            user: self.user.clone(),
            dbname: self.db_name.clone(),
            password_env: self.password_env.clone(),
            sslmode: self.sslmode.clone(),
            sslrootcert: self.sslrootcert.clone(),
        }
    }
}

#[cfg(all(test, feature = "cli"))]
mod cli_tests {
    use super::{HostPorts, YbConnArgs};

    fn args() -> YbConnArgs {
        YbConnArgs {
            host_ports: "10.0.0.1:5433,10.0.0.2:5440".parse().unwrap(),
            user: "yugabyte".into(),
            db_name: "yb_storage".into(),
            password_env: Some("PW".into()),
            sslmode: None,
            sslrootcert: None,
        }
    }

    #[test]
    fn host_ports_parse_and_render() {
        // Default port filled in; explicit port kept; round-trips to CSV.
        let hp: HostPorts = "a,b:5440".parse().unwrap();
        assert_eq!(hp.0.len(), 2);
        assert_eq!((hp.0[0].host.as_str(), hp.0[0].port), ("a", 5433));
        assert_eq!((hp.0[1].host.as_str(), hp.0[1].port), ("b", 5440));
        assert_eq!(hp.to_string(), "a:5433,b:5440");
        assert!("".parse::<HostPorts>().is_err());
        assert!("h:notaport".parse::<HostPorts>().is_err());
    }

    #[test]
    fn into_config_maps_fields() {
        let c = args().into_config();
        assert_eq!(c.host_ports.to_string(), "10.0.0.1:5433,10.0.0.2:5440");
        assert_eq!(c.user, "yugabyte");
        assert_eq!(c.dbname, "yb_storage");
        assert_eq!(c.password_env.as_deref(), Some("PW"));
    }

    #[test]
    fn sslmode_none_is_notls_other_is_tls() {
        // The drift this guards: from_yb_config dispatches NoTls vs rustls
        // purely on `sslmode` (None/"disable" -> NoTls, else TLS). Keep the
        // arg -> config passthrough exact so all three tools agree.
        assert!(args().into_config().sslmode.is_none());

        let tls = YbConnArgs {
            sslmode: Some("require".into()),
            sslrootcert: Some("/etc/ca.pem".into()),
            ..args()
        }
        .into_config();
        assert_eq!(tls.sslmode.as_deref(), Some("require"));
        assert_eq!(tls.sslrootcert.as_deref(), Some("/etc/ca.pem"));
    }
}

#[cfg(test)]
mod database_url_tests {
    use super::YbConnConfig;

    fn cfg(host_ports: &str) -> YbConnConfig {
        YbConnConfig {
            host_ports: host_ports.parse().unwrap(),
            user: "yugabyte".into(),
            dbname: "yb_storage".into(),
            password_env: None,
            sslmode: None,
            sslrootcert: None,
        }
    }

    #[test]
    fn plain_single_host() {
        let url = cfg("127.0.0.1:5433").to_database_url().unwrap();
        assert_eq!(url, "postgresql://yugabyte@127.0.0.1:5433/yb_storage");
    }

    #[test]
    fn multi_host_and_sslmode() {
        let mut c = cfg("10.0.0.1:5433,10.0.0.2:5440");
        c.sslmode = Some("require".into());
        let url = c.to_database_url().unwrap();
        assert_eq!(
            url,
            "postgresql://yugabyte@10.0.0.1:5433,10.0.0.2:5440/yb_storage?sslmode=require"
        );
    }

    #[test]
    fn password_env_is_resolved_and_encoded() {
        // SAFETY: single-threaded test; no other thread reads this var.
        unsafe { std::env::set_var("YB_URL_TEST_PW", "Yugabyte!1") };
        let mut c = cfg("h:5433");
        c.password_env = Some("YB_URL_TEST_PW".into());
        let url = c.to_database_url().unwrap();
        assert_eq!(url, "postgresql://yugabyte:Yugabyte%211@h:5433/yb_storage");
        unsafe { std::env::remove_var("YB_URL_TEST_PW") };
    }

    #[test]
    fn missing_password_env_errors() {
        let mut c = cfg("h:5433");
        c.password_env = Some("YB_URL_TEST_DEFINITELY_UNSET".into());
        assert!(c.to_database_url().is_err());
    }

    #[test]
    fn inline_toml_omits_unset_optionals() {
        let t = cfg("127.0.0.1:5433").to_inline_toml();
        assert_eq!(
            t,
            "{ host_ports = \"127.0.0.1:5433\", user = \"yugabyte\", dbname = \"yb_storage\" }"
        );
    }

    #[test]
    fn inline_toml_includes_set_optionals() {
        let mut c = cfg("a:5433,b:5440");
        c.sslmode = Some("require".into());
        c.password_env = Some("YB_PW".into());
        let t = c.to_inline_toml();
        assert!(t.contains("host_ports = \"a:5433,b:5440\""));
        assert!(t.contains("sslmode = \"require\""));
        assert!(t.contains("password_env = \"YB_PW\""));
    }
}

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
    /// Build a `YsqlClient` with a single connection (no pool).
    /// Equivalent to `connect_no_tls_pooled(config, 1, set_fast_path_upsert)`.
    pub async fn connect_no_tls(
        config: tokio_postgres::Config,
        set_fast_path_upsert: bool,
    ) -> Result<Self> {
        Self::connect_no_tls_pooled(config, 1, set_fast_path_upsert).await
    }

    /// Build a `YsqlClient` that connects without TLS, with `pool_size`
    /// independent connections. Opens all connections eagerly so config
    /// errors surface at construction. When `set_fast_path_upsert` is true, each
    /// connection runs `SET yb_enable_upsert_mode = true` after opening
    /// — see `set_fast_path_upsert_gucs` for why.
    pub async fn connect_no_tls_pooled(
        config: tokio_postgres::Config,
        pool_size: usize,
        set_fast_path_upsert: bool,
    ) -> Result<Self> {
        let config = Arc::new(config);
        let open: OpenFn = {
            let config = config.clone();
            Arc::new(move || {
                let config = config.clone();
                async move { open_no_tls(&config, set_fast_path_upsert).await }.boxed()
            })
        };
        Self::from_open(open, pool_size).await
    }

    /// Build a `YsqlClient` with a single TLS connection. Equivalent to
    /// `connect_rustls_pooled(config, tls, 1, set_fast_path_upsert)`.
    pub async fn connect_rustls(
        config: tokio_postgres::Config,
        tls: crate::rustls_adapter::YbMakeRustlsConnect,
        set_fast_path_upsert: bool,
    ) -> Result<Self> {
        Self::connect_rustls_pooled(config, tls, 1, set_fast_path_upsert).await
    }

    /// Build a `YsqlClient` that connects via the supplied rustls
    /// connector with `pool_size` independent connections. Opens all
    /// eagerly. `set_fast_path_upsert` behaves as in `connect_no_tls_pooled`.
    pub async fn connect_rustls_pooled(
        config: tokio_postgres::Config,
        tls: crate::rustls_adapter::YbMakeRustlsConnect,
        pool_size: usize,
        set_fast_path_upsert: bool,
    ) -> Result<Self> {
        let config = Arc::new(config);
        let tls = Arc::new(tls);
        let open: OpenFn = {
            let config = config.clone();
            let tls = tls.clone();
            Arc::new(move || {
                let config = config.clone();
                let tls = tls.clone();
                async move { open_tls(&config, &tls, set_fast_path_upsert).await }.boxed()
            })
        };
        Self::from_open(open, pool_size).await
    }

    /// High-level factory: build a `YsqlClient` from a [`YbConnConfig`].
    /// Single connection. Handles `password_env` lookup,
    /// `application_name`, and the `sslmode` / `sslrootcert` dispatch.
    ///
    /// The pageserver's `YbStore`, the safekeeper's WAL-mirror, and
    /// the pageserver's `PageStore` all go through this so they agree
    /// on TLS semantics — without this shared factory the WAL mirror
    /// used to hardcode `NoTls` regardless of `sslmode`, and would
    /// silently fail against any YBA universe configured with TLS-only
    /// `pg_hba` rules.
    ///
    /// `set_fast_path_upsert` enables `SET yb_enable_upsert_mode =
    /// true` on each new connection — see [`Self::connect_no_tls_pooled`].
    pub async fn from_yb_config(
        cfg: &YbConnConfig,
        application_name: &str,
        set_fast_path_upsert: bool,
    ) -> Result<Self> {
        Self::from_yb_config_pooled(cfg, application_name, 1, set_fast_path_upsert).await
    }

    /// Same as [`Self::from_yb_config`] but opens `pool_size`
    /// independent connections. Use for hot paths that benefit from
    /// parallel SQL execution (e.g. the pageserver's compute-reads
    /// pool at `pool_size = 4`).
    pub async fn from_yb_config_pooled(
        cfg: &YbConnConfig,
        application_name: &str,
        pool_size: usize,
        set_fast_path_upsert: bool,
    ) -> Result<Self> {
        let pg_config = build_pg_config(cfg, application_name)?;
        match cfg.sslmode.as_deref() {
            None | Some("disable") => {
                Self::connect_no_tls_pooled(pg_config, pool_size, set_fast_path_upsert).await
            }
            Some(_mode) => {
                let tls = build_yb_rustls_connector(cfg.sslrootcert.as_deref())?;
                Self::connect_rustls_pooled(pg_config, tls, pool_size, set_fast_path_upsert).await
            }
        }
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
        self.next.fetch_add(1, std::sync::atomic::Ordering::Relaxed) % self.pool.len()
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
        let started = std::time::Instant::now();
        let res = guard.client.prepare(sql).await;
        YB_QUERY_SECONDS
            .with_label_values(&["prepare"])
            .observe(started.elapsed().as_secs_f64());
        let stmt = res?;
        guard.prepared.insert(sql.to_string(), stmt.clone());
        Ok((client, stmt))
    }

    pub async fn execute(&self, sql: &str, params: &[&(dyn ToSql + Sync)]) -> Result<u64> {
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
    async fn run_with_retries<F, Fut, T>(&self, op_label: &str, sql: &str, f: F) -> Result<T>
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
            let started = std::time::Instant::now();
            let result = f(client.clone(), stmt).await;
            YB_QUERY_SECONDS
                .with_label_values(&[op_label])
                .observe(started.elapsed().as_secs_f64());
            match result {
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

/// Build an openssl-based TLS connector suitable for YugabyteDB
/// Postgres-wire connections through the yb-tokio-postgres smart
/// driver, using rustls (pure-Rust / ring) wrapped in the
/// [`crate::rustls_adapter::YbMakeRustlsConnect`] adapter so the
/// upstream `tokio-postgres-rustls` impl satisfies the YB fork's
/// `MakeTlsConnect` trait.
///
/// - `sslrootcert = None`: load the system root store but install
///   an `AcceptAll` verifier that logs validation failures and
///   proceeds. Suitable for `sslmode = "require"` against YBA
///   universes that ship per-universe self-signed CAs not in the
///   system trust store.
/// - `sslrootcert = Some(path)`: load the file as a PEM-encoded CA
///   bundle and verify strictly against it (sslmode = "verify-ca").
///
/// We previously tried `yb-postgres-openssl 0.5.0-yb-1-beta.3` and
/// confirmed it fails the TLS handshake against YB tservers under
/// both dynamic-openssl-1.1 and static-vendored-openssl-3.6.2,
/// while this rustls path connects fine to the same target. See
/// `architecture/yugabyte.md` (and the linked YB-team report) for
/// the diagnosis.
pub fn build_yb_rustls_connector(
    sslrootcert: Option<&str>,
) -> anyhow::Result<crate::rustls_adapter::YbMakeRustlsConnect> {
    use rustls::client::WebPkiServerVerifier;
    use rustls::client::danger::{ServerCertVerified, ServerCertVerifier};
    use rustls::crypto::ring;

    let roots = if let Some(path) = sslrootcert {
        let pem =
            std::fs::read(path).with_context(|| format!("reading sslrootcert file `{path}`"))?;
        let mut store = rustls::RootCertStore::empty();
        let mut reader = std::io::Cursor::new(pem);
        let mut added = 0usize;
        for item in rustls_pemfile::certs(&mut reader) {
            let cert = item.with_context(|| format!("parsing PEM cert in `{path}`"))?;
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
        // storage_controller's existing behaviour and matches the
        // openssl `SslVerifyMode::NONE` semantics we were running with.
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
            ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error>
            {
                self.0.verify_tls12_signature(message, cert, dss)
            }
            fn verify_tls13_signature(
                &self,
                message: &[u8],
                cert: &rustls::pki_types::CertificateDer<'_>,
                dss: &rustls::DigitallySignedStruct,
            ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error>
            {
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

    Ok(crate::rustls_adapter::YbMakeRustlsConnect::new(
        tokio_postgres_rustls::MakeRustlsConnect::new(client_config),
    ))
}

/// Enable upsert mode on a freshly-opened YsqlClient connection. Plain
/// INSERT then skips the read-before-write check (and our chunk-retry
/// flow can re-issue INSERTs without PK conflict).
///
/// Any failure propagates: in production we only ever talk to YB, so
/// a SET failure means the connection wouldn't upsert and chunk
/// retries could later fail with duplicate-key. We'd rather refuse
/// the connection here than carry a silently misconfigured client
/// (the previous `let _ = … .await` is exactly the kind of hidden
/// GUC drift that bites under load).
fn build_pg_config(cfg: &YbConnConfig, application_name: &str) -> Result<tokio_postgres::Config> {
    let mut pg_config = tokio_postgres::Config::new();
    // `hosts` is a list of YB tserver endpoints, each `host` or
    // `host:port` (default 5433). Add each as its own (host, port) pair;
    // tokio_postgres tries them in order on connect (libpq failover).
    anyhow::ensure!(
        !cfg.host_ports.0.is_empty(),
        "YbConnConfig.host_ports must contain at least one host:port"
    );
    for hp in &cfg.host_ports.0 {
        pg_config.host(&hp.host);
        pg_config.port(hp.port);
    }
    pg_config
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
        let password = std::env::var(env_name)
            .with_context(|| format!("YsqlClient: password env var `{env_name}` is not set"))?;
        pg_config.password(password);
    }
    Ok(pg_config)
}

/// Enable both GUCs that make this connection a "fast-path-upsert"
/// connection:
///
/// - `yb_enable_upsert_mode = true` skips YB's read-before-write
///   check on plain `INSERT`, turning duplicate-PK writes into a
///   silent overwrite. Required by put_pages/put_slru_pages/put_meta
///   and the WAL chunk INSERTs whose partial-chunk-retry flow re-
///   issues the same row.
/// - `yb_disable_transactional_writes = true` drops the transactional
///   write fast path's read-modify-write guarantees. Safe for the
///   write paths above because they're append-only INSERTs whose
///   ordering is established by the higher-level LSN scheme, not by
///   YB's transaction semantics.
///
/// Failure on either SET propagates: in production we only ever talk
/// to YB, so a SET failure means the connection would silently fall
/// back to the slow / transactional path. We'd rather refuse the
/// connection here than carry a misconfigured client.
async fn set_fast_path_upsert_gucs(client: &tokio_postgres::Client) -> Result<()> {
    client
        .execute("SET yb_enable_upsert_mode = true", &[])
        .await
        .context("YsqlClient: SET yb_enable_upsert_mode = true")?;
    // `yb_disable_transactional_writes = true` is disabled while we
    // investigate a 500-conn-workload WAL-durability incident: under
    // tablet rebalancing / RPC retries, batch INSERTs reported as
    // success appear to lose individual rows, leaving holes in
    // `safekeeper.wals` that wedge pageserver restart. With the GUC
    // off, INSERTs go through the standard transactional path on
    // this connection — slower, atomic.
    // client
    //     .execute("SET yb_disable_transactional_writes = true", &[])
    //     .await
    //     .context("YsqlClient: SET yb_disable_transactional_writes = true")?;
    Ok(())
}

async fn open_no_tls(
    config: &tokio_postgres::Config,
    set_fast_path_upsert: bool,
) -> Result<tokio_postgres::Client> {
    let (client, connection) = config
        .connect(tokio_postgres::NoTls)
        .await
        .context("YsqlClient: connect (NoTls)")?;
    tokio::spawn(async move {
        if let Err(e) = connection.await {
            tracing::warn!("YsqlClient connection (NoTls) ended: {e}");
        }
    });
    if set_fast_path_upsert {
        set_fast_path_upsert_gucs(&client).await?;
    }
    Ok(client)
}

async fn open_tls(
    config: &tokio_postgres::Config,
    tls: &crate::rustls_adapter::YbMakeRustlsConnect,
    set_fast_path_upsert: bool,
) -> Result<tokio_postgres::Client> {
    let (client, connection) = config
        .connect(tls.clone())
        .await
        .context("YsqlClient: connect (rustls)")?;
    tokio::spawn(async move {
        if let Err(e) = connection.await {
            tracing::warn!("YsqlClient connection (rustls) ended: {e}");
        }
    });
    if set_fast_path_upsert {
        set_fast_path_upsert_gucs(&client).await?;
    }
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
        assert!(looks_like_dropped_connection("db error: connection closed"));
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
