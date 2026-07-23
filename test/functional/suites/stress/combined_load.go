// combined_load is the signature COMBINED STRESS scenario. For one co-located
// YugabyteDB-in-k8s backing it runs FOUR client workloads in parallel —
// SqlTransactions + SqlBankTransfer + a two-phase SqlDataLoad (all the Java
// sample-app) plus a BenchBase TPC-C run — inside a single in-cluster client pod,
// while the chaos NemesisLoop injects faults against the backing YB pods in the
// background. When the workloads and chaos stop it asserts the cluster is healthy
// (every tserver/master back to Ready) and a seeded workloadgen dataset loaded
// BEFORE the chaos is still fully consistent — the oracle that the combined load
// plus faults neither corrupted nor lost data.
//
// It stitches together already-built, green pieces: apps/ybk8s (backing +
// teardown), apps/clientpod (the in-cluster workload pod — finally wired via its
// pod-bound executor), apps/sampleapp + apps/benchbase (the workload drivers),
// chaos (the NemesisLoop), apps/workloadgen (the consistency oracle) and
// apps/pg. It reuses apps/ybk8s directly rather than features.StandUp because
// StandUp additionally deploys the amp control plane (which this data-plane stress
// test does not use) and hides the deployer handles (namespace, kube, tserver
// pods) the chaos targeting and client pod need.
//
// It self-provisions real infrastructure (a k8s cluster + the PRIVATE sample-app
// jar + a chart), so it SKIPS cleanly unless stress.combined.enabled and its
// config gate are satisfied. Teardown is guaranteed on every path (LIFO stack on
// its own deadline): report_kill for the workloads (logs collected before kill),
// chaos recovery, and the backing + client pod reaped (the client pod lives in
// the YB namespace, so the namespace delete cascades it too). Registered by init.
package stress

import (
	"context"
	"fmt"
	"log"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/benchbase"
	"github.com/yugabyte/amp/test/functional/framework/apps/clientpod"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/sampleapp"
	"github.com/yugabyte/amp/test/functional/framework/apps/workloadgen"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/artifact"
	"github.com/yugabyte/amp/test/functional/framework/chaos"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/diag"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// tserverSelector is the chart-fixed label on the YB tserver pods (the chaos
// targets and the coordinator-reachable YSQL endpoints).
const tserverSelector = "app=yb-tserver"

// combinedTeardownBudget bounds the whole LIFO teardown stack, independent of the
// test body deadline (AGENTS.md §5 — teardown runs on its own budget).
const combinedTeardownBudget = 20 * time.Minute

// reportKillTimeout bounds each workload's collect-then-kill at teardown.
const reportKillTimeout = 3 * time.Minute

func init() {
	core.Register(core.Test{
		Name:        "stress/combined_load",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe},
		Tags:        []string{"nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Combined stress: run four sample-app + BenchBase workloads in parallel under chaos on a co-located YB backing, then assert cluster health + data consistency.",
		Params:      []string{"stress.combined", "chaos", "apps.ybk8s"},
		Run:         runCombinedLoad,
	})
}

// runCombinedLoad is the live body: gate → provision the co-located YB backing →
// deploy the client pod + workload jars → create the sample-app schema (phase 1)
// → load the consistency dataset → run the four workloads in parallel WITH chaos
// (then health + verify) → guaranteed teardown.
func runCombinedLoad(t *core.T) (err error) {
	cfg := combinedFromConfig(t.Options)
	if reason, ok := combinedGate(t.Options, cfg); !ok {
		t.Skip(reason)
		return nil
	}
	if reason, ok := gateNoDoubleProvision(t.Env); !ok {
		t.Skip(reason)
		return nil
	}

	r := &combinedRun{
		t:    t,
		cfg:  cfg,
		td:   &teardownStack{},
		logf: t.Masker().Logf(log.Printf),
	}
	r.sink = attachSink{t: t, logf: r.logf}

	defer func() {
		// Collect k8s failure diagnostics BEFORE any teardown cleanup deletes the
		// namespace (§5 "collect before killing"), on its own budget so it can't
		// starve teardown. Gated by report.collect_diagnostics against the outcome.
		dctx, dcancel := context.WithTimeout(context.Background(), combinedTeardownBudget)
		r.collectDiagnostics(dctx, err)
		dcancel()

		tctx, cancel := context.WithTimeout(context.Background(), combinedTeardownBudget)
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
		{"schema_setup", r.schemaSetup},
		{"load_consistency_dataset", r.loadConsistencyDataset},
		{"combined_load_with_chaos", r.combinedLoadWithChaos},
	}
	for _, p := range phases {
		if err := step(p.name, p.fn); err != nil {
			return err
		}
	}
	return nil
}

// combinedRun carries the shared state the phases read/write.
type combinedRun struct {
	t    *core.T
	cfg  combinedConfig
	td   *teardownStack
	logf func(string, ...any)
	sink apps.ArtifactSink

	info          ybk8s.Info
	kube          *k8s.Kubectl // scoped to the YB namespace (chaos targets + client pod)
	inClusterHost string       // tserver Service DNS host the in-pod workloads connect at
	inClusterPort int
	jarPath       string // sample-app jar path inside the client pod
	benchbaseDir  string // unpacked benchbase dir inside the client pod
	podExec       exec.RemoteExecutor
	clientPod     *clientpod.K8s

	chaosMu       sync.Mutex
	chaosInjects  int
	chaosRecovers int
}

func (r *combinedRun) suffix() string { return r.t.ResourceSuffix() }

// provisionBacking deploys a co-located YugabyteDB cluster in k8s (RF forced up
// to cfg.RF so single-node chaos is survivable), registers its teardown, and
// resolves the two connection paths plus a namespace-scoped kubectl for the chaos
// targets and client pod.
func (r *combinedRun) provisionBacking(ctx context.Context) error {
	k8sCfg := ybk8s.FromConfig(r.t.Options)
	if k8sCfg.ReplicationFactor < r.cfg.RF {
		k8sCfg.ReplicationFactor = r.cfg.RF
	}
	// 0 → clamp to ReplicationFactor at deploy; keep them coupled so the RF the
	// chaos survive-check assumes matches the live master/tserver counts.
	k8sCfg.MasterReplicas = 0
	k8sCfg.TServerReplicas = 0

	secrets, err := core.NewSecrets(r.t.Options)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	d := ybk8s.New(k8sCfg, r.suffix()).WithLogf(r.logf).WithSecretResolver(secrets.Value)
	r.td.push("teardown yb-in-k8s (namespace + PVCs + client pod)", func(ctx context.Context) error {
		return d.Teardown(ctx)
	})

	info, err := d.Deploy(ctx)
	if err != nil {
		return fmt.Errorf("provision yb in k8s: %w", err)
	}
	r.info = info
	r.inClusterHost, r.inClusterPort = ybk8s.FirstHostPort(info.InClusterHostPorts)
	r.kube = k8s.New(k8s.Config{Context: k8sCfg.KubeContext, Namespace: info.Namespace}).
		WithRunner(k8s.KubeconfigRunner(k8sCfg.Kubeconfig))
	return nil
}

// deployClientPod launches the long-lived in-cluster client pod (sleep-forever,
// carrying Java) and delivers the sample-app + benchbase artifacts into it over
// its pod-bound executor. The pod is collected-then-deleted on teardown (before
// the namespace delete that would also cascade it).
func (r *combinedRun) deployClientPod(ctx context.Context) error {
	spec := clientpod.Spec{
		Name:    "ampqa-client-" + r.suffix(),
		Image:   r.cfg.ClientImage,
		Command: []string{"sh", "-c", "sleep infinity"},
	}
	if r.cfg.ClientPullSecret != "" {
		spec.ImagePullSecrets = []string{r.cfg.ClientPullSecret}
	}
	cp := clientpod.NewK8s(r.kube)
	if err := cp.Start(ctx, spec); err != nil {
		return fmt.Errorf("start client pod: %w", err)
	}
	r.clientPod = cp
	r.podExec = cp.Exec()
	r.td.push("report-kill client pod (collect logs + delete)", func(ctx context.Context) error {
		return cp.Stop(ctx, r.sink)
	})

	specs, err := artifact.Resolve(r.t.Options)
	if err != nil {
		return fmt.Errorf("resolve artifacts: %w", err)
	}
	secrets, err := core.NewSecrets(r.t.Options)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	jarSecret, err := secrets.Value(r.cfg.JarSecret)
	if err != nil {
		return fmt.Errorf("resolve sample-app jar secret %q: %w", r.cfg.JarSecret, err)
	}

	dep := &artifact.Deployer{}
	execs := []exec.RemoteExecutor{r.podExec}
	sa, ok := specs["sampleapp"]
	if !ok {
		return fmt.Errorf("artifacts.sampleapp is not declared")
	}
	if err := dep.Deploy(ctx, execs, sa, artifact.DeployOptions{RemoteDir: r.cfg.RemoteDir, Secret: jarSecret}); err != nil {
		return fmt.Errorf("deploy sample-app jar into client pod: %w", err)
	}
	r.jarPath = path.Join(r.cfg.RemoteDir, sa.GlobPath)

	bb, ok := specs["benchbase"]
	if !ok {
		return fmt.Errorf("artifacts.benchbase is not declared")
	}
	if err := dep.Deploy(ctx, execs, bb, artifact.DeployOptions{RemoteDir: r.cfg.RemoteDir}); err != nil {
		return fmt.Errorf("deploy benchbase into client pod: %w", err)
	}
	r.benchbaseDir = path.Join(r.cfg.RemoteDir, benchbase.DefaultSubdir)
	return nil
}

// schemaSetup runs SqlDataLoad phase 1 (schema only, --skip_workload) to
// completion, so the concurrent phase-2 data load (--skip_ddl) loads into an
// existing schema — the two-phase SqlDataLoad pattern.
func (r *combinedRun) schemaSetup(ctx context.Context) error {
	spec := r.dataLoadSpec(true, false)
	spec.RunTime = 0
	spec.Marker = "sql_data_load_schema-" + r.suffix()
	spec.UUID = r.workloadUUID("sql_data_load_schema")
	app := sampleapp.New(r.podExec, spec,
		sampleapp.WithLogf(r.logf),
		sampleapp.WithReadyTimeout(r.cfg.ReadyTimeout))
	if err := app.Start(ctx); err != nil {
		return fmt.Errorf("schema-setup start: %w", err)
	}
	// Schema-only: the jar creates tables then exits, so wait for the process to
	// finish (not for "load going", which never happens with no workload).
	if err := r.waitProcessDone(ctx, app, r.cfg.ReadyTimeout); err != nil {
		return fmt.Errorf("schema-setup did not complete: %w", err)
	}
	return app.ReportKill(ctx, r.sink)
}

// loadConsistencyDataset seeds a workloadgen dataset on the backing (over a fresh
// coordinator-reachable tserver connection) BEFORE the workloads + chaos start,
// so verifyConsistency can prove it survived them. It registers a best-effort
// drop for teardown.
func (r *combinedRun) loadConsistencyDataset(ctx context.Context) error {
	ci, err := r.freshTailnetConn(ctx)
	if err != nil {
		return err
	}
	if err := pg.WaitReady(ctx, ci, r.cfg.ReadyTimeout); err != nil {
		return fmt.Errorf("backing YSQL not ready: %w", err)
	}
	cl, err := pg.Connect(ctx, ci)
	if err != nil {
		return err
	}
	defer cl.Close()

	spec := r.wgSpec()
	if err := workloadgen.Schema(ctx, cl.Pool(), spec); err != nil {
		return fmt.Errorf("consistency schema: %w", err)
	}
	r.td.push("drop consistency dataset", r.dropConsistencyDataset)
	if err := workloadgen.Load(ctx, cl.Pool(), spec, r.consistencySeed(), nil); err != nil {
		return fmt.Errorf("consistency load: %w", err)
	}
	return nil
}

// combinedLoadWithChaos assembles the chaos runner + the four parallel workloads
// into a plan and runs it: chaos in the background while the workloads run, then
// (chaos stopped, workloads collected+killed) the health + consistency oracle.
func (r *combinedRun) combinedLoadWithChaos(ctx context.Context) error {
	runner, err := r.buildChaos(ctx)
	if err != nil {
		return err
	}
	plan := &combinedPlan{
		chaos:     runner,
		workloads: r.buildWorkloads(),
		health:    r.healthCheck,
		verify:    r.verifyConsistency,
		logf:      r.logf,
	}
	runErr := plan.run(ctx, r.sink)
	r.chaosMu.Lock()
	inj, rec := r.chaosInjects, r.chaosRecovers
	r.chaosMu.Unlock()
	r.t.Metric("chaos_injects", float64(inj), "count")
	r.t.Metric("chaos_recovers", float64(rec), "count")
	return runErr
}

// buildChaos resolves the live tserver pods as owned chaos targets and builds a
// NemesisLoop restricted to the k8s-safe action subset, its randomness forked
// from the run seed.
func (r *combinedRun) buildChaos(ctx context.Context) (*chaos.Runner, error) {
	out, err := r.kube.GetSelector(ctx, "pod", tserverSelector, "{.items[*].metadata.name}")
	if err != nil {
		return nil, fmt.Errorf("list tserver pods for chaos: %w", err)
	}
	names := strings.Fields(out)
	if len(names) == 0 {
		return nil, fmt.Errorf("no tserver pods to target with chaos")
	}
	targets := make([]*chaos.Target, 0, len(names))
	for _, p := range names {
		targets = append(targets, chaos.NewPodTarget(p, r.kube, p, ""))
	}
	ccfg := chaos.FromConfig(r.t.Options)
	ccfg.Actions = r.cfg.ChaosActions
	ccfg.Observe = r.onChaosEvent
	return chaos.NewRunner(r.t.Seed.Fork("chaos"), chaos.DefaultRegistry(), ccfg, targets)
}

// onChaosEvent folds each inject/recover into the run's counters + log (masked).
func (r *combinedRun) onChaosEvent(e chaos.Event) {
	r.chaosMu.Lock()
	switch e.Phase {
	case "inject":
		r.chaosInjects++
	case "recover":
		r.chaosRecovers++
	}
	r.chaosMu.Unlock()
	if e.Err != nil {
		r.logf("chaos %s %s on %s: %v", e.Phase, e.Action, e.Target, e.Err)
		return
	}
	r.logf("chaos %s %s on %s", e.Phase, e.Action, e.Target)
}

// buildWorkloads returns the four workloads the plan runs concurrently.
func (r *combinedRun) buildWorkloads() []*managedWorkload {
	return []*managedWorkload{
		r.sampleWorkload(sampleapp.SqlTransactions, "sql_transactions", false, false),
		r.sampleWorkload(sampleapp.SqlBankTransfer, "sql_bank_transfer", false, false),
		r.dataLoadWorkload(),
		r.benchWorkload(),
	}
}

// sampleWorkload builds a managed sample-app workload: Start → WaitReady →
// run for cfg.RunTime, with a report_kill (collect-before-stop) teardown.
func (r *combinedRun) sampleWorkload(w sampleapp.Workload, name string, skipWorkload, skipDDL bool) *managedWorkload {
	spec := sampleapp.RunSpec{
		JarPath:         r.jarPath,
		Workload:        w,
		Hosts:           []string{r.inClusterHost},
		YSQLPort:        r.inClusterPort,
		NumTables:       r.cfg.SampleTables,
		NumThreadsRead:  r.cfg.SampleThreadsRead,
		NumThreadsWrite: r.cfg.SampleThreadsWrite,
		RunTime:         r.cfg.RunTime,
		Marker:          name + "-" + r.suffix(),
		UUID:            r.workloadUUID(name),
		SkipWorkload:    skipWorkload,
		SkipDDL:         skipDDL,
	}
	return r.sampleWorkloadFromSpec(name, spec)
}

// dataLoadWorkload is the concurrent SqlDataLoad phase 2: --skip_ddl loads data
// into the schema schemaSetup created.
func (r *combinedRun) dataLoadWorkload() *managedWorkload {
	spec := r.dataLoadSpec(false, true)
	spec.Marker = "sql_data_load-" + r.suffix()
	spec.UUID = r.workloadUUID("sql_data_load")
	return r.sampleWorkloadFromSpec("sql_data_load", spec)
}

func (r *combinedRun) sampleWorkloadFromSpec(name string, spec sampleapp.RunSpec) *managedWorkload {
	app := sampleapp.New(r.podExec, spec,
		sampleapp.WithLogf(r.logf),
		sampleapp.WithReadyTimeout(r.cfg.ReadyTimeout))
	return &managedWorkload{
		name: name,
		run: func(ctx context.Context) error {
			if err := app.Start(ctx); err != nil {
				return fmt.Errorf("%s start: %w", name, err)
			}
			if err := app.WaitReady(ctx); err != nil {
				return fmt.Errorf("%s not generating load: %w", name, err)
			}
			return sleepCtx(ctx, r.cfg.RunTime)
		},
		reportKill: func(ctx context.Context, sink apps.ArtifactSink) error {
			return app.ReportKill(ctx, sink)
		},
	}
}

// dataLoadSpec is the shared SqlDataLoad spec (identical table/column shape in
// both phases so phase 2's --skip_ddl matches phase 1's schema).
func (r *combinedRun) dataLoadSpec(skipWorkload, skipDDL bool) sampleapp.RunSpec {
	return sampleapp.RunSpec{
		JarPath:         r.jarPath,
		Workload:        sampleapp.SqlDataLoad,
		Hosts:           []string{r.inClusterHost},
		YSQLPort:        r.inClusterPort,
		NumTables:       r.cfg.SampleTables,
		NumValueColumns: r.cfg.SampleValueColumns,
		NumThreadsWrite: r.cfg.SampleThreadsWrite,
		RunTime:         r.cfg.RunTime,
		SkipWorkload:    skipWorkload,
		SkipDDL:         skipDDL,
	}
}

// benchWorkload builds the BenchBase TPC-C workload. BenchBase runs to completion
// (create+load+execute) in one blocking call; its report_kill attaches the
// captured output (there is no long-lived process to kill).
func (r *combinedRun) benchWorkload() *managedWorkload {
	spec := benchbase.RunSpec{
		RunDir:     r.benchbaseDir,
		Benchmark:  benchbase.TPCC,
		Conn:       pg.ConnInfo{Host: r.inClusterHost, Port: r.inClusterPort, User: r.info.User, Database: r.info.User, SSLMode: "disable"},
		Warehouses: r.cfg.BenchWarehouses,
		Terminals:  r.cfg.BenchTerminals,
		Duration:   r.cfg.RunTime,
	}
	var mu sync.Mutex
	var raw string
	return &managedWorkload{
		name:        "benchbase_tpcc",
		tolerateErr: true, // TPC-C has no connection-death retry; a chaos node-kill aborts it (expected).
		run: func(ctx context.Context) error {
			res, err := benchbase.Run(ctx, r.podExec, spec)
			mu.Lock()
			raw = res.Raw
			mu.Unlock()
			if err != nil {
				return fmt.Errorf("benchbase tpcc: %w", err)
			}
			return nil
		},
		reportKill: func(_ context.Context, sink apps.ArtifactSink) error {
			mu.Lock()
			out := raw
			mu.Unlock()
			if out == "" {
				return nil
			}
			return sink.Add("benchbase-tpcc.out", "log", []byte(out))
		},
	}
}

// healthCheck asserts the cluster recovered from the chaos: every tserver and
// master pod is back to Ready (readyReplicas == RF).
func (r *combinedRun) healthCheck(ctx context.Context) error {
	want := fmt.Sprintf("jsonpath={.status.readyReplicas}=%d", r.cfg.RF)
	if err := r.kube.WaitFor(ctx, "statefulset/yb-tserver", want, r.cfg.HealthTimeout); err != nil {
		return fmt.Errorf("tservers not all Ready after chaos: %w", err)
	}
	if err := r.kube.WaitFor(ctx, "statefulset/yb-master", want, r.cfg.HealthTimeout); err != nil {
		return fmt.Errorf("masters not all Ready after chaos: %w", err)
	}
	return nil
}

// verifyConsistency re-verifies the seeded workloadgen dataset over a fresh
// coordinator-reachable tserver connection — the oracle that the combined load +
// faults neither corrupted nor lost the pre-loaded data.
func (r *combinedRun) verifyConsistency(ctx context.Context) error {
	ci, err := r.freshTailnetConn(ctx)
	if err != nil {
		return err
	}
	if err := pg.WaitReady(ctx, ci, r.cfg.VerifyTimeout); err != nil {
		return fmt.Errorf("backing YSQL not serving for verify: %w", err)
	}
	cl, err := pg.Connect(ctx, ci)
	if err != nil {
		return err
	}
	defer cl.Close()
	report, err := workloadgen.Verify(ctx, cl.Pool(), r.wgSpec(), r.consistencySeed())
	if err != nil {
		return fmt.Errorf("consistency verify: %w", err)
	}
	if !report.OK {
		bad := 0
		for _, tr := range report.Tables {
			if !tr.OK {
				bad++
			}
		}
		return fmt.Errorf("consistency verify failed on %d/%d table(s) after combined load + chaos", bad, len(report.Tables))
	}
	return nil
}

// dropConsistencyDataset best-effort drops the workloadgen tables at teardown
// over a fresh connection (the load pool is long gone by then).
func (r *combinedRun) dropConsistencyDataset(ctx context.Context) error {
	ci, err := r.freshTailnetConn(ctx)
	if err != nil {
		return err
	}
	cl, err := pg.Connect(ctx, ci)
	if err != nil {
		return err
	}
	defer cl.Close()
	spec := r.wgSpec()
	for i := 0; i < spec.NumTables; i++ {
		if _, derr := cl.Exec(ctx, "DROP TABLE IF EXISTS "+spec.TableName(i)); derr != nil {
			r.logf("combined teardown: drop %s (best-effort): %v", spec.TableName(i), derr)
		}
	}
	return nil
}

// freshTailnetConn resolves a currently-live tserver pod IP into a
// coordinator-reachable YSQL ConnInfo. Resolved fresh each call so a pod that
// chaos rescheduled (new IP) never leaves the oracle pointed at a dead endpoint.
func (r *combinedRun) freshTailnetConn(ctx context.Context) (pg.ConnInfo, error) {
	out, err := r.kube.GetSelector(ctx, "pod", tserverSelector, "{.items[*].status.podIP}")
	if err != nil {
		return pg.ConnInfo{}, fmt.Errorf("resolve tserver pod IPs: %w", err)
	}
	ips := strings.Fields(out)
	if len(ips) == 0 {
		return pg.ConnInfo{}, fmt.Errorf("no live tserver pod IPs")
	}
	return pg.ConnInfo{
		Host:     ips[0],
		Port:     r.info.YSQLPort,
		User:     r.info.User,
		Database: r.info.User,
		SSLMode:  "disable",
	}, nil
}

// wgSpec is the consistency-oracle workloadgen dataset (seed-unique prefix so
// parallel runs never collide).
func (r *combinedRun) wgSpec() workloadgen.Spec {
	return workloadgen.Spec{
		TablePrefix:  "cmb_" + r.suffix(),
		NumTables:    r.cfg.VerifyTables,
		RowsPerTable: r.cfg.VerifyRows,
	}
}

func (r *combinedRun) consistencySeed() int64 { return r.t.Seed.Fork("consistency").Value() }

// workloadUUID derives a deterministic RFC-4122-shaped UUID for a workload's
// --uuid. The sample-app parses --uuid with java.util.UUID.fromString, which
// demands the 8-4-4-4-12 hex grouping — a bare marker string is rejected
// ("UUID string too large"). Seed-forked per label so a rerun with the same
// seed reproduces it (and the four concurrent workloads never collide).
func (r *combinedRun) workloadUUID(label string) string {
	rnd := r.t.Seed.Fork("uuid-" + label).Rand()
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		rnd.Uint32(), uint16(rnd.Uint32()), uint16(rnd.Uint32()), uint16(rnd.Uint32()), rnd.Uint64()&0xffffffffffff)
}

// waitProcessDone polls until the sample-app process has exited (schema-only
// completion) or the timeout elapses.
func (r *combinedRun) waitProcessDone(ctx context.Context, app *sampleapp.App, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		alive, err := app.Running(wctx)
		if err == nil && !alive {
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("process still running after %s: %w", timeout, wctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// --- pure orchestration (unit-tested with fakes) ---

// chaosController is the subset of *chaos.Runner the orchestrator drives.
type chaosController interface {
	Start() error
	Stop()
}

// managedWorkload is one workload the orchestrator runs and always report-kills.
// run blocks until the workload's own duration ends; reportKill collects its logs
// THEN stops it (the report_kill rule), and is idempotent via killed.
type managedWorkload struct {
	name       string
	run        func(context.Context) error
	reportKill func(context.Context, apps.ArtifactSink) error
	killed     atomic.Bool
	// tolerateErr marks a workload whose run error is EXPECTED under chaos (a
	// batch driver with no connection-death retry, e.g. BenchBase, aborts when a
	// node it is mid-transaction on is killed). Such an error is logged/attached
	// but does not fail the scenario or skip the health + consistency oracle — the
	// oracle (cluster recovered + seeded data intact) is the real gate. A workload
	// that never STARTS (never generates load) leaves this false, so a setup bug
	// still fails hard.
	tolerateErr bool
}

// combinedPlan is the pure orchestrator: run the workloads concurrently with
// chaos in the background, then — chaos stopped and every workload collected +
// killed — assert cluster health and data consistency.
type combinedPlan struct {
	chaos     chaosController
	workloads []*managedWorkload
	health    func(context.Context) error
	verify    func(context.Context) error
	logf      func(string, ...any)
}

// run executes the scenario core. chaos.Stop and report_kill for every workload
// are guaranteed (deferred backstops) even on an early error or panic; the happy
// path runs them inline first so the health + consistency oracle observes a
// recovered, quiescent cluster. Returns the first workload/health/verify error.
func (p *combinedPlan) run(ctx context.Context, sink apps.ArtifactSink) (err error) {
	defer p.reportKillAll(sink)
	if e := p.chaos.Start(); e != nil {
		return fmt.Errorf("start chaos: %w", e)
	}
	defer p.chaos.Stop()

	fatal, tolerated := p.launchAndWait(ctx)

	// Quiesce before the oracle: recover chaos + collect/kill workloads now — even
	// when a tolerated workload aborted, so the oracle observes a recovered cluster.
	p.chaos.Stop()
	p.reportKillAll(sink)

	for _, te := range tolerated {
		if p.logf != nil {
			p.logf("combined: workload aborted under chaos (tolerated; oracle still gates): %v", te)
		}
	}
	// A workload that never generated load (not a tolerated driver) is a setup
	// failure — surface it and skip the oracle (it would observe a bad setup).
	if fatal != nil {
		return fatal
	}

	if p.health != nil {
		if e := p.health(ctx); e != nil {
			return fmt.Errorf("cluster unhealthy after combined load + chaos: %w", e)
		}
	}
	if p.verify != nil {
		return p.verify(ctx)
	}
	return nil
}

// launchAndWait runs every workload concurrently. It returns the first
// non-tolerated (fatal) error and the collected errors from tolerate-marked
// workloads separately, so a driver aborting on a chaos-induced connection reset
// does not skip the health + consistency oracle.
func (p *combinedPlan) launchAndWait(ctx context.Context) (fatal error, tolerated []error) {
	var wg sync.WaitGroup
	errs := make([]error, len(p.workloads))
	for i, w := range p.workloads {
		wg.Add(1)
		go func(i int, w *managedWorkload) {
			defer wg.Done()
			errs[i] = w.run(ctx)
		}(i, w)
	}
	wg.Wait()
	for i, e := range errs {
		if e == nil {
			continue
		}
		if p.workloads[i].tolerateErr {
			tolerated = append(tolerated, fmt.Errorf("%s: %w", p.workloads[i].name, e))
		} else if fatal == nil {
			fatal = e
		}
	}
	return fatal, tolerated
}

// reportKillAll collect-then-kills every workload exactly once, on a fresh
// background context so logs are collected even if the run ctx was cancelled.
func (p *combinedPlan) reportKillAll(sink apps.ArtifactSink) {
	for _, w := range p.workloads {
		if w.reportKill == nil || w.killed.Swap(true) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), reportKillTimeout)
		if e := w.reportKill(ctx, sink); e != nil && p.logf != nil {
			p.logf("combined: report-kill %s (best-effort): %v", w.name, e)
		}
		cancel()
	}
}

// --- config ---

// Cheap defaults: a small combined run is inexpensive. Overridable under
// stress.combined.* (configs/default.yaml).
const (
	DefaultCombinedRF                 = 3
	DefaultCombinedRunTime            = 3 * time.Minute
	DefaultCombinedClientImage        = "eclipse-temurin:21-jdk"
	DefaultCombinedRemoteDir          = "/opt/ampqa"
	DefaultCombinedJarSecret          = "gh_package"
	DefaultCombinedSampleTables       = 2
	DefaultCombinedSampleValueColumns = 10
	DefaultCombinedSampleThreadsRead  = 1
	DefaultCombinedSampleThreadsWrite = 2
	DefaultCombinedBenchWarehouses    = 1
	DefaultCombinedBenchTerminals     = 2
	DefaultCombinedVerifyTables       = 4
	DefaultCombinedVerifyRows         = 500
	DefaultCombinedReadyTimeout       = 5 * time.Minute
	DefaultCombinedVerifyTimeout      = 5 * time.Minute
	DefaultCombinedHealthTimeout      = 5 * time.Minute
)

// DefaultCombinedChaosActions is the k8s-safe subset the loop draws from: the
// pod/process-level actions that need no privileged tc/iptables tooling.
var DefaultCombinedChaosActions = []string{
	chaos.ActionPodDelete, chaos.ActionProcessKill, chaos.ActionProcessRestart,
}

// combinedConfig is the resolved stress.combined.* configuration.
type combinedConfig struct {
	Enabled            bool
	RF                 int
	RunTime            time.Duration
	ClientImage        string
	ClientPullSecret   string
	RemoteDir          string
	JarSecret          string
	SampleTables       int
	SampleValueColumns int
	SampleThreadsRead  int
	SampleThreadsWrite int
	BenchWarehouses    int
	BenchTerminals     int
	VerifyTables       int
	VerifyRows         int
	ChaosActions       []string
	ReadyTimeout       time.Duration
	VerifyTimeout      time.Duration
	HealthTimeout      time.Duration
}

// combinedFromConfig reads stress.combined.* into a combinedConfig.
func combinedFromConfig(c *core.Config) combinedConfig {
	return combinedConfig{
		Enabled:            c.GetBool("stress.combined.enabled", false),
		RF:                 c.GetInt("stress.combined.rf", DefaultCombinedRF),
		RunTime:            c.GetDuration("stress.combined.run_time", DefaultCombinedRunTime),
		ClientImage:        c.GetString("stress.combined.client_image", DefaultCombinedClientImage),
		ClientPullSecret:   c.GetString("stress.combined.client_pull_secret", ""),
		RemoteDir:          c.GetString("stress.combined.remote_dir", DefaultCombinedRemoteDir),
		JarSecret:          c.GetString("stress.combined.jar_secret", DefaultCombinedJarSecret),
		SampleTables:       c.GetInt("stress.combined.sample_tables", DefaultCombinedSampleTables),
		SampleValueColumns: c.GetInt("stress.combined.sample_value_columns", DefaultCombinedSampleValueColumns),
		SampleThreadsRead:  c.GetInt("stress.combined.sample_threads_read", DefaultCombinedSampleThreadsRead),
		SampleThreadsWrite: c.GetInt("stress.combined.sample_threads_write", DefaultCombinedSampleThreadsWrite),
		BenchWarehouses:    c.GetInt("stress.combined.benchbase_warehouses", DefaultCombinedBenchWarehouses),
		BenchTerminals:     c.GetInt("stress.combined.benchbase_terminals", DefaultCombinedBenchTerminals),
		VerifyTables:       c.GetInt("stress.combined.verify_tables", DefaultCombinedVerifyTables),
		VerifyRows:         c.GetInt("stress.combined.verify_rows", DefaultCombinedVerifyRows),
		ChaosActions:       c.GetStringSlice("stress.combined.chaos_actions", DefaultCombinedChaosActions),
		ReadyTimeout:       c.GetDuration("stress.combined.ready_timeout", DefaultCombinedReadyTimeout),
		VerifyTimeout:      c.GetDuration("stress.combined.verify_timeout", DefaultCombinedVerifyTimeout),
		HealthTimeout:      c.GetDuration("stress.combined.health_timeout", DefaultCombinedHealthTimeout),
	}
}

// combinedGate returns a skip reason (and false) unless the combined stress
// scenario may run: enabled, a k8s cluster to deploy into, a resolvable chart
// version, and the PRIVATE sample-app jar secret resolvable. It never resolves a
// secret VALUE into the reason — only names the missing secret — so credentials
// never leak into a skip line.
func combinedGate(c *core.Config, cfg combinedConfig) (string, bool) {
	if !cfg.Enabled {
		return "stress.combined.enabled is not set (self-provisioning; needs a k8s cluster + the private sample-app jar)", false
	}
	if c.GetString("apps.ybk8s.kubecontext", "") == "" &&
		c.GetString("apps.ybk8s.kubeconfig", "") == "" &&
		c.GetString("env.ampstack.context", "") == "" {
		return "combined stress needs a k8s cluster: set apps.ybk8s.kubecontext or apps.ybk8s.kubeconfig (or env.ampstack.context)", false
	}
	chart := c.GetString("apps.ybk8s.chart_version", "")
	if chart == "" {
		chart = c.GetString("versions.yugabytedb", "")
	}
	if chart == "" {
		return "combined stress needs a YB chart version: set apps.ybk8s.chart_version or versions.yugabytedb", false
	}
	secrets, err := core.NewSecrets(c)
	if err != nil {
		return "secrets not configured: " + err.Error(), false
	}
	if _, err := secrets.Value(cfg.JarSecret); err != nil {
		return fmt.Sprintf("sample-app jar secret %q not resolvable: declare it under secrets: (e.g. gh_package: {env: GH_PACKAGE_TOKEN})", cfg.JarSecret), false
	}
	return "", true
}

// modeReporter is implemented by env providers that expose their resolved
// provisioning mode (env/universe.Env). Declared locally so the gate stays
// decoupled from the concrete package and unit-testable with a bare fake.
type modeReporter interface{ Mode() string }

// gateNoDoubleProvision skips the combined test when the resolved env layer is
// itself a real-infra universe.Env in a provisioning mode: the test
// self-provisions its OWN backing, so running the body would provision a SECOND
// real backing on top of the first (the double-spend footgun). --fake-env (no
// Mode method) and attach-mode envs (a mode outside the provisioning set) pass.
func gateNoDoubleProvision(env core.Environment) (string, bool) {
	mr, ok := env.(modeReporter)
	if !ok {
		return "", true
	}
	switch mr.Mode() {
	case universe.ModeProvisionYugabyted, universe.ModeProvisionYBA:
		return fmt.Sprintf("combined stress self-provisions its backing; run with --fake-env — env.universe.mode=%s would double-provision", mr.Mode()), false
	}
	return "", true
}

// --- helpers ---

// teardownStack is a LIFO stack of best-effort cleanups run at the end regardless
// of outcome: every cleanup executes (most recent first) even when an earlier one
// errs, and the first error is returned — nothing is left running because one
// teardown step failed.
type teardownStack struct {
	mu  sync.Mutex
	fns []namedCleanup
}

type namedCleanup struct {
	name string
	fn   func(context.Context) error
}

func (s *teardownStack) push(name string, fn func(context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fns = append(s.fns, namedCleanup{name, fn})
}

func (s *teardownStack) run(ctx context.Context, logf func(string, ...any)) error {
	s.mu.Lock()
	fns := s.fns
	s.fns = nil
	s.mu.Unlock()
	var firstErr error
	for i := len(fns) - 1; i >= 0; i-- {
		c := fns[i]
		if err := c.fn(ctx); err != nil {
			if logf != nil {
				logf("combined teardown: %s (best-effort): %v", c.name, err)
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", c.name, err)
			}
		}
	}
	return firstErr
}

// attachSink records every collected blob on the result AND persists its bytes
// (via T.AttachBytes), so workload logs, benchbase output, and failure
// diagnostics land on disk under the run's artifacts dir — satisfying
// report_kill's collect step with the evidence, not just its size.
type attachSink struct {
	t    *core.T
	logf func(string, ...any)
}

func (s attachSink) Add(name, kind string, content []byte) error {
	s.t.AttachBytes(name, kind, content)
	if s.logf != nil {
		s.logf("collected %s (%s, %d bytes)", name, kind, len(content))
	}
	return nil
}

// collectDiagnostics is the pre-teardown diagnostics hook: before the namespace
// delete, gather the YB-namespace k8s failure evidence (tserver/master pod logs
// under "Universe logs/<pod>/<role>/", every pod's describe, events) into the
// persisting sink. Gated by report.collect_diagnostics against the run outcome
// (runErr is the phase error; a body may also have marked t failed).
// Best-effort; never blocks teardown.
func (r *combinedRun) collectDiagnostics(ctx context.Context, runErr error) {
	mode := r.t.Options.GetString("report.collect_diagnostics", core.DefaultCollectDiagnostics)
	failed := runErr != nil || r.t.Failed()
	if !diag.ShouldCollect(mode, failed) {
		return
	}
	if r.kube == nil {
		return // backing never provisioned; no namespace client to reach
	}
	diag.CollectUniverseK8sDiagnostics(ctx, r.kube, r.info.Namespace, r.sink, r.logf)
}

// sleepCtx waits d, returning early (with ctx.Err) if the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
