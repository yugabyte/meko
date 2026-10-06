package loadtest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	execk8s "github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

// Probe measures what the stack's servers did over a window of load: the CPU
// and memory of every component, the request and latency histograms the
// api-server, MCP server and gateway export, the database's RPCs, and the LLM
// calls. It is best-effort: a source it cannot read is logged and left out.
type Probe struct {
	kube    *execk8s.Kubectl
	dbKube  *execk8s.Kubectl
	llm     *llmmock.Client
	every   time.Duration
	logf    func(string, ...any)
	sources []promSource
}

type promSource struct {
	component string
	kube      *execk8s.Kubectl
	selector  string
	port      int
	path      string
	sumMS     float64
	// keep, when set, is the only families read, and their labels are dropped:
	// the database exports thousands of per-tablet series.
	keep *regexp.Regexp
}

// NewProbe reads perf.server_metrics; off returns nil, which every method accepts.
func NewProbe(r *Rig) *Probe {
	if !r.T.Options.GetBool("perf.server_metrics.enabled", true) {
		return nil
	}
	p := &Probe{
		kube:  r.Stack.Kube(),
		llm:   r.Stack.LLM(),
		every: r.T.Options.GetDuration("perf.server_metrics.sample_interval", 15*time.Second),
		logf:  r.logf,
	}
	if b, err := r.Stack.Backing(r.T); err == nil && b.Namespace != "" {
		p.dbKube = p.kube.InNamespace(b.Namespace)
	}
	for _, c := range []struct{ name, selector string }{
		{"api", "app=meko-api-server"}, {"mcp", "app=meko-mcp-server"}, {"gateway", "app=inference-gateway"},
	} {
		p.sources = append(p.sources, promSource{component: c.name, kube: p.kube, selector: c.selector, port: 9464, path: "metrics", sumMS: 1000})
	}
	if p.dbKube != nil {
		p.sources = append(p.sources, promSource{component: "db", kube: p.dbKube, selector: "app=yugabyted", port: 9000, path: "prometheus-metrics", sumMS: 0.001,
			keep: regexp.MustCompile(`^(handler_latency_yb_tserver_TabletServerService_(Read|Write)|handler_latency_yb_tserver_PgClientService_Perform|log_append_latency|log_sync_latency)_(count|sum)$|^rpc_inbound_calls_created$`)})
	}
	return p
}

// Window is one measured stretch.
type Window struct {
	p      *Probe
	start  time.Time
	prom   map[string]float64
	llm    llmmock.Stats
	stop   context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	usage  map[string][]usagePoint
	errors []string
}

type usagePoint struct {
	cpuMilli float64
	memMiB   float64
}

// Begin snapshots the counters and starts sampling pod usage.
func (p *Probe) Begin(ctx context.Context) *Window {
	if p == nil {
		return nil
	}
	w := &Window{p: p, start: time.Now(), usage: map[string][]usagePoint{}, done: make(chan struct{})}
	w.prom = p.scrape(ctx)
	if p.llm != nil {
		if st, err := p.llm.Stats(ctx); err == nil {
			w.llm = st
		} else {
			p.logf("probe: llm stats: %v", err)
		}
	}
	bg, cancel := context.WithCancel(context.Background())
	w.stop = cancel
	go w.sample(bg)
	return w
}

func (w *Window) sample(ctx context.Context) {
	defer close(w.done)
	tick := time.NewTicker(w.p.every)
	defer tick.Stop()
	for {
		w.sampleOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (w *Window) sampleOnce(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, w.p.every)
	defer cancel()
	per := map[string]usagePoint{}
	for _, k := range []*execk8s.Kubectl{w.p.kube, w.p.dbKube} {
		if k == nil {
			continue
		}
		raw, err := k.GetRaw(cctx, "/apis/metrics.k8s.io/v1beta1/namespaces/"+k.Namespace()+"/pods")
		if err != nil {
			w.mu.Lock()
			if len(w.errors) < 3 {
				w.errors = append(w.errors, err.Error())
			}
			w.mu.Unlock()
			continue
		}
		for pod, u := range parsePodMetrics(raw) {
			c := component(pod)
			cur := per[c]
			cur.cpuMilli += u.cpuMilli
			cur.memMiB += u.memMiB
			per[c] = cur
		}
	}
	w.mu.Lock()
	for c, u := range per {
		w.usage[c] = append(w.usage[c], u)
	}
	w.mu.Unlock()
}

// component names a pod by the workload it belongs to, the same name on every stack.
func component(pod string) string {
	for _, p := range []string{
		"meko-api-server", "meko-mcp-server", "inference-gateway", "rag-agent-text", "rag-agent-pdf", "rag-agent-conversations",
		"langfuse-web-worker", "valkey", "llmmock", "meko-ui", "cognito-mock", "minio", "mekoload", "mekoqa-ysql",
	} {
		if strings.HasPrefix(pod, p) {
			return p
		}
	}
	if strings.HasPrefix(pod, "yb-") {
		return "yugabyted"
	}
	if i := strings.LastIndex(pod, "-"); i > 0 {
		return pod[:i]
	}
	return pod
}

type podUsage struct{ cpuMilli, memMiB float64 }

func parsePodMetrics(raw string) map[string]podUsage {
	var doc struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Containers []struct {
				Usage struct {
					CPU    string `json:"cpu"`
					Memory string `json:"memory"`
				} `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	out := map[string]podUsage{}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return out
	}
	for _, it := range doc.Items {
		var u podUsage
		for _, c := range it.Containers {
			u.cpuMilli += cpuMilli(c.Usage.CPU)
			u.memMiB += memMiB(c.Usage.Memory)
		}
		out[it.Metadata.Name] = u
	}
	return out
}

func cpuMilli(q string) float64 {
	units := []struct {
		suffix string
		scale  float64
	}{{"n", 1e-6}, {"u", 1e-3}, {"m", 1}}
	for _, u := range units {
		if v, ok := strings.CutSuffix(q, u.suffix); ok {
			f, _ := strconv.ParseFloat(v, 64)
			return f * u.scale
		}
	}
	f, _ := strconv.ParseFloat(q, 64)
	return f * 1000
}

func memMiB(q string) float64 {
	units := []struct {
		suffix string
		scale  float64
	}{{"Ki", 1.0 / 1024}, {"Mi", 1}, {"Gi", 1024}, {"K", 1000.0 / (1 << 20)}, {"M", 1e6 / (1 << 20)}, {"G", 1e9 / (1 << 20)}}
	for _, u := range units {
		if v, ok := strings.CutSuffix(q, u.suffix); ok {
			f, _ := strconv.ParseFloat(v, 64)
			return f * u.scale
		}
	}
	f, _ := strconv.ParseFloat(q, 64)
	return f / (1 << 20)
}

// scrape reads every source pod's exposition through the API server's pod proxy
// and keeps the counters and histogram sums and counts, summed over pods.
func (p *Probe) scrape(ctx context.Context) map[string]float64 {
	out := map[string]float64{}
	for _, s := range p.sources {
		pods, err := s.kube.GetSelector(ctx, "pods", s.selector, `{range .items[*]}{.metadata.name}{"\n"}{end}`)
		if err != nil {
			p.logf("probe: list %s: %v", s.selector, err)
			continue
		}
		for _, pod := range strings.Fields(pods) {
			body, err := s.kube.GetRaw(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy/%s", s.kube.Namespace(), pod, s.port, s.path))
			if err != nil {
				p.logf("probe: scrape %s %s: %v", s.component, pod, err)
				continue
			}
			for k, v := range parseExposition(body, s.keep) {
				out[s.component+"|"+k] += v
			}
		}
	}
	return out
}

var (
	sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+([^\s]+)`)
	labelKV    = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)
)

// parseExposition keys each kept sample "name|label values". It keeps counters
// (_total) and the _count and _sum of histograms and summaries, which are what
// a delta over a window means something for.
func parseExposition(body string, keep *regexp.Regexp) map[string]float64 {
	out := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if keep != nil {
			if !keep.MatchString(name) {
				continue
			}
			v, err := strconv.ParseFloat(m[3], 64)
			if err == nil && !math.IsNaN(v) {
				if !strings.HasSuffix(name, "_count") && !strings.HasSuffix(name, "_sum") {
					name += "_total"
				}
				out[name+"|"] += v
			}
			continue
		}
		if !strings.HasSuffix(name, "_total") && !strings.HasSuffix(name, "_count") && !strings.HasSuffix(name, "_sum") {
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil || math.IsNaN(v) {
			continue
		}
		var vals []string
		for _, kv := range labelKV.FindAllStringSubmatch(m[2], -1) {
			if kv[1] == "le" || kv[1] == "quantile" || kv[1] == "exported_instance" {
				continue
			}
			vals = append(vals, kv[1]+"="+kv[2])
		}
		sort.Strings(vals)
		out[name+"|"+strings.Join(vals, ",")] += v
	}
	return out
}

// ServerReport is what the servers did over a window.
type ServerReport struct {
	Seconds float64
	// Usage is per component: mean and peak CPU in millicores, peak memory in MiB.
	Usage map[string]Usage
	// Server is per "component family labels": requests per second and, for a
	// histogram, mean latency in ms.
	Server map[string]ServerSeries
	// LLM is calls per route over the window.
	LLM    map[string]int
	Errors []string
}

type Usage struct{ CPUAvg, CPUMax, MemMax float64 }

type ServerSeries struct {
	Count  float64
	Rate   float64
	MeanMS float64
}

// End stops sampling and computes the window's report.
func (w *Window) End(ctx context.Context) ServerReport {
	if w == nil {
		return ServerReport{}
	}
	w.stop()
	<-w.done
	secs := time.Since(w.start).Seconds()
	rep := ServerReport{Seconds: secs, Usage: map[string]Usage{}, Server: map[string]ServerSeries{}, LLM: map[string]int{}}
	w.mu.Lock()
	for c, pts := range w.usage {
		var u Usage
		for _, pt := range pts {
			u.CPUAvg += pt.cpuMilli
			u.CPUMax = max(u.CPUMax, pt.cpuMilli)
			u.MemMax = max(u.MemMax, pt.memMiB)
		}
		u.CPUAvg /= float64(len(pts))
		rep.Usage[c] = u
	}
	rep.Errors = append(rep.Errors, w.errors...)
	w.mu.Unlock()

	end := w.p.scrape(ctx)
	sumMS := map[string]float64{}
	for _, s := range w.p.sources {
		sumMS[s.component] = s.sumMS
	}
	for k, v := range end {
		d := v - w.prom[k]
		if d <= 0 {
			continue
		}
		comp, rest, _ := strings.Cut(k, "|")
		name, labels, _ := strings.Cut(rest, "|")
		switch {
		case strings.HasSuffix(name, "_count"):
			base := strings.TrimSuffix(name, "_count")
			key := comp + " " + base + " " + labels
			s := rep.Server[key]
			s.Count, s.Rate = d, d/secs
			sum := end[comp+"|"+base+"_sum|"+labels] - w.prom[comp+"|"+base+"_sum|"+labels]
			s.MeanMS = sum * sumMS[comp] / d
			rep.Server[key] = s
		case strings.HasSuffix(name, "_total"):
			key := comp + " " + name + " " + labels
			rep.Server[key] = ServerSeries{Count: d, Rate: d / secs}
		}
	}
	if w.p.llm != nil {
		if st, err := w.p.llm.Stats(ctx); err == nil {
			for r, n := range st.Routes {
				if d := n - w.llm.Routes[r]; d > 0 {
					rep.LLM[r] = d
				}
			}
		}
	}
	return rep
}

// Emit records a window as datapoints under prefix: CPU and memory per
// component, CPU and LLM calls per successful op, and every server series that
// saw at least minCount events. ops is the load's successful calls over the
// same window.
func (rep ServerReport) Emit(t *core.T, prefix string, ops int64, minCount int64) {
	if rep.Seconds == 0 {
		return
	}
	dp := func(name string, v float64, unit string, dir core.Direction, n int) {
		t.Datapoint(core.Datapoint{Name: prefix + "." + name, Value: round3(v), Unit: unit, Direction: dir, N: max(n, 1)})
	}
	var cpuTotal float64
	for _, c := range sortedKeys(rep.Usage) {
		u := rep.Usage[c]
		if c == "mekoload" {
			t.Metric(prefix+".res.client.cpu_avg_mcores", round3(u.CPUAvg), "mcores")
			continue
		}
		cpuTotal += u.CPUAvg
		dp("res."+c+".cpu_avg_mcores", u.CPUAvg, "mcores", core.LowerIsBetter, 1)
		dp("res."+c+".cpu_max_mcores", u.CPUMax, "mcores", core.LowerIsBetter, 1)
		dp("res."+c+".mem_max_mib", u.MemMax, "MiB", core.LowerIsBetter, 1)
	}
	if ops > 0 && cpuTotal > 0 {
		// millicores x seconds / ops = CPU milliseconds the stack spent per op.
		dp("res.stack.cpu_ms_per_op", cpuTotal*rep.Seconds/float64(ops), "ms", core.LowerIsBetter, int(min(ops, 1<<30)))
	}
	for _, r := range sortedKeys(rep.LLM) {
		n := rep.LLM[r]
		if ops > 0 {
			dp("llm."+r+".calls_per_op", float64(n)/float64(ops), "calls", core.LowerIsBetter, n)
		}
		dp("llm."+r+".calls_per_s", float64(n)/rep.Seconds, "calls/s", core.LowerIsBetter, n)
	}
	for _, k := range sortedKeys(rep.Server) {
		s := rep.Server[k]
		if int64(s.Count) < minCount {
			continue
		}
		name := "srv." + sanitize(k)
		dp(name+".rate", s.Rate, "/s", core.HigherIsBetter, int(s.Count))
		if s.MeanMS > 0 {
			dp(name+".mean_ms", s.MeanMS, "ms", core.LowerIsBetter, int(s.Count))
		}
	}
	for i, e := range rep.Errors {
		t.Metric(fmt.Sprintf("%s.probe_error_%d", prefix, i), 1, e)
	}
}

func sanitize(s string) string {
	s = strings.NewReplacer(" ", ".", "|", ".", ",", ";").Replace(strings.TrimSpace(s))
	return strings.Trim(s, ".")
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
