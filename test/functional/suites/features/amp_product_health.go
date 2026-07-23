// amp_product_health is the GLOBAL PRODUCT HEALTH scenario: one end-to-end run
// proving the amp product works as a whole under real use. Sustained sample-app
// workloads run in the background THROUGH an amp compute endpoint (from a client
// pod inside the control-plane namespace), while the foreground exercises the
// control plane under that load — an LSN fork with a divergence assertion, and a
// delete+recreate endpoint lifecycle on the forked branch. A workloadgen dataset
// seeded on the base branch before the fork is the consistency oracle: it must
// verify clean on the LSN-forked child AND on the base after all the churn.
// No chaos here — a background workload dying mid-run is a product failure.
package features

import (
	"context"
	"fmt"
	"net"
	"path"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/clientpod"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/sampleapp"
	"github.com/yugabyte/amp/test/functional/framework/apps/workloadgen"
	"github.com/yugabyte/amp/test/functional/framework/artifact"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

const (
	// healthEndpointPortFallback matches env/sharedk8s DefaultEndpointPort — used
	// only when the controller-reported endpoint address carries no port.
	healthEndpointPortFallback = 55433
	// healthMonitorGrace: a workload exiting this close to its own --run_time end
	// is a natural finish, not a mid-run death.
	healthMonitorGrace = 20 * time.Second
	// healthKillTimeout bounds each workload's collect-then-kill.
	healthKillTimeout = 3 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_product_health",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Global product health: sustained sample-app workloads through an amp endpoint while the control plane is exercised (LSN fork, endpoint delete+recreate), gated by a workloadgen consistency oracle and controller health.",
		Params:      []string{"health", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpProductHealth,
	})
}

func runAmpProductHealth(t *core.T) error {
	cfg := healthFromConfig(t.Options)
	if reason, ok := healthGate(t.Options, cfg); !ok {
		t.Skip(reason)
		return nil
	}
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		h := &healthRun{a: a, cfg: cfg, sink: attachSink{t: a.f.t, logf: a.f.logf}}
		return []phase{
			{"base_endpoint", h.baseEndpoint},
			{"load_oracle_dataset", h.loadOracleDataset},
			{"deploy_client_pod", h.deployClientPod},
			{"background_workloads", h.backgroundWorkloads},
			{"fork_under_load", h.forkUnderLoad},
			{"endpoint_lifecycle", h.endpointLifecycle},
			{"stop_workloads", h.stopWorkloads},
			{"final_verify", h.finalVerify},
		}
	})
}

// healthRun carries the state the phases read/write.
type healthRun struct {
	a    *ampFeatureRun
	cfg  healthConfig
	sink attachSink

	baseEP controller.Endpoint
	basePG *pg.Client // held open through the load/fork phases; closed before final_verify

	clientPod *clientpod.K8s
	jarPath   string

	workloads []*healthWorkload
	wg        sync.WaitGroup
	deadline  time.Time

	childBranch controller.Branch
	childEP     controller.Endpoint
	childPG     *pg.Client
}

func (h *healthRun) suffix() string { return h.a.f.suffix() }

func (h *healthRun) markerTable() string { return "hlth_marker_" + h.suffix() }

func (h *healthRun) oracleSpec() workloadgen.Spec {
	return workloadgen.Spec{
		TablePrefix:  "hlth_" + h.suffix(),
		NumTables:    h.cfg.VerifyTables,
		RowsPerTable: h.cfg.VerifyRows,
	}
}

func (h *healthRun) oracleSeed() int64 { return h.a.f.t.Seed.Fork("health-oracle").Value() }

// healthEndpointPG opens a pooled pg client to an amp endpoint (attach mode goes
// through the provider's Service port-forward; self-provision resolves the
// address directly) — the *pg.Client shape workloadgen needs a Pool() from.
func (h *healthRun) healthEndpointPG(ctx context.Context, ep controller.Endpoint) (*pg.Client, error) {
	if h.a.f.ampEnv != nil {
		return h.a.f.ampEnv.ConnectEndpoint(ctx, ep)
	}
	ci, err := h.a.f.endpointConnInfo(ep)
	if err != nil {
		return nil, err
	}
	return connectPG(ctx, ci)
}

// baseEndpoint creates the read_write endpoint on the project's default branch
// and holds a connection for the oracle load + LSN capture.
func (h *healthRun) baseEndpoint(ctx context.Context) error {
	cl, err := h.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, h.a.proj.poolID, h.a.proj.projID, h.a.proj.branchID)
	if err != nil {
		return fmt.Errorf("create base endpoint: %w", err)
	}
	h.baseEP = ep
	pgc, err := h.healthEndpointPG(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect base endpoint: %w", err)
	}
	h.basePG = pgc
	h.a.f.register("close base endpoint connection", func(context.Context) error {
		h.closeBasePG()
		return nil
	})
	return nil
}

func (h *healthRun) closeBasePG() {
	if h.basePG != nil {
		h.basePG.Close()
		h.basePG = nil
	}
}

// loadOracleDataset seeds the workloadgen consistency dataset on the base branch
// BEFORE the fork LSN, so the child must inherit it clean.
func (h *healthRun) loadOracleDataset(ctx context.Context) error {
	spec := h.oracleSpec()
	if err := workloadgen.Schema(ctx, h.basePG.Pool(), spec); err != nil {
		return fmt.Errorf("oracle schema: %w", err)
	}
	if err := workloadgen.Load(ctx, h.basePG.Pool(), spec, h.oracleSeed(), nil); err != nil {
		return fmt.Errorf("oracle load: %w", err)
	}
	return nil
}

// deployClientPod launches the sleep-forever client pod in the CONTROL-PLANE
// namespace (so the endpoint Service is reachable in-cluster) and delivers the
// sample-app jar into it.
func (h *healthRun) deployClientPod(ctx context.Context) error {
	kube := h.a.f.ampKube()
	if kube == nil {
		return fmt.Errorf("no kubectl for the control-plane namespace (direct-URL control plane); the client pod cannot be deployed")
	}
	spec := clientpod.Spec{
		Name:    "ampqa-health-" + h.suffix(),
		Image:   h.cfg.ClientImage,
		Command: []string{"sh", "-c", "sleep infinity"},
	}
	if h.cfg.ClientPullSecret != "" {
		spec.ImagePullSecrets = []string{h.cfg.ClientPullSecret}
	}
	cp := clientpod.NewK8s(kube)
	if err := cp.Start(ctx, spec); err != nil {
		return fmt.Errorf("start client pod: %w", err)
	}
	h.clientPod = cp
	h.a.f.register("report-kill client pod (collect logs + delete)", func(ctx context.Context) error {
		return cp.Stop(ctx, h.sink)
	})

	specs, err := artifact.Resolve(h.a.f.t.Options)
	if err != nil {
		return fmt.Errorf("resolve artifacts: %w", err)
	}
	sa, ok := specs["sampleapp"]
	if !ok {
		return fmt.Errorf("artifacts.sampleapp is not declared")
	}
	secrets, err := core.NewSecrets(h.a.f.t.Options)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	jarSecret, err := secrets.Value(h.cfg.JarSecret)
	if err != nil {
		return fmt.Errorf("resolve sample-app jar secret %q: %w", h.cfg.JarSecret, err)
	}
	dep := &artifact.Deployer{}
	if err := dep.Deploy(ctx, []exec.RemoteExecutor{cp.Exec()}, sa, artifact.DeployOptions{RemoteDir: h.cfg.RemoteDir, Secret: jarSecret}); err != nil {
		return fmt.Errorf("deploy sample-app jar into client pod: %w", err)
	}
	h.jarPath = path.Join(h.cfg.RemoteDir, sa.GlobPath)
	return nil
}

// healthWorkload is one background sample-app run: started + WaitReady in the
// launch phase, then liveness-monitored in a goroutine until its run window ends.
type healthWorkload struct {
	name   string
	app    *sampleapp.App
	err    error
	killed atomic.Bool
}

// endpointHostPort resolves the in-cluster address the client pod dials: the
// endpoint's Service name (yb-ep-<id>, same namespace) at the compute port
// parsed from the controller-reported address — the Service tracks the ready
// pod, so the address stays valid across compute reschedules.
func endpointHostPort(ep controller.Endpoint) (string, int) {
	port := healthEndpointPortFallback
	if _, p, err := net.SplitHostPort(ep.Address); err == nil {
		if n, perr := strconv.Atoi(p); perr == nil {
			port = n
		}
	}
	return endpointDeployName(ep.ID), port
}

// healthWorkloadUUID derives an RFC-4122-shaped UUID for the jar's --uuid
// (java.util.UUID.fromString rejects bare markers), seed-forked per label.
func (h *healthRun) healthWorkloadUUID(label string) string {
	rnd := h.a.f.t.Seed.Fork("health-uuid-" + label).Rand()
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		rnd.Uint32(), uint16(rnd.Uint32()), uint16(rnd.Uint32()), uint16(rnd.Uint32()), rnd.Uint64()&0xffffffffffff)
}

// backgroundWorkloads starts the three sample-app workloads against the base
// endpoint's in-cluster address, requires each to actually generate load
// (WaitReady), registers their report-kill teardowns, and leaves a liveness
// monitor per workload running for the run window — a death mid-run is a
// failure (no chaos here; nothing should kill them).
func (h *healthRun) backgroundWorkloads(ctx context.Context) error {
	host, port := endpointHostPort(h.baseEP)
	fcfg := h.a.f.cfg
	for _, w := range []struct {
		workload sampleapp.Workload
		name     string
	}{
		{sampleapp.SqlDataLoadWithDDL, "sql_data_load_with_ddl"},
		{sampleapp.SqlTransactions, "sql_transactions"},
		{sampleapp.SqlBankTransfer, "sql_bank_transfer"},
	} {
		spec := sampleapp.RunSpec{
			JarPath:         h.jarPath,
			Workload:        w.workload,
			Hosts:           []string{host},
			YSQLPort:        port,
			NumTables:       h.cfg.SampleTables,
			NumThreadsRead:  h.cfg.SampleThreadsRead,
			NumThreadsWrite: h.cfg.SampleThreadsWrite,
			RunTime:         h.cfg.RunTime,
			Marker:          w.name + "-" + h.suffix(),
			UUID:            h.healthWorkloadUUID(w.name),
			// The amp endpoint provisions cloud_admin/postgres, not the jar's
			// yugabyte defaults.
			Props: map[string]string{
				"username":                  fcfg.EndpointUser,
				"password":                  fcfg.EndpointPassword,
				"default_postgres_database": fcfg.EndpointDB,
			},
		}
		if w.workload == sampleapp.SqlBankTransfer {
			// amp compute is vanilla PG: the jar's default SPLIT INTO n TABLETS
			// DDL is YB-only syntax — 0 omits the clause.
			spec.Props["bank_number_of_tablets"] = "0"
		}
		app := sampleapp.New(h.clientPod.Exec(), spec,
			sampleapp.WithLogf(h.a.f.logf),
			sampleapp.WithReadyTimeout(h.cfg.ReadyTimeout))
		hw := &healthWorkload{name: w.name, app: app}
		if err := app.Start(ctx); err != nil {
			return fmt.Errorf("%s start: %w", w.name, err)
		}
		h.a.f.register("report-kill workload "+w.name, func(ctx context.Context) error {
			return hw.reportKill(ctx, h.sink)
		})
		if err := app.WaitReady(ctx); err != nil {
			return fmt.Errorf("%s never generated load: %w", w.name, err)
		}
		h.workloads = append(h.workloads, hw)
	}

	h.deadline = time.Now().Add(h.cfg.RunTime)
	for _, hw := range h.workloads {
		hw := hw
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			hw.err = hw.monitor(ctx, h.deadline)
		}()
	}
	return nil
}

// monitor polls the workload process until deadline (minus grace — the jar ends
// itself at --run_time); an exit before that is a mid-run death.
func (w *healthWorkload) monitor(ctx context.Context, deadline time.Time) error {
	for {
		if !time.Now().Before(deadline.Add(-healthMonitorGrace)) {
			return nil
		}
		alive, err := w.app.Running(ctx)
		if err == nil && !alive {
			return fmt.Errorf("workload %s died mid-run (%s before its run window ended)", w.name, time.Until(deadline).Round(time.Second))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// reportKill collect-then-kills the workload exactly once, on a fresh context so
// logs are collected even when the run ctx is gone.
func (w *healthWorkload) reportKill(ctx context.Context, sink attachSink) error {
	if w.killed.Swap(true) {
		return nil
	}
	kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthKillTimeout)
	defer cancel()
	return w.app.ReportKill(kctx, sink)
}

// forkUnderLoad captures the base LSN (the oracle dataset is fully committed
// below it), forks a child branch there, and asserts on the child — under the
// still-running background load — that the oracle verifies clean and a marker
// written on the child does not leak to the base.
func (h *healthRun) forkUnderLoad(ctx context.Context) error {
	cl, err := h.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	forkLSN, err := h.basePG.QueryString(ctx, "SELECT pg_current_wal_lsn()")
	if err != nil {
		return fmt.Errorf("capture fork LSN: %w", err)
	}
	if forkLSN == "" {
		return fmt.Errorf("pg_current_wal_lsn() returned empty")
	}

	childName := "hlth-child-" + h.suffix()
	if _, err := cl.CreateBranchAndWait(ctx, h.a.proj.poolID, h.a.proj.projID, controller.CreateBranchRequest{
		Name:           childName,
		ParentBranchID: h.a.proj.branchID,
		ForkPointLSN:   forkLSN,
	}); err != nil {
		return fmt.Errorf("fork child branch at LSN %s: %w", forkLSN, err)
	}
	child, err := findBranchCtl(ctx, cl, h.a.proj.poolID, h.a.proj.projID, childName)
	if err != nil {
		return err
	}
	h.childBranch = child
	ep, err := createEndpoint(ctx, cl, h.a.proj.poolID, h.a.proj.projID, child.BranchID)
	if err != nil {
		return fmt.Errorf("create child endpoint: %w", err)
	}
	h.childEP = ep
	kid, err := h.healthEndpointPG(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect child endpoint: %w", err)
	}
	h.childPG = kid
	h.a.f.register("close child endpoint connection", func(context.Context) error {
		h.closeChildPG()
		return nil
	})

	// The oracle was committed before the fork LSN → the child must verify clean.
	vctx, vcancel := context.WithTimeout(ctx, h.cfg.VerifyTimeout)
	defer vcancel()
	report, err := workloadgen.Verify(vctx, kid.Pool(), h.oracleSpec(), h.oracleSeed())
	if err != nil {
		return fmt.Errorf("oracle verify on child: %w", err)
	}
	if !report.OK {
		return fmt.Errorf("LSN fork lost or corrupted oracle data on the child (%d table(s) checked)", len(report.Tables))
	}

	// A child-side write must not exist on the base.
	marker := h.markerTable()
	if _, err := kid.Exec(ctx, "CREATE TABLE "+marker+" (id int primary key)"); err != nil {
		return fmt.Errorf("create marker table on child: %w", err)
	}
	if _, err := kid.Exec(ctx, "INSERT INTO "+marker+" (id) VALUES (1)"); err != nil {
		return fmt.Errorf("write marker row on child: %w", err)
	}
	if n, err := h.basePG.QueryInt(ctx, "SELECT count(*) FROM pg_tables WHERE tablename = $1", marker); err != nil {
		return fmt.Errorf("check marker table on base: %w", err)
	} else if n != 0 {
		return fmt.Errorf("branch isolation broken: child marker table %s visible on base", marker)
	}
	return nil
}

func (h *healthRun) closeChildPG() {
	if h.childPG != nil {
		h.childPG.Close()
		h.childPG = nil
	}
}

// endpointLifecycle deletes the child's endpoint under load and recreates it,
// asserting the child-side marker survived the compute retire+redeploy.
func (h *healthRun) endpointLifecycle(ctx context.Context) error {
	cl, err := h.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	h.closeChildPG()
	if _, err := cl.DeleteEndpointAndWait(ctx, h.a.proj.poolID, h.a.proj.projID, h.childBranch.BranchID, h.childEP.ID); err != nil {
		return fmt.Errorf("delete child endpoint: %w", err)
	}
	ep, err := createEndpoint(ctx, cl, h.a.proj.poolID, h.a.proj.projID, h.childBranch.BranchID)
	if err != nil {
		return fmt.Errorf("recreate child endpoint: %w", err)
	}
	h.childEP = ep
	kid, err := h.healthEndpointPG(ctx, ep)
	if err != nil {
		return fmt.Errorf("reconnect recreated child endpoint: %w", err)
	}
	h.childPG = kid
	n, err := kid.QueryInt(ctx, "SELECT count(*) FROM "+h.markerTable())
	if err != nil {
		return fmt.Errorf("read marker after endpoint recreate: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("marker row lost across endpoint delete+recreate (count=%d, want 1)", n)
	}
	return nil
}

// stopWorkloads waits out the remaining run window, collect-then-kills all
// three workloads, and fails on any mid-run death the monitors observed.
func (h *healthRun) stopWorkloads(ctx context.Context) error {
	h.wg.Wait()
	var firstErr error
	for _, hw := range h.workloads {
		if kerr := hw.reportKill(ctx, h.sink); kerr != nil {
			h.a.f.logf("health: report-kill %s (best-effort): %v", hw.name, kerr)
		}
		if hw.err != nil && firstErr == nil {
			firstErr = hw.err
		}
	}
	return firstErr
}

// finalVerify re-verifies the oracle on the base branch over a FRESH (cold)
// connection after all the churn, and asserts the controller is healthy.
func (h *healthRun) finalVerify(ctx context.Context) error {
	h.closeBasePG()
	h.closeChildPG()
	pgc, err := h.healthEndpointPG(ctx, h.baseEP)
	if err != nil {
		return fmt.Errorf("fresh connection to base endpoint: %w", err)
	}
	defer pgc.Close()
	vctx, vcancel := context.WithTimeout(ctx, h.cfg.VerifyTimeout)
	defer vcancel()
	report, err := workloadgen.Verify(vctx, pgc.Pool(), h.oracleSpec(), h.oracleSeed())
	if err != nil {
		return fmt.Errorf("final oracle verify on base: %w", err)
	}
	if !report.OK {
		bad := 0
		for _, tr := range report.Tables {
			if !tr.OK {
				bad++
			}
		}
		return fmt.Errorf("oracle dataset corrupted on base after the run (%d/%d table(s) bad)", bad, len(report.Tables))
	}

	cl, err := h.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	if err := cl.Healthz(ctx); err != nil {
		return fmt.Errorf("amp controller not healthy after the run: %w", err)
	}
	return nil
}

// --- config ---

const (
	DefaultHealthRunTime            = 5 * time.Minute
	DefaultHealthClientImage        = "eclipse-temurin:21-jdk"
	DefaultHealthRemoteDir          = "/opt/ampqa"
	DefaultHealthJarSecret          = "gh_package"
	DefaultHealthSampleTables       = 2
	DefaultHealthSampleThreadsRead  = 1
	DefaultHealthSampleThreadsWrite = 2
	DefaultHealthVerifyTables       = 4
	DefaultHealthVerifyRows         = 500
	DefaultHealthReadyTimeout       = 5 * time.Minute
	DefaultHealthVerifyTimeout      = 5 * time.Minute
)

// healthConfig is the resolved health.* configuration.
type healthConfig struct {
	RunTime            time.Duration
	ClientImage        string
	ClientPullSecret   string
	RemoteDir          string
	JarSecret          string
	SampleTables       int
	SampleThreadsRead  int
	SampleThreadsWrite int
	VerifyTables       int
	VerifyRows         int
	ReadyTimeout       time.Duration
	VerifyTimeout      time.Duration
}

func healthFromConfig(c *core.Config) healthConfig {
	return healthConfig{
		RunTime:            c.GetDuration("health.run_time", DefaultHealthRunTime),
		ClientImage:        c.GetString("health.client_image", DefaultHealthClientImage),
		ClientPullSecret:   c.GetString("health.client_pull_secret", ""),
		RemoteDir:          c.GetString("health.remote_dir", DefaultHealthRemoteDir),
		JarSecret:          c.GetString("health.jar_secret", DefaultHealthJarSecret),
		SampleTables:       c.GetInt("health.sample_tables", DefaultHealthSampleTables),
		SampleThreadsRead:  c.GetInt("health.sample_threads_read", DefaultHealthSampleThreadsRead),
		SampleThreadsWrite: c.GetInt("health.sample_threads_write", DefaultHealthSampleThreadsWrite),
		VerifyTables:       c.GetInt("health.verify_tables", DefaultHealthVerifyTables),
		VerifyRows:         c.GetInt("health.verify_rows", DefaultHealthVerifyRows),
		ReadyTimeout:       c.GetDuration("health.ready_timeout", DefaultHealthReadyTimeout),
		VerifyTimeout:      c.GetDuration("health.verify_timeout", DefaultHealthVerifyTimeout),
	}
}

// healthGate returns a skip reason (and false) unless the scenario may run: a
// k8s cluster for the client pod + control plane, and the PRIVATE sample-app
// jar secret resolvable. Selection is intent — no enabled flag. It never
// resolves a secret VALUE into the reason, only names the missing secret.
func healthGate(c *core.Config, cfg healthConfig) (string, bool) {
	if c.GetString("apps.ybk8s.kubecontext", "") == "" &&
		c.GetString("apps.ybk8s.kubeconfig", "") == "" &&
		c.GetString("env.sharedk8s.context", "") == "" &&
		c.GetString("env.ampstack.context", "") == "" {
		return "product health needs a k8s cluster: set apps.ybk8s.kubecontext/kubeconfig, env.sharedk8s.context or env.ampstack.context", false
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
