package perf

import (
	"strings"
	"testing"
)

func TestLatencyTopologyTableShowsRatios(t *testing.T) {
	results := map[string]BenchResult{
		latencyBaselineName: {TPS: BenchStats{Median: 200}, LatencyAvg: BenchStats{Median: 40}, LatencyP95: BenchStats{Median: 60}},
		"multi_region":      {TPS: BenchStats{Median: 50}, LatencyAvg: BenchStats{Median: 160}, LatencyP95: BenchStats{Median: 300}},
	}
	out := latencyTopologyTable([]string{latencyBaselineName, "multi_region"}, results)
	if !strings.Contains(out, "0.250") || !strings.Contains(out, "1.000") {
		t.Errorf("table lacks the ratios:\n%s", out)
	}
}

func TestLatencyTopologySpecsDropP95ForPgbench(t *testing.T) {
	if n := len(latencyTopologySpecs("x", BenchResult{}, true)); n != 2 {
		t.Errorf("pgbench specs = %d, want 2", n)
	}
	if n := len(latencyTopologySpecs("x", BenchResult{}, false)); n != 3 {
		t.Errorf("sysbench specs = %d, want 3", n)
	}
}
