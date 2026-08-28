package perf

import (
	"math"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// TestRegressionPct: the percent change is expressed in the metric's worse
// direction, and a non-positive reference is refused rather than divided by.
func TestRegressionPct(t *testing.T) {
	cases := []struct {
		name     string
		from, to float64
		dir      core.Direction
		want     float64
		wantOK   bool
	}{
		{"throughput halved is a 50pct drop", 1000, 500, core.HigherIsBetter, 50, true},
		{"throughput doubled is -100pct (improved)", 500, 1000, core.HigherIsBetter, -100, true},
		{"latency 5x is +400pct worse", 3704, 18520, core.LowerIsBetter, 400, true},
		{"latency halved is -50pct (improved)", 100, 50, core.LowerIsBetter, -50, true},
		{"flat is 0", 42, 42, core.LowerIsBetter, 0, true},
		{"zero reference is refused", 0, 100, core.LowerIsBetter, 0, false},
		{"negative reference is refused", -1, 100, core.HigherIsBetter, 0, false},
		{"near-zero reference still divides safely", 1e-9, 1e-9, core.LowerIsBetter, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := regressionPct(c.from, c.to, c.dir)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && math.Abs(got-c.want) > 1e-6 {
				t.Errorf("pct = %v, want %v", got, c.want)
			}
		})
	}
}

// TestIterDriftPct: first kept iteration vs last, and a single iteration (or
// none) has no drift to report.
func TestIterDriftPct(t *testing.T) {
	cases := []struct {
		name   string
		vals   []float64
		dir    core.Direction
		want   float64
		wantOK bool
	}{
		{"tps drifting down", []float64{1000, 900, 800}, core.HigherIsBetter, 20, true},
		{"latency drifting up", []float64{10, 15, 20}, core.LowerIsBetter, 100, true},
		{"single iteration", []float64{1000}, core.HigherIsBetter, 0, false},
		{"no iterations", nil, core.HigherIsBetter, 0, false},
		{"zero first iteration", []float64{0, 900}, core.HigherIsBetter, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := iterDriftPct(BenchStats{Values: c.vals}, c.dir)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && math.Abs(got-c.want) > 1e-6 {
				t.Errorf("drift = %v, want %v", got, c.want)
			}
		})
	}
}

// series builds a progress series with the given tps readings.
func series(tps ...float64) []SamplePoint {
	pts := make([]SamplePoint, len(tps))
	for i, v := range tps {
		pts[i] = SamplePoint{ElapsedSec: float64(i + 1), TPS: v, LatencyMS: v}
	}
	return pts
}

// TestSeriesDecayPct: first third vs last third, and a series too short to split
// into thirds yields nothing rather than panicking on an empty slice.
func TestSeriesDecayPct(t *testing.T) {
	cases := []struct {
		name   string
		series []SamplePoint
		dir    core.Direction
		want   float64
		wantOK bool
	}{
		{"tps decaying across the run", series(100, 100, 100, 60, 50, 50), core.HigherIsBetter, 50, true},
		{"tps steady", series(100, 100, 100, 100, 100, 100), core.HigherIsBetter, 0, true},
		{"tps ramping up is negative", series(50, 50, 50, 100, 100, 100), core.HigherIsBetter, -100, true},
		{"exactly three samples", series(100, 80, 50), core.HigherIsBetter, 50, true},
		{"two samples cannot be split", series(100, 50), core.HigherIsBetter, 0, false},
		{"one sample", series(100), core.HigherIsBetter, 0, false},
		{"empty series", nil, core.HigherIsBetter, 0, false},
		{"nil accessor", series(100, 80, 50), core.HigherIsBetter, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pick := pointTPS
			if c.name == "nil accessor" {
				pick = nil
			}
			got, ok := seriesDecayPct(c.series, pick, c.dir)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && math.Abs(got-c.want) > 1e-6 {
				t.Errorf("decay = %v, want %v", got, c.want)
			}
		})
	}
}

// TestRunDecayPct: the reported decay is the MEDIAN across the kept iterations,
// and iterations with no usable series are skipped.
func TestRunDecayPct(t *testing.T) {
	kept := []Sample{
		{Series: series(100, 100, 100, 50, 50, 50)}, // 50%
		{Series: series(100, 100, 100, 90, 90, 90)}, // 10%
		{Series: series(100, 100, 100, 70, 70, 70)}, // 30%
		{Series: series(100)},                       // skipped
		{},                                          // skipped
	}
	got, ok := runDecayPct(kept, pointTPS, core.HigherIsBetter)
	if !ok {
		t.Fatal("ok = false, want a decay from the three usable iterations")
	}
	if math.Abs(got-30) > 1e-6 {
		t.Errorf("decay = %v, want the median 30", got)
	}
	if _, ok := runDecayPct([]Sample{{Series: series(100)}}, pointTPS, core.HigherIsBetter); ok {
		t.Error("a run with no splittable series reported a decay")
	}
	if _, ok := runDecayPct(nil, pointTPS, core.HigherIsBetter); ok {
		t.Error("an empty run reported a decay")
	}
}

// TestBenchShapeGateFiresOnIterationDrift: the insert-after-truncate shape (a
// fast first pass, then permanently slower) crosses the drift limit and names
// the metric — with no stored baseline involved.
func TestBenchShapeGateFiresOnIterationDrift(t *testing.T) {
	res := BenchResult{
		Warmup: &Sample{LatencyAvgMS: 3704},
		Kept: []Sample{
			{LatencyAvgMS: 18499},
			{LatencyAvgMS: 17419},
			{LatencyAvgMS: 21267},
		},
	}
	res.LatencyAvg = aggregate([]float64{18499, 17419, 21267})
	specs := []metricSpec{{
		Name: "x.latency_avg", Unit: "ms", Dir: core.LowerIsBetter, Stats: res.LatencyAvg,
		FromSample: sampleLatencyAvg,
	}}

	err := benchShapeGate(res, specs, benchGates{MaxIterDriftPct: 10, MaxDecayPct: 35})
	if err == nil {
		t.Fatal("gate passed on a 15 pct iteration drift with a 10 pct limit")
	}
	if !strings.Contains(err.Error(), "x.latency_avg drifted") {
		t.Errorf("error = %q, want it to name the drifted metric", err)
	}
}

// TestBenchShapeGateDegenerateResults: a single-iteration run, a run with no
// warmup, and an empty result must produce no assertion and no panic.
func TestBenchShapeGateDegenerateResults(t *testing.T) {
	specs := func(res BenchResult) []metricSpec {
		return []metricSpec{{
			Name: "x.tps", Unit: "tps", Dir: core.HigherIsBetter, Stats: res.TPS,
			FromSample: sampleTPS, FromPoint: pointTPS,
		}}
	}
	gates := benchGates{MaxIterDriftPct: 10, MaxDecayPct: 10}

	single := BenchResult{Kept: []Sample{{TPS: 500}}}
	single.TPS = aggregate([]float64{500})
	if err := benchShapeGate(single, specs(single), gates); err != nil {
		t.Errorf("single-iteration run failed the shape gate: %v", err)
	}

	noWarmup := BenchResult{Kept: []Sample{{TPS: 500}, {TPS: 490}}}
	noWarmup.TPS = aggregate([]float64{500, 490})
	if err := benchShapeGate(noWarmup, specs(noWarmup), gates); err != nil {
		t.Errorf("a 2 pct drift failed a 10 pct gate: %v", err)
	}

	zeroWarmup := BenchResult{Warmup: &Sample{}, Kept: []Sample{{TPS: 500}}}
	zeroWarmup.TPS = aggregate([]float64{500})
	if err := benchShapeGate(zeroWarmup, specs(zeroWarmup), gates); err != nil {
		t.Errorf("zero-valued warmup failed the shape gate: %v", err)
	}

	if err := benchShapeGate(BenchResult{}, specs(BenchResult{}), gates); err != nil {
		t.Errorf("empty result failed the shape gate: %v", err)
	}
}

// TestBenchShapeGateDisabledLimits: a limit of 0 drops the assertion.
func TestBenchShapeGateDisabledLimits(t *testing.T) {
	res := BenchResult{Kept: []Sample{
		{TPS: 1000, Series: series(1000, 1000, 1000, 100, 100, 100)},
		{TPS: 100, Series: series(1000, 1000, 1000, 100, 100, 100)},
	}}
	res.TPS = aggregate([]float64{1000, 100})
	specs := []metricSpec{{
		Name: "x.tps", Unit: "tps", Dir: core.HigherIsBetter, Stats: res.TPS,
		FromSample: sampleTPS, FromPoint: pointTPS,
	}}

	if err := benchShapeGate(res, specs, benchGates{}); err != nil {
		t.Errorf("both limits disabled still failed: %v", err)
	}
	if err := benchShapeGate(res, specs, benchGates{MaxDecayPct: 35}); err == nil || !strings.Contains(err.Error(), "decayed") {
		t.Errorf("decay limit did not fire: %v", err)
	}
}
