// Package metrics is the background server-metrics sampler: a composable app
// that polls the pods behind a test while the test runs, and hands back a
// per-pod time series per metric on stop.
//
// It is universal — any test holding a namespace-scoped kubectl can start one —
// and it never fails its caller: every probe error is logged and skipped, so a
// pod that disappears mid-run simply stops contributing points. Storage is
// capped per line (drop-oldest), so a long soak cannot grow it without bound.
//
// Every emitted key is "[<section>] <human title>". The raw prometheus names
// behind a series travel alongside it in Series.Raw, never in the key: a reader
// gets "[yb tserver] Write latency", and the machinery that produced it is still
// one field away.
package metrics

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// Defaults for apps.metrics.* (mirrored in configs/default.yaml). Collection is
// ON by default: a test that does not compose the app is simply unaffected.
// MaxPoints is generous — a 15s interval over a 1h run is 240 points per line.
const (
	DefaultEnabled        = true
	DefaultSampleInterval = 15 * time.Second
	DefaultMaxPoints      = 2000
	DefaultTServerPort    = 9000
	DefaultMasterPort     = 7000
	DefaultEndpointPort   = 9898
)

// Entry kinds: how an entry's raw values become a series.
const (
	// KindGauge passes the scraped value through (scaled).
	KindGauge = "gauge"
	// KindCounter reports the per-second rate of the value's delta.
	KindCounter = "counter"
	// KindLatency matches a prometheus histogram's _count and _sum pair and
	// derives TWO series from it: rate(count) as throughput, and
	// delta(sum)/delta(count) as the mean latency, converted to milliseconds.
	KindLatency = "latency"
)

// latencyRound is the display precision of a derived mean latency: sub-
// millisecond page/RPC latencies are the normal case, so 2 decimals would round
// most of them to the same number.
const latencyRound = 3

// Entry is one collected metric declared in apps.metrics.collect: a regex over
// the bare prometheus name plus the display metadata the emitted key and unit
// come from. Raw prometheus names never reach a key — Title does.
type Entry struct {
	// Match is a regex matched against the bare metric name (labels stripped).
	Match string
	// Title is the human name. The emitted key is "[<section>] <Title>"; for a
	// latency entry it names the THROUGHPUT series.
	Title string
	// Kind is gauge (default), counter, or latency.
	Kind string
	// Unit is the display unit. Defaults to "/s" for counter and latency.
	Unit string
	// DivideBy divides the emitted value (e.g. 1048576 for bytes → MiB). Zero
	// means 1. A divisor rather than a multiplier so the config stays readable
	// and exact — "1048576", not "0.00000095367431640625".
	DivideBy float64
	// Round is the display precision. Zero means 2.
	Round int
	// LatencyTitle names a latency entry's derived mean-latency series.
	// Defaults to Title + " latency".
	LatencyTitle string
	// SumUnit is the time unit of the histogram's _sum, converted to ms:
	// ns, us, ms or s. Defaults to us (YB handler latencies are microseconds).
	SumUnit string
}

// DefaultCollect is the yb-tserver / yb-master metric set kept when
// apps.metrics.collect is unset. Read/write RPC throughput and mean latency, WAL
// append/sync throughput and latency, WAL volume, compaction and flush volume,
// RPC queue depth and inbound call rate, SST footprint and the root memory
// tracker.
//
// cpu_utime / cpu_stime are deliberately NOT here: the cgroup probe already
// emits "[<role>] CPU" in cores for the whole container, which is the same
// quantity measured better (it counts every thread, not just the ones PG-style
// accounting attributes) — two CPU series in different units would only invite
// the reader to reconcile them.
var DefaultCollect = []Entry{
	{
		Match: `^handler_latency_yb_tserver_TabletServerService_Read_(count|sum)$`,
		Title: "Read RPCs", Kind: KindLatency, LatencyTitle: "Read latency", SumUnit: "us",
	},
	{
		Match: `^handler_latency_yb_tserver_TabletServerService_Write_(count|sum)$`,
		Title: "Write RPCs", Kind: KindLatency, LatencyTitle: "Write latency", SumUnit: "us",
	},
	{
		Match: `^log_append_latency_(count|sum)$`,
		Title: "WAL appends", Kind: KindLatency, LatencyTitle: "WAL append latency", SumUnit: "us",
	},
	{
		Match: `^log_sync_latency_(count|sum)$`,
		Title: "WAL syncs", Kind: KindLatency, LatencyTitle: "WAL sync latency", SumUnit: "us",
	},
	{Match: `^log_bytes_logged$`, Title: "WAL bytes", Kind: KindCounter, Unit: "MiB/s", DivideBy: 1 << 20},
	{Match: `^rocksdb_compact_read_bytes$`, Title: "Compactions read", Kind: KindCounter, Unit: "MiB/s", DivideBy: 1 << 20},
	{Match: `^rocksdb_compact_write_bytes$`, Title: "Compactions written", Kind: KindCounter, Unit: "MiB/s", DivideBy: 1 << 20},
	{Match: `^rocksdb_flush_write_bytes$`, Title: "Flushes written", Kind: KindCounter, Unit: "MiB/s", DivideBy: 1 << 20},
	{Match: `^rpc_inbound_calls_created$`, Title: "Inbound RPCs", Kind: KindCounter},
	{Match: `^rpcs_in_queue_yb_(tserver|master)_`, Title: "RPC queue depth", Unit: "rpcs"},
	{Match: `^total_sst_files_size$`, Title: "SST files size", Unit: "MiB", DivideBy: 1 << 20},
	{Match: `^mem_tracker$`, Title: "Memory tracker", Unit: "MiB", DivideBy: 1 << 20},
}

// DefaultEndpointCollect is the amp compute endpoint's pageserver metric set
// (its HTTP /metrics, verified in compute/pageserver/src/http/routes.rs). The
// endpoint pod also contributes cgroup CPU and Memory, which need no entry.
var DefaultEndpointCollect = []Entry{
	{
		Match: `^pageserver_smgr_query_seconds_(count|sum)$`,
		Title: "Page requests", Kind: KindLatency, LatencyTitle: "Page request latency", SumUnit: "s",
	},
	{
		Match: `^pageserver_wait_lsn_seconds_(count|sum)$`,
		Title: "LSN waits", Kind: KindLatency, LatencyTitle: "LSN wait latency", SumUnit: "s",
	},
	{Match: `^pageserver_replayed_wal_records_total$`, Title: "WAL records replayed", Kind: KindCounter},
	{Match: `^pageserver_io_operations_bytes_total$`, Title: "Pageserver I/O", Kind: KindCounter, Unit: "MiB/s", DivideBy: 1 << 20},
	{Match: `^pageserver_resident_physical_size$`, Title: "Resident layer size", Unit: "MiB", DivideBy: 1 << 20},
}

// Config is the resolved apps.metrics.* configuration.
type Config struct {
	Enabled         bool
	SampleInterval  time.Duration
	MaxPoints       int
	Collect         []Entry
	EndpointCollect []Entry
	TServerPort     int
	MasterPort      int
	EndpointPort    int
}

// Getter is the config surface this app reads (satisfied by *core.Config).
type Getter interface {
	GetBool(key string, def bool) bool
	GetInt(key string, def int) int
	GetDuration(key string, def time.Duration) time.Duration
	GetMapSlice(key string, def []map[string]any) []map[string]any
}

// FromConfig reads apps.metrics.* into a Config.
func FromConfig(get Getter) Config {
	return Config{
		Enabled:         get.GetBool("apps.metrics.enabled", DefaultEnabled),
		SampleInterval:  get.GetDuration("apps.metrics.sample_interval", DefaultSampleInterval),
		MaxPoints:       get.GetInt("apps.metrics.max_points", DefaultMaxPoints),
		Collect:         entriesFromConfig(get, "apps.metrics.collect", DefaultCollect),
		EndpointCollect: entriesFromConfig(get, "apps.metrics.endpoint_collect", DefaultEndpointCollect),
		TServerPort:     get.GetInt("apps.metrics.tserver_port", DefaultTServerPort),
		MasterPort:      get.GetInt("apps.metrics.master_port", DefaultMasterPort),
		EndpointPort:    get.GetInt("apps.metrics.endpoint_port", DefaultEndpointPort),
	}
}

// entriesFromConfig decodes a configured list of entry maps. An element without
// a match or a title cannot produce a key and is dropped.
func entriesFromConfig(get Getter, key string, def []Entry) []Entry {
	raw := get.GetMapSlice(key, nil)
	if raw == nil {
		return def
	}
	out := make([]Entry, 0, len(raw))
	for _, m := range raw {
		e := Entry{
			Match:        mapString(m, "match"),
			Title:        mapString(m, "title"),
			Kind:         mapString(m, "kind"),
			Unit:         mapString(m, "unit"),
			DivideBy:     mapFloat(m, "divide_by"),
			Round:        int(mapFloat(m, "round")),
			LatencyTitle: mapString(m, "latency_title"),
			SumUnit:      mapString(m, "sum_unit"),
		}
		if e.Match == "" || e.Title == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func mapString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func mapFloat(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

// Point is one sample: a wall-clock millisecond timestamp and a value.
type Point struct {
	UnixMS int64
	Value  float64
}

// Series is one metric's collected data — a line per sampled pod. Round is the
// display precision (decimal places) a reporter should render the values at. Raw
// names the prometheus series the values were derived from, so the human key
// stays readable without losing what produced it.
type Series struct {
	Key   string
	Unit  string
	Round int
	Raw   []string
	// Desc is the server's own # HELP text for the metric, when the exposition
	// carried one — the product describing itself beats us paraphrasing it.
	Desc  string
	Lines map[string][]Point
}

// Average is the mean across every point of every line — the single scalar a
// reader that only understands one-value metrics sees.
func (s Series) Average() float64 {
	var sum float64
	var n int
	for _, pts := range s.Lines {
		for _, p := range pts {
			sum += p.Value
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// Pairs renders the series as [[unix_ms, value], …] per line — the wire shape a
// reporter sink forwards verbatim.
func (s Series) Pairs() map[string][][2]float64 {
	out := make(map[string][][2]float64, len(s.Lines))
	for line, pts := range s.Lines {
		pairs := make([][2]float64, 0, len(pts))
		for _, p := range pts {
			pairs = append(pairs, [2]float64{float64(p.UnixMS), p.Value})
		}
		out[line] = pairs
	}
	return out
}

// role is one server kind a sampled workload exposes: its pod label selector,
// its container name, the HTTP port and path serving prometheus exposition, the
// entries collected from it, and the tag that prefixes every metric key it
// contributes.
type role struct {
	tag       string
	selector  string
	container string
	port      int
	path      string
	entries   []compiledEntry
}

// ybRoles are the two roles a YugabyteDB-in-k8s backing exposes.
func ybRoles(cfg Config) []role {
	entries := compileEntries(cfg.Collect)
	return []role{
		{tag: "yb tserver", selector: "app=yb-tserver", container: "yb-tserver", port: cfg.TServerPort, path: ybPromPath, entries: entries},
		{tag: "yb master", selector: "app=yb-master", container: "yb-master", port: cfg.MasterPort, path: ybPromPath, entries: entries},
	}
}

// endpointRole is the amp compute endpoint's pod. The container is named
// explicitly: the pod also runs a postgres-exporter sidecar, and an exec that
// does not say which container is answered by whichever one k8s picks.
func endpointRole(cfg Config, selector string) role {
	return role{
		tag: "endpoint", selector: selector, container: "endpoint",
		port: cfg.EndpointPort, path: pageserverPromPath, entries: compileEntries(cfg.EndpointCollect),
	}
}

// target is one sampled pod.
type target struct {
	pod  string
	role role
}

// Sampler polls a set of pods on an interval until stopped.
type Sampler struct {
	kube  *k8s.Kubectl
	cfg   Config
	roles []role
	logf  func(string, ...any)

	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once

	mu    sync.Mutex
	store map[string]*series
	keys  []string
}

// series accumulates one metric's lines in arrival order.
type series struct {
	unit  string
	round int
	raw   []string
	desc  string
	lines map[string]*ring
}

// New builds a Sampler over the YB pods in an already namespace-scoped kubectl.
func New(kube *k8s.Kubectl, cfg Config) *Sampler {
	return newSampler(kube, cfg, ybRoles(cfg))
}

// NewEndpoint builds a Sampler over the amp compute endpoint pods matching
// selector (the endpoint Deployment's app.kubernetes.io/instance label).
func NewEndpoint(kube *k8s.Kubectl, cfg Config, selector string) *Sampler {
	return newSampler(kube, cfg, []role{endpointRole(cfg, selector)})
}

func newSampler(kube *k8s.Kubectl, cfg Config, roles []role) *Sampler {
	if cfg.SampleInterval <= 0 {
		cfg.SampleInterval = DefaultSampleInterval
	}
	if cfg.MaxPoints <= 0 {
		cfg.MaxPoints = DefaultMaxPoints
	}
	return &Sampler{kube: kube, cfg: cfg, roles: roles, logf: func(string, ...any) {}, store: map[string]*series{}}
}

// WithLogf routes the sampler's best-effort diagnostics through f.
func (s *Sampler) WithLogf(f func(string, ...any)) *Sampler {
	if f != nil {
		s.logf = f
	}
	return s
}

// Start discovers the target pods (bounded by ctx) and launches sampling. The
// sampling goroutine is rooted in context.Background(), not ctx: it must outlive
// the call that started it and is released solely by Stop (AGENTS.md §5).
func (s *Sampler) Start(ctx context.Context) error {
	targets := s.discover(ctx)
	if len(targets) == 0 {
		return fmt.Errorf("no pods matched %s in namespace %q", s.selectors(), s.kube.Namespace())
	}
	bg, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.loop(bg, targets)
	return nil
}

func (s *Sampler) selectors() string {
	sels := make([]string, 0, len(s.roles))
	for _, r := range s.roles {
		sels = append(sels, r.selector)
	}
	return strings.Join(sels, " / ")
}

// Stop halts sampling and returns the collected series — the FIRST call only;
// later calls return nil. That is what makes the double-call pattern safe: a
// caller drains on the success path and still leaves a teardown-registered drain
// in place as the failure-path net.
func (s *Sampler) Stop() []Series {
	var out []Series
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.done != nil {
			<-s.done
		}
		out = s.snapshot()
	})
	return out
}

// discover lists the pods behind each role's label selector. A role whose
// selector errors or matches nothing contributes nothing.
func (s *Sampler) discover(ctx context.Context) []target {
	var out []target
	for _, r := range s.roles {
		names, err := s.kube.GetSelector(ctx, "pod", r.selector, "{.items[*].metadata.name}")
		if err != nil {
			s.logf("metrics: list pods %s: %v", r.selector, err)
			continue
		}
		for _, name := range strings.Fields(names) {
			out = append(out, target{pod: name, role: r})
		}
	}
	return out
}

// loop samples immediately (establishing the counter baselines) then every
// interval until ctx is cancelled.
func (s *Sampler) loop(ctx context.Context, targets []target) {
	defer close(s.done)
	states := make(map[string]*podState, len(targets))
	for _, tg := range targets {
		states[tg.pod] = newPodState()
	}
	tick := time.NewTicker(s.cfg.SampleInterval)
	defer tick.Stop()
	s.sampleAll(ctx, targets, states)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.sampleAll(ctx, targets, states)
		}
	}
}

// sampleAll probes every target concurrently. Each goroutine touches only its
// own pod's state, so the states map is read-only here. One tick is bounded by
// the sample interval so a hung exec cannot stall the next tick indefinitely.
func (s *Sampler) sampleAll(ctx context.Context, targets []target, states map[string]*podState) {
	tctx, cancel := context.WithTimeout(ctx, s.cfg.SampleInterval)
	defer cancel()
	now := time.Now()
	var wg sync.WaitGroup
	for _, tg := range targets {
		wg.Add(1)
		go func(tg target) {
			defer wg.Done()
			ex := s.kube.ForPod(tg.pod, tg.role.container)
			st := states[tg.pod]
			s.record(tg, sampleCgroup(tctx, ex, st, now, s.logf))
			s.record(tg, samplePrometheus(tctx, ex, tg.role.port, tg.role.path, tg.role.entries, st, now, s.logf))
		}(tg)
	}
	wg.Wait()
}

// record files a pod's readings under "[<role tag>] <human title>" keys.
func (s *Sampler) record(tg target, readings []reading) {
	if len(readings) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rd := range readings {
		key := "[" + tg.role.tag + "] " + rd.name
		ser := s.store[key]
		if ser == nil {
			ser = &series{unit: rd.unit, round: rd.round, lines: map[string]*ring{}}
			s.store[key] = ser
			s.keys = append(s.keys, key)
		}
		if ser.desc == "" && rd.desc != "" {
			ser.desc = rd.desc
		}
		ser.raw = mergeRaw(ser.raw, rd.raw)
		line := ser.lines[tg.pod]
		if line == nil {
			line = &ring{cap: s.cfg.MaxPoints}
			ser.lines[tg.pod] = line
		}
		line.add(Point{UnixMS: rd.at.UnixMilli(), Value: rd.value})
	}
}

// mergeRaw unions the raw prometheus names behind a series, kept sorted so the
// emitted metric is stable across runs.
func mergeRaw(have, add []string) []string {
	for _, name := range add {
		if i := sort.SearchStrings(have, name); i < len(have) && have[i] == name {
			continue
		}
		have = append(have, name)
		sort.Strings(have)
	}
	return have
}

// snapshot copies the accumulated series out, keys sorted for stable reporting.
func (s *Sampler) snapshot() []Series {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := append([]string(nil), s.keys...)
	sort.Strings(keys)
	out := make([]Series, 0, len(keys))
	for _, key := range keys {
		ser := s.store[key]
		lines := make(map[string][]Point, len(ser.lines))
		for pod, r := range ser.lines {
			if len(r.pts) == 0 {
				continue
			}
			lines[pod] = append([]Point(nil), r.pts...)
		}
		if len(lines) == 0 {
			continue
		}
		out = append(out, Series{
			Key: key, Unit: ser.unit, Round: ser.round, Desc: ser.desc,
			Raw: append([]string(nil), ser.raw...), Lines: lines,
		})
	}
	return out
}

// compiledEntry is an Entry with its regex compiled and its optional fields
// defaulted, so the sampling path never re-derives them.
type compiledEntry struct {
	re    *regexp.Regexp
	entry Entry
}

// compileEntries normalizes and compiles the configured entries, dropping any
// whose regex does not compile — a pattern matching nothing is harmless either
// way, and collection must never fail its caller.
func compileEntries(entries []Entry) []compiledEntry {
	out := make([]compiledEntry, 0, len(entries))
	for _, e := range entries {
		re, err := regexp.Compile(e.Match)
		if err != nil {
			continue
		}
		out = append(out, compiledEntry{re: re, entry: normalize(e)})
	}
	return out
}

// normalize fills an entry's optional fields with their documented defaults.
func normalize(e Entry) Entry {
	if e.Kind == "" {
		e.Kind = KindGauge
	}
	if e.DivideBy == 0 {
		e.DivideBy = 1
	}
	if e.Round == 0 {
		e.Round = 2
	}
	if e.Unit == "" && (e.Kind == KindCounter || e.Kind == KindLatency) {
		e.Unit = "/s"
	}
	if e.Kind == KindLatency {
		if e.LatencyTitle == "" {
			e.LatencyTitle = e.Title + " latency"
		}
		if e.SumUnit == "" {
			e.SumUnit = "us"
		}
	}
	return e
}

// reading is one sampled value for one pod, already carrying its human title.
type reading struct {
	name  string
	unit  string
	round int
	value float64
	at    time.Time
	raw   []string
	desc  string
}

// ring is a per-line point buffer capped at cap, dropping the oldest points when
// full — a long soak must not grow the sampler's memory without bound.
type ring struct {
	cap int
	pts []Point
}

func (r *ring) add(p Point) {
	if r.cap > 0 && len(r.pts) >= r.cap {
		r.pts = append(r.pts[:0], r.pts[len(r.pts)-r.cap+1:]...)
	}
	r.pts = append(r.pts, p)
}

// runner is the one capability a probe needs. A pod-bound *k8s.Kubectl satisfies
// it; unit tests inject a scripted fake.
type runner interface {
	Run(ctx context.Context, cmd string) (execpkg.Result, error)
}

// podState is the per-pod memory a counter-to-rate conversion needs.
type podState struct {
	cgroup layout
	cpu    cpuSample
	prom   map[string]promSample
}

func newPodState() *podState { return &podState{prom: map[string]promSample{}} }
