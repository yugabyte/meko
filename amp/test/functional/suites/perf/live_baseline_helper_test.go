package perf

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func rel(tag string) portalBuild  { return portalBuild{BuildTag: tag, Branch: "main"} }
func cust(tag string) portalBuild { return portalBuild{BuildTag: tag, Branch: "someone/wip"} }

func TestPickBaselineTag(t *testing.T) {
	builds := []portalBuild{
		cust("custom-abc123"),
		rel("0.260827.0-aaa"),
		cust("custom-def456"),
		rel("0.260826.1-bbb"),
		rel("0.260825.0-ccc"),
	}
	cases := []struct {
		name, current, want string
	}{
		{"release current gets the one before it", "0.260827.0-aaa", "0.260826.1-bbb"},
		{"mid-list release steps back too", "0.260826.1-bbb", "0.260825.0-ccc"},
		{"oldest release has nothing before it", "0.260825.0-ccc", ""},
		{"custom current gets the newest release", "custom-abc123", "0.260827.0-aaa"},
		{"unknown current gets the newest release", "", "0.260827.0-aaa"},
	}
	for _, tc := range cases {
		if got := pickBaselineTag(builds, tc.current); got != tc.want {
			t.Errorf("%s: pickBaselineTag(%q) = %q, want %q", tc.name, tc.current, got, tc.want)
		}
	}
	if got := pickBaselineTag([]portalBuild{cust("custom-x")}, "custom-x"); got != "" {
		t.Errorf("no releases at all must yield \"\", got %q", got)
	}
}

func benchOf(tps, p95 float64) BenchResult {
	return BenchResult{TPS: aggregate([]float64{tps}), LatencyP95: aggregate([]float64{p95})}
}

func TestLiveBaselineGate(t *testing.T) {
	thr := float64Thresholds{DropPct: 20, RisePct: 35}

	if table, err := liveBaselineGate(benchOf(95, 110), benchOf(100, 100), thr, "0.1-x"); err != nil {
		t.Fatalf("a 5%% drop and a 10%% rise inside the thresholds failed: %v\n%s", err, table)
	}
	_, err := liveBaselineGate(benchOf(70, 100), benchOf(100, 100), thr, "0.1-x")
	if err == nil || !strings.Contains(err.Error(), "tps") {
		t.Fatalf("a 30%% tps drop past the 20%% threshold must regress: %v", err)
	}
	_, err = liveBaselineGate(benchOf(100, 150), benchOf(100, 100), thr, "0.1-x")
	if err == nil || !strings.Contains(err.Error(), "latency_p95") {
		t.Fatalf("a 50%% p95 rise past the 35%% threshold must regress: %v", err)
	}
	if _, err := liveBaselineGate(benchOf(200, 50), benchOf(100, 100), thr, "0.1-x"); err != nil {
		t.Fatalf("an improvement must not regress: %v", err)
	}
	if _, err := liveBaselineGate(benchOf(0, 0), benchOf(100, 100), thr, "0.1-x"); err != nil {
		t.Fatalf("metrics the workload did not produce must be skipped: %v", err)
	}
}

func TestLocalGateItems(t *testing.T) {
	pg := portalGateConfig{MADK: 3, FloorPct: 5, MinRuns: 3}
	gated := []core.Datapoint{
		{Name: "x.tps", Value: 80, Direction: core.HigherIsBetter},
		{Name: "x.lat", Value: 104, Direction: core.LowerIsBetter},
		{Name: "x.new", Value: 1, Direction: core.LowerIsBetter},
	}
	items := []portalGateItem{
		{Name: "x.tps", Value: 80, Prev: portalGatePrev{N: 10, Median: 100, MAD: 1}, Verdict: "neutral"},
		{Name: "x.lat", Value: 104, Prev: portalGatePrev{N: 10, Median: 100, MAD: 10}, Verdict: "regressed"},
		{Name: "x.new", Value: 1, Prev: portalGatePrev{N: 1, Median: 1}, Verdict: "regressed"},
	}
	out := localGateItems(gated, items, pg)
	if out[0].Verdict != "regressed" || *out[0].DeltaPct != 20 {
		t.Errorf("tps -20%% vs a tight (MAD 1%%) history: verdict %q delta %v, want regressed 20", out[0].Verdict, out[0].DeltaPct)
	}
	if out[1].Verdict != "neutral" {
		t.Errorf("lat +4%% inside a 30%% MAD band: verdict %q, want neutral (the portal said regressed and must be ignored)", out[1].Verdict)
	}
	if out[2].Verdict != "no_signal" || out[2].DeltaPct != nil {
		t.Errorf("1 prior run under min_runs 3: verdict %q, want no_signal", out[2].Verdict)
	}
	r, i, n := gateCounts(out)
	if r != 1 || i != 0 || n != 1 {
		t.Errorf("counts = %d/%d/%d, want 1 regressed, 0 improved, 1 no_signal", r, i, n)
	}
}

func TestStartBarrierReleasesDespiteADeadArm(t *testing.T) {
	b := newStartBarrier(3)
	for i := 0; i < 3; i++ {
		go b.wg.Done()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.wait(ctx); err != nil {
		t.Fatalf("barrier did not release after all arrivals: %v", err)
	}
}

func TestStartBarrierWaitHonorsContext(t *testing.T) {
	b := newStartBarrier(2)
	b.wg.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := b.wait(ctx); err == nil {
		t.Fatal("a barrier missing an arrival must respect the context, not hang")
	}
}

// perf/lifecycle's load phase measured 42s-151s inside one run, so a +96% delta
// against history says nothing. The run's own spread has to veto the accusation.
func TestLocalGateItemsSpreadVetoesRegression(t *testing.T) {
	pg := portalGateConfig{MADK: 3, FloorPct: 5, MinRuns: 3}
	prev := portalGatePrev{N: 10, Median: 75524, MAD: 20000}
	noisy := []core.Datapoint{{Name: "lifecycle.load", Value: 148300, Direction: core.LowerIsBetter, SpreadPct: 259}}
	quiet := []core.Datapoint{{Name: "lifecycle.load", Value: 148300, Direction: core.LowerIsBetter, SpreadPct: 8}}
	items := []portalGateItem{{Name: "lifecycle.load", Value: 148300, Prev: prev}}

	if got := localGateItems(noisy, items, pg)[0].Verdict; got != verdictSpreadVetoed {
		t.Errorf("a +96%% delta under a 259%% within-run spread: verdict %q, want %s", got, verdictSpreadVetoed)
	}
	// It must not be counted as a short history window; that is a different fact.
	if _, _, noSignal := gateCounts(localGateItems(noisy, items, pg)); noSignal != 0 {
		t.Error("a spread veto is not a missing history window")
	}
	if got := localGateItems(quiet, items, pg)[0].Verdict; got != "regressed" {
		t.Errorf("the same delta from a steady run must still regress, got %q", got)
	}
}

func TestLocalGateItemsMinDeltaVetoesSubResolutionRegression(t *testing.T) {
	pg := portalGateConfig{MADK: 3, FloorPct: 5, MinRuns: 5}
	prev := portalGatePrev{N: 5, Median: 4.112, MAD: 0.05}
	items := []portalGateItem{{Name: "client_window.compute_restart.error_seconds", Value: 4.961, Prev: prev}}
	declared := []core.Datapoint{{Name: items[0].Name, Value: 4.961, Unit: "s", Direction: core.LowerIsBetter, MinDelta: 15}}
	bare := []core.Datapoint{{Name: items[0].Name, Value: 4.961, Unit: "s", Direction: core.LowerIsBetter}}

	if got := localGateItems(declared, items, pg)[0].Verdict; got != verdictBelowFloor {
		t.Errorf("a +20.7%% delta of 0.85s under a 15s resolution: verdict %q, want %s", got, verdictBelowFloor)
	}
	if got := localGateItems(bare, items, pg)[0].Verdict; got != "regressed" {
		t.Errorf("a datapoint declaring no resolution keeps the percentage verdict, got %q", got)
	}
	real := []portalGateItem{{Name: items[0].Name, Value: 604, Prev: prev}}
	realDP := []core.Datapoint{{Name: items[0].Name, Value: 604, Unit: "s", Direction: core.LowerIsBetter, MinDelta: 15}}
	if got := localGateItems(realDP, real, pg)[0].Verdict; got != "regressed" {
		t.Errorf("4.1s -> 604s must still regress, got %q", got)
	}
}

func TestMinDeltaVetoIsNotCountedAsRegressed(t *testing.T) {
	pg := portalGateConfig{MADK: 3, FloorPct: 5, MinRuns: 5}
	items := []portalGateItem{{Name: "client_window.settings_update.error_seconds", Value: 1.36, Prev: portalGatePrev{N: 5, Median: 1.226, MAD: 0.01}}}
	gated := []core.Datapoint{{Name: items[0].Name, Value: 1.36, Unit: "s", Direction: core.LowerIsBetter, MinDelta: 15}}

	out := localGateItems(gated, items, pg)
	if r, _, n := gateCounts(out); r != 0 || n != 0 {
		t.Errorf("counts = %d regressed / %d no_signal, want neither for a floor veto", r, n)
	}
	if countVerdict(out, verdictBelowFloor) != 1 {
		t.Errorf("the veto must be reported, got %v", out[0].Verdict)
	}
}

func TestHistoryGateKeepsMsMetricsWithoutADeclaredMinDelta(t *testing.T) {
	pg := portalGateConfig{MADK: 3, FloorPct: 5, MinRuns: 5}
	items := []portalGateItem{{Name: "tpcc.latency_p95", Value: 205, Prev: portalGatePrev{N: 5, Median: 5, MAD: 0.1}}}
	gated := []core.Datapoint{{Name: items[0].Name, Value: 205, Unit: "ms", Direction: core.LowerIsBetter}}

	if got := localGateItems(gated, items, pg)[0].Verdict; got != "regressed" {
		t.Errorf("5ms -> 205ms: verdict %q, want regressed; the 250ms floor belongs to the live arm", got)
	}
}
