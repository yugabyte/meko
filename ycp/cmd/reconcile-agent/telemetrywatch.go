package main

import (
	"context"
	"sync"
	"time"

	"meko/ycp/internal/api"
)

// envCollectorMetricsURL is the CONTROLLER collector's own-telemetry endpoint,
// set by the chart. Deliberately not the compute collector's and not the logs
// daemonset's:
//
//   - the compute collector scrapes endpoint pods, so an environment that owns no
//     endpoints has no targets and a legitimately flat counter — watching it would
//     report every freshly-created environment as broken;
//   - the logs daemonset ships only what containers actually write, so a quiet
//     environment is indistinguishable from a broken one.
//
// The controller collector always has a target (the amp controller itself, which
// exposes its own process and runtime metrics), so its counter climbs on the
// scrape clock regardless of what the environment is doing. That is what makes
// "flat" mean broken rather than idle.
const envCollectorMetricsURL = "OTEL_COLLECTOR_METRICS_URL"

// byoc exporter counters, as the collector names them. Verified against a live
// contrib 0.158.0; see awaitExport for the rename caveat.
const (
	metricSent      = "otelcol_exporter_sent_metric_points"
	metricFailed    = "otelcol_exporter_send_failed_metric_points"
	metricQueueSize = "otelcol_exporter_queue_size"
	byocExporterTag = `exporter="otlphttp/byoc"`
)

// telemetryWatch turns the collector's counters into "is this environment still
// shipping?" It keeps the previous reading so the CONTROL PLANE does not have to:
// a stall is only visible as a delta, and the agent is already the thing polling
// on a loop.
//
// Safe for concurrent use: Observe runs on the reconcile tick while the struct is
// read when a status report is built.
type telemetryWatch struct {
	url string

	mu         sync.Mutex
	lastSent   float64
	lastMoved  time.Time
	started    bool
	lastStats  api.Telemetry
	probeError string
	// probeFailedSince is when reads started failing, so a BLIP is not reported as
	// an outage. The control plane degrades an environment on a probe error, and a
	// single connection reset against an in-cluster Service is not evidence that
	// anything is wrong with the environment — it is evidence that one HTTP GET
	// failed. Only a failure that persists past probeGrace is worth saying.
	probeFailedSince time.Time
}

// probeGrace is how long reads may fail before the agent reports it. Comfortably
// more than a collector rollout (the operator restarts the pod on a config change)
// and several reconcile ticks, so a restart does not surface as a telemetry fault.
const probeGrace = 2 * time.Minute

func newTelemetryWatch(url string) *telemetryWatch {
	return &telemetryWatch{url: url}
}

// Observe reads the collector and folds the result into the running view. Two GETs
// of the same exposition page — the counters and the queue depth are separate
// scrapes — which is one more than it needs but keeps scrapeCounter a single shape.
//
// A read that fails leaves the stall clock alone: the agent being unable to look is
// not evidence that metrics stopped, and it is only reported at all once it has
// persisted past probeGrace.
func (w *telemetryWatch) Observe(ctx context.Context, now time.Time) {
	if w == nil || w.url == "" {
		return
	}
	sent, failed, err := scrapeCounter(ctx, w.url, metricSent, metricFailed, byocExporterTag)
	queued, _, qerr := scrapeCounter(ctx, w.url, metricQueueSize, metricQueueSize, byocExporterTag)
	if qerr != nil {
		queued = 0
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if err != nil {
		if w.probeFailedSince.IsZero() {
			w.probeFailedSince = now
		}
		// Reported only once it has persisted: below that this is indistinguishable
		// from a collector rolling, and reporting it would flip the environment to
		// DEGRADED for a single tick.
		if now.Sub(w.probeFailedSince) >= probeGrace {
			w.probeError = err.Error()
		}
		return
	}
	w.probeFailedSince = time.Time{}
	w.probeError = ""

	// First reading establishes the baseline rather than a stall: the agent may
	// have started long after the collector, and we have no idea how long that
	// counter had already been sitting still.
	if !w.started || sent > w.lastSent {
		w.lastMoved = now
	}
	w.started = true
	w.lastSent, w.lastStats = sent, api.Telemetry{
		MetricsSent:       sent,
		MetricsSendFailed: failed,
		MetricsQueued:     queued,
	}
}

// Report folds the observed runtime state onto the configuration attestation the
// chart supplied. Returns cfg unchanged when there is nothing to add, so a
// deployment whose chart wires no collector URL reports exactly what it did before.
func (w *telemetryWatch) Report(cfg *api.Telemetry, now time.Time) *api.Telemetry {
	if w == nil || cfg == nil || w.url == "" {
		return cfg
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	out := *cfg
	out.ProbeError = w.probeError
	if !w.started {
		return &out
	}
	out.MetricsSent = w.lastStats.MetricsSent
	out.MetricsSendFailed = w.lastStats.MetricsSendFailed
	out.MetricsQueued = w.lastStats.MetricsQueued
	if s := int(now.Sub(w.lastMoved).Seconds()); s > 0 {
		out.StalledSeconds = s
	}
	return &out
}
