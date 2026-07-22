// pgbench is the PERFORMANCE tier's pgbench workload. It stands up the same
// co-located YugabyteDB-in-k8s backing + in-cluster client pod as perf/tpcc,
// but drives pgbench instead of BenchBase.
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
// perf.enabled + a k8s cluster + a chart version, same as perf/tpcc. The pgbench
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

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/pgbench"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/artifact"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
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
		Description: "Run pgbench (YB's ysql_bench fork) on a co-located YB-in-k8s backing and gate tps + latency against a stored baseline.",
		Params:      []string{"perf", "apps.ybk8s"},
		Run:         runPgbench,
	})
}

// runPgbench is the live body: gate → provision the co-located YB backing →
// deploy the client pod + deliver pgbench → wait for YSQL → run pgbench Init+Run
// → parse tps/latency → gate against the baseline → guaranteed teardown.
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

	bc := benchConfigFromConfig(t.Options)
	params := pgbenchParamsFromConfig(t.Options)
	loadSig := LoadSignature(loadFieldsPgbench(bc, cfg, params))
	commit := frameworkCommit()

	// Comparison mode: run the SAME pgbench at the SAME load across labelled
	// variants (differing only in the overrides), instead of gating one run.
	// perf.compare.mode picks the strategy over that ordered variant list: "all"
	// benchmarks every variant (the N-way comparison), "bisect" binary-searches
	// them for the first regressed one.
	if cmp := compareConfigFromConfig(t.Options); len(cmp.Variants) > 0 {
		logf := t.Masker().Logf(log.Printf)
		sink := attachSink{t: t, logf: logf}
		deliver := func(ctx context.Context, podExec exec.RemoteExecutor) error {
			return deliverPgbenchArtifact(ctx, t, cfg, podExec)
		}
		mk := func(conn pg.ConnInfo) Workload { return pgbenchWorkloadFor(params, conn) }
		if cmp.Mode == CompareModeBisect {
			return runBisectSuite(t, bisectSpec{
				perf: cfg, cmp: cmp, bc: bc, loadSig: loadSig, commit: commit,
				podPrefix: "ampqa-pgbench", deliver: deliver, mkWorkload: mk, sink: sink, logf: logf,
			})
		}
		return runComparisonSuite(t, comparisonSpec{
			perf: cfg, cmp: cmp, bc: bc, loadSig: loadSig, commit: commit,
			podPrefix: "ampqa-pgbench", deliver: deliver, mkWorkload: mk, sink: sink, logf: logf,
		})
	}

	r := &pgbenchRun{
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
		{"run_pgbench", r.runBenchmark},
		{"gate_baseline", r.gateBaseline},
	}
	for _, p := range phases {
		if err := step(p.name, p.fn); err != nil {
			return err
		}
	}
	return nil
}

// pgbenchRun carries the shared state the phases read/write.
type pgbenchRun struct {
	t       *core.T
	cfg     perfConfig
	bc      BenchConfig
	hcfg    historyConfig
	params  pgbenchParams
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

func (r *pgbenchRun) suffix() string { return r.t.ResourceSuffix() }

// provisionBacking deploys the co-located YB backing and resolves the in-cluster
// connection path plus a namespace-scoped kubectl.
func (r *pgbenchRun) provisionBacking(ctx context.Context) error {
	info, kube, err := deployYBBacking(ctx, r.t, r.cfg.RF, r.td, r.logf)
	if err != nil {
		return err
	}
	r.info = info
	r.kube = kube
	r.inClusterHost, r.inClusterPort = ybk8s.FirstHostPort(info.InClusterHostPorts)
	return nil
}

// deployClientPod launches the client pod and delivers the pgbench binary
// (YB's ysql_bench, wrapped as `pgbench`) into it — before any pgbench Init/Run.
func (r *pgbenchRun) deployClientPod(ctx context.Context) error {
	podExec, err := startPerfClientPod(ctx, r.kube, r.sink, r.td, "ampqa-pgbench-"+r.suffix(), r.cfg.ClientImage, r.cfg.ClientPullSecret)
	if err != nil {
		return err
	}
	r.podExec = podExec
	return deliverPgbenchArtifact(ctx, r.t, r.cfg, r.podExec)
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

// waitReady blocks until the backing serves YSQL.
func (r *pgbenchRun) waitReady(ctx context.Context) error {
	return waitBackingYSQL(ctx, r.kube, r.info, r.cfg.RF, r.cfg.ReadyTimeout)
}

// conn is the in-cluster YSQL target reached through the tserver Service.
func (r *pgbenchRun) conn() pg.ConnInfo {
	return pg.ConnInfo{Host: r.inClusterHost, Port: r.inClusterPort, User: r.info.User, Database: r.info.User, SSLMode: "disable"}
}

// runBenchmark drives pgbench through the harness: load the schema ONCE, then a
// discarded warmup + N measured runs, aggregated with robust statistics. Every
// run's captured output is attached for triage regardless of parse/gate outcome.
func (r *pgbenchRun) runBenchmark(ctx context.Context) error {
	wl := &pgbenchWorkload{
		conn:    r.conn(),
		scale:   r.params.Scale,
		clients: r.params.Clients,
		jobs:    r.params.Jobs,
	}
	res, err := RunBenchmark(ctx, r.podExec, wl, r.bc)
	attachBenchSamples(r.sink, res, "pgbench")
	if err != nil {
		return err
	}
	if len(res.Kept) == 0 || res.TPS.Median == 0 {
		return fmt.Errorf("pgbench produced no tps — parse of pgbench output failed (see attached pgbench-iter-*.out)")
	}
	r.benchResult = res
	emitBenchCharts(r.t, res)
	return nil
}

// gateBaseline emits the aggregate's datapoints: the medians gated against the
// baseline, and the .cv noise signal informationally, exactly like perf/tpcc.
// It then records this run in the history store and emits the cross-run trend
// chart(s), before propagating any gate failure.
func (r *pgbenchRun) gateBaseline(_ context.Context) error {
	gated, info := pgbenchDatapoints(r.benchResult)
	gateErr := emitAndGate(r.t, r.cfg, r.version, testNamePgbench, gated, info, r.id, r.logf)
	emitHistoryTrend(r.t, r.hcfg, testNamePgbench, r.version, r.benchResult, r.id, r.logf)
	return gateErr
}

// pgbenchWorkload adapts the pgbench driver to the harness Workload: `pgbench
// -i` (schema load) is the one-time Prepare, each `pgbench -c -j -T` is a
// RunOnce. When a sample interval is requested it passes `-P <sec>` so pgbench
// reports periodic progress, parsed into the Sample's time series.
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
	return splitDatapoints([]metricSpec{
		{"pgbench.tps", "tps", core.HigherIsBetter, res.TPS},
		{"pgbench.latency_avg", "ms", core.LowerIsBetter, res.LatencyAvg},
	})
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
