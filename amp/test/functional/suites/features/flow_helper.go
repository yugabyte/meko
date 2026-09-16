package features

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
	"github.com/yugabyte/amp/test/functional/framework/env/registry"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
)

const pgReadyTimeout = 5 * time.Minute

// endpointReadyTimeout bounds the wait for a self-provisioned endpoint's compute
// workload before its Service is port-forwarded: timeouts.endpoint_create,
// scaled by timeouts.provision_multiplier.
func endpointReadyTimeout(t *core.T) time.Duration {
	if t == nil {
		return core.DefaultEndpointCreateTimeout
	}
	return core.ResolveEndpointCreateTimeout(t.Options)
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

// gateNoDoubleProvision skips a self-provisioning test when the env RunOne
// already resolved is itself a real-infra universe.Env in a provisioning mode.
// Such a test stands its OWN backing up in its body; if RunOne's env layer ALSO
// provisioned real infra under one of realInfraProvisionModes, running the body
// would provision a SECOND real backing on top of the first — the double-provision (and double-spend)
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
	return fmt.Sprintf("this test self-provisions its backing; run with --fake-env to avoid double-provisioning — env.universe.mode=%s would double-provision", mode), false
}

// --- guaranteed-cleanup engine ---

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

// pushLast puts a step at the bottom of the stack, so the LIFO run reaches it
// after everything registered before it. For teardown that other teardown
// depends on: restoring cluster-wide trust has to happen after the deletes
// that still talk to the database, not before them.
func (s *teardownStack) pushLast(name string, fn func(context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fns = append([]namedCleanup{{name, fn}}, s.fns...)
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
				logf("teardown: %s (best-effort): %v", c.name, err)
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
	rlog *runLog // backs logf; its bytes are attached as the framework log

	// ampEnv is the standing amp control plane the run attaches to (nil in the
	// self-provision path). When set, the flow sources its controller / pool /
	// endpoint-connect / namespace-kube from it instead of standing up its own
	// backing + amp — see runAmpFeature's attach-vs-self-provision decision.
	ampEnv ampControlPlane

	store ybbacking.BackingStore
	reap  *reap.Plan // reap handles the backing provisioner recorded (StandUp reads it)

	// reg + regID are the crash-recovery net for a `run` (nil under StandUp, which
	// records its own plan in cmd/ampqa/setup). A non-empty backing plan is
	// recorded into reg after provisioning and removed after a clean teardown.
	reg   *registry.Registry
	regID string

	ampApp *ampstack.App
}

func (f *flow) register(name string, fn func(context.Context) error) { f.td.push(name, fn) }

// registerLast is register for a step every other teardown depends on; it runs
// after all of them.
func (f *flow) registerLast(name string, fn func(context.Context) error) { f.td.pushLast(name, fn) }

// suffix is the per-invocation unique token every resource name embeds
// (seed + nonce; see core.T.ResourceSuffix).
func (f *flow) suffix() string { return f.t.ResourceSuffix() }

// namespace is the DNS-safe amp namespace (labelled ampqa-managed by ampstack).
func (f *flow) namespace() string { return "ampqa-fnd-" + f.suffix() }

func (f *flow) provisionBacking(ctx context.Context) error {
	store, plan, err := f.prov(ctx, f.t, f.td, f.logf)
	if err != nil {
		// Standing infrastructure up is ENVIRONMENT work, not a product signal.
		return core.AsEnvFailure(err)
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

// addAmpNamespaceToReapPlan extends this run's crash-recovery record with
// another amp namespace.
//
// The plan is recorded once, at provision time, naming the one namespace the
// flow knows about. A test that REINSTALLS the control plane stands it up in a
// namespace that did not exist then, and a run killed after that would leave it
// (its controller, its endpoint compute pods) untracked by `ampqa env reap`.
// The registry has no update, so the record is replaced: the new one is added
// before the old is removed, because a duplicate reaps twice harmlessly while a
// gap loses the infrastructure.
func (f *flow) addAmpNamespaceToReapPlan(namespace string) {
	if f.reg == nil || f.regID == "" || f.reap == nil {
		return
	}
	planJSON, err := withAmpNamespaceReap(f.reap, ampstack.FromConfig(f.t.Options).Context, namespace).JSON()
	if err != nil || planJSON == "" {
		return
	}
	entry, err := f.reg.Add(registry.Entry{
		Name:     "run-" + f.suffix(),
		Shape:    "functional-run",
		Provider: "functional",
		Data:     map[string]string{"reap_plan": planJSON},
	})
	if err != nil {
		f.logf("re-record crash-recovery reap plan for %s (best-effort): %v", namespace, err)
		return
	}
	old := f.regID
	f.regID = entry.ID
	if _, err := f.reg.Remove(old); err != nil {
		f.logf("remove superseded crash-recovery reap record %s (best-effort): %v", old, err)
	}
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
		f.logf("remove crash-recovery reap record %s (best-effort): %v", f.regID, err)
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
			logf("record crash-recovery reap plan (best-effort): %v", err)
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
			logf("reap registry unavailable, crash-recovery record disabled (best-effort): %v", err)
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

// controllerClient returns a live amp controller client. When a direct
// cluster-internal address was resolved (tailnet mode, the default; see
// apps/ampstack.ControllerReachMode) this is a stable URL with nothing to
// reconnect. When a kubectl port-forward is held instead — portforward mode, or
// tailnet falling back on a cluster whose ranges aren't routable from here — it
// reconnects that forward first if it died since the last call — kubectl
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

// ampStorageBackend is the attach-mode capability for reaching the amp storage
// database: a standing control plane owns the route to its own backing, so the
// flow asks for it rather than deriving one. sharedk8s.Env satisfies it.
type ampStorageBackend interface {
	YBStorage(ctx context.Context) (pg.ConnInfo, error)
}

// ampStorageSQL opens a connection to the backing's amp storage database — the
// one holding the `amp_storage` schema the data path writes. Both flow modes are
// covered: a self-provisioned backing is dialled directly (this process stood it
// up, so its address is known and there is no forward to go stale), and an
// attached control plane is asked for its own route.
//
// The attached route goes through connectStorageConn, NOT a bare connectPG: that
// route is a kubectl port-forward, kubectl outlives its own tunnel, and
// YBStorage's liveness check only sees a dead PROCESS. Without the refresh a
// stale-but-alive forward burns the caller's whole budget on a corpse and then
// reads as a missing capability — or, mid-poll, as a product failure.
func (f *flow) ampStorageSQL(ctx context.Context) (*pg.Client, error) {
	if f.ampEnv != nil {
		be, ok := f.ampEnv.(ampStorageBackend)
		if !ok {
			return nil, fmt.Errorf("%w: this standing control plane exposes no route to its backing amp storage database", errNoStorageRoute)
		}
		refresh := func(context.Context) (pg.ConnInfo, error) { return pg.ConnInfo{}, errNoStorageRefresh }
		if r, ok := f.ampEnv.(storageRefresher); ok {
			refresh = r.RefreshYBStorage
		}
		return connectStorageConn(ctx, be.YBStorage, refresh)
	}
	if f.store == nil {
		return nil, fmt.Errorf("%w: no backing has been provisioned yet", errNoStorageRoute)
	}
	ci := f.store.ConnInfo()
	ci.Database = f.t.Options.GetString("env.ampstack.db_name", ampstack.DefaultDBName)
	return connectPG(ctx, ci)
}

var (
	// errNoStorageRoute marks the capability as ABSENT (no route exists at all),
	// which a test skips on. A route that exists but does not answer is an
	// environment failure instead — the difference decides whether a run reports
	// "not asserted" or "the cluster broke".
	errNoStorageRoute = errors.New("no route to the backing amp storage database")
	// errNoStorageRefresh is what a route with no rebuild path reports; a refused
	// connection on such a route is simply retried until the budget ends.
	errNoStorageRefresh = errors.New("this route cannot be rebuilt")
)

// connectStorageConn is the dead-forward-aware connect shared by every
// storage-side assertion: resolve the route, wait for YSQL in short slices, and
// on a REFUSED loopback port (a dead tunnel behind a live kubectl, never a dead
// database) ask for a fresh route before the next slice. Any other error just
// retries until the budget ends.
func connectStorageConn(ctx context.Context, resolve, refresh func(context.Context) (pg.ConnInfo, error)) (*pg.Client, error) {
	deadline := time.Now().Add(storageReadyTimeout)
	for {
		ci, err := resolve(ctx)
		if err != nil {
			return nil, fmt.Errorf("reach the backing amp storage database: %w", err)
		}
		wait := min(time.Until(deadline), storageProbeBudget)
		if err = pg.WaitReady(ctx, ci, wait); err == nil {
			return pg.Connect(ctx, ci)
		}
		if core.IsConnRefused(err) {
			if fresh, rerr := refresh(ctx); rerr == nil {
				ci = fresh
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("backing amp storage database %s never answered: %w", ci.Addr(), err)
		}
	}
}

type discardSink struct{}

func (discardSink) Add(string, string, []byte) error { return nil }
