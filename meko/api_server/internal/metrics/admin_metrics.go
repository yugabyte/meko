package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Admin business metrics expose the admin dashboard's scalar aggregates (user
// counts, datapacks, token savings, memories) as gauges on the same :9464
// endpoint the collector scrapes, so BYOC fleets surface them upstream through
// the OTel door without a second transport. They read the admin repo directly,
// not the /admin/* HTTP API — the admin shared-secret surface is untouched.
//
// Only point-in-time, low-cardinality aggregates belong here. Date windows are a
// PromQL concern upstream (the value is the current total), and per-user or
// leaderboard tables stay behind the admin API: a Prometheus series per user is
// unbounded cardinality, and re-exporting user-identifying rows through the
// telemetry plane is a trust boundary the admin secret otherwise holds.

// adminMetricsTTL bounds how often the aggregates run, independent of the scrape
// interval. These move on the scale of minutes and some scan sizeable tables, so
// recomputing every scrape would be waste for no fresher a number.
const adminMetricsTTL = time.Minute

// adminRefreshTimeout caps one background refresh of all probes. Because the
// refresh runs off the scrape path (Collect never waits on it — see the collector
// doc), this is a generous backstop, not a scrape-latency budget: it sits above
// the KB scan's own statement_timeout (kbStmtTimeout, 30s) so a legitimate
// all-time dist_rag scan on a large deployment completes instead of being
// canceled every cycle. Every source runs under this context, so a stuck
// datastore's query is canceled here and that probe drops its series for the
// cycle without stalling the others.
const adminRefreshTimeout = 45 * time.Second

// Every api-server replica exports the same deployment-wide totals, labeled by
// pod, so a reader must collapse replicas with max (never sum, which multiplies
// by the replica count). The help text says so because this value ships upstream
// as the series description.
var (
	adminUsersDesc = prometheus.NewDesc(namespace+"_admin_users",
		"Current registered users by tier (free, standard, pro — pro includes enterprise). "+
			"One series per replica: aggregate with max by (tier), not sum. Refreshed at most once a minute.",
		[]string{"tier"}, nil)
	adminDatapacksDesc = prometheus.NewDesc(namespace+"_admin_datapacks",
		"Current total number of datapacks. One series per replica: aggregate with max. "+
			"Refreshed at most once a minute.",
		nil, nil)
	adminTokenSavingsRequestsDesc = prometheus.NewDesc(namespace+"_admin_token_savings_requests",
		"Lifetime count of contextual-search requests (each may save zero or more tokens). One "+
			"series per replica: aggregate with max. Refreshed at most once a minute.",
		nil, nil)
	adminTokenSavingsTokensDesc = prometheus.NewDesc(namespace+"_admin_token_savings_tokens",
		"Lifetime tokens saved by contextual search versus raw retrieval. One series per replica: "+
			"aggregate with max. Refreshed at most once a minute.",
		nil, nil)
	adminMemoriesDesc = prometheus.NewDesc(namespace+"_admin_memories",
		"Current memories by kind (private, promoted). One series per replica: aggregate with "+
			"max by (kind), not sum. Refreshed at most once a minute.",
		[]string{"kind"}, nil)
	adminKBDocumentsDesc = prometheus.NewDesc(namespace+"_admin_kb_documents",
		"Current indexed (COMPLETED) knowledge-base documents, across every datapack. One series "+
			"per replica: aggregate with max. Refreshed at most once a minute.",
		nil, nil)
	adminKBChunksDesc = prometheus.NewDesc(namespace+"_admin_kb_chunks",
		"Current chunks across indexed knowledge-base documents. One series per replica: aggregate "+
			"with max. Refreshed at most once a minute.",
		nil, nil)
	adminKBDocumentsByTypeDesc = prometheus.NewDesc(namespace+"_admin_kb_documents_by_type",
		"Indexed knowledge-base documents by file type (MIME, 'unknown' when unset); sums to "+
			"meko_api_admin_kb_documents. One series per replica: aggregate with max by (type). "+
			"Refreshed at most once a minute.",
		[]string{"type"}, nil)
)

// AdminBusinessSources are the read-only admin aggregates exported as gauges.
// Every field is optional: a nil source is skipped, so a mode or deployment that
// lacks a datastore (e.g. a control plane with no data plane) simply omits those
// series. Each returns the current absolute count(s); the collector caches them
// and windowing is left to PromQL.
//
// The context carries the refresh deadline; every source must run its query
// under it (QueryRowContext) so a stuck datastore is canceled at the deadline
// rather than holding the refresh — and with it the scrape — open.
type AdminBusinessSources struct {
	// UsersByTier returns the current user count in each tier (free, standard, pro).
	UsersByTier func(ctx context.Context) (free, standard, pro int64, err error)
	// Datapacks returns the current total datapack count.
	Datapacks func(ctx context.Context) (int64, error)
	// TokenSavings returns the lifetime contextual-search request count and the
	// tokens those searches saved versus raw retrieval.
	TokenSavings func(ctx context.Context) (requests, tokensSaved int64, err error)
	// Memories returns the current private and promoted memory counts.
	Memories func(ctx context.Context) (private, promoted int64, err error)
	// KnowledgeBase returns the current indexed-document and chunk totals and the
	// per-file-type document counts (the totals are the sum across types).
	KnowledgeBase func(ctx context.Context) (documents, chunks int64, byType map[string]int64, err error)
}

// adminSample is one emitted series: a descriptor, its label values in order, and
// the value.
type adminSample struct {
	desc   *prometheus.Desc
	labels []string
	value  float64
}

// adminProbe is one admin query and the series it produces. A probe either
// succeeds and caches its samples, or fails and emits nothing — a stale or zeroed
// number would read as a real count. descs lists every series the probe can emit,
// for Describe.
type adminProbe struct {
	descs   []*prometheus.Desc
	read    func(ctx context.Context) ([]adminSample, error)
	samples []adminSample
	ok      bool
}

// adminMetricsCollector serves its probes' last cached values and, when the cache
// is older than adminMetricsTTL, kicks off a single background refresh. Collect
// never runs a query itself, so a scrape is always fast and a slow or stuck
// datastore can never stall the :9464 endpoint — only its own series goes stale
// then absent. A failed probe is omitted without failing the others.
type adminMetricsCollector struct {
	at         time.Time
	now        func() time.Time
	probes     []*adminProbe
	refreshing bool
	mu         sync.Mutex
}

func newAdminMetricsCollector(src AdminBusinessSources) *adminMetricsCollector {
	c := &adminMetricsCollector{now: time.Now}

	if f := src.UsersByTier; f != nil {
		c.probes = append(c.probes, &adminProbe{
			descs: []*prometheus.Desc{adminUsersDesc},
			read: func(ctx context.Context) ([]adminSample, error) {
				free, standard, pro, err := f(ctx)
				if err != nil {
					return nil, err
				}
				return []adminSample{
					{desc: adminUsersDesc, labels: []string{"free"}, value: float64(free)},
					{desc: adminUsersDesc, labels: []string{"standard"}, value: float64(standard)},
					{desc: adminUsersDesc, labels: []string{"pro"}, value: float64(pro)},
				}, nil
			},
		})
	}

	if f := src.Datapacks; f != nil {
		c.probes = append(c.probes, &adminProbe{
			descs: []*prometheus.Desc{adminDatapacksDesc},
			read: func(ctx context.Context) ([]adminSample, error) {
				n, err := f(ctx)
				if err != nil {
					return nil, err
				}
				return []adminSample{{desc: adminDatapacksDesc, value: float64(n)}}, nil
			},
		})
	}

	if f := src.TokenSavings; f != nil {
		c.probes = append(c.probes, &adminProbe{
			descs: []*prometheus.Desc{adminTokenSavingsRequestsDesc, adminTokenSavingsTokensDesc},
			read: func(ctx context.Context) ([]adminSample, error) {
				requests, tokens, err := f(ctx)
				if err != nil {
					return nil, err
				}
				return []adminSample{
					{desc: adminTokenSavingsRequestsDesc, value: float64(requests)},
					{desc: adminTokenSavingsTokensDesc, value: float64(tokens)},
				}, nil
			},
		})
	}

	if f := src.Memories; f != nil {
		c.probes = append(c.probes, &adminProbe{
			descs: []*prometheus.Desc{adminMemoriesDesc},
			read: func(ctx context.Context) ([]adminSample, error) {
				private, promoted, err := f(ctx)
				if err != nil {
					return nil, err
				}
				return []adminSample{
					{desc: adminMemoriesDesc, labels: []string{"private"}, value: float64(private)},
					{desc: adminMemoriesDesc, labels: []string{"promoted"}, value: float64(promoted)},
				}, nil
			},
		})
	}

	if f := src.KnowledgeBase; f != nil {
		c.probes = append(c.probes, &adminProbe{
			descs: []*prometheus.Desc{adminKBDocumentsDesc, adminKBChunksDesc, adminKBDocumentsByTypeDesc},
			read: func(ctx context.Context) ([]adminSample, error) {
				documents, chunks, byType, err := f(ctx)
				if err != nil {
					return nil, err
				}
				samples := []adminSample{
					{desc: adminKBDocumentsDesc, value: float64(documents)},
					{desc: adminKBChunksDesc, value: float64(chunks)},
				}
				for docType, n := range byType {
					samples = append(samples, adminSample{
						desc: adminKBDocumentsByTypeDesc, labels: []string{docType}, value: float64(n),
					})
				}
				return samples, nil
			},
		})
	}

	return c
}

func (c *adminMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, p := range c.probes {
		for _, d := range p.descs {
			ch <- d
		}
	}
}

func (c *adminMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	if !c.refreshing && (c.at.IsZero() || c.now().Sub(c.at) >= adminMetricsTTL) {
		c.refreshing = true
		go c.backgroundRefresh()
	}
	var out []prometheus.Metric
	for _, p := range c.probes {
		if !p.ok {
			continue
		}
		for _, s := range p.samples {
			out = append(out, prometheus.MustNewConstMetric(s.desc, prometheus.GaugeValue, s.value, s.labels...))
		}
	}
	c.mu.Unlock()
	for _, m := range out {
		ch <- m
	}
}

// backgroundRefresh runs one refresh off the scrape path and marks the cache
// fresh. Only one runs at a time (the refreshing flag), so a slow refresh does
// not pile up behind frequent scrapes.
func (c *adminMetricsCollector) backgroundRefresh() {
	ctx, cancel := context.WithTimeout(context.Background(), adminRefreshTimeout)
	defer cancel()
	c.runProbes(ctx)
	c.mu.Lock()
	c.at = c.now()
	c.refreshing = false
	c.mu.Unlock()
}

// runProbes reads every probe concurrently, then commits the results under c.mu.
// Each goroutine writes only its own result slot, which wg.Wait synchronizes with
// the commit, so no probe field is touched outside the lock.
func (c *adminMetricsCollector) runProbes(ctx context.Context) {
	results := make([]struct {
		samples []adminSample
		ok      bool
	}, len(c.probes))
	var wg sync.WaitGroup
	for i := range c.probes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			samples, err := c.probes[i].read(ctx)
			if err != nil {
				slog.Warn("admin metric: refresh failed", "metric", c.probes[i].descs[0].String(), "error", err)
				return
			}
			results[i].samples, results[i].ok = samples, true
		}(i)
	}
	wg.Wait()
	c.mu.Lock()
	for i, p := range c.probes {
		p.samples, p.ok = results[i].samples, results[i].ok
	}
	c.mu.Unlock()
}

// WithAdminBusiness adds the admin business-metric gauges. Only for a data plane:
// a control plane runs none of these queries. A call with no non-nil source
// registers nothing. Chainable and nil-safe, like WithRAGBacklog.
func (m *Metrics) WithAdminBusiness(src AdminBusinessSources) *Metrics {
	if m == nil {
		return m
	}
	c := newAdminMetricsCollector(src)
	if len(c.probes) == 0 {
		return m
	}
	m.reg.MustRegister(c)
	return m
}
