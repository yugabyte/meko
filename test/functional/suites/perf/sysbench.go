// sysbench is the PERFORMANCE tier's sysbench workload. It stands up the same
// co-located YugabyteDB-in-k8s backing + in-cluster client pod as perf/tpcc and
// perf/pgbench, but drives sysbench's bundled oltp_* lua workloads over the
// pgsql driver instead of BenchBase/pgbench.
//
// sysbench is not delivered from an artifact: it is a distro package, installed
// idempotently into the (Debian/Ubuntu-family) client pod via apt at deploy
// (apps/sysbench.EnsureInstalled). The Debian/Ubuntu build ships the pgsql
// driver and the oltp_* scripts, so no lua delivery is needed either.
//
// It runs `sysbench <script> prepare` (dataset load) then timed `run`s over the
// pod executor, emits tps + average/p95 latency as gated core.Datapoints, and
// gates them against the stored baseline exactly like perf/tpcc and
// perf/pgbench (reusing gatePerf/baseline). perf.compare.variants turns it into
// the same A/B (N-way) variant comparison / bisect the other perf workloads
// support. Teardown is guaranteed (LIFO stack on its own deadline). Gated
// behind perf.enabled + a k8s cluster + a chart version. The sysbench load
// shape lives under perf.sysbench.* (script/tables/table_size/threads); the
// backing + gate knobs are shared under perf.*. Registered by init.
package perf

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/sysbench"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec/k8s"
)

// testNameSysbench is the registered name and the baseline key for the sysbench test.
const testNameSysbench = "perf/sysbench"

// Cheap defaults for the sysbench load shape (perf.sysbench.*). The backing +
// gate knobs are shared with perf/tpcc and perf/pgbench under perf.*
// (perfConfig).
const (
	DefaultSysbenchScript    = "oltp_read_write"
	DefaultSysbenchTables    = 4
	DefaultSysbenchTableSize = 100000
	DefaultSysbenchThreads   = 8
)

// sysbenchParams is the resolved perf.sysbench.* load shape.
type sysbenchParams struct {
	Script    string
	Tables    int
	TableSize int
	Threads   int
}

// sysbenchParamsFromConfig reads perf.sysbench.* into a sysbenchParams.
func sysbenchParamsFromConfig(c *core.Config) sysbenchParams {
	return sysbenchParams{
		Script:    c.GetString("perf.sysbench.script", DefaultSysbenchScript),
		Tables:    c.GetInt("perf.sysbench.tables", DefaultSysbenchTables),
		TableSize: c.GetInt("perf.sysbench.table_size", DefaultSysbenchTableSize),
		Threads:   c.GetInt("perf.sysbench.threads", DefaultSysbenchThreads),
	}
}

func init() {
	core.Register(core.Test{
		Name:        testNameSysbench,
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe},
		OwningTeam:  core.TeamCompute,
		Description: "Run sysbench (pgsql driver, bundled oltp_* lua workloads) on a co-located YB-in-k8s backing and gate tps + latency against a stored baseline.",
		Params:      []string{"perf", "apps.ybk8s"},
		Run:         runSysbench,
	})
}

// runSysbench is the live body: gate → provision the co-located YB backing →
// deploy the client pod + install sysbench → wait for YSQL → run sysbench
// prepare + timed runs → parse tps/latency → gate against the baseline →
// guaranteed teardown.
func runSysbench(t *core.T) (err error) {
	cfg := perfFromConfig(t.Options)
	if reason, ok := perfGate(t.Options, cfg); !ok {
		t.Skip(reason)
		return nil
	}
	if reason, ok := gateNoDoubleProvision(t.Env); !ok {
		t.Skip(reason)
		return nil
	}

	bc := benchConfigFromConfig(t.Options)
	params := sysbenchParamsFromConfig(t.Options)
	loadSig := LoadSignature(loadFieldsSysbench(bc, cfg, params))
	commit := frameworkCommit()

	// Comparison mode: run the SAME sysbench at the SAME load across labelled
	// variants (differing only in the overrides), instead of gating one run.
	// perf.compare.mode picks the strategy over that ordered variant list: "all"
	// benchmarks every variant (the N-way comparison), "bisect" binary-searches
	// them for the first regressed one.
	if cmp := compareConfigFromConfig(t.Options); len(cmp.Variants) > 0 {
		logf := t.Masker().Logf(log.Printf)
		sink := attachSink{t: t, logf: logf}
		deliver := func(ctx context.Context, podExec exec.RemoteExecutor) error {
			return sysbench.EnsureInstalled(ctx, podExec)
		}
		mk := func(conn pg.ConnInfo) Workload { return sysbenchWorkloadFor(params, conn) }
		if cmp.Mode == CompareModeBisect {
			return runBisectSuite(t, bisectSpec{
				perf: cfg, cmp: cmp, bc: bc, loadSig: loadSig, commit: commit,
				podPrefix: "ampqa-sysbench", deliver: deliver, mkWorkload: mk, sink: sink, logf: logf,
			})
		}
		return runComparisonSuite(t, comparisonSpec{
			perf: cfg, cmp: cmp, bc: bc, loadSig: loadSig, commit: commit,
			podPrefix: "ampqa-sysbench", deliver: deliver, mkWorkload: mk, sink: sink, logf: logf,
		})
	}

	r := &sysbenchRun{
		t:       t,
		cfg:     cfg,
		bc:      bc,
		hcfg:    historyConfigFromConfig(t.Options),
		params:  params,
		version: chartVersion(t.Options),
		id:      identityFromConfig(t.Options, "", commit, loadSig),
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
		{"deploy_client_pod", r.deployClientPod},
		{"wait_ready", r.waitReady},
		{"run_sysbench", r.runBenchmark},
		{"gate_baseline", r.gateBaseline},
	}
	for _, p := range phases {
		if err := step(p.name, p.fn); err != nil {
			return err
		}
	}
	return nil
}

// sysbenchRun carries the shared state the phases read/write.
type sysbenchRun struct {
	t       *core.T
	cfg     perfConfig
	bc      BenchConfig
	hcfg    historyConfig
	params  sysbenchParams
	version string
	id      Identity
	td      *teardownStack
	logf    func(string, ...any)
	sink    apps.ArtifactSink

	info          ybk8s.Info
	kube          *k8s.Kubectl
	inClusterHost string
	inClusterPort int
	podExec       exec.RemoteExecutor

	benchResult BenchResult
}

func (r *sysbenchRun) suffix() string { return r.t.ResourceSuffix() }

// provisionBacking deploys the co-located YB backing and resolves the in-cluster
// connection path plus a namespace-scoped kubectl.
func (r *sysbenchRun) provisionBacking(ctx context.Context) error {
	info, kube, err := deployYBBacking(ctx, r.t, r.cfg.RF, r.td, r.logf)
	if err != nil {
		return err
	}
	r.info = info
	r.kube = kube
	r.inClusterHost, r.inClusterPort = ybk8s.FirstHostPort(info.InClusterHostPorts)
	return nil
}

// deployClientPod launches the client pod and installs sysbench into it —
// before any sysbench prepare/run.
func (r *sysbenchRun) deployClientPod(ctx context.Context) error {
	podExec, err := startPerfClientPod(ctx, r.kube, r.sink, r.td, "ampqa-sysbench-"+r.suffix(), r.cfg.ClientImage, r.cfg.ClientPullSecret)
	if err != nil {
		return err
	}
	r.podExec = podExec
	return sysbench.EnsureInstalled(ctx, r.podExec)
}

// sysbenchWorkloadFor builds the sysbench workload bound to conn at the shared
// load (script/tables/table_size/threads). Shared by the single-run path and
// each comparison variant.
func sysbenchWorkloadFor(params sysbenchParams, conn pg.ConnInfo) Workload {
	return &sysbenchWorkload{conn: conn, params: params}
}

// waitReady blocks until the backing serves YSQL.
func (r *sysbenchRun) waitReady(ctx context.Context) error {
	return waitBackingYSQL(ctx, r.kube, r.info, r.cfg.RF, r.cfg.ReadyTimeout)
}

// conn is the in-cluster YSQL target reached through the tserver Service.
func (r *sysbenchRun) conn() pg.ConnInfo {
	return pg.ConnInfo{Host: r.inClusterHost, Port: r.inClusterPort, User: r.info.User, Database: r.info.User, SSLMode: "disable"}
}

// runBenchmark drives sysbench through the harness: load the dataset ONCE, then
// a discarded warmup + N measured runs, aggregated with robust statistics. Every
// run's captured output is attached for triage regardless of parse/gate outcome.
func (r *sysbenchRun) runBenchmark(ctx context.Context) error {
	wl := sysbenchWorkloadFor(r.params, r.conn())
	res, err := RunBenchmark(ctx, r.podExec, wl, r.bc)
	attachBenchSamples(r.sink, res, "sysbench")
	if err != nil {
		return err
	}
	if len(res.Kept) == 0 || res.TPS.Median == 0 {
		return fmt.Errorf("sysbench produced no tps — parse of sysbench output failed (see attached sysbench-iter-*.out)")
	}
	r.benchResult = res
	emitBenchCharts(r.t, res)
	return nil
}

// gateBaseline emits the aggregate's datapoints: the medians gated against the
// baseline, and the .cv noise signal informationally, exactly like perf/tpcc.
// It then records this run in the history store and emits the cross-run trend
// chart(s), before propagating any gate failure.
func (r *sysbenchRun) gateBaseline(_ context.Context) error {
	gated, info := sysbenchDatapoints(r.benchResult)
	gateErr := emitAndGate(r.t, r.cfg, r.version, testNameSysbench, gated, info, r.id, r.logf)
	emitHistoryTrend(r.t, r.hcfg, testNameSysbench, r.version, r.benchResult, r.id, r.logf)
	return gateErr
}

// sysbenchWorkload adapts the sysbench driver to the harness Workload:
// `sysbench prepare` (dataset load) is the one-time Prepare, each timed
// `sysbench run` is a RunOnce. When a sample interval is requested it passes
// --report-interval so sysbench reports periodic progress, parsed into the
// Sample's time series.
type sysbenchWorkload struct {
	conn   pg.ConnInfo
	params sysbenchParams
}

func (w *sysbenchWorkload) Name() string { return testNameSysbench }

// Prepare loads the sysbench dataset once (sysbench <script> prepare).
func (w *sysbenchWorkload) Prepare(ctx context.Context, ex exec.RemoteExecutor) error {
	opts := sysbench.PrepareOptions{
		Conn:      w.conn,
		Script:    w.params.Script,
		Tables:    w.params.Tables,
		TableSize: w.params.TableSize,
	}
	if err := sysbench.Prepare(ctx, ex, opts); err != nil {
		return fmt.Errorf("sysbench prepare: %w", err)
	}
	return nil
}

// RunOnce drives one timed sysbench run. sysbench reports avg + one percentile
// (95th by default), so the Sample carries tps + avg + p95; --report-interval
// progress feeds Series.
func (w *sysbenchWorkload) RunOnce(ctx context.Context, ex exec.RemoteExecutor, p RunParams) (Sample, error) {
	opts := sysbench.RunOptions{
		Conn:           w.conn,
		Script:         w.params.Script,
		Tables:         w.params.Tables,
		TableSize:      w.params.TableSize,
		Threads:        w.params.Threads,
		Duration:       p.Duration,
		ReportInterval: p.SampleInterval,
	}
	res, err := sysbench.Run(ctx, ex, opts)
	s := Sample{
		TPS:          res.TPS,
		LatencyAvgMS: res.LatencyAvgMs,
		LatencyP95MS: res.LatencyP95Ms,
		Series:       parseSysbenchProgress(res.Raw),
		Raw:          res.Raw,
	}
	if err != nil {
		return s, fmt.Errorf("sysbench run: %w", err)
	}
	return s, nil
}

// sysbenchProgressRe matches sysbench's --report-interval progress lines, e.g.
// "[ 10s ] thds: 8 tps: 411.58 qps: 8237.42 (r/w/o: ...) lat (ms,95%): 44.98 err/s: 0.00".
var sysbenchProgressRe = regexp.MustCompile(`\[\s*([0-9.]+)s\s*\]\s*thds:\s*\d+\s*tps:\s*([0-9.]+).*?lat \(ms,\d+%\):\s*([0-9.]+)`)

// parseSysbenchProgress extracts the periodic progress readings from raw
// sysbench output into a per-run time series (the latency reading is the
// interval's 95th percentile — the only latency sysbench reports per interval).
// No progress lines yields an empty series.
func parseSysbenchProgress(raw string) []SamplePoint {
	var series []SamplePoint
	for _, m := range sysbenchProgressRe.FindAllStringSubmatch(raw, -1) {
		elapsed, _ := strconv.ParseFloat(m[1], 64)
		tps, _ := strconv.ParseFloat(m[2], 64)
		lat, _ := strconv.ParseFloat(m[3], 64)
		series = append(series, SamplePoint{ElapsedSec: elapsed, TPS: tps, LatencyMS: lat})
	}
	return series
}

// sysbenchDatapoints turns the aggregate into the tier's datapoints: the GATED
// medians (tps higher-is-better; latencies lower-is-better) and the
// informational .cv noise signal per metric.
func sysbenchDatapoints(res BenchResult) (gated, info []core.Datapoint) {
	return splitDatapoints([]metricSpec{
		{"sysbench.tps", "tps", core.HigherIsBetter, res.TPS},
		{"sysbench.latency_avg", "ms", core.LowerIsBetter, res.LatencyAvg},
		{"sysbench.latency_p95", "ms", core.LowerIsBetter, res.LatencyP95},
	})
}

// loadFieldsSysbench is the perf/sysbench load: the shared bench cadence plus
// RF and the sysbench script/tables/table_size/threads shape (Scale carries the
// per-table row count; Clients carries the thread count).
func loadFieldsSysbench(bc BenchConfig, cfg perfConfig, params sysbenchParams) LoadFields {
	return LoadFields{
		Iterations:    bc.Iterations,
		WarmupMS:      bc.Warmup.Milliseconds(),
		MeasurementMS: bc.MeasurementDuration.Milliseconds(),
		Scale:         params.TableSize,
		Clients:       params.Threads,
		Tables:        params.Tables,
		Script:        params.Script,
		RF:            cfg.RF,
	}
}
