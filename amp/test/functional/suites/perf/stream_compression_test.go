package perf

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func scDefaults(t *testing.T, overrides ...string) streamCompressionParams {
	t.Helper()
	c, err := core.LoadConfig(nil, nil, overrides)
	if err != nil {
		t.Fatal(err)
	}
	return streamCompressionParamsFromConfig(c)
}

func TestStreamCompressionDefaultsCompareGzipWithNone(t *testing.T) {
	p := scDefaults(t)
	if !slices.Equal(p.Modes, []string{"gzip", "none"}) {
		t.Errorf("modes = %v", p.Modes)
	}
	if !slices.Equal(p.RampClients, []int{2, 4, 8, 16}) || !slices.Equal(p.AnalyzeTargets, []int{100, 1000}) {
		t.Errorf("ramp %v, targets %v", p.RampClients, p.AnalyzeTargets)
	}
	if err := p.validate(); err != nil {
		t.Errorf("defaults do not validate: %v", err)
	}
	if r := p.endpointSpec().Resources; r == nil || r.CPU != "2" || r.Memory != "8Gi" {
		t.Errorf("endpoint resources = %+v, want 2 / 8Gi", r)
	}
}

func TestStreamCompressionValidate(t *testing.T) {
	for name, override := range map[string]string{
		"unknown mode":              "perf.stream_compression.modes=list:gzip,lz4",
		"empty mode":                "perf.stream_compression.modes=list:gzip,",
		"duplicate mode":            "perf.stream_compression.modes=list:gzip,gzip",
		"random over":               "perf.stream_compression.random_pct=int:150",
		"seed too small":            "perf.stream_compression.seed_rows=int:10",
		"seed not a batch multiple": "perf.stream_compression.seed_rows=int:2000050",
		"typo in ramp":              "perf.stream_compression.ramp_clients=list:2,8x",
		"empty targets":             "perf.stream_compression.analyze_targets=list:",
	} {
		if err := scDefaults(t, override).validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestScInsertScriptMixesRandomAndRepeatedText(t *testing.T) {
	p := streamCompressionParams{RowBytes: 1000, RandomPct: 25, Batch: 100}
	s := scInsertScript("sc_seed", p)
	for _, want := range []string{
		`\set r random(`,
		"INSERT INTO sc_seed (v)",
		"generate_series(1, 8) s), 250)",
		"generate_series(1, 100) g",
		", 750)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	if !strings.Contains(s, "g * 7919") {
		t.Error("the md5 subquery must be correlated on the row, or every row of a batch is identical")
	}
}

func TestParseAnalyze(t *testing.T) {
	out := `SET
Timing is on.
Time: 0.412 ms
INFO:  analyzing "public.sc_seed"
INFO:  "sc_seed": scanned 30000 of 285714 pages, containing 210000 live rows and 0 dead rows; 30000 rows in sample, 2000000 estimated total rows
ANALYZE
Time: 11873.205 ms (00:11.873)`
	ms, pages, err := parseAnalyze(out)
	if err != nil || ms != 11873.205 || pages != 30000 {
		t.Errorf("parseAnalyze = %v, %d, %v", ms, pages, err)
	}
	if _, _, err := parseAnalyze("ANALYZE"); err == nil {
		t.Error("output with no timing must fail")
	}
}

func TestGflagMismatches(t *testing.T) {
	varz := "--enable_stream_compression=true\n--stream_compression_algo=0\n--other=x\n"
	if bad := gflagMismatches(varz, map[string]string{"stream_compression_algo": "0"}); len(bad) != 0 {
		t.Errorf("matching flags reported: %v", bad)
	}
	bad := gflagMismatches(varz, map[string]string{"stream_compression_algo": "1", "gzip_stream_compression_level": "1"})
	if len(bad) != 2 || !strings.Contains(bad[1], `stream_compression_algo="0"`) {
		t.Errorf("mismatches = %v", bad)
	}
}

func TestScDatapointsGateEachArmAndReportTheRatio(t *testing.T) {
	gzip := scArmResult{Mode: "gzip", SeedRowsPerSec: 5000, RampRowsPerSec: map[int]float64{4: 6000},
		AnalyzeMS: map[int][]float64{100: {12000, 11000}}, AnalyzePages: map[int]int{100: 30000}, ReadTPS: 800, ReadLatencyAvg: 10}
	none := scArmResult{Mode: "none", SeedRowsPerSec: 10000, RampRowsPerSec: map[int]float64{4: 12000},
		AnalyzeMS: map[int][]float64{100: {3000}}, AnalyzePages: map[int]int{100: 30000}, ReadTPS: 1000, ReadLatencyAvg: 8}
	gated, info := scDatapoints([]scArmResult{gzip, none})
	find := func(dps []core.Datapoint, name string) (core.Datapoint, bool) {
		for _, dp := range dps {
			if dp.Name == name {
				return dp, true
			}
		}
		return core.Datapoint{}, false
	}
	for _, name := range []string{
		"stream_compression.gzip.seed_rows_per_s", "stream_compression.none.seed_rows_per_s",
		"stream_compression.gzip.insert_rows_per_s@c4", "stream_compression.gzip.analyze_ms@target100",
		"stream_compression.gzip.read_tps", "stream_compression.none.read_latency_avg_ms",
	} {
		if _, ok := find(gated, name); !ok {
			t.Errorf("%s is not gated", name)
		}
	}
	if dp, _ := find(gated, "stream_compression.gzip.analyze_ms@target100"); dp.Value != 11500 || dp.N != 2 {
		t.Errorf("analyze datapoint = %+v, want the median of both reps", dp)
	}
	if dp, ok := find(info, "stream_compression.gzip_over_none.seed_rows_per_s"); !ok || dp.Value != 0.5 {
		t.Errorf("seed ratio = %+v", dp)
	}
	if dp, ok := find(info, "stream_compression.gzip_over_none.analyze_ms@target100"); !ok || dp.Direction != core.LowerIsBetter {
		t.Errorf("analyze ratio = %+v", dp)
	}
	if _, ok := find(gated, "stream_compression.gzip_over_none.seed_rows_per_s"); ok {
		t.Error("the ratio is reported, not gated")
	}
	if _, info := scDatapoints([]scArmResult{gzip}); slices.ContainsFunc(info, func(dp core.Datapoint) bool { return strings.Contains(dp.Name, "gzip_over_none") }) {
		t.Error("a single arm has no ratio to report")
	}
}

func TestModeBarrierWaitsForEveryLiveArm(t *testing.T) {
	b := newModeBarrier(2)
	var order []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.sync("seed")
		mu.Lock()
		order = append(order, "first through")
		mu.Unlock()
	}()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(order) != 0 {
		t.Fatal("an arm passed the barrier alone")
	}
	mu.Unlock()
	b.sync("seed")
	wg.Wait()

	b.leave()
	done := make(chan struct{})
	go func() { b.sync("analyze"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the last live arm waited for one that left")
	}
}

func TestScWriteFilesCmdChainsNothingAfterAHeredoc(t *testing.T) {
	cmd := scWriteFilesCmd("/tmp/x", map[string]string{"b.sql": "select 2;", "a.sql": "select 1;"})
	want := "set -e\nmkdir -p /tmp/x\n" +
		"cat > /tmp/x/a.sql <<'AMPQA_SC_EOF'\nselect 1;\nAMPQA_SC_EOF\n" +
		"cat > /tmp/x/b.sql <<'AMPQA_SC_EOF'\nselect 2;\nAMPQA_SC_EOF\n"
	if cmd != want {
		t.Errorf("cmd =\n%s\nwant\n%s", cmd, want)
	}
}
