package perf

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

func TestSignTestK(t *testing.T) {
	for n, want := range map[int]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 5, 10: 8} {
		if got := signTestK(n); got != want {
			t.Errorf("signTestK(%d) = %d, want %d", n, got, want)
		}
	}
}

func pairedGate(t *testing.T, pairs []float64, pol liveArmPolicy) (string, error) {
	t.Helper()
	cur := []core.Datapoint{{Name: "x.tps", Value: 70, Unit: "tps", Direction: core.HigherIsBetter}}
	base := []core.Datapoint{{Name: "x.tps", Value: 100, Unit: "tps", Direction: core.HigherIsBetter}}
	pol.Pairs = map[string][]float64{"x.tps": pairs}
	table, _, err := liveArmGate(cur, base, nil, nil, pol, "0.1")
	return table, err
}

func TestLiveArmGatePairsNeedSignAgreement(t *testing.T) {
	pol := strictPolicy(float64Thresholds{DropPct: 20, RisePct: 35})
	if _, err := pairedGate(t, []float64{30, 28, 35}, pol); err == nil {
		t.Error("three pairs all 28-35% worse must fail")
	}
	table, err := pairedGate(t, []float64{60, 30, -5}, pol)
	if err != nil {
		t.Errorf("a median of 30%% carried by two of three pairs is split, not a regression: %v", err)
	}
	if !strings.Contains(table, "2/3") || !strings.Contains(table, "split") {
		t.Errorf("the table must show the pair count and the split verdict:\n%s", table)
	}
}

func TestLiveArmGateUsesMedianPairDelta(t *testing.T) {
	pol := strictPolicy(float64Thresholds{DropPct: 20, RisePct: 35})
	// The aggregates say -30%, the pairs say 5-10%: the pairs are the measurement.
	if _, err := pairedGate(t, []float64{5, 10, 8}, pol); err != nil {
		t.Errorf("median pair delta 8%% is inside 20%%: %v", err)
	}
}

func TestLiveArmGateAABandReplacesFlatThreshold(t *testing.T) {
	pol := strictPolicy(float64Thresholds{DropPct: 20, RisePct: 35})
	pol.AABands = map[string]float64{"x.tps": 6}
	if _, err := pairedGate(t, []float64{9, 10, 11}, pol); err == nil {
		t.Error("10% worse against a 6% A/A band must fail even though the flat threshold is 20%")
	}
}

func TestLiveArmGateNoisyIsNotFailed(t *testing.T) {
	pol := strictPolicy(float64Thresholds{DropPct: 20, RisePct: 35})
	pol.AABands = map[string]float64{"x.tps": 70}
	pol.MaxBandPct = 50
	table, err := pairedGate(t, []float64{80, 90, 95}, pol)
	if err != nil {
		t.Errorf("an allowance past max_band_pct is reported, not failed: %v", err)
	}
	if !strings.Contains(table, "noisy") {
		t.Errorf("want a noisy verdict:\n%s", table)
	}
}

func TestProvisionPairs(t *testing.T) {
	spec := benchWorkloadSpec{datapoints: pgbenchDatapoints}
	bench := func(tps float64) BenchResult { return BenchResult{TPS: aggregate([]float64{tps})} }
	got := provisionPairs(spec, []BenchResult{bench(90), bench(100)}, []BenchResult{bench(100), bench(80)})
	if l := got["pgbench.tps"]; len(l) != 2 || l[0] != 10 || l[1] != -25 {
		t.Errorf("pairs = %v, want [10 -25]", l)
	}
}

func TestRateBarrierTakesLowestPositive(t *testing.T) {
	b := newRateBarrier(3)
	b.arrive(300)
	b.arrive(0)
	b.arrive(0) // a second arrival from a finished barrier is ignored
	b.arrive(250)
	got, err := b.wait(context.Background())
	if err != nil || got != 300 {
		t.Errorf("wait = %v, %v; want 300 (the third arrival closed it)", got, err)
	}
	b = newRateBarrier(2)
	b.arrive(400)
	b.arrive(250)
	if got, _ := b.wait(context.Background()); got != 250 {
		t.Errorf("want the lowest proposal 250, got %v", got)
	}
}

type rateWorkload struct {
	tps   float64
	rates []float64
}

func (w *rateWorkload) Name() string                                       { return "rate" }
func (w *rateWorkload) Prepare(context.Context, exec.RemoteExecutor) error { return nil }
func (w *rateWorkload) RunOnce(_ context.Context, _ exec.RemoteExecutor, p RunParams) (Sample, error) {
	w.rates = append(w.rates, p.Rate)
	if p.Rate > 0 {
		return Sample{TPS: p.Rate, LatencyAvgMS: 5, LatencyP95MS: 9}, nil
	}
	return Sample{TPS: w.tps, LatencyAvgMS: 20, LatencyP95MS: 40}, nil
}

func TestRunBenchmarkFixedRate(t *testing.T) {
	w := &rateWorkload{tps: 1000}
	bc := BenchConfig{Iterations: 2, FixedRateFraction: 0.6, MeasurementDuration: time.Second}
	res, err := RunBenchmark(context.Background(), nil, w, bc)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.rates) != 3 || w.rates[0] != 0 || w.rates[2] != 600 {
		t.Fatalf("rates = %v, want two unthrottled runs then one at 600", w.rates)
	}
	if res.FixedTarget != 600 || res.FixedP95.Median != 9 {
		t.Errorf("fixed target %v p95 %v, want 600 and 9", res.FixedTarget, res.FixedP95.Median)
	}

	w = &rateWorkload{tps: 1000}
	bc.RateGate = func(context.Context, float64) (float64, error) { return 450, nil }
	if _, err := RunBenchmark(context.Background(), nil, w, bc); err != nil {
		t.Fatal(err)
	}
	if w.rates[2] != 450 {
		t.Errorf("the agreed rate must win over the arm's own proposal, ran at %v", w.rates[2])
	}

	w = &rateWorkload{tps: 1000}
	bc.FixedRateFraction = 0
	if _, err := RunBenchmark(context.Background(), nil, w, bc); err != nil || len(w.rates) != 2 {
		t.Errorf("fraction 0 skips the fixed-rate run: rates %v err %v", w.rates, err)
	}
}

func TestBenchDatapointsGateThroughputAndFixedRateLatency(t *testing.T) {
	res := BenchResult{
		TPS: aggregate([]float64{900, 1000, 1100}), LatencyAvg: aggregate([]float64{9}),
		FixedTarget: 540, FixedTPS: aggregate([]float64{540}), FixedAvg: aggregate([]float64{4}),
		FixedP95: aggregate([]float64{7}), FixedP99: aggregate([]float64{11}),
	}
	gated, info := sysbenchDatapoints(res)
	names := func(dps []core.Datapoint) string {
		var out []string
		for _, dp := range dps {
			out = append(out, dp.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(gated); got != "sysbench.tps,sysbench.fixed_rate.latency_avg,sysbench.fixed_rate.latency_p95,sysbench.fixed_rate.latency_p99" {
		t.Errorf("gated = %s", got)
	}
	if !strings.Contains(names(info), "sysbench.latency_avg") || !strings.Contains(names(info), "sysbench.fixed_rate.tps") {
		t.Errorf("saturated latency and the achieved rate are reported, not gated: %s", names(info))
	}
	if gated[0].N != 3 || len(gated[0].Samples) != 3 {
		t.Errorf("tps must carry its n and samples: %+v", gated[0])
	}
	if gated[0].SpreadPct != 0 {
		t.Error("the cross-provision spread must not reach the history gate's spread veto")
	}
	if gated[1].MinDelta != benchLatencyFloorMS {
		t.Errorf("a benchmark latency floors at %vms, got %v", benchLatencyFloorMS, gated[1].MinDelta)
	}
}

func TestLocalGateItemsNoisyBand(t *testing.T) {
	pg := portalGateConfig{MADK: 3, FloorPct: 5, MinRuns: 3, MaxBandPct: 50}
	gated := []core.Datapoint{{Name: "lifecycle.load", Value: 200, Direction: core.LowerIsBetter}}
	items := []portalGateItem{{Name: "lifecycle.load", Value: 200, Prev: portalGatePrev{N: 10, Median: 100, MAD: 25}}}
	if got := localGateItems(gated, items, pg)[0].Verdict; got != verdictNoisy {
		t.Errorf("+100%% against a 75%% band: verdict %q, want %s", got, verdictNoisy)
	}
	if r, _, _ := gateCounts(localGateItems(gated, items, pg)); r != 0 {
		t.Error("a noisy datapoint must not count as regressed")
	}
}

func TestAALoadSignatureIsSeparate(t *testing.T) {
	c := core.NewConfig()
	id := identityFromConfig(c, "", "abc", LoadFields{Scale: 10, Harness: benchHarnessRev})
	if id.AALoadSignature == "" || id.AALoadSignature == id.LoadSignature {
		t.Fatalf("an A/B run must know the A/A signature and not share it: %+v", id)
	}
	aaCfg := c.Derive(map[string]string{"perf.baseline.aa": "bool:true"})
	aa := identityFromConfig(aaCfg, "", "abc", LoadFields{Scale: 10, Harness: benchHarnessRev})
	if aa.LoadSignature != id.LoadSignature {
		t.Error("a test with no live arm keeps its own signature on an A/A run")
	}
	if aa.forLiveArm(aaCfg).LoadSignature != id.AALoadSignature {
		t.Error("a live-arm run on an A/A run must store under the A/A signature")
	}
	if id.forLiveArm(c).LoadSignature != id.LoadSignature {
		t.Error("an A/B run keeps its signature")
	}
}

func TestIsolateBacking(t *testing.T) {
	cfg := perfFromConfig(core.NewConfig())
	y := cfg.isolateBacking(ybk8s.Config{TServerCPU: "0.5"})
	if !y.GuaranteedQoS || y.TServerCPU != DefaultIsolationTServerCPU || y.NodeSelector != DefaultArmNodeSelector {
		t.Errorf("isolated backing = %+v", y)
	}
	if got := cfg.isolateBacking(ybk8s.Config{NodeSelector: "a=b"}).NodeSelector; got != "a=b" {
		t.Errorf("a selector the run set must win, got %q", got)
	}
	if got := cfg.isolateEndpoints(ampstack.Config{}).EndpointNodeSelector; got != DefaultArmNodeSelector {
		t.Errorf("endpoints must be pinned too, got %q", got)
	}
	cfg.Isolation.Enabled = false
	if y := cfg.isolateBacking(ybk8s.Config{TServerCPU: "0.5"}); y.GuaranteedQoS || y.TServerCPU != "0.5" {
		t.Errorf("isolation off keeps the configured shape: %+v", y)
	}
	if sh := cfg.clientShape(); sh.CPU != "" || len(sh.NodeSelector) != 1 {
		t.Errorf("isolation off still pins the client but leaves it unbounded: %+v", sh)
	}
}

func TestParsePgbenchPercentiles(t *testing.T) {
	p95, p99, err := parsePgbenchPercentiles("12345 45678\n")
	if err != nil || p95 != 12.345 || p99 != 45.678 {
		t.Errorf("got %v %v %v", p95, p99, err)
	}
	if _, _, err := parsePgbenchPercentiles(""); err == nil {
		t.Error("empty output must be an error")
	}
	if !strings.Contains(pgbenchLogPercentilesCmd("/tmp/x"), "rm -f '/tmp/x'.*") {
		t.Error("the per-transaction logs must be removed after reading")
	}
}

func TestParseLFCCounters(t *testing.T) {
	h, m, err := parseLFCCounters("CREATE EXTENSION\n900 100\n")
	if err != nil || h != 900 || m != 100 {
		t.Errorf("got %v %v %v", h, m, err)
	}
	if _, _, err := parseLFCCounters("oops"); err == nil {
		t.Error("garbage must be an error")
	}
}

func TestIsolationSuffixSeparatesSeries(t *testing.T) {
	cfg := perfFromConfig(core.NewConfig())
	if cfg.isolationSuffix() != "" || benchHarness(cfg) != benchHarnessRev {
		t.Errorf("isolated runs carry the bare rev, got %q", benchHarness(cfg))
	}
	cfg.Isolation.Enabled = false
	if cfg.isolationSuffix() != "-shared" || benchHarness(cfg) != benchHarnessRev+"-shared" {
		t.Errorf("unisolated runs are a separate series, got %q", benchHarness(cfg))
	}
}

func TestRunBenchmarkFixedRateFailureKeepsSaturated(t *testing.T) {
	w := &rateWorkload{tps: 1000}
	bc := BenchConfig{Iterations: 2, FixedRateFraction: 0.6, MeasurementDuration: time.Second,
		RateGate: func(context.Context, float64) (float64, error) { return 0, context.Canceled }}
	res, err := RunBenchmark(context.Background(), nil, w, bc)
	if err != nil {
		t.Fatalf("a failed fixed-rate window must not fail the benchmark: %v", err)
	}
	if res.FixedErr == nil || len(res.Fixed) != 0 || res.TPS.Median != 1000 {
		t.Errorf("want the saturated result with FixedErr set and no Fixed: %+v", res)
	}
	if specs := fixedRateSpecs("x", "latency_avg", res); specs != nil {
		t.Errorf("no fixed-rate datapoints without a fixed-rate run: %+v", specs)
	}
}

func TestFixedRateSpecsNamesAndZeros(t *testing.T) {
	res := BenchResult{FixedTarget: 100, FixedTPS: aggregate([]float64{100}), FixedAvg: aggregate([]float64{0}),
		FixedP95: aggregate([]float64{7})}
	var names []string
	for _, m := range fixedRateSpecs("tpcc", "latency_median", res) {
		names = append(names, m.Name)
	}
	if got := strings.Join(names, ","); got != "tpcc.fixed_rate.tps,tpcc.fixed_rate.latency_p95" {
		t.Errorf("specs = %s: an unreported (zero) centre latency is left out, TPC-C's is its median", got)
	}
	res.FixedAvg = aggregate([]float64{3})
	if specs := fixedRateSpecs("tpcc", "latency_median", res); specs[1].Name != "tpcc.fixed_rate.latency_median" {
		t.Errorf("TPC-C's fixed-rate centre must match tpcc.latency_median, got %s", specs[1].Name)
	}
}

func TestLiveArmGateSingleViolationFailsBench(t *testing.T) {
	pol := liveArmPolicy{Thr: float64Thresholds{DropPct: 20, RisePct: 35}, MinViolations: 2, GrossFactor: 2}
	if _, err := pairedGate(t, []float64{30, 29, 31}, pol); err != nil {
		t.Fatalf("under the quorum a lone 30%% drop only warns: %v", err)
	}
	pol.MinViolations = 1
	if _, err := pairedGate(t, []float64{30, 29, 31}, pol); err == nil {
		t.Error("with the bench quorum of 1 a lone 30% throughput drop on every pair must fail")
	}
}

func TestOpsDatapointsGateEndToEndOnly(t *testing.T) {
	var results []phaseResult
	for _, g := range []string{"cluster_create", "cluster_create_api", "cluster_create_task", "seed", "transport_floor"} {
		results = append(results, phaseResult{Name: g, Group: g, MS: 100}, phaseResult{Name: g, Group: g, MS: 120})
	}
	gated, info := opsDatapoints(results)
	var g, i []string
	for _, dp := range gated {
		g = append(g, dp.Name)
	}
	for _, dp := range info {
		i = append(i, dp.Name)
	}
	if got := strings.Join(g, ","); got != "ops.cluster_create" {
		t.Errorf("gated = %s, want only the end-to-end time", got)
	}
	for _, want := range []string{"ops.cluster_create_api", "ops.cluster_create_task", "ops.seed", "ops.transport_floor", "ops.cluster_create.cv"} {
		if !strings.Contains(strings.Join(i, ","), want) {
			t.Errorf("%s must be reported as info, info = %v", want, i)
		}
	}
}

func TestLFCHitPct(t *testing.T) {
	if got, ok := lfcHitPct(100, 50, 190, 60); !ok || got != 90 {
		t.Errorf("90 hits 10 misses = %v %v, want 90", got, ok)
	}
	if _, ok := lfcHitPct(100, 50, 100, 50); ok {
		t.Error("a run that never touched the cache has no ratio")
	}
	if _, ok := lfcHitPct(100, 50, 10, 5); ok {
		t.Error("counters that went backwards (a restart) are no ratio")
	}
}

func TestPgbenchUncachedCapsAndSpecs(t *testing.T) {
	p := pgbenchUncachedParamsFromConfig(core.NewConfig())
	s := p.pgSettings()
	if s["shared_buffers"] != DefaultPgbenchUncachedSharedBuffers || s["yb_amp.file_cache_size_limit"] != DefaultPgbenchUncachedFileCache || len(s) != 2 {
		t.Errorf("pg settings = %v", s)
	}
	res := BenchResult{TPS: aggregate([]float64{500}), CacheHit: aggregate([]float64{12})}
	var names []string
	for _, m := range pgbenchUncachedSpecs(res) {
		names = append(names, m.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "pgbench_uncached.tps") || strings.Contains(joined, "pgbench.") || !strings.Contains(joined, "pgbench_uncached.lfc_hit_pct") {
		t.Errorf("specs = %s", joined)
	}
	res.CacheHit = aggregate([]float64{0})
	for _, m := range pgbenchUncachedSpecs(res) {
		if m.Name == "pgbench_uncached.lfc_hit_pct" {
			t.Error("an unread hit ratio must not be reported as 0%")
		}
	}
}

func TestCompareSettings(t *testing.T) {
	want := map[string]string{"shared_buffers": "128MB", "yb_amp.file_cache_size_limit": "128MB"}
	names := []string{"shared_buffers", "yb_amp.file_cache_size_limit"}
	if err := compareSettings(names, want, "CREATE EXTENSION\n128MB 128MB\n"); err != nil {
		t.Errorf("matching settings: %v", err)
	}
	err := compareSettings(names, want, "2048MB 128MB\n")
	if err == nil || !strings.Contains(err.Error(), "shared_buffers = 2048MB") {
		t.Errorf("a cap that did not apply must fail and say which: %v", err)
	}
	if compareSettings(names, want, "128MB\n") == nil {
		t.Error("a short answer must fail")
	}
}

func TestSingleStackPairNeedsABand(t *testing.T) {
	cur := []core.Datapoint{{Name: "microbench.insert_cold", Value: 3206, Unit: "ms", Direction: core.LowerIsBetter}}
	base := []core.Datapoint{{Name: "microbench.insert_cold", Value: 1687, Unit: "ms", Direction: core.LowerIsBetter}}
	pol := liveArmPolicy{Thr: float64Thresholds{DropPct: 20, RisePct: 35}, MinViolations: 1, GrossFactor: 2, RequireBand: true, UseHistoryBand: true}
	table, _, err := liveArmGate(cur, base, nil, nil, pol, "0.260923.0")
	if err != nil || !strings.Contains(table, "no_band") {
		t.Errorf("one timed statement per arm, no band yet: must report no_band, not fail: %v\n%s", err, table)
	}
	pol.Bands = map[string]float64{"microbench.insert_cold": 40}
	if _, _, err := liveArmGate(cur, base, nil, nil, pol, "0.260923.0"); err == nil {
		t.Error("with a measured band a 90% rise past it (and past the gross bar) still fails")
	}
}

func TestChainDatapointsGateP50Only(t *testing.T) {
	reads := map[int][]float64{1: {2, 2, 2, 30}, 20: {3, 3, 3, 3}}
	gated, info, err := chainDatapoints(reads, chainParams{Depths: []int{1, 20}})
	if err != nil {
		t.Fatal(err)
	}
	for _, dp := range gated {
		if strings.Contains(dp.Name, "p99") {
			t.Errorf("a p99 of ~3 reads must not gate: %s", dp.Name)
		}
	}
	if len(gated) != 1 || gated[0].Name != chainPrefix+"p50_ratio@depth20" || gated[0].Value != 1.5 {
		t.Errorf("want only the depth-20 p50 over the depth-1 p50 gated (1.5x), got %+v", gated)
	}
	names := map[string]bool{}
	for _, dp := range info {
		names[dp.Name] = true
	}
	for _, want := range []string{"p99_ms@depth1", "p50_ms@depth1", "p50_ms@depth20"} {
		if !names[chainPrefix+want] {
			t.Errorf("%s must still be reported", want)
		}
	}
}

func TestShapeGateSkipsDriftOnSaturatedTails(t *testing.T) {
	g := benchGates{MaxIterDriftPct: 50, MaxDecayPct: 35}
	tail := metricSpec{Name: "pgbench.latency_p99", Dir: core.LowerIsBetter, Stats: aggregate([]float64{20.5, 36.5}), Info: true, NoDrift: true}
	if err := benchShapeGate(BenchResult{}, []metricSpec{tail}, g); err != nil {
		t.Errorf("a saturated tail percentile must not trip the drift gate: %v", err)
	}
	avg := metricSpec{Name: "pgbench.latency_avg", Dir: core.LowerIsBetter, Stats: aggregate([]float64{10, 20}), Info: true}
	if err := benchShapeGate(BenchResult{}, []metricSpec{avg}, g); err == nil {
		t.Error("average latency drift is still judged, reported-only or not")
	}
	for _, m := range pgbenchMetricSpecs(BenchResult{}) {
		if m.Name == "pgbench.latency_avg" && m.NoDrift {
			t.Error("pgbench.latency_avg keeps its drift gate")
		}
		if m.Name == "pgbench.latency_p99" && !m.NoDrift {
			t.Error("pgbench.latency_p99 skips the drift gate")
		}
	}
}

func TestOpsDatapointsKeepThinGroupsOutOfTheGate(t *testing.T) {
	var results []phaseResult
	for i := 0; i < 10; i++ {
		results = append(results, phaseResult{Group: "endpoint_create_warm", MS: 100})
	}
	results = append(results, phaseResult{Group: "endpoint_create_cold", MS: 900}, phaseResult{Group: "endpoint_create_cold", MS: 950})
	gated, info := opsDatapoints(results)
	if len(gated) != 1 || gated[0].Name != "ops.endpoint_create_warm" {
		t.Errorf("gated = %+v, want only the 10-sample group", gated)
	}
	found := false
	for _, dp := range info {
		found = found || dp.Name == "ops.endpoint_create_cold"
	}
	if !found {
		t.Errorf("the 2-sample group must be reported, info = %+v", info)
	}

	// Repeats below the floor: the full-count groups still gate.
	few := []phaseResult{{Group: "resize", MS: 1}, {Group: "resize", MS: 2}, {Group: "resize", MS: 3}}
	if gated, _ := opsDatapoints(few); len(gated) != 1 {
		t.Errorf("with 3 repeats every full group gates, got %+v", gated)
	}
}

func TestIntervalTicksCountsSurvivorsOnly(t *testing.T) {
	prev := parseProcStat("100 (postgres) S 0 0 0 0 0 0 0 0 0 0 50 50 0 0 0 0 0 0 0 0 10\n"+
		"101 (postgres) S 0 0 0 0 0 0 0 0 0 0 500 500 0 0 0 0 0 0 0 0 10\n", time.Now())
	// 101 exited with 1000 lifetime ticks; 100 used 10; 102 was born with 4.
	cur := parseProcStat("100 (postgres) S 0 0 0 0 0 0 0 0 0 0 55 55 0 0 0 0 0 0 0 0 10\n"+
		"102 (postgres) S 0 0 0 0 0 0 0 0 0 0 2 2 0 0 0 0 0 0 0 0 10\n", time.Now())
	if got := intervalTicks(prev, cur)["postgres"]; got != 14 {
		t.Errorf("postgres ticks = %v, want 14 (the survivor's 10 plus the newborn's 4)", got)
	}
}

func TestIdleTotalIsTheMedianOfPerIntervalSums(t *testing.T) {
	usage := []procUsage{
		{Name: "pageserver", CPURates: []float64{10, 10, 40}, CPUMilli: 10},
		{Name: "safekeeper", CPURates: []float64{1, 30, 1}, CPUMilli: 1},
	}
	gated, _ := idleDatapoints(usage)
	for _, dp := range gated {
		if dp.Name == idlePrefix+"cpu_mcores_total" && dp.Value != 40 {
			t.Errorf("total = %v, want the median of 11, 40, 41 = 40", dp.Value)
		}
	}
}

func TestBranchSlicesAreDisjointAndCover(t *testing.T) {
	for _, c := range []struct{ rows, n int }{{25000, 5}, {25003, 5}, {7, 5}, {3, 5}, {10, 1}} {
		want := int64(1)
		for _, sl := range branchWriteSlices(c.rows, c.n) {
			if sl[0] != want || sl[1] < sl[0] {
				t.Fatalf("rows %d n %d: slice %v does not start at %d", c.rows, c.n, sl, want)
			}
			want = sl[1] + 1
		}
		if want != int64(c.rows)+1 {
			t.Errorf("rows %d n %d: slices end at %d, want %d", c.rows, c.n, want-1, c.rows)
		}
	}
	for _, c := range []struct {
		blocks int64
		n      int
		want   int
	}{{28572, 5, 5}, {3, 5, 1}, {10, 1, 1}} {
		got := branchScanSlices(c.blocks, c.n)
		if len(got) != c.want || !strings.HasPrefix(got[0], "ctid >= '(0,0)'::tid") || strings.Contains(got[len(got)-1], "<") {
			t.Errorf("blocks %d n %d: %v, want %d slices from block 0, the last open-ended", c.blocks, c.n, got, c.want)
		}
		for i := 1; i < len(got); i++ {
			prevHi := got[i-1][strings.LastIndex(got[i-1], "<"):]
			lo := strings.TrimPrefix(strings.SplitN(got[i], " AND", 2)[0], "ctid >= ")
			if !strings.Contains(prevHi, lo) {
				t.Errorf("blocks %d: slice %d starts at %s, previous ends %s", c.blocks, i, lo, prevHi)
			}
		}
	}
}

func TestGzipStartsItsOwnSeries(t *testing.T) {
	load := LoadFields{Scale: 10, Harness: benchHarnessRev}
	sig := func(mode string) Identity {
		return identityFromConfig(core.NewConfig().Derive(map[string]string{"env.yb_stream_compression": mode}), "", "abc", load)
	}
	gzip, none := sig("gzip"), sig("none")
	if gzip.LoadSignature == none.LoadSignature || gzip.LoadShape == none.LoadShape {
		t.Error("gzip and uncompressed numbers must not share a series or fall back to one by shape")
	}
	if none.LoadSignature != LoadSignature(load) {
		t.Error("none is YB's default and must keep the series that predates the knob")
	}
}
