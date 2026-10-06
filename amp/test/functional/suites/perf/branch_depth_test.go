package perf

import (
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Both gates compare |v - baseline| with the floor, so the floor must flag at
// branchDepthColdFloorPct over the baseline, not over v.
func TestColdFloorFlagsAtThePercentOverBaseline(t *testing.T) {
	const base = 50.0
	at := base * (1 + branchDepthColdFloorPct/100)
	if d := at - base; d < coldFloorMS(at)-1e-9 {
		t.Errorf("v=%.2f is %.0f%% over %.0f and must clear its floor: delta %.3f < floor %.3f", at, branchDepthColdFloorPct, base, d, coldFloorMS(at))
	}
	below := at - 0.5
	if d := below - base; d >= coldFloorMS(below) {
		t.Errorf("v=%.2f is under %.0f%% over %.0f and must not clear its floor: delta %.3f >= floor %.3f", below, branchDepthColdFloorPct, base, d, coldFloorMS(below))
	}
}

func TestScanColdGatesThePassMedianWithTheSliceSpread(t *testing.T) {
	slices := []float64{100, 110, 120, 105, 115}
	levels := []*branchLevel{
		{depth: 0, scanMS: 110, scanRuns: slices},
		{depth: 1, scanMS: 720, scanRuns: slices, scanPasses: []float64{1256, 717, 720}},
	}
	gated, _, err := branchDepthDatapoints(levels, 0, branchDepthParams{GateDepths: []int{0, 1}})
	if err != nil {
		t.Fatal(err)
	}
	want := spreadPct(aggregate(slices))
	for _, tc := range []struct {
		name    string
		value   float64
		samples int
	}{{"branch.scan_cold_ms@depth0", 110, 5}, {"branch.scan_cold_ms@depth1", 720, 3}} {
		var dp *core.Datapoint
		for i := range gated {
			if gated[i].Name == tc.name {
				dp = &gated[i]
			}
		}
		if dp == nil {
			t.Fatalf("%s not gated", tc.name)
		}
		if dp.Value != tc.value || dp.N != tc.samples || len(dp.Samples) != tc.samples {
			t.Errorf("%s: value %.0f N %d samples %d, want %.0f, %d, %d", tc.name, dp.Value, dp.N, len(dp.Samples), tc.value, tc.samples, tc.samples)
		}
		if dp.SpreadPct != want {
			t.Errorf("%s: SpreadPct %.1f, want the slice spread %.1f", tc.name, dp.SpreadPct, want)
		}
	}
}
