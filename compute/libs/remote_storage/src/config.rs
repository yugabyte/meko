use std::str::FromStr;
use std::time::Duration;

use serde::{Deserialize, Serialize};

use crate::DEFAULT_REMOTE_STORAGE_CONCURRENCY_LIMIT;

/// External backup storage configuration, enough for creating a client for that storage.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
pub struct RemoteStorageConfig {
    /// The storage connection configuration.
    #[serde(flatten)]
    pub storage: RemoteStorageKind,
    /// A common timeout enforced for all requests after concurrency limiter permit has been
    /// acquired.
    // Always serialized (no skip_serializing_if): the pageserver treats any
    // on-disk config key absent from the re-serialized config as an unknown
    // item and refuses to start. A value-dependent skip here would drop the
    // key on round-trip when it equals the default, falsely flagging a valid
    // `timeout` as unknown.
    #[serde(with = "humantime_serde", default = "default_timeout")]
    pub timeout: Duration,
    /// Alternative timeout used for metadata objects which are expected to be small.
    /// Retained for backwards-compat with existing config files; the
    /// surviving backend (YbStore) doesn't currently use it.
    #[serde(with = "humantime_serde", default = "default_small_timeout")]
    pub small_timeout: Duration,
}

impl RemoteStorageKind {
    pub fn bucket_name(&self) -> Option<&str> {
        match self {
            // The blob store's table is hardcoded (see yb_store::BLOBS_TABLE);
            // the connection config carries no table name.
            RemoteStorageKind::YbStore(_) => Some("blobs"),
        }
    }
}

impl RemoteStorageConfig {
    /// Helper to fetch the configured concurrency limit.
    pub fn concurrency_limit(&self) -> usize {
        match &self.storage {
            RemoteStorageKind::YbStore(_) => DEFAULT_REMOTE_STORAGE_CONCURRENCY_LIMIT,
        }
    }
}

fn default_timeout() -> Duration {
    RemoteStorageConfig::DEFAULT_TIMEOUT
}

fn default_small_timeout() -> Duration {
    RemoteStorageConfig::DEFAULT_SMALL_TIMEOUT
}

/// A kind of a remote storage to connect to, with its connection
/// configuration. Cloud variants (S3/Azure/GCS) were removed in lane E
/// and the LocalFs variant was dropped along with the dev fallback —
/// yb-light writes only to YbStore.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
#[serde(untagged)]
pub enum RemoteStorageKind {
    /// YugabyteDB-based storage, storing all files as rows in a YB
    /// table over a Postgres-wire-compatible SQL connection. The config
    /// is the shared [`ysql_client::YbConnConfig`] (connection only); the
    /// table is hardcoded (see `yb_store::BLOBS_TABLE`).
    YbStore(ysql_client::YbConnConfig),
}

/// Version of `RemoteStorageKind` that deserializes via an external
/// `type:` tag. Used by services (endpoint_storage) that load their
/// storage backend from a JSON config file at startup.
#[derive(Deserialize)]
#[serde(tag = "type")]
pub enum TypedRemoteStorageKind {
    YbStore(ysql_client::YbConnConfig),
}

impl From<TypedRemoteStorageKind> for RemoteStorageKind {
    fn from(value: TypedRemoteStorageKind) -> Self {
        match value {
            TypedRemoteStorageKind::YbStore(v) => RemoteStorageKind::YbStore(v),
        }
    }
}

impl RemoteStorageConfig {
    pub const DEFAULT_TIMEOUT: Duration = std::time::Duration::from_secs(120);
    pub const DEFAULT_SMALL_TIMEOUT: Duration = std::time::Duration::from_secs(30);

    pub fn from_toml(toml: &toml_edit::Item) -> anyhow::Result<RemoteStorageConfig> {
        Ok(utils::toml_edit_ext::deserialize_item(toml)?)
    }

    pub fn from_toml_str(input: &str) -> anyhow::Result<RemoteStorageConfig> {
        let toml_document = toml_edit::DocumentMut::from_str(input)?;
        if let Some(item) = toml_document.get("remote_storage") {
            return Self::from_toml(item);
        }
        Self::from_toml(toml_document.as_item())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(input: &str) -> anyhow::Result<RemoteStorageConfig> {
        RemoteStorageConfig::from_toml_str(input)
    }

    #[test]
    fn test_yb_store_parsing() {
        let toml = "\
    host_ports = '10.0.0.1:5433,10.0.0.2:5440'
    user = 'yugabyte'
    password_env = 'YB_STORAGE_PASSWORD'
    dbname = 'neon_storage'
    sslmode = 'require'
    ";

        let config = parse(toml).unwrap();
        let RemoteStorageKind::YbStore(yb) = &config.storage;
        assert_eq!(yb.host_ports.to_string(), "10.0.0.1:5433,10.0.0.2:5440");
        assert_eq!(yb.user, "yugabyte");
        assert_eq!(yb.dbname, "neon_storage");
        assert_eq!(yb.password_env.as_deref(), Some("YB_STORAGE_PASSWORD"));
        assert_eq!(yb.sslmode.as_deref(), Some("require"));
        assert_eq!(config.timeout, RemoteStorageConfig::DEFAULT_TIMEOUT);
    }

    #[test]
    fn test_yb_store_defaults() {
        // user/dbname default; sslmode unset → NoTls; single host gets :5433.
        let config = parse("host_ports = 'localhost'").unwrap();
        let RemoteStorageKind::YbStore(yb) = &config.storage;
        assert_eq!(yb.host_ports.to_string(), "localhost:5433");
        assert_eq!(yb.user, "yugabyte");
        assert_eq!(yb.dbname, "yb_storage");
        assert!(yb.sslmode.is_none());
    }

    #[test]
    fn test_yb_store_timeout_not_rejected_by_conn_deny() {
        // `timeout` belongs to RemoteStorageConfig, not YbConnConfig. With
        // YbConnConfig#[deny_unknown_fields], the flatten must consume the
        // RemoteStorageConfig fields before YbConnConfig sees the residual —
        // otherwise a valid config that sets a timeout would be rejected.
        let config = parse("host_ports = 'localhost'\ntimeout = '42s'").unwrap();
        let RemoteStorageKind::YbStore(yb) = &config.storage;
        assert_eq!(yb.host_ports.to_string(), "localhost:5433");
        assert_eq!(config.timeout, Duration::from_secs(42));
    }

    #[test]
    fn test_yb_store_rejects_unknown_field() {
        // A misspelled/stale connection key fails to parse rather than being
        // silently ignored (deny_unknown_fields on YbConnConfig).
        let err = parse("host_ports = 'localhost'\npasswordenv = 'X'").unwrap_err();
        assert!(
            err.to_string().contains("passwordenv")
                || err.to_string().contains("unknown")
                || err.to_string().contains("did not match"),
            "expected an unknown-field error, got: {err}"
        );
    }
}
