//! The lane's Prometheus series. They register in the process-global default
//! registry, so the pageserver and the safekeeper both export them, next to
//! `ysql_client`'s `yb_client_*` series for the YSQL lane.

use metrics::{HistogramVec, IntCounterVec, register_histogram_vec, register_int_counter_vec};
use once_cell::sync::Lazy;
use strum::IntoEnumIterator;

use crate::error::YbError;

/// Failed `Perform` attempts, retried or not. `op` is `read` or `upsert`,
/// `code` the [`YbError::code`] of the failure.
pub(crate) static ERRORS: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "yb_thin_client_errors_total",
        "Failed thin-client Perform attempts, by op and error code",
        &["op", "code"],
    )
    .expect("register yb_thin_client_errors_total")
});

/// Calls that failed every attempt of their retry budget with a retryable
/// error. Same labels as [`ERRORS`].
pub(crate) static RETRIES_EXHAUSTED: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "yb_thin_client_retries_exhausted_total",
        "Thin-client calls that used their whole retry budget, by op and error code",
        &["op", "code"],
    )
    .expect("register yb_thin_client_retries_exhausted_total")
});

/// Latency of one `Perform` attempt, by `op` and [`AttemptOutcome`].
pub(crate) static ATTEMPT_SECONDS: Lazy<HistogramVec> = Lazy::new(|| {
    register_histogram_vec!(
        "yb_thin_client_attempt_seconds",
        "Thin-client Perform latency per attempt (success or failure), seconds",
        &["op", "outcome"],
        metrics::exponential_buckets(0.0005, 2.0, 16)
            .expect("yb_thin_client attempt histogram bucket spec"),
    )
    .expect("register yb_thin_client_attempt_seconds")
});

/// The `op` label of [`ERRORS`], [`RETRIES_EXHAUSTED`] and [`ATTEMPT_SECONDS`].
#[derive(Clone, Copy, Debug, strum_macros::EnumIter, strum_macros::IntoStaticStr)]
#[strum(serialize_all = "snake_case")]
pub(crate) enum Op {
    Read,
    Upsert,
}

/// The `outcome` label of [`ATTEMPT_SECONDS`].
#[derive(Clone, Copy, Debug, strum_macros::EnumIter, strum_macros::IntoStaticStr)]
#[strum(serialize_all = "snake_case")]
pub(crate) enum AttemptOutcome {
    Ok,
    Error,
}

/// Generation rebuilds by [`crate::Recycling`], by [`RecycleTrigger`] and
/// [`RecycleOutcome`].
pub(crate) static CONNECTION_RECYCLES: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "yb_thin_client_connection_recycles_total",
        "Thin-client generations rebuilt, by trigger and outcome",
        &["trigger", "outcome"],
    )
    .expect("register yb_thin_client_connection_recycles_total")
});

/// The `trigger` label of [`CONNECTION_RECYCLES`].
#[derive(Clone, Copy, Debug, strum_macros::EnumIter, strum_macros::IntoStaticStr)]
#[strum(serialize_all = "snake_case")]
pub(crate) enum RecycleTrigger {
    /// The generation reached its configured lifetime.
    Lifetime,
    /// The TLS files changed.
    TlsReload,
}

/// The `outcome` label of [`CONNECTION_RECYCLES`].
#[derive(Clone, Copy, Debug, strum_macros::EnumIter, strum_macros::IntoStaticStr)]
#[strum(serialize_all = "snake_case")]
pub(crate) enum RecycleOutcome {
    Ok,
    /// The rebuild failed and the old generation kept serving.
    Failed,
}

/// Creates every series at zero, so a first failure shows as an increase
/// rather than as a new series that `rate()` cannot see.
pub fn init() {
    zero_series(
        &ERRORS,
        &RETRIES_EXHAUSTED,
        &ATTEMPT_SECONDS,
        &CONNECTION_RECYCLES,
    );
}

// Takes the vecs so a test can check them apart from the shared registry.
fn zero_series(
    errors: &IntCounterVec,
    exhausted: &IntCounterVec,
    attempts: &HistogramVec,
    recycles: &IntCounterVec,
) {
    for op in Op::iter() {
        let op: &str = op.into();
        for e in YbError::iter() {
            errors.with_label_values(&[op, e.code()]);
            if e.is_retryable() {
                exhausted.with_label_values(&[op, e.code()]);
            }
        }
        for outcome in AttemptOutcome::iter() {
            attempts.with_label_values(&[op, outcome.into()]);
        }
    }
    for trigger in RecycleTrigger::iter() {
        for outcome in RecycleOutcome::iter() {
            recycles.with_label_values(&[trigger.into(), outcome.into()]);
        }
    }
}

#[cfg(test)]
mod tests {
    use metrics::core::Collector;
    use metrics::prometheus::{HistogramOpts, Opts};
    use metrics::{HistogramVec, IntCounterVec};
    use strum::IntoEnumIterator;

    use super::zero_series;
    use crate::error::YbError;

    /// The `label` values of the series in `vec` whose `key` label is `value`.
    fn values(vec: &dyn Collector, (key, value): (&str, &str), label: &str) -> Vec<String> {
        let mut out = Vec::new();
        for family in vec.collect() {
            for metric in family.get_metric() {
                let get = |name: &str| {
                    metric
                        .get_label()
                        .iter()
                        .find(|pair| pair.get_name() == name)
                        .map(|pair| pair.get_value().to_string())
                };
                if get(key).as_deref() == Some(value) {
                    out.extend(get(label));
                }
            }
        }
        out.sort();
        out
    }

    /// Every series starts at zero, and the exhausted budget only for the
    /// classes that are retried. The labels are pinned since dashboards query
    /// them and the enums derive them from the variant names.
    #[test]
    fn every_series_starts_at_zero() {
        let counters = |labels: &[&str]| IntCounterVec::new(Opts::new("c", "c"), labels).unwrap();
        let errors = counters(&["op", "code"]);
        let exhausted = counters(&["op", "code"]);
        let recycles = counters(&["trigger", "outcome"]);
        let attempts = HistogramVec::new(HistogramOpts::new("h", "h"), &["op", "outcome"]).unwrap();
        zero_series(&errors, &exhausted, &attempts, &recycles);

        let mut all: Vec<&str> = YbError::iter().map(|e| e.code()).collect();
        all.sort();
        assert_eq!(
            all,
            [
                "fenced",
                "invalid",
                "network",
                "other",
                "read_restart",
                "schema",
                "try_again"
            ]
        );
        for op in ["read", "upsert"] {
            assert_eq!(values(&errors, ("op", op), "code"), all);
            assert_eq!(
                values(&exhausted, ("op", op), "code"),
                ["network", "read_restart", "try_again"]
            );
            assert_eq!(values(&attempts, ("op", op), "outcome"), ["error", "ok"]);
        }
        for trigger in ["lifetime", "tls_reload"] {
            assert_eq!(
                values(&recycles, ("trigger", trigger), "outcome"),
                ["failed", "ok"]
            );
        }
    }
}
