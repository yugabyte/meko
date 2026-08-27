// pgbench is the PERFORMANCE tier's pgbench workload. It stands up the same
// co-located YugabyteDB-in-k8s backing + amp control plane + compute endpoint +
// in-cluster client pod as perf/tpcc, but drives pgbench instead of BenchBase.
//
// YugabyteDB does NOT bundle upstream pgbench — the release tarball ships YB's
// own fork, `ysql_bench`, at postgres/bin/ysql_bench. perf/pgbench delivers that
// tarball into the client pod (the `pgbench` artifact — the same S3 release
// apps/ybpod deploys, fetched coordinator-side then kubectl-cp'd in) and installs
// a tiny /usr/local/bin/pgbench wrapper that execs ysql_bench with the tarball's
// postgres/lib on LD_LIBRARY_PATH. That bridges the version-matched YB pgbench to
// the bare `pgbench` name apps/pgbench shells — no reimplementation of the driver.
//
// It runs pgbench.Init (-i -s scale) then pgbench.Run (-c -j -T) over the pod
// executor, emits tps + average latency as gated core.Datapoints, and gates them
// against the stored baseline exactly like perf/tpcc (reusing gatePerf/baseline).
// Teardown is guaranteed (LIFO stack on its own deadline). Gated behind
// a k8s cluster + a chart version, same as perf/tpcc. The pgbench
// load shape lives under perf.pgbench.* (scale/clients/jobs/duration); the
// backing + gate knobs are shared under perf.*. Registered by init.
//
// StandUpPgbenchClient is the same glue reused as a SETUP-ONLY preset (see the
// presets suite): it stands the backing + client pod + pgbench up, loads the
// pgbench schema, and LEAVES them running so a developer can drive load by hand.
package perf

import (
	"context"
	"fmt"
	"log"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/pgbench"
	"github.com/yugabyte/amp/test/functional/framework/artifact"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// testNamePgbench is the registered name and the baseline key for the pgbench test.
const testNamePgbench = "perf/pgbench"

// pgbenchArtifactName is the config artifacts{} entry carrying the YB release
// tarball perf/pgbench mines ysql_bench out of.
const pgbenchArtifactName = "pgbench"

// pgbenchShimPath is where the client-PATH `pgbench` wrapper is installed. It is
// on the default PATH of the (Debian/Ubuntu-family) client image, so the bare
// `pgbench` name apps/pgbench shells resolves to it.
const pgbenchShimPath = "/usr/local/bin/pgbench"

// ysqlBenchRelPath / pgLibRelPath are the tarball-relative locations of YB's
// pgbench fork and the postgres client libs it links (libpq).
const (
	ysqlBenchRelPath = "postgres/bin/ysql_bench"
	pgLibRelPath     = "postgres/lib"
)

// Cheap defaults for the pgbench load shape (perf.pgbench.*). The backing + gate
// knobs are shared with perf/tpcc under perf.* (perfConfig).
const (
	DefaultPgbenchScale    = 10
	DefaultPgbenchClients  = 8
	DefaultPgbenchJobs     = 2
	DefaultPgbenchDuration = 90 * time.Second
)

// pgbenchParams is the resolved perf.pgbench.* load shape.
type pgbenchParams struct {
	Scale    int
	Clients  int
	Jobs     int
	Duration time.Duration
}

// pgbenchParamsFromConfig reads perf.pgbench.* into a pgbenchParams.
func pgbenchParamsFromConfig(c *core.Config) pgbenchParams {
	return pgbenchParams{
		Scale:    c.GetInt("perf.pgbench.scale", DefaultPgbenchScale),
		Clients:  c.GetInt("perf.pgbench.clients", DefaultPgbenchClients),
		Jobs:     c.GetInt("perf.pgbench.jobs", DefaultPgbenchJobs),
		Duration: c.GetDuration("perf.pgbench.duration", DefaultPgbenchDuration),
	}
}

func init() {
	core.Register(core.Test{
		Name:        testNamePgbench,
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe},
		Tags:        []string{"perf", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Run pgbench (YB's ysql_bench fork) through an amp compute endpoint backed by a co-located YB-in-k8s cluster, and gate tps + latency against a stored baseline.",
		Params:      []string{"perf", "apps.ybk8s", "env.ampstack"},
		Run:         runPgbench,
	})
}

// runPgbench is the live body: gate → provision the co-located YB backing →
// stand the amp control plane + compute endpoint on it → deploy the client pod +
// deliver pgbench → run pgbench Init+Run through the endpoint → parse
// tps/latency → gate against the baseline → guaranteed teardown.
func runPgbench(t *core.T) (err error) {
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
	params := pgbenchParamsFromConfig(t.Options)
	load := loadFieldsPgbench(bc, cfg, params)
	commit := frameworkCommit()

	// Comparison mode: run the SAME pgbench at the SAME load across labelled
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
			return deliverPgbenchArtifact(ctx, t, cfg, podExec)
		}
		mk := func(conn pg.ConnInfo) Workload { return pgbenchWorkloadFor(params, conn) }
		if cmp.Mode == CompareModeBisect {
			return runBisectSuite(t, bisectSpec{
				perf: cfg, cmp: cmp, bc: bc, load: load, commit: commit,
				podPrefix: "ampqa-pgbench", deliver: deliver, mkWorkload: mk, sink: sink, logf: logf,
			})
		}
		return runComparisonSuite(t, comparisonSpec{
			perf: cfg, cmp: cmp, bc: bc, load: load, commit: commit,
			podPrefix: "ampqa-pgbench", deliver: deliver, mkWorkload: mk, sink: sink, logf: logf,
		})
	}

	return runBenchProvisioned(t, benchWorkloadSpec{
		testName:     testNamePgbench,
		runStep:      "run_pgbench",
		podPrefix:    "ampqa-pgbench",
		attachPrefix: "pgbench",
		noTPSMsg:     "pgbench produced no tps — parse of pgbench output failed (see attached pgbench-iter-*.out)",
		deliver:      deliverPgbenchArtifact,
		mkWorkload:   func(conn pg.ConnInfo) Workload { return pgbenchWorkloadFor(params, conn) },
		specs:        pgbenchMetricSpecs,
		datapoints:   pgbenchDatapoints,
	}, cfg, bc, identityFromConfig(t.Options, "", commit, load))
}

// deliverPgbenchArtifact resolves the pgbench artifact (the YB release tarball)
// and delivers it + the `pgbench` shim into the client pod. Shared by the
// single-run path and each comparison variant.
func deliverPgbenchArtifact(ctx context.Context, t *core.T, cfg perfConfig, podExec exec.RemoteExecutor) error {
	spec, err := pgbenchArtifact(t.Options)
	if err != nil {
		return err
	}
	if _, err := deliverPgbench(ctx, &artifact.Deployer{}, podExec, spec, cfg.RemoteDir); err != nil {
		return err
	}
	return nil
}

// pgbenchWorkloadFor builds the pgbench workload bound to conn at the shared
// load (scale/clients/jobs). Shared by the single-run path and each variant.
func pgbenchWorkloadFor(params pgbenchParams, conn pg.ConnInfo) Workload {
	return &pgbenchWorkload{conn: conn, scale: params.Scale, clients: params.Clients, jobs: params.Jobs}
}

// pgbenchWorkload adapts the pgbench driver to the harness Workload: `pgbench
// -i` (schema load) is the one-time Prepare, each `pgbench -c -j -T` is a
// RunOnce. When a sample interval is requested it passes `-P <sec>` so pgbench
// reports periodic progress, parsed into the Sample's time series.
//
// It targets the amp compute endpoint — one host, which is all this driver can
// take anyway: it is YB's ysql_bench, linking the release tarball's own libpq,
// and libpq only gained per-connection host balancing (load_balance_hosts) in
// PostgreSQL 16.
type pgbenchWorkload struct {
	conn    pg.ConnInfo
	scale   int
	clients int
	jobs    int
}

func (w *pgbenchWorkload) Name() string { return testNamePgbench }

// Prepare loads the pgbench schema once (pgbench -i -s scale).
func (w *pgbenchWorkload) Prepare(ctx context.Context, ex exec.RemoteExecutor) error {
	if err := pgbench.Init(ctx, ex, pgbench.InitOptions{Conn: w.conn, Scale: w.scale}); err != nil {
		return fmt.Errorf("pgbench init: %w", err)
	}
	return nil
}

// RunOnce drives one timed pgbench run. pgbench has no percentile output, so the
// Sample carries tps + average latency only; -P progress feeds Series.
func (w *pgbenchWorkload) RunOnce(ctx context.Context, ex exec.RemoteExecutor, p RunParams) (Sample, error) {
	opts := pgbench.RunOptions{Conn: w.conn, Clients: w.clients, Jobs: w.jobs, Duration: p.Duration}
	if secs := int(p.SampleInterval.Round(time.Second) / time.Second); secs > 0 {
		opts.ExtraArgs = []string{"-P", strconv.Itoa(secs)}
	}
	res, err := pgbench.Run(ctx, ex, opts)
	s := Sample{TPS: res.TPS, LatencyAvgMS: res.LatencyAvgMs, Series: parsePgbenchProgress(res.Raw), Raw: res.Raw}
	if err != nil {
		return s, fmt.Errorf("pgbench run: %w", err)
	}
	return s, nil
}

// pgbenchProgressRe matches pgbench's `-P` progress lines, e.g.
// "progress: 10.0 s, 452.1 tps, lat 22.121 ms stddev 5.2".
var pgbenchProgressRe = regexp.MustCompile(`progress:\s*([0-9.]+)\s*s,\s*([0-9.]+)\s*tps,\s*lat\s*([0-9.]+)\s*ms`)

// parsePgbenchProgress extracts the periodic progress readings from raw pgbench
// output into a per-run time series. No progress lines yields an empty series.
func parsePgbenchProgress(raw string) []SamplePoint {
	var series []SamplePoint
	for _, m := range pgbenchProgressRe.FindAllStringSubmatch(raw, -1) {
		elapsed, _ := strconv.ParseFloat(m[1], 64)
		tps, _ := strconv.ParseFloat(m[2], 64)
		lat, _ := strconv.ParseFloat(m[3], 64)
		series = append(series, SamplePoint{ElapsedSec: elapsed, TPS: tps, LatencyMS: lat})
	}
	return series
}

// pgbenchDatapoints turns the aggregate into the tier's datapoints: the GATED
// medians (tps higher-is-better; average latency lower-is-better) and the
// informational .cv noise signal per metric.
func pgbenchDatapoints(res BenchResult) (gated, info []core.Datapoint) {
	return splitDatapoints(pgbenchMetricSpecs(res))
}

// pgbenchMetricSpecs is the metric set perf/pgbench reports, with the accessors
// the shape gates need (pgbench's -P progress carries tps and latency).
func pgbenchMetricSpecs(res BenchResult) []metricSpec {
	return []metricSpec{
		{Name: "pgbench.tps", Unit: "tps", Dir: core.HigherIsBetter, Stats: res.TPS,
			FromSample: sampleTPS, FromPoint: pointTPS},
		{Name: "pgbench.latency_avg", Unit: "ms", Dir: core.LowerIsBetter, Stats: res.LatencyAvg,
			FromSample: sampleLatencyAvg, FromPoint: pointLatency},
	}
}

// --- shared pgbench glue (test + preset) ---

// pgbenchDeployer is the subset of *artifact.Deployer deliverPgbench uses;
// injectable so tests assert the tarball delivery without a real fetch/kubectl-cp.
type pgbenchDeployer interface {
	Deploy(ctx context.Context, execs []exec.RemoteExecutor, spec artifact.Spec, opts artifact.DeployOptions) error
}

// pgbenchArtifact resolves the `pgbench` artifact (the YB release tarball) from
// config.
func pgbenchArtifact(c *core.Config) (artifact.Spec, error) {
	specs, err := artifact.Resolve(c)
	if err != nil {
		return artifact.Spec{}, fmt.Errorf("resolve artifacts: %w", err)
	}
	spec, ok := specs[pgbenchArtifactName]
	if !ok {
		return artifact.Spec{}, fmt.Errorf("artifacts.%s is not declared", pgbenchArtifactName)
	}
	return spec, nil
}

// deliverPgbench delivers the YB release tarball into the client pod over
// podExec and installs the /usr/local/bin/pgbench wrapper that execs the bundled
// ysql_bench. It returns the install root (the tarball's extracted directory
// under remoteDir). The delivery happens before any pgbench Init/Run.
func deliverPgbench(ctx context.Context, dep pgbenchDeployer, podExec exec.RemoteExecutor, spec artifact.Spec, remoteDir string) (string, error) {
	if err := dep.Deploy(ctx, []exec.RemoteExecutor{podExec}, spec, artifact.DeployOptions{RemoteDir: remoteDir}); err != nil {
		return "", fmt.Errorf("deploy pgbench (yb tarball) into client pod: %w", err)
	}
	installRoot := path.Join(remoteDir, spec.Version)
	res, err := podExec.Run(ctx, installShimCmd(installRoot))
	if err != nil {
		return "", fmt.Errorf("install pgbench shim: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("install pgbench shim exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return installRoot, nil
}

// pgbenchShimScript is the /usr/local/bin/pgbench wrapper body: it execs the YB
// tarball's ysql_bench with the tarball's postgres/lib on LD_LIBRARY_PATH (libpq
// et al.), forwarding all arguments. installRoot is the tarball's extracted dir.
func pgbenchShimScript(installRoot string) string {
	return fmt.Sprintf("#!/bin/sh\nexport LD_LIBRARY_PATH=%s:${LD_LIBRARY_PATH}\nexec %s \"$@\"\n",
		path.Join(installRoot, pgLibRelPath), path.Join(installRoot, ysqlBenchRelPath))
}

// installShimCmd is the remote command that writes pgbenchShimScript to
// pgbenchShimPath (on the client PATH) and makes it executable.
func installShimCmd(installRoot string) string {
	return fmt.Sprintf("mkdir -p %s && printf '%%s' %s > %s && chmod +x %s",
		shellQuote(path.Dir(pgbenchShimPath)),
		shellQuote(pgbenchShimScript(installRoot)),
		shellQuote(pgbenchShimPath),
		shellQuote(pgbenchShimPath))
}
