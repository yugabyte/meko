package perf

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// strictPolicy fails on the first datapoint past its allowance, which is what the
// per-datapoint mechanics below are about. Quorum and bands have their own tests.
func strictPolicy(thr float64Thresholds) liveArmPolicy {
	return liveArmPolicy{Thr: thr, MinViolations: 1}
}

func TestLiveArmGateJudgesByDirection(t *testing.T) {
	thr := float64Thresholds{DropPct: 20, RisePct: 35}
	cur := []core.Datapoint{
		{Name: "x.tps", Value: 70, Unit: "tps", Direction: core.HigherIsBetter},
		{Name: "x.lat", Value: 120, Unit: "ms", Direction: core.LowerIsBetter},
		{Name: "x.only_current", Value: 1, Unit: "ms", Direction: core.LowerIsBetter},
	}
	base := []core.Datapoint{
		{Name: "x.tps", Value: 100, Unit: "tps", Direction: core.HigherIsBetter},
		{Name: "x.lat", Value: 100, Unit: "ms", Direction: core.LowerIsBetter},
	}
	table, _, err := liveArmGate(cur, base, nil, nil, strictPolicy(thr), "0.1.2-abc")
	if err == nil || !strings.Contains(err.Error(), "x.tps") {
		t.Fatalf("a 30%% throughput drop against a 20%% allowance must fail naming x.tps; got %v", err)
	}
	if strings.Contains(err.Error(), "x.lat") {
		t.Errorf("a 20%% latency rise is inside the 35%% allowance, but the verdict names x.lat: %v", err)
	}
	if strings.Contains(table, "only_current") {
		t.Errorf("a datapoint with no baseline counterpart must be skipped, but the table shows it:\n%s", table)
	}
}

func TestLiveArmGateImprovementPasses(t *testing.T) {
	thr := float64Thresholds{DropPct: 20, RisePct: 35}
	cur := []core.Datapoint{
		{Name: "x.tps", Value: 150, Unit: "tps", Direction: core.HigherIsBetter},
		{Name: "x.lat", Value: 50, Unit: "ms", Direction: core.LowerIsBetter},
	}
	base := []core.Datapoint{
		{Name: "x.tps", Value: 100, Unit: "tps", Direction: core.HigherIsBetter},
		{Name: "x.lat", Value: 100, Unit: "ms", Direction: core.LowerIsBetter},
	}
	if _, _, err := liveArmGate(cur, base, nil, nil, strictPolicy(thr), "0.1.2-abc"); err != nil {
		t.Fatalf("an improvement on both axes must pass: %v", err)
	}
}

func TestLiveArmDatapointsSuffixed(t *testing.T) {
	out := liveArmDatapoints([]core.Datapoint{{Name: "x.tps", Value: 100}})
	if len(out) != 1 || out[0].Name != "x.tps.baseline" {
		t.Fatalf("baseline datapoints = %+v, want x.tps.baseline", out)
	}
}

func TestLiveArmGateSpreadGuard(t *testing.T) {
	thr := float64Thresholds{DropPct: 20, RisePct: 35}
	base := []core.Datapoint{{Name: "x.load", Value: 45000, Unit: "ms", Direction: core.LowerIsBetter}}
	cur := func(spread float64) []core.Datapoint {
		return []core.Datapoint{{Name: "x.load", Value: 105000, Unit: "ms", Direction: core.LowerIsBetter, SpreadPct: spread}}
	}

	if _, _, err := liveArmGate(cur(0), base, nil, nil, strictPolicy(thr), "0.1.2-abc"); err == nil {
		t.Fatal("a +133% delta must fail when the arm reports no spread")
	}
	table, _, err := liveArmGate(cur(200), base, nil, nil, strictPolicy(thr), "0.1.2-abc")
	if err != nil {
		t.Errorf("a delta inside the arm's own 200%% spread is a draw, not a regression: %v", err)
	}
	if !strings.Contains(table, "spread%") {
		t.Errorf("the table must carry the spread column so the skip is visible:\n%s", table)
	}
	if _, _, err := liveArmGate(cur(10), base, nil, nil, strictPolicy(thr), "0.1.2-abc"); err == nil {
		t.Error("a delta far outside a 10% spread must still fail")
	}
}

// The best-iteration check is the stronger evidence: where the best draw also
// regressed, the cruder spread veto must not throw the verdict away.
func TestLiveArmGateSpreadVetoYieldsToBestIterationCorroboration(t *testing.T) {
	thr := float64Thresholds{DropPct: 20, RisePct: 35}
	cur := []core.Datapoint{{Name: "x.load", Value: 105000, Unit: "ms", Direction: core.LowerIsBetter, SpreadPct: 200}}
	base := []core.Datapoint{{Name: "x.load", Value: 45000, Unit: "ms", Direction: core.LowerIsBetter}}
	curBest := map[string]float64{"x.load": 105000}
	baseBest := map[string]float64{"x.load": 45000}

	if _, _, err := liveArmGate(cur, base, curBest, baseBest, strictPolicy(thr), "0.1.2-abc"); err == nil {
		t.Error("the best draw regressed +133% too, so the spread veto must not suppress it")
	}
	// The best draw did not regress: no corroboration, and the veto still applies.
	quietBest := map[string]float64{"x.load": 46000}
	if _, _, err := liveArmGate(cur, base, quietBest, baseBest, strictPolicy(thr), "0.1.2-abc"); err != nil {
		t.Errorf("an uncorroborated median delta must not fail: %v", err)
	}
}

func msDP(name string, v, spread float64) core.Datapoint {
	return core.Datapoint{Name: name, Value: v, Unit: "ms", Direction: core.LowerIsBetter, SpreadPct: spread}
}

// Run fbf0de9f: delete_fork_branch 2969 vs 1383 ms, +114.6%, steady inside the
// run at 27.3% spread. Its own population runs 906ms to 2969ms on builds that
// changed nothing, so a 35% flat allowance cannot judge it and the trailing MAD
// can.
func TestLiveArmGateHistoryBandCoversAJumpyMetric(t *testing.T) {
	pol := liveArmPolicy{
		Thr: float64Thresholds{DropPct: 20, RisePct: 35}, UseHistoryBand: true,
		GrossFactor: 2, MinViolations: 2,
	}
	cur := []core.Datapoint{msDP("lifecycle.delete_fork_branch", 2968.746, 27.3)}
	base := []core.Datapoint{msDP("lifecycle.delete_fork_branch", 1383.212, 0)}

	if _, _, err := liveArmGate(cur, base, nil, nil, pol, "0.260907.0"); err == nil {
		t.Fatal("with no history the flat 35% stands and +114.6% must fail")
	}

	pol.Bands = map[string]float64{"lifecycle.delete_fork_branch": 130}
	table, _, err := liveArmGate(cur, base, nil, nil, pol, "0.260907.0")
	if err != nil {
		t.Errorf("a delta inside the metric's own trailing dispersion is not a regression: %v", err)
	}
	if !strings.Contains(table, "130.0%") {
		t.Errorf("the table must show the band that was applied:\n%s", table)
	}

	// A steady metric keeps the flat allowance, so the same delta still fails.
	pol.Bands = map[string]float64{"lifecycle.load": 12}
	if _, _, err := liveArmGate(cur, base, nil, nil, pol, "0.260907.0"); err == nil {
		t.Error("a metric with no band of its own must keep the flat threshold")
	}

	// The band is a floor, never a ceiling.
	pol.Bands = map[string]float64{"lifecycle.delete_fork_branch": 5}
	if _, _, err := liveArmGate(cur, base, nil, nil, pol, "0.260907.0"); err == nil {
		t.Error("a band below the flat threshold must not tighten the gate")
	}
}

func TestLiveArmGateQuorum(t *testing.T) {
	pol := liveArmPolicy{
		Thr: float64Thresholds{DropPct: 20, RisePct: 35}, UseHistoryBand: true,
		GrossFactor: 2, MinViolations: 2,
	}
	base := []core.Datapoint{msDP("a", 1000, 0), msDP("b", 1000, 0), msDP("c", 1000, 0)}

	// One moderate outlier out of three is the shape noise takes here.
	one := []core.Datapoint{msDP("a", 1600, 0), msDP("b", 1000, 0), msDP("c", 1000, 0)}
	table, _, err := liveArmGate(one, base, nil, nil, pol, "0.1.2")
	if err != nil {
		t.Errorf("a lone moderate violation must not fail: %v", err)
	}
	if !strings.Contains(table, "quorum") {
		t.Errorf("a violation that missed quorum must still be reported:\n%s", table)
	}

	// Two agreeing is not noise.
	two := []core.Datapoint{msDP("a", 1600, 0), msDP("b", 1600, 0), msDP("c", 1000, 0)}
	if _, _, err := liveArmGate(two, base, nil, nil, pol, "0.1.2"); err == nil {
		t.Error("two datapoints past their allowance must fail")
	}

	// 3x the allowance is past arguing about, quorum or not.
	gross := []core.Datapoint{msDP("a", 2200, 0), msDP("b", 1000, 0), msDP("c", 1000, 0)}
	err = func() error { _, _, e := liveArmGate(gross, base, nil, nil, pol, "0.1.2"); return e }()
	if err == nil {
		t.Error("one datapoint past GrossFactor x allowance must fail alone")
	}
}

func TestLiveArmPolicyAllowance(t *testing.T) {
	pol := liveArmPolicy{Thr: float64Thresholds{DropPct: 20, RisePct: 35}, UseHistoryBand: true,
		Bands: map[string]float64{"lat": 90, "tps": 8}}
	if got := pol.allowanceFor(core.Datapoint{Name: "lat", Direction: core.LowerIsBetter}); got != 90 {
		t.Errorf("lat allowance = %v, want the 90%% band", got)
	}
	if got := pol.allowanceFor(core.Datapoint{Name: "tps", Direction: core.HigherIsBetter}); got != 20 {
		t.Errorf("tps allowance = %v, want the 20%% flat threshold (band is lower)", got)
	}
	pol.UseHistoryBand = false
	if got := pol.allowanceFor(core.Datapoint{Name: "lat", Direction: core.LowerIsBetter}); got != 35 {
		t.Errorf("with bands off, lat allowance = %v, want 35", got)
	}
}

// Replays run fbf0de9f, the 09-08 nightly, from its own live-baseline tables and
// the bands its perf-portal-gate.json carried.
//
// lifecycle's single violation drops below quorum and passes. microbench's two
// still fail, and that is the policy working as specified rather than a verdict
// on the build: run f0fecd4d reran the same two images and every one of those
// datapoints came back the other way (insert_after_truncate +63.6% then -34.0%).
// Correlated datapoints moving together is whole-arm drift, and a quorum counts
// them as independent corroboration. Only persistence across runs separates the
// two.
func TestLiveArmGateOnTheNightlyThatMotivatedThePolicy(t *testing.T) {
	pol := liveArmPolicy{
		Thr: float64Thresholds{DropPct: 20, RisePct: 35}, UseHistoryBand: true,
		GrossFactor: DefaultLiveArmGrossFactor, MinViolations: DefaultLiveArmMinViolations,
	}
	type row struct {
		name                    string
		cur, base, spread, band float64
	}
	run := func(rows []row) (string, []string, error) {
		var cur, base []core.Datapoint
		bands := map[string]float64{}
		for _, r := range rows {
			cur = append(cur, msDP(r.name, r.cur, r.spread))
			base = append(base, msDP(r.name, r.base, 0))
			bands[r.name] = r.band
		}
		pol.Bands = bands
		return liveArmGate(cur, base, nil, nil, pol, "0.260907.0-b2183539a")
	}

	lifecycle := []row{
		{"fork_small", 2410.219, 2683.598, 178.4, 27.3},
		{"fork_loaded", 1833.919, 2838.790, 120.2, 55.1},
		{"load", 35904.908, 36484.257, 17.1, 83.4},
		{"fork_count_cold", 15795.464, 17685.974, 51.0, 57.5},
		{"fork_count_warm", 181.917, 172.636, 2.8, 24.5},
		{"fork_insert", 8221.449, 7645.552, 19.1, 33.8},
		{"delete_half", 2693.220, 2565.582, 44.0, 31.5},
		{"count_after_delete", 152.660, 167.205, 58.6, 113.5},
		{"drop_table", 48.156, 61.952, 28.8, 34.4},
		{"delete_fork_branch", 2968.746, 1383.212, 27.3, 82.4},
	}
	table, suspects, err := run(lifecycle)
	if err != nil {
		t.Errorf("lifecycle: one datapoint 114.6%% past a metric whose own band is 82.4%% is not a release verdict: %v", err)
	}
	if !strings.Contains(table, "quorum") {
		t.Errorf("lifecycle: delete_fork_branch must still be reported:\n%s", table)
	}
	// Nothing else asserts while the arm is up, so a below-quorum violation has
	// to reach the caller or it leaves no trace outside the attached table.
	if len(suspects) != 1 || !strings.Contains(suspects[0], "delete_fork_branch") {
		t.Errorf("lifecycle: suspects = %v, want delete_fork_branch", suspects)
	}

	microbench := []row{
		{"create", 79.975, 44.912, 0, 52.3},
		{"insert_cold", 2332.729, 1756.144, 0, 44.8},
		{"truncate", 43.082, 39.009, 0, 90.9},
		{"insert_after_truncate", 2666.092, 1700.425, 0, 51.7},
		{"count", 40.435, 39.977, 0, 62.0},
		{"pk_lookup", 28.633, 27.865, 0, 9.3},
		{"update", 3418.020, 2249.569, 0, 27.0},
		{"delete", 298.603, 591.311, 0, 36.0},
		{"drop", 72.191, 35.289, 0, 21.2},
	}
	_, _, err = run(microbench)
	if err == nil {
		t.Error("microbench: two datapoints past their allowance must meet quorum")
	}
	for _, want := range []string{"insert_after_truncate", "update"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("microbench verdict must name %s: %v", want, err)
		}
	}
	// create +78% and drop +105% are 35ms swings on sub-100ms operations; the
	// absolute floor is what keeps them out of the quorum.
	for _, never := range []string{"create", "drop"} {
		if strings.Contains(err.Error(), never+" ") {
			t.Errorf("microbench: %s is under the ms floor and must not count: %v", never, err)
		}
	}
}
