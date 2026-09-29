package perf

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// TestMicrobenchPhases: the phase list is create → cold insert → N (truncate,
// re-insert) cycles → count → pk lookup → update → delete → drop, with the
// repeated phases numbered and the SAME insert statement re-used across cycles.
func TestMicrobenchPhases(t *testing.T) {
	phases := microbenchPhases("t1", microbenchParams{Rows: 1000, TruncateCycles: 3})

	var names []string
	for _, p := range phases {
		names = append(names, p.Name)
	}
	want := []string{
		"create", "insert_cold",
		"truncate_1", "insert_after_truncate_1",
		"truncate_2", "insert_after_truncate_2",
		"truncate_3", "insert_after_truncate_3",
		"count", "pk_lookup", "update", "delete", "drop",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("phases = %v, want %v", names, want)
	}

	byName := map[string]phase{}
	for _, p := range phases {
		byName[p.Name] = p
	}
	if cold, warm := byName["insert_cold"].SQL, byName["insert_after_truncate_2"].SQL; cold != warm {
		t.Errorf("the re-insert is not the same statement as the cold insert:\n%s\n%s", cold, warm)
	}
	if !strings.Contains(byName["insert_cold"].SQL, "generate_series(1, 1000)") {
		t.Errorf("insert does not carry the row count: %s", byName["insert_cold"].SQL)
	}
	if byName["insert_after_truncate_1"].Group != "insert_after_truncate" {
		t.Errorf("numbered phase lost its group: %+v", byName["insert_after_truncate_1"])
	}

	// Degenerate shapes must still produce a valid statement list.
	zero := microbenchPhases("t1", microbenchParams{})
	if len(zero) != 7 {
		t.Errorf("zero cycles produced %d phases, want the 7 non-cycle ones", len(zero))
	}
	if !strings.Contains(zero[1].SQL, "generate_series(1, 1)") {
		t.Errorf("a zero row count must clamp to 1 row, got %s", zero[1].SQL)
	}
}

// TestPhaseRatio: the ratio is median(numerator) / median(denominator), and any
// missing or non-positive denominator leaves it unreported.
func TestPhaseRatio(t *testing.T) {
	full := []phaseResult{
		{Name: "insert_cold", Group: phaseInsertCold, MS: 3704},
		{Name: "insert_after_truncate_1", Group: phaseInsertAfterTruncate, MS: 18499},
		{Name: "insert_after_truncate_2", Group: phaseInsertAfterTruncate, MS: 17419},
		{Name: "insert_after_truncate_3", Group: phaseInsertAfterTruncate, MS: 21267},
	}
	cases := []struct {
		name    string
		results []phaseResult
		want    float64
		wantOK  bool
	}{
		{"reported timings", full, 4.994, true},
		{"single cycle", full[:2], 4.994, true},
		{"no cold phase", full[1:], 0, false},
		{"no warm phase", full[:1], 0, false},
		{"zero cold phase", []phaseResult{
			{Group: phaseInsertCold, MS: 0},
			{Group: phaseInsertAfterTruncate, MS: 100},
		}, 0, false},
		{"no results at all", nil, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := phaseRatio(c.results, phaseInsertAfterTruncate, phaseInsertCold)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && math.Abs(got-c.want) > 0.01 {
				t.Errorf("ratio = %v, want ~%v", got, c.want)
			}
		})
	}
}

// TestMicrobenchDatapoints: every phase group is one gated ms datapoint, the
// median of its timings carrying the samples, the ratio becomes its own
// datapoint, and it fails the run when it crosses the limit — with no baseline
// anywhere in the picture.
func TestMicrobenchDatapoints(t *testing.T) {
	results := []phaseResult{
		{Name: "create", Group: phaseCreate, MS: 12},
		{Name: "insert_cold", Group: phaseInsertCold, MS: 3704},
		{Name: "truncate_1", Group: phaseTruncate, MS: 31},
		{Name: "insert_after_truncate_1", Group: phaseInsertAfterTruncate, MS: 18499},
		{Name: "insert_after_truncate_2", Group: phaseInsertAfterTruncate, MS: 17419},
		{Name: "insert_after_truncate_3", Group: phaseInsertAfterTruncate, MS: 21267},
	}

	gated, info, err := microbenchDatapoints(results, microbenchParams{MaxPhaseRatio: 2})
	if err == nil {
		t.Fatal("a ~5x insert-after-truncate ratio passed a 2.0x limit")
	}
	if !strings.Contains(err.Error(), ratioDatapoint) {
		t.Errorf("error = %q, want it to name the ratio datapoint", err)
	}
	wantGated := map[string]float64{
		"microbench.create": 12, "microbench.insert_cold": 3704, "microbench.truncate": 31,
		"microbench.insert_after_truncate": 18499,
	}
	if len(gated) != len(wantGated) {
		t.Errorf("gated %d datapoints, want one per group (%d): %+v", len(gated), len(wantGated), gated)
	}
	for _, dp := range gated {
		if want, ok := wantGated[dp.Name]; !ok || dp.Value != want {
			t.Errorf("gated %s = %v, want %v (median for a repeated group)", dp.Name, dp.Value, want)
		}
		if dp.Unit != "ms" || dp.Direction != core.LowerIsBetter {
			t.Errorf("phase datapoint %+v, want ms / lower-is-better", dp)
		}
		if !strings.HasPrefix(dp.Name, microbenchDatapointPrefix) {
			t.Errorf("phase datapoint %q is not namespaced", dp.Name)
		}
		if dp.Name == "microbench.insert_after_truncate" && (dp.N != 3 || len(dp.Samples) != 3) {
			t.Errorf("the repeated group must carry its samples: %+v", dp)
		}
	}
	if len(info) != 1 {
		t.Fatalf("info datapoints = %+v, want only the ratio", info)
	}
	ratio := info[0]
	if ratio.Name != ratioDatapoint || ratio.Unit != "x" {
		t.Fatalf("last info datapoint = %+v, want the ratio in x", ratio)
	}
	if math.Abs(ratio.Value-4.994) > 0.01 {
		t.Errorf("ratio datapoint = %v, want ~4.99", ratio.Value)
	}
	if best := microbenchBestValues(results); len(best) != 1 || best["microbench.insert_after_truncate"] != 17419 {
		t.Errorf("best values = %v, want only the repeated group's fastest cycle", best)
	}

	// Under the limit, and with the gate disabled, the datapoints still flow.
	if _, _, err := microbenchDatapoints(results, microbenchParams{MaxPhaseRatio: 10}); err != nil {
		t.Errorf("a 5x ratio failed a 10x limit: %v", err)
	}
	_, info, err = microbenchDatapoints(results, microbenchParams{})
	if err != nil {
		t.Errorf("max_phase_ratio 0 must disable the assertion, got %v", err)
	}
	if len(info) != 1 {
		t.Errorf("max_phase_ratio 0 dropped the ratio datapoint: %+v", info)
	}

	// No phases at all: nothing to emit, nothing to assert, no panic.
	if gated, info, err := microbenchDatapoints(nil, microbenchParams{MaxPhaseRatio: 2}); err != nil || len(gated) != 0 || len(info) != 0 {
		t.Errorf("empty results produced %d/%d datapoints, err %v", len(gated), len(info), err)
	}
	// A cold phase that measured 0 ms cannot be a denominator.
	if _, info, err := microbenchDatapoints([]phaseResult{
		{Name: "insert_cold", Group: phaseInsertCold, MS: 0},
		{Name: "insert_after_truncate_1", Group: phaseInsertAfterTruncate, MS: 500},
	}, microbenchParams{MaxPhaseRatio: 2}); err != nil || len(info) != 0 {
		t.Errorf("zero-ms cold phase produced info %+v, err %v", info, err)
	}
}

// TestMicrobenchLiveArmGateCycles: the 0.260905.0 run. Cycle 3 alone was +35.9%
// against the baseline's fastest draw while the group medians sat 2% apart, so
// the median-plus-best verdict passes; a uniform slowdown of every cycle fails.
func TestMicrobenchLiveArmGateCycles(t *testing.T) {
	cycles := func(a, b, c float64) []phaseResult {
		return []phaseResult{
			{Name: "insert_cold", Group: phaseInsertCold, MS: 1896},
			{Name: "insert_after_truncate_1", Group: phaseInsertAfterTruncate, MS: a},
			{Name: "insert_after_truncate_2", Group: phaseInsertAfterTruncate, MS: b},
			{Name: "insert_after_truncate_3", Group: phaseInsertAfterTruncate, MS: c},
		}
	}
	thr := float64Thresholds{DropPct: 20, RisePct: 35}
	base := cycles(1689, 1781, 1265)
	baseGated, _, _ := microbenchDatapoints(base, microbenchParams{})

	cur := cycles(1722, 1446, 1719)
	gated, _, _ := microbenchDatapoints(cur, microbenchParams{})
	if _, _, err := liveArmGate(gated, baseGated, microbenchBestValues(cur), microbenchBestValues(base), strictPolicy(thr), "0.260904.1"); err != nil {
		t.Errorf("one slow cycle against the baseline's fastest failed the gate: %v", err)
	}

	slow := cycles(2500, 2600, 2400)
	gated, _, _ = microbenchDatapoints(slow, microbenchParams{})
	if _, _, err := liveArmGate(gated, baseGated, microbenchBestValues(slow), microbenchBestValues(base), strictPolicy(thr), "0.260904.1"); err == nil {
		t.Error("every cycle ~45% slower on median and best passed the gate")
	}
}

// fakeExecer fails on the statement whose text contains failOn.
type fakeExecer struct {
	ran    []string
	failOn string
}

func (f *fakeExecer) Exec(_ context.Context, sql string, _ ...any) (int64, error) {
	f.ran = append(f.ran, sql)
	if f.failOn != "" && strings.Contains(sql, f.failOn) {
		return 0, fmt.Errorf("boom")
	}
	return 0, nil
}

// TestRunPhases: every phase runs in order and is timed; a failing phase names
// itself and the phases already completed are still returned as evidence.
func TestRunPhases(t *testing.T) {
	phases := microbenchPhases("t1", microbenchParams{Rows: 10, TruncateCycles: 1})

	ex := &fakeExecer{}
	results, err := runPhases(context.Background(), ex, phases, nil, nil)
	if err != nil {
		t.Fatalf("runPhases: %v", err)
	}
	if len(results) != len(phases) || len(ex.ran) != len(phases) {
		t.Fatalf("ran %d phases / recorded %d, want %d", len(ex.ran), len(results), len(phases))
	}
	for _, r := range results {
		if r.MS < 0 {
			t.Errorf("phase %s recorded a negative duration %v", r.Name, r.MS)
		}
	}

	ex = &fakeExecer{failOn: "TRUNCATE"}
	results, err = runPhases(context.Background(), ex, phases, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "phase truncate_1") {
		t.Fatalf("err = %v, want it to name the failing phase", err)
	}
	if len(results) != 2 {
		t.Errorf("kept %d results, want the 2 phases that completed", len(results))
	}
}

// TestRunPhasesSteps: with a stepper, every phase is wrapped in a step of its own
// named after the phase and carrying its group and timing; a failing phase's step
// is the last one opened and the error still propagates.
func TestRunPhasesSteps(t *testing.T) {
	phases := microbenchPhases("t1", microbenchParams{Rows: 10, TruncateCycles: 1})

	type opened struct {
		name  string
		props map[string]any
	}
	var steps []opened
	stepper := func(name string, fn func(set func(string, any)) error) error {
		s := opened{name: name, props: map[string]any{}}
		err := fn(func(k string, v any) { s.props[k] = v })
		steps = append(steps, s)
		return err
	}

	if _, err := runPhases(context.Background(), &fakeExecer{}, phases, nil, stepper); err != nil {
		t.Fatalf("runPhases: %v", err)
	}
	if len(steps) != len(phases) {
		t.Fatalf("opened %d steps, want one per phase (%d)", len(steps), len(phases))
	}
	for i, ph := range phases {
		if steps[i].name != ph.Name {
			t.Errorf("step %d named %q, want the phase label %q", i, steps[i].name, ph.Name)
		}
		if steps[i].props["group"] != ph.Group {
			t.Errorf("step %s group = %v, want %q", ph.Name, steps[i].props["group"], ph.Group)
		}
		if _, ok := steps[i].props["ms"]; !ok {
			t.Errorf("step %s recorded no ms property", ph.Name)
		}
	}

	steps = nil
	if _, err := runPhases(context.Background(), &fakeExecer{failOn: "TRUNCATE"}, phases, nil, stepper); err == nil {
		t.Fatal("failing phase returned no error")
	}
	if n := len(steps); n != 3 || steps[n-1].name != "truncate_1" {
		t.Errorf("steps = %+v, want the failing phase to be the last one opened", steps)
	}
}

// TestFormatPhasesAndChart: the attached table lists every phase plus the ratio
// line, and the chart carries one named bar per phase.
func TestFormatPhasesAndChart(t *testing.T) {
	results := []phaseResult{
		{Name: "insert_cold", Group: phaseInsertCold, MS: 3704},
		{Name: "insert_after_truncate_1", Group: phaseInsertAfterTruncate, MS: 18499},
	}
	out := formatPhases(results)
	for _, want := range []string{"insert_cold", "3704.0", "insert_after_truncate_1", "= 4.99x"} {
		if !strings.Contains(out, want) {
			t.Errorf("phase table missing %q:\n%s", want, out)
		}
	}
	if got := formatPhases(nil); !strings.Contains(got, "phase") {
		t.Errorf("empty phase table = %q", got)
	}

	c := phaseChart(results)
	if c.Kind != core.ChartBar || len(c.Series) != 2 || c.Series[1].Name != "insert_after_truncate_1" {
		t.Errorf("chart = %+v, want a named bar per phase", c)
	}
}

// TestLoadFieldsMicrobenchSignature: the row count and cycle count are part of
// the load signature (two different loads are not comparable), and the new
// fields leave the other workloads' signatures untouched.
func TestLoadFieldsMicrobenchSignature(t *testing.T) {
	a := LoadSignature(loadFieldsMicrobench(microbenchParams{Rows: 200000, TruncateCycles: 3}))
	b := LoadSignature(loadFieldsMicrobench(microbenchParams{Rows: 1000000, TruncateCycles: 3}))
	c := LoadSignature(loadFieldsMicrobench(microbenchParams{Rows: 200000, TruncateCycles: 5}))
	if a == b || a == c || b == c {
		t.Errorf("different microbench loads hashed the same: %s %s %s", a, b, c)
	}
	if a != LoadSignature(loadFieldsMicrobench(microbenchParams{Rows: 200000, TruncateCycles: 3})) {
		t.Error("the same microbench load hashed differently")
	}
}
