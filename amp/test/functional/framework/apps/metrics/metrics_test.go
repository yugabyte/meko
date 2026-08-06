package metrics

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

type fakeGet struct{ m map[string]any }

func (f fakeGet) GetBool(key string, def bool) bool {
	if v, ok := f.m[key].(bool); ok {
		return v
	}
	return def
}

func (f fakeGet) GetInt(key string, def int) int {
	if v, ok := f.m[key].(int); ok {
		return v
	}
	return def
}

func (f fakeGet) GetDuration(key string, def time.Duration) time.Duration {
	if v, ok := f.m[key].(time.Duration); ok {
		return v
	}
	return def
}

func (f fakeGet) GetStringSlice(key string, def []string) []string {
	if v, ok := f.m[key].([]string); ok {
		return v
	}
	return def
}

func TestFromConfigDefaultsAndOverrides(t *testing.T) {
	cfg := FromConfig(fakeGet{})
	if !cfg.Enabled || cfg.SampleInterval != DefaultSampleInterval || cfg.MaxPoints != DefaultMaxPoints {
		t.Errorf("defaults = %+v", cfg)
	}
	if !slices.Equal(cfg.Include, DefaultInclude) {
		t.Errorf("Include = %q, want the default set", cfg.Include)
	}
	if !slices.Equal(cfg.Counters, DefaultCounters) {
		t.Errorf("Counters = %q, want the default set", cfg.Counters)
	}

	over := FromConfig(fakeGet{m: map[string]any{
		"apps.metrics.enabled":         false,
		"apps.metrics.sample_interval": 5 * time.Second,
		"apps.metrics.include":         []string{"^only_this$"},
		"apps.metrics.tserver_port":    19000,
	}})
	if over.Enabled || over.SampleInterval != 5*time.Second || over.TServerPort != 19000 {
		t.Errorf("overrides = %+v", over)
	}
	if !slices.Equal(over.Include, []string{"^only_this$"}) {
		t.Errorf("Include override = %q", over.Include)
	}
}

// An invalid include pattern is dropped rather than failing construction.
func TestNewDropsBadIncludePattern(t *testing.T) {
	s := New(nil, Config{Include: []string{`^good$`, `[unclosed`}, Counters: []string{`[also bad`}})
	if len(s.include) != 1 || !s.include[0].MatchString("good") {
		t.Errorf("compiled %d include patterns, want the 1 valid one", len(s.include))
	}
	if len(s.counters) != 0 {
		t.Errorf("compiled %d counter patterns, want none", len(s.counters))
	}
}

func TestRingDropsOldest(t *testing.T) {
	r := &ring{cap: 3}
	for i := 1; i <= 5; i++ {
		r.add(Point{UnixMS: int64(i), Value: float64(i)})
	}
	if len(r.pts) != 3 {
		t.Fatalf("len = %d, want 3", len(r.pts))
	}
	if r.pts[0].UnixMS != 3 || r.pts[2].UnixMS != 5 {
		t.Errorf("kept %v, want the last three points", r.pts)
	}
}

// The cap is enforced per line by the sampler's configured MaxPoints, so a long
// run can never grow storage without bound.
func TestRecordCapsEachLineAtMaxPoints(t *testing.T) {
	s := New(nil, Config{MaxPoints: 4, SampleInterval: time.Second})
	tg := target{pod: "yb-tserver-0", role: rolesFor(s.cfg)[0]}
	base := time.Unix(1730000000, 0)
	for i := 0; i < 50; i++ {
		s.record(tg, []reading{{name: "CPU", unit: "cores", round: 2, value: float64(i), at: base.Add(time.Duration(i) * time.Second)}})
	}
	got := s.snapshot()
	if len(got) != 1 {
		t.Fatalf("series = %+v, want 1", got)
	}
	pts := got[0].Lines["yb-tserver-0"]
	if len(pts) != 4 || pts[0].Value != 46 || pts[3].Value != 49 {
		t.Errorf("line = %+v, want the last 4 points (46..49)", pts)
	}
}

// Keys are "[<role tag>] <name>", one series per key with a line per pod, sorted.
func TestRecordKeyNamingAndSnapshotOrder(t *testing.T) {
	s := New(nil, Config{MaxPoints: 10, SampleInterval: time.Second})
	roles := rolesFor(s.cfg)
	tserver, master := roles[0], roles[1]
	now := time.Unix(1730000000, 0)

	s.record(target{pod: "yb-tserver-1", role: tserver}, []reading{{name: "CPU", unit: "cores", round: 2, value: 1.5, at: now}})
	s.record(target{pod: "yb-tserver-0", role: tserver}, []reading{{name: "CPU", unit: "cores", round: 2, value: 0.5, at: now}})
	s.record(target{pod: "yb-master-0", role: master}, []reading{{name: "Memory", unit: "MiB", round: 1, value: 256, at: now}})

	got := s.snapshot()
	var keys []string
	for _, ser := range got {
		keys = append(keys, ser.Key)
	}
	want := []string{"[yb master] Memory", "[yb tserver] CPU"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys = %q, want %q", keys, want)
	}
	cpu := got[1]
	if cpu.Unit != "cores" || cpu.Round != 2 || len(cpu.Lines) != 2 {
		t.Errorf("cpu series = %+v, want cores/round 2 with a line per pod", cpu)
	}
	if cpu.Average() != 1.0 {
		t.Errorf("Average = %v, want 1.0", cpu.Average())
	}
}

func TestSeriesPairsWireShape(t *testing.T) {
	ser := Series{Key: "[yb tserver] CPU", Unit: "cores", Round: 2, Lines: map[string][]Point{
		"yb-tserver-0": {{UnixMS: 1730000000000, Value: 1.42}, {UnixMS: 1730000015000, Value: 1.61}},
	}}
	raw, err := json.Marshal(ser.Pairs())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"yb-tserver-0":[[1730000000000,1.42],[1730000015000,1.61]]}`
	if string(raw) != want {
		t.Errorf("pairs JSON = %s, want %s", raw, want)
	}
	if got := ser.Average(); math.Abs(got-1.515) > 1e-9 {
		t.Errorf("Average = %v, want 1.515", got)
	}
}

func TestSeriesAverageOfEmptyIsZero(t *testing.T) {
	if got := (Series{}).Average(); got != 0 {
		t.Errorf("Average = %v, want 0", got)
	}
}

// scriptedKube answers pod discovery and pod-exec probes over an injected
// kubectl Runner — nothing real runs.
func scriptedKube(t *testing.T) *k8s.Kubectl {
	t.Helper()
	var mu sync.Mutex
	run := func(_ context.Context, _ io.Reader, argv []string) (execpkg.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "get pod -l app=yb-tserver"):
			return execpkg.Result{Stdout: "yb-tserver-0 yb-tserver-1"}, nil
		case strings.Contains(joined, "get pod -l app=yb-master"):
			return execpkg.Result{Stdout: "yb-master-0"}, nil
		case strings.Contains(joined, "cpu.stat"):
			// A monotonically advancing CPU counter so the second tick has a delta.
			return execpkg.Result{Stdout: "V2\nusage_usec 1000000\n@@\n1048576\n"}, nil
		case strings.Contains(joined, "prometheus-metrics"):
			return execpkg.Result{Stdout: "mem_tracker 4096\n"}, nil
		}
		return execpkg.Result{}, nil
	}
	return k8s.New(k8s.Config{Namespace: "ampqa-yb-test"}).WithRunner(run)
}

func TestSamplerStartCollectsAndStopIsDrainOnce(t *testing.T) {
	s := New(scriptedKube(t), Config{
		SampleInterval: 5 * time.Millisecond,
		MaxPoints:      100,
		Include:        []string{`^mem_tracker$`},
		TServerPort:    9000,
		MasterPort:     7000,
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(s.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	got := s.Stop()
	if len(got) < 2 {
		t.Fatalf("collected %+v, want at least the memory and mem_tracker series", got)
	}
	var keys []string
	for _, ser := range got {
		keys = append(keys, ser.Key)
	}
	if !slices.Contains(keys, "[yb tserver] Memory") || !slices.Contains(keys, "[yb master] Memory") {
		t.Errorf("keys = %q, want a Memory series for both roles", keys)
	}
	mem := got[slices.Index(keys, "[yb tserver] Memory")]
	if len(mem.Lines) != 2 {
		t.Errorf("tserver Memory lines = %v, want one per discovered pod", mem.Lines)
	}

	// Stop yields the series ONCE: the perf suites call it on the success path and
	// again from teardown, and the second call must emit nothing.
	if again := s.Stop(); again != nil {
		t.Errorf("second Stop returned %d series, want none", len(again))
	}
}

// With no matching pods there is nothing to sample, and the caller is told so
// rather than getting a sampler that silently collects nothing.
func TestSamplerStartErrorsWithNoPods(t *testing.T) {
	kube := k8s.New(k8s.Config{Namespace: "empty"}).WithRunner(
		func(context.Context, io.Reader, []string) (execpkg.Result, error) { return execpkg.Result{}, nil })
	s := New(kube, Config{SampleInterval: time.Second})
	if err := s.Start(context.Background()); err == nil {
		t.Errorf("Start on an empty namespace returned no error")
	}
}
