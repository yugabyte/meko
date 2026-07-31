# compute_ctl Prometheus metrics

Source: `compute/compute_tools/src/metrics.rs` (core metrics) and
`compute/compute_tools/src/compute_metrics.rs` (the `pg_cctl_*` family). See also
[`compute_ctl-apis.md`](compute_ctl-apis.md) and [`compute_ctl.md`](compute_ctl.md).

All metrics here are exposed as Prometheus text at **`GET /metrics`** on the external
HTTP server (default port 3080, **unauthenticated**). The handler
(`http/routes/metrics.rs::get_metrics`) concatenates `metrics::collect()` and
`compute_metrics::collect()`, filtering out empty metric families before encoding
(the text encoder errors on empty families).

Everything on `/metrics` is **`compute_ctl`-native** — instrumented inside the
supervisor itself. The `pg_cctl_*` family is also emitted by `compute_ctl`, but its
values are *driven by Postgres/attachment events*. Two adjacent surfaces are easy to
confuse with this one:

- **`GET /autoscaling_metrics`** proxies the `yb_amp` extension's own in-Postgres
  exporter byte-for-byte (over the `yb_amp-communicator.socket` Unix socket in
  pgdata). Those metrics are **not** part of this list and are not re-exposed on
  `/metrics`.
- **`GET /metrics.json`** is a JSON snapshot of one-shot startup timings
  (`ComputeMetrics` in `compute/libs/compute_api/src/responses.rs`), not a
  Prometheus surface.

## Process / lifecycle (`metrics.rs`)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `compute_ctl_up` | IntGauge vec | `build_tag`, `status` | Whether `compute_ctl` is running, keyed by its current `ComputeStatus`. Set to `{status="empty"}` before the HTTP server even starts (so the first scrape sees the process), then reset + re-set on every status transition (`ComputeState::set_status`). |
| `compute_pg_current_downtime_ms` | Gauge (f64) | — | Non-cumulative current Postgres downtime in ms; reset to 0 after each successful liveness check. Driven by the 500 ms monitor loop (`monitor.rs`). |
| `compute_pg_downtime_ms_total` | Counter (u64) | — | Cumulative Postgres downtime in ms. |
| `compute_audit_log_dir_size` | Gauge (f64) | — | Size of the pgaudit log directory in bytes; updated by the audit-log GC loop (`rsyslog.rs`), so it only moves when compliance audit logging is enabled. |
| `compute_ctl_cplane_requests_total` | IntCounter vec | `rpc`, `http_status` | Control-plane requests made by `compute_ctl`, by RPC (`GetConfig` is the only RPC today) and HTTP status (`unknown` when the request never got a response). |
| `compute_ctl_remote_ext_requests_total` | IntCounter vec | `http_status`, `filename` | Requests to the remote-extension proxy gateway (`--remote-ext-base-url`), by status and file. |
| `compute_ctl_db_migration_failed_total` | IntCounter vec | `migration_id` | Failed catalog migrations. Per-compute this is effectively boolean — the runner stops at the first failure — so the fleet-wide sum is the total failure count. |

## Installed extensions (`metrics.rs`)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `compute_installed_extensions` | UIntGauge vec | `extension_name`, `version`, `owned_by_superuser` | Number of databases where this extension version is installed. Populated by the periodic `pg_extension` inventory (`installed_extensions.rs`); the collection interval derives from `suspend_timeout_seconds` (default 3600 s). |

## LFC prewarm / offload (`metrics.rs`)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `compute_ctl_lfc_prewarms_total` | IntCounter | — | LFC prewarms requested (via `POST /lfc/prewarm` or the `autoprewarm` spec option). |
| `compute_ctl_lfc_prewarm_errors_total` | IntCounter | — | LFC prewarm attempts that ended in `failed` (a 404 from endpoint_storage counts as `skipped`, not an error). |
| `compute_ctl_lfc_offloads_total` | IntCounter | — | LFC offloads requested (via `POST /lfc/offload` or the `offload_lfc_interval_seconds` loop). |
| `compute_ctl_lfc_offload_errors_total` | IntCounter | — | LFC offload errors. |

## `pg_cctl_*` (`compute_metrics.rs`)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `pg_cctl_configure_statement_timeout_errors_total` | IntCounter | — | Reconfigurations that failed with Postgres error 57014 (statement timeout) inside `ComputeNode::reconfigure()`. Usually means Postgres is stuck retrying against a pageserver that rejects its connections (e.g. pointed at the wrong pageserver); check PG and pageserver logs when it moves. |
| `pg_cctl_attached` | IntGauge vec | `pg_compute_id`, `pg_instance_id`, `branch_id` | Compute attached status (1 if attached). Reset + re-set whenever the spec is (re)loaded (`update_attached_metric` in `compute.rs`). Only emitted when an `instance_id` is configured (the `INSTANCE_ID` env var) — without one the gauge stays empty and is filtered from the exposition. The branch is keyed by the single `branch_id`; there are no shard or tenant/timeline labels anywhere in the metric set. |

Registration is lazy (`once_cell::Lazy`), so a family appears in the exposition only
after first touch — except in `--lakebase-mode`, where `main()` calls
`initialize_metrics()` for the `pg_cctl_*` and installed-extensions families to
force them to exist (at 0) from the first scrape.

## Tracing

Independent of Prometheus, `compute_ctl` exports OpenTelemetry traces:
`init_tracing_and_logging()` (`logger.rs`) sets up an OTLP provider, request spans
carry `X-Request-Id`, and the startup sequence attaches to a trace context passed
either by the `/configure` request or via `OTEL_*`/startup env vars — so a cold
start is visible as one distributed trace from the control plane down.
