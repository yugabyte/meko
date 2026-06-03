use metrics::{IntCounterVec, register_int_counter_vec};
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
