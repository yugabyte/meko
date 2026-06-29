# compute_ctl Prometheus metrics

Source: `compute_tools/src/metrics.rs` (core metrics) and
`compute_tools/src/compute_metrics.rs` (Postgres-derived `pg_cctl_*` metrics). See also
[`compute_ctl-apis.md`](compute_ctl-apis.md) and [`compute_ctl.md`](compute_ctl.md).

All metrics are exposed as Prometheus text at **`GET /metrics`** on the external HTTP
server (default port 3080, **unauthenticated**). The handler
(`http/routes/metrics.rs::get_metrics`) concatenates `metrics::collect()` and
`compute_metrics::collect()`, filtering out empty metric families before encoding.

Everything here is **`compute_ctl`-native** (instrumented inside the supervisor itself).
The `pg_cctl_*` family is still emitted by `compute_ctl`, but its values are *driven by
Postgres behaviour* — e.g. incremented when Postgres calls back into `compute_ctl`. The
one truly forwarded surface is **`GET /autoscaling_metrics`**, which proxies the `neon`
extension's own exporter byte-for-byte; those metrics are *not* part of this list and are
not re-exposed on `/metrics`.

## Process / lifecycle (`metrics.rs`)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `compute_ctl_up` | IntGauge vec | `build_tag`, `status` | Whether `compute_ctl` is running and its current `ComputeStatus`. Reset + re-set on each status transition (`compute.rs`). |
| `compute_pg_current_downtime_ms` | Gauge (f64) | — | Non-cumulative current Postgres downtime in ms; resets to 0 after a successful liveness check (driven by the monitor loop). |
| `compute_pg_downtime_ms_total` | Counter (u64) | — | Cumulative Postgres downtime in ms. |
| `compute_audit_log_dir_size` | Gauge (f64) | — | Size of the audit-log directory in bytes. |
| `compute_ctl_cplane_requests_total` | IntCounter vec | `rpc`, `http_status` | Control-plane requests made by `compute_ctl`, by RPC (e.g. `GetConfig`) and HTTP status. |
| `compute_ctl_remote_ext_requests_total` | IntCounter vec | `http_status`, `filename` | Requests to download extensions from the S3/remote-ext proxy, by status and file. |
| `compute_ctl_db_migration_failed_total` | IntCounter vec | `migration_id` | Failed catalog migrations. Per-compute effectively boolean (stops at first failure); fleet sum = total failures. |

## Installed extensions (`metrics.rs`)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `compute_installed_extensions` | UIntGauge vec | `extension_name`, `version`, `owned_by_superuser` | Number of databases where this extension version is installed. Populated by the periodic `installed_extensions.rs` inventory of `pg_extension`. |

## LFC prewarm / offload (`metrics.rs`)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `compute_ctl_lfc_prewarms_total` | IntCounter | — | LFC prewarms requested (via `/lfc/prewarm` or the `autoprewarm` option). |
| `compute_ctl_lfc_prewarm_errors_total` | IntCounter | — | LFC prewarm errors. |
| `compute_ctl_lfc_offloads_total` | IntCounter | — | LFC offloads requested (via `/lfc/offload` or `lfc_offload_period_seconds`). |
| `compute_ctl_lfc_offload_errors_total` | IntCounter | — | LFC offload errors. |

## Postgres-derived (`compute_metrics.rs`)

Native to `compute_ctl` but incremented/updated in response to Postgres signals.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `pg_cctl_configure_statement_timeout_errors_total` | IntCounter | — | Compute reconfiguration failures caused by a Postgres statement timeout (error code 57014) inside `ComputeNode::reconfigure()`. Usually indicates PG stuck retrying against a wrong/rejecting pageserver. |
| `pg_cctl_attached` | IntGauge vec | `pg_compute_id`, `pg_instance_id`, `branch_id` | Compute attached status (1 if attached). Reset + re-set around attach (`compute.rs`). The storage stack identifies a branch by a single `branch_id`, so the former `tenant_id` / `timeline_id` label pair collapsed to one. |

The `pg_cctl_*` family is force-initialized at startup via
`compute_metrics::initialize_metrics()` so the counters appear (at 0) before the first
event.

## yb-light specifics

The metric set is upstream Neon's, inherited unchanged. The only fork-relevant note is in
`pg_cctl_attached`'s labels: yb-light collapsed `(tenant_id, timeline_id)` into a single
`branch_id` (and removed sharding), so the branch is keyed by `branch_id` rather than a
`TenantShardId` or an id pair. Otherwise these counters behave identically against the
YB-backed pageserver/safekeeper tier.
