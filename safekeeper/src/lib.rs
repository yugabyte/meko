#![deny(clippy::undocumented_unsafe_blocks)]

extern crate hyper0 as hyper;

use std::time::Duration;

use camino::Utf8PathBuf;
use once_cell::sync::Lazy;
use pem::Pem;
use tokio::runtime::Runtime;
use utils::auth::SwappableJwtAuth;
use utils::id::NodeId;
use utils::logging::SecretString;

mod auth;
pub mod control_file;
pub mod debug_dump;
pub mod disk_usage;
pub mod handler;
pub mod http;
pub mod metrics;
pub mod patch_control_file;
pub mod receive_wal;
pub mod remove_wal;
pub mod safekeeper;
pub mod send_interpreted_wal;
pub mod send_wal;
pub mod state;
pub mod timeline;
pub mod timeline_guard;
pub mod timeline_manager;
pub mod timelines_set;
pub mod wal_cache;
pub mod wal_reader_stream;
pub mod wal_service;
pub mod wal_storage;
pub mod yb_wal_storage;

#[cfg(any(test, feature = "benchmarking"))]
pub mod test_utils;

mod timelines_global_map;

use std::sync::Arc;

pub use timelines_global_map::GlobalTimelines;
use utils::auth::JwtAuth;

/// Default `--scale-factor` for the safekeeper binary. 1.0 preserves
/// historical sizing; 0.25 fits a small dev box; values >1 scale up.
pub const DEFAULT_SCALE_FACTOR: f64 = 1.0;

/// `scale_count` analogue for safekeeper. Multiplies `base` by
/// `scale_factor`, rounds, floors at 1.
pub fn scale_count(base: usize, scale_factor: f64) -> usize {
    let scaled = (base as f64 * scale_factor).round();
    if scaled < 1.0 { 1 } else { scaled as usize }
}

pub mod defaults {
    pub use safekeeper_api::{
        DEFAULT_HTTP_LISTEN_ADDR, DEFAULT_HTTP_LISTEN_PORT, DEFAULT_PG_LISTEN_ADDR,
        DEFAULT_PG_LISTEN_PORT,
    };

    pub const DEFAULT_HEARTBEAT_TIMEOUT: &str = "5000ms";

    /// Capacity (in MiB) of the in-memory cache of recently-written WAL
    /// chunks in YB-WAL mode. See [`crate::wal_cache`]. Set to 0 to
    /// disable the cache entirely (every WAL read hits YB).
    pub const DEFAULT_WAL_CACHE_SIZE_MIB: usize = 256;

    /// Default disk usage limit is 0 (disabled) — yb-light has no
    /// local-FS WAL anyway. Retained as a CLI knob because the
    /// global-disk watcher uses it as the per-timeline cap when set.
    pub const DEFAULT_MAX_TIMELINE_DISK_USAGE_BYTES: u64 = 0;

    pub const DEFAULT_CONTROL_FILE_SAVE_INTERVAL: &str = "300s";

    pub const DEFAULT_SSL_KEY_FILE: &str = "server.key";
    pub const DEFAULT_SSL_CERT_FILE: &str = "server.crt";
    pub const DEFAULT_SSL_CERT_RELOAD_PERIOD: &str = "60s";

    // Global disk watcher defaults
    pub const DEFAULT_GLOBAL_DISK_CHECK_INTERVAL: &str = "60s";
    pub const DEFAULT_MAX_GLOBAL_DISK_USAGE_RATIO: f64 = 0.0;
}

#[derive(Debug, Clone)]
pub struct SafeKeeperConf {
    // Repository directory, relative to current working directory.
    // Normally, the safekeeper changes the current working directory
    // to the repository, and 'workdir' is always '.'. But we don't do
    // that during unit testing, because the current directory is global
    // to the process but different unit tests work on different
    // data directories to avoid clashing with each other.
    pub workdir: Utf8PathBuf,
    pub my_id: NodeId,
    pub listen_pg_addr: String,
    pub listen_pg_addr_tenant_only: Option<String>,
    pub listen_http_addr: String,
    pub listen_https_addr: Option<String>,
    pub advertise_pg_addr: Option<String>,
    pub availability_zone: Option<String>,
    pub heartbeat_timeout: Duration,
    /// Optional YB connection for mirroring live WAL writes. When `Some`,
    /// every `write_wal` is INSERT'd into the (hardcoded) `public.wals`
    /// table in addition to being written to the local-FS shadow. When
    /// `None`, WAL only lives on local FS (default).
    pub wal_yb_config: Option<ysql_client::YbConnConfig>,
    /// In-memory cache capacity (MiB) for recent WAL chunks in YB-WAL
    /// mode. See [`crate::wal_cache`]. Ignored when
    /// [`Self::wal_yb_config`] is `None`. Set to 0 to disable the cache.
    pub wal_cache_size_mib: usize,
    pub max_timeline_disk_usage_bytes: u64,
    /// How often to check the working directory's filesystem for total disk usage.
    pub global_disk_check_interval: Duration,
    /// The portion of the filesystem capacity that can be used by all timelines.
    pub max_global_disk_usage_ratio: f64,
    pub pg_auth: Option<Arc<JwtAuth>>,
    pub pg_tenant_only_auth: Option<Arc<JwtAuth>>,
    pub http_auth: Option<Arc<SwappableJwtAuth>>,
    /// JWT token to connect to other safekeepers with.
    pub sk_auth_token: Option<SecretString>,
    pub current_thread_runtime: bool,
    pub control_file_save_interval: Duration,
    pub wal_reader_fanout: bool,
    pub max_delta_for_fanout: Option<u64>,
    pub ssl_key_file: Utf8PathBuf,
    pub ssl_cert_file: Utf8PathBuf,
    pub ssl_cert_reload_period: Duration,
    pub ssl_ca_certs: Vec<Pem>,
    pub use_https_safekeeper_api: bool,
    pub enable_tls_wal_service_api: bool,
    pub force_metric_collection_on_scrape: bool,
}

impl SafeKeeperConf {
    /// Are we in YB-only WAL mode? When true, every byte of WAL is mirrored
    /// to a YugabyteDB table on each `write_wal()` and the local FS is left
    /// empty of segment files. In this mode the *only* durable copy of WAL
    /// is in YB — which is itself a quorum-replicated store — so running
    /// multiple safekeepers + Paxos on top of YB would be double consensus.
    /// The system therefore requires exactly one safekeeper in this mode;
    /// see [`require_single_safekeeper`] and call sites that enforce this.
    pub fn is_yb_wal_mode(&self) -> bool {
        self.wal_yb_config.is_some()
    }
}

/// Validate that the configured safekeeper membership has at most one
/// active member when running in YB WAL mode. YB itself provides the
/// durability + replication guarantees the safekeeper's Raft acceptor
/// would otherwise provide; layering Paxos on top is redundant and
/// (worse) means two consensus protocols racing on different LSN views.
///
/// Called at boot (against the loaded control-file membership) and at
/// runtime when a walproposer requests a membership switch.
pub fn require_single_safekeeper(
    mconf: &safekeeper_api::membership::Configuration,
) -> anyhow::Result<()> {
    use anyhow::bail;
    let n_now = mconf.members.m.len();
    let n_new = mconf.new_members.as_ref().map(|s| s.m.len()).unwrap_or(0);
    if n_now > 1 {
        bail!(
            "YB WAL mode requires exactly one safekeeper, but membership has \
             {n_now} members: {}. YB already provides quorum replication; \
             running multiple safekeepers on top would be double consensus.",
            mconf.members
        );
    }
    if n_new > 1 {
        bail!(
            "YB WAL mode requires exactly one safekeeper, but a joint \
             configuration is requesting {n_new} new members: {}. \
             Refusing to switch.",
            mconf.new_members.as_ref().unwrap()
        );
    }
    Ok(())
}

#[cfg(test)]
mod yb_mode_tests {
    use super::require_single_safekeeper;
    use safekeeper_api::membership::{
        Configuration, MemberSet, SafekeeperGeneration, SafekeeperId,
    };
    use utils::id::NodeId;

    fn member(id: u64) -> SafekeeperId {
        SafekeeperId {
            id: NodeId(id),
            host: "h".into(),
            pg_port: 5474,
        }
    }

    fn conf(members: Vec<SafekeeperId>, new_members: Option<Vec<SafekeeperId>>) -> Configuration {
        let mut ms = MemberSet::empty();
        for m in members {
            ms.add(m).unwrap();
        }
        let nm = new_members.map(|v| {
            let mut s = MemberSet::empty();
            for m in v {
                s.add(m).unwrap();
            }
            s
        });
        Configuration {
            generation: SafekeeperGeneration::new(1),
            members: ms,
            new_members: nm,
        }
    }

    #[test]
    fn empty_config_is_accepted() {
        // pre-generations timelines have empty MemberSet; YB mode shouldn't
        // misfire on that initial state.
        require_single_safekeeper(&conf(vec![], None)).unwrap();
    }

    #[test]
    fn single_member_is_accepted() {
        require_single_safekeeper(&conf(vec![member(1)], None)).unwrap();
    }

    #[test]
    fn two_members_is_rejected() {
        let err = require_single_safekeeper(&conf(vec![member(1), member(2)], None)).unwrap_err();
        let msg = format!("{err}");
        assert!(
            msg.contains("YB WAL mode requires exactly one safekeeper"),
            "unexpected error message: {msg}"
        );
        assert!(msg.contains("2 members"), "unexpected error message: {msg}");
    }

    #[test]
    fn joint_with_two_new_members_is_rejected() {
        let err =
            require_single_safekeeper(&conf(vec![member(1)], Some(vec![member(2), member(3)])))
                .unwrap_err();
        assert!(format!("{err}").contains("2 new members"));
    }
}

impl SafeKeeperConf {
    pub fn dummy() -> Self {
        SafeKeeperConf {
            workdir: Utf8PathBuf::from("./"),
            listen_pg_addr: defaults::DEFAULT_PG_LISTEN_ADDR.to_string(),
            listen_pg_addr_tenant_only: None,
            listen_http_addr: defaults::DEFAULT_HTTP_LISTEN_ADDR.to_string(),
            listen_https_addr: None,
            advertise_pg_addr: None,
            availability_zone: None,
            wal_yb_config: None,
            wal_cache_size_mib: defaults::DEFAULT_WAL_CACHE_SIZE_MIB,
            my_id: NodeId(0),
            pg_auth: None,
            pg_tenant_only_auth: None,
            http_auth: None,
            sk_auth_token: None,
            heartbeat_timeout: Duration::new(5, 0),
            max_timeline_disk_usage_bytes: defaults::DEFAULT_MAX_TIMELINE_DISK_USAGE_BYTES,
            global_disk_check_interval: Duration::from_secs(60),
            max_global_disk_usage_ratio: defaults::DEFAULT_MAX_GLOBAL_DISK_USAGE_RATIO,
            current_thread_runtime: false,
            control_file_save_interval: Duration::from_secs(1),
            wal_reader_fanout: false,
            max_delta_for_fanout: None,
            ssl_key_file: Utf8PathBuf::from(defaults::DEFAULT_SSL_KEY_FILE),
            ssl_cert_file: Utf8PathBuf::from(defaults::DEFAULT_SSL_CERT_FILE),
            ssl_cert_reload_period: Duration::from_secs(60),
            ssl_ca_certs: Vec::new(),
            use_https_safekeeper_api: false,
            enable_tls_wal_service_api: false,
            force_metric_collection_on_scrape: true,
        }
    }
}

// Tokio runtimes.
pub static WAL_SERVICE_RUNTIME: Lazy<Runtime> = Lazy::new(|| {
    tokio::runtime::Builder::new_multi_thread()
        .thread_name("WAL service worker")
        .enable_all()
        .build()
        .expect("Failed to create WAL service runtime")
});

pub static HTTP_RUNTIME: Lazy<Runtime> = Lazy::new(|| {
    tokio::runtime::Builder::new_multi_thread()
        .thread_name("HTTP worker")
        .enable_all()
        .build()
        .expect("Failed to create HTTP runtime")
});

/// Dedicated runtime for infrequent background tasks.
pub static BACKGROUND_RUNTIME: Lazy<Runtime> = Lazy::new(|| {
    tokio::runtime::Builder::new_multi_thread()
        .thread_name("SK background worker")
        // One worker thread is enough, as most of the actual tasks run on blocking threads
        // which has it own thread pool.
        .worker_threads(1)
        .enable_all()
        .build()
        .expect("Failed to create background runtime")
});
