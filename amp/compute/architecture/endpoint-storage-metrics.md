# `endpoint_storage` Prometheus metrics

Source: `compute/endpoint_storage/src/app.rs` (the `/metrics` route and handler),
`compute/endpoint_storage/src/main.rs` (`logging::init`),
`compute/libs/utils/src/logging.rs` (the only metrics present in the registry),
`compute/libs/remote_storage/src/metrics.rs` (confirms the backend registers none).

## Summary: no service-specific metrics

`endpoint_storage` defines **no custom metrics of its own**: no counters, gauges,
or histograms for uploads, downloads, deletes, request latency, body sizes, or
storage errors. The `YbStore` backend it writes through also registers no metrics —
`remote_storage::metrics` contains only the `RequestKind` enum, which exists to
select a read-vs-write concurrency semaphore, not to record anything. This is a
deliberately thin service: reads and writes are single blob operations against
YugabyteDB (which carries the durability and has its own observability), and the
one caller that matters (`compute_ctl`) already counts prewarm/offload outcomes on
its side (`compute_ctl_lfc_*`, see
[`compute_ctl-metrics.md`](compute_ctl-metrics.md)).

## The `/metrics` endpoint

`GET /metrics` (unauthenticated) returns the Prometheus text exposition of the
process-global default registry:

```rust
prometheus::TextEncoder::new()
    .encode_to_string(&prometheus::gather())
```

(`app.rs`). `prometheus::gather()` reads the default global registry, so whatever
any linked crate registers there is exposed. In this binary that is limited to the
logging counters installed by `utils::logging::init(...)` (`main.rs`).

## Metrics actually exposed

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `libmetrics_tracing_event_count` | counter (IntCounterVec) | `level` (`critical`, `error`, `warn`, `info`, `debug`, `trace`) | Count of emitted tracing/log events by level. Registered eagerly: `logging::init` forces `TRACING_EVENT_COUNT_METRIC` to install the counting layer, so this family is present from the first scrape. |
| `critical_storage_event_count` | counter (IntCounterVec) | `branch_id` | Count of "critical storage events" (the `critical!` logging path in `utils::logging`). Registered lazily on first use; absent from the exposition until then. |

There are **no** process/runtime collectors (no `process_cpu_seconds_total`,
`process_resident_memory_bytes`, etc.) — nothing in the binary's dependency chain
registers a process collector.

## Operational notes

- `yb_amp_local` uses `GET /metrics` as the service readiness/liveness probe — a
  `200` response means the service is up (`controller/local/launch/nodes.go`). It
  does not parse any metric values; `GET /status` would serve equally well.
- Because the only reliably-present series is `libmetrics_tracing_event_count`,
  `/metrics` supports log-volume / error-rate alerting on this service, not
  request- or storage-level observability. There is no metric that directly counts
  upload/download failures — those surface as `error`-level log events (and as
  `compute_ctl_lfc_prewarm_errors_total` / `compute_ctl_lfc_offload_errors_total`
  on the calling compute), or in YugabyteDB's own metrics underneath.
