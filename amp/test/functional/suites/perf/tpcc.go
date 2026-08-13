package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/benchbase"
	"github.com/yugabyte/amp/test/functional/framework/apps/metrics"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/artifact"
	"github.com/yugabyte/amp/test/functional/framework/baseline"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
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
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe},
		Tags:        []string{"perf", "nightly"},
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

	r := &perfRun{
		t:       t,
		cfg:     cfg,
		bc:      bc,
		gates:   benchGatesFromConfig(t.Options),
		hcfg:    historyConfigFromConfig(t.Options),
		version: chartVersion(t.Options),
		id:      identityFromConfig(t.Options, "", commit, load),
		td:      &teardownStack{},
		logf:    t.Masker().Logf(log.Printf),
	}
	r.sink = attachSink{t: t, logf: r.logf}
	t.SetPerfIdentity(r.id.core())

	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
		defer cancel()
		if tdErr := r.td.run(tctx, r.logf); tdErr != nil && err == nil {
			err = fmt.Errorf("teardown: %w", tdErr)
		}
	}()

	step := func(name string, fn func(context.Context) error) error {
		return t.Step(name, func(*core.StepHandle) error { return fn(t.Context) })
	}
	phases := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"provision_backing", r.provisionBacking},
		{"wait_backing_ready", r.waitReady},
		{"provision_amp_endpoint", r.provisionEndpoint},
		{"deploy_client_pod", r.deployClientPod},
		{"run_tpcc", r.runBenchmark},
		{"gate_baseline", r.gateBaseline},
	}
	for _, p := range phases {
		if err := step(p.name, p.fn); err != nil {
			return err
		}
	}
	return nil
}

// perfRun carries the shared state the phases read/write.
type perfRun struct {
	t       *core.T
	cfg     perfConfig
	bc      BenchConfig
	gates   benchGates
	hcfg    historyConfig
	version string
	id      Identity
	td      *teardownStack
	logf    func(string, ...any)
	sink    apps.ArtifactSink

	info         ybk8s.Info
	kube         *k8s.Kubectl
	endpoint     ampEndpoint
	benchbaseDir string
	podExec      exec.RemoteExecutor
	sampler      *metrics.Sampler
	epSampler    *metrics.Sampler

	benchResult BenchResult
}

func (r *perfRun) suffix() string { return r.t.ResourceSuffix() }

// provisionBacking deploys a co-located YugabyteDB cluster in k8s (RF forced up
// to cfg.RF), registers its teardown, and resolves the connection path plus a
// namespace-scoped kubectl.
func (r *perfRun) provisionBacking(ctx context.Context) error {
	info, kube, err := deployYBBacking(ctx, r.t, r.cfg.RF, r.td, r.logf)
	if err != nil {
		return err
	}
	r.info = info
	r.kube = kube
	return nil
}

// provisionEndpoint stands the amp control plane + one compute endpoint on the
// running backing and starts the endpoint-pod metrics sampler. The workload
// connects here, so the amp compute + pageserver are in every measured request.
func (r *perfRun) provisionEndpoint(ctx context.Context) error {
	ep, err := provisionAmpEndpoint(ctx, r.t, ampstack.FromConfig(r.t.Options), ybk8s.FromConfig(r.t.Options), r.info, r.suffix(), r.sink, r.td, r.logf)
	if err != nil {
		return err
	}
	r.endpoint = ep
	r.epSampler = startEndpointSampler(ctx, r.t, ep, r.td, r.logf)
	return nil
}

// deployClientPod launches the long-lived in-cluster client pod (sleep-forever,
// carrying Java) and delivers the PUBLIC benchbase artifact into it over its
// pod-bound executor. No private sample-app jar is needed for the perf tier.
func (r *perfRun) deployClientPod(ctx context.Context) error {
	podExec, err := startPerfClientPod(ctx, r.kube, r.sink, r.td, "ampqa-perf-"+r.suffix(), r.cfg.ClientImage, r.cfg.ClientPullSecret)
	if err != nil {
		return err
	}
	r.podExec = podExec

	if err := deliverBenchbase(ctx, r.t, r.cfg, r.podExec); err != nil {
		return err
	}
	r.benchbaseDir = path.Join(r.cfg.RemoteDir, benchbase.DefaultSubdir)
	return nil
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

// waitReady blocks until the backing actually serves YSQL. Pod-Ready is
// necessary but NOT sufficient: the tserver readiness probe gates the tserver
// RPC port, while YSQL (postgres on :5433) comes up slightly later — and the
// control plane deployed next creates its yb_storage database over YSQL. Once
// it is serving, the backing's pods are known and the metrics sampler starts.
func (r *perfRun) waitReady(ctx context.Context) error {
	if err := waitBackingYSQL(ctx, r.kube, r.info, r.cfg.RF, r.cfg.ReadyTimeout, r.sink, r.logf); err != nil {
		return err
	}
	r.sampler = startMetricsSampler(ctx, r.t, r.kube, r.td, r.logf)
	return nil
}

// runBenchmark drives BenchBase TPC-C through the harness: load ONCE, then a
// discarded warmup + N measured execute iterations, aggregated with robust
// statistics. Every run's captured output is attached for triage regardless of
// parse/gate outcome.
func (r *perfRun) runBenchmark(ctx context.Context) error {
	wl := &tpccWorkload{
		runDir:     r.benchbaseDir,
		conn:       r.endpoint.Conn,
		warehouses: r.cfg.Warehouses,
		terminals:  r.cfg.Terminals,
		maxHeap:    r.cfg.MaxHeap,
	}
	res, err := RunBenchmark(ctx, r.podExec, wl, r.bc)
	attachBenchSamples(r.sink, res, "benchbase-tpcc")
	if err != nil {
		return err
	}
	if len(res.Kept) == 0 || res.TPS.Median == 0 {
		return fmt.Errorf("benchbase tpcc produced no throughput — parse of results/summary.json failed (see attached benchbase-tpcc-iter-*.out)")
	}
	r.benchResult = res
	emitBenchCharts(r.t, res)
	emitMetricSeries(r.t, r.sampler)
	emitMetricSeries(r.t, r.epSampler)
	return nil
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

// tpccDatapoints turns the aggregate into the tier's datapoints: the GATED
// medians (throughput higher-is-better; p95/p99/median latency lower-is-better)
// and the informational .cv noise signal per metric.
func tpccDatapoints(res BenchResult) (gated, info []core.Datapoint) {
	return splitDatapoints(tpccMetricSpecs(res))
}

// tpccMetricSpecs is the metric set perf/tpcc reports, with the Sample accessors
// the shape gates need. BenchBase reports no periodic progress, so no FromPoint
// is set and the intra-run decay shape is skipped for TPC-C.
func tpccMetricSpecs(res BenchResult) []metricSpec {
	return []metricSpec{
		{Name: "tpcc.throughput", Unit: "req/s", Dir: core.HigherIsBetter, Stats: res.TPS, FromSample: sampleTPS},
		{Name: "tpcc.latency_p95", Unit: "ms", Dir: core.LowerIsBetter, Stats: res.LatencyP95, FromSample: sampleLatencyP95},
		{Name: "tpcc.latency_p99", Unit: "ms", Dir: core.LowerIsBetter, Stats: res.LatencyP99, FromSample: sampleLatencyP99},
		{Name: "tpcc.latency_median", Unit: "ms", Dir: core.LowerIsBetter, Stats: res.LatencyAvg, FromSample: sampleLatencyAvg},
	}
}

// gateBaseline emits the aggregate's datapoints — the medians gated against the
// baseline (a regression fails the test; a first run or --update-baseline
// establishes/refreshes it and WARNs), and the .cv datapoints informationally.
// It then records this run in the history store and emits the cross-run trend
// chart(s) — including a regressed run, which is still a point on the trend —
// before propagating any gate failure.
func (r *perfRun) gateBaseline(_ context.Context) error {
	gated, info := tpccDatapoints(r.benchResult)
	shape, shapeErr := benchShapeGate(r.benchResult, tpccMetricSpecs(r.benchResult), r.gates)
	gateErr := emitAndGate(r.t, r.cfg, r.version, testNameTPCC, gated, append(info, shape...), r.id, r.logf)
	emitHistoryTrend(r.t, r.hcfg, testNameTPCC, r.version, r.benchResult, r.id, r.logf)
	return errors.Join(gateErr, shapeErr)
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
