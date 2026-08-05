//! `yb-endpoint-supervisor` — PID 1 of the endpoint pod.
//!
//! An endpoint pod runs four processes: `endpoint_storage`, the safekeeper, the
//! pageserver, and `compute_ctl` with Postgres. This binary renders their config
//! and supervises them.
//!
//! # Modules
//!
//! - [`resources`] — what the pod has: the container's cgroup CPU and memory
//!   limits, and the size of its data volume. Every derived setting is sized from
//!   these, and an input that cannot be determined stays `None` so the caller
//!   leaves the setting alone rather than guessing.
//! - [`pg_settings`] — the Postgres GUCs derived from those resources.
//!   `shared_buffers` takes half the memory; the LFC is bounded by the data
//!   volume. Every other knob is a postmaster-context ceiling plus a live value
//!   beneath it, so the live value can be retuned with a reload.
//! - [`config`] — rendering `pageserver.toml` and `safekeeper.toml`, with
//!   operator overrides merged in. Keys are merged by name, so a key cannot be
//!   emitted twice, and each document is type-checked against the struct its
//!   daemon parses.

use anyhow::Result;
use camino::Utf8PathBuf;

pub mod config;
pub mod pg_settings;
pub mod resources;

use resources::{Cgroup, Resources};

fn env_opt(name: &str) -> Option<String> {
    std::env::var(name).ok().filter(|v| !v.is_empty())
}

fn main() -> Result<()> {
    init_logging();

    let run_dir: Utf8PathBuf = env_opt("RUN_DIR").unwrap_or_else(|| "/data".into()).into();
    let cgroup_root: Utf8PathBuf = env_opt("CGROUP_ROOT")
        .unwrap_or_else(|| "/sys/fs/cgroup".into())
        .into();

    let res = Resources::resolve(
        &Cgroup::new(cgroup_root),
        env_opt("ENDPOINT_CPU").as_deref(),
        env_opt("ENDPOINT_MEMORY").as_deref(),
        env_opt("ENDPOINT_DATA_VOLUME_SIZE").as_deref(),
        resources::statvfs_total_bytes(&run_dir),
    );

    tracing::info!(
        memory_bytes = res.memory_bytes,
        vcpu = res.vcpu,
        data_volume_bytes = res.data_volume_bytes,
        "resolved endpoint resources"
    );

    let (settings, skipped) = pg_settings::derive(&res);
    for s in &settings {
        tracing::info!(name = %s.name, value = %s.value, "derived");
    }
    if skipped.memory_unknown {
        tracing::warn!("endpoint memory unknown; shared_buffers left at the Postgres default");
    }
    if skipped.cpu_unknown {
        tracing::warn!("endpoint CPU unknown; parallel-worker GUCs left at their defaults");
    }
    if skipped.volume_unknown {
        tracing::warn!("data volume budget unknown; the LFC stays disabled");
    }

    Ok(())
}

/// Registers a stdout subscriber, without which every `tracing::` call in this
/// binary is silently dropped — which is the whole output of this slice.
///
/// `RUST_LOG` is honoured so a pod can be turned up to debug without a rebuild;
/// the default is `info`, matching what the other daemons log at. When this takes
/// over as PID 1 it should adopt `utils::logging` instead, so its lines are
/// formatted the same as the children whose stdout it shares.
fn init_logging() {
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_writer(std::io::stdout)
        .init();
}
