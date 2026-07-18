package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// RegistryStatsSource supplies live registry inventory counts. It is queried on
// each Prometheus scrape, so implementations must keep it cheap.
// registry.Store satisfies this interface.
type RegistryStatsSource interface {
	RegistryCounts(ctx context.Context) (branches, nodes int64, err error)
}

// registryCollector emits the registry inventory gauges
// (amp_controller_registry_branches / amp_controller_registry_nodes) by querying
// the source at scrape time — no cached value to go stale and no background
// goroutine to manage. The counts are cheap (count(*) over two small tables).
type registryCollector struct {
	src      RegistryStatsSource
	logger   *slog.Logger
	branches *prometheus.Desc
	nodes    *prometheus.Desc
}

func newRegistryCollector(src RegistryStatsSource, logger *slog.Logger) *registryCollector {
	if logger == nil {
		logger = slog.Default()
	}
	return &registryCollector{
		src:    src,
		logger: logger,
		branches: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "registry", "branches"),
			"Branches currently recorded in the registry.", nil, nil),
		nodes: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "registry", "nodes"),
			"Pageserver nodes currently registered.", nil, nil),
	}
}

func (c *registryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.branches
	ch <- c.nodes
}

// Collect queries the counts under a short timeout. On error it logs and emits
// nothing for this scrape — a missing sample is safer than a wrong one, and a
// transient DB blip resolves on the next scrape.
func (c *registryCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	branches, nodes, err := c.src.RegistryCounts(ctx)
	if err != nil {
		c.logger.Warn("registry metrics: count query failed; skipping scrape", "error", err)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.branches, prometheus.GaugeValue, float64(branches))
	ch <- prometheus.MustNewConstMetric(c.nodes, prometheus.GaugeValue, float64(nodes))
}

// WithRegistrySource registers a scrape-time collector for the registry
// inventory gauges. No-op on a nil *Metrics or nil source; call once, after New.
func (m *Metrics) WithRegistrySource(src RegistryStatsSource, logger *slog.Logger) {
	if m == nil || src == nil {
		return
	}
	m.reg.MustRegister(newRegistryCollector(src, logger))
}
