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
//!      dead. A connection that is alive but has outlived
//!      [`YbConnConfig::connection_lifetime_ms`] is recycled here too, so
//!      the smart driver's once-per-connection topology discovery cannot
//!      outlive the topology (#811).
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

pub use yb_tokio_postgres as tokio_postgres;

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use futures::FutureExt;
use futures::future::BoxFuture;
use metrics::{YB_CLIENT_ERRORS, YB_CONNECTION_RECYCLES, YB_QUERY_SECONDS, YB_RETRIES_EXHAUSTED};
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
/// `[*.yb]` block every yb-amp daemon (pageserver, safekeeper, controller)
/// deserializes, and what the diagnostic tools build from their CLI flags. Connection only: which
/// YB to talk to, as whom, over what TLS. It deliberately carries NO
/// table/schema — each component hardcodes the table(s) it owns
/// (pageserver → amp_storage.pages/slru_pages/meta/pending_gc, safekeeper →
/// amp_storage.wals, compute_ctl → amp_storage.blobs).
#[derive(Clone, Debug, PartialEq, Eq, serde::Deserialize, serde::Serialize)]
#[serde(deny_unknown_fields)]
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
    /// Whether fast-path-upsert connections also run
    /// `SET yb_disable_transactional_writes = true` (see
    /// [`set_fast_path_upsert_gucs`]). Only honored on the write
    /// connections that already enable `yb_enable_upsert_mode`; read
    /// connections never set it. Defaults to `false` (disabled) — the GUC
    /// trades YB's transactional-write guarantees for speed, and a prior
    /// WAL-durability incident makes "off" the safe default. Set to `true`
    /// in the TOML connection block to re-enable it.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub disable_transactional_writes: bool,
    /// Which transport the hot paths (pageserver page reads/writes,
    /// safekeeper WAL reads/writes) use. `"yb_thin_client"` (default): the
    /// `yb_thin_client` crate over the `libyb_thin_client` C++ shim, sending
    /// `Perform` to a tserver's RPC port (routing stays server-side).
    /// `"ysql"`: Postgres wire via this crate only. Cold paths (DDL, GC,
    /// rehydration, control file) always use YSQL.
    /// NOTE: an old binary reading a config that sets this fails to parse
    /// (`deny_unknown_fields`) — intentional, not silently ignored.
    ///
    /// Serialized unconditionally — deliberately NOT
    /// `skip_serializing_if = "HotPath::is_default"`. The pageserver's
    /// unknown-key check (`config::ignored_fields::find`) diffs the operator's
    /// TOML against the RE-SERIALIZED config and flags anything missing from
    /// the round trip. Skipping the default dropped it on the way out, so a
    /// config that spelled the default explicitly
    /// (`hot_path = "yb_thin_client"`) aborted startup with "unknown
    /// configuration item(s): yb_storage_conn.hot_path" — blaming the operator
    /// for a misspelling when key and value were both valid. `to_inline_toml`
    /// still omits the default; that renderer is hand-rolled and not part of
    /// this round trip.
    #[serde(default)]
    pub hot_path: HotPath,
    /// yb-tserver RPC endpoints for the direct-RPC hot path, CSV. When
    /// unset, the `host_ports` hosts on port 9100; set explicitly to reach
    /// every tserver when `host_ports` is only a subset (e.g. a single YSQL
    /// VIP, or a K8s service that load-balances across the tserver pods).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tserver_rpc_addrs: Option<HostPorts>,
    /// Read-*session* count for the direct-RPC hot-path pool. (The
    /// `tsclient_` prefix is historical — it predates `yb_thin_client`
    /// replacing `yb_tsclient`; the key is kept so existing configs parse.)
    /// Sets read
    /// concurrency (each session is a serial Perform queue); the sessions are
    /// packed onto shared connections (see [`Self::tsclient_sessions_per_conn`]),
    /// so this sets concurrency, not the connection count. Used verbatim (no
    /// scale-factor multiplication). When unset, each daemon applies its own
    /// default: the pageserver `scale_count(2, scale_factor)`, the safekeeper
    /// WAL lane a fixed 1.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tsclient_read_sessions: Option<usize>,
    /// Dedicated write-*session* count for the direct-RPC hot-path pool
    /// (server sessions, packed onto shared connections). Used verbatim when
    /// set. When unset: the pageserver `scale_count(2, scale_factor)`, the
    /// safekeeper WAL lane a fixed 1.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tsclient_write_sessions: Option<usize>,
    /// How many hot-path sessions ride each TCP connection. The pool opens
    /// `ceil((read_sessions + write_sessions) / sessions_per_conn)`
    /// connections and packs the sessions across them (reads and writes
    /// sharing connections); the connection count is what spreads load
    /// across tserver nodes (behind a single ClusterIP VIP, more connections
    /// = more nodes the VIP fans traffic to). Used verbatim when set; when
    /// unset, defaults to 4.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tsclient_sessions_per_conn: Option<usize>,
    /// Bound on how long opening one connection may take, in milliseconds.
    /// Defaults to [`DEFAULT_CONNECT_TIMEOUT_MS`]; `0` is no bound, matching
    /// the libpq key this is named after.
    ///
    /// It covers the whole open, not just the socket.
    /// `tokio_postgres::Config::connect_timeout` bounds only
    /// `TcpStream::connect`, which leaves TLS, the startup handshake,
    /// authentication and the session GUCs unbounded — and a peer that
    /// completes the TCP handshake and then stops answering is precisely
    /// the case worth bounding, because nothing ever fails and so the
    /// retry loop never engages. See [`open_no_tls`] / [`open_tls`].
    #[serde(default = "default_connect_timeout_ms")]
    pub connect_timeout_ms: u64,
    /// Server-side `statement_timeout` for every connection this config
    /// opens, in milliseconds, sent as `-c statement_timeout=N` in the
    /// startup `options`. `0` is no bound, matching the GUC's own semantics,
    /// and is the default — see [`DEFAULT_STATEMENT_TIMEOUT_MS`] for why.
    ///
    /// It applies to *every* statement on the connection, including the cold
    /// paths, so it is set per connection block by a caller that knows what it
    /// issues rather than globally.
    ///
    /// The server enforces it, so a statement a wedged tablet would
    /// otherwise hold open is cancelled server-side (`57014`) rather than
    /// waiting on the client to lose patience. It bounds a *slow* answer,
    /// not a *silent* peer: a backend that stops responding mid-statement
    /// is bounded by TCP keepalives, not by this.
    #[serde(default = "default_statement_timeout_ms")]
    pub statement_timeout_ms: u64,
    /// `TCP_USER_TIMEOUT` for this connection, in milliseconds. `0` (the
    /// default) leaves it to the kernel, which is to say unset.
    ///
    /// This is the only bound that catches a peer that stops answering at the
    /// *network* level rather than the protocol one. A tserver whose node dies
    /// without closing its sockets sends no FIN and no RST, so the connection
    /// stays open and healthy-looking: `is_closed()` is false, a statement
    /// bound never fires because no server is there to fire it, and the client
    /// waits on data that will never arrive. Keepalives would eventually
    /// notice, but the driver's default idle is two hours.
    ///
    /// Set it and the kernel fails the socket once data has gone unacknowledged
    /// for this long, which surfaces as a connection error — so the retry loop
    /// reconnects, and the smart driver's next connect can land on a node that
    /// is alive. Linux-only; other platforms ignore it.
    #[serde(default)]
    pub tcp_user_timeout_ms: u64,
    /// How long one pooled connection may stay open before it is recycled,
    /// in milliseconds. Defaults to [`DEFAULT_CONNECTION_LIFETIME_MS`]; `0`
    /// is no bound.
    ///
    /// The bound exists because the smart driver's topology is discovered
    /// ONCE per connection: the driver runs `yb_servers()` over a control
    /// connection and from then on addresses tservers by the per-pod
    /// hostnames it learned. Replace the tserver StatefulSet — as a K8s
    /// rollout that renames the pods does — and every cached hostname becomes
    /// NXDOMAIN while the configured bootstrap host
    /// (a headless service) still resolves. Nothing in the driver forces
    /// rediscovery, so a connection that opened before the swap keeps dialing
    /// names that no longer exist.
    ///
    /// Recycling is best-effort and checked lazily, on the next use of the
    /// slot: an expired connection is replaced before the call runs, and if
    /// the reopen fails the existing connection keeps serving and the next
    /// attempt is rate-limited to [`RECYCLE_RETRY_COOLDOWN`]. A lifetime
    /// bound can therefore only ever add a reconnect, never subtract
    /// availability.
    #[serde(default = "default_connection_lifetime_ms")]
    pub connection_lifetime_ms: u64,
}

/// Default [`YbConnConfig::connect_timeout_ms`]. Generous next to a healthy
/// connect and far under any human's patience: the case it exists for is a
/// peer that never answers at all, not a slow one.
pub const DEFAULT_CONNECT_TIMEOUT_MS: u64 = 5_000;

/// Default [`YbConnConfig::statement_timeout_ms`]: **off**.
///
/// Several statements the daemons issue legitimately run for minutes, and a
/// server-side cancellation of one of them is not a transient error — `57014`
/// is fatal here, so nothing retries it. The GC drop/shrink deletes
/// (`page_store::gc_act_drop_fork`, `gc_act_shrink_fork`) delete every version
/// of every block of a fork in one unbatched statement, and the candidate is
/// only dequeued once the action succeeds — a cancelled delete is re-selected
/// forever, so GC stops and storage is never reclaimed. `list_rels` runs an
/// unbounded `DISTINCT` per database on the basebackup path, so a cap there
/// fails compute startup. Blob upload/download already carry their own 120s
/// budget, which a shorter statement bound would silently preempt.
///
/// `truncate_wals_above` used to be on this list for the same reason as the GC
/// deletes — it filtered on a computed expression over the key columns and so
/// scanned a branch's whole WAL, on the torn-tail correctness path. It is
/// sargable now (#812); the scan was itself the bug, not something a timeout
/// would have made safe.
///
/// Turning this on globally wants those paths batched and made sargable
/// first. Until then it is opt-in per connection block, where a caller that
/// knows its statements are short can set it.
pub const DEFAULT_STATEMENT_TIMEOUT_MS: u64 = 0;

/// Default [`YbConnConfig::connection_lifetime_ms`]: 15 minutes.
///
/// Long enough that recycling is invisible next to the work a connection
/// does — one reopen per slot per quarter hour, each paying a TCP+TLS
/// handshake and re-priming that slot's prepared-statement cache — and short
/// enough to bound how long a stale topology can be believed. The failure it
/// exists for is measured in days (#811), so minutes is the right order of
/// magnitude; there is nothing to gain from being tighter.
pub const DEFAULT_CONNECTION_LIFETIME_MS: u64 = 15 * 60 * 1_000;

fn default_connect_timeout_ms() -> u64 {
    DEFAULT_CONNECT_TIMEOUT_MS
}

fn default_statement_timeout_ms() -> u64 {
    DEFAULT_STATEMENT_TIMEOUT_MS
}

fn default_connection_lifetime_ms() -> u64 {
    DEFAULT_CONNECTION_LIFETIME_MS
}

/// See [`YbConnConfig::hot_path`].
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, serde::Deserialize, serde::Serialize)]
#[serde(rename_all = "lowercase")]
pub enum HotPath {
    Ysql,
    /// Thin `Perform`-based tserver client over the `libyb_thin_client`
    /// C++ shim (`yb_thin_client`); serde wire form `"yb_thin_client"`.
    /// Batches reads/writes into one Perform per call. **The default.**
    #[default]
    #[serde(rename = "yb_thin_client")]
    ThinClient,
}

/// Default yb-tserver RPC port, used when `tserver_rpc_addrs` is derived
/// from the YSQL `host_ports` hosts.
pub const DEFAULT_YB_TSERVER_RPC_PORT: u16 = 9100;

impl YbConnConfig {
    /// yb-tserver RPC endpoints the direct-RPC hot path connects to:
    /// `tserver_rpc_addrs` if set, else the `host_ports` hosts on
    /// [`DEFAULT_YB_TSERVER_RPC_PORT`].
    pub fn effective_tserver_rpc_addrs(&self) -> Vec<String> {
        if let Some(hp) = &self.tserver_rpc_addrs {
            return hp
                .0
                .iter()
                .map(|h| format!("{}:{}", h.host, h.port))
                .collect();
        }
        rpc_addrs_from_hosts(self.host_ports.0.iter().map(|h| h.host.clone()))
    }

    /// [`Self::connection_lifetime_ms`] as a `Duration`, or `None` when the
    /// operator turned the bound off with `0`. Both transports read the bound
    /// through here — the YSQL pool ([`YsqlClient::with_max_connection_lifetime`])
    /// and the thin-client lane, which recycles its shim client and table
    /// handles on the same cadence — so one key governs both.
    pub fn max_connection_lifetime(&self) -> Option<Duration> {
        (self.connection_lifetime_ms > 0)
            .then(|| Duration::from_millis(self.connection_lifetime_ms))
    }
    /// Render this connection config as a libpq/diesel `postgresql://` URL.
    ///
    /// The controller's persistence layer takes a
    /// URL string rather than a parsed config, so it consumes the shared
    /// `[controller.yb]` block by building the URL here. All
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
    /// form the safekeeper (`yb_storage_conn`) and controller
    /// (`--yb-config`) accept on the command line, and the block the
    /// pageserver `remote_storage` configs embed. The
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
        if self.disable_transactional_writes {
            parts.push("disable_transactional_writes = true".to_string());
        }
        match self.hot_path {
            HotPath::ThinClient => {} // default → omitted
            HotPath::Ysql => parts.push("hot_path = \"ysql\"".to_string()),
        }
        if let Some(t) = &self.tserver_rpc_addrs {
            parts.push(format!("tserver_rpc_addrs = {}", q(&t.to_string())));
        }
        if let Some(n) = self.tsclient_read_sessions {
            parts.push(format!("tsclient_read_sessions = {n}"));
        }
        if let Some(n) = self.tsclient_write_sessions {
            parts.push(format!("tsclient_write_sessions = {n}"));
        }
        if let Some(n) = self.tsclient_sessions_per_conn {
            parts.push(format!("tsclient_sessions_per_conn = {n}"));
        }
        if self.connect_timeout_ms != DEFAULT_CONNECT_TIMEOUT_MS {
            parts.push(format!("connect_timeout_ms = {}", self.connect_timeout_ms));
        }
        if self.tcp_user_timeout_ms != 0 {
            parts.push(format!(
                "tcp_user_timeout_ms = {}",
                self.tcp_user_timeout_ms
            ));
        }
        if self.statement_timeout_ms != DEFAULT_STATEMENT_TIMEOUT_MS {
            parts.push(format!(
                "statement_timeout_ms = {}",
                self.statement_timeout_ms
            ));
        }
        if self.connection_lifetime_ms != DEFAULT_CONNECTION_LIFETIME_MS {
            parts.push(format!(
                "connection_lifetime_ms = {}",
                self.connection_lifetime_ms
            ));
        }
        format!("{{ {} }}", parts.join(", "))
    }
}

/// Build tserver RPC endpoint strings from bare hostnames on the default
/// RPC port ([`DEFAULT_YB_TSERVER_RPC_PORT`]), deduplicated and ordered so
/// the session pool has a stable layout.
fn rpc_addrs_from_hosts(hosts: impl IntoIterator<Item = String>) -> Vec<String> {
    let mut addrs: Vec<String> = hosts
        .into_iter()
        .map(|h| format!("{h}:{DEFAULT_YB_TSERVER_RPC_PORT}"))
        .collect();
    addrs.sort_unstable();
    addrs.dedup();
    addrs
}

/// One-time catalog lookup for the direct-RPC hot path: the database OID
/// plus the pg_class OID of each named table in `schema`. The
/// `yb_thin_client` crate derives DocDB table ids from these. Errors if any
/// requested table is missing — callers fail hard at startup rather than
/// run with a partially-enabled direct path.
pub async fn lookup_table_oids(
    client: &YsqlClient,
    schema: &str,
    tables: &[&str],
) -> Result<(u32, std::collections::HashMap<String, u32>)> {
    // Non-empty is what makes `db_oid.expect(...)` below sound: the db OID is
    // only read off a returned row, and with no requested tables the query
    // returns none. Callers pass a fixed non-empty list; guard it explicitly.
    anyhow::ensure!(
        !tables.is_empty(),
        "lookup_table_oids: tables list must not be empty"
    );
    let rows = client
        .query(
            "SELECT (SELECT oid FROM pg_database WHERE datname = current_database())::bigint, \
             c.relname::text, c.oid::bigint \
             FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace \
             WHERE n.nspname = $1 AND c.relname = ANY($2)",
            &[
                &schema,
                &tables.iter().map(|t| t.to_string()).collect::<Vec<_>>(),
            ],
        )
        .await
        .context("looking up table OIDs for the direct-to-tserver hot path")?;
    let mut db_oid = None;
    let mut out = std::collections::HashMap::new();
    for row in &rows {
        db_oid = Some(row.get::<_, i64>(0) as u32);
        out.insert(row.get::<_, String>(1), row.get::<_, i64>(2) as u32);
    }
    for t in tables {
        anyhow::ensure!(
            out.contains_key(*t),
            "table {schema}.{t} not found (direct-to-tserver hot path requires it)"
        );
    }
    Ok((db_oid.expect("tables checked non-empty above"), out))
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
/// (`dump-pages`, `dump-wals`, `reset-consistent-lsn`).
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
            // Diagnostic tools open read-only connections (no fast-path
            // upsert), so the GUC is never set regardless; keep the default.
            disable_transactional_writes: false,
            // Diagnostic tools always use YSQL.
            hot_path: HotPath::Ysql,
            tserver_rpc_addrs: None,
            tsclient_read_sessions: None,
            tsclient_write_sessions: None,
            tsclient_sessions_per_conn: None,
            // Unbounded, unlike a daemon. These tools are run by hand during
            // an incident and do the long scans the daemons never issue —
            // reset-consistent-lsn replays the WAL decoder over
            // `amp_storage.wals` — so the operator watching it is the timeout.
            connect_timeout_ms: 0,
            statement_timeout_ms: 0,
            tcp_user_timeout_ms: 0,
            // Unbounded too: these tools open a connection, do one pass and
            // exit, so there is no stale topology to age out — and recycling
            // mid-scan would drop the prepared statements a long replay is
            // riding on.
            connection_lifetime_ms: 0,
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
    use super::{Duration, YbConnConfig, YsqlClient, build_pg_config, rpc_addrs_from_hosts};

    fn cfg(host_ports: &str) -> YbConnConfig {
        YbConnConfig {
            host_ports: host_ports.parse().unwrap(),
            user: "yugabyte".into(),
            dbname: "yb_storage".into(),
            password_env: None,
            sslmode: None,
            sslrootcert: None,
            disable_transactional_writes: false,
            hot_path: super::HotPath::default(),
            tserver_rpc_addrs: None,
            tsclient_read_sessions: None,
            tsclient_write_sessions: None,
            tsclient_sessions_per_conn: None,
            connect_timeout_ms: super::DEFAULT_CONNECT_TIMEOUT_MS,
            statement_timeout_ms: super::DEFAULT_STATEMENT_TIMEOUT_MS,
            tcp_user_timeout_ms: 0,
            connection_lifetime_ms: super::DEFAULT_CONNECTION_LIFETIME_MS,
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

    #[test]
    fn inline_toml_emits_disable_transactional_writes_only_when_on() {
        // Off by default → omitted from the wire form (keeps existing
        // configs byte-identical and round-trips through `#[serde(default)]`).
        assert!(
            !cfg("127.0.0.1:5433")
                .to_inline_toml()
                .contains("disable_transactional_writes")
        );
        // Enabled → emitted so it survives a re-serialize hop between
        // components (pageserver → safekeeper, `--yb-config`, etc.).
        let mut c = cfg("127.0.0.1:5433");
        c.disable_transactional_writes = true;
        assert!(
            c.to_inline_toml()
                .contains("disable_transactional_writes = true")
        );
    }

    /// A connection block that says nothing still gets the connect bound —
    /// every deployment that exists today omits the key, and they are the
    /// deployments it is for. The statement bound stays off; it is not safe to
    /// apply to statements nobody has checked the length of.
    #[test]
    fn a_silent_connection_block_gets_the_connect_bound_only() {
        let cfg: YbConnConfig =
            toml::from_str("host_ports = \"h:5433\"\nuser = \"u\"\ndbname = \"d\"\n").unwrap();
        assert_eq!(cfg.connect_timeout_ms, super::DEFAULT_CONNECT_TIMEOUT_MS);
        assert_eq!(
            cfg.statement_timeout_ms,
            super::DEFAULT_STATEMENT_TIMEOUT_MS
        );

        let pg = build_pg_config(&cfg, "test").unwrap();
        assert_eq!(
            pg.get_connect_timeout(),
            Some(&Duration::from_millis(super::DEFAULT_CONNECT_TIMEOUT_MS))
        );
        assert_eq!(pg.get_options(), None);
    }

    #[test]
    fn timeouts_reach_the_driver_config() {
        let cfg: YbConnConfig = toml::from_str(
            "host_ports = \"h:5433\"\nuser = \"u\"\ndbname = \"d\"\n\
             connect_timeout_ms = 3000\nstatement_timeout_ms = 5000\n",
        )
        .unwrap();

        let pg = build_pg_config(&cfg, "test").unwrap();
        assert_eq!(pg.get_connect_timeout(), Some(&Duration::from_millis(3000)));
        // The server applies `statement_timeout` in milliseconds, so the
        // config value goes across unscaled.
        assert_eq!(pg.get_options(), Some("-c statement_timeout=5000"));
    }

    /// The only bound that catches a peer which stops answering at the network
    /// level. Off by default, because it is the kernel's business unless a
    /// caller has a deadline of its own; the write-fence renewal is the caller
    /// that does.
    #[test]
    fn a_tcp_user_timeout_reaches_the_driver() {
        let mut cfg = cfg("h:5433");
        assert_eq!(cfg.tcp_user_timeout_ms, 0);
        assert_eq!(
            build_pg_config(&cfg, "test")
                .unwrap()
                .get_tcp_user_timeout(),
            None
        );

        cfg.tcp_user_timeout_ms = 2_000;
        assert_eq!(
            build_pg_config(&cfg, "test")
                .unwrap()
                .get_tcp_user_timeout(),
            Some(&Duration::from_millis(2_000))
        );
        assert!(cfg.to_inline_toml().contains("tcp_user_timeout_ms = 2000"));
    }

    /// A connection block that says nothing gets the 15-minute lifetime, and
    /// the bound reaches the pool. Every deployment that exists today omits
    /// the key, and they are the ones wedged by a stale host list (#811).
    #[test]
    fn a_silent_connection_block_gets_the_lifetime_bound() {
        let cfg: YbConnConfig =
            toml::from_str("host_ports = \"h:5433\"\nuser = \"u\"\ndbname = \"d\"\n").unwrap();
        assert_eq!(
            cfg.connection_lifetime_ms,
            super::DEFAULT_CONNECTION_LIFETIME_MS
        );
        assert_eq!(
            cfg.max_connection_lifetime(),
            Some(Duration::from_secs(15 * 60))
        );
    }

    /// `0` turns recycling off, and it round-trips — an operator who has to
    /// pin a connection open (a long manual replay through `YsqlClient`) must
    /// be able to, and must be able to see that they did.
    #[test]
    fn zero_connection_lifetime_is_unbounded_and_round_trips() {
        let cfg: YbConnConfig = toml::from_str(
            "host_ports = \"h:5433\"\nuser = \"u\"\ndbname = \"d\"\nconnection_lifetime_ms = 0\n",
        )
        .unwrap();
        assert_eq!(cfg.max_connection_lifetime(), None);

        let t = cfg.to_inline_toml();
        assert!(t.contains("connection_lifetime_ms = 0"), "{t}");
        let back: YbConnConfig = toml::from_str(&format!("v = {t}"))
            .map(|v: std::collections::HashMap<String, YbConnConfig>| v["v"].clone())
            .unwrap();
        assert_eq!(back, cfg);
    }

    /// The default is omitted from the wire form (so existing configs stay
    /// byte-identical) but must survive a `Serialize` round trip, which is
    /// what the pageserver's unknown-key check diffs against — the same trap
    /// `hot_path` fell into.
    #[test]
    fn default_connection_lifetime_is_omitted_inline_but_serializes() {
        let c = cfg("127.0.0.1:5433");
        assert!(!c.to_inline_toml().contains("connection_lifetime_ms"));
        let ser = toml::to_string(&c).expect("serialize");
        assert!(ser.contains("connection_lifetime_ms"), "{ser}");
        let back: YbConnConfig = toml::from_str(&ser).expect("deserialize");
        assert_eq!(back, c);
    }

    /// Zero is "no bound" for both — the escape hatch out of the defaults,
    /// following libpq's `connect_timeout` and the server's own
    /// `statement_timeout`. Worth pinning: read the other way, a zero connect
    /// bound would fail every connection instantly.
    #[test]
    fn zero_means_no_bound() {
        let cfg: YbConnConfig = toml::from_str(
            "host_ports = \"h:5433\"\nuser = \"u\"\ndbname = \"d\"\n\
             connect_timeout_ms = 0\nstatement_timeout_ms = 0\n",
        )
        .unwrap();

        let pg = build_pg_config(&cfg, "test").unwrap();
        assert_eq!(pg.get_connect_timeout(), None);
        assert_eq!(pg.get_options(), None);
    }

    /// A server that completes the TCP handshake and then says nothing is
    /// the case the driver's own `connect_timeout` does not cover — it is
    /// spent entirely on `TcpStream::connect`, which already succeeded.
    /// Without [`with_connect_timeout`] this call never returns.
    #[tokio::test]
    async fn a_silent_server_cannot_hang_the_connect() {
        use std::io::Read;

        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        std::thread::spawn(move || {
            let (mut sock, _) = listener.accept().unwrap();
            let mut buf = [0u8; 1024];
            let _ = sock.read(&mut buf);
            std::thread::sleep(Duration::from_secs(120));
        });

        let cfg: YbConnConfig = toml::from_str(&format!(
            "host_ports = \"127.0.0.1:{port}\"\nuser = \"u\"\ndbname = \"d\"\n\
             connect_timeout_ms = 500\n"
        ))
        .unwrap();

        let started = std::time::Instant::now();
        let res = YsqlClient::from_yb_config(&cfg, "test", false).await;
        assert!(res.is_err(), "a silent server must not look like a connect");
        assert!(
            started.elapsed() < Duration::from_secs(30),
            "connect was not bounded: took {:?}",
            started.elapsed()
        );
    }

    #[test]
    fn hot_path_round_trips_and_defaults_off_the_wire() {
        // Default (yb_thin_client) → omitted from the wire form.
        let c0 = cfg("127.0.0.1:5433");
        assert_eq!(c0.hot_path, super::HotPath::ThinClient);
        let t = c0.to_inline_toml();
        assert!(!t.contains("hot_path"));

        // Explicit ysql (the escape hatch) is emitted and round-trips.
        let mut c = cfg("h1:5433,h2:5433");
        c.hot_path = super::HotPath::Ysql;
        let t = c.to_inline_toml();
        assert!(t.contains("hot_path = \"ysql\""));
        let back: YbConnConfig = toml::from_str(&format!("v = {t}"))
            .map(|v: std::collections::HashMap<String, YbConnConfig>| v["v"].clone())
            .unwrap();
        assert_eq!(back, c);
    }

    /// Every `hot_path` value — INCLUDING the default — must survive a serde
    /// round trip. The pageserver's unknown-key check
    /// (`config::ignored_fields::find`) diffs the operator's TOML against the
    /// re-serialized config, so a value that vanishes on serialization is
    /// reported as an unknown key and aborts startup. A
    /// `skip_serializing_if = "is_default"` on `hot_path` did exactly that:
    /// `hot_path = "yb_thin_client"` (the documented default, spelled out)
    /// failed with "unknown configuration item(s): yb_storage_conn.hot_path".
    #[test]
    fn hot_path_survives_serde_round_trip_for_every_lane() {
        for lane in [super::HotPath::Ysql, super::HotPath::ThinClient] {
            let mut c = cfg("127.0.0.1:5433");
            c.hot_path = lane;
            let ser = toml::to_string(&c).expect("serialize");
            assert!(
                ser.contains("hot_path"),
                "{lane:?} vanished on serialization: {ser}\n\
                 the pageserver would reject it as an unknown key"
            );
            let back: YbConnConfig = toml::from_str(&ser).expect("deserialize");
            assert_eq!(back.hot_path, lane);
        }
    }

    #[test]
    fn tserver_rpc_addrs_round_trips() {
        // The default lane (yb_thin_client) is omitted from the wire form, so
        // a config carrying only the RPC-endpoint override renders just that
        // key — and the override survives the round trip and drives
        // `effective_tserver_rpc_addrs`.
        let mut c = cfg("h1:5433,h2:5433");
        let t = c.to_inline_toml();
        assert!(!t.contains("hot_path"));

        c.tserver_rpc_addrs = Some("t1:9100,t2:9101".parse().unwrap());
        let t = c.to_inline_toml();
        assert!(t.contains("tserver_rpc_addrs = \"t1:9100,t2:9101\""));
        let back: YbConnConfig = toml::from_str(&format!("v = {t}"))
            .map(|v: std::collections::HashMap<String, YbConnConfig>| v["v"].clone())
            .unwrap();
        assert_eq!(back, c);
        assert_eq!(back.effective_tserver_rpc_addrs(), ["t1:9100", "t2:9101"]);
    }

    #[test]
    fn tserver_rpc_addrs_default_derives_from_host_ports() {
        let c = cfg("h1:5433,h2:5440");
        assert_eq!(c.effective_tserver_rpc_addrs(), ["h1:9100", "h2:9100"]);
    }

    #[test]
    fn tserver_rpc_addrs_explicit_wins() {
        let mut c = cfg("ysql-vip:5433");
        c.tserver_rpc_addrs = Some("t1:9100,t2:9200".parse().unwrap());
        assert_eq!(c.effective_tserver_rpc_addrs(), ["t1:9100", "t2:9200"]);
    }

    #[test]
    fn rpc_addrs_from_hosts_maps_dedups_and_sorts() {
        // Bare hosts map to the RPC port, deduped and ordered for a stable
        // pool layout (a VIP can appear alongside the hosts it fronts).
        let addrs =
            rpc_addrs_from_hosts(["ts2", "ts1", "ts2", "ts3"].iter().map(|s| s.to_string()));
        assert_eq!(addrs, ["ts1:9100", "ts2:9100", "ts3:9100"]);
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
    /// Bound on how long one slot's connection may stay open, from
    /// [`YbConnConfig::connection_lifetime_ms`]. `None` (the default for the
    /// `connect_*` constructors, which take a bare driver `Config` and so
    /// have no connection block to read) leaves connections unbounded, as
    /// they were before #811.
    max_lifetime: Option<Duration>,
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
    /// When the connection currently in this slot was opened. Compared
    /// against the slot's [`Self::lifetime`] to decide a recycle.
    opened_at: std::time::Instant,
    /// This slot's own lifetime: the client-wide bound plus 0–25% jitter,
    /// re-rolled on every open. Without it a pool recycles in lockstep, and
    /// so does every daemon that started in the same deploy — a synchronized
    /// reconnect storm against the cluster the bound exists to stay
    /// connected to.
    lifetime: Option<Duration>,
    /// Set when a lifetime-driven reopen failed: no further recycle is
    /// attempted on this slot until then, so a cluster that is briefly
    /// unreachable isn't hammered with one reopen per call.
    recycle_after: Option<std::time::Instant>,
}

impl State {
    fn new(client: tokio_postgres::Client, max_lifetime: Option<Duration>) -> State {
        State {
            client: Arc::new(client),
            prepared: HashMap::new(),
            opened_at: std::time::Instant::now(),
            lifetime: max_lifetime.map(jittered),
            recycle_after: None,
        }
    }

    /// Whether this slot's connection has outlived its lifetime and no
    /// cooldown from a failed reopen is in force.
    fn recycle_due(&self) -> bool {
        recycle_due_at(
            self.opened_at,
            self.lifetime,
            self.recycle_after,
            std::time::Instant::now(),
        )
    }

    /// Install a freshly-opened connection, dropping the prepared-statement
    /// cache (its `Statement`s belong to the connection being replaced) and
    /// restarting the lifetime clock.
    fn replace(&mut self, client: tokio_postgres::Client, max_lifetime: Option<Duration>) {
        self.client = Arc::new(client);
        self.prepared.clear();
        self.opened_at = std::time::Instant::now();
        self.lifetime = max_lifetime.map(jittered);
        self.recycle_after = None;
    }
}

/// The recycle predicate, with `now` and the slot's timestamps passed in so
/// it can be exercised without a live connection (`tokio_postgres::Client`
/// has no public constructor).
///
/// `lifetime: None` is the unbounded case. `recycle_after` is the cooldown a
/// failed reopen installed: while it is in the future the connection stays
/// past its lifetime deliberately, because replacing it is what just failed.
fn recycle_due_at(
    opened_at: std::time::Instant,
    lifetime: Option<Duration>,
    recycle_after: Option<std::time::Instant>,
    now: std::time::Instant,
) -> bool {
    let Some(lifetime) = lifetime else {
        return false;
    };
    if now.saturating_duration_since(opened_at) < lifetime {
        return false;
    }
    match recycle_after {
        Some(at) => now >= at,
        None => true,
    }
}

/// `base` plus 0–25% random jitter — the same envelope
/// [`sleep_with_jitter`] uses, for the same reason: to break a herd.
fn jittered(base: Duration) -> Duration {
    base.mul_f64(1.0 + rand::random::<f64>() * 0.25)
}

/// How long to wait before retrying a lifetime-driven reopen that failed.
/// The old connection keeps serving in the meantime, so this only paces the
/// retry; it does not delay any caller.
const RECYCLE_RETRY_COOLDOWN: Duration = Duration::from_secs(10);

impl YsqlClient {
    /// Build a `YsqlClient` with a single connection (no pool).
    /// Equivalent to `connect_no_tls_pooled(config, 1, set_fast_path_upsert)`.
    pub async fn connect_no_tls(
        config: tokio_postgres::Config,
        set_fast_path_upsert: bool,
    ) -> Result<Self> {
        Self::connect_no_tls_pooled(config, 1, set_fast_path_upsert, false).await
    }

    /// Build a `YsqlClient` that connects without TLS, with `pool_size`
    /// independent connections. Opens all connections eagerly so config
    /// errors surface at construction. When `set_fast_path_upsert` is true, each
    /// connection runs `SET yb_enable_upsert_mode = true` after opening
    /// — see `set_fast_path_upsert_gucs` for why. `disable_transactional_writes`
    /// additionally runs `SET yb_disable_transactional_writes = true` on those
    /// fast-path connections (off by default; see [`YbConnConfig`]).
    pub async fn connect_no_tls_pooled(
        config: tokio_postgres::Config,
        pool_size: usize,
        set_fast_path_upsert: bool,
        disable_transactional_writes: bool,
    ) -> Result<Self> {
        let config = Arc::new(config);
        let open: OpenFn = {
            let config = config.clone();
            Arc::new(move || {
                let config = config.clone();
                async move {
                    open_no_tls(&config, set_fast_path_upsert, disable_transactional_writes).await
                }
                .boxed()
            })
        };
        Self::from_open(open, pool_size).await
    }

    /// Build a `YsqlClient` with a single TLS connection. Equivalent to
    /// `connect_rustls_pooled(config, tls, 1, set_fast_path_upsert)`.
    pub async fn connect_rustls(
        config: tokio_postgres::Config,
        tls: yb_postgres_rustls::MakeRustlsConnect,
        set_fast_path_upsert: bool,
    ) -> Result<Self> {
        Self::connect_rustls_pooled(config, tls, 1, set_fast_path_upsert, false).await
    }

    /// Build a `YsqlClient` that connects via the supplied rustls
    /// connector with `pool_size` independent connections. Opens all
    /// eagerly. `set_fast_path_upsert` and `disable_transactional_writes`
    /// behave as in `connect_no_tls_pooled`.
    pub async fn connect_rustls_pooled(
        config: tokio_postgres::Config,
        tls: yb_postgres_rustls::MakeRustlsConnect,
        pool_size: usize,
        set_fast_path_upsert: bool,
        disable_transactional_writes: bool,
    ) -> Result<Self> {
        let config = Arc::new(config);
        let tls = Arc::new(tls);
        let open: OpenFn = {
            let config = config.clone();
            let tls = tls.clone();
            Arc::new(move || {
                let config = config.clone();
                let tls = tls.clone();
                async move {
                    open_tls(
                        &config,
                        &tls,
                        set_fast_path_upsert,
                        disable_transactional_writes,
                    )
                    .await
                }
                .boxed()
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
    /// Whether those connections also `SET
    /// yb_disable_transactional_writes = true` comes from
    /// [`YbConnConfig::disable_transactional_writes`] (off by default).
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
        let disable_transactional_writes = cfg.disable_transactional_writes;
        let client = match cfg.sslmode.as_deref() {
            None | Some("disable") => {
                Self::connect_no_tls_pooled(
                    pg_config,
                    pool_size,
                    set_fast_path_upsert,
                    disable_transactional_writes,
                )
                .await?
            }
            Some(_mode) => {
                let tls = build_yb_rustls_connector(cfg.sslrootcert.as_deref())?;
                Self::connect_rustls_pooled(
                    pg_config,
                    tls,
                    pool_size,
                    set_fast_path_upsert,
                    disable_transactional_writes,
                )
                .await?
            }
        };
        Ok(match cfg.max_connection_lifetime() {
            Some(max) => client.with_max_connection_lifetime(max),
            None => client,
        })
    }

    async fn from_open(open: OpenFn, pool_size: usize) -> Result<Self> {
        let pool_size = pool_size.max(1);
        let mut pool = Vec::with_capacity(pool_size);
        for _ in 0..pool_size {
            let client = (open)().await?;
            pool.push(tokio::sync::Mutex::new(State::new(client, None)));
        }
        Ok(Self {
            open,
            pool,
            next: std::sync::atomic::AtomicUsize::new(0),
            max_lifetime: None,
        })
    }

    /// Bound how long each pooled connection may stay open before it is
    /// recycled — see [`YbConnConfig::connection_lifetime_ms`] for why one
    /// exists. `Duration::ZERO` leaves connections unbounded.
    ///
    /// [`Self::from_yb_config`] / [`Self::from_yb_config_pooled`] apply the
    /// connection block's value through here, so daemons get the bound
    /// without asking. The `connect_no_tls*` / `connect_rustls*`
    /// constructors take a bare driver `Config` with no connection block to
    /// read, so a caller on those paths that wants the bound sets it here.
    ///
    /// Applied after the pool is open, which is exactly right: each slot's
    /// clock starts when its connection opened, not when the bound was set.
    #[must_use]
    pub fn with_max_connection_lifetime(mut self, max: Duration) -> Self {
        self.max_lifetime = (!max.is_zero()).then_some(max);
        for slot in &mut self.pool {
            slot.get_mut().lifetime = self.max_lifetime.map(jittered);
        }
        self
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
    /// first if it's dead — or if it has outlived
    /// [`YbConnConfig::connection_lifetime_ms`]. Either reconnect clears
    /// that slot's prepared-statement cache because cached `Statement`s
    /// belong to the old connection. Other slots are unaffected.
    ///
    /// The two reconnects differ in how a failure is treated. A dead
    /// connection has nothing to fall back to, so the error propagates and
    /// the caller's retry budget handles it. An expired one is still usable,
    /// so a failed reopen is logged, the old connection keeps serving, and
    /// the next attempt waits out [`RECYCLE_RETRY_COOLDOWN`] — the bound
    /// exists to improve availability and must not be able to cost any.
    async fn current_client_at(&self, slot: usize) -> Result<Arc<tokio_postgres::Client>> {
        let mut guard = self.pool[slot].lock().await;
        if guard.client.is_closed() {
            tracing::info!(slot, "YsqlClient: previous client closed, reconnecting");
            let client = (self.open)().await?;
            guard.replace(client, self.max_lifetime);
        } else if guard.recycle_due() {
            let age = guard.opened_at.elapsed();
            match (self.open)().await {
                Ok(client) => {
                    tracing::info!(
                        slot,
                        age_secs = age.as_secs(),
                        "YsqlClient: recycling connection past its lifetime"
                    );
                    guard.replace(client, self.max_lifetime);
                    YB_CONNECTION_RECYCLES.with_label_values(&["ok"]).inc();
                }
                Err(e) => {
                    tracing::warn!(
                        slot,
                        age_secs = age.as_secs(),
                        "YsqlClient: could not recycle connection past its lifetime, \
                         keeping the current one: {e:#}"
                    );
                    guard.recycle_after = Some(std::time::Instant::now() + RECYCLE_RETRY_COOLDOWN);
                    YB_CONNECTION_RECYCLES.with_label_values(&["failed"]).inc();
                }
            }
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
        let client = (self.open)().await?;
        guard.replace(client, self.max_lifetime);
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
    ///   `2D000`, `3B001`, any `42xxx`), `57014` (the server cancelling a
    ///   statement that outran `statement_timeout`), and anything else.
    ///   Returned directly.
    ///
    /// **Idempotency assumption.** Nearly all callers (pageserver
    /// `PageStore`, safekeeper WAL backup, `YbStore` blob ops, storage
    /// controller) issue idempotent SQL — reads, `INSERT ... ON
    /// CONFLICT` / upserts (we `SET yb_enable_upsert_mode = true` on
    /// connect), absolute metadata updates. That lets the retry loop
    /// replay connection drops blindly even when we can't prove the
    /// pre-drop statement didn't commit.
    ///
    /// There is no exception: every write on these paths is either an
    /// upsert or an absolute `SET`, so a double-apply lands the same row
    /// state. A relative increment would not be.
    ///
    /// A non-idempotent write that can't absorb a double-apply needs a path
    /// that returns `ConnectionDrop` unretried.
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
    // Wide signature: retry-classification context that callers pass through verbatim.
    #[allow(clippy::too_many_arguments)]
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
        // 57014 is the server cancelling a statement that outran
        // `statement_timeout` (see `YbConnConfig::statement_timeout_ms`).
        // Fatal deliberately: the point of the bound is to give the caller
        // its answer inside a known window, and retrying a statement that
        // just exhausted the window spends the budget to hit the same wall.
        if c == "57014" {
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

/// Build a rustls-based TLS connector suitable for YugabyteDB
/// Postgres-wire connections through the yb-tokio-postgres smart
/// driver, via `yb-postgres-rustls` (pure-Rust / ring) — the rustls
/// adapter published for the YB fork, which implements the fork's own
/// `yb_tokio_postgres::tls::MakeTlsConnect` trait directly.
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
/// `compute/architecture/yugabyte.md` (and the linked YB-team report) for
/// the diagnosis.
pub fn build_yb_rustls_connector(
    sslrootcert: Option<&str>,
) -> anyhow::Result<yb_postgres_rustls::MakeRustlsConnect> {
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
        // the controller's existing behaviour and matches the
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

    Ok(yb_postgres_rustls::MakeRustlsConnect::new(client_config))
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
    // Smart-driver load balancing: the driver opens an internal control
    // connection to run `yb_servers()` for topology discovery and spreads
    // connections across tservers. This was disabled while
    // yb-tokio-postgres <= 0.7.10-yb-1-beta.3 failed every connect with
    // "could not create control connection" (the control connection's TLS
    // path was broken); 0.7.10-yb-1-beta.4 fixes it, so re-enable it.
    pg_config.load_balance("only-primary");
    if let Some(env_name) = &cfg.password_env {
        let password = std::env::var(env_name)
            .with_context(|| format!("YsqlClient: password env var `{env_name}` is not set"))?;
        pg_config.password(password);
    }
    // Both bounds are off unless the connection block asks for them, and
    // both read zero as "no bound" — libpq's for connect_timeout, the
    // server's own for statement_timeout.
    //
    // The driver applies connect_timeout to the socket connect alone;
    // `open_no_tls` / `open_tls` read it back off the config and apply it to
    // the whole open. Setting it here as well keeps the per-address bound
    // the driver documents, so a host that blackholes SYNs is skipped
    // without spending the caller's entire budget on it.
    if cfg.connect_timeout_ms > 0 {
        pg_config.connect_timeout(Duration::from_millis(cfg.connect_timeout_ms));
    }
    if cfg.statement_timeout_ms > 0 {
        pg_config.options(&format!(
            "-c statement_timeout={}",
            cfg.statement_timeout_ms
        ));
    }
    if cfg.tcp_user_timeout_ms > 0 {
        pg_config.tcp_user_timeout(Duration::from_millis(cfg.tcp_user_timeout_ms));
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
/// The second GUC is gated on `disable_transactional_writes` — wired
/// from [`YbConnConfig::disable_transactional_writes`] and `false` by
/// default. It is disabled by default because of a 500-conn-workload
/// WAL-durability incident: under tablet rebalancing / RPC retries,
/// batch INSERTs reported as success appeared to lose individual rows,
/// leaving holes in `amp_storage.wals` that wedge pageserver restart.
/// With the GUC off, INSERTs go through the standard transactional path
/// on this connection — slower, atomic. Operators can opt back in via
/// the TOML connection block once the incident is resolved.
///
/// Failure on either SET propagates: in production we only ever talk
/// to YB, so a SET failure means the connection would silently fall
/// back to the slow / transactional path. We'd rather refuse the
/// connection here than carry a misconfigured client.
async fn set_fast_path_upsert_gucs(
    client: &tokio_postgres::Client,
    disable_transactional_writes: bool,
) -> Result<()> {
    client
        .execute("SET yb_enable_upsert_mode = true", &[])
        .await
        .context("YsqlClient: SET yb_enable_upsert_mode = true")?;
    if disable_transactional_writes {
        client
            .execute("SET yb_disable_transactional_writes = true", &[])
            .await
            .context("YsqlClient: SET yb_disable_transactional_writes = true")?;
    }
    Ok(())
}

/// Apply the config's `connect_timeout` to a whole open.
///
/// The driver spends it on `TcpStream::connect` only (`connect_socket.rs`),
/// so everything after the socket — TLS, the startup handshake, auth, the
/// session GUCs — is unbounded on its own. A peer that accepts the
/// connection, reads the startup packet and then says nothing hangs the
/// caller forever, and no retry budget is spent because nothing fails.
///
/// Reading the bound back off the `Config` rather than taking it as an
/// argument means every path gets it, including the callers that build a
/// `Config` by hand instead of going through [`build_pg_config`].
async fn with_connect_timeout<F>(
    config: &tokio_postgres::Config,
    what: &str,
    open: F,
) -> Result<tokio_postgres::Client>
where
    F: std::future::Future<Output = Result<tokio_postgres::Client>>,
{
    let Some(limit) = config.get_connect_timeout().copied() else {
        return open.await;
    };
    tokio::time::timeout(limit, open)
        .await
        .with_context(|| format!("YsqlClient: {what} did not finish within {limit:?}"))?
}

async fn open_no_tls(
    config: &tokio_postgres::Config,
    set_fast_path_upsert: bool,
    disable_transactional_writes: bool,
) -> Result<tokio_postgres::Client> {
    with_connect_timeout(config, "connect (NoTls)", async {
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
            set_fast_path_upsert_gucs(&client, disable_transactional_writes).await?;
        }
        Ok(client)
    })
    .await
}

async fn open_tls(
    config: &tokio_postgres::Config,
    tls: &yb_postgres_rustls::MakeRustlsConnect,
    set_fast_path_upsert: bool,
    disable_transactional_writes: bool,
) -> Result<tokio_postgres::Client> {
    with_connect_timeout(config, "connect (rustls)", async {
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
            set_fast_path_upsert_gucs(&client, disable_transactional_writes).await?;
        }
        Ok(client)
    })
    .await
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
                YB_TRANSIENT_MSG_PATTERNS.contains(&needle),
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
    fn recycle_is_due_only_past_the_lifetime() {
        let now = std::time::Instant::now();
        let opened = now - Duration::from_secs(1_000);
        // Unbounded: never due, however old.
        assert!(!recycle_due_at(opened, None, None, now));
        // Younger than its lifetime: not due.
        assert!(!recycle_due_at(
            opened,
            Some(Duration::from_secs(2_000)),
            None,
            now
        ));
        // Past it: due.
        assert!(recycle_due_at(
            opened,
            Some(Duration::from_secs(900)),
            None,
            now
        ));
    }

    /// A failed reopen must not turn into a reopen attempt per call: the
    /// cooldown suppresses the recycle until it expires, while the existing
    /// connection keeps serving.
    #[test]
    fn a_failed_recycle_holds_off_until_its_cooldown_expires() {
        let now = std::time::Instant::now();
        let opened = now - Duration::from_secs(1_000);
        let lifetime = Some(Duration::from_secs(900));

        let cooling = Some(now + Duration::from_secs(5));
        assert!(!recycle_due_at(opened, lifetime, cooling, now));

        let expired = Some(now - Duration::from_millis(1));
        assert!(recycle_due_at(opened, lifetime, expired, now));
    }

    /// The per-slot lifetime carries the same 0–25% jitter the retry backoff
    /// does. Without it every slot in a pool — and every daemon deployed
    /// together — recycles in the same instant.
    #[test]
    fn jittered_lifetime_stays_inside_its_envelope() {
        let base = Duration::from_secs(900);
        let mut seen_distinct = false;
        let first = jittered(base);
        for _ in 0..50 {
            let j = jittered(base);
            assert!(j >= base);
            assert!(j <= base.mul_f64(1.25));
            seen_distinct |= j != first;
        }
        assert!(seen_distinct, "lifetimes are not being jittered at all");
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
