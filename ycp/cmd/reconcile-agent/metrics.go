package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"meko/ycp/reconcile"
)

// envMetricsPort is the port /metrics is served on; 0 turns it off. Its own
// listener: the agent serves nothing else, and a collector reaching it needs no
// route into anything the agent drives.
const envMetricsPort = "AGENT_METRICS_PORT"

const defaultMetricsPort = 9464

// agentMetrics is what the loop reports about itself. The control plane sees a
// heartbeat go stale; these say why — a fetch the control plane refuses, a backend
// the engine cannot list, a report that does not land, or a tick that runs but
// leaves resources failing. A nil *agentMetrics records nothing.
type agentMetrics struct {
	reg          *prometheus.Registry
	ticks        *prometheus.CounterVec
	tickDuration prometheus.Histogram
	lastTickOK   prometheus.Gauge
	actions      *prometheus.CounterVec
	resources    *prometheus.GaugeVec
	observed     prometheus.Gauge
	desiredFetch *prometheus.CounterVec
	reports      *prometheus.CounterVec
	lastReportOK prometheus.Gauge
}

func newAgentMetrics(kind string) *agentMetrics {
	const ns = "ycp_agent"
	m := &agentMetrics{
		reg: prometheus.NewRegistry(),
		ticks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "ticks_total",
			Help: "Reconcile ticks, by outcome: ok, or error when the backend could not be listed.",
		}, []string{"outcome"}),
		tickDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: ns, Name: "tick_duration_seconds",
			Help:    "Time to list, diff and dispatch one tick.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}),
		lastTickOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Name: "last_successful_tick_timestamp_seconds",
			Help: "Unix time of the last tick that listed the backend. Alert on time() minus this.",
		}),
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "actions_total",
			Help: "Verbs that succeeded against the backend, by action: create, update, delete.",
		}, []string{"action"}),
		resources: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Name: "resources",
			Help: "Resources in each non-converged condition as of the last tick: failed, permanent, " +
				"updating, stuck, unreadable, deferred.",
		}, []string{"condition"}),
		observed: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Name: "resources_observed",
			Help: "Resources the backend listed on the last tick.",
		}),
		desiredFetch: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "desired_state_fetches_total",
			Help: "Desired-state reads from YCP, by outcome: ok, unauthorized (the key was revoked or " +
				"is unbound), error.",
		}, []string{"outcome"}),
		reports: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "status_reports_total",
			Help: "Status reports posted to YCP, by outcome: ok, error.",
		}, []string{"outcome"}),
		lastReportOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Name: "last_successful_report_timestamp_seconds",
			Help: "Unix time YCP last accepted a status report: the agent's side of the heartbeat.",
		}),
	}
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "info",
		Help: "Constant 1, labeled with the kind this agent serves.",
	}, []string{"kind"})
	info.WithLabelValues(kind).Set(1)
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
		info, m.ticks, m.tickDuration, m.lastTickOK, m.actions, m.resources, m.observed,
		m.desiredFetch, m.reports, m.lastReportOK,
	)
	// Present from the first scrape, so "no failures" reads as 0 rather than absent.
	for _, c := range []string{"failed", "permanent", "updating", "stuck", "unreadable", "deferred"} {
		m.resources.WithLabelValues(c)
	}
	return m
}

// serve starts the listener when AGENT_METRICS_PORT allows it. A listener that
// cannot bind is logged rather than fatal: the agent converging matters more than
// the agent being observable.
func (m *agentMetrics) serve() {
	port := defaultMetricsPort
	if v := os.Getenv(envMetricsPort); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			log.Printf("WARNING %s=%q is not a port; serving no metrics", envMetricsPort, v)
			return
		}
		port = p
	}
	if port <= 0 {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: ":" + strconv.Itoa(port), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("metrics on :%d", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("WARNING metrics listener stopped: %v", err)
		}
	}()
}

// tick is the engine's OnTick hook.
func (m *agentMetrics) tick(res reconcile.Result, err error, took time.Duration) {
	if m == nil {
		return
	}
	m.tickDuration.Observe(took.Seconds())
	if err != nil {
		m.ticks.WithLabelValues("error").Inc()
		return
	}
	m.ticks.WithLabelValues("ok").Inc()
	m.lastTickOK.SetToCurrentTime()
	m.actions.WithLabelValues("create").Add(float64(res.Created))
	m.actions.WithLabelValues("update").Add(float64(res.Updated))
	m.actions.WithLabelValues("delete").Add(float64(res.Deleted))
	m.resources.WithLabelValues("failed").Set(float64(len(res.Failed)))
	m.resources.WithLabelValues("permanent").Set(float64(len(res.Permanent)))
	m.resources.WithLabelValues("updating").Set(float64(len(res.Updating)))
	m.resources.WithLabelValues("stuck").Set(float64(len(res.Stuck)))
	m.resources.WithLabelValues("unreadable").Set(float64(len(res.Unreadable)))
	m.resources.WithLabelValues("deferred").Set(float64(res.Deferred))
	m.observed.Set(float64(len(res.Actual)))
}

func (m *agentMetrics) fetched(outcome string) {
	if m != nil {
		m.desiredFetch.WithLabelValues(outcome).Inc()
	}
}

func (m *agentMetrics) reported(err error) {
	if m == nil {
		return
	}
	if err != nil {
		m.reports.WithLabelValues("error").Inc()
		return
	}
	m.reports.WithLabelValues("ok").Inc()
	m.lastReportOK.SetToCurrentTime()
}
