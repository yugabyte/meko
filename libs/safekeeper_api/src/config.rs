//! The safekeeper's on-disk configuration: `safekeeper.toml`, read once at
//! startup. This mirrors `pageserver_api::config::ConfigToml` — neon_local
//! writes the file into the safekeeper datadir and the process deserializes
//! it. Only operational flags stay on the CLI (`-D`/`--datadir` to locate
//! the file, `--init`, `--dump-control-file`); every real setting, including
//! the node id, lives here.
//!
//! All fields have defaults (`#[serde(default)]` + the `Default` impl below),
//! so a partial `safekeeper.toml` is valid. Unknown keys are **rejected**
//! (`deny_unknown_fields`): a misspelled or stale config item fails the
//! safekeeper at startup rather than being silently ignored.

use std::time::Duration;

use serde::{Deserialize, Serialize};
use utils::id::{NodeId, TenantId, TimelineId};

pub mod defaults {
    pub use crate::{DEFAULT_HTTP_LISTEN_ADDR, DEFAULT_PG_LISTEN_ADDR};

    pub const DEFAULT_HEARTBEAT_TIMEOUT: &str = "5000ms";

    /// Capacity (MiB) of the in-memory cache of recently-written WAL chunks
    /// in YB-WAL mode. Set to 0 to disable the cache entirely.
    pub const DEFAULT_WAL_CACHE_SIZE_MIB: usize = 256;

    /// Default per-timeline disk usage limit is 0 (disabled) — yb-light has
    /// no local-FS WAL anyway.
    pub const DEFAULT_MAX_TIMELINE_DISK_USAGE_BYTES: u64 = 0;

    pub const DEFAULT_CONTROL_FILE_SAVE_INTERVAL: &str = "300s";

    pub const DEFAULT_SSL_KEY_FILE: &str = "server.key";
    pub const DEFAULT_SSL_CERT_FILE: &str = "server.crt";
    pub const DEFAULT_SSL_CERT_RELOAD_PERIOD: &str = "60s";

    pub const DEFAULT_GLOBAL_DISK_CHECK_INTERVAL: &str = "60s";
    pub const DEFAULT_MAX_GLOBAL_DISK_USAGE_RATIO: f64 = 0.0;

    /// Multiplier on scale-derived defaults (WAL cache size, worker-thread
    /// counts). 1.0 preserves historical sizing.
    pub const DEFAULT_SCALE_FACTOR: f64 = 1.0;

    pub const DEFAULT_LOG_FORMAT: &str = "plain";
}

/// The deserialized `safekeeper.toml`. The safekeeper builds its runtime
/// `SafeKeeperConf` from this (loading the auth-key / ssl-CA files the path
/// fields point at); neon_local serializes it.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct SafekeeperConfigToml {
    /// Safekeeper node id. Must be stable across restarts of this instance.
    pub id: NodeId,
    /// The single (tenant, timeline) this process serves for its lifetime
    /// (single-timeline-per-process). Required in YB-WAL mode.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tenant_id: Option<TenantId>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub timeline_id: Option<TimelineId>,

    pub listen_pg: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub listen_pg_tenant_only: Option<String>,
    pub listen_http: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub listen_https: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub advertise_pg: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub availability_zone: Option<String>,

    #[serde(with = "humantime_serde")]
    pub heartbeat_timeout: Duration,

    /// YB connection for mirroring live WAL, as a string-encoded inline TOML
    /// table (the same `[*.yb]` block the other components consume) — matches
    /// how `pageserver.toml` stores `remote_storage`. Parsed by the
    /// safekeeper into a `ysql_client::YbConnConfig`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub yb_storage_conn: Option<String>,
    /// Capacity (MiB) of the recent-WAL cache. `None` derives it from
    /// `DEFAULT_WAL_CACHE_SIZE_MIB * scale_factor`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub wal_cache_size_mib: Option<usize>,
    pub scale_factor: f64,

    pub max_timeline_disk_usage_bytes: u64,
    #[serde(with = "humantime_serde")]
    pub global_disk_check_interval: Duration,
    pub max_global_disk_usage_ratio: f64,

    /// Paths to the .pem public keys enabling JWT auth on each surface. Empty
    /// string / absent means auth disabled for that surface.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pg_auth_public_key_path: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pg_tenant_only_auth_public_key_path: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub http_auth_public_key_path: Option<String>,
    /// Path to the JWT token used to authenticate to peer safekeepers.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub auth_token_path: Option<String>,

    pub log_format: String,
    pub current_thread_runtime: bool,
    #[serde(with = "humantime_serde")]
    pub control_file_save_interval: Duration,
    pub wal_reader_fanout: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_delta_for_fanout: Option<u64>,

    pub ssl_key_file: String,
    pub ssl_cert_file: String,
    #[serde(with = "humantime_serde")]
    pub ssl_cert_reload_period: Duration,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ssl_ca_file: Option<String>,
    pub use_https_safekeeper_api: bool,
    pub enable_tls_wal_service_api: bool,

    pub force_metric_collection_on_scrape: bool,
    pub dev: bool,
}

impl Default for SafekeeperConfigToml {
    fn default() -> Self {
        use defaults::*;
        Self {
            id: NodeId(0),
            tenant_id: None,
            timeline_id: None,
            listen_pg: DEFAULT_PG_LISTEN_ADDR.to_string(),
            listen_pg_tenant_only: None,
            listen_http: DEFAULT_HTTP_LISTEN_ADDR.to_string(),
            listen_https: None,
            advertise_pg: None,
            availability_zone: None,
            heartbeat_timeout: Duration::from_millis(5000),
            yb_storage_conn: None,
            wal_cache_size_mib: None,
            scale_factor: DEFAULT_SCALE_FACTOR,
            max_timeline_disk_usage_bytes: DEFAULT_MAX_TIMELINE_DISK_USAGE_BYTES,
            global_disk_check_interval: Duration::from_secs(60),
            max_global_disk_usage_ratio: DEFAULT_MAX_GLOBAL_DISK_USAGE_RATIO,
            pg_auth_public_key_path: None,
            pg_tenant_only_auth_public_key_path: None,
            http_auth_public_key_path: None,
            auth_token_path: None,
            log_format: DEFAULT_LOG_FORMAT.to_string(),
            current_thread_runtime: false,
            control_file_save_interval: Duration::from_secs(300),
            wal_reader_fanout: false,
            max_delta_for_fanout: None,
            ssl_key_file: DEFAULT_SSL_KEY_FILE.to_string(),
            ssl_cert_file: DEFAULT_SSL_CERT_FILE.to_string(),
            ssl_cert_reload_period: Duration::from_secs(60),
            ssl_ca_file: None,
            use_https_safekeeper_api: false,
            enable_tls_wal_service_api: false,
            force_metric_collection_on_scrape: true,
            dev: false,
        }
    }
}

impl SafekeeperConfigToml {
    /// Parse a `safekeeper.toml` document.
    pub fn from_toml_str(s: &str) -> anyhow::Result<Self> {
        Ok(toml::from_str(s)?)
    }

    /// Serialize to a `safekeeper.toml` document.
    pub fn to_toml_string(&self) -> anyhow::Result<String> {
        Ok(toml::to_string_pretty(self)?)
    }
}

#[cfg(test)]
mod tests {
    use utils::id::{TenantId, TimelineId};

    use super::*;

    #[test]
    fn round_trips() {
        let cfg = SafekeeperConfigToml {
            id: NodeId(7),
            tenant_id: Some(TenantId::generate()),
            timeline_id: Some(TimelineId::generate()),
            listen_pg: "127.0.0.1:5474".to_string(),
            listen_http: "127.0.0.1:7686".to_string(),
            yb_storage_conn: Some("{ host_ports = '127.0.0.1:5433', dbname = 'yb_storage' }".to_string()),
            wal_cache_size_mib: Some(128),
            availability_zone: Some("sk-7".to_string()),
            ..Default::default()
        };
        let s = cfg.to_toml_string().unwrap();
        let back = SafekeeperConfigToml::from_toml_str(&s).unwrap();
        assert_eq!(cfg, back);
    }

    #[test]
    fn partial_toml_fills_defaults() {
        // A minimal file: missing keys take their defaults.
        let cfg = SafekeeperConfigToml::from_toml_str(
            "id = 3\nlisten_pg = '127.0.0.1:5474'\n",
        )
        .unwrap();
        assert_eq!(cfg.id, NodeId(3));
        assert_eq!(cfg.listen_pg, "127.0.0.1:5474");
        assert_eq!(cfg.heartbeat_timeout, Duration::from_millis(5000));
        assert_eq!(cfg.control_file_save_interval, Duration::from_secs(300));
        assert!(cfg.yb_storage_conn.is_none());
    }

    #[test]
    fn unknown_key_is_rejected() {
        // An unknown/misspelled key fails the safekeeper at startup rather
        // than being silently ignored (deny_unknown_fields).
        let err = SafekeeperConfigToml::from_toml_str(
            "id = 3\nlisten_pg = '127.0.0.1:5474'\nfuture_key = 'oops'\n",
        )
        .unwrap_err();
        assert!(
            err.to_string().contains("future_key") || err.to_string().contains("unknown"),
            "expected an unknown-field error, got: {err}"
        );
    }
}
