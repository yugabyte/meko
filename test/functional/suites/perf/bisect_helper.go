package perf

import (
	"fmt"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// Perf BISECT is the N-way variant comparison turned into a SEARCH: it reuses
// the SAME ordered perf.compare.variants list (oldest→newest, good→bad — each a
// {label, overrides} differing only in image/version) and the SAME per-variant
// benchmark at the SAME fixed load, but instead of benchmarking every variant it
// binary-searches — like `git bisect`, for a performance metric — for the FIRST
// variant whose metric regressed beyond a threshold vs the assumed-good
// reference (index 0). It probes only ~log2(N) variants, NOT all N. A single
// perf.compare.mode knob picks the strategy: `all` (today's compare — benchmark
// every variant) or `bisect` (this). This file is the PURE core: the mode
// constants, the regression predicate, and the search itself. The orchestration
// (provision→benchmark→teardown each PROBED variant, then emit the report) lives
// in bisect_run.go.

const (
	// CompareModeAll benchmarks every variant and reports the N-way comparison
	// (the default — today's behaviour).
	CompareModeAll = "all"
	// CompareModeBisect binary-searches the ordered variants for the first
	// regressed one, probing only ~log2(N).
	CompareModeBisect = "bisect"

	// DefaultBisectMetric is the metric whose regression a bisect localizes.
	DefaultBisectMetric = "tps"
	// DefaultBisectThresholdPct is how far the metric must regress (in its worse
	// direction) vs the reference for a variant to count as BAD.
	DefaultBisectThresholdPct = 10.0
)

// bisectProbe is one variant the search evaluated: its index in the ordered
// list, its measured metric value, and whether it was judged BAD (regressed
// beyond the threshold vs the reference).
type bisectProbe struct {
	Index  int
	Metric float64
	Bad    bool
}

// bisectResult is the pure search outcome: the FIRST-bad index (the localized
// transition — the boundary hi), the last-known-good index (the boundary lo),
// and every probe in evaluation order (the reference at index 0 first, then each
// midpoint the search visited).
type bisectResult struct {
	FirstBad  int
	GoodIndex int
	Probes    []bisectProbe
}

// regressedBeyond reports whether a variant's metric value regressed more than
// thresholdPct vs the good reference, in the metric's WORSE direction: a
// higher-is-better metric (tps) is bad when it DROPS more than the threshold; a
// lower-is-better metric (latency) is bad when it RISES more than the threshold.
// A zero reference is never regressed (nothing to compare against).
func regressedBeyond(dir core.Direction, good, val, thresholdPct float64) bool {
	if good == 0 {
		return false
	}
	deltaPct := (val - good) / good * 100
	if dir == core.LowerIsBetter {
		return deltaPct > thresholdPct
	}
	return deltaPct < -thresholdPct
}

// bisectFirstBad binary-searches n ordered variants for the FIRST bad one.
// Index 0 is the assumed-GOOD reference whose metric is `good` (already measured
// by the caller — the +1 benchmark), index n-1 is the assumed-BAD end. It holds
// lo=0 (good) and hi=n-1 (bad) and, while hi-lo>1, benchmarks the midpoint and
// moves the boundary it belongs to (bad → hi=mid, good → lo=mid), so it probes
// only ~log2(n) interior variants — NOT all n. The result is hi (the first bad).
// It assumes monotonicity (good then bad, like git bisect); a non-monotone
// metric series is a caller precondition, not something a bisection can detect.
// Pure and unit-tested with a fake benchmark. n must be >= 2 (guaranteed by the
// variant resolver, which errors on fewer before any provisioning).
func bisectFirstBad(n int, good, thresholdPct float64, dir core.Direction, benchmark func(i int) (float64, error)) (bisectResult, error) {
	res := bisectResult{Probes: []bisectProbe{{Index: 0, Metric: good, Bad: false}}}
	lo, hi := 0, n-1
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		val, err := benchmark(mid)
		if err != nil {
			return res, fmt.Errorf("benchmark candidate %d: %w", mid, err)
		}
		bad := regressedBeyond(dir, good, val, thresholdPct)
		res.Probes = append(res.Probes, bisectProbe{Index: mid, Metric: val, Bad: bad})
		if bad {
			hi = mid
		} else {
			lo = mid
		}
	}
	res.FirstBad = hi
	res.GoodIndex = lo
	return res, nil
}
