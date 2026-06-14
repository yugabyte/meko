# `endpoint_storage` Prometheus metrics

Source: `endpoint_storage/src/app.rs:28,41-46` (the `/metrics` route and
handler), `endpoint_storage/src/main.rs:43-49` (`logging::init`),
`libs/utils/src/logging.rs:84-119` (the only metrics forced into the
registry at startup), `libs/remote_storage/src/metrics.rs` (confirms the
backend registers none).

## Summary: no service-specific metrics

`endpoint_storage` defines **no custom metrics of its own**. It does not
register any counters, gauges, or histograms for uploads, downloads,
deletes, request latency, body sizes, or storage errors. The `YbStore`
backend it writes to also registers no metrics: the S3-era bucket-request
metrics were removed when YB became the sole backend, and
`remote_storage::metrics` now contains only the `RequestKind` enum used to
pick a concurrency semaphore — no metric types
(`libs/remote_storage/src/metrics.rs:1-14`).

## The `/metrics` endpoint

`GET /metrics` (unauthenticated) returns the Prometheus text exposition of
the process-global default registry:

```rust
prometheus::TextEncoder::new()
    .encode_to_string(&prometheus::gather())
```

(`app.rs:41-46`). `prometheus::gather()` reads the default global registry,
so whatever any linked crate registers there is exposed. In this binary that
is limited to the logging counters set up by `utils::logging::init(...)`
(`main.rs:45-49`).

## Metrics actually exposed

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `libmetrics_tracing_event_count` | counter (IntCounterVec) | `level` (`critical`, `error`, `warn`, `info`, `debug`, `trace`) | Count of emitted tracing/log events by level. Registered eagerly: `logging::init` references `TRACING_EVENT_COUNT_METRIC` to install the counting layer (`logging.rs:111-119,216-218`). |
| `critical_storage_event_count` | counter (IntCounterVec) | `tenant_id`, `timeline_id` | Count of "critical storage events" (`logging.rs:88-108`). Registered lazily on first use of the `critical!` path; absent from the exposition until then. |

There are **no** process/runtime collectors (no
`process_cpu_seconds_total`, `process_resident_memory_bytes`, Go-style
runtime metrics, etc.) — no process collector is registered anywhere in the
dependency chain (`libs/utils/src/` has no `ProcessCollector` /
`process_collector` registration).

## Operational notes

- `control_plane` uses `GET /metrics` as the service liveness probe — a
  `200` response means the service is up
  (`control_plane/src/endpoint_storage.rs:106-112`). It does not parse any
  metric values.
- Because the only reliably-present series is
  `libmetrics_tracing_event_count`, `/metrics` is useful for log-volume /
  error-rate alerting on this service, not for request- or storage-level
  observability. There is currently no metric to alert on upload/download
  failures directly.
