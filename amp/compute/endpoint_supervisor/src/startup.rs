//! How long each phase of bringing the endpoint up took, served on the settings
//! API's `/metrics`.
//!
//! `yb_wait` and `render` run once per process; the lease, service and handshake
//! phases and [`STARTUP_SECONDS`] once per bring-up, so again after a lost lease is
//! regained. Only phases that succeeded are observed — a `yb_wait` that timed out
//! is not — so the phases need not add up to the total.

use std::time::{Duration, Instant};

use metrics::{Histogram, HistogramVec, register_histogram, register_histogram_vec};
use once_cell::sync::Lazy;

/// The supervisor's share of a bring-up, in milliseconds, handed to the compute
/// on its first spawn of the lease so `total_startup_ms` covers the whole boot.
pub const BOOT_MS_ENV: &str = "AMP_SUPERVISOR_BOOT_MS";

const BUCKETS: &[f64] = &[
    0.05, 0.1, 0.25, 0.5, 1.0, 2.0, 5.0, 10.0, 30.0, 60.0, 120.0, 300.0,
];

/// Phases: `yb_wait`, `render`, `lease`, one per service started (`safekeeper`,
/// `pageserver`, `lfc_state`, `compute_ctl` — spawn to healthy), and `handshake`.
pub static PHASE_SECONDS: Lazy<HistogramVec> = Lazy::new(|| {
    register_histogram_vec!(
        "endpoint_supervisor_startup_phase_seconds",
        "Seconds one phase of an endpoint bring-up took",
        &["phase"],
        BUCKETS.to_vec(),
    )
    .expect("register endpoint_supervisor_startup_phase_seconds")
});

/// From the start of a bring-up — process start, or the wait for the next lease
/// after one was lost — to the compute accepting connections.
pub static STARTUP_SECONDS: Lazy<Histogram> = Lazy::new(|| {
    register_histogram!(
        "endpoint_supervisor_startup_seconds",
        "Seconds from the start of an endpoint bring-up to the compute accepting connections",
        BUCKETS.to_vec(),
    )
    .expect("register endpoint_supervisor_startup_seconds")
});

pub fn phase(name: &str, took: Duration) {
    PHASE_SECONDS
        .with_label_values(&[name])
        .observe(took.as_secs_f64());
}

/// Runs `f` and observes it as `name` if it succeeds. A failure is not a phase
/// that took that long: it is a timeout or an error, and it ends the bring-up.
pub fn timed<T, E>(name: &str, f: impl FnOnce() -> Result<T, E>) -> Result<T, E> {
    let start = Instant::now();
    let out = f()?;
    phase(name, start.elapsed());
    Ok(out)
}
