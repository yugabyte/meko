package perf

import (
	"fmt"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// SHAPE gates: assertions about the shape of a benchmark run, computed from what
// the harness already collected — no extra load, no extra runtime.
//
// The baseline gate compares an absolute median against a stored reference, so
// in a fresh workspace (no baseline file) a perf run asserts NOTHING. These
// gates instead threshold RATIOS computed inside one run, so they fire on the
// very first run:
//
//   - iteration drift: the first measured iteration vs the last. State that
//     accumulates across iterations (the shape where a statement gets
//     permanently slower after the first pass) is invisible to a median.
//   - intra-run decay: the median of the first third of one run's progress
//     samples vs the last third. Throughput that degrades while the run proceeds
//     is averaged away by a whole-run median.
//
// Both are expressed as a REGRESSION PERCENT in the metric's own worse direction
// (a drop for throughput, a rise for latency): positive = worse, negative =
// improving, so one threshold per gate covers both directions. A limit <= 0
// turns that gate off; the datapoint is still emitted.
//
// The discarded warmup is recorded too (`<metric>_warmup` plus the delta to the
// measured median), ungated: a warmup that is FASTER than steady state is the
// signal the harness currently throws away, but a cold warmup being slower is
// normal, so it is reported rather than asserted.

// Shape-gate defaults (perf.bench.*). Sized to sit ABOVE the run-to-run spread
// a small co-located load already shows (see perf.warehouses in
// configs/default.yaml: ~14% throughput and up to ~48% tail-latency spread at a
// 2-warehouse TPC-C) — these gates exist to catch MULTIPLES, and a limit inside
// the noise band would fire on healthy runs.
const (
	// DefaultMaxIterDriftPct compares two SINGLE iterations (no median
	// smoothing), the noisiest of the two shapes, so it sits above the observed
	// tail-latency spread.
	DefaultMaxIterDriftPct = 50.0
	// DefaultMaxDecayPct compares medians of ~a third of a run's progress
	// samples each (~20 readings at the 1s default), materially steadier than a
	// single-iteration comparison, so it can be tighter.
	DefaultMaxDecayPct = 35.0
)

// benchGates is the resolved perf.bench.* shape-gate configuration.
type benchGates struct {
	MaxIterDriftPct float64
	MaxDecayPct     float64
}

// benchGatesFromConfig reads the shape-gate limits from perf.bench.*.
func benchGatesFromConfig(c *core.Config) benchGates {
	return benchGates{
		MaxIterDriftPct: c.GetFloat("perf.bench.max_iter_drift_pct", DefaultMaxIterDriftPct),
		MaxDecayPct:     c.GetFloat("perf.bench.max_decay_pct", DefaultMaxDecayPct),
	}
}

// benchShapeGate turns a finished BenchResult into the shape datapoints and
// asserts the gated ones against g. The datapoints are returned for the caller
// to emit alongside the other informational ones; the error names every
// violation. Pure — unit-tested without a live run.
func benchShapeGate(res BenchResult, specs []metricSpec, g benchGates) ([]core.Datapoint, error) {
	var dps []core.Datapoint
	var violations []string

	for _, m := range specs {
		if m.FromSample != nil && res.Warmup != nil {
			if w := m.FromSample(*res.Warmup); w > 0 {
				// The cold number itself, kept: discarding it is what hides a
				// "fast first pass, slow forever after" shape.
				dps = append(dps, core.Datapoint{Name: m.Name + "_warmup", Value: w, Unit: m.Unit, Direction: m.Dir})
				if d, ok := regressionPct(w, m.Stats.Median, m.Dir); ok {
					dps = append(dps, pctDatapoint(m.Name+"_warmup_delta_pct", d))
				}
			}
		}
		if d, ok := iterDriftPct(m.Stats, m.Dir); ok {
			dps = append(dps, pctDatapoint(m.Name+"_iter_delta_pct", d))
			if g.MaxIterDriftPct > 0 && d > g.MaxIterDriftPct {
				violations = append(violations, fmt.Sprintf("%s drifted %.1f%% across iterations (limit %.1f%%: first %.4g → last %.4g %s)",
					m.Name, d, g.MaxIterDriftPct, first(m.Stats.Values), last(m.Stats.Values), m.Unit))
			}
		}
		if m.FromPoint != nil {
			if d, ok := runDecayPct(res.Kept, m.FromPoint, m.Dir); ok {
				dps = append(dps, pctDatapoint(m.Name+"_decay_pct", d))
				if g.MaxDecayPct > 0 && d > g.MaxDecayPct {
					violations = append(violations, fmt.Sprintf("%s decayed %.1f%% within the run (limit %.1f%%: first third vs last third of the progress samples)",
						m.Name, d, g.MaxDecayPct))
				}
			}
		}
	}

	if len(violations) > 0 {
		return dps, fmt.Errorf("perf shape regression: %s", strings.Join(violations, "; "))
	}
	return dps, nil
}

// pctDatapoint is one shape datapoint: a regression percent in the metric's
// worse direction, so lower is always better and 0 is flat.
func pctDatapoint(name string, pct float64) core.Datapoint {
	return core.Datapoint{Name: name, Value: pct, Unit: "%", Direction: core.LowerIsBetter}
}

// regressionPct is the percent change from → to expressed in dir's WORSE
// direction: positive when the metric got worse, negative when it improved. It
// reports ok=false for a non-positive `from` (a zero or missing reference makes
// the ratio meaningless — never a division by zero).
func regressionPct(from, to float64, dir core.Direction) (float64, bool) {
	if from <= 0 {
		return 0, false
	}
	delta := (to - from) / from * 100
	if dir == core.HigherIsBetter {
		return -delta, true
	}
	return delta, true
}

// iterDriftPct is the regression from the FIRST kept iteration to the LAST. A
// single kept iteration has no drift to measure (ok=false).
func iterDriftPct(st BenchStats, dir core.Direction) (float64, bool) {
	if len(st.Values) < 2 {
		return 0, false
	}
	return regressionPct(first(st.Values), last(st.Values), dir)
}

// runDecayPct is the MEDIAN across the kept iterations of each iteration's
// intra-run decay — median, not mean, for the same reason the headline is a
// median. Iterations whose progress series is too short to split are skipped;
// no iteration yields a decay at all reports ok=false.
func runDecayPct(kept []Sample, pick func(SamplePoint) float64, dir core.Direction) (float64, bool) {
	var per []float64
	for _, s := range kept {
		if d, ok := seriesDecayPct(s.Series, pick, dir); ok {
			per = append(per, d)
		}
	}
	if len(per) == 0 {
		return 0, false
	}
	return median(per), true
}

// seriesDecayPct is the regression from the median of the first third of one
// run's progress readings to the median of the last third. Fewer than 3 samples
// cannot be split into thirds (ok=false).
func seriesDecayPct(series []SamplePoint, pick func(SamplePoint) float64, dir core.Direction) (float64, bool) {
	n := len(series)
	if n < 3 || pick == nil {
		return 0, false
	}
	third := n / 3
	return regressionPct(medianOf(series[:third], pick), medianOf(series[n-third:], pick), dir)
}

// medianOf projects pick over the points and takes the median.
func medianOf(points []SamplePoint, pick func(SamplePoint) float64) float64 {
	vals := make([]float64, len(points))
	for i, p := range points {
		vals[i] = pick(p)
	}
	return median(vals)
}

func first(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	return vals[0]
}

func last(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	return vals[len(vals)-1]
}

// Metric accessors the workloads hand to their metric specs: the harness Sample
// and progress-reading fields each metric lives in.
func sampleTPS(s Sample) float64         { return s.TPS }
func sampleLatencyAvg(s Sample) float64  { return s.LatencyAvgMS }
func sampleLatencyP95(s Sample) float64  { return s.LatencyP95MS }
func sampleLatencyP99(s Sample) float64  { return s.LatencyP99MS }
func pointTPS(p SamplePoint) float64     { return p.TPS }
func pointLatency(p SamplePoint) float64 { return p.LatencyMS }
