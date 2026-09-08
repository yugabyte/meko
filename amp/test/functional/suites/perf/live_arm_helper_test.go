package perf

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

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
	table, err := liveArmGate(cur, base, nil, nil, thr, "0.1.2-abc")
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
	if _, err := liveArmGate(cur, base, nil, nil, thr, "0.1.2-abc"); err != nil {
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

	if _, err := liveArmGate(cur(0), base, nil, nil, thr, "0.1.2-abc"); err == nil {
		t.Fatal("a +133% delta must fail when the arm reports no spread")
	}
	table, err := liveArmGate(cur(200), base, nil, nil, thr, "0.1.2-abc")
	if err != nil {
		t.Errorf("a delta inside the arm's own 200%% spread is a draw, not a regression: %v", err)
	}
	if !strings.Contains(table, "spread%") {
		t.Errorf("the table must carry the spread column so the skip is visible:\n%s", table)
	}
	if _, err := liveArmGate(cur(10), base, nil, nil, thr, "0.1.2-abc"); err == nil {
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

	if _, err := liveArmGate(cur, base, curBest, baseBest, thr, "0.1.2-abc"); err == nil {
		t.Error("the best draw regressed +133% too, so the spread veto must not suppress it")
	}
	// The best draw did not regress: no corroboration, and the veto still applies.
	quietBest := map[string]float64{"x.load": 46000}
	if _, err := liveArmGate(cur, base, quietBest, baseBest, thr, "0.1.2-abc"); err != nil {
		t.Errorf("an uncorroborated median delta must not fail: %v", err)
	}
}
