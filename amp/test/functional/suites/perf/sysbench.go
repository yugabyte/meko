// sysbench is the PERFORMANCE tier's sysbench workload. It stands up the same
// co-located YugabyteDB-in-k8s backing + amp control plane + compute endpoint +
// in-cluster client pod as perf/tpcc and perf/pgbench, but drives sysbench's
// bundled oltp_* lua workloads over the pgsql driver instead of
// BenchBase/pgbench.
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
// behind a k8s cluster + a chart version. The sysbench load
// shape lives under perf.sysbench.* (script/tables/table_size/threads); the
// backing + gate knobs are shared under perf.*. Registered by init.
package perf

import (
	"context"
	"fmt"
	"log"
	"math"
	"regexp"
	"strconv"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/sysbench"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
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
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe, ClusterSlots: benchClusterSlots},
		Tags:        []string{"perf"},
		OwningTeam:  core.TeamCompute,
		Description: "Run sysbench (pgsql driver, bundled oltp_* lua workloads) through an amp compute endpoint backed by a co-located YB-in-k8s cluster, and gate tps + latency against a stored baseline.",
		Params:      []string{"perf", "apps.ybk8s", "env.ampstack"},
		Run:         runSysbench,
	})
}

// runSysbench is the live body: gate → provision the co-located YB backing →
// stand the amp control plane + compute endpoint on it → deploy the client pod +
// install sysbench → run sysbench prepare + timed runs through the endpoint →
// parse tps/latency → gate against the baseline → guaranteed teardown.
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
	if reason, ok := ampEndpointGate(t.Options); !ok {
		t.Skip(reason)
		return nil
	}

	bc := benchConfigFromConfig(t.Options)
	params := sysbenchParamsFromConfig(t.Options)
	load := loadFieldsSysbench(bc, cfg, params)
	commit := frameworkCommit()

	// Comparison mode: run the SAME sysbench at the SAME load across labelled
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
			return sysbench.EnsureInstalled(ctx, podExec)
		}
		mk := func(conn pg.ConnInfo) Workload { return sysbenchWorkloadFor(params, conn) }
		if cmp.Mode == CompareModeBisect {
			return runBisectSuite(t, bisectSpec{
				perf: cfg, cmp: cmp, bc: bc, load: load, commit: commit,
				podPrefix: "ampqa-sysbench", deliver: deliver, mkWorkload: mk, sink: sink, logf: logf,
			})
		}
		return runComparisonSuite(t, comparisonSpec{
			perf: cfg, cmp: cmp, bc: bc, load: load, commit: commit,
			podPrefix: "ampqa-sysbench", deliver: deliver, mkWorkload: mk, sink: sink, logf: logf,
		})
	}

	return runBenchProvisioned(t, benchWorkloadSpec{
		testName:     testNameSysbench,
		runStep:      "run_sysbench",
		podPrefix:    "ampqa-sysbench",
		attachPrefix: "sysbench",
		noTPSMsg:     "sysbench produced no tps — parse of sysbench output failed (see attached sysbench-iter-*.out)",
		deliver: func(ctx context.Context, _ *core.T, _ perfConfig, podExec exec.RemoteExecutor) error {
			return sysbench.EnsureInstalled(ctx, podExec)
		},
		mkWorkload: func(conn pg.ConnInfo) Workload { return sysbenchWorkloadFor(params, conn) },
		specs:      sysbenchMetricSpecs,
		datapoints: sysbenchDatapoints,
	}, cfg, bc, identityFromConfig(t.Options, "", commit, load))
}

// sysbenchWorkloadFor builds the sysbench workload bound to conn at the shared
// load (script/tables/table_size/threads). Shared by the single-run path and
// each comparison variant.
func sysbenchWorkloadFor(params sysbenchParams, conn pg.ConnInfo) Workload {
	return &sysbenchWorkload{conn: conn, params: params}
}

// sysbenchWorkload adapts the sysbench driver to the harness Workload:
// `sysbench prepare` (dataset load) is the one-time Prepare, each timed
// `sysbench run` is a RunOnce. When a sample interval is requested it passes
// --report-interval so sysbench reports periodic progress, parsed into the
// Sample's time series.
//
// It targets the amp compute endpoint, which is also all its pgsql driver can
// take: ONE --pgsql-host, with no per-connection host balancing.
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
	if p.Rate > 0 {
		opts.ExtraArgs = append(opts.ExtraArgs, fmt.Sprintf("--rate=%d", max(int(math.Round(p.Rate)), 1)))
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

// sysbenchDatapoints turns the aggregate into the tier's datapoints: saturated
// tps and the fixed-rate latencies gated, saturated latencies reported.
func sysbenchDatapoints(res BenchResult) (gated, info []core.Datapoint) {
	return splitDatapoints(sysbenchMetricSpecs(res))
}

// sysbenchMetricSpecs is the metric set perf/sysbench reports, with the
// accessors the shape gates need (sysbench --report-interval carries tps and
// latency; its progress lines report no percentile, so p95 gets no FromPoint).
func sysbenchMetricSpecs(res BenchResult) []metricSpec {
	return append([]metricSpec{
		{Name: "sysbench.tps", Unit: "tps", Dir: core.HigherIsBetter, Stats: res.TPS,
			FromSample: sampleTPS, FromPoint: pointTPS},
		{Name: "sysbench.latency_avg", Unit: "ms", Dir: core.LowerIsBetter, Stats: res.LatencyAvg,
			FromSample: sampleLatencyAvg, FromPoint: pointLatency, Info: true},
		{Name: "sysbench.latency_p95", Unit: "ms", Dir: core.LowerIsBetter, Stats: res.LatencyP95,
			FromSample: sampleLatencyP95, Info: true, NoDrift: true},
	}, fixedRateSpecs("sysbench", "latency_avg", res)...)
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
		Harness:       benchHarness(cfg),
		FixedRatePct:  fixedRatePct(bc),
	}
}
