// Package metrics owns the api-server's Prometheus instrumentation: a private
// registry, the series definitions, and the Echo middleware that records them.
// The chart's OTel collector scrapes it (deploy/helm/meko/templates/otel), the
// same collect-and-export shape as amp's controller (ac-metrics.md).
//
// Recording is nil-safe on a *Metrics receiver, so a server built with metrics
// off records nothing without every call site checking.
package metrics

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "meko_api"

// unmatchedRoute is the route label for a request no route matched. Echo leaves
// the path empty there, and labeling by raw URI instead would let any scanner
// mint a series per probe.
const unmatchedRoute = "unmatched"

// otherMethod labels any method outside knownMethods. The method is
// client-controlled and net/http accepts any token, so an unclamped label lets
// anyone reaching the API mint series without bound.
const otherMethod = "OTHER"

var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true,
	http.MethodDelete: true, http.MethodHead: true, http.MethodOptions: true,
}

// Metrics holds the registry and the series recorded at runtime. A nil *Metrics
// is a valid no-op.
type Metrics struct {
	reg          *prometheus.Registry
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	httpInflight prometheus.Gauge
}

// New builds the registry with the Go runtime, process and build-info
// collectors, the HTTP series, and connection-pool stats for each database.
// systemDB is registered only when it is a different pool from dataDB: the
// colocated layout hands both fields the same *sql.DB, and reporting it twice
// would double every sum.
func New(dataDB, systemDB *sql.DB) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "http", Name: "requests_total",
			Help: "HTTP requests handled, by method, route template and response status code.",
		}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "http", Name: "request_duration_seconds",
			Help:    "HTTP request latency in seconds, by method and route template.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		httpInflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "http", Name: "requests_inflight",
			Help: "HTTP requests currently being served.",
		}),
	}
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
		m.httpRequests,
		m.httpDuration,
		m.httpInflight,
	)
	if dataDB != nil {
		reg.MustRegister(collectors.NewDBStatsCollector(dataDB, "data"))
	}
	if systemDB != nil && systemDB != dataDB {
		reg.MustRegister(collectors.NewDBStatsCollector(systemDB, "system"))
	}
	return m
}

// WithRAGBacklog adds the knowledge-base ingestion backlog. Only for a data
// plane: a control plane has no dist_rag to count.
func (m *Metrics) WithRAGBacklog(count RAGBacklogFunc) *Metrics {
	if m != nil && count != nil {
		m.reg.MustRegister(newRagBacklogCollector(count))
	}
	return m
}

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// Middleware records every request against its route TEMPLATE
// (/api/v1/datapacks/:id), never the raw URI, so path parameters cannot grow
// the label set. It returns the handler's error untouched — the request logger
// renders it — so for an uncommitted response the status is read off the error
// the way the error handler will.
func (m *Metrics) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if m == nil {
			return next
		}
		return func(c echo.Context) error {
			m.httpInflight.Inc()
			defer m.httpInflight.Dec()
			start := time.Now()
			err := next(c)
			route := c.Path()
			if route == "" {
				route = unmatchedRoute
			}
			method := c.Request().Method
			if !knownMethods[method] {
				method = otherMethod
			}
			m.httpRequests.WithLabelValues(method, route, strconv.Itoa(statusOf(c, err))).Inc()
			m.httpDuration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
			return err
		}
	}
}

func statusOf(c echo.Context, err error) int {
	if err == nil || c.Response().Committed {
		return c.Response().Status
	}
	var he *echo.HTTPError
	if errors.As(err, &he) {
		return he.Code
	}
	return http.StatusInternalServerError
}
