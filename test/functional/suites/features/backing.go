package features

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/workloadgen"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
	"github.com/yugabyte/amp/test/functional/framework/env/registry"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
)

const pgReadyTimeout = 5 * time.Minute

func init() {
	core.Register(core.Test{
		Name:        "features/amp_on_yugabyted_aws",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe},
		OwningTeam:  core.TeamControlPlane,
		Description: "End-to-end amp-on-backing: awsvm host → yugabyted cluster → layer-1 + layer-2 workloads → teardown.",
		Params:      []string{"functional", "env.ampstack", "env.vm", "env.awsvm", "apps.cloud.aws"},
		Run:         runAmpOnYugabytedAWS,
	})
	core.Register(core.Test{
		Name:        "features/amp_on_raw_yugabyte_aws",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe},
		OwningTeam:  core.TeamControlPlane,
		Description: "End-to-end amp-on-backing: awsvm host → raw master + tserver cluster → layer-1 + layer-2 workloads → teardown.",
		Params:      []string{"functional", "env.ampstack", "env.vm", "env.awsvm", "apps.cloud.aws"},
		Run:         runAmpOnRawYugabyteAWS,
	})
	core.Register(core.Test{
		Name:        "features/amp_on_yba_universe",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe},
		OwningTeam:  core.TeamControlPlane,
		Description: "End-to-end amp-on-backing: attach or create a YBA universe → layer-1 + layer-2 workloads → teardown.",
		Params:      []string{"functional", "env.ampstack", "env.universe", "apps.yba"},
		Run:         runAmpOnYBAUniverse,
	})
	core.Register(core.Test{
		Name:        "features/amp_on_yba_provisioned",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe},
		OwningTeam:  core.TeamControlPlane,
		Description: "End-to-end amp-on-backing: stand up an owned YBA in k8s → create a provider + universe → layer-1 + layer-2 → teardown.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba"},
		Run:         runAmpOnYBAProvisioned,
	})
	core.Register(core.Test{
		Name:        "features/amp_on_yb_k8s",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe},
		Tags:        []string{"nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "End-to-end amp-on-backing: deploy YugabyteDB in k8s co-located with amp → layer-1 + layer-2 workloads → teardown.",
		Params:      []string{"functional", "env.ampstack", "apps.ybk8s"},
		Run:         runAmpOnYBK8s,
	})
}

// runAmpOnYugabytedAWS: awsvm host → yugabyted cluster → layers 1+2 → teardown.
func runAmpOnYugabytedAWS(t *core.T) error {
	cfg := FromConfig(t.Options)
	if reason, ok := cfg.gateAWS(t.Options); !ok {
		t.Skip(reason)
		return nil
	}
	return runFoundation(t, cfg, cfg.provisionYugabytedOnAWS)
}

// runAmpOnRawYugabyteAWS: awsvm host → raw master+tserver cluster → layers 1+2
// → teardown.
func runAmpOnRawYugabyteAWS(t *core.T) error {
	cfg := FromConfig(t.Options)
	if reason, ok := cfg.gateAWS(t.Options); !ok {
		t.Skip(reason)
		return nil
	}
	return runFoundation(t, cfg, cfg.provisionRawYugabyteOnAWS)
}

// runAmpOnYBAUniverse: attach/create a YBA universe → layers 1+2 → teardown
// (delete only if owned).
func runAmpOnYBAUniverse(t *core.T) error {
	cfg := FromConfig(t.Options)
	if reason, ok := cfg.gateYBA(t.Options); !ok {
		t.Skip(reason)
		return nil
	}
	return runFoundation(t, cfg, cfg.provisionYBAUniverse)
}

// runAmpOnYBAProvisioned: stand up an OWNED YBA in k8s → create a provider +
// universe on it → layers 1+2 → teardown (delete universe + tear the YBA down).
// The whole deploy→provider→universe flow (and its guaranteed, partial-safe
// cleanup) lives in env/universe's provision_yba mode; this provisioner just
// drives it and adapts the resulting endpoint into a backing store.
func runAmpOnYBAProvisioned(t *core.T) error {
	cfg := FromConfig(t.Options)
	if reason, ok := cfg.gateYBAProvisioned(t.Options); !ok {
		t.Skip(reason)
		return nil
	}
	return runFoundation(t, cfg, cfg.provisionYBAProvisioned)
}

// runAmpOnYBK8s: deploy a YugabyteDB cluster IN k8s and deploy amp into the
// SAME cluster pointed at the in-cluster YB Service DNS (co-location) → layers
// 1+2 → teardown (namespace + PVCs). The cheapest fully-GCP co-located mode:
// amp↔YB traffic never leaves the cluster; no cloud VM creds needed.
func runAmpOnYBK8s(t *core.T) error {
	cfg := FromConfig(t.Options)
	if reason, ok := cfg.gateYBK8s(t.Options); !ok {
		t.Skip(reason)
		return nil
	}
	return runFoundation(t, cfg, cfg.provisionYBK8s)
}

// backingProvisioner stands up the backing YugabyteDB cluster, registers its
// teardown (cluster destroy + host terminate / universe delete) on td, and
// returns the resolved backing store plus a reap.Plan — the serializable
// ownership handles a separate-process `ampqa env rm` needs to reap this
// backing's real infrastructure without the in-memory teardown closure. A `run`
// discards the plan (it tears down in-process); `ampqa setup` records it. The
// shapes differ only here.
type backingProvisioner func(ctx context.Context, t *core.T, td *teardownStack, logf func(string, ...any)) (ybbacking.BackingStore, *reap.Plan, error)

// realInfraProvisionModes are env.universe.mode values under which RunOne's env
// layer (env/universe.Provider, selected by cmd/ampqa/run.go from this same
// config) itself stands up brand-new real infrastructure — as opposed to
// attach_yba/attach_address, which only point at something already running.
var realInfraProvisionModes = map[string]bool{
	universe.ModeProvisionYugabyted: true,
	universe.ModeProvisionYBA:       true,
}

// modeReporter is implemented by env providers that expose their resolved
// provisioning mode — currently only env/universe.Env. Declared locally (not
// as env.Environment itself) so gateNoDoubleProvision stays decoupled from the
// concrete universe package and unit-testable with a bare fake.
type modeReporter interface{ Mode() string }

// gateNoDoubleProvision skips a foundation test when the env RunOne already
// resolved is itself a real-infra universe.Env in a provisioning mode. Every
// foundation test self-provisions its OWN backing in its body (package doc);
// if RunOne's env layer ALSO provisioned real infra under one of
// realInfraProvisionModes, running the body would provision a SECOND real
// backing on top of the first — the double-provision (and double-spend)
// footgun this guard exists to catch. The happy path is untouched: --fake-env
// resolves the fake env (no Mode method at all), and an attach_yba /
// attach_address env reports a mode outside realInfraProvisionModes.
func gateNoDoubleProvision(env core.Environment) (string, bool) {
	mr, ok := env.(modeReporter)
	if !ok {
		return "", true
	}
	mode := mr.Mode()
	if !realInfraProvisionModes[mode] {
		return "", true
	}
	return fmt.Sprintf("foundation tests self-provision their backing; run with --fake-env to avoid double-provisioning — env.universe.mode=%s would double-provision", mode), false
}

// runFoundation encodes the whole 6-step foundation lifecycle generically. It
// owns the guaranteed-cleanup discipline: a LIFO teardown stack that runs every
// registered cleanup even when a middle phase fails, on an independent deadline,
// surfacing the first real error.
func runFoundation(t *core.T, cfg Config, prov backingProvisioner) error {
	if reason, ok := gateNoDoubleProvision(t.Env); !ok {
		t.Skip(reason)
		return nil
	}
	f := &flow{t: t, cfg: cfg, prov: prov, td: &teardownStack{}, logf: runFlowLogf(t.Masker(), log.Printf)}
	// A run tears its own infra down in-process on the happy path, but a killed
	// (SIGKILL) or hung run never reaches that teardown. Recording the same
	// reap.Plan `ampqa setup` records makes such a run recoverable via `ampqa env
	// ls` / `ampqa env reap`; a clean teardown removes the record below.
	f.reg = openRunReapRegistry(f.logf)
	step := func(name string, fn func() error) error {
		return t.Step(name, func(*core.StepHandle) error { return fn() })
	}
	phases := []phase{
		{"provision_backing", f.provisionBacking},
		{"workload_yb_native", f.workloadNative},
		{"deploy_amp", f.deployAmp},
		{"workload_amp_compute", f.workloadAmp},
		{"cross_layer_sanity", f.crossLayer},
	}
	return runLifecycle(t.Context, step, f.td, phases, f.logf, f.collectDiagnostics, f.onTeardownDone)
}

// --- guaranteed-cleanup engine (unit-tested in foundation_test.go) ---

// stepRunner runs a named phase, surfacing the portal/stdout timeline. The real
// flow wraps t.Step; unit tests pass a trivial runner.
type stepRunner func(name string, fn func() error) error

// phase is one named lifecycle step.
type phase struct {
	name string
	run  func(context.Context) error
}

// teardownStack is a LIFO stack of best-effort cleanups the lifecycle registers
// as it provisions. run() executes EVERY cleanup (most recent first) even when
// earlier ones fail, and returns the first error — nothing is left running
// because one teardown step erred.
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
				logf("foundation teardown: %s (best-effort): %v", c.name, err)
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", c.name, err)
			}
		}
	}
	return firstErr
}

// runLifecycle runs phases in order under step, then runs td LIFO regardless of
// outcome, on its own background deadline (AGENTS.md §5). It returns the first
// phase error; if the phases all passed but teardown erred, it surfaces the
// teardown error instead. onBeforeTeardown (nil-safe) runs FIRST — before any
// teardown cleanup deletes evidence — with the phase error, on its own budget so
// it can never starve the teardown that follows (§5 "collect before killing").
// onTeardown (nil-safe) is invoked with the teardown's own error AFTER it runs —
// the caller uses it to drop a crash-recovery record only when teardown
// positively succeeded (tdErr == nil).
func runLifecycle(ctx context.Context, step stepRunner, td *teardownStack, phases []phase, logf func(string, ...any), onBeforeTeardown, onTeardown func(context.Context, error)) (err error) {
	defer func() {
		if onBeforeTeardown != nil {
			dctx, dcancel := context.WithTimeout(context.Background(), teardownBudget)
			onBeforeTeardown(dctx, err)
			dcancel()
		}
		tctx, cancel := context.WithTimeout(context.Background(), teardownBudget)
		defer cancel()
		tdErr := td.run(tctx, logf)
		if tdErr != nil && err == nil {
			err = fmt.Errorf("teardown: %w", tdErr)
		}
		if onTeardown != nil {
			onTeardown(tctx, tdErr)
		}
	}()
	for _, p := range phases {
		p := p
		if perr := step(p.name, func() error { return p.run(ctx) }); perr != nil {
			return perr
		}
	}
	return nil
}

// --- the live flow ---

// flow carries the shared state the phases read/write.
type flow struct {
	t    *core.T
	cfg  Config
	prov backingProvisioner
	td   *teardownStack
	logf func(string, ...any)

	// ampEnv is the standing amp control plane the run attaches to (nil in the
	// self-provision path). When set, the flow sources its controller / pool /
	// endpoint-connect / namespace-kube from it instead of standing up its own
	// backing + amp — see runAmpFeature's attach-vs-self-provision decision.
	ampEnv ampControlPlane

	store  ybbacking.BackingStore
	native NativeReport
	reap   *reap.Plan // reap handles the backing provisioner recorded (StandUp reads it)

	// reg + regID are the crash-recovery net for a `run` (nil under StandUp, which
	// records its own plan in cmd/ampqa/setup). A non-empty backing plan is
	// recorded into reg after provisioning and removed after a clean teardown.
	reg   *registry.Registry
	regID string

	ampApp *ampstack.App
}

// runFlowLogf is the one place a foundation flow's logger is built (§7): it
// wraps base in the run's secret masker so a resolved credential (DSN password,
// token, kubeconfig contents) the lifecycle or a provisioner logs through
// f.logf is scrubbed before it reaches stdout/CI logs. Every entrypoint routes
// through here so masking can never be forgotten at one call site.
func runFlowLogf(m *core.Masker, base func(string, ...any)) func(string, ...any) {
	return m.Logf(base)
}

func (f *flow) register(name string, fn func(context.Context) error) { f.td.push(name, fn) }

// suffix is the per-invocation unique token every resource name embeds
// (seed + nonce; see core.T.ResourceSuffix).
func (f *flow) suffix() string { return f.t.ResourceSuffix() }

// sqlPrefix is the SQL-identifier-safe name prefix for databases/tables.
func (f *flow) sqlPrefix() string { return "fnd_" + f.suffix() }

// namespace is the DNS-safe amp namespace (labelled ampqa-managed by ampstack).
func (f *flow) namespace() string { return "ampqa-fnd-" + f.suffix() }

func (f *flow) provisionBacking(ctx context.Context) error {
	store, plan, err := f.prov(ctx, f.t, f.td, f.logf)
	if err != nil {
		return err
	}
	f.store = store
	f.reap = plan
	// Record the crash-recovery reap plan NOW — before the long workload phases —
	// so a run killed after provisioning is recoverable. No-op under StandUp
	// (f.reg is nil) and for attach-mode runs (empty backing plan).
	f.recordReapPlan()
	return nil
}

// recordReapPlan writes this run's reap plan into the standing registry so a
// killed/timed-out run's REAL infrastructure is recoverable by `ampqa env ls` /
// `ampqa env reap`. It augments the backing plan with the (deterministic) amp
// control-plane namespace deployAmp will create, so a run killed during/after
// deploy_amp still has amp reaped too — reaping a not-yet-created namespace is a
// harmless no-op.
func (f *flow) recordReapPlan() {
	f.regID = recordRunReapPlan(f.reg, f.reap, ampstack.FromConfig(f.t.Options).Context, f.namespace(), f.suffix(), f.logf)
}

// onTeardownDone drops the crash-recovery record ONLY when teardown positively
// succeeded; on a teardown error the surviving infrastructure must stay
// discoverable (the record is the leak net, not a cleanliness cosmetic).
func (f *flow) onTeardownDone(_ context.Context, tdErr error) {
	if tdErr == nil {
		f.clearReapRecord()
	}
}

func (f *flow) clearReapRecord() {
	if f.reg == nil || f.regID == "" {
		return
	}
	if _, err := f.reg.Remove(f.regID); err != nil && f.logf != nil {
		f.logf("foundation: remove crash-recovery reap record %s (best-effort): %v", f.regID, err)
	}
}

// recordRunReapPlan records backing (augmented with the amp namespace) into reg
// and returns the entry id, or "" when nothing was recorded. Only a
// self-provisioned real-infra plan (non-empty) earns a record; an attach-mode
// run (empty plan) records nothing. Best-effort: a registry write failure is
// logged and yields "" rather than failing the run.
func recordRunReapPlan(reg *registry.Registry, backing *reap.Plan, ampContext, ampNamespace, suffix string, logf func(string, ...any)) string {
	if reg == nil || backing.Empty() {
		return ""
	}
	plan := withAmpNamespaceReap(backing, ampContext, ampNamespace)
	planJSON, err := plan.JSON()
	if err != nil || planJSON == "" {
		return ""
	}
	entry, err := reg.Add(registry.Entry{
		Name:     "run-" + suffix,
		Shape:    "functional-run",
		Provider: "functional",
		Data:     map[string]string{"reap_plan": planJSON},
	})
	if err != nil {
		if logf != nil {
			logf("foundation: record crash-recovery reap plan (best-effort): %v", err)
		}
		return ""
	}
	return entry.ID
}

// openRunReapRegistry opens the standing registry at its default path for the
// run crash-recovery net. A resolution failure disables recording (best-effort)
// rather than failing the run.
func openRunReapRegistry(logf func(string, ...any)) *registry.Registry {
	path, err := registry.DefaultPath()
	if err != nil {
		if logf != nil {
			logf("foundation: reap registry unavailable, crash-recovery record disabled (best-effort): %v", err)
		}
		return nil
	}
	return registry.New(path)
}

// waitBackingReady blocks until the backing cluster's YSQL accepts connections
// on the exact address deployAmp will use. The run lifecycle gets this
// implicitly — workloadNative connects (and WaitReady-s) before deploy_amp — but
// the setup path (StandUp) goes straight from provision to deployAmp, so it must
// gate here or amp's storage-DB create races a freshly-provisioned tserver whose
// YSQL endpoint is not yet accepting connections (observed: connection refused
// on the tailnet pod IP moments after the StatefulSet reports Ready).
func (f *flow) waitBackingReady(ctx context.Context) error {
	if err := pg.WaitReady(ctx, f.store.ConnInfo(), pgReadyTimeout); err != nil {
		return fmt.Errorf("backing YSQL not ready: %w", err)
	}
	return nil
}

// workloadNative runs the layer-1 YB-native workload against the backing
// cluster: several databases, tablet-split DDL, DDL/DML variations, and a
// workloadgen load + checksum verify.
func (f *flow) workloadNative(ctx context.Context) error {
	base := f.store.ConnInfo()
	admin, err := connectPG(ctx, base)
	if err != nil {
		return fmt.Errorf("connect backing maintenance db: %w", err)
	}
	defer admin.Close()

	spec := NativeSpec{
		Prefix:       f.sqlPrefix(),
		NumDBs:       f.cfg.NumDBs,
		TablesPerDB:  f.cfg.TablesPerDB,
		RowsPerTable: f.cfg.RowsPerTable,
		Tablets:      f.cfg.Tablets,
		Seed:         f.t.Seed.Fork("native").Value(),
		Concurrency:  f.cfg.Concurrency,
	}
	// The DROP DATABASE cleanups RunNative registers run at final teardown,
	// after admin's pool above is closed by this function's defer — they must
	// reconnect fresh rather than reuse admin (AGENTS.md: teardown discipline).
	reconnectAdmin := func(ctx context.Context) (Querier, func(), error) {
		cl, err := connectPG(ctx, base)
		if err != nil {
			return nil, nil, fmt.Errorf("reconnect backing maintenance db: %w", err)
		}
		return cl.Pool(), cl.Close, nil
	}
	report, err := RunNative(ctx, admin.Pool(), f.connector(base), reconnectAdmin, f.register, spec, f.logf)
	if err != nil {
		return err
	}
	if !report.OK {
		return fmt.Errorf("layer-1 workloadgen verify failed on %d database(s)", countBadDBs(report))
	}
	f.native = report
	return nil
}

// connector opens a Querier scoped to a named database on the backing cluster.
func (f *flow) connector(base pg.ConnInfo) Connector {
	return func(ctx context.Context, db string) (Querier, func(), error) {
		ci := base
		ci.Database = db
		cl, err := connectPG(ctx, ci)
		if err != nil {
			return nil, nil, err
		}
		return cl.Pool(), cl.Close, nil
	}
}

// deployAmp stands up the amp control plane on the backing cluster (ampstack
// external mode) and registers its teardown.
func (f *flow) deployAmp(ctx context.Context) error {
	acfg := ampstack.FromConfig(f.t.Options)
	acfg.Namespace = f.namespace()
	acfg.Backing = ybbacking.ExternalBacking(f.store)
	acfg.ExtraNamespaceLabels = map[string]string{"ampqa-foundation": f.suffix()}

	app := ampstack.New(acfg)
	f.ampApp = app
	// Registered before Deploy returns is unnecessary (nothing to tear down until
	// Deploy created the namespace); register both cleanups on success.
	if _, err := app.Deploy(ctx); err != nil {
		// Deploy leaves what it created in place; ensure it is reaped.
		f.register("close amp port-forward", func(ctx context.Context) error { return app.Close(ctx) })
		f.register("teardown amp namespace", func(ctx context.Context) error { return app.Teardown(ctx) })
		return fmt.Errorf("deploy amp: %w", err)
	}
	f.register("close amp port-forward", func(ctx context.Context) error { return app.Close(ctx) })
	f.register("teardown amp namespace", func(ctx context.Context) error { return app.Teardown(ctx) })
	return nil
}

// controllerClient returns a live amp controller client. In tailnet mode (the
// default; see apps/ampstack.ControllerReachMode) this is a stable direct URL
// with nothing to reconnect. In portforward mode (the fallback) it reconnects
// the held kubectl port-forward first if it died since the last call — kubectl
// port-forward proved known-flaky here, and workload_yb_native runs a long
// time before workload_amp_compute makes its first controller call, giving it
// plenty of opportunity to die silently in between. Either way, callers must
// fetch the client through here rather than caching a URL from deployAmp's
// StackInfo.
func (f *flow) controllerClient(ctx context.Context) (*controller.Client, error) {
	if f.ampEnv != nil {
		cl, err := f.ampEnv.AmpController(ctx)
		if err != nil {
			return nil, fmt.Errorf("amp controller unreachable: %w", err)
		}
		return cl, nil
	}
	cl, _, err := f.ampApp.ControllerClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("amp controller unreachable: %w", err)
	}
	return cl, nil
}

// workloadAmp runs the layer-2 workload through amp's compute: create a project
// + endpoint via the controller, connect to the endpoint's Postgres, and run a
// parallel multi-schema DDL/DML exercise plus a workloadgen load + verify. The
// project delete (cascade) is registered so the endpoint is retired on teardown.
func (f *flow) workloadAmp(ctx context.Context) error {
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	user, err := cl.CreateUser(ctx, controller.CreateUserRequest{IsAdmin: true})
	if err != nil {
		return fmt.Errorf("create amp user: %w", err)
	}
	pool, err := cl.CreateStoragePool(ctx, controller.CreateStoragePoolRequest{
		Name:        "fnd-" + f.suffix(),
		OwnerUserID: user.ID,
	})
	if err != nil {
		return fmt.Errorf("create amp storage pool: %w", err)
	}

	projName := "fnd-" + f.suffix()
	if _, err := cl.CreateProjectAndWait(ctx, pool.ID, controller.CreateProjectRequest{
		Name:      projName,
		PgVersion: f.cfg.PgVersion,
	}); err != nil {
		return fmt.Errorf("create amp project: %w", err)
	}
	proj, err := findProject(ctx, cl, pool.ID, projName)
	if err != nil {
		return err
	}
	f.register("delete amp project", func(ctx context.Context) error {
		_, e := cl.DeleteProjectAndWait(ctx, pool.ID, proj.ID)
		return e
	})

	branchID := proj.DefaultBranch.BranchID
	if _, err := cl.CreateEndpointAndWait(ctx, pool.ID, proj.ID, branchID, controller.CreateEndpointRequest{Role: "read_write"}); err != nil {
		return fmt.Errorf("create amp endpoint: %w", err)
	}
	eps, err := cl.ListAllEndpoints(ctx, pool.ID, proj.ID, branchID)
	if err != nil {
		return fmt.Errorf("list amp endpoints: %w", err)
	}
	if len(eps) == 0 {
		return fmt.Errorf("amp endpoint create returned no endpoint")
	}
	ci, err := f.endpointConnInfo(eps[0])
	if err != nil {
		return err
	}
	client, err := connectPG(ctx, ci)
	if err != nil {
		return fmt.Errorf("connect amp endpoint %s: %w", eps[0].Address, err)
	}
	defer client.Close()

	report, err := RunAmpCompute(ctx, client.Pool(), AmpSpec{
		Prefix:       f.sqlPrefix(),
		NumSchemas:   f.cfg.NumDBs,
		TablesPerSet: f.cfg.TablesPerDB,
		RowsPerTable: f.cfg.RowsPerTable,
		Seed:         f.t.Seed.Fork("amp").Value(),
		Concurrency:  f.cfg.Concurrency,
	})
	if err != nil {
		return err
	}
	if !report.OK {
		return fmt.Errorf("layer-2 workloadgen verify failed against amp compute")
	}
	return nil
}

// crossLayer asserts both layers are healthy and the amp deploy actually used
// the external cluster: the controller answers /healthz, amp's storage database
// exists on the backing cluster, and the backing cluster is still serving its
// own layer-1 data.
func (f *flow) crossLayer(ctx context.Context) error {
	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	if err := cl.Healthz(ctx); err != nil {
		return fmt.Errorf("amp controller not healthy: %w", err)
	}

	base := f.store.ConnInfo()
	admin, err := connectPG(ctx, base)
	if err != nil {
		return fmt.Errorf("connect backing to verify amp storage: %w", err)
	}
	defer admin.Close()

	storageDB := f.t.Options.GetString("env.ampstack.db_name", ampstack.DefaultDBName)
	n, err := admin.QueryInt(ctx, "SELECT count(*) FROM pg_database WHERE datname=$1", storageDB)
	if err != nil {
		return fmt.Errorf("check amp storage db on backing: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("amp storage database %q not found on backing cluster (deploy did not use the external store)", storageDB)
	}

	if len(f.native.DBs) == 0 {
		return fmt.Errorf("no layer-1 databases to re-verify")
	}
	db := f.native.DBs[0]
	q, closeFn, err := f.connector(base)(ctx, db.DBName)
	if err != nil {
		return fmt.Errorf("reconnect layer-1 db %s: %w", db.DBName, err)
	}
	defer closeFn()
	report, err := workloadgen.Verify(ctx, q, db.Spec, f.t.Seed.Fork("native").Value())
	if err != nil {
		return fmt.Errorf("re-verify layer-1 db %s: %w", db.DBName, err)
	}
	if !report.OK {
		return fmt.Errorf("backing cluster no longer serving its layer-1 data in db %s", db.DBName)
	}
	return nil
}

// endpointConnInfo builds a pg.ConnInfo for an amp endpoint address ("host" or
// "host:port"), applying the configured endpoint user/database/sslmode.
func (f *flow) endpointConnInfo(ep controller.Endpoint) (pg.ConnInfo, error) {
	addr := strings.TrimSpace(ep.Address)
	if addr == "" {
		return pg.ConnInfo{}, fmt.Errorf("amp endpoint has no address")
	}
	host, port := addr, 5432
	if h, p, err := net.SplitHostPort(addr); err == nil {
		host = h
		if n, perr := strconv.Atoi(p); perr == nil {
			port = n
		}
	}
	return pg.ConnInfo{
		Host:     host,
		Port:     port,
		User:     f.cfg.EndpointUser,
		Password: f.cfg.EndpointPassword,
		Database: f.cfg.EndpointDB,
		SSLMode:  f.cfg.EndpointSSLMode,
	}, nil
}

// --- helpers ---

// connectPG waits for YSQL readiness then opens a client.
func connectPG(ctx context.Context, ci pg.ConnInfo) (*pg.Client, error) {
	if err := pg.WaitReady(ctx, ci, pgReadyTimeout); err != nil {
		return nil, err
	}
	return pg.Connect(ctx, ci)
}

func findProject(ctx context.Context, cl *controller.Client, poolID, name string) (controller.Project, error) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListProjects(ctx, poolID, page, offset)
		if err != nil {
			return controller.Project{}, fmt.Errorf("list projects: %w", err)
		}
		for _, p := range list.Projects {
			if p.Name == name {
				return p, nil
			}
		}
		if list.Count < page || offset+len(list.Projects) >= list.Total {
			return controller.Project{}, fmt.Errorf("project %q not found after create", name)
		}
	}
}

func countBadDBs(r NativeReport) int {
	n := 0
	for _, db := range r.DBs {
		if !db.Report.OK {
			n++
		}
	}
	return n
}

// discardSink drops collected blobs — the scheduler collects separately before
// teardown, so cluster Destroy inside a cleanup does not need a live sink.
type discardSink struct{}

func (discardSink) Add(string, string, []byte) error { return nil }
