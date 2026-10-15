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

	"github.com/yugabyte/amp/controller/internal/version"
)

// namespace prefixes every series this package defines.
const namespace = "amp_controller"

// statusInterrupted is the task outcome for a task left reclaimable by shutdown
// (not a terminal success/failure). It mirrors the worker's semantics: the task
// is counted, but its partial duration is not observed.
const statusInterrupted = "interrupted"

// taskBuckets extend the default buckets out past a minute: provisioning tasks
// (create endpoint/branch) and their steps run seconds to minutes.
var taskBuckets = []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// Metrics holds the controller's registry and the series recorded at runtime.
// Construct it with New; a nil *Metrics is a valid no-op recorder.
type Metrics struct {
	reg *prometheus.Registry

	httpRequests   *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	tasksTotal     *prometheus.CounterVec
	taskDuration   *prometheus.HistogramVec
	tasksInflight  prometheus.Gauge
	tasksReclaimed prometheus.Counter
	stepRetries    *prometheus.CounterVec
	taskPickup     *prometheus.HistogramVec
	stepDuration   *prometheus.HistogramVec
	deployPhase    *prometheus.HistogramVec
	gcOrphans      *prometheus.CounterVec
	gcSweeps       prometheus.Counter
	gcSweepDur     prometheus.Histogram
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
			Help:    "Task execution wall-clock time in seconds, by kind (terminal outcomes only).",
			Buckets: taskBuckets,
		}, []string{"kind"}),
		tasksInflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "tasks", Name: "inflight",
			Help: "Tasks the worker is currently executing.",
		}),
		// Unlabelled so the series exists at 0 from startup: a vec has none until
		// its first increment, and a series that appears only once something has
		// gone wrong cannot be alerted on.
		tasksReclaimed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "tasks", Name: "reclaimed_total",
			Help: "Tasks taken over from another controller process that left them running: it died holding their leases, or shut down with them in flight.",
		}),
		stepRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "tasks", Name: "step_retries_total",
			Help: "Step re-runs after losing a write interlock (SQLSTATE 40001). Labelled by task_step.kind, deliberately NOT `kind` like tasks_total — a step kind and a task kind are different name spaces and joining them yields nonsense. Contention, not breakage.",
		}, []string{"step_kind"}),
		taskPickup: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "tasks", Name: "pickup_seconds",
			Help:    "Queue wait in seconds from a task being planned to its first claim, by kind. Reclaims are not observed.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		}, []string{"kind"}),
		stepDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "tasks", Name: "step_duration_seconds",
			Help:    "Wall-clock seconds of one successful step attempt, retries included, by task_step.kind.",
			Buckets: taskBuckets,
		}, []string{"step_kind"}),
		deployPhase: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "deploy", Name: "phase_seconds",
			Help:    "Seconds an endpoint deploy spent in each phase: apply (helm, less the pod coming Ready), pod_ready (pod created to Ready, by the pod's own timestamps), verify (the safekeeper answering through the Service). Successful Kubernetes deploys only.",
			Buckets: taskBuckets,
		}, []string{"phase"}),
	}

	m.gcOrphans = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "gc", Name: "orphans_total",
		Help: "Failed-create orphans seen by the GC sweeper, by resource (cluster, endpoint) and outcome: enqueued (teardown recorded), skipped (no longer reclaimable), failed (left for the next sweep).",
	}, []string{"resource", "outcome"})
	m.gcSweeps = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "gc", Name: "sweeps_total",
		Help: "Completed GC sweeps. A sweep that found nothing still counts, so a flat orphans_total against a rising sweeps_total means the sweeper is running and idle rather than stuck.",
	})
	m.gcSweepDur = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "gc", Name: "sweep_duration_seconds",
		Help:    "GC sweep duration in seconds, covering selection plus every per-orphan precheck and enqueue.",
		Buckets: prometheus.DefBuckets,
	})

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.httpRequests,
		m.httpDuration,
		m.tasksTotal,
		m.taskDuration,
		m.tasksInflight,
		m.tasksReclaimed,
		m.stepRetries,
		m.taskPickup,
		m.stepDuration,
		m.deployPhase,
		m.gcOrphans,
		m.gcSweeps,
		m.gcSweepDur,
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

// TaskReclaimed counts one task taken over from another process that left it
// running.
func (m *Metrics) TaskReclaimed() {
	if m == nil {
		return
	}
	m.tasksReclaimed.Inc()
}

// StepRetried counts one re-run of a step after it lost a write interlock.
// stepKind is the task_step.kind, not the task kind.
func (m *Metrics) StepRetried(stepKind string) {
	if m == nil {
		return
	}
	m.stepRetries.WithLabelValues(stepKind).Inc()
}

// TaskPickedUp observes a task's queue wait before its first claim.
func (m *Metrics) TaskPickedUp(kind string, wait time.Duration) {
	if m == nil {
		return
	}
	m.taskPickup.WithLabelValues(kind).Observe(wait.Seconds())
}

// StepFinished observes one successful step attempt. stepKind is the
// task_step.kind, not the task kind.
func (m *Metrics) StepFinished(stepKind string, dur time.Duration) {
	if m == nil {
		return
	}
	m.stepDuration.WithLabelValues(stepKind).Observe(dur.Seconds())
}

// DeployPhase observes one phase of an endpoint deploy.
func (m *Metrics) DeployPhase(phase string, dur time.Duration) {
	if m == nil {
		return
	}
	m.deployPhase.WithLabelValues(phase).Observe(dur.Seconds())
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

// GCSwept records one completed GC sweep, covering every resource kind's pass.
func (m *Metrics) GCSwept(dur time.Duration) {
	if m == nil {
		return
	}
	m.gcSweeps.Inc()
	m.gcSweepDur.Observe(dur.Seconds())
}

// GCOrphans records what became of one resource kind's failed-create orphans in
// a sweep. How many were found is not a series of its own — it is
// enqueued+skipped+failed by construction, so a fourth counter could only
// disagree with the other three.
func (m *Metrics) GCOrphans(resource string, enqueued, skipped, failed int) {
	if m == nil {
		return
	}
	m.gcOrphans.WithLabelValues(resource, "enqueued").Add(float64(enqueued))
	m.gcOrphans.WithLabelValues(resource, "skipped").Add(float64(skipped))
	m.gcOrphans.WithLabelValues(resource, "failed").Add(float64(failed))
}
