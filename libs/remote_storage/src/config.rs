use std::fmt::Debug;
use std::num::NonZeroUsize;
use std::str::FromStr;
use std::time::Duration;

use camino::Utf8PathBuf;
use serde::{Deserialize, Serialize};

use crate::DEFAULT_REMOTE_STORAGE_LOCALFS_CONCURRENCY_LIMIT;

/// External backup storage configuration, enough for creating a client for that storage.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
pub struct RemoteStorageConfig {
    /// The storage connection configuration.
    #[serde(flatten)]
    pub storage: RemoteStorageKind,
    /// A common timeout enforced for all requests after concurrency limiter permit has been
    /// acquired.
    #[serde(
        with = "humantime_serde",
        default = "default_timeout",
        skip_serializing_if = "is_default_timeout"
    )]
    pub timeout: Duration,
    /// Alternative timeout used for metadata objects which are expected to be small.
    /// Retained for backwards-compat with existing config files; the
    /// surviving backends (LocalFs, YbStore) don't currently use it.
    #[serde(
        with = "humantime_serde",
        default = "default_small_timeout",
        skip_serializing_if = "is_default_small_timeout"
    )]
    pub small_timeout: Duration,
}

impl RemoteStorageKind {
    pub fn bucket_name(&self) -> Option<&str> {
        match self {
            RemoteStorageKind::LocalFs { .. } => None,
            RemoteStorageKind::YbStore(config) => Some(&config.table_name),
        }
    }
}

impl RemoteStorageConfig {
    /// Helper to fetch the configured concurrency limit.
    pub fn concurrency_limit(&self) -> usize {
        match &self.storage {
            RemoteStorageKind::LocalFs { .. } => DEFAULT_REMOTE_STORAGE_LOCALFS_CONCURRENCY_LIMIT,
            RemoteStorageKind::YbStore(c) => c.concurrency_limit.into(),
        }
    }
}

fn default_timeout() -> Duration {
    RemoteStorageConfig::DEFAULT_TIMEOUT
}

fn default_small_timeout() -> Duration {
    RemoteStorageConfig::DEFAULT_SMALL_TIMEOUT
}

fn is_default_timeout(d: &Duration) -> bool {
    *d == RemoteStorageConfig::DEFAULT_TIMEOUT
}

fn is_default_small_timeout(d: &Duration) -> bool {
    *d == RemoteStorageConfig::DEFAULT_SMALL_TIMEOUT
}

/// A kind of a remote storage to connect to, with its connection
/// configuration. Cloud variants (S3/Azure/GCS) were removed in lane
/// E — yb-light writes only to LocalFs (dev) or YbStore (production).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
#[serde(untagged)]
pub enum RemoteStorageKind {
    /// Storage based on local file system. Specify a root folder to
    /// place all stored files into.
    LocalFs { local_path: Utf8PathBuf },
    /// YugabyteDB-based storage, storing all files as rows in a YB
    /// table over a Postgres-wire-compatible SQL connection.
    YbStore(YbStoreConfig),
}

/// Version of `RemoteStorageKind` that deserializes via an external
/// `type:` tag. Used by services (endpoint_storage) that load their
/// storage backend from a JSON config file at startup.
#[derive(Deserialize)]
#[serde(tag = "type")]
pub enum TypedRemoteStorageKind {
    LocalFs { local_path: Utf8PathBuf },
    YbStore(YbStoreConfig),
}

impl From<TypedRemoteStorageKind> for RemoteStorageKind {
    fn from(value: TypedRemoteStorageKind) -> Self {
        match value {
            TypedRemoteStorageKind::LocalFs { local_path } => {
                RemoteStorageKind::LocalFs { local_path }
            }
            TypedRemoteStorageKind::YbStore(v) => RemoteStorageKind::YbStore(v),
        }
    }
}

/// YugabyteDB-backed storage configuration.
///
/// The pageserver connects to a YB cluster over the Postgres wire protocol and
/// stores objects as rows in a single user-managed table. The table itself must
/// already exist; the schema this code expects is documented on `YbStore`.
#[derive(Clone, PartialEq, Eq, Deserialize, Serialize)]
pub struct YbStoreConfig {
    /// Host (or comma-separated host list) for the YB cluster's YSQL endpoint.
    pub host: String,
    /// YSQL port (default 5433).
    #[serde(default = "default_yb_port")]
    pub port: u16,
    /// Database user.
    pub user: String,
    /// Name of an environment variable holding the password. If unset, no
    /// password is sent (suitable for trust-auth dev clusters).
    pub password_env: Option<String>,
    /// Database name to connect to. Defaults to `yb_storage` — the shared
    /// DB that neon_local provisions for pageserver/safekeeper/storage_controller
    /// to colocate `blobs`, `wals`, and the `storage_controller` schema.
    /// Override per-component if you need a different layout.
    #[serde(default = "default_yb_dbname")]
    pub dbname: String,
    /// Name of the table that holds the objects. Defaults to `blobs` —
    /// the table neon_local provisions in `yb_storage.public`.
    #[serde(default = "default_yb_table_name")]
    pub table_name: String,
    /// Optional "subfolder" prefix that's prepended to every key, allowing
    /// multiple tenants to share a single backing table.
    pub prefix: Option<String>,
    /// Maximum number of concurrent in-flight DB requests.
    #[serde(default = "default_yb_concurrency_limit")]
    pub concurrency_limit: NonZeroUsize,
    /// `"disable"` (default) → plain TCP, no TLS. Anything else (`"require"`,
    /// `"prefer"`, `"verify-ca"`, `"verify-full"`) → rustls. With
    /// `sslrootcert` unset the verifier is AcceptAll (suitable for YBA
    /// universes with self-signed CAs not in the system trust store).
    /// With `sslrootcert` set the cert chain is verified against the
    /// supplied CA bundle (sslmode = verify-ca semantics).
    #[serde(default)]
    pub sslmode: Option<String>,
    /// Optional path to a PEM-encoded CA bundle for sslmode = "verify-ca"
    /// / "verify-full". When `Some`, the server cert is verified against
    /// this CA; otherwise the AcceptAll verifier is used.
    #[serde(default)]
    pub sslrootcert: Option<String>,
}

fn default_yb_port() -> u16 {
    5433
}

fn default_yb_dbname() -> String {
    "yb_storage".to_string()
}

fn default_yb_table_name() -> String {
    "blobs".to_string()
}

fn default_yb_concurrency_limit() -> NonZeroUsize {
    NonZeroUsize::new(DEFAULT_REMOTE_STORAGE_LOCALFS_CONCURRENCY_LIMIT).unwrap()
}

impl Debug for YbStoreConfig {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("YbStoreConfig")
            .field("host", &self.host)
            .field("port", &self.port)
            .field("user", &self.user)
            .field("dbname", &self.dbname)
            .field("table_name", &self.table_name)
            .field("prefix", &self.prefix)
            .field("concurrency_limit", &self.concurrency_limit)
            .field("sslmode", &self.sslmode)
            .field("sslrootcert", &self.sslrootcert)
            .finish()
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
    fn parse_localfs_config_with_timeout() {
        let input = "local_path = '.'
timeout = '5s'";

        let config = parse(input).unwrap();

        assert_eq!(
            config,
            RemoteStorageConfig {
                storage: RemoteStorageKind::LocalFs {
                    local_path: Utf8PathBuf::from(".")
                },
                timeout: Duration::from_secs(5),
                small_timeout: RemoteStorageConfig::DEFAULT_SMALL_TIMEOUT
            }
        );
    }

    #[test]
    fn test_yb_store_parsing() {
        let toml = "\
    host = 'yb-cluster.example.com'
    port = 5433
    user = 'yugabyte'
    password_env = 'YB_STORAGE_PASSWORD'
    dbname = 'neon_storage'
    table_name = 'pageserver_objects'
    prefix = 'tenant-a/'
    ";

        let config = parse(toml).unwrap();

        assert_eq!(
            config,
            RemoteStorageConfig {
                storage: RemoteStorageKind::YbStore(YbStoreConfig {
                    host: "yb-cluster.example.com".into(),
                    port: 5433,
                    user: "yugabyte".into(),
                    password_env: Some("YB_STORAGE_PASSWORD".into()),
                    dbname: "neon_storage".into(),
                    table_name: "pageserver_objects".into(),
                    prefix: Some("tenant-a/".into()),
                    concurrency_limit: default_yb_concurrency_limit(),
                    sslmode: None,
                    sslrootcert: None,
                }),
                timeout: RemoteStorageConfig::DEFAULT_TIMEOUT,
                small_timeout: RemoteStorageConfig::DEFAULT_SMALL_TIMEOUT,
            }
        );
    }
}
