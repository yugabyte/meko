package perf

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec"
)

// The benchmark HARNESS: it turns a single-shot workload run into a trustworthy
// multi-iteration benchmark. A Workload is prepared ONCE (schema + initial
// load), then driven for a discarded warmup and N measured iterations; the
// harness aggregates the kept iterations with robust statistics (median as the
// headline, plus mean/stddev/CV/min/max) and hands back the per-iteration
// samples with the aggregate. The caller emits datapoints from the MEDIAN and
// gates the baseline on it, and emits each metric's coefficient of variation
// (.cv) as an informational signal so a noisy run is visible in the report.

// Bench defaults (perf.bench.*). Kept modest so a benchmark stays cheap while
// still producing a stable multi-iteration signal.
const (
	DefaultBenchIterations          = 3
	DefaultBenchWarmup              = 30 * time.Second
	DefaultBenchMeasurementDuration = 60 * time.Second
	DefaultBenchCooldown            = 0 * time.Second
	DefaultBenchDiscardFirst        = true
	DefaultBenchSampleInterval      = 10 * time.Second
	DefaultBenchMinIterations       = 1
)

// RunParams parameterizes one measured (or warmup) RunOnce: how long to drive
// the workload, and how often to request a periodic progress sample (for the
// per-run time series — a workload whose tool cannot report progress leaves
// Sample.Series empty).
type RunParams struct {
	Duration       time.Duration
	SampleInterval time.Duration
}

// SamplePoint is one periodic progress reading within a single run.
type SamplePoint struct {
	ElapsedSec float64
	TPS        float64
	LatencyMS  float64
}

// Sample is the outcome of one workload run: the headline throughput/latency,
// an optional per-run time series, and the raw captured output for triage.
type Sample struct {
	TPS          float64
	LatencyAvgMS float64
	LatencyP95MS float64
	LatencyP99MS float64
	Series       []SamplePoint
	Raw          string
}

// Workload is a benchmark the harness drives: prepared once, then run for one
// measured interval at a time. Implementations adapt a concrete driver
// (benchbase, pgbench) — Prepare is their one-time init/load, RunOnce is a
// single timed run parsed into a Sample.
type Workload interface {
	Name() string
	// Prepare does the one-time setup (schema + initial load) ONCE before any
	// iteration. It must be safe to run a measured RunOnce afterwards with no
	// further loading.
	Prepare(ctx context.Context, ex exec.RemoteExecutor) error
	// RunOnce drives one measured run for p.Duration and parses it into a Sample.
	// It requests periodic progress at p.SampleInterval where the tool supports
	// it; Series may be empty otherwise.
	RunOnce(ctx context.Context, ex exec.RemoteExecutor, p RunParams) (Sample, error)
}

// BenchConfig is the resolved perf.bench.* harness configuration.
type BenchConfig struct {
	Iterations          int
	Warmup              time.Duration
	MeasurementDuration time.Duration
	Cooldown            time.Duration
	DiscardFirst        bool
	SampleInterval      time.Duration
	MinIterations       int
	// SkipLoad runs only the measurement phase against an already-loaded cluster
	// (perf.bench.skip_load) — pair with --env-mode attach to execute on a
	// standing, pre-loaded environment. LoadOnly runs only the load phase and
	// stops (perf.bench.load_only). They are mutually exclusive.
	SkipLoad bool
	LoadOnly bool
}

// benchConfigFromConfig reads perf.bench.* into a BenchConfig.
func benchConfigFromConfig(c *core.Config) BenchConfig {
	return BenchConfig{
		Iterations:          c.GetInt("perf.bench.iterations", DefaultBenchIterations),
		Warmup:              c.GetDuration("perf.bench.warmup", DefaultBenchWarmup),
		MeasurementDuration: c.GetDuration("perf.bench.measurement_duration", DefaultBenchMeasurementDuration),
		Cooldown:            c.GetDuration("perf.bench.cooldown", DefaultBenchCooldown),
		DiscardFirst:        c.GetBool("perf.bench.discard_first", DefaultBenchDiscardFirst),
		SampleInterval:      c.GetDuration("perf.bench.sample_interval", DefaultBenchSampleInterval),
		MinIterations:       c.GetInt("perf.bench.min_iterations", DefaultBenchMinIterations),
		SkipLoad:            c.GetBool("perf.bench.skip_load", false),
		LoadOnly:            c.GetBool("perf.bench.load_only", false),
	}
}

// keptIterations is the number of measured iterations that will be aggregated
// (iterations, minus the discarded first when DiscardFirst is set).
func (bc BenchConfig) keptIterations() int {
	n := bc.Iterations
	if bc.DiscardFirst {
		n--
	}
	return n
}

// minIterations is the effective aggregation floor (never below 1 — an
// aggregate over zero samples is meaningless).
func (bc BenchConfig) minIterations() int {
	if bc.MinIterations < 1 {
		return 1
	}
	return bc.MinIterations
}

// BenchStats is the robust aggregate of one metric across the kept iterations.
// Median is the headline (never the mean — run-to-run variance skews the mean);
// CV (stddev/mean) is the run's noise/trust signal.
type BenchStats struct {
	Median float64
	Mean   float64
	Stddev float64
	CV     float64
	Min    float64
	Max    float64
	Values []float64
}

// BenchResult is the harness outcome: every run's Sample (the discarded warmup,
// every measured iteration in order) plus the aggregate of the kept iterations.
type BenchResult struct {
	Warmup     *Sample  // the discarded warmup run, recorded; nil when Warmup <= 0
	All        []Sample // every measured iteration in order (incl. a discarded first)
	Kept       []Sample // the iterations actually aggregated
	TPS        BenchStats
	LatencyAvg BenchStats
	LatencyP95 BenchStats
	LatencyP99 BenchStats
}

// RunBenchmark prepares w once, runs a discarded warmup (when configured) and
// bc.Iterations measured runs (with an optional cooldown between them), then
// aggregates the kept iterations into a BenchResult. The first measured
// iteration is dropped from aggregation when bc.DiscardFirst — but still
// recorded in All. It returns the partial BenchResult alongside any error so a
// caller can attach whatever output was captured for triage.
func RunBenchmark(ctx context.Context, ex exec.RemoteExecutor, w Workload, bc BenchConfig) (BenchResult, error) {
	var res BenchResult
	if bc.SkipLoad && bc.LoadOnly {
		return res, fmt.Errorf("benchmark %s: skip_load and load_only are mutually exclusive", w.Name())
	}
	if !bc.LoadOnly {
		if kept, min := bc.keptIterations(), bc.minIterations(); kept < min {
			return res, fmt.Errorf("benchmark %s: %d iterations (discard_first=%v) leaves %d for aggregation, below min_iterations %d",
				w.Name(), bc.Iterations, bc.DiscardFirst, kept, min)
		}
	}

	// Load phase (schema + data). Skipped when measuring against a cluster that
	// was already loaded by an earlier run (perf.bench.skip_load).
	if !bc.SkipLoad {
		if err := w.Prepare(ctx, ex); err != nil {
			return res, fmt.Errorf("prepare %s: %w", w.Name(), err)
		}
	}
	// Load-only: stop after loading, no measurement (perf.bench.load_only) — load
	// a (kept) environment once, then re-run execute-only against it later.
	if bc.LoadOnly {
		return res, nil
	}

	if bc.Warmup > 0 {
		ws, err := w.RunOnce(ctx, ex, RunParams{Duration: bc.Warmup, SampleInterval: bc.SampleInterval})
		res.Warmup = &ws
		if err != nil {
			return res, fmt.Errorf("warmup run: %w", err)
		}
	}

	for i := 0; i < bc.Iterations; i++ {
		if i > 0 {
			if err := sleepCtx(ctx, bc.Cooldown); err != nil {
				return res, fmt.Errorf("cooldown before iteration %d: %w", i, err)
			}
		}
		s, err := w.RunOnce(ctx, ex, RunParams{Duration: bc.MeasurementDuration, SampleInterval: bc.SampleInterval})
		res.All = append(res.All, s)
		if err != nil {
			return res, fmt.Errorf("iteration %d: %w", i, err)
		}
	}

	res.Kept = res.All
	if bc.DiscardFirst && len(res.Kept) > 0 {
		res.Kept = res.Kept[1:]
	}
	res.TPS = aggregate(field(res.Kept, func(s Sample) float64 { return s.TPS }))
	res.LatencyAvg = aggregate(field(res.Kept, func(s Sample) float64 { return s.LatencyAvgMS }))
	res.LatencyP95 = aggregate(field(res.Kept, func(s Sample) float64 { return s.LatencyP95MS }))
	res.LatencyP99 = aggregate(field(res.Kept, func(s Sample) float64 { return s.LatencyP99MS }))
	return res, nil
}

// field projects one metric out of each sample into a fresh slice.
func field(samples []Sample, pick func(Sample) float64) []float64 {
	out := make([]float64, len(samples))
	for i, s := range samples {
		out[i] = pick(s)
	}
	return out
}

// --- robust statistics (implemented here, no deps) ---

// aggregate fills a BenchStats from vals. An empty slice yields the zero value
// (all statistics 0) rather than panicking.
func aggregate(vals []float64) BenchStats {
	s := BenchStats{Values: vals}
	if len(vals) == 0 {
		return s
	}
	s.Median = median(vals)
	s.Mean = mean(vals)
	s.Stddev = stddev(vals)
	s.CV = cv(vals)
	s.Min, s.Max = minMax(vals)
	return s
}

// median is the middle value (average of the two middle values for an even
// count). Empty → 0.
func median(vals []float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// mean is the arithmetic average. Empty → 0.
func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// stddev is the SAMPLE standard deviation (Bessel's n-1 correction — the
// unbiased estimator of run-to-run variance). Fewer than two values → 0.
func stddev(vals []float64) float64 {
	n := len(vals)
	if n < 2 {
		return 0
	}
	m := mean(vals)
	var ss float64
	for _, v := range vals {
		d := v - m
		ss += d * d
	}
	return math.Sqrt(ss / float64(n-1))
}

// cv is the coefficient of variation (stddev/mean) — a scale-free noise signal.
// A zero (or empty) mean yields 0.
func cv(vals []float64) float64 {
	m := mean(vals)
	if m == 0 {
		return 0
	}
	return stddev(vals) / m
}

// minMax returns the smallest and largest values. Empty → 0, 0.
func minMax(vals []float64) (float64, float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	mn, mx := vals[0], vals[0]
	for _, v := range vals[1:] {
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}
	return mn, mx
}

// --- datapoint building (shared by the workloads) ---

// metricSpec ties one aggregated metric to the datapoint it emits: the gated
// headline (the median) and the informational .cv companion.
type metricSpec struct {
	Name  string
	Unit  string
	Dir   core.Direction
	Stats BenchStats
}

// splitDatapoints turns the metric specs into two lists: the GATED headline
// datapoints (each carrying the median) and the informational .cv datapoints
// (the run's noise signal, emitted ungated so a noisy run is visible without
// gating on the volatile CV itself).
func splitDatapoints(specs []metricSpec) (gated, info []core.Datapoint) {
	for _, m := range specs {
		gated = append(gated, core.Datapoint{Name: m.Name, Value: m.Stats.Median, Unit: m.Unit, Direction: m.Dir})
		info = append(info, core.Datapoint{Name: m.Name + ".cv", Value: m.Stats.CV, Unit: "cv", Direction: core.LowerIsBetter})
	}
	return gated, info
}
