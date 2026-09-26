package perf

import (
	"context"
	"fmt"
	"log"
	"math"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/benchbase"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/artifact"
	"github.com/yugabyte/amp/test/functional/framework/baseline"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// testNameTPCC is the registered name and the baseline key for the TPC-C test.
const testNameTPCC = "perf/tpcc"

// tserverSelector is the chart-fixed label on the YB tserver pods (the YSQL
// endpoints the readiness wait probes over the tailnet).
const tserverSelector = "app=yb-tserver"

// ysqlSettle is a short pause after YSQL first accepts a connection, giving the
// tserver Service's endpoints time to stabilize before the in-pod benchmark
// connects through the Service (rather than the pod IP the wait probed).
const ysqlSettle = 10 * time.Second

// perfTeardownBudget bounds the whole LIFO teardown stack, independent of the
// test body deadline (AGENTS.md §5 — teardown runs on its own budget).
const perfTeardownBudget = 20 * time.Minute

func init() {
	core.Register(core.Test{
		Name:        testNameTPCC,
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe, ClusterSlots: benchClusterSlots},
		Tags:        []string{"perf"},
		OwningTeam:  core.TeamCompute,
		Description: "Run BenchBase TPC-C at a fixed scale through an amp compute endpoint backed by a co-located RF3 YB-in-k8s cluster, and gate tps + latency against a stored baseline.",
		Params:      []string{"perf", "apps.ybk8s", "env.ampstack"},
		Run:         runTPCC,
	})
}

// runTPCC is the live body: gate → provision the co-located YB backing → stand
// the amp control plane + compute endpoint on it → deploy the client pod +
// benchbase → run TPC-C through the endpoint → parse tps/latency → gate against
// the baseline → guaranteed teardown.
func runTPCC(t *core.T) (err error) {
	cfg := perfFromConfig(t.Options)
	if reason, ok := perfGate(t.Options, cfg); !ok {
		t.Skip(reason)
		return nil
	}
	if reason, ok := gateNoDoubleProvision(t.Env); !ok {
		t.Skip(reason)
		return nil
	}
	if reason, ok := ampEndpointGate(t.Options); !ok {
		t.Skip(reason)
		return nil
	}

	bc := benchConfigFromConfig(t.Options)
	load := loadFieldsTPCC(bc, cfg)
	commit := frameworkCommit()

	// Comparison mode: run the SAME TPC-C at the SAME load across labelled
	// variants (differing only in the overrides), instead of gating one run.
	// perf.compare.mode picks the strategy over that ordered variant list: "all"
	// benchmarks every variant (the N-way comparison), "bisect" binary-searches
	// them for the first regressed one.
	cmp, err := compareConfigFromConfig(t.Options)
	if err != nil {
		return err
	}
	if len(cmp.Variants) > 0 {
		logf := t.Masker().Logf(log.Printf)
		sink := attachSink{t: t, logf: logf}
		deliver := func(ctx context.Context, podExec exec.RemoteExecutor) error {
			return deliverBenchbase(ctx, t, cfg, podExec)
		}
		mk := func(conn pg.ConnInfo) Workload { return tpccWorkloadFor(cfg, conn) }
		if cmp.Mode == CompareModeBisect {
			return runBisectSuite(t, bisectSpec{
				perf: cfg, cmp: cmp, bc: bc, load: load, commit: commit,
				podPrefix: "ampqa-perf", deliver: deliver, mkWorkload: mk, sink: sink, logf: logf,
			})
		}
		return runComparisonSuite(t, comparisonSpec{
			perf: cfg, cmp: cmp, bc: bc, load: load, commit: commit,
			podPrefix: "ampqa-perf", deliver: deliver, mkWorkload: mk, sink: sink, logf: logf,
		})
	}

	return runBenchProvisioned(t, benchWorkloadSpec{
		testName:     testNameTPCC,
		runStep:      "run_tpcc",
		podPrefix:    "ampqa-perf",
		attachPrefix: "benchbase-tpcc",
		noTPSMsg:     "benchbase tpcc produced no throughput — parse of results/summary.json failed (see attached benchbase-tpcc-iter-*.out)",
		deliver:      deliverBenchbase,
		mkWorkload:   func(conn pg.ConnInfo) Workload { return tpccWorkloadFor(cfg, conn) },
		specs:        tpccMetricSpecs,
		datapoints:   tpccDatapoints,
	}, cfg, bc, identityFromConfig(t.Options, "", commit, load))
}

// deliverBenchbase resolves the public benchbase artifact and delivers it into
// the client pod over podExec. Shared by the single-run path and each
// comparison variant.
func deliverBenchbase(ctx context.Context, t *core.T, cfg perfConfig, podExec exec.RemoteExecutor) error {
	specs, err := artifact.Resolve(t.Options)
	if err != nil {
		return fmt.Errorf("resolve artifacts: %w", err)
	}
	bb, ok := specs["benchbase"]
	if !ok {
		return fmt.Errorf("artifacts.benchbase is not declared")
	}
	if err := (&artifact.Deployer{}).Deploy(ctx, []exec.RemoteExecutor{podExec}, bb, artifact.DeployOptions{RemoteDir: cfg.RemoteDir}); err != nil {
		return fmt.Errorf("deploy benchbase into client pod: %w", err)
	}
	return nil
}

// tpccWorkloadFor builds the TPC-C workload bound to conn at the shared load
// (warehouses/terminals/heap). Shared by the single-run path and each variant.
func tpccWorkloadFor(cfg perfConfig, conn pg.ConnInfo) Workload {
	return &tpccWorkload{
		runDir:     path.Join(cfg.RemoteDir, benchbase.DefaultSubdir),
		conn:       conn,
		benchmark:  benchbase.Benchmark(cfg.Benchmark),
		warehouses: cfg.Warehouses,
		terminals:  cfg.Terminals,
		maxHeap:    cfg.MaxHeap,
	}
}

// tpccWorkload adapts the benchbase TPC-C driver to the harness Workload: the
// benchmark's create+load is the one-time Prepare, each timed execute is a
// RunOnce. It drives ONE host — the amp compute endpoint's Service DNS — so the
// JDBC URL takes the single-host path and the smart driver's cluster-aware
// balancing stays OFF (LoadBalance=false): the endpoint is the only server the
// client may talk to, and the topology it would discover behind amp is not its
// to spread over.
type tpccWorkload struct {
	runDir     string
	conn       pg.ConnInfo
	benchmark  benchbase.Benchmark
	warehouses int
	terminals  int
	maxHeap    string
}

func (w *tpccWorkload) Name() string { return testNameTPCC }

// baseSpec is the RunSpec shared by Prepare and RunOnce (phases set per call).
func (w *tpccWorkload) baseSpec() benchbase.RunSpec {
	return benchbase.RunSpec{
		RunDir:      w.runDir,
		Benchmark:   w.benchmark,
		Conn:        w.conn,
		Warehouses:  w.warehouses,
		Terminals:   w.terminals,
		MaxHeap:     w.maxHeap,
		LoadBalance: benchbase.Ptr(false),
	}
}

// Prepare creates the schema and loads the TPC-C dataset once (no execute).
func (w *tpccWorkload) Prepare(ctx context.Context, ex exec.RemoteExecutor) error {
	spec := w.baseSpec()
	spec.Create = benchbase.Ptr(true)
	spec.Load = benchbase.Ptr(true)
	spec.Execute = benchbase.Ptr(false)
	res, err := benchbase.Run(ctx, ex, spec)
	if err != nil {
		return fmt.Errorf("benchbase tpcc create+load: %w (output: %s)", err, tail(res.Raw, 400))
	}
	return nil
}

// RunOnce runs one timed execute against the already-loaded data (BenchBase
// does not report periodic progress, so Sample.Series stays empty).
func (w *tpccWorkload) RunOnce(ctx context.Context, ex exec.RemoteExecutor, p RunParams) (Sample, error) {
	spec := w.baseSpec()
	spec.Create = benchbase.Ptr(false)
	spec.Load = benchbase.Ptr(false)
	spec.Execute = benchbase.Ptr(true)
	spec.Duration = p.Duration
	if p.Rate > 0 {
		spec.Rate = strconv.Itoa(max(int(math.Round(p.Rate)), 1))
	}
	res, err := benchbase.Run(ctx, ex, spec)
	s := benchbaseSample(res)
	if err != nil {
		return s, fmt.Errorf("benchbase tpcc execute: %w (output: %s)", err, tail(res.Raw, 400))
	}
	return s, nil
}

// benchbaseSample maps a parsed BenchBase result into a harness Sample. TPC-C's
// authoritative latency figure is its median, carried in the Sample's primary
// latency slot; latencies convert µs → ms.
func benchbaseSample(res benchbase.Result) Sample {
	usToMs := func(us float64) float64 { return us / 1000 }
	return Sample{
		TPS:          res.ThroughputReqPerSec,
		LatencyAvgMS: usToMs(res.MedianLatencyUS),
		LatencyP95MS: usToMs(res.P95LatencyUS),
		LatencyP99MS: usToMs(res.P99LatencyUS),
		Raw:          res.Raw,
	}
}

// tpccDatapoints turns the aggregate into the tier's datapoints: saturated
// throughput and the fixed-rate latencies gated, saturated latencies reported.
func tpccDatapoints(res BenchResult) (gated, info []core.Datapoint) {
	return splitDatapoints(tpccMetricSpecs(res))
}

// tpccMetricSpecs is the metric set perf/tpcc reports, with the Sample accessors
// the shape gates need. BenchBase reports no periodic progress, so no FromPoint
// is set and the intra-run decay shape is skipped for TPC-C.
func tpccMetricSpecs(res BenchResult) []metricSpec {
	return append([]metricSpec{
		{Name: "tpcc.throughput", Unit: "req/s", Dir: core.HigherIsBetter, Stats: res.TPS, FromSample: sampleTPS},
		{Name: "tpcc.latency_p95", Unit: "ms", Dir: core.LowerIsBetter, Stats: res.LatencyP95, FromSample: sampleLatencyP95, Info: true, NoDrift: true},
		{Name: "tpcc.latency_p99", Unit: "ms", Dir: core.LowerIsBetter, Stats: res.LatencyP99, FromSample: sampleLatencyP99, Info: true, NoDrift: true},
		{Name: "tpcc.latency_median", Unit: "ms", Dir: core.LowerIsBetter, Stats: res.LatencyAvg, FromSample: sampleLatencyAvg, Info: true},
	}, fixedRateSpecs("tpcc", "latency_median", res)...)
}

// gateOutcome is the pure result of the baseline gate: the datapoints (with
// comparisons filled), which baseline the verdict came from, and which branch
// fired. It mutates the passed baseline file in place when establishing/updating
// (the caller saves it when Wrote).
type gateOutcome struct {
	Datapoints  []core.Datapoint
	Selection   baseline.Selection // which stored baseline was gated against
	Gated       bool               // a comparison actually happened
	Established bool               // this line had no baseline → wrote one, no gate
	Updated     bool               // --update-baseline → refreshed, no gate
	Ungated     bool               // a baseline exists but none is load-comparable
	Regressed   bool               // gated and a datapoint regressed
	Wrote       bool               // the store was modified and must be saved
}

// gatePerf is the pure gate: resolve which stored baseline this run may be
// judged against, compare against exactly that one, and decide whether to also
// start this version's own line. Pure and unit-tested with an in-memory baseline
// file.
//
// A run whose own version has no baseline gates against the nearest preceding
// version instead of self-establishing, which is what makes a regression landing
// AT a version bump visible; it still writes its own line so the next run on
// this version gates against itself.
func gatePerf(f *baseline.File, version, test string, dps []core.Datapoint, cfg perfConfig, want baseline.Meta) gateOutcome {
	now := time.Now()
	if cfg.UpdateBaseline {
		f.Update(version, test, dps, now)
		return gateOutcome{Datapoints: dps, Updated: true, Wrote: true}
	}

	_, hasOwnLine := f.StoredMeta(version, test)
	sel, ok := baseline.Resolve(f, version, test, want)
	if !ok {
		if hasOwnLine {
			// This line has a baseline, at a load nothing here can be compared
			// against. Gating on it would be dishonest; overwriting it would lose it.
			return gateOutcome{Datapoints: dps, Ungated: true}
		}
		f.Update(version, test, dps, now)
		return gateOutcome{Datapoints: dps, Established: true, Wrote: true}
	}

	rep := baseline.Compare(f, sel.Version, test, dps, cfg.thresholds())
	out := gateOutcome{Datapoints: rep.Datapoints, Selection: sel, Gated: rep.HadBaseline}
	switch {
	case rep.HadBaseline && rep.Regressed:
		out.Regressed = true
	case !rep.HadBaseline && !hasOwnLine:
		out.Established = true
	case !rep.HadBaseline:
		out.Ungated = true
	}
	if !hasOwnLine {
		f.Update(version, test, dps, now)
		out.Wrote = true
	}
	return out
}

// regressionSummary lists the regressed datapoints for the failure message.
func regressionSummary(dps []core.Datapoint) string {
	var parts []string
	for _, dp := range dps {
		b := dp.Baseline
		if b == nil || !b.Regressed {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %g%s vs baseline %g (%+.1f%%, tol %.1f%%)",
			dp.Name, dp.Value, unitSuffix(dp.Unit), b.Value, b.DeltaPct, b.Threshold))
	}
	return strings.Join(parts, "; ")
}

func unitSuffix(unit string) string {
	if unit == "" {
		return ""
	}
	return " " + unit
}

// tail returns the last n bytes of s (whitespace-trimmed), for embedding a
// captured benchmark output snippet in an error without flooding it.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
