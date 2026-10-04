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
//!      no connection outlives the topology it was opened against by more
//!      than that bound (#811).
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
mod tls_reload;

pub use yb_tokio_postgres as tokio_postgres;

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use futures::FutureExt;
use futures::future::BoxFuture;
use metrics::{
    YB_CLIENT_CERT_NOT_AFTER_SECONDS, YB_CLIENT_ERRORS, YB_CONNECTION_RECYCLES, YB_QUERY_SECONDS,
    YB_RETRIES_EXHAUSTED,
};
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
    /// `None` the verifier falls back to AcceptAll (logged). The thin-client
    /// lane has no AcceptAll mode, so daemons that open it require this
    /// whenever `sslmode` enables TLS (see [`Self::thin_client_tls`]).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sslrootcert: Option<String>,
    /// Path to the PEM-encoded client certificate (chain) presented to the
    /// server, as libpq's `sslcert`. Set together with [`Self::sslkey`]; with
    /// a `cert` HBA rule the certificate's CN is the login, so it must equal
    /// [`Self::user`].
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sslcert: Option<String>,
    /// Path to the PEM-encoded private key for [`Self::sslcert`], as libpq's
    /// `sslkey`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sslkey: Option<String>,
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
    /// The smart driver discovers topology by running `yb_servers()` over a
    /// control connection and then addresses tservers by the per-pod
    /// hostnames it learned. Replace the tserver StatefulSet — as a K8s
    /// rollout that renames the pods does — and every cached hostname becomes
    /// NXDOMAIN while the configured bootstrap host (a headless service) still
    /// resolves. The driver refreshes its cache through the configured hosts
    /// (every `yb_servers_refresh_interval`, 300s by default) and falls back
    /// to a plain connect against them when that refresh fails, so a wedged
    /// process recovers on its own within one refresh interval. A recycle
    /// reopens through that same driver path, so the bound does not fix
    /// staleness independently of it; it exists to keep this lane in step with
    /// `yb_thin_client`, whose session pool resolves its addresses exactly
    /// once and has no refresh of its own, and to give every connection a
    /// bounded life regardless of lane.
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
/// is fatal here, so nothing retries it. The GC shrink delete
/// (`page_store::gc_act_shrink_fork`) deletes every version of every block
/// above a fork's new length in one unbatched statement, and the candidate is
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
/// guards against is measured in days (#811), so minutes is the right order of
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
        if let Some(cert) = &self.sslcert {
            params.push(format!("sslcert={}", percent_encode(cert)));
        }
        if let Some(key) = &self.sslkey {
            params.push(format!("sslkey={}", percent_encode(key)));
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
        if let Some(c) = &self.sslcert {
            parts.push(format!("sslcert = {}", q(c)));
        }
        if let Some(k) = &self.sslkey {
            parts.push(format!("sslkey = {}", q(k)));
        }
        if self.disable_transactional_writes {
            parts.push("disable_transactional_writes = true".to_string());
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

/// The client identity a connection presents to YugabyteDB: libpq's `sslcert`
/// and `sslkey`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ClientIdentity {
    pub cert_path: String,
    pub key_path: String,
}

/// The files the thin-client lane hands the shim. The shim always verifies
/// the tserver against `ca_cert_path`, and authenticates with `client` when
/// it is set.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TlsFiles {
    pub ca_cert_path: String,
    pub client: Option<ClientIdentity>,
}

/// What the client certificate on disk says about itself.
#[derive(Clone, Debug, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct ClientCertInfo {
    pub common_name: Option<String>,
    /// The `subjectAltName` DNS entries — the names a client actually matches
    /// against, since a modern client ignores the CN. Empty when the extension
    /// is absent. `serde(default)` because this was added after the type was
    /// already on the daemons' status wire, where an older peer sends none.
    #[serde(default)]
    pub dns_names: Vec<String>,
    /// Hex, as `openssl x509 -serial` prints it.
    pub serial: String,
    pub not_before_unix: u64,
    pub not_after_unix: u64,
}

/// What a parsed certificate says about itself. No validation: a caller that
/// only wants to watch an expiry — a metrics gauge, a status line — must not be
/// refused an answer because the certificate has already expired, which is
/// precisely the case it exists to report.
fn describe_certificate(cert: &x509_cert::Certificate) -> ClientCertInfo {
    use x509_cert::der::asn1::{PrintableStringRef, Utf8StringRef};
    use x509_cert::ext::pkix::SubjectAltName;
    use x509_cert::ext::pkix::name::GeneralName;

    let tbs = &cert.tbs_certificate;
    // The names a client matches against. Only DNS names: this describes a
    // Postgres server or client certificate, and neither is reached by IP here.
    // An absent or unparseable extension is an empty list rather than an error —
    // a caller watching an expiry must still get its answer.
    let dns_names = tbs
        .get::<SubjectAltName>()
        .ok()
        .flatten()
        .map(|(_critical, san)| {
            san.0
                .iter()
                .filter_map(|name| match name {
                    GeneralName::DnsName(dns) => Some(dns.as_str().to_string()),
                    _ => None,
                })
                .collect()
        })
        .unwrap_or_default();
    let common_name = tbs
        .subject
        .0
        .iter()
        .flat_map(|rdn| rdn.0.iter())
        .find(|atv| atv.oid == CN_OID)
        .and_then(|atv| {
            Utf8StringRef::try_from(&atv.value)
                .map(|s| s.as_str().to_string())
                .or_else(|_| {
                    PrintableStringRef::try_from(&atv.value).map(|s| s.as_str().to_string())
                })
                .ok()
        });
    ClientCertInfo {
        common_name,
        dns_names,
        serial: tbs
            .serial_number
            .as_bytes()
            .iter()
            .map(|b| format!("{b:02X}"))
            .collect(),
        not_before_unix: tbs.validity.not_before.to_unix_duration().as_secs(),
        not_after_unix: tbs.validity.not_after.to_unix_duration().as_secs(),
    }
}

/// Describe the FIRST certificate in a PEM bundle, without validating it.
///
/// For watching a mounted certificate's expiry from outside a connection block:
/// the supervisor reports the compute's own server certificate and the CA that
/// issued it this way, and neither is a certificate this crate connects with.
/// `Ok(None)` when the bytes carry no PEM CERTIFICATE block, which is how an
/// issuer that ships no `ca.crt` (ACME) reads.
pub fn describe_certificate_pem(pem: &[u8]) -> anyhow::Result<Option<ClientCertInfo>> {
    let Some(chain) = load_pem_chain_guarded(pem)? else {
        return Ok(None);
    };
    Ok(chain.first().map(describe_certificate))
}

/// Postgres' `SSLRequest`: length 8, then the magic 80877103. The server answers
/// one byte — `S` to proceed with TLS, `N` to refuse — before any TLS bytes move.
const SSL_REQUEST: [u8; 8] = [0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f];

/// The leaf certificate a Postgres server is presenting RIGHT NOW, from a bare
/// handshake — no authentication, no query, no connection kept.
///
/// This is the only place the loaded certificate is observable. Postgres re-reads
/// `ssl_cert_file` at each reload and uses the new context for *new* backends
/// only, and nothing in SQL reports it: `SHOW ssl_cert_file` gives the configured
/// path, which does not change when the bytes behind it do, and
/// `pg_conf_load_time()` advances even for a reload whose new pair failed to load
/// and left the OLD context in place (`be_tls_init` swaps `SSL_context` only on
/// success, and logs otherwise). So a caller that needs to know whether a renewal
/// actually took has to ask the wire.
///
/// Validation is deliberately OFF, and that is not a weakening: the question is
/// WHICH certificate is being served, not whether it is trustworthy, and the
/// caller already holds the file it is comparing against. Nothing is sent, so no
/// credential is at risk on an unverified connection.
///
/// `Ok(None)` when the server answers `N` — it is not offering TLS at all, which
/// for a caller watching a certificate is an answer rather than an error.
pub fn served_certificate_der(
    host: &str,
    port: u16,
    timeout: std::time::Duration,
) -> anyhow::Result<Option<Vec<u8>>> {
    use std::io::{Read, Write};
    use std::net::ToSocketAddrs;

    let addr = (host, port)
        .to_socket_addrs()
        .with_context(|| format!("resolving `{host}:{port}`"))?
        .next()
        .ok_or_else(|| anyhow::anyhow!("`{host}:{port}` resolved to nothing"))?;
    let mut sock = std::net::TcpStream::connect_timeout(&addr, timeout)
        .with_context(|| format!("connecting to `{host}:{port}`"))?;
    sock.set_read_timeout(Some(timeout))?;
    sock.set_write_timeout(Some(timeout))?;
    sock.write_all(&SSL_REQUEST).context("sending SSLRequest")?;
    let mut answer = [0u8; 1];
    sock.read_exact(&mut answer)
        .context("reading the SSLRequest answer")?;
    if answer[0] != b'S' {
        return Ok(None);
    }

    #[derive(Debug)]
    struct AcceptAny;
    impl rustls::client::danger::ServerCertVerifier for AcceptAny {
        fn verify_server_cert(
            &self,
            _end_entity: &rustls::pki_types::CertificateDer<'_>,
            _intermediates: &[rustls::pki_types::CertificateDer<'_>],
            _server_name: &rustls::pki_types::ServerName<'_>,
            _ocsp: &[u8],
            _now: rustls::pki_types::UnixTime,
        ) -> Result<rustls::client::danger::ServerCertVerified, rustls::Error> {
            Ok(rustls::client::danger::ServerCertVerified::assertion())
        }
        fn verify_tls12_signature(
            &self,
            _message: &[u8],
            _cert: &rustls::pki_types::CertificateDer<'_>,
            _dss: &rustls::DigitallySignedStruct,
        ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
            Ok(rustls::client::danger::HandshakeSignatureValid::assertion())
        }
        fn verify_tls13_signature(
            &self,
            _message: &[u8],
            _cert: &rustls::pki_types::CertificateDer<'_>,
            _dss: &rustls::DigitallySignedStruct,
        ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
            Ok(rustls::client::danger::HandshakeSignatureValid::assertion())
        }
        fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
            rustls::crypto::ring::default_provider()
                .signature_verification_algorithms
                .supported_schemes()
        }
    }

    let config = rustls::ClientConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .context("building a rustls config")?
    .dangerous()
    .with_custom_certificate_verifier(Arc::new(AcceptAny))
    .with_no_client_auth();

    // The name is never matched against anything (the verifier accepts all), but
    // rustls requires one for SNI.
    let name = rustls::pki_types::ServerName::try_from("localhost")
        .context("localhost is not a server name")?
        .to_owned();
    let mut conn = rustls::ClientConnection::new(Arc::new(config), name)
        .context("starting the TLS handshake")?;
    let mut tls = rustls::Stream::new(&mut conn, &mut sock);
    // Drives the handshake without sending a byte of Postgres protocol: there is
    // no startup message, so the server never gets as far as asking who we are.
    tls.flush().context("completing the TLS handshake")?;

    let leaf = conn
        .peer_certificates()
        .and_then(|c| c.first())
        .map(|c| c.as_ref().to_vec())
        .ok_or_else(|| anyhow::anyhow!("the server presented no certificate"))?;
    Ok(Some(leaf))
}

/// The DER of the first certificate in a PEM bundle — byte for byte what a TLS
/// server presents as its leaf, which is what makes it comparable with
/// [`served_certificate_der`] without re-parsing either side.
///
/// `None` when the bytes carry no certificate: absent, empty, truncated, or a
/// PEM block of some other type. Same contract as [`describe_certificate_pem`],
/// because both read files a kubelet writes.
pub fn leaf_der_from_pem(pem: &[u8]) -> Option<Vec<u8>> {
    rustls_pemfile::certs(&mut std::io::Cursor::new(pem))
        .next()?
        .ok()
        .map(|c| c.as_ref().to_vec())
}

/// The `BEGIN CERTIFICATE` armour, which `load_pem_chain` requires and does not
/// check for.
const PEM_CERT_BEGIN: &[u8] = b"-----BEGIN CERTIFICATE-----";

/// `Certificate::load_pem_chain`, minus the panic.
///
/// x509-cert 0.2.5 subtracts with overflow inside `load_pem_chain` when the
/// input carries no PEM block at all (`certificate.rs:256`) — so an empty or
/// truncated mount does not fail the read, it kills the thread. Both callers
/// read files a kubelet writes, where empty-for-a-moment is a real state, so the
/// armour is checked first and absence is reported as `Ok(None)` rather than
/// reaching upstream.
fn load_pem_chain_guarded(pem: &[u8]) -> anyhow::Result<Option<Vec<x509_cert::Certificate>>> {
    if !pem
        .windows(PEM_CERT_BEGIN.len())
        .any(|w| w == PEM_CERT_BEGIN)
    {
        return Ok(None);
    }
    x509_cert::Certificate::load_pem_chain(pem)
        .map(Some)
        .context("parsing the PEM chain")
}

/// The TLS posture of a connection block, as the daemons' status endpoints
/// report it.
#[derive(Clone, Debug, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct TlsStatus {
    pub enabled: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sslmode: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sslrootcert: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub client_cert: Option<ClientCertInfo>,
    /// Why `client_cert` is absent although `sslcert` is set.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub client_cert_error: Option<String>,
    /// How many times this process has noticed a watched TLS file change and
    /// asked its pools to reconnect. `client_cert` above is re-read from disk
    /// per request, so it moves when cert-manager writes the file; this moves
    /// only when a watcher acted on it.
    #[serde(default)]
    pub reloads: u64,
}

const CN_OID: x509_cert::der::oid::ObjectIdentifier =
    x509_cert::der::oid::ObjectIdentifier::new_unwrap("2.5.4.3");

impl YbConnConfig {
    /// Whether `sslmode` selects TLS at all (`None` and `disable` do not).
    pub fn tls_enabled(&self) -> bool {
        !matches!(self.sslmode.as_deref(), None | Some("disable"))
    }

    fn verifies_server(&self) -> bool {
        matches!(
            self.sslmode.as_deref(),
            Some("verify-ca") | Some("verify-full")
        )
    }

    /// `sslcert` and `sslkey` as one identity. Both or neither.
    pub fn client_identity(&self) -> anyhow::Result<Option<ClientIdentity>> {
        match (&self.sslcert, &self.sslkey) {
            (None, None) => Ok(None),
            (Some(cert), Some(key)) => Ok(Some(ClientIdentity {
                cert_path: cert.clone(),
                key_path: key.clone(),
            })),
            (Some(_), None) => {
                anyhow::bail!("sslcert is set but sslkey is not; a client certificate needs both")
            }
            (None, Some(_)) => {
                anyhow::bail!("sslkey is set but sslcert is not; a client certificate needs both")
            }
        }
    }

    /// Reject combinations that would otherwise fail later, in a less legible
    /// place: a client certificate without TLS, or a verifying `sslmode` with
    /// nothing to verify against.
    pub fn validate(&self) -> anyhow::Result<()> {
        let identity = self.client_identity()?;
        if !self.tls_enabled() {
            anyhow::ensure!(
                identity.is_none(),
                "sslcert/sslkey are set but sslmode is {:?}; a client certificate is only \
                 presented over TLS",
                self.sslmode
            );
            return Ok(());
        }
        if self.verifies_server() {
            anyhow::ensure!(
                self.sslrootcert.is_some(),
                "sslmode={} requires sslrootcert",
                self.sslmode.as_deref().unwrap_or_default()
            );
        }
        // A client certificate identifies this process to whoever answers. On
        // an `sslmode` that does not verify the server, that is any peer on
        // the address, so refuse the pair rather than hand the identity out.
        anyhow::ensure!(
            identity.is_none() || self.sslrootcert.is_some(),
            "sslcert/sslkey are set but sslrootcert is not; a client certificate is only \
             presented to a server this process can verify"
        );
        Ok(())
    }

    /// TLS for the thin-client lane, or `None` when TLS is off.
    ///
    /// The shim verifies the tserver unconditionally and has no accept-all
    /// mode, so TLS without a CA cannot work there; it is reported here, as
    /// configuration, rather than as a handshake failure.
    pub fn thin_client_tls(&self) -> anyhow::Result<Option<TlsFiles>> {
        self.validate()?;
        if !self.tls_enabled() {
            return Ok(None);
        }
        let ca_cert_path = self.sslrootcert.clone().ok_or_else(|| {
            anyhow::anyhow!(
                "sslmode={} enables TLS but sslrootcert is unset; the thin-client lane needs \
                 the CA to verify the tserver",
                self.sslmode.as_deref().unwrap_or_default()
            )
        })?;
        Ok(Some(TlsFiles {
            ca_cert_path,
            client: self.client_identity()?,
        }))
    }

    /// Read and check the client certificate, when one is configured.
    ///
    /// The key must parse, the certificate must be inside its validity
    /// window, and without a password its CN must equal `user`, since a
    /// `cert` HBA rule logs the CN in and nothing else could. Errors name the
    /// file so a bad mount reads as a bad mount, not as an RPC failure.
    pub fn inspect_client_certificate(&self) -> anyhow::Result<Option<ClientCertInfo>> {
        let Some(id) = self.client_identity()? else {
            return Ok(None);
        };
        let pem = std::fs::read(&id.cert_path)
            .with_context(|| format!("reading sslcert `{}`", id.cert_path))?;
        let chain = load_pem_chain_guarded(&pem)
            .with_context(|| format!("parsing sslcert `{}`", id.cert_path))?
            .unwrap_or_default();
        let leaf = chain.first().ok_or_else(|| {
            anyhow::anyhow!(
                "sslcert `{}` contains no PEM CERTIFICATE blocks",
                id.cert_path
            )
        })?;
        let key_pem = std::fs::read(&id.key_path)
            .with_context(|| format!("reading sslkey `{}`", id.key_path))?;
        rustls_pemfile::private_key(&mut std::io::Cursor::new(key_pem))
            .with_context(|| format!("parsing sslkey `{}`", id.key_path))?
            .ok_or_else(|| {
                anyhow::anyhow!("sslkey `{}` contains no PEM private key", id.key_path)
            })?;

        let info = describe_certificate(leaf);

        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs())
            .unwrap_or(0);
        anyhow::ensure!(
            now >= info.not_before_unix,
            "sslcert `{}` is not valid yet (not_before={} now={now})",
            id.cert_path,
            info.not_before_unix
        );
        anyhow::ensure!(
            now <= info.not_after_unix,
            "sslcert `{}` expired (not_after={} now={now})",
            id.cert_path,
            info.not_after_unix
        );
        if info.common_name.as_deref() != Some(self.user.as_str()) {
            let msg = format!(
                "sslcert `{}` has CN {:?} but the connection logs in as user `{}`",
                id.cert_path, info.common_name, self.user
            );
            if self.password_env.is_none() {
                anyhow::bail!(
                    "{msg}; with no password the certificate is the login, so its CN must match"
                );
            }
            tracing::warn!(
                "{msg}; the server will accept it only if the HBA rule does not check the CN"
            );
        }
        Ok(Some(info))
    }

    /// The status endpoints' view of this block. Never fails: a certificate
    /// that cannot be read is reported as such instead.
    pub fn tls_status(&self) -> TlsStatus {
        let (client_cert, client_cert_error) = match self.inspect_client_certificate() {
            Ok(info) => (info, None),
            Err(e) => (None, Some(format!("{e:#}"))),
        };
        TlsStatus {
            enabled: self.tls_enabled(),
            sslmode: self.sslmode.clone(),
            sslrootcert: self.sslrootcert.clone(),
            client_cert,
            client_cert_error,
            reloads: yb_tls::reloads(),
        }
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
    /// PEM client certificate presented to the server (with `--sslkey`).
    #[arg(long, requires = "sslkey")]
    pub sslcert: Option<String>,
    /// PEM private key for `--sslcert`.
    #[arg(long, requires = "sslcert")]
    pub sslkey: Option<String>,
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
            sslcert: self.sslcert.clone(),
            sslkey: self.sslkey.clone(),
            // Diagnostic tools open read-only connections (no fast-path
            // upsert), so the GUC is never set regardless; keep the default.
            disable_transactional_writes: false,
            tserver_rpc_addrs: None,
            tsclient_read_sessions: None,
            tsclient_write_sessions: None,
            tsclient_sessions_per_conn: None,
            // Unbounded, unlike a daemon. These tools are run by hand during
            // an incident and do the long scans the daemons never issue —
            // reset-consistent-lsn replays the WAL decoder over
            // `amp_storage.wals` — so the operator watching it is the timeout.
            // The smart driver still bounds its own control connection (15s
            // per configured host, see `build_pg_config`); a cluster that
            // never answers costs that much before the plain connect it falls
            // back to hangs as before.
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
            sslcert: None,
            sslkey: None,
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

    #[test]
    fn client_cert_flags_pass_through() {
        let c = YbConnArgs {
            sslmode: Some("verify-full".into()),
            sslrootcert: Some("/tls/ca.crt".into()),
            sslcert: Some("/tls/tls.crt".into()),
            sslkey: Some("/tls/tls.key".into()),
            ..args()
        }
        .into_config();
        assert_eq!(c.sslcert.as_deref(), Some("/tls/tls.crt"));
        assert_eq!(c.sslkey.as_deref(), Some("/tls/tls.key"));
        c.validate().unwrap();
    }
}

#[cfg(test)]
mod database_url_tests {
    use super::{
        Duration, YbConnConfig, YsqlClient, add_startup_gucs, build_pg_config, rpc_addrs_from_hosts,
    };

    fn cfg(host_ports: &str) -> YbConnConfig {
        YbConnConfig {
            host_ports: host_ports.parse().unwrap(),
            user: "yugabyte".into(),
            dbname: "yb_storage".into(),
            password_env: None,
            sslmode: None,
            sslrootcert: None,
            sslcert: None,
            sslkey: None,
            disable_transactional_writes: false,
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

    fn mtls_cfg() -> YbConnConfig {
        let mut c = cfg("h:5433");
        c.sslmode = Some("verify-full".into());
        c.sslrootcert = Some("/tls/ca/ca.crt".into());
        c.sslcert = Some("/tls/client/tls.crt".into());
        c.sslkey = Some("/tls/client/tls.key".into());
        c
    }

    #[test]
    fn client_cert_is_rendered_in_url_and_toml() {
        let c = mtls_cfg();
        assert_eq!(
            c.to_database_url().unwrap(),
            "postgresql://yugabyte@h:5433/yb_storage?sslmode=verify-full\
             &sslrootcert=%2Ftls%2Fca%2Fca.crt&sslcert=%2Ftls%2Fclient%2Ftls.crt\
             &sslkey=%2Ftls%2Fclient%2Ftls.key"
        );
        let toml = c.to_inline_toml();
        assert!(toml.contains("sslcert = \"/tls/client/tls.crt\""), "{toml}");
        assert!(toml.contains("sslkey = \"/tls/client/tls.key\""), "{toml}");
        let back: YbConnConfig = toml::from_str(&format!("c = {toml}"))
            .map(|t: toml::Table| t["c"].clone().try_into().unwrap())
            .unwrap();
        assert_eq!(back, c);
    }

    #[test]
    fn a_client_certificate_needs_both_halves() {
        let mut c = mtls_cfg();
        c.sslkey = None;
        assert!(c.validate().unwrap_err().to_string().contains("sslkey"));
        let mut c = mtls_cfg();
        c.sslcert = None;
        assert!(c.validate().unwrap_err().to_string().contains("sslcert"));
    }

    #[test]
    fn a_client_certificate_without_tls_is_refused() {
        let mut c = mtls_cfg();
        c.sslmode = None;
        assert!(c.validate().is_err());
        c.sslmode = Some("disable".into());
        assert!(c.validate().is_err());
    }

    #[test]
    fn a_client_certificate_needs_a_verified_server() {
        // `require` does not verify the tserver, so presenting an identity to
        // it would hand that identity to anyone on the address.
        let mut c = mtls_cfg();
        c.sslmode = Some("require".into());
        c.sslrootcert = None;
        assert!(
            c.validate()
                .unwrap_err()
                .to_string()
                .contains("sslrootcert")
        );
        c.sslrootcert = Some("/ca.crt".into());
        c.validate().unwrap();
    }

    #[test]
    fn verifying_modes_need_a_root_cert() {
        for mode in ["verify-ca", "verify-full"] {
            let mut c = cfg("h:5433");
            c.sslmode = Some(mode.into());
            assert!(c.validate().is_err(), "{mode}");
            c.sslrootcert = Some("/ca.crt".into());
            c.validate().unwrap();
        }
        // `require` keeps its accept-all behaviour on the YSQL lane.
        let mut c = cfg("h:5433");
        c.sslmode = Some("require".into());
        c.validate().unwrap();
    }

    #[test]
    fn thin_client_tls_follows_sslmode_and_demands_a_ca() {
        assert_eq!(cfg("h:5433").thin_client_tls().unwrap(), None);
        let t = mtls_cfg().thin_client_tls().unwrap().unwrap();
        assert_eq!(t.ca_cert_path, "/tls/ca/ca.crt");
        assert_eq!(
            t.client.unwrap(),
            super::ClientIdentity {
                cert_path: "/tls/client/tls.crt".into(),
                key_path: "/tls/client/tls.key".into(),
            }
        );
        // The shim has no accept-all mode, so TLS without a CA is a config
        // error here rather than a handshake failure later.
        let mut c = cfg("h:5433");
        c.sslmode = Some("require".into());
        let err = c.thin_client_tls().unwrap_err().to_string();
        assert!(err.contains("sslrootcert"), "{err}");
    }

    #[test]
    fn missing_client_cert_files_are_named() {
        let c = mtls_cfg();
        let err = c.inspect_client_certificate().unwrap_err().to_string();
        assert!(err.contains("/tls/client/tls.crt"), "{err}");
        let status = c.tls_status();
        assert!(status.enabled);
        assert!(status.client_cert.is_none());
        assert!(
            status
                .client_cert_error
                .unwrap()
                .contains("/tls/client/tls.crt")
        );
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

    #[test]
    fn startup_gucs_follow_the_statement_timeout() {
        let cfg: YbConnConfig = toml::from_str(
            "host_ports = \"h:5433\"\nuser = \"u\"\ndbname = \"d\"\n\
             statement_timeout_ms = 5000\n",
        )
        .unwrap();

        let mut pg = build_pg_config(&cfg, "test").unwrap();
        add_startup_gucs(&mut pg, &[("a.b", "on"), ("c", "1")]).unwrap();
        assert_eq!(
            pg.get_options(),
            Some("-c statement_timeout=5000 -c a.b=on -c c=1")
        );
        assert!(add_startup_gucs(&mut pg, &[("a", "x y")]).is_err());
        assert!(add_startup_gucs(&mut pg, &[("a", "x\\y")]).is_err());
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
    /// what the pageserver's unknown-key check diffs against.
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
    fn tserver_rpc_addrs_round_trips() {
        // The override survives the round trip and drives
        // `effective_tserver_rpc_addrs`.
        let mut c = cfg("h1:5433,h2:5433");
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

/// Builds the rustls connector one connection will use; see
/// [`YsqlClient::connect_rustls_pooled_with`].
pub type MakeTlsFn =
    Arc<dyn Fn() -> Result<Arc<yb_postgres_rustls::MakeRustlsConnect>> + Send + Sync>;

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
    /// Bumped by [`Self::recycle_now`]. A slot opened under an older value is
    /// reconnected on its next use, which is how a renewed client certificate
    /// reaches the pool without waiting out the lifetime. Shared with the
    /// TLS file watcher (`tls_reload`) through a `Weak`, so the watcher stops
    /// when the client is dropped.
    reload_generation: Arc<std::sync::atomic::AtomicU64>,
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
    /// The [`YsqlClient::reload_generation`] this connection was opened under.
    generation: u64,
}

impl State {
    fn new(
        client: tokio_postgres::Client,
        max_lifetime: Option<Duration>,
        generation: u64,
    ) -> State {
        State {
            client: Arc::new(client),
            prepared: HashMap::new(),
            opened_at: std::time::Instant::now(),
            lifetime: max_lifetime.map(jittered),
            recycle_after: None,
            generation,
        }
    }

    /// Whether this slot's connection should be replaced: it has outlived its
    /// lifetime or predates a [`YsqlClient::recycle_now`], and no cooldown from
    /// a failed reopen is in force.
    fn recycle_due(&self, current_generation: u64) -> bool {
        recycle_due_at(
            self.opened_at,
            self.lifetime,
            self.recycle_after,
            std::time::Instant::now(),
            self.generation != current_generation,
        )
    }

    /// Install a freshly-opened connection, dropping the prepared-statement
    /// cache (its `Statement`s belong to the connection being replaced) and
    /// restarting the lifetime clock.
    fn replace(
        &mut self,
        client: tokio_postgres::Client,
        max_lifetime: Option<Duration>,
        generation: u64,
    ) {
        self.client = Arc::new(client);
        self.prepared.clear();
        self.opened_at = std::time::Instant::now();
        self.lifetime = max_lifetime.map(jittered);
        self.recycle_after = None;
        self.generation = generation;
    }
}

/// The recycle predicate, with `now` and the slot's timestamps passed in so
/// it can be exercised without a live connection (`tokio_postgres::Client`
/// has no public constructor).
///
/// `lifetime: None` is the unbounded case. `stale` is a connection opened
/// before the last [`YsqlClient::recycle_now`]: due regardless of lifetime.
/// `recycle_after` is the cooldown a failed reopen installed: while it is in
/// the future the connection stays deliberately, because replacing it is
/// what just failed.
fn recycle_due_at(
    opened_at: std::time::Instant,
    lifetime: Option<Duration>,
    recycle_after: Option<std::time::Instant>,
    now: std::time::Instant,
    stale: bool,
) -> bool {
    if !stale {
        let Some(lifetime) = lifetime else {
            return false;
        };
        if now.saturating_duration_since(opened_at) < lifetime {
            return false;
        }
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
        let tls = Arc::new(tls);
        Self::connect_rustls_pooled_with(
            config,
            Arc::new(move || Ok(tls.clone())),
            pool_size,
            set_fast_path_upsert,
            disable_transactional_writes,
        )
        .await
    }

    /// Like [`Self::connect_rustls_pooled`], but builds the connector afresh
    /// for every connection opened, so a connector that reads certificate
    /// files from disk presents the files as they are at (re)connect time.
    pub async fn connect_rustls_pooled_with(
        config: tokio_postgres::Config,
        make_tls: MakeTlsFn,
        pool_size: usize,
        set_fast_path_upsert: bool,
        disable_transactional_writes: bool,
    ) -> Result<Self> {
        let config = Arc::new(config);
        let open: OpenFn = {
            let config = config.clone();
            Arc::new(move || {
                let config = config.clone();
                let make_tls = make_tls.clone();
                async move {
                    let tls = make_tls()?;
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
        Self::from_yb_config_pooled(cfg, application_name, 1, set_fast_path_upsert, &[]).await
    }

    /// Same as [`Self::from_yb_config`] but opens `pool_size`
    /// independent connections. Use for hot paths that benefit from
    /// parallel SQL execution (e.g. the pageserver's compute-reads
    /// pool at `pool_size = 4`).
    ///
    /// `gucs` are sent as `-c name=value` in the startup `options`, so every
    /// session this client opens starts with them set — including one it
    /// reopens after the connection dies, which a `SET` issued through the
    /// client does not survive.
    pub async fn from_yb_config_pooled(
        cfg: &YbConnConfig,
        application_name: &str,
        pool_size: usize,
        set_fast_path_upsert: bool,
        gucs: &[(&str, &str)],
    ) -> Result<Self> {
        cfg.validate()?;
        if let Some(info) = cfg.inspect_client_certificate()? {
            YB_CLIENT_CERT_NOT_AFTER_SECONDS.set(info.not_after_unix as i64);
        }
        let mut pg_config = build_pg_config(cfg, application_name)?;
        add_startup_gucs(&mut pg_config, gucs)?;
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
                // Built per connection so a reconnect — lifetime-driven or a
                // `recycle_now` from the file watcher — presents the renewed
                // certificate. A failure to read the files at reconnect keeps
                // the current connection, as any failed reopen does.
                let sslrootcert = cfg.sslrootcert.clone();
                let identity = cfg.client_identity()?;
                build_yb_rustls_connector(sslrootcert.as_deref(), identity.as_ref())
                    .context("building the TLS connector")?;
                let make_tls: MakeTlsFn = Arc::new(move || {
                    build_yb_rustls_connector(sslrootcert.as_deref(), identity.as_ref())
                        .map(Arc::new)
                });
                Self::connect_rustls_pooled_with(
                    pg_config,
                    make_tls,
                    pool_size,
                    set_fast_path_upsert,
                    disable_transactional_writes,
                )
                .await?
            }
        };
        let client = match cfg.max_connection_lifetime() {
            Some(max) => {
                let client = client.with_max_connection_lifetime(max);
                tls_reload::watch_for_client(cfg, application_name, &client);
                client
            }
            // An unbounded lifetime is an explicit "do not recycle this
            // connection underneath me" — what `YbConnArgs::into_config` sets
            // for the diagnostic CLIs, whose long single passes ride on their
            // prepared statements. A reload forces a reopen regardless of age,
            // so honour the opt-out and start no watch.
            None => client,
        };
        Ok(client)
    }

    async fn from_open(open: OpenFn, pool_size: usize) -> Result<Self> {
        let pool_size = pool_size.max(1);
        let mut pool = Vec::with_capacity(pool_size);
        for _ in 0..pool_size {
            let client = (open)().await?;
            pool.push(tokio::sync::Mutex::new(State::new(client, None, 0)));
        }
        Ok(Self {
            open,
            pool,
            next: std::sync::atomic::AtomicUsize::new(0),
            max_lifetime: None,
            reload_generation: Arc::new(std::sync::atomic::AtomicU64::new(0)),
        })
    }

    /// Replace every pooled connection on its next use, each re-running the
    /// open closure — which on the TLS path re-reads the certificate files.
    /// Infallible and non-blocking: a reopen that fails keeps the current
    /// connection and retries after [`RECYCLE_RETRY_COOLDOWN`], as a
    /// lifetime-driven recycle does.
    pub fn recycle_now(&self) {
        self.reload_generation
            .fetch_add(1, std::sync::atomic::Ordering::AcqRel);
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
        let generation = self
            .reload_generation
            .load(std::sync::atomic::Ordering::Acquire);
        if guard.client.is_closed() {
            tracing::info!(slot, "YsqlClient: previous client closed, reconnecting");
            let client = (self.open)().await?;
            guard.replace(client, self.max_lifetime, generation);
        } else if guard.recycle_due(generation) {
            let age = guard.opened_at.elapsed();
            let reason = if guard.generation != generation {
                "after a recycle_now (TLS files changed)"
            } else {
                "past its lifetime"
            };
            match (self.open)().await {
                Ok(client) => {
                    tracing::info!(
                        slot,
                        age_secs = age.as_secs(),
                        "YsqlClient: recycling connection {reason}"
                    );
                    guard.replace(client, self.max_lifetime, generation);
                    YB_CONNECTION_RECYCLES.with_label_values(&["ok"]).inc();
                }
                Err(e) => {
                    tracing::warn!(
                        slot,
                        age_secs = age.as_secs(),
                        "YsqlClient: could not recycle connection {reason}, \
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
        let generation = self
            .reload_generation
            .load(std::sync::atomic::Ordering::Acquire);
        guard.replace(client, self.max_lifetime, generation);
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
    identity: Option<&ClientIdentity>,
) -> anyhow::Result<yb_postgres_rustls::MakeRustlsConnect> {
    use rustls::client::WebPkiServerVerifier;
    use rustls::client::danger::{ServerCertVerified, ServerCertVerifier};
    use rustls::crypto::ring;

    let client_auth = identity
        .map(|id| -> anyhow::Result<_> {
            let pem = std::fs::read(&id.cert_path)
                .with_context(|| format!("reading sslcert file `{}`", id.cert_path))?;
            let certs = rustls_pemfile::certs(&mut std::io::Cursor::new(pem))
                .collect::<Result<Vec<_>, _>>()
                .with_context(|| format!("parsing PEM certs in `{}`", id.cert_path))?;
            anyhow::ensure!(
                !certs.is_empty(),
                "sslcert `{}` contains no PEM CERTIFICATE blocks",
                id.cert_path
            );
            let pem = std::fs::read(&id.key_path)
                .with_context(|| format!("reading sslkey file `{}`", id.key_path))?;
            let key = rustls_pemfile::private_key(&mut std::io::Cursor::new(pem))
                .with_context(|| format!("parsing PEM key in `{}`", id.key_path))?
                .ok_or_else(|| {
                    anyhow::anyhow!("sslkey `{}` contains no PEM private key", id.key_path)
                })?;
            Ok((certs, key))
        })
        .transpose()?;

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
        let builder = builder.with_root_certificates(roots);
        match client_auth {
            Some((certs, key)) => builder
                .with_client_auth_cert(certs, key)
                .context("loading the client certificate into rustls")?,
            None => builder.with_no_client_auth(),
        }
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
        let builder = builder
            .dangerous()
            .with_custom_certificate_verifier(verifier);
        match client_auth {
            Some((certs, key)) => builder
                .with_client_auth_cert(certs, key)
                .context("loading the client certificate into rustls")?,
            None => builder.with_no_client_auth(),
        }
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
    // connections across tservers. It dials the configured hosts first on
    // every refresh, bounds the control connection (10s connect / 15s login /
    // 10s `yb_servers()`), and falls back to a plain connect against the
    // configured hosts when the refresh fails outright. That is the only
    // fallback: when the refresh succeeds but `only-primary` filters every
    // discovered server out, the driver returns the error rather than hand us
    // a node of the kind we ruled out, which is what we want.
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

/// Append `gucs` to the startup `options`, after anything
/// [`build_pg_config`] put there.
fn add_startup_gucs(pg_config: &mut tokio_postgres::Config, gucs: &[(&str, &str)]) -> Result<()> {
    let mut options = pg_config.get_options().unwrap_or_default().to_owned();
    for (name, value) in gucs {
        // The server splits `options` on whitespace and treats `\` as an
        // escape, so either would need escaping; nothing needs one yet.
        let special = |c: char| c.is_whitespace() || c == '\\';
        anyhow::ensure!(
            !name.contains(special) && !value.contains(special),
            "startup GUC `{name}={value}` contains whitespace or a backslash"
        );
        if !options.is_empty() {
            options.push(' ');
        }
        options.push_str(&format!("-c {name}={value}"));
    }
    if !options.is_empty() {
        pg_config.options(&options);
    }
    Ok(())
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
        // Unbounded: never due, however old — unless a recycle_now is pending.
        assert!(!recycle_due_at(opened, None, None, now, false));
        assert!(recycle_due_at(opened, None, None, now, true));
        // Younger than its lifetime: not due, unless stale.
        assert!(!recycle_due_at(
            opened,
            Some(Duration::from_secs(2_000)),
            None,
            now,
            false
        ));
        assert!(recycle_due_at(
            opened,
            Some(Duration::from_secs(2_000)),
            None,
            now,
            true
        ));
        // Past it: due.
        assert!(recycle_due_at(
            opened,
            Some(Duration::from_secs(900)),
            None,
            now,
            false
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
        assert!(!recycle_due_at(opened, lifetime, cooling, now, false));
        // The cooldown also paces a recycle_now.
        assert!(!recycle_due_at(opened, lifetime, cooling, now, true));

        let expired = Some(now - Duration::from_millis(1));
        assert!(recycle_due_at(opened, lifetime, expired, now, false));
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

#[cfg(test)]
mod cert_tests {
    /// x509-cert 0.2.5 panics rather than erroring when handed bytes with no PEM
    /// armour, and both callers read files a kubelet writes — where empty for a
    /// moment is a real state, not a corruption. A panic there kills the thread
    /// that was watching for the file to arrive.
    #[test]
    fn bytes_with_no_pem_block_are_absence_not_a_panic() {
        for bytes in [
            b"".as_slice(),
            b"not a certificate",
            b"-----BEGIN PRIVATE KEY-----",
        ] {
            assert_eq!(
                super::describe_certificate_pem(bytes).unwrap(),
                None,
                "{:?} should read as no certificate",
                String::from_utf8_lossy(bytes)
            );
        }
    }
}
