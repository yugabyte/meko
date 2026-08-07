package metrics

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"slices"
	"sort"
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

func (f fakeGet) GetMapSlice(key string, def []map[string]any) []map[string]any {
	if v, ok := f.m[key].([]map[string]any); ok {
		return v
	}
	return def
}

func TestFromConfigDefaultsAndOverrides(t *testing.T) {
	cfg := FromConfig(fakeGet{})
	if !cfg.Enabled || cfg.SampleInterval != DefaultSampleInterval || cfg.MaxPoints != DefaultMaxPoints {
		t.Errorf("defaults = %+v", cfg)
	}
	if !slices.Equal(cfg.Collect, DefaultCollect) || !slices.Equal(cfg.EndpointCollect, DefaultEndpointCollect) {
		t.Errorf("collect lists are not the code defaults")
	}

	over := FromConfig(fakeGet{m: map[string]any{
		"apps.metrics.enabled":         false,
		"apps.metrics.sample_interval": 5 * time.Second,
		"apps.metrics.tserver_port":    19000,
		"apps.metrics.collect": []map[string]any{
			{"match": "^only_this$", "title": "Only this", "unit": "MiB", "divide_by": 1048576, "round": 1},
			{"title": "no match, dropped"},
			{"match": "^no_title_dropped$"},
		},
	}})
	if over.Enabled || over.SampleInterval != 5*time.Second || over.TServerPort != 19000 {
		t.Errorf("overrides = %+v", over)
	}
	want := []Entry{{Match: "^only_this$", Title: "Only this", Unit: "MiB", DivideBy: 1048576, Round: 1}}
	if !slices.Equal(over.Collect, want) {
		t.Errorf("Collect override = %+v, want %+v (entries without match or title dropped)", over.Collect, want)
	}
}

// Every default entry produces a usable key, and no key carries a raw
// prometheus name — the whole point of the structured list.
func TestDefaultCollectTitlesAreHumanReadable(t *testing.T) {
	for _, set := range [][]Entry{DefaultCollect, DefaultEndpointCollect} {
		for _, e := range set {
			if e.Title == "" || e.Match == "" {
				t.Errorf("entry %+v is missing match or title", e)
			}
			if strings.Contains(e.Title, "_") {
				t.Errorf("title %q looks like a raw prometheus name", e.Title)
			}
			if e.Kind == KindLatency && !strings.Contains(e.Match, "(count|sum)") {
				t.Errorf("latency entry %q must match both histogram components: %q", e.Title, e.Match)
			}
		}
	}
}

// The exact key set the default config emits from a full YB scrape. This is the
// contract a human reads on the report.
func TestDefaultConfigEmittedKeys(t *testing.T) {
	body := strings.Join([]string{
		"handler_latency_yb_tserver_TabletServerService_Read_count 100",
		"handler_latency_yb_tserver_TabletServerService_Read_sum 200000",
		"handler_latency_yb_tserver_TabletServerService_Write_count 100",
		"handler_latency_yb_tserver_TabletServerService_Write_sum 300000",
		"log_append_latency_count 50",
		"log_append_latency_sum 5000",
		"log_sync_latency_count 50",
		"log_sync_latency_sum 90000",
		"log_bytes_logged 1048576",
		"rocksdb_compact_read_bytes 2097152",
		"rocksdb_compact_write_bytes 1048576",
		"rocksdb_flush_write_bytes 524288",
		"rpc_inbound_calls_created 900",
		`rpcs_in_queue_yb_tserver_TabletServerService{x="y"} 3`,
		"total_sst_files_size 10485760",
		"mem_tracker 20971520",
		"cpu_utime 700",
	}, "\n")
	// Two identical scrapes ten seconds apart: the second has every delta seeded,
	// but no new calls, so the mean-latency series stay out (idle interval).
	busier := strings.ReplaceAll(body, "_count 100", "_count 200")
	busier = strings.ReplaceAll(busier, "Read_sum 200000", "Read_sum 400000")
	busier = strings.ReplaceAll(busier, "Write_sum 300000", "Write_sum 900000")
	busier = strings.ReplaceAll(busier, "log_append_latency_count 50", "log_append_latency_count 150")
	busier = strings.ReplaceAll(busier, "log_append_latency_sum 5000", "log_append_latency_sum 15000")
	busier = strings.ReplaceAll(busier, "log_sync_latency_count 50", "log_sync_latency_count 150")
	busier = strings.ReplaceAll(busier, "log_sync_latency_sum 90000", "log_sync_latency_sum 290000")
	busier = strings.ReplaceAll(busier, "log_bytes_logged 1048576", "log_bytes_logged 11534336")

	ex := &fakeRunner{out: []execpkg.Result{{Stdout: body}, {Stdout: busier}}}
	entries := compileEntries(DefaultCollect)
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	quiet := func(string, ...any) {}

	samplePrometheus(context.Background(), ex, 9000, ybPromPath, entries, st, t0, quiet)
	got := samplePrometheus(context.Background(), ex, 9000, ybPromPath, entries, st, t0.Add(10*time.Second), quiet)

	var keys []string
	for _, rd := range got {
		keys = append(keys, "[yb tserver] "+rd.name)
	}
	sort.Strings(keys)
	want := []string{
		"[yb tserver] Compactions read",
		"[yb tserver] Compactions written",
		"[yb tserver] Flushes written",
		"[yb tserver] Inbound RPCs",
		"[yb tserver] Memory tracker",
		"[yb tserver] RPC queue depth",
		"[yb tserver] Read RPCs",
		"[yb tserver] Read latency",
		"[yb tserver] SST files size",
		"[yb tserver] WAL append latency",
		"[yb tserver] WAL appends",
		"[yb tserver] WAL bytes",
		"[yb tserver] WAL sync latency",
		"[yb tserver] WAL syncs",
		"[yb tserver] Write RPCs",
		"[yb tserver] Write latency",
	}
	if !slices.Equal(keys, want) {
		t.Errorf("emitted keys =\n%s\nwant\n%s", strings.Join(keys, "\n"), strings.Join(want, "\n"))
	}

	byName := map[string]reading{}
	for _, rd := range got {
		byName[rd.name] = rd
	}
	// 100 writes in 10s at 600000 µs total → 10/s and 6 ms mean.
	if rd := byName["Write RPCs"]; math.Abs(rd.value-10) > 1e-9 || rd.unit != "/s" {
		t.Errorf("Write RPCs = %+v, want 10 /s", rd)
	}
	if rd := byName["Write latency"]; math.Abs(rd.value-6) > 1e-9 || rd.unit != "ms" {
		t.Errorf("Write latency = %+v, want 6 ms", rd)
	}
	if rd := byName["WAL bytes"]; math.Abs(rd.value-1) > 1e-9 || rd.unit != "MiB/s" {
		t.Errorf("WAL bytes = %+v, want 1 MiB/s", rd)
	}
	if rd := byName["Memory tracker"]; rd.value != 20 || rd.unit != "MiB" {
		t.Errorf("Memory tracker = %+v, want 20 MiB", rd)
	}
	// cpu_utime is scraped by no entry: the cgroup CPU series already covers it.
	if _, ok := byName["cpu_utime"]; ok {
		t.Errorf("cpu_utime leaked into the readings")
	}
}

// A default-config endpoint scrape emits the pageserver keys under [endpoint].
func TestDefaultEndpointConfigEmittedKeys(t *testing.T) {
	body := strings.Join([]string{
		"pageserver_smgr_query_seconds_count 100",
		"pageserver_smgr_query_seconds_sum 0.2",
		"pageserver_wait_lsn_seconds_count 10",
		"pageserver_wait_lsn_seconds_sum 0.05",
		"pageserver_replayed_wal_records_total 1000",
		"pageserver_io_operations_bytes_total 1048576",
		"pageserver_resident_physical_size 52428800",
	}, "\n")
	busier := strings.ReplaceAll(body, "_seconds_count 100", "_seconds_count 300")
	busier = strings.ReplaceAll(busier, "query_seconds_sum 0.2", "query_seconds_sum 0.6")
	busier = strings.ReplaceAll(busier, "_seconds_count 10", "_seconds_count 20")
	busier = strings.ReplaceAll(busier, "wait_lsn_seconds_sum 0.05", "wait_lsn_seconds_sum 0.15")
	busier = strings.ReplaceAll(busier, "records_total 1000", "records_total 2000")
	busier = strings.ReplaceAll(busier, "bytes_total 1048576", "bytes_total 11534336")

	ex := &fakeRunner{out: []execpkg.Result{{Stdout: body}, {Stdout: busier}}}
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	quiet := func(string, ...any) {}
	entries := compileEntries(DefaultEndpointCollect)

	samplePrometheus(context.Background(), ex, 9898, pageserverPromPath, entries, st, t0, quiet)
	got := samplePrometheus(context.Background(), ex, 9898, pageserverPromPath, entries, st, t0.Add(10*time.Second), quiet)

	var keys []string
	byName := map[string]reading{}
	for _, rd := range got {
		keys = append(keys, "[endpoint] "+rd.name)
		byName[rd.name] = rd
	}
	sort.Strings(keys)
	want := []string{
		"[endpoint] LSN wait latency",
		"[endpoint] LSN waits",
		"[endpoint] Page request latency",
		"[endpoint] Page requests",
		"[endpoint] Pageserver I/O",
		"[endpoint] Resident layer size",
		"[endpoint] WAL records replayed",
	}
	if !slices.Equal(keys, want) {
		t.Errorf("endpoint keys =\n%s\nwant\n%s", strings.Join(keys, "\n"), strings.Join(want, "\n"))
	}
	// 200 requests over 0.4s → 2 ms mean.
	if rd := byName["Page request latency"]; math.Abs(rd.value-2) > 1e-9 || rd.unit != "ms" {
		t.Errorf("Page request latency = %+v, want 2 ms", rd)
	}
	if rd := byName["Resident layer size"]; rd.value != 50 || rd.unit != "MiB" {
		t.Errorf("Resident layer size = %+v, want 50 MiB", rd)
	}
}

// An invalid match is dropped rather than failing construction.
func TestCompileEntriesDropsBadPattern(t *testing.T) {
	out := compileEntries([]Entry{{Match: `^good$`, Title: "Good"}, {Match: `[unclosed`, Title: "Bad"}})
	if len(out) != 1 || !out[0].re.MatchString("good") {
		t.Errorf("compiled %d entries, want the 1 valid one", len(out))
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
	tg := target{pod: "yb-tserver-0", role: s.roles[0]}
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

// Keys are "[<role tag>] <title>", one series per key with a line per pod,
// sorted, and the raw prometheus names ride on the series instead of the key.
func TestRecordKeyNamingAndSnapshotOrder(t *testing.T) {
	s := New(nil, Config{MaxPoints: 10, SampleInterval: time.Second})
	tserver, master := s.roles[0], s.roles[1]
	now := time.Unix(1730000000, 0)

	s.record(target{pod: "yb-tserver-1", role: tserver}, []reading{
		{name: "Write latency", unit: "ms", round: 3, value: 2, at: now, raw: []string{"handler_latency_yb_tserver_TabletServerService_Write_sum"}}})
	s.record(target{pod: "yb-tserver-0", role: tserver}, []reading{
		{name: "Write latency", unit: "ms", round: 3, value: 4, at: now, raw: []string{"handler_latency_yb_tserver_TabletServerService_Write_count"}}})
	s.record(target{pod: "yb-master-0", role: master}, []reading{{name: "Memory", unit: "MiB", round: 1, value: 256, at: now}})

	got := s.snapshot()
	var keys []string
	for _, ser := range got {
		keys = append(keys, ser.Key)
	}
	want := []string{"[yb master] Memory", "[yb tserver] Write latency"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys = %q, want %q", keys, want)
	}
	lat := got[1]
	if lat.Unit != "ms" || lat.Round != 3 || len(lat.Lines) != 2 {
		t.Errorf("latency series = %+v, want ms/round 3 with a line per pod", lat)
	}
	if lat.Average() != 3.0 {
		t.Errorf("Average = %v, want 3.0", lat.Average())
	}
	wantRaw := []string{
		"handler_latency_yb_tserver_TabletServerService_Write_count",
		"handler_latency_yb_tserver_TabletServerService_Write_sum",
	}
	if !slices.Equal(lat.Raw, wantRaw) {
		t.Errorf("Raw = %q, want the union of both pods' sources sorted", lat.Raw)
	}
	if got[0].Raw != nil && len(got[0].Raw) != 0 {
		t.Errorf("cgroup series Raw = %q, want none", got[0].Raw)
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
// kubectl Runner — nothing real runs. It records the exec argv so a test can
// assert WHICH container was targeted.
func scriptedKube(t *testing.T, rec *[]string) *k8s.Kubectl {
	t.Helper()
	var mu sync.Mutex
	run := func(_ context.Context, _ io.Reader, argv []string) (execpkg.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		joined := strings.Join(argv, " ")
		if rec != nil {
			*rec = append(*rec, joined)
		}
		switch {
		case strings.Contains(joined, "get pod -l app=yb-tserver"):
			return execpkg.Result{Stdout: "yb-tserver-0 yb-tserver-1"}, nil
		case strings.Contains(joined, "get pod -l app=yb-master"):
			return execpkg.Result{Stdout: "yb-master-0"}, nil
		case strings.Contains(joined, "get pod -l app.kubernetes.io/instance=yb-ep-ep-1"):
			return execpkg.Result{Stdout: "yb-ep-ep-1-pod0"}, nil
		case strings.Contains(joined, "cpu.stat"):
			// A monotonically advancing CPU counter so the second tick has a delta.
			return execpkg.Result{Stdout: "V2\nusage_usec 1000000\n@@\n1048576\n"}, nil
		case strings.Contains(joined, "prometheus-metrics"):
			return execpkg.Result{Stdout: "mem_tracker 4194304\n"}, nil
		case strings.Contains(joined, "localhost:9898/metrics"):
			return execpkg.Result{Stdout: "pageserver_resident_physical_size 52428800\n"}, nil
		}
		return execpkg.Result{}, nil
	}
	return k8s.New(k8s.Config{Namespace: "ampqa-yb-test"}).WithRunner(run)
}

func TestSamplerStartCollectsAndStopIsDrainOnce(t *testing.T) {
	s := New(scriptedKube(t, nil), Config{
		SampleInterval: 5 * time.Millisecond,
		MaxPoints:      100,
		Collect:        []Entry{{Match: `^mem_tracker$`, Title: "Memory tracker", Unit: "MiB", DivideBy: 1 << 20}},
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
		t.Fatalf("collected %+v, want at least the memory and tracker series", got)
	}
	var keys []string
	for _, ser := range got {
		keys = append(keys, ser.Key)
	}
	if !slices.Contains(keys, "[yb tserver] Memory") || !slices.Contains(keys, "[yb master] Memory") {
		t.Errorf("keys = %q, want a Memory series for both roles", keys)
	}
	if !slices.Contains(keys, "[yb tserver] Memory tracker") {
		t.Errorf("keys = %q, want the titled prometheus series", keys)
	}
	tracker := got[slices.Index(keys, "[yb tserver] Memory tracker")]
	if !slices.Equal(tracker.Raw, []string{"mem_tracker"}) {
		t.Errorf("Raw = %q, want the source prometheus name", tracker.Raw)
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

// The endpoint sampler discovers pods by the endpoint's own selector and execs
// into the `endpoint` container by name — the pod also runs a postgres-exporter
// sidecar, and an unqualified exec would be answered by whichever k8s picks.
func TestNewEndpointTargetsSelectorAndContainer(t *testing.T) {
	var calls []string
	s := NewEndpoint(scriptedKube(t, &calls), Config{
		SampleInterval:  5 * time.Millisecond,
		MaxPoints:       100,
		EndpointCollect: DefaultEndpointCollect,
		EndpointPort:    DefaultEndpointPort,
	}, "app.kubernetes.io/instance=yb-ep-ep-1")

	if len(s.roles) != 1 || s.roles[0].tag != "endpoint" || s.roles[0].container != "endpoint" {
		t.Fatalf("roles = %+v, want a single endpoint role", s.roles)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(s.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := s.Stop()

	var keys []string
	for _, ser := range got {
		keys = append(keys, ser.Key)
	}
	if !slices.Contains(keys, "[endpoint] Memory") || !slices.Contains(keys, "[endpoint] Resident layer size") {
		t.Errorf("keys = %q, want cgroup Memory plus a titled pageserver series", keys)
	}

	var sawSelector, sawContainer bool
	for _, c := range calls {
		if strings.Contains(c, "get pod -l app.kubernetes.io/instance=yb-ep-ep-1") {
			sawSelector = true
		}
		if strings.Contains(c, "exec yb-ep-ep-1-pod0 -c endpoint") {
			sawContainer = true
		}
	}
	if !sawSelector {
		t.Errorf("pods were not discovered by the endpoint selector: %v", calls)
	}
	if !sawContainer {
		t.Errorf("exec did not target the endpoint container explicitly: %v", calls)
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
