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
	table, err := liveArmGate(cur, base, thr, "0.1.2-abc")
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
	if _, err := liveArmGate(cur, base, thr, "0.1.2-abc"); err != nil {
		t.Fatalf("an improvement on both axes must pass: %v", err)
	}
}

func TestLiveArmDatapointsSuffixed(t *testing.T) {
	out := liveArmDatapoints([]core.Datapoint{{Name: "x.tps", Value: 100}})
	if len(out) != 1 || out[0].Name != "x.tps.baseline" {
		t.Fatalf("baseline datapoints = %+v, want x.tps.baseline", out)
	}
}
