use metrics::{HistogramVec, IntCounterVec, register_histogram_vec, register_int_counter_vec};
use once_cell::sync::Lazy;

/// Counts YugabyteDB client errors observed by `YsqlClient`, after
/// classification. Labels:
///
/// - `op`: which YsqlClient method hit the error (`execute`, `query`,
///   `query_opt`, `prepare`).
/// - `class`: `connection_drop`, `transient`, or `fatal`. The retry
///   decision is determined by this — transients and connection drops
///   are retried internally up to a budget; fatals propagate.
/// - `sqlstate`: the PG SQLSTATE the server returned (e.g. `40001`,
///   `XX000`, `42P01`), or `none` if the error has no code (typical
///   for client-side connection failures).
///
/// Surfaced in both pageserver and safekeeper `/metrics` endpoints
/// because they both link `ysql_client` and the underlying
/// `prometheus::default_registry()` is process-global.
pub static YB_CLIENT_ERRORS: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "yb_client_errors_total",
        "YugabyteDB client errors observed by YsqlClient, classified by retry class and SQLSTATE",
        &["op", "class", "sqlstate"],
    )
    .expect("register yb_client_errors_total")
});

/// Counts cases where `YsqlClient` exhausted a retry budget and had to
/// surface the error to the caller. Label `reason`: `connection_drop`
/// (reconnect budget) or `transient` (transient-retry budget).
pub static YB_RETRIES_EXHAUSTED: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "yb_client_retries_exhausted_total",
        "YsqlClient retry budgets exhausted, by reason",
        &["reason"],
    )
    .expect("register yb_client_retries_exhausted_total")
});

/// Per-attempt latency of YsqlClient operations. Observed for every
/// completed call to `tokio_postgres::Client::{execute,query,query_opt}`,
/// regardless of success or failure — so the histogram reflects the
/// distribution of work YB actually performed, not just the successful
/// terminal attempts. The label set matches `yb_client_errors_total.op`:
///
/// - `op`: `execute`, `query`, `query_opt`, or `prepare`. `prepare` fires
///   only on cache miss for the SQL text.
///
/// Combined with `yb_client_errors_total` and `yb_client_retries_exhausted_total`,
/// this gives a complete view of YB-side wire latency from any caller
/// (pageserver `PageStore`, safekeeper WAL flush, etc.) — no need to
/// instrument callers individually.
pub static YB_QUERY_SECONDS: Lazy<HistogramVec> = Lazy::new(|| {
    register_histogram_vec!(
        "yb_client_query_seconds",
        "YsqlClient operation latency per attempt (success or failure), seconds",
        &["op"],
        metrics::exponential_buckets(0.0005, 2.0, 16)
            .expect("ysql_client query histogram bucket spec"),
    )
    .expect("register yb_client_query_seconds")
});

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn yb_query_seconds_observes_each_op_label() {
        // Both pageserver and safekeeper use these `op` strings — anchor
        // them so a typo in the call site can't silently drop a metric
        // dimension.
        for op in ["execute", "query", "query_opt", "prepare"] {
            let h = YB_QUERY_SECONDS.with_label_values(&[op]);
            let count_before = h.get_sample_count();
            let sum_before = h.get_sample_sum();
            h.observe(0.123);
            h.observe(0.456);
            assert_eq!(h.get_sample_count(), count_before + 2, "{op}: count");
            let delta = h.get_sample_sum() - sum_before;
            assert!((delta - 0.579).abs() < 1e-6, "{op}: sum delta {delta}");
        }
    }

    #[test]
    fn yb_query_seconds_buckets_span_submillisecond_to_seconds() {
        // The histogram must keep its bucket spec wide enough to resolve
        // both cached lookups (sub-ms) and tablet-leader-change retries
        // (multi-second). exponential_buckets(0.0005, 2.0, 16) gives
        // 0.5 ms .. ~16 s; lock that in so a future tweak can't quietly
        // lose either tail.
        let buckets = metrics::exponential_buckets(0.0005, 2.0, 16)
            .expect("exponential_buckets is well-formed");
        assert_eq!(buckets.len(), 16);
        assert!((buckets[0] - 0.0005).abs() < 1e-9, "first bucket = 0.5ms");
        assert!(
            buckets[15] > 16.0 && buckets[15] < 17.0,
            "last bucket ≈ 16s, got {}",
            buckets[15]
        );
    }
}
