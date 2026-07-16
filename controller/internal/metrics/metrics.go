// Package metrics owns the controller's Prometheus instrumentation: a private
// registry, the metric definitions, and the recording helpers the HTTP server
// and task worker call. It is the first step of the amp metrics pipeline
// (architecture/ac-metrics.md) — the controller is the one component that has
// to be instrumented from scratch (the Neon-derived services already emit
// Prometheus), so the series defined here are what the OTel Collector scrapes
// from GET /metrics and remote-writes to the metrics store.
//
// The metric names follow the Prometheus convention
// namespace_subsystem_name{labels}, all under the amp_controller_ namespace.
// Recording helpers are nil-safe on a *Metrics receiver so callers that were
// built without metrics (tests) can invoke them unconditionally.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/yugabyte/yb-amp/controller/internal/version"
)

// namespace prefixes every series this package defines.
const namespace = "amp_controller"

// statusInterrupted is the task outcome for a task left reclaimable by shutdown
// (not a terminal success/failure). It mirrors the worker's semantics: the task
// is counted, but its partial duration is not observed.
const statusInterrupted = "interrupted"

// Metrics holds the controller's registry and the series recorded at runtime.
// Construct it with New; a nil *Metrics is a valid no-op recorder.
type Metrics struct {
	reg *prometheus.Registry

	httpRequests  *prometheus.CounterVec
	httpDuration  *prometheus.HistogramVec
	tasksTotal    *prometheus.CounterVec
	taskDuration  *prometheus.HistogramVec
	tasksInflight prometheus.Gauge
}

// New builds the registry, registers the Go runtime and process collectors and
// the controller's own series, and records a constant build_info series stamped
// with the version package's build identity.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		reg: reg,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "http", Name: "requests_total",
			Help: "Total HTTP requests handled, by method, route template, and response status code.",
		}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "http", Name: "request_duration_seconds",
			Help:    "HTTP request latency in seconds, by method and route template.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		tasksTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "tasks", Name: "total",
			Help: "Tasks the worker finished executing, by kind and outcome (succeeded, failed, interrupted).",
		}, []string{"kind", "status"}),
		taskDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "tasks", Name: "duration_seconds",
			Help: "Task execution wall-clock time in seconds, by kind (terminal outcomes only).",
			// Provisioning tasks (create endpoint/branch) run seconds to minutes,
			// so extend the default buckets out past a minute.
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"kind"}),
		tasksInflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "tasks", Name: "inflight",
			Help: "Tasks the worker is currently executing.",
		}),
	}

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.httpRequests,
		m.httpDuration,
		m.tasksTotal,
		m.taskDuration,
		m.tasksInflight,
	)

	// build_info: a constant-1 gauge whose labels carry the build identity, so a
	// dashboard can join running-version onto any series (the standard
	// *_build_info convention).
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "build_info",
		Help: "Controller build identity; constant 1 with version, commit, and build_date labels.",
	}, []string{"version", "commit", "build_date"})
	reg.MustRegister(buildInfo)
	v := version.Get()
	buildInfo.WithLabelValues(v.Version, v.Commit, v.BuildDate).Set(1)

	return m
}

// Registry exposes the underlying registry for the /metrics HTTP handler.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// ObserveHTTP records one handled request. route is the matched route template
// (e.g. /api/v1/pools/:poolId), never the raw URI, so label cardinality stays
// bounded regardless of path parameters.
func (m *Metrics) ObserveHTTP(method, route string, status int, dur time.Duration) {
	if m == nil {
		return
	}
	m.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(dur.Seconds())
}

// TaskStarted marks a task entering execution (inflight++). Pair every call with
// exactly one TaskFinished.
func (m *Metrics) TaskStarted() {
	if m == nil {
		return
	}
	m.tasksInflight.Inc()
}

// TaskFinished marks a task leaving execution (inflight--) and counts the
// outcome. status is one of "succeeded", "failed", or "interrupted"; duration is
// observed only for terminal outcomes (an interrupted task did partial work and
// is left reclaimable, so its partial time would skew the histogram).
func (m *Metrics) TaskFinished(kind, status string, dur time.Duration) {
	if m == nil {
		return
	}
	m.tasksInflight.Dec()
	m.tasksTotal.WithLabelValues(kind, status).Inc()
	if status != statusInterrupted {
		m.taskDuration.WithLabelValues(kind).Observe(dur.Seconds())
	}
}
