use metrics::{
    IntCounter, IntGaugeVec, core::Collector, proto::MetricFamily, register_int_counter,
    register_int_gauge_vec,
};
use once_cell::sync::Lazy;

// Counter keeping track of the number of compute configuration errors due to Postgres statement
// timeouts. An error is registered every time `ComputeNode::reconfigure()` fails due to Postgres
// error code 57014 (query cancelled). This statement timeout typically occurs when postgres is
// stuck in a problematic retry loop when the PS is reject its connection requests (usually due
// to PG pointing at the wrong PS). We should investigate the root cause when this counter value
// increases by checking PG and PS logs.
pub static COMPUTE_CONFIGURE_STATEMENT_TIMEOUT_ERRORS: Lazy<IntCounter> = Lazy::new(|| {
    register_int_counter!(
        "pg_cctl_configure_statement_timeout_errors_total",
        "Number of compute configuration errors due to Postgres statement timeouts."
    )
    .expect("failed to define a metric")
});

pub static COMPUTE_ATTACHED: Lazy<IntGaugeVec> = Lazy::new(|| {
    register_int_gauge_vec!(
        "pg_cctl_attached",
        "Compute node attached status (1 if attached)",
        &["pg_compute_id", "pg_instance_id", "branch_id"]
    )
    .expect("failed to define a metric")
});

pub fn collect() -> Vec<MetricFamily> {
    let mut metrics = Vec::new();
    metrics.extend(COMPUTE_CONFIGURE_STATEMENT_TIMEOUT_ERRORS.collect());
    metrics.extend(COMPUTE_ATTACHED.collect());
    metrics
}

pub fn initialize_metrics() {
    Lazy::force(&COMPUTE_CONFIGURE_STATEMENT_TIMEOUT_ERRORS);
    Lazy::force(&COMPUTE_ATTACHED);
}
