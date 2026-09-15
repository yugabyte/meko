// Runs planned maintenance (gflag change, data move, upgrade) on the YBA
// universe backing amp, asserting commits keep flowing through each operation
// and every acknowledged row reads back after. Skips unless the env's backing
// is a YBA universe. backing_nemesis covers the ungraceful case (killing pods).
package stress

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"path"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/benchbase"
	"github.com/yugabyte/amp/test/functional/framework/apps/clientpod"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/sampleapp"
	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybadmin"
	"github.com/yugabyte/amp/test/functional/framework/artifact"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// The chart renders its gflags Secret into tserverConfPath and runs yb-tserver
// with it as --flagfile, so that file is what the process is configured by.
const (
	tserverContainer = "yb-tserver"
	tserverConfPath  = "/tmp/yugabyte/tserver/conf/server.conf"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/backing_maintenance",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, FaultsSharedInfra: true},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamCompute,
		Description: "Put the YugabyteDB under amp through planned maintenance — rolling restart, gflag change, data move, software upgrade — while amp tenants write through their endpoints, then assert commits kept flowing through every operation and no acknowledged row was lost.",
		Params:      []string{"stress.backing_maintenance", "stress.consistency"},
		Timeout:     4 * time.Hour,
		Run:         runBackingMaintenance,
	})
}

func runBackingMaintenance(t *core.T) error {
	cfg := maintenanceFromConfig(t.Options)
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane: the workload writes THROUGH amp")
		return nil
	}
	target, reason, ok := resolveMaintenanceTarget(t, cfg)
	if !ok {
		t.Skip(reason)
		return nil
	}
	if reason, ok := maintenanceGate(t.Options, cfg); !ok {
		t.Skip(reason)
		return nil
	}
	// The gate polices the test's own rf knob; this reads the backing's REAL
	// shape. The default pipeline universe is RF1 (apps.yba.k8s.num_nodes), and
	// taking its only tserver out stops the writes for a legitimate reason —
	// which the commit assertions would report as a dead write path.
	if reason, ok := enoughTServersForMaintenance(t, cfg); !ok {
		t.Skip(reason)
		return nil
	}

	r := &maintenanceRun{
		t:      t,
		cfg:    cfg,
		env:    env,
		target: target,
		logf:   t.Masker().Logf(log.Printf),
	}
	if err := r.resolveUpgradeTarget(t.Context); err != nil {
		return err
	}

	r.sink = attachSink{t: t, logf: r.logf}

	fleet, err := newTenantFleet(t, env, r.logf)
	if err != nil {
		return err
	}
	defer fleet.Close()
	r.fleet = fleet
	defer func() {
		if r.loads != nil {
			r.loads.CollectAll(r.sink)
		}
		if r.pod != nil {
			pctx, cancel := context.WithTimeout(context.Background(), reportKillTimeout)
			if err := r.pod.Stop(pctx, r.sink); err != nil {
				r.logf("stop client pod (best-effort): %v", err)
			}
			cancel()
		}
	}()

	phases := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"create_tenants", r.createTenants},
		{"amp_is_backed_by_this_universe", r.assertAmpBackedByTarget},
		{"baseline_write", r.baselineWrite},
		{"start_endpoint_workloads", r.startEndpointWorkloads},
		{"maintenance_under_load", r.maintenanceUnderLoad},
		{"endpoints_serve_again", r.endpointsServeAgain},
		{"writes_flow_again", r.writesFlowAgain},
		{"verify_data", r.verifyData},
		{"physical_consistency", r.physicalConsistency},
	}
	for _, p := range phases {
		if err := t.Step(p.name, func(*core.StepHandle) error { return p.fn(t.Context) }); err != nil {
			return err
		}
	}
	return nil
}

type maintenanceRun struct {
	t      *core.T
	cfg    maintenanceConfig
	env    ampTenantEnv
	fleet  *tenantFleet
	target maintenanceTarget
	admin  *ybadmin.Client
	logf   func(string, ...any)
	sink   apps.ArtifactSink
	// benchbaseReconnects: the stress fork survives a dropped connection, so
	// TPC-C stopping is a finding only when it is the driver in use.
	benchbaseReconnects bool
	loads               *workloadSet
	pod                 *clientpod.K8s

	fromVersion string
}

// Only a YBA universe qualifies: a built-in yugabyted has no maintenance API.
func resolveMaintenanceTarget(t *core.T, cfg maintenanceConfig) (maintenanceTarget, string, bool) {
	benv, ok := t.Env.(ybaBackedEnv)
	if !ok {
		return nil, "this env exposes no YBA universe behind amp, so there is no maintenance API to drive", false
	}
	client, universeID := benv.YBAUniverse()
	if client == nil {
		return nil, "amp's backing here is not a YBA universe (env.yb_deploy=yba_provisioned gives one), so there is nothing to upgrade or re-flag", false
	}
	target := &ybaTarget{
		client:       client,
		universeID:   universeID,
		restartSleep: cfg.NodeRestartSleep,
		arch:         cfg.UpgradeArch,
		logf:         t.Masker().Logf(log.Printf),
	}
	// data_move needs somewhere in-cluster to run yb-admin.
	if kenv, ok := t.Env.(kubeUniverseEnv); ok {
		kubeContext, kubeconfig, namespace := kenv.KubeUniverse()
		if namespace != "" {
			target.k8s = true
			target.kube = k8s.New(k8s.Config{Context: kubeContext, Namespace: namespace}).
				WithRunner(k8s.KubeconfigRunner(kubeconfig))
		}
	}
	return target, "", true
}

// enoughTServersForMaintenance sizes the backing: rolling a tserver out is only
// survivable when the ones left can still form a write quorum.
func enoughTServersForMaintenance(t *core.T, cfg maintenanceConfig) (string, bool) {
	yt, ok := t.Env.(ybaBackedEnv)
	if !ok {
		return "", true
	}
	client, universeID := yt.YBAUniverse()
	if client == nil {
		return "", true
	}
	u, err := client.GetUniverse(t.Context, universeID)
	if err != nil {
		return fmt.Sprintf("cannot read the backing universe %s to check its shape: %v", universeID, err), false
	}
	return enoughTServers(u, cfg.RF)
}

func enoughTServers(u yba.Universe, need int) (string, bool) {
	tservers := 0
	for _, n := range u.UniverseDetails.NodeDetailsSet {
		if n.IsTserver {
			tservers++
		}
	}
	if tservers < need {
		return fmt.Sprintf("the backing has %d tserver(s) and this needs %d to stay writable while one is out: create the universe with apps.yba.k8s.num_nodes=%d and apps.yba.k8s.replication_factor=%d",
			tservers, need, need, need), false
	}
	return "", true
}

// Local so the suite depends on the capability, not the layer that provides it.
type ybaBackedEnv interface {
	YBAUniverse() (universe.YBAMaintainer, string)
}

type kubeUniverseEnv interface {
	KubeUniverse() (kubeContext, kubeconfig, namespace string)
}

// The pairing is fixed at universe creation; this only names the expected train.
func (r *maintenanceRun) resolveUpgradeTarget(ctx context.Context) error {
	v, err := r.target.Version(ctx)
	if err != nil {
		return err
	}
	r.fromVersion = v
	if !r.cfg.wants(OpSoftwareUpgrade) {
		return nil
	}
	if r.cfg.UpgradeTo == "" {
		target, err := r.target.UpgradeTarget(ctx)
		if err != nil {
			return err
		}
		r.cfg.UpgradeTo = target
		r.cfg.UpgradeImageTag = target
		r.logf("upgrade target defaulted to the YBA's own version %s", target)
	}
	if r.fromVersion == "" {
		return fmt.Errorf("the backing reports no YugabyteDB version, so an upgrade to %s cannot be checked", r.cfg.UpgradeTo)
	}
	cmp, err := core.Compare(r.fromVersion, r.cfg.UpgradeTo)
	if err != nil {
		return fmt.Errorf("compare backing version %q with target %q: %w", r.fromVersion, r.cfg.UpgradeTo, err)
	}
	if cmp >= 0 {
		return fmt.Errorf("the backing is already on %s and the upgrade target is %s: create the universe a train behind (the latest %s.x release) so there is an upgrade to make, or drop software_upgrade from stress.backing_maintenance.operations",
			r.fromVersion, r.cfg.UpgradeTo, yba.PreviousReleaseTrain(r.cfg.UpgradeTo))
	}
	r.logf("software upgrade planned: %s -> %s", r.fromVersion, r.cfg.UpgradeTo)
	return nil
}

func (r *maintenanceRun) createTenants(ctx context.Context) error {
	if err := r.fleet.Create(r.t, r.cfg.Tenants); err != nil {
		return err
	}
	r.logf("maintaining %s under %d amp tenants", r.target.Describe(), len(r.fleet.Tenants))
	return nil
}

// Guards against the backing handle pointing at an idle cluster amp doesn't use.
func (r *maintenanceRun) assertAmpBackedByTarget(ctx context.Context) error {
	kube := r.kube()
	if kube == nil {
		r.logf("backing is not a kubernetes universe; cannot query it for amp's storage state")
		return nil
	}
	pod, err := firstTserverPod(ctx, kube)
	if err != nil {
		return err
	}
	const q = `select (select count(*) from amp_registry.clusters)`
	out, err := kube.ForPod(pod, tserverContainer).Run(ctx,
		"ysqlsh -h $(hostname)."+tserverHeadlessOf(pod)+" -U yugabyte -d "+ampStorageDB+" -tAc "+shellQuote(q))
	if err != nil {
		return fmt.Errorf("query %s on the backing universe: %w", ampStorageDB, err)
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("the universe under maintenance has no %s database reachable (exit %d: %s) — amp is not backed by it",
			ampStorageDB, out.ExitCode, strings.TrimSpace(out.Stderr))
	}
	clusters, err := strconv.Atoi(strings.TrimSpace(out.Stdout))
	if err != nil {
		return fmt.Errorf("read amp's cluster count from the universe (%q): %w", strings.TrimSpace(out.Stdout), err)
	}
	if clusters < len(r.fleet.Tenants) {
		return fmt.Errorf("the universe under maintenance holds %d amp clusters but this run created %d tenants: amp is not backed by the universe being maintained",
			clusters, len(r.fleet.Tenants))
	}
	r.logf("confirmed: amp stores into the universe under maintenance (%s holds %d clusters for %d tenants)",
		ampStorageDB, clusters, len(r.fleet.Tenants))
	r.t.Metric("amp_clusters_in_backing_universe", float64(clusters), "count")
	return nil
}

// The StatefulSet is "<prefix>-yb-tserver", its service "<prefix>-yb-tservers".
func tserverHeadlessOf(pod string) string {
	i := strings.LastIndex(pod, "-yb-tserver-")
	if i < 0 {
		return "yb-tservers"
	}
	return pod[:i] + "-yb-tservers"
}

// Confirms the workload works before maintenance, so a zero-commit verdict later is about the maintenance, not a workload that never worked.
func (r *maintenanceRun) baselineWrite(ctx context.Context) error {
	runWriters(ctx, r.env, r.fleet.Tenants, r.cfg.BaselineDuration, r.cfg.PayloadBytes)
	rows := totalWritten(r.fleet.Tenants)
	if rows == 0 {
		return fmt.Errorf("no tenant committed a row through amp before the maintenance started")
	}
	r.t.Metric("baseline_rows", float64(rows), "count")
	return nil
}

func (r *maintenanceRun) startEndpointWorkloads(ctx context.Context) error {
	kenv, ok := r.env.(ampKubeEnv)
	if !ok || kenv.AmpKube() == nil {
		return fmt.Errorf("no kube handle for amp's namespace: the workloads run in a pod beside the endpoints they drive")
	}
	kube := kenv.AmpKube()

	cp := clientpod.NewK8s(kube)
	spec := clientpod.Spec{
		Name:    "ampqa-maint-" + r.t.ResourceSuffix(),
		Image:   r.cfg.ClientImage,
		Command: []string{"sh", "-c", "sleep infinity"},
	}
	if r.cfg.ClientPullSecret != "" {
		spec.ImagePullSecrets = []string{r.cfg.ClientPullSecret}
	}
	if err := cp.Start(ctx, spec); err != nil {
		return fmt.Errorf("start client pod: %w", err)
	}
	r.pod = cp
	podExec := cp.Exec()

	jarPath, benchDir, err := r.deployArtifacts(ctx, podExec)
	if err != nil {
		return err
	}

	addr, ok := r.env.(endpointAddrEnv)
	if !ok {
		return fmt.Errorf("this env cannot name an endpoint's in-cluster address, so a pod cannot dial it")
	}
	creds := r.cfg.EndpointCreds

	r.loads = &workloadSet{logf: r.logf}
	for i := range r.fleet.Tenants {
		ten := &r.fleet.Tenants[i]
		host, port := addr.EndpointInClusterAddr(ten.Endpoint)
		label := fmt.Sprintf("%s-t%d", r.t.ResourceSuffix(), i)
		switch i % 3 {
		case 0:
			r.loads.workloads = append(r.loads.workloads, sampleAppOnEndpoint(podExec, jarPath,
				sampleapp.SqlTransactions, i, host, port, creds, r.cfg,
				"sqltransactions-"+label, r.workloadUUID("txn", i), nil, r.logf))
		case 1:
			r.loads.workloads = append(r.loads.workloads, sampleAppOnEndpoint(podExec, jarPath,
				sampleapp.SqlBankTransfer, i, host, port, creds, r.cfg,
				"sqlbanktransfer-"+label, r.workloadUUID("bank", i),
				bankOnAmpProps(r.cfg.BankAccounts), r.logf))
		default:
			tpcc := benchbaseOnEndpoint(podExec, benchDir, i, host, port, creds, r.cfg, r.logf)
			tpcc.exitIsFinding = r.benchbaseReconnects
			r.loads.workloads = append(r.loads.workloads, tpcc)
		}
	}
	return r.loads.Start(ctx)
}

func (r *maintenanceRun) deployArtifacts(ctx context.Context, podExec exec.RemoteExecutor) (jarPath, benchDir string, err error) {
	specs, err := artifact.Resolve(r.t.Options)
	if err != nil {
		return "", "", fmt.Errorf("resolve artifacts: %w", err)
	}
	secrets, err := core.NewSecrets(r.t.Options)
	if err != nil {
		return "", "", fmt.Errorf("secrets: %w", err)
	}
	jarSecret, err := secrets.Value(r.cfg.JarSecret)
	if err != nil {
		return "", "", fmt.Errorf("resolve sample-app jar secret %q: %w", r.cfg.JarSecret, err)
	}
	sa, ok := specs["sampleapp"]
	if !ok {
		return "", "", fmt.Errorf("artifacts.sampleapp is not declared")
	}
	dep := &artifact.Deployer{}
	execs := []exec.RemoteExecutor{podExec}
	if err := dep.Deploy(ctx, execs, sa, artifact.DeployOptions{RemoteDir: r.cfg.RemoteDir, Secret: jarSecret}); err != nil {
		return "", "", fmt.Errorf("deploy the sample-app jar: %w", err)
	}
	// Prefer the stress fork: its workers reconnect where the public build's die on the dropped connections a rolling operation causes.
	bbName := "benchbase_stress"
	bb, declared := specs[bbName]
	if declared {
		if _, serr := secrets.Value(bb.Secret); bb.Secret != "" && serr != nil {
			r.logf("benchbase fork secret %q not resolvable (%v); falling back to the public build, whose workers do not reconnect", bb.Secret, serr)
			declared = false
		}
	}
	if !declared {
		bbName = "benchbase"
		bb, declared = specs[bbName]
	}
	if !declared {
		return "", "", fmt.Errorf("neither artifacts.benchbase_stress nor artifacts.benchbase is declared")
	}
	bbOpts := artifact.DeployOptions{RemoteDir: r.cfg.RemoteDir}
	if bb.Secret != "" {
		value, serr := secrets.Value(bb.Secret)
		if serr != nil {
			return "", "", fmt.Errorf("resolve the %s secret: %w", bbName, serr)
		}
		bbOpts.Secret = value
	}
	if err := dep.Deploy(ctx, execs, bb, bbOpts); err != nil {
		return "", "", fmt.Errorf("deploy %s: %w", bbName, err)
	}
	r.benchbaseReconnects = bbName == "benchbase_stress"
	return path.Join(r.cfg.RemoteDir, sa.GlobPath), path.Join(r.cfg.RemoteDir, benchbase.DefaultSubdir), nil
}

// Names an endpoint as a pod reaches it, not via the coordinator's port-forward.
type endpointAddrEnv interface {
	EndpointInClusterAddr(ep controller.Endpoint) (string, int)
}

// The jar demands an RFC-4122-shaped --uuid; seed-forked so a replay repeats it.
func (r *maintenanceRun) workloadUUID(label string, i int) string {
	rnd := r.t.Seed.Fork(fmt.Sprintf("uuid-%s-%d", label, i)).Rand()
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		rnd.Uint32(), uint16(rnd.Uint32()), uint16(rnd.Uint32()), uint16(rnd.Uint32()), rnd.Uint64()&0xffffffffffff)
}

func (r *maintenanceRun) maintenanceUnderLoad(ctx context.Context) error {
	loadCtx, stopLoad := context.WithCancel(ctx)
	defer stopLoad()
	done := startTenantWriters(loadCtx, r.env, r.fleet.Tenants, r.cfg.PayloadBytes)

	plan := &maintenancePlan{
		ops:         r.buildOps(),
		progress:    func() int64 { return totalWritten(r.fleet.Tenants) },
		minProgress: int64(r.cfg.MinCommitsPerOp),
		observe: func(op string, commits int64) {
			r.t.Metric("commits_during_"+op, float64(commits), "count")
		},
		after: func(ctx context.Context, op string) error {
			if r.loads == nil {
				return nil
			}
			return r.loads.Check(ctx, op)
		},
		step: func(name string, fn func() error) error {
			return r.t.Step(name, func(*core.StepHandle) error { return fn() })
		},
		logf: r.logf,
	}
	before := totalWritten(r.fleet.Tenants)
	err := plan.run(ctx)

	stopLoad()
	<-done
	// Delta, not total: totalWritten also carries the baseline phase.
	r.t.Metric("commits_during_maintenance", float64(totalWritten(r.fleet.Tenants)-before), "count")
	return err
}

func (r *maintenanceRun) buildOps() []maintenanceOp {
	ops := make([]maintenanceOp, 0, len(r.cfg.Operations))
	for _, name := range r.cfg.Operations {
		switch name {
		case OpRollingRestart:
			ops = append(ops, maintenanceOp{name, r.rollingRestart})
		case OpGflagChange:
			ops = append(ops, maintenanceOp{name, r.gflagChange})
		case OpDataMove:
			ops = append(ops, maintenanceOp{name, r.dataMove})
		case OpSoftwareUpgrade:
			ops = append(ops, maintenanceOp{name, r.softwareUpgrade})
		}
	}
	return ops
}

func (r *maintenanceRun) rollingRestart(ctx context.Context) error {
	if err := r.runRolling(ctx, OpRollingRestart, func() error {
		return r.target.RollingRestart(ctx)
	}); err != nil {
		return err
	}
	return r.target.Health(ctx)
}

// YBA REJECTS a gflag upgrade that changes nothing, so the configured value
// must differ from what the universe already carries.
func (r *maintenanceRun) gflagChange(ctx context.Context) error {
	if err := r.runRolling(ctx, OpGflagChange, func() error {
		return r.target.SetTServerGflag(ctx, r.cfg.GflagKey, r.cfg.GflagValue)
	}); err != nil {
		return err
	}
	return r.target.Health(ctx)
}

// Asserts op stayed rolling; the task succeeding alone proves nothing.
func (r *maintenanceRun) runRolling(ctx context.Context, name string, fn func() error) error {
	obs, err := watchRolling(ctx, r.kube(), r.cfg.RollingSampleInterval, fn)
	if err != nil {
		return err
	}
	if !obs.Complete() {
		r.logf("%s: rolling behaviour not observed (%v); the operation itself succeeded", name, obs.Err)
		return nil
	}
	r.logf("%s was rolling — %s", name, obs)
	r.t.Metric(name+"_max_nodes_out", float64(obs.MaxOut), "count")
	r.t.Metric(name+"_min_nodes_ready", float64(obs.MinReady), "count")
	if obs.MaxOut > r.cfg.MaxNodesOutAtOnce {
		return fmt.Errorf("%s took %d of %d serving nodes down at once (allowed %d): that is not a rolling operation — %s",
			name, obs.MaxOut, obs.Expected, r.cfg.MaxNodesOutAtOnce, obs)
	}
	return nil
}

func (r *maintenanceRun) kube() *k8s.Kubectl {
	if t, ok := r.target.(*ybaTarget); ok {
		return t.kube
	}
	return nil
}

func (r *maintenanceRun) dataMove(ctx context.Context) error {
	mover, ok := r.target.(dataMover)
	if !ok || !mover.SupportsDataMove() {
		return fmt.Errorf("data_move cannot run against %s (nowhere in-cluster to run yb-admin): drop it from stress.backing_maintenance.operations", r.target.Describe())
	}
	if r.admin == nil {
		admin, err := mover.NewAdmin(ctx, r.logf)
		if err != nil {
			return err
		}
		r.admin = admin
	}
	// Seed-derived so a replay drains the same node; modulo since int(uint32) can go negative.
	pick := func(n int) int { return int(r.t.Seed.Fork("data-move").Rand().Uint32() % uint32(n)) }
	if err := mover.DataMove(ctx, r.admin, r.cfg.RF, pick, r.cfg.MoveTimeout, r.logf); err != nil {
		return err
	}
	return r.target.Health(ctx)
}

// The API reporting success is not the same as the nodes carrying the version.
func (r *maintenanceRun) softwareUpgrade(ctx context.Context) error {
	if err := r.runRolling(ctx, OpSoftwareUpgrade, func() error {
		return r.target.UpgradeSoftware(ctx, r.cfg.UpgradeTo)
	}); err != nil {
		return err
	}
	if err := r.target.Health(ctx); err != nil {
		return err
	}
	got, err := r.target.Version(ctx)
	if err != nil {
		return err
	}
	if !strings.Contains(got, r.cfg.UpgradeImageTag) {
		return fmt.Errorf("the backing reports version %q after upgrading to %s", got, r.cfg.UpgradeImageTag)
	}
	r.logf("software upgrade done: %s -> %s", r.fromVersion, got)
	return nil
}

func (r *maintenanceRun) endpointsServeAgain(ctx context.Context) error {
	return assertEndpointsServe(r.t, r.env, r.fleet.Tenants, r.cfg.SettleTimeout)
}

// Probes for COMMIT working again: reads alone still answer from the pageserver (cf. meko#727).
// Waits first: a lapsed write token restarts Postgres, outlasting the window.
func (r *maintenanceRun) writesFlowAgain(ctx context.Context) error {
	waited, err := awaitWritePath(ctx, r.env, r.fleet.Tenants, nil, r.cfg.SettleTimeout, r.cfg.PayloadBytes)
	if err != nil {
		return fmt.Errorf("the write path never came back after the maintenance finished: %w", err)
	}
	r.t.Metric("write_path_back_after_s", waited.Seconds(), "s")
	before := totalWritten(r.fleet.Tenants)
	runWriters(ctx, r.env, r.fleet.Tenants, r.cfg.ResumeDuration, r.cfg.PayloadBytes)
	delta := totalWritten(r.fleet.Tenants) - before
	r.t.Metric("rows_after_maintenance", float64(delta), "count")
	if delta == 0 {
		return fmt.Errorf("every tenant committed a probe row, then none committed anything through amp in the following %s. The write path stalled again", r.cfg.ResumeDuration)
	}
	return nil
}

func (r *maintenanceRun) verifyData(ctx context.Context) error {
	return r.t.Step("verify_tenant_rows", func(s *core.StepHandle) error {
		return verifyTenantData(r.t, s, r.env, r.fleet.Tenants, r.cfg.PayloadBytes)
	})
}

func (r *maintenanceRun) physicalConsistency(ctx context.Context) error {
	return r.t.Step("amcheck", func(s *core.StepHandle) error {
		return verifyPhysicalConsistency(r.t, s, r.env, r.fleet.Tenants)
	})
}
