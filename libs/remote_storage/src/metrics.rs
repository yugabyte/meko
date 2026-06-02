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
/// because they both link `remote_storage` and the underlying
/// `prometheus::default_registry()` is process-global.
pub static YB_CLIENT_ERRORS: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "yb_client_errors_total",
        "YugabyteDB client errors observed by YsqlClient, classified by retry class and SQLSTATE",
        &["op", "class", "sqlstate"],
    )
    .expect("register yb_client_errors_total")
});

/// Counts cases where setting the `yb_enable_upsert_mode = true`
/// session GUC failed on a freshly-opened YsqlClient connection (the
/// `42704 undefined_object` case is benign and *not* counted here —
/// it just means we're talking to vanilla Postgres in a test).
/// Anything else means subsequent INSERTs on that connection's
/// lifetime won't upsert and chunk retries can hit duplicate-key.
pub static YB_SET_UPSERT_MODE_FAILED: Lazy<metrics::IntCounter> = Lazy::new(|| {
    metrics::register_int_counter!(
        "yb_set_upsert_mode_failed_total",
        "SET yb_enable_upsert_mode = true rejected with a non-undefined-GUC error",
    )
    .expect("register yb_set_upsert_mode_failed_total")
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

/// Selects which `ConcurrencyLimiter` semaphore an operation acquires
/// (`read` vs `write`). The S3-era bucket-request metrics that used to
/// hang off this enum were removed when the YB-only store became the
/// sole backend; the variant set is preserved as-is so `YbStore` call
/// sites don't need to change shape.
#[derive(Clone, Copy, Debug)]
pub(crate) enum RequestKind {
    Get,
    Put,
    Delete,
    List,
    Copy,
    Head,
}
