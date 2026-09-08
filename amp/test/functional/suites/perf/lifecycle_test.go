package perf

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func TestLifecycleDatapointsGateForkRatio(t *testing.T) {
	results := []phaseResult{
		{Name: "fork_small", Group: "fork_small", MS: 1000},
		{Name: "load", Group: "load", MS: 20000},
		{Name: "fork_loaded", Group: "fork_loaded", MS: 7000},
	}
	p := lifecycleParams{Rows: 100000, MaxForkRatio: 5}
	gated, info, err := lifecycleDatapoints(results, nil, p)
	if len(gated) != 3 {
		t.Fatalf("gated = %d, want 3", len(gated))
	}
	if len(info) != 2 || info[0].Name != "lifecycle.load_rows_per_s" || info[0].Value != 5000 {
		t.Fatalf("info = %+v", info)
	}
	if err == nil || !strings.Contains(err.Error(), "7.00x") {
		t.Fatalf("err = %v, want the 7x ratio to fail the 5x limit", err)
	}
	p.MaxForkRatio = 0
	if _, _, err := lifecycleDatapoints(results, nil, p); err != nil {
		t.Fatalf("limit 0 must not assert: %v", err)
	}
	if _, _, err := lifecycleDatapoints(results[:2], nil, p); err != nil {
		t.Fatalf("no loaded fork must not assert: %v", err)
	}
}

// The gated value is the median of the iterations, not the first or the worst.
func TestLifecycleDatapointsAggregateIterations(t *testing.T) {
	results := []phaseResult{
		{Name: "fork_small", Group: "fork_small", MS: 1000},
		{Name: "fork_loaded", Group: "fork_loaded", MS: 1100},
		{Name: "fork_small", Group: "fork_small", MS: 1000},
		{Name: "fork_loaded", Group: "fork_loaded", MS: 900},
		{Name: "fork_small", Group: "fork_small", MS: 1000},
		{Name: "fork_loaded", Group: "fork_loaded", MS: 30000},
	}
	gated, info, err := lifecycleDatapoints(results, []float64{460, 470, 465}, lifecycleParams{Rows: 100000, MaxForkRatio: 5, Iterations: 3})
	if err != nil {
		t.Fatalf("median ratio 1.1x must not fire the 5x gate: %v", err)
	}
	if len(gated) != 2 {
		t.Fatalf("gated = %+v, want one per group", gated)
	}
	byName := map[string]float64{}
	for _, dp := range append(gated, info...) {
		byName[dp.Name] = dp.Value
	}
	if got := byName["lifecycle.fork_loaded"]; got != 1100 {
		t.Errorf("fork_loaded = %v, want the median 1100 (not the 30000 outlier)", got)
	}
	if got := byName["lifecycle.fork_size_ratio"]; got != 1.1 {
		t.Errorf("fork_size_ratio = %v, want median/median = 1.1", got)
	}
	if got := byName["lifecycle.table_mb"]; got != 465 {
		t.Errorf("table_mb = %v, want the median 465", got)
	}
	for name := range byName {
		if strings.HasSuffix(name, ".cv") || strings.HasSuffix(name, ".mad") {
			t.Errorf("derived statistic %s reported; spreads are computed report-side", name)
		}
	}
}

// Provisioning and teardown phases are timed but not tracked: perf/multitenant
// already gates cluster/endpoint create, and the rest is plumbing.
func TestLifecycleDatapointsTracksOnlyTheInterestingPhases(t *testing.T) {
	var results []phaseResult
	for _, g := range []string{"create_cluster", "create_endpoint", "first_sql", "fork_endpoint",
		"fork_pk_lookup", "delete_cluster", "delete_half"} {
		results = append(results, phaseResult{Name: g, Group: g, MS: 100})
	}
	gated, _, err := lifecycleDatapoints(results, nil, lifecycleParams{Rows: 1})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(gated) != 1 || gated[0].Name != "lifecycle.delete_half" {
		t.Fatalf("gated = %+v, want only the tracked delete_half", gated)
	}
}

func TestLifecycleDatapointsSingleIterationHasNoSpread(t *testing.T) {
	results := []phaseResult{{Name: "drop_table", Group: "drop_table", MS: 70}}
	gated, info, err := lifecycleDatapoints(results, nil, lifecycleParams{Rows: 1, Iterations: 1})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(gated) != 1 || gated[0].Value != 70 {
		t.Fatalf("gated = %+v", gated)
	}
	for _, dp := range info {
		if strings.HasSuffix(dp.Name, ".cv") || strings.HasSuffix(dp.Name, ".mad") {
			t.Errorf("single iteration emitted %s", dp.Name)
		}
	}
}

func TestLifecycleDatapointsStampSpread(t *testing.T) {
	results := []phaseResult{
		{Name: "load", Group: "load", MS: 1000},
		{Name: "load2", Group: "load", MS: 3000},
		{Name: "delete_half", Group: "delete_half", MS: 500},
	}
	gated, _, _ := lifecycleDatapoints(results, nil, lifecycleParams{})
	byName := map[string]core.Datapoint{}
	for _, dp := range gated {
		byName[dp.Name] = dp
	}
	// (3000-1000)/1000*100.
	if got := byName["lifecycle.load"].SpreadPct; got != 200 {
		t.Errorf("lifecycle.load SpreadPct = %v, want 200", got)
	}
	// One sample cannot disagree with itself, so it carries no spread and the
	// gates fall back to their thresholds.
	if got := byName["lifecycle.delete_half"].SpreadPct; got != 0 {
		t.Errorf("a single iteration must carry no spread, got %v", got)
	}
}

func TestSpreadPctGuards(t *testing.T) {
	if got := spreadPct(aggregate([]float64{5})); got != 0 {
		t.Errorf("one value = %v, want 0", got)
	}
	if got := spreadPct(aggregate(nil)); got != 0 {
		t.Errorf("no values = %v, want 0", got)
	}
	if got := spreadPct(aggregate([]float64{0, 4})); got != 0 {
		t.Errorf("a zero minimum has no defined ratio, got %v", got)
	}
	if got := spreadPct(aggregate([]float64{2, 4, 3})); got != 100 {
		t.Errorf("(4-2)/2*100 = %v, want 100", got)
	}
}
