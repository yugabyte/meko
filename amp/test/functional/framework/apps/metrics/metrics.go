// Package metrics is the background server-metrics sampler: a composable app
// that polls the YugabyteDB pods behind a test while the test runs, and hands
// back a per-pod time series per metric on stop.
//
// It is universal — any test holding a namespace-scoped kubectl can start one —
// and it never fails its caller: every probe error is logged and skipped, so a
// pod that disappears mid-run simply stops contributing points. Storage is
// capped per line (drop-oldest), so a long soak cannot grow it without bound.
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
)

// DefaultInclude is the prometheus metric set kept when apps.metrics.include is
// unset: regex patterns matched against the bare metric name (labels stripped).
// Compaction/flush volume, WAL append/sync, RPC queue depth, read/write op
// counters, SST footprint, the root memory tracker, and process CPU time.
var DefaultInclude = []string{
	`^rocksdb_(compact|flush)_[a-z_]*bytes$`,
	`^log_(append|sync)_latency_(count|sum)$`,
	`^log_bytes_logged$`,
	`^rpc_inbound_calls_created$`,
	`^rpcs_in_queue_yb_(tserver|master)_`,
	`^handler_latency_yb_tserver_TabletServerService_(Read|Write)_(count|sum)$`,
	`^total_sst_files_size$`,
	`^mem_tracker$`,
	`^cpu_(utime|stime)$`,
}

// DefaultCounters names the kept series that are COUNTERS and so are reported as
// per-second rates. _count/_sum-suffixed names are counters regardless. Anything
// matching neither is a gauge and passes through: mem_tracker and
// total_sst_files_size rise under load but are not rates.
var DefaultCounters = []string{
	`^rocksdb_(compact|flush)_`,
	`^log_bytes_logged$`,
	`^rpc_inbound_calls_created$`,
	`^cpu_(utime|stime)$`,
}

// Config is the resolved apps.metrics.* configuration.
type Config struct {
	Enabled        bool
	SampleInterval time.Duration
	MaxPoints      int
	Include        []string
	Counters       []string
	TServerPort    int
	MasterPort     int
}

// Getter is the config surface this app reads (satisfied by *core.Config).
type Getter interface {
	GetBool(key string, def bool) bool
	GetInt(key string, def int) int
	GetDuration(key string, def time.Duration) time.Duration
	GetStringSlice(key string, def []string) []string
}

// FromConfig reads apps.metrics.* into a Config.
func FromConfig(get Getter) Config {
	return Config{
		Enabled:        get.GetBool("apps.metrics.enabled", DefaultEnabled),
		SampleInterval: get.GetDuration("apps.metrics.sample_interval", DefaultSampleInterval),
		MaxPoints:      get.GetInt("apps.metrics.max_points", DefaultMaxPoints),
		Include:        get.GetStringSlice("apps.metrics.include", DefaultInclude),
		Counters:       get.GetStringSlice("apps.metrics.counters", DefaultCounters),
		TServerPort:    get.GetInt("apps.metrics.tserver_port", DefaultTServerPort),
		MasterPort:     get.GetInt("apps.metrics.master_port", DefaultMasterPort),
	}
}

// Point is one sample: a wall-clock millisecond timestamp and a value.
type Point struct {
	UnixMS int64
	Value  float64
}

// Series is one metric's collected data — a line per sampled pod. Round is the
// display precision (decimal places) a reporter should render the values at.
type Series struct {
	Key   string
	Unit  string
	Round int
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

// role is one YB server kind a backing exposes: its chart-fixed pod label, its
// container name, the HTTP port serving /prometheus-metrics, and the tag that
// prefixes every metric key it contributes.
type role struct {
	tag       string
	selector  string
	container string
	port      int
}

func rolesFor(cfg Config) []role {
	return []role{
		{tag: "yb tserver", selector: "app=yb-tserver", container: "yb-tserver", port: cfg.TServerPort},
		{tag: "yb master", selector: "app=yb-master", container: "yb-master", port: cfg.MasterPort},
	}
}

// target is one sampled pod.
type target struct {
	pod  string
	role role
}

// Sampler polls a set of pods on an interval until stopped.
type Sampler struct {
	kube     *k8s.Kubectl
	cfg      Config
	logf     func(string, ...any)
	include  []*regexp.Regexp
	counters []*regexp.Regexp

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
	lines map[string]*ring
}

// New builds a Sampler over an already namespace-scoped kubectl.
func New(kube *k8s.Kubectl, cfg Config) *Sampler {
	if cfg.SampleInterval <= 0 {
		cfg.SampleInterval = DefaultSampleInterval
	}
	if cfg.MaxPoints <= 0 {
		cfg.MaxPoints = DefaultMaxPoints
	}
	s := &Sampler{kube: kube, cfg: cfg, logf: func(string, ...any) {}, store: map[string]*series{}}
	s.include = compilePatterns(cfg.Include)
	s.counters = compilePatterns(cfg.Counters)
	return s
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
		return fmt.Errorf("no yb pods matched app=yb-tserver / app=yb-master in namespace %q", s.kube.Namespace())
	}
	bg, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.loop(bg, targets)
	return nil
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
	for _, r := range rolesFor(s.cfg) {
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
			s.record(tg, samplePrometheus(tctx, ex, tg.role.port, s.include, s.counters, st, now, s.logf))
		}(tg)
	}
	wg.Wait()
}

// record files a pod's readings under "[<role tag>] <name>" keys.
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
		line := ser.lines[tg.pod]
		if line == nil {
			line = &ring{cap: s.cfg.MaxPoints}
			ser.lines[tg.pod] = line
		}
		line.add(Point{UnixMS: rd.at.UnixMilli(), Value: rd.value})
	}
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
		out = append(out, Series{Key: key, Unit: ser.unit, Round: ser.round, Lines: lines})
	}
	return out
}

// compilePatterns compiles the configured regexes, dropping any that do not
// compile — a pattern matching nothing is harmless either way.
func compilePatterns(pats []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, pat := range pats {
		re, err := regexp.Compile(pat)
		if err != nil {
			continue
		}
		out = append(out, re)
	}
	return out
}

// reading is one sampled value for one pod, before the role tag is applied.
type reading struct {
	name  string
	unit  string
	round int
	value float64
	at    time.Time
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
