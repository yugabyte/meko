package metrics

import (
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// RAGBacklogFunc counts knowledge-base documents by ingestion status. The rag
// workers drain what is not COMPLETED, so this is their backlog and failure count
// without the workers — which build from another repository — exporting anything.
// AdminRepo.CountDocumentsByStatus is the implementation.
type RAGBacklogFunc func() (map[string]int64, error)

// ragBacklogTTL bounds how often the count runs, whatever the scrape interval: it
// scans every pipeline run, and the backlog moves on the scale of minutes.
const ragBacklogTTL = time.Minute

const ragDocumentsHelp = "Knowledge-base documents by latest ingestion status, across every datapack. " +
	"Everything not COMPLETED is the rag workers' backlog or failures. Refreshed at most once a minute."

var ragDocumentsDesc = prometheus.NewDesc(namespace+"_rag_documents", ragDocumentsHelp, []string{"status"}, nil)

// ragBacklogCollector runs the count at scrape time, cached for ragBacklogTTL. A
// failed count emits nothing rather than a stale or zero one, so the series goes
// absent instead of lying.
type ragBacklogCollector struct {
	count   RAGBacklogFunc
	now     func() time.Time
	counts  map[string]int64
	lastErr error
	at      time.Time
	mu      sync.Mutex
}

func newRagBacklogCollector(count RAGBacklogFunc) *ragBacklogCollector {
	return &ragBacklogCollector{count: count, now: time.Now}
}

func (c *ragBacklogCollector) Describe(ch chan<- *prometheus.Desc) { ch <- ragDocumentsDesc }

func (c *ragBacklogCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || c.now().Sub(c.at) >= ragBacklogTTL {
		c.at = c.now()
		c.counts, c.lastErr = c.count()
		if c.lastErr != nil {
			// Once per TTL, not per scrape.
			slog.Warn("rag backlog metric: count failed", "error", c.lastErr)
		}
	}
	if c.lastErr != nil {
		return
	}
	for status, n := range c.counts {
		ch <- prometheus.MustNewConstMetric(ragDocumentsDesc, prometheus.GaugeValue, float64(n), status)
	}
}
