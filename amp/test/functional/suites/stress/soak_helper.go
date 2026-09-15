package stress

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pageserver"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	DefaultSoakDuration         = 8 * time.Hour
	DefaultSoakTenants          = 3
	DefaultSoakPayloadBytes     = 256
	DefaultSoakChurnInterval    = 10 * time.Minute
	DefaultSoakSampleInterval   = 30 * time.Second
	DefaultSoakMaxSamples       = 4000
	DefaultSoakRSSSlopeMiB      = 50.0
	DefaultSoakRSSFitFraction   = 0.75
	DefaultSoakWalsGrowthFactor = 2.0
	DefaultSoakWalsBaselineAt   = time.Hour
	DefaultSoakGCLagMonotonic   = 30 * time.Minute
	DefaultSoakChainLimit       = 20.0
	DefaultSoakChainSlack       = 5.0
	DefaultSoakChainFraction    = 0.95
	DefaultSoakLatencyFactor    = 2.0
	DefaultSoakLatencyBucket    = 10 * time.Minute
	DefaultSoakLatencyProbe     = time.Second
	DefaultSoakMaxTasks         = 20000
	DefaultSoakRSSWarmup        = 30 * time.Minute
	DefaultSoakMinFitSamples    = 10
	DefaultSoakThreadsRead      = 1
	DefaultSoakThreadsWrite     = 2
	DefaultSoakBankAccounts     = 200
	DefaultSoakClientImage      = "eclipse-temurin:21-jdk"
	DefaultSoakRemoteDir        = "/opt/ampqa"
	DefaultSoakJarSecret        = "gh_package"
	DefaultSoakSettleTimeout    = 10 * time.Minute
)

// The pod also runs a postgres-exporter sidecar, so the container must be named.
const soakEndpointContainer = "endpoint"

type soakConfig struct {
	Enabled             bool
	Duration            time.Duration
	Tenants             int
	PayloadBytes        int
	ChurnInterval       time.Duration
	SampleInterval      time.Duration
	MaxSamples          int
	RSSSlopeMiBPerHour  float64
	RSSFitFraction      float64
	WalsGrowthFactor    float64
	WalsBaselineAfter   time.Duration
	GCLagMonotonicLimit time.Duration
	ChainLimit          float64
	ChainLimitSlack     float64
	ChainSampleFraction float64
	LatencyGrowthFactor float64
	LatencyBucket       time.Duration
	LatencyProbe        time.Duration
	MaxTasks            int
	RSSWarmup           time.Duration
	MinFitSamples       int
	ThreadsRead         int
	ThreadsWrite        int
	BankAccounts        int
	ClientImage         string
	ClientPullSecret    string
	RemoteDir           string
	JarSecret           string
	SettleTimeout       time.Duration
	Creds               endpointCreds
}

func soakFromConfig(c *core.Config) soakConfig {
	const p = "stress.soak."
	return soakConfig{
		Enabled:             c.GetBool(p+"enabled", true),
		Duration:            c.GetDuration(p+"duration", DefaultSoakDuration),
		Tenants:             c.GetInt(p+"tenants", DefaultSoakTenants),
		PayloadBytes:        c.GetInt(p+"payload_bytes", DefaultSoakPayloadBytes),
		ChurnInterval:       c.GetDuration(p+"churn_interval", DefaultSoakChurnInterval),
		SampleInterval:      c.GetDuration(p+"sample_interval", DefaultSoakSampleInterval),
		MaxSamples:          c.GetInt(p+"max_samples", DefaultSoakMaxSamples),
		RSSSlopeMiBPerHour:  c.GetFloat(p+"rss_slope_mib_per_hour", DefaultSoakRSSSlopeMiB),
		RSSFitFraction:      c.GetFloat(p+"rss_fit_fraction", DefaultSoakRSSFitFraction),
		WalsGrowthFactor:    c.GetFloat(p+"wals_growth_factor", DefaultSoakWalsGrowthFactor),
		WalsBaselineAfter:   c.GetDuration(p+"wals_baseline_after", DefaultSoakWalsBaselineAt),
		GCLagMonotonicLimit: c.GetDuration(p+"gc_lag_monotonic_limit", DefaultSoakGCLagMonotonic),
		ChainLimit:          c.GetFloat(p+"chain_limit", DefaultSoakChainLimit),
		ChainLimitSlack:     c.GetFloat(p+"chain_limit_slack", DefaultSoakChainSlack),
		ChainSampleFraction: c.GetFloat(p+"chain_sample_fraction", DefaultSoakChainFraction),
		LatencyGrowthFactor: c.GetFloat(p+"latency_growth_factor", DefaultSoakLatencyFactor),
		LatencyBucket:       c.GetDuration(p+"latency_bucket", DefaultSoakLatencyBucket),
		LatencyProbe:        c.GetDuration(p+"latency_probe_interval", DefaultSoakLatencyProbe),
		MaxTasks:            c.GetInt(p+"max_tasks", DefaultSoakMaxTasks),
		RSSWarmup:           c.GetDuration(p+"rss_warmup", DefaultSoakRSSWarmup),
		MinFitSamples:       c.GetInt(p+"min_fit_samples", DefaultSoakMinFitSamples),
		ThreadsRead:         c.GetInt(p+"workload_threads_read", DefaultSoakThreadsRead),
		ThreadsWrite:        c.GetInt(p+"workload_threads_write", DefaultSoakThreadsWrite),
		BankAccounts:        c.GetInt(p+"bank_accounts", DefaultSoakBankAccounts),
		ClientImage:         c.GetString(p+"client_image", DefaultSoakClientImage),
		ClientPullSecret:    c.GetString(p+"client_pull_secret", ""),
		RemoteDir:           c.GetString(p+"remote_dir", DefaultSoakRemoteDir),
		JarSecret:           c.GetString(p+"jar_secret", DefaultSoakJarSecret),
		SettleTimeout:       c.GetDuration(p+"settle_timeout", DefaultSoakSettleTimeout),
		Creds: endpointCreds{
			User:     c.GetString("functional.amp.endpoint_user", "cloud_admin"),
			Password: c.GetString("functional.amp.endpoint_password", "cloud_admin"),
			Database: c.GetString("functional.amp.endpoint_db", "postgres"),
		},
	}
}

func soakGate(cfg soakConfig) (string, bool) {
	if !cfg.Enabled {
		return "stress.soak.enabled is false: the scenario was turned off for this run", false
	}
	if cfg.Tenants < 3 {
		return fmt.Sprintf("stress.soak.tenants is %d: the soak needs the steady, bank and churn tenants", cfg.Tenants), false
	}
	if cfg.SampleInterval <= 0 || cfg.LatencyBucket <= 0 || cfg.LatencyProbe <= 0 {
		return fmt.Sprintf("stress.soak sample_interval %s, latency_bucket %s and latency_probe_interval %s must all be positive", cfg.SampleInterval, cfg.LatencyBucket, cfg.LatencyProbe), false
	}
	if cfg.Duration < cfg.SampleInterval*4 {
		return fmt.Sprintf("stress.soak.duration %s leaves fewer than four samples at stress.soak.sample_interval %s: no trend can be fitted", cfg.Duration, cfg.SampleInterval), false
	}
	return "", true
}

type soakPoint struct {
	At    time.Time
	Value float64
}

type soakSeries struct {
	Name  string
	Unit  string
	Lines map[string][]soakPoint
}

type soakSample struct {
	Series string
	Line   string
	Value  float64
}

type soakProbe struct {
	Name string
	Fn   func(context.Context) ([]soakSample, error)
}

type soakCollector struct {
	interval time.Duration
	max      int
	units    map[string]string
	probes   []soakProbe
	logf     func(string, ...any)

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	mu     sync.Mutex
	series map[string]*soakSeries
	order  []string
}

func newSoakCollector(interval time.Duration, max int, units map[string]string, logf func(string, ...any)) *soakCollector {
	return &soakCollector{interval: interval, max: max, units: units, logf: logf, series: map[string]*soakSeries{}}
}

func (c *soakCollector) Add(name string, fn func(context.Context) ([]soakSample, error)) {
	c.probes = append(c.probes, soakProbe{Name: name, Fn: fn})
}

// Rooted in context.Background(), released only by Stop (AGENTS.md §5).
func (c *soakCollector) Start() {
	bg, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done = make(chan struct{})
	go c.loop(bg)
}

func (c *soakCollector) loop(ctx context.Context) {
	defer close(c.done)
	tick := time.NewTicker(c.interval)
	defer tick.Stop()
	c.sample(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.sample(ctx)
		}
	}
}

func (c *soakCollector) sample(ctx context.Context) {
	for _, p := range c.probes {
		pctx, cancel := context.WithTimeout(ctx, c.interval)
		samples, err := p.Fn(pctx)
		cancel()
		if err != nil {
			c.logf("soak: probe %s (best-effort): %v", p.Name, err)
			continue
		}
		now := time.Now()
		c.mu.Lock()
		for _, s := range samples {
			ser, ok := c.series[s.Series]
			if !ok {
				ser = &soakSeries{Name: s.Series, Unit: c.units[s.Series], Lines: map[string][]soakPoint{}}
				c.series[s.Series] = ser
				c.order = append(c.order, s.Series)
			}
			pts := append(ser.Lines[s.Line], soakPoint{At: now, Value: s.Value})
			if len(pts) > c.max {
				pts = pts[len(pts)-c.max:]
			}
			ser.Lines[s.Line] = pts
		}
		c.mu.Unlock()
	}
}

func (c *soakCollector) Stop() []soakSeries {
	var out []soakSeries
	c.once.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		if c.done != nil {
			<-c.done
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, name := range c.order {
			out = append(out, *c.series[name])
		}
	})
	return out
}

func soakJSONL(series []soakSeries) []byte {
	var b strings.Builder
	for _, ser := range series {
		for line, pts := range ser.Lines {
			for _, p := range pts {
				rec, err := json.Marshal(struct {
					T      string  `json:"t"`
					Series string  `json:"series"`
					Line   string  `json:"line"`
					Value  float64 `json:"value"`
					Unit   string  `json:"unit"`
				}{p.At.UTC().Format(time.RFC3339), ser.Name, line, p.Value, ser.Unit})
				if err != nil {
					continue
				}
				b.Write(rec)
				b.WriteByte('\n')
			}
		}
	}
	return []byte(b.String())
}

// Pods are re-resolved every tick: an endpoint restarted mid-soak comes back under a new name.
func soakPageserverProbe(kube *k8s.Kubectl, tenants []tenant) func(context.Context) ([]soakSample, error) {
	type chainState struct{ sum, count float64 }
	prev := map[string]chainState{}
	return func(ctx context.Context) ([]soakSample, error) {
		var out []soakSample
		var failed []string
		for i := range tenants {
			id := tenants[i].Endpoint.ID
			if id == "" {
				continue
			}
			sel := "app.kubernetes.io/instance=" + ampstack.EndpointWorkloadName(id)
			names, err := kube.GetSelector(ctx, "pod", sel, "{.items[*].metadata.name}")
			if err != nil || strings.TrimSpace(names) == "" {
				failed = append(failed, fmt.Sprintf("tenant %d: no pod for %s", i, sel))
				continue
			}
			pod := strings.Fields(names)[0]
			ps := pageserver.New(soakPodExec(kube, pod), pageserver.DefaultHTTPPort)
			samples, err := ps.Metrics(ctx)
			if err != nil {
				failed = append(failed, fmt.Sprintf("tenant %d (%s): %v", i, pod, err))
				continue
			}
			line := fmt.Sprintf("tenant %d", i)
			if v, ok := samples.Value("process_resident_memory_bytes", nil); ok {
				out = append(out, soakSample{soakSeriesRSS, line, v / (1 << 20)})
			}
			if v, ok := samples.Value("pageserver_yb_gc_drain_lag_bytes", nil); ok {
				out = append(out, soakSample{soakSeriesGCLag, line, v})
			}
			sum, okSum := samples.Value("pageserver_deltas_per_read_global_sum", nil)
			count, okCount := samples.Value("pageserver_deltas_per_read_global_count", nil)
			if okSum && okCount {
				last := prev[line]
				prev[line] = chainState{sum: sum, count: count}
				if d := count - last.count; d > 0 {
					out = append(out, soakSample{soakSeriesChain, line, (sum - last.sum) / d})
				}
			}
		}
		if len(out) == 0 && len(failed) > 0 {
			return nil, fmt.Errorf("no endpoint pageserver answered: %s", strings.Join(failed, "; "))
		}
		return out, nil
	}
}

func soakPodExec(kube *k8s.Kubectl, pod string) pageserver.ExecFunc {
	return func(ctx context.Context, script string) (string, error) {
		res, err := kube.ForPod(pod, soakEndpointContainer).Run(ctx, script)
		if err != nil {
			return "", fmt.Errorf("exec in pod %s: %w", pod, err)
		}
		if res.ExitCode != 0 {
			return "", fmt.Errorf("pageserver request in pod %s exited %d: %s", pod, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		return res.Stdout, nil
	}
}

func soakWalsProbe(storage *pg.Client) func(context.Context) ([]soakSample, error) {
	return func(ctx context.Context) ([]soakSample, error) {
		n, err := storage.QueryInt(ctx, "SELECT count(*) FROM amp_storage.wals")
		if err != nil {
			return nil, err
		}
		return []soakSample{{soakSeriesWals, "backing", float64(n)}}, nil
	}
}

func soakTasksProbe(cl *controller.Client) func(context.Context) ([]soakSample, error) {
	return func(ctx context.Context) ([]soakSample, error) {
		list, err := cl.ListTasks(ctx, controller.ListTasksOptions{Limit: 1})
		if err != nil {
			return nil, err
		}
		return []soakSample{{soakSeriesTasks, "controller", float64(list.Total)}}, nil
	}
}

const (
	soakSeriesRSS     = "pageserver_rss"
	soakSeriesGCLag   = "gc_drain_lag"
	soakSeriesChain   = "delta_chain_length"
	soakSeriesWals    = "wals_rows"
	soakSeriesTasks   = "controller_tasks"
	soakSeriesLatency = "steady_p99_latency"
)

func soakSeriesUnits() map[string]string {
	return map[string]string{
		soakSeriesRSS:     "MiB",
		soakSeriesGCLag:   "bytes",
		soakSeriesChain:   "deltas",
		soakSeriesWals:    "rows",
		soakSeriesTasks:   "rows",
		soakSeriesLatency: "ms",
	}
}

type soakLatency struct {
	bucket time.Duration
	cap    int
	start  time.Time

	mu      sync.Mutex
	buckets map[int][]float64
	errs    int
}

func newSoakLatency(bucket time.Duration, cap int, start time.Time) *soakLatency {
	return &soakLatency{bucket: bucket, cap: cap, start: start, buckets: map[int][]float64{}}
}

func (l *soakLatency) record(at time.Time, d time.Duration) {
	idx := int(at.Sub(l.start) / l.bucket)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets[idx]) < l.cap {
		l.buckets[idx] = append(l.buckets[idx], float64(d)/float64(time.Millisecond))
	}
}

func (l *soakLatency) recordErr() {
	l.mu.Lock()
	l.errs++
	l.mu.Unlock()
}

func (l *soakLatency) p99s(minSamples int) []float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	idxs := make([]int, 0, len(l.buckets))
	for i := range l.buckets {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	var out []float64
	for _, i := range idxs {
		if len(l.buckets[i]) < minSamples {
			continue
		}
		out = append(out, percentile(l.buckets[i], 0.99))
	}
	return out
}

func percentile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

func slopePerHour(pts []soakPoint) float64 {
	if len(pts) < 2 {
		return 0
	}
	t0 := pts[0].At
	var sx, sy, sxy, sxx float64
	n := float64(len(pts))
	for _, p := range pts {
		x := p.At.Sub(t0).Hours()
		sx += x
		sy += p.Value
		sxy += x * p.Value
		sxx += x * x
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

func tail(pts []soakPoint, fraction float64) []soakPoint {
	if len(pts) < 2 || fraction <= 0 || fraction >= 1 {
		return pts
	}
	span := pts[len(pts)-1].At.Sub(pts[0].At)
	from := pts[len(pts)-1].At.Add(-time.Duration(float64(span) * fraction))
	for i, p := range pts {
		if !p.At.Before(from) {
			return pts[i:]
		}
	}
	return pts[len(pts)-1:]
}

func valueAt(pts []soakPoint, d time.Duration) (float64, bool) {
	if len(pts) == 0 {
		return 0, false
	}
	from := pts[0].At.Add(d)
	for _, p := range pts {
		if !p.At.Before(from) {
			return p.Value, true
		}
	}
	return 0, false
}

func longestRisingRun(pts []soakPoint) float64 {
	var longest time.Duration
	runStart := 0
	for i := 1; i < len(pts); i++ {
		if pts[i].Value <= pts[i-1].Value {
			runStart = i
			continue
		}
		if d := pts[i].At.Sub(pts[runStart].At); d > longest {
			longest = d
		}
	}
	return longest.Seconds()
}

func after(pts []soakPoint, d time.Duration) []soakPoint {
	if len(pts) == 0 || d <= 0 {
		return pts
	}
	from := pts[0].At.Add(d)
	for i, p := range pts {
		if !p.At.Before(from) {
			return pts[i:]
		}
	}
	return nil
}

func lastValue(pts []soakPoint) float64 {
	if len(pts) == 0 {
		return 0
	}
	return pts[len(pts)-1].Value
}

func sortedLines(ser soakSeries) []string {
	lines := make([]string, 0, len(ser.Lines))
	for line := range ser.Lines {
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return lines
}

func maxOverLines(ser soakSeries, of func([]soakPoint) float64) (string, float64, bool) {
	var bestLine string
	var best float64
	var found bool
	for _, line := range sortedLines(ser) {
		v := of(ser.Lines[line])
		if !found || v > best {
			bestLine, best, found = line, v, true
		}
	}
	return bestLine, best, found
}

func belowLimitFraction(ser soakSeries, limit float64) (float64, int) {
	var ok, total int
	for _, pts := range ser.Lines {
		for _, p := range pts {
			total++
			if p.Value <= limit {
				ok++
			}
		}
	}
	if total == 0 {
		return 0, 0
	}
	return float64(ok) / float64(total), total
}

func findSeries(series []soakSeries, name string) (soakSeries, bool) {
	for _, s := range series {
		if s.Name == name {
			return s, true
		}
	}
	return soakSeries{}, false
}
