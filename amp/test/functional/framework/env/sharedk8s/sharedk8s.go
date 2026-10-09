// Package sharedk8s is the cluster-per-test environment provider (architecture
// §5.1): it drives a standing amp control plane over the controller REST API to
// carve out an isolated cluster + endpoint for each test, and tears the cluster
// down (cascade) on teardown. The controller is reached directly by URL or, when
// only a kube context is configured, through a kubectl port-forward. The
// endpoint's Postgres is reached at the API-reported address, port-forwarded
// through kubectl when it is cluster-internal.
package sharedk8s

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybdeploy"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/diag"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// Defaults.
const (
	DefaultControllerService = "amp-controller"
	DefaultControllerPort    = 8080
	DefaultEndpointPort      = 55433
	DefaultLocalPortBase     = 0 // 0 → kubectl auto-allocates each local port (collision-free)
	DefaultPoolName          = "ampqa"
	DefaultNamespacePrefix   = "ampqa-cp"
	computeUser              = "cloud_admin"
	computeDB                = "postgres"
	// endpointContainer is the endpoint pod's amp container, named explicitly
	// because the pod also carries a postgres-exporter sidecar — an unqualified
	// `kubectl exec` would be ambiguous.
	endpointContainer = "endpoint"
	// computeRestartReadyTimeout bounds waiting for the endpoint's compute
	// workload to report ready again after its pod is rolled.
	computeRestartReadyTimeout = 5 * time.Minute
	// provisionRetries is how many extra attempts cluster/endpoint creation makes
	// on a transient controller 5xx (concurrent-DDL deadlock, endpoint helm race).
	provisionRetries = 4
	// DefaultSharedInfraRecovery matches what the faulting suites allow their own
	// post-chaos settle steps; ReadyTimeout is a provisioning budget and too short.
	DefaultSharedInfraRecovery = 15 * time.Minute
	// sharedInfraCanaryBudget bounds one probe attempt's cluster round trip; a
	// healthy control plane serves it in well under a minute.
	sharedInfraCanaryBudget = 4 * time.Minute
)

// Config is the env.sharedk8s.* configuration.
type Config struct {
	ControllerURL     string // direct controller root URL; wins over port-forward
	Context           string // kube-context for port-forward + collect
	Namespace         string
	ControllerService string
	ControllerPort    int
	EndpointPort      int
	LocalPortBase     int
	UserID            string // pinned user id (idempotent); "" → create a fresh user
	PoolName          string // pool resolved/created by name
	PoolID            string // explicit pool id (skips name lookup)
	ReadyTimeout      time.Duration
	PGReadyTimeout    time.Duration
	// SharedInfraRecovery bounds the post-fault health probe; 0 → DefaultSharedInfraRecovery.
	SharedInfraRecovery time.Duration
	Product             string // recorded as the product version (no version endpoint exists)

	// Deploy selects self-building k8s: when true this provider OWNS one
	// run-scoped amp control plane, deploying the same ampstack.App exclusivek8s
	// builds into an owned ampqa-labelled namespace and carving clusters out of
	// it, rather than attaching to a standing control plane. When false (attach
	// mode) the behaviour is unchanged. Default: deploy unless a standing
	// controller is already configured (a controller_url or a namespace to attach
	// to), so a bare kube-context self-builds and `run --env <id>` attaches.
	Deploy bool
	// DeployTimeout bounds the one-time control-plane bring-up. It is rooted in a
	// background context (not a per-test provision deadline), because the control
	// plane must outlive any single Provision call.
	DeployTimeout time.Duration
	// AmpStack is the composed control-plane config (deploy mode only). It is read
	// from the SAME env.ampstack.* knobs the exclusivek8s provider and the
	// deploy-amp task read, so there is one control-plane config surface; this
	// provider only overrides the namespace (a fresh owned one) and context.
	AmpStack ampstack.Config
}

// FromConfig reads env.sharedk8s.* from a framework Config.
func FromConfig(c *core.Config) Config {
	controllerURL := c.GetString("env.sharedk8s.controller_url", "")
	namespace := c.GetString("env.sharedk8s.namespace", "")
	// Deploy defaults on when there is no standing control plane to attach to
	// (no controller_url and no namespace) — so a bare kube-context self-builds,
	// while `run --env <id>` (which injects a controller_url/namespace) attaches.
	deploy := c.GetBool("env.sharedk8s.deploy", controllerURL == "" && namespace == "")

	amp := ampstack.FromConfig(c)
	if deploy {
		// The control plane's kube-context comes from env.sharedk8s.context first
		// (what the run command pins), falling back to env.ampstack.context and
		// the ybk8s kube-context so a single context knob is enough.
		amp.Context = firstNonEmptyStr(
			c.GetString("env.sharedk8s.context", ""),
			amp.Context,
			c.GetString("apps.ybk8s.kubecontext", ""),
		)
	}
	ctx := c.GetString("env.sharedk8s.context", "")
	if deploy && ctx == "" {
		ctx = amp.Context
	}

	return Config{
		ControllerURL:       controllerURL,
		Context:             ctx,
		Namespace:           namespace,
		ControllerService:   c.GetString("env.sharedk8s.controller_service", DefaultControllerService),
		ControllerPort:      c.GetInt("env.sharedk8s.controller_port", DefaultControllerPort),
		EndpointPort:        c.GetInt("env.sharedk8s.endpoint_port", DefaultEndpointPort),
		LocalPortBase:       c.GetInt("env.sharedk8s.local_port_base", DefaultLocalPortBase),
		UserID:              c.GetString("env.sharedk8s.user_id", ""),
		PoolName:            c.GetString("env.sharedk8s.pool_name", DefaultPoolName),
		PoolID:              c.GetString("env.sharedk8s.pool_id", ""),
		ReadyTimeout:        c.GetDuration("env.sharedk8s.controller_ready_timeout", 2*time.Minute),
		PGReadyTimeout:      c.GetDuration("env.sharedk8s.pg_ready_timeout", 3*time.Minute),
		SharedInfraRecovery: c.GetDuration("env.sharedk8s.shared_infra_recovery", DefaultSharedInfraRecovery),
		Product:             c.GetString("versions.product", ""),
		Deploy:              deploy,
		DeployTimeout:       c.GetDuration("env.sharedk8s.deploy_timeout", amp.YBReadyTimeout+amp.ControllerReadyTimeout+2*time.Minute),
		AmpStack:            amp,
	}
}

// Configured reports whether a shared control plane is reachable (used to gate
// provider selection): a direct controller_url or a kube-context (attach), or —
// in deploy mode — a kube-context to self-build on.
func (c Config) Configured() bool {
	if c.Deploy {
		return c.deployContext() != ""
	}
	return c.ControllerURL != "" || c.Context != ""
}

// deployContext is the kube-context the control plane deploys onto in deploy
// mode (Context, resolved in FromConfig, or the composed ampstack context).
func (c Config) deployContext() string {
	return firstNonEmptyStr(c.Context, c.AmpStack.Context)
}

// SharesInfraAcrossTests: every environment here is a cluster on one control
// plane, whose metadata lives in one YugabyteDB.
func (p *Provider) SharesInfraAcrossTests() bool { return true }

// Provider provisions cluster-per-test environments on a shared control plane.
type Provider struct {
	cfg  Config
	kube *k8s.Kubectl
	logf func(format string, args ...any)

	// Injected in tests; defaults wired in New.
	newClient     func(url string, opts ...controller.Option) *controller.Client
	waitPG        func(ctx context.Context, ci pg.ConnInfo, timeout time.Duration) error
	detectYB      func(ctx context.Context, ci pg.ConnInfo) string
	detectProduct func(ctx context.Context, ci pg.ConnInfo) string

	// Deploy mode (self-building k8s): this provider owns one run-scoped control
	// plane. ampCfg is the composed ampstack config (namespace already pinned to
	// an owned one); newApp builds the App from it (overridable in tests); app is
	// the deployed control plane, nil until the first ensureAppLocked.
	deploy bool
	ampCfg ampstack.Config
	newApp func(cfg ampstack.Config) *ampstack.App
	app    *ampstack.App
	// Monotone: once the shared namespace is gone it never returns, so the
	// per-failure probe is paid once rather than by every red test.
	sharedGone atomic.Bool

	// Run-scoped deploy selection (deploy mode). ybDeployCfg is the raw config the
	// selector reads (env.yb_deploy, knob only — a shared control plane cannot vary
	// its backing per test); ybDeployTeardown reaps what Prepare provisioned, run
	// in Close AFTER the control plane (LIFO).
	ybDeployCfg *core.Config
	// backing provisions every backing of the run, the shared one and each
	// dedicated stack's, off one YBA; built on first use, closed in Close. A
	// stack whose test declares its own BackingConfig gets a provisioner (and
	// YBA) of its own instead, built by newBackingProvisioner (a seam for tests).
	backingMu             sync.Mutex
	backing               backingProvisioner
	newBackingProvisioner func(cfg *core.Config, mode string) (backingProvisioner, error)
	// backingPlan reaps the backing this provider provisioned, from a process
	// that shares no memory with it (an aborted CI build).
	backingPlan      *reap.Plan
	ybDeployTeardown func(context.Context) error
	// backingUniverse is the env that provisioned the backing universe, when the
	// backing IS a universe (yba_provisioned). It is the only handle to the YBA
	// managing it, so a test that maintains the backing needs it; nil for every
	// other backing.
	backingUniverse core.Environment

	ctrl controllerConn // one long-lived controller forward shared by all tests

	// stacks are the live per-test stacks of DedicatedStack tests (dedicated.go);
	// onPlanChange fires when the set changes so the reap plan on disk follows.
	stacksMu     sync.Mutex
	stacks       map[*stack]struct{}
	planMu       sync.Mutex // serializes a stack-set change with the plan write it triggers
	onPlanChange func()

	portCounter atomic.Int64
	nameCounter atomic.Int64
}

// controllerConn owns the provider-wide controller access: a single long-lived
// kubectl port-forward (when the controller is reached by context rather than a
// direct URL) shared across every concurrent test, guarded for concurrent use
// and transparently re-established if the forward dies. Per-test Postgres
// forwards, by contrast, belong to their Env and close in Teardown.
type controllerConn struct {
	p   *Provider
	mu  sync.Mutex
	pf  *k8s.PortForward
	cl  *controller.Client
	url string
}

// get returns a live controller client and its URL, opening or re-opening the
// shared forward as needed.
func (cc *controllerConn) get(ctx context.Context) (*controller.Client, string, error) {
	p := cc.p
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if p.deploy {
		if err := cc.ensureAppLocked(ctx); err != nil {
			return nil, "", err
		}
		cl, url, err := p.app.ControllerClient(ctx)
		if err == nil {
			cc.cl, cc.url = cl, url
		}
		return cl, url, err
	}
	if p.cfg.ControllerURL != "" {
		if cc.cl == nil {
			cc.cl = p.newClient(p.cfg.ControllerURL)
			cc.url = p.cfg.ControllerURL
		}
		return cc.cl, cc.url, nil
	}
	if p.kube == nil {
		return nil, "", fmt.Errorf("env.sharedk8s: neither controller_url nor context configured")
	}
	if cc.pf != nil && cc.pf.Alive() {
		return cc.cl, cc.url, nil
	}
	if cc.pf != nil {
		_ = cc.pf.Close()
		cc.pf, cc.cl, cc.url = nil, nil, ""
	}
	pf, err := p.kube.PortForward(ctx, "svc/"+p.cfg.ControllerService, p.nextLocalPort(), p.cfg.ControllerPort)
	if err != nil {
		return nil, "", fmt.Errorf("port-forward controller: %w", err)
	}
	cc.pf = pf
	cc.url = "http://" + pf.LocalAddr()
	// The client resolves the forward's address per request instead of pinning
	// it, so a test already holding this client survives the forward dying under
	// it. See revive.go.
	cc.cl = p.newClient(cc.url, controller.WithHTTPClient(&http.Client{
		Transport: &revivingTransport{host: cc.liveHost, logf: p.logf},
	}))
	return cc.cl, cc.url, nil
}

// liveHost is the forward's current "host:port". force re-establishes it first,
// which is what a refused connection needs: kubectl port-forward does not
// reconnect, so the dead one must be replaced before a retry can land.
//
// It takes the same lock get() does, so it must never be called from under it.
func (cc *controllerConn) liveHost(force bool) (string, error) {
	if force {
		cc.reset()
	}
	ctx, cancel := context.WithTimeout(context.Background(), controllerReviveTimeout)
	defer cancel()
	_, rawURL, err := cc.get(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse controller url %q: %w", rawURL, err)
	}
	return u.Host, nil
}

// alive reports whether the shared controller forward is currently up (always
// true when the controller is a direct URL).
func (cc *controllerConn) alive() bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.pf == nil || cc.pf.Alive()
}

// reset drops the current forward so the next get re-establishes it.
func (cc *controllerConn) reset() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.pf != nil {
		_ = cc.pf.Close()
		cc.pf, cc.cl, cc.url = nil, nil, ""
	}
}

// reconnect drops whatever address the connection is pinned to and
// re-establishes it. In deploy mode that is the app's resolve-then-forward
// chain (a direct address can be a pod IP, which a pod replacement silently
// invalidates); a fixed ControllerURL has nothing to re-resolve; otherwise the
// shared forward is reopened.
func (cc *controllerConn) reconnect(ctx context.Context) (*controller.Client, string, error) {
	p := cc.p
	cc.mu.Lock()
	if p.deploy && p.app != nil {
		defer cc.mu.Unlock()
		cl, url, err := p.app.ReconnectController(ctx)
		if err == nil {
			cc.cl, cc.url = cl, url
		}
		return cl, url, err
	}
	if p.cfg.ControllerURL != "" {
		defer cc.mu.Unlock()
		return cc.cl, cc.url, nil
	}
	if cc.pf != nil {
		_ = cc.pf.Close()
		cc.pf, cc.cl, cc.url = nil, nil, ""
	}
	cc.mu.Unlock()
	return cc.get(ctx)
}

// currentURL is the controller URL as of the live forward ("" before first use).
func (cc *controllerConn) currentURL() string {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.url
}

func (cc *controllerConn) close() { cc.reset() }

// ensureAppLocked deploys the owned control plane on first use (deploy mode),
// caller holding cc.mu. The bring-up is rooted in context.Background() bounded
// by DeployTimeout — NOT the caller's provision ctx — because the control plane
// must outlive any single Provision call (architecture §5); a short per-test
// provision deadline must not tear the whole control plane down. On success it
// points the provider kube at the deployed namespace so endpoint forwards and
// RestartCompute resolve against it.
func (cc *controllerConn) ensureAppLocked(ctx context.Context) error {
	p := cc.p
	if p.app != nil {
		return nil
	}
	app := p.newApp(p.ampCfg)
	dctx, cancel := context.WithTimeout(context.Background(), p.cfg.DeployTimeout)
	defer cancel()
	p.log("sharedk8s: deploying control plane in namespace %s (context %q, timeout %s)", p.ampCfg.Namespace, p.ampCfg.Context, p.cfg.DeployTimeout)
	if _, err := app.Deploy(dctx); err != nil {
		_ = app.Teardown(context.Background())
		return fmt.Errorf("sharedk8s: deploy control plane in %s: %w", p.ampCfg.Namespace, err)
	}
	p.app = app
	p.kube = app.Kube()
	p.log("sharedk8s: control plane ready in namespace %s", p.ampCfg.Namespace)
	return nil
}

// storageAccess reports how to reach the control plane's backing amp storage
// database. Only deploy mode can answer: this provider then owns the stack and
// knows its backing, whereas an attached standing control plane is reached
// through the controller API alone and its backing is not ours to name.
func (p *Provider) storageAccess() (ampstack.StorageAccess, error) {
	p.ctrl.mu.Lock()
	defer p.ctrl.mu.Unlock()
	if !p.deploy || p.app == nil {
		return ampstack.StorageAccess{}, fmt.Errorf("env.sharedk8s in attach mode does not know the standing control plane's backing YugabyteDB")
	}
	return p.app.StorageAccess(), nil
}

// New builds a Provider from cfg.
func New(cfg Config) *Provider {
	if cfg.ControllerService == "" {
		cfg.ControllerService = DefaultControllerService
	}
	if cfg.ControllerPort == 0 {
		cfg.ControllerPort = DefaultControllerPort
	}
	if cfg.EndpointPort == 0 {
		cfg.EndpointPort = DefaultEndpointPort
	}
	if cfg.PoolName == "" {
		cfg.PoolName = DefaultPoolName
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 2 * time.Minute
	}
	if cfg.PGReadyTimeout == 0 {
		cfg.PGReadyTimeout = 3 * time.Minute
	}
	p := &Provider{
		cfg:           cfg,
		logf:          log.Printf,
		newClient:     defaultNewClient,
		waitPG:        pg.WaitReady,
		detectYB:      detectYBVersion,
		detectProduct: detectProductVersion,
		deploy:        cfg.Deploy,
		newApp:        ampstack.New,
	}
	// Not in the literal above because it captures p: a nil sink here silences
	// the whole own-backing bring-up, which for a multi-region stack is up to
	// 90 minutes of YBA install, provider create, release import and node
	// bring-up reporting nothing but its final error.
	p.newBackingProvisioner = func(c *core.Config, mode string) (backingProvisioner, error) {
		return ybdeploy.NewProvisioner(c, mode, p.logf)
	}
	if cfg.Context != "" {
		p.kube = k8s.New(k8s.Config{Context: cfg.Context, Namespace: cfg.Namespace})
	}
	if cfg.Deploy {
		p.ampCfg = cfg.AmpStack
		if p.ampCfg.Context == "" {
			p.ampCfg.Context = cfg.deployContext()
		}
		if p.ampCfg.Namespace == "" {
			p.ampCfg.Namespace = DefaultNamespacePrefix + "-" + randHex()
		}
		if cfg.DeployTimeout > 0 {
			p.cfg.DeployTimeout = cfg.DeployTimeout
		} else {
			p.cfg.DeployTimeout = p.ampCfg.YBReadyTimeout + p.ampCfg.ControllerReadyTimeout + 2*time.Minute
		}
		// The control plane's namespace is where endpoint compute pods and the
		// controller live; bind the provider kube to it up front so endpoint
		// forwards and RestartCompute resolve there even before the app deploys.
		p.kube = k8s.New(k8s.Config{Context: p.ampCfg.Context, Namespace: p.ampCfg.Namespace})
	}
	p.ctrl.p = p
	return p
}

// WithYBDeployConfig injects the raw framework config the run-scoped deploy
// selector reads (env.yb_deploy plus the yb_k8s/yba sub-app knobs), used in deploy
// mode only. Without it the control plane uses ampstack's built-in yugabyted.
func (p *Provider) WithYBDeployConfig(c *core.Config) *Provider {
	p.ybDeployCfg = c
	return p
}

// Prepare eagerly deploys the run-scoped control plane (deploy mode) so it is up
// BEFORE the scheduler starts provisioning per-test clusters — otherwise the
// first test's short provision deadline would have to absorb the whole
// control-plane bring-up. A no-op in attach mode. The `run` command calls this
// (via an optional-interface assertion) before sched.Run.
func (p *Provider) Prepare(ctx context.Context) error {
	if !p.deploy {
		return nil
	}
	if err := p.prepareYBDeploy(); err != nil {
		return err
	}
	_, _, err := p.ctrl.get(ctx)
	return err
}

// prepareYBDeploy provisions the run-scoped external deployment (deploy mode) before
// the control plane deploys, pointing ampCfg at it. Resolution is KNOB-ONLY: a
// shared control plane serves every test off one YugabyteDB, so a per-test
// preference cannot apply — only env.yb_deploy does. Like the control-plane
// bring-up (ensureAppLocked), it roots the work in context.Background() bounded
// by DeployTimeout, NOT any per-test provision ctx, since the backing must
// outlive every Provision (AGENTS.md §5); ybDeployTeardown releases it in Close.
func (p *Provider) prepareYBDeploy() error {
	if p.ybDeployCfg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.DeployTimeout)
	defer cancel()
	store, td, plan, err := p.provisionBacking(ctx, p.ampCfg.Namespace, core.EnvSpec{})
	if err != nil {
		return err
	}
	p.ybDeployTeardown = td
	p.backingPlan = plan
	if store != nil {
		p.ampCfg.Backing = ybbacking.ExternalBacking(store)
		p.backingUniverse = ybdeploy.UniverseEnvOf(store)
	}
	return nil
}

// backingProvisioner is the ybdeploy.Provisioner surface this provider drives;
// an interface so a unit test can stand in for the real one.
type backingProvisioner interface {
	Provision(ctx context.Context, runTag string) (ybbacking.BackingStore, func(context.Context) error, *reap.Plan, error)
	Plan() *reap.Plan
	Close(ctx context.Context) error
}

// provisionBacking stands up the backing env.yb_deploy names, tagged with the
// namespace of the control plane it serves. A nil store means the built-in
// yugabyted: nothing external to point the control plane at. The shared control
// plane passes an empty spec: its backing is knob-only. A dedicated stack passes
// the test's spec, and a test declaring its own BackingConfig gets that shape on
// a provisioner (and YBA) of its own, returned as own so the stack closes it.
func (p *Provider) provisionBacking(ctx context.Context, tag string, spec core.EnvSpec) (ybbacking.BackingStore, func(context.Context) error, *reap.Plan, error) {
	store, td, plan, _, err := p.provisionBackingFor(ctx, tag, spec)
	return store, td, plan, err
}

func (p *Provider) provisionBackingFor(ctx context.Context, tag string, spec core.EnvSpec) (ybbacking.BackingStore, func(context.Context) error, *reap.Plan, backingProvisioner, error) {
	if p.ybDeployCfg == nil {
		return nil, nil, nil, nil, nil
	}
	cfg := p.ybDeployCfg
	var own backingProvisioner
	declared := false
	if spec.BackingConfig != nil && spec.Dedicated() {
		full := spec.BackingConfig(cfg)
		declared = len(full) > 0
		if overlay := backingOnlyOverlay(full); len(overlay) > 0 {
			cfg = cfg.Derive(overlay)
		}
	}
	mode := ybdeploy.Resolve(cfg, spec)
	if mode == ybdeploy.ModeBuiltin {
		return nil, nil, nil, nil, nil
	}
	var b backingProvisioner
	var err error
	if cfg != p.ybDeployCfg {
		if b, err = p.newBackingProvisioner(cfg, mode); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("sharedk8s: %s backing for %s: %w", mode, tag, err)
		}
		own = b
	} else if b, err = p.backingProvisioner(mode); err != nil {
		return nil, nil, nil, nil, err
	}
	store, td, plan, err := b.Provision(ctx, tag)
	if err != nil && own != nil {
		// A backing the test asked for by shape is never downgraded: the test
		// would run against something other than what it declared.
		cctx, cancel := context.WithTimeout(context.Background(), p.cfg.DeployTimeout)
		cerr := own.Close(cctx)
		cancel()
		if cerr == nil {
			return nil, nil, nil, nil, core.AsEnvFailure(fmt.Errorf("sharedk8s: the backing %s declared for %s failed: %w", mode, tag, err))
		}
		// Close failed, so the YBA it helm-installed is still standing. Hand the
		// provisioner back with the failure: the caller keeps it on the stack so
		// its namespace reaches the reap plan, which is the only record of it.
		p.log("sharedk8s: %s: the declared backing failed AND its YBA could not be torn down (%v); it stays in the reap plan", tag, cerr)
		return nil, nil, own.Plan(), own, core.AsEnvFailure(fmt.Errorf("sharedk8s: the backing %s declared for %s failed: %w (its YBA could not be torn down either: %v)", mode, tag, err, cerr))
	}
	if err != nil && declared {
		// Declared by addressing alone, so the run's shared provisioner served it,
		// but the same rule holds: no fallback to a backing it did not ask for.
		return nil, nil, nil, nil, core.AsEnvFailure(fmt.Errorf("sharedk8s: the backing %s declared for %s failed: %w", mode, tag, err))
	}
	if err != nil && mode == ybdeploy.ModeYBAProvisioned && ybdeploy.Requested(cfg) {
		// The run ASKED for YBA (env.yb_deploy was set explicitly), so a
		// downgrade would answer a different question than the one it was
		// launched to answer: a single-node yugabyted is not an RF=3 universe,
		// and a perf number measured against it is not comparable to one that
		// was. Three multitenant runs were reported as "YBA RF=3" that way
		// before this refused.
		return nil, nil, nil, nil, core.AsEnvFailure(fmt.Errorf("sharedk8s: yba_provisioned backing was requested explicitly and failed; refusing to downgrade to built-in yugabyted: %w", err))
	}
	if err != nil && mode == ybdeploy.ModeYBAProvisioned {
		// Merely the default (no explicit knob): it needs a deployable YBA
		// (quay pull secret + a k8s/cloud provider). Where that isn't available,
		// fall back to ampstack's built-in single-node yugabyted (the proven
		// default backing) so the run still proceeds instead of failing the env.
		p.log("sharedk8s: yba_provisioned backing failed (%v); falling back to built-in yugabyted", err)
		return nil, nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("sharedk8s: provision %s deployment: %w", mode, err)
	}
	if own != nil && plan != nil {
		// The universe plan names the YBA only when the universe owns it; a
		// provisioner-owned YBA is named by the provisioner.
		if bp := own.Plan(); bp != nil {
			plan.Namespaces = append(plan.Namespaces, bp.Namespaces...)
		}
	}
	return store, td, plan, own, nil
}

func (p *Provider) backingProvisioner(mode string) (backingProvisioner, error) {
	p.backingMu.Lock()
	defer p.backingMu.Unlock()
	if p.backing == nil {
		b, err := ybdeploy.NewProvisioner(p.ybDeployCfg, mode, p.logf)
		if err != nil {
			return nil, fmt.Errorf("sharedk8s: %s backing: %w", mode, err)
		}
		p.backing = b
	}
	return p.backing, nil
}

// StandingEnv reports the run-scoped control plane as a standing environment to
// RECORD when `run --env-mode keep` / `--setup-only` skips teardown, so a later
// `run --env <id>` attaches to it and `env rm` reaps its namespace. It mirrors
// the deploy-amp task's record shape (provider "ampstack" + shape
// "dedicated_stack"), which both attachFromEntry and env rm already understand.
// Returns false in attach mode or before the control plane is deployed (nothing
// this provider owns to keep).
func (p *Provider) StandingEnv() (*core.StandingEnv, bool) {
	if !p.deploy || p.app == nil {
		return nil, false
	}
	info := map[string]string{
		"namespace":          p.ampCfg.Namespace,
		"context":            p.ampCfg.Context,
		"controller_service": p.ampCfg.ControllerService,
		"controller_port":    strconv.Itoa(p.ampCfg.ControllerPort),
		"pool_name":          p.cfg.PoolName,
	}
	// Record controller_url only when it is reachable from another process. A
	// loopback URL is a process-local kubectl port-forward (the non-default
	// port-forward reach); persisting it would make a later `--env` attach target
	// a dead local address (controller_url wins over context). Skipping it lets
	// attach fall back to context+namespace and re-establish its own forward.
	if url := p.ctrl.currentURL(); url != "" && !isLoopbackURL(url) {
		info["controller_url"] = url
	}
	return &core.StandingEnv{
		Name:     p.ampCfg.Namespace,
		Shape:    "dedicated_stack",
		Provider: "ampstack",
		Info:     info,
	}, true
}

// Close releases provider-wide resources. In deploy mode it OWNS the run-scoped
// control plane, so it tears it down (release any held forward, then delete the
// namespace). In attach mode it only frees the shared controller port-forward,
// leaving the standing control plane intact.
func (p *Provider) Close() error {
	if !p.deploy {
		p.ctrl.close()
		return nil
	}
	bg := context.Background()
	var firstErr error
	// LIFO: the control plane tears down first, then its backing. The backing is
	// reaped even if the control plane never came up (Prepare provisioned it, then
	// the app deploy failed) — p.app may be nil while ybDeployTeardown is set.
	if p.app != nil {
		_ = p.app.Close(bg)
		if err := p.app.Teardown(bg); err != nil {
			firstErr = err
		}
	}
	if p.ybDeployTeardown != nil {
		if err := p.ybDeployTeardown(bg); err != nil {
			p.log("sharedk8s: teardown backing failed: %v", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	// Last: the YBA every backing of this run was created on.
	p.backingMu.Lock()
	b := p.backing
	p.backingMu.Unlock()
	if b != nil {
		ctx, cancel := context.WithTimeout(bg, p.cfg.DeployTimeout)
		defer cancel()
		if err := b.Close(ctx); err != nil {
			p.log("sharedk8s: teardown yba failed: %v", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func defaultNewClient(url string, opts ...controller.Option) *controller.Client {
	return controller.New(url, opts...)
}

// controllerReviveTimeout bounds re-establishing a dead forward. Short: the
// caller is a request already in flight, and a forward that cannot come back in
// this long is not coming back.
const controllerReviveTimeout = 90 * time.Second

func (p *Provider) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// nextLocalPort returns the local port to request for a forward. Default (base
// unset) is 0 — kubectl auto-allocates a distinct ephemeral port per forward, so
// parallel tests never collide. A configured base restores the legacy
// deterministic scheme (base, base+1, …) for environments that need fixed ports.
func (p *Provider) nextLocalPort() int {
	if p.cfg.LocalPortBase <= 0 {
		return 0
	}
	return p.cfg.LocalPortBase + int(p.portCounter.Add(1)-1)
}

// Provision creates a cluster + endpoint on the shared control plane and returns
// a live environment bound to the endpoint's Postgres.
func (p *Provider) Provision(ctx context.Context, spec core.EnvSpec, seed *core.Seed) (core.Environment, error) {
	if spec.Shape == core.DedicatedStack {
		return p.provisionDedicated(ctx, spec, seed)
	}
	cl, url, err := p.ctrl.get(ctx)
	if err != nil {
		return nil, err
	}
	return p.carve(ctx, cl, url, seed, nil)
}

// carve creates the per-test cluster and endpoint on a control plane: the shared
// one (own == nil) or the stack a DedicatedStack test just brought up.
func (p *Provider) carve(ctx context.Context, cl *controller.Client, url string, seed *core.Seed, own *stack) (core.Environment, error) {
	kube, app, ns := p.kube, p.sharedApp(), p.cfg.Namespace
	if own != nil {
		kube, app, ns = own.kube, own.app, own.ampCfg.Namespace
	}
	var epPF *k8s.PortForward // per-test pg forward, owned by the Env once returned
	fail := func(e error) (core.Environment, error) {
		if epPF != nil {
			_ = epPF.Close()
		}
		// A dead shared controller forward mid-provision otherwise surfaces only
		// as opaque connection-refused API errors; name it explicitly.
		if own == nil && !p.ctrl.alive() {
			return nil, fmt.Errorf("%w (controller port-forward died during provisioning)", e)
		}
		return nil, e
	}

	if err := p.waitControllerReady(ctx, cl); err != nil {
		return fail(err)
	}
	userID, err := p.ensureUser(ctx, cl)
	if err != nil {
		return fail(err)
	}
	poolID, err := p.ensurePool(ctx, cl, userID)
	if err != nil {
		return fail(err)
	}

	proj, branchID, err := p.createCluster(ctx, cl, poolID, seed)
	if err != nil {
		return fail(err)
	}

	ep, err := p.createEndpoint(ctx, cl, poolID, proj.ID, branchID)
	if err != nil {
		return fail(err)
	}

	ci, pf, err := p.resolvePG(ctx, kube, ep)
	if err != nil {
		return fail(err)
	}
	epPF = pf
	if epPF != nil && !epPF.Alive() {
		return fail(fmt.Errorf("endpoint %s port-forward died during provisioning", ep.ID))
	}
	if err := p.waitPG(ctx, ci, p.cfg.PGReadyTimeout); err != nil {
		return fail(fmt.Errorf("endpoint postgres never became ready: %w", err))
	}

	// Product version: the controller's own build identity is the authority
	// (GET /api/v1/version, amp CalVer); the compute-side probe and the
	// configured static value are fallbacks for controllers predating it.
	product, productLive := p.cfg.Product, false
	if vi, err := cl.Version(ctx); err == nil && vi.Version != "" {
		product, productLive = vi.Version, true
	} else if v := p.detectProduct(ctx, ci); v != "" {
		product, productLive = v, true
	}
	// YB version: ask the backing itself. The compute endpoint answers amp's own
	// version(), which carries no YugabyteDB marker, so it is only a fallback for
	// an attached stack whose backing this provider cannot name.
	yb := backingYB(ctx, app)
	if yb == "" {
		yb = p.detectYB(ctx, ci)
	}
	p.log("sharedk8s: versions detected — product=%q (live=%v) yb=%q (live=%v)", product, productLive, yb, yb != "")
	env := &Env{
		controllerURL: url,
		versions:      core.DetectedVersions{Product: product, YB: yb},
		pg:            ci,
		prov:          p,
		poolID:        poolID,
		clusterID:     proj.ID,
		endpointID:    ep.ID,
		endpointAddr:  ep.Address,
		ownCluster:    true,
		pfs:           nonNilPFs(epPF),
		kube:          kube,
		own:           own,
		logf:          p.logf,
		handles: core.EnvHandles{
			Namespace: ns,
			Cluster:   proj.ID,
			Endpoint:  ep.ID,
		},
	}
	if own == nil {
		env.ownCtrl = p.ownControllerConn()
	}
	env.capturePGPod(ctx)
	return env, nil
}

// ownControllerConn returns a fresh per-environment controller connection, or nil
// when the mode has no forward to own (a direct controller URL, or deploy mode
// where the app owns the reach). Opened lazily on first use, so an Env that never
// talks to the controller costs no kubectl process.
func (p *Provider) ownControllerConn() *controllerConn {
	if p.deploy || p.cfg.ControllerURL != "" || p.kube == nil {
		return nil
	}
	return &controllerConn{p: p}
}

// Attach treats ref as a cluster id on the configured pool: it reads the cluster
// and its first endpoint and returns an environment that will NOT delete the
// cluster on teardown.
func (p *Provider) Attach(ctx context.Context, ref string) (core.Environment, error) {
	cl, url, err := p.ctrl.get(ctx)
	if err != nil {
		return nil, err
	}
	var epPF *k8s.PortForward
	fail := func(e error) (core.Environment, error) {
		if epPF != nil {
			_ = epPF.Close()
		}
		return nil, e
	}
	poolID := p.cfg.PoolID
	if poolID == "" {
		pid, err := p.ensurePool(ctx, cl, p.cfg.UserID)
		if err != nil {
			return fail(err)
		}
		poolID = pid
	}
	proj, err := cl.GetCluster(ctx, poolID, ref)
	if err != nil {
		return fail(fmt.Errorf("attach cluster %s: %w", ref, err))
	}
	branchID := proj.DefaultBranch.BranchID
	eps, err := cl.ListAllEndpoints(ctx, poolID, proj.ID, branchID)
	if err != nil {
		return fail(err)
	}
	if len(eps) == 0 {
		return fail(fmt.Errorf("attach cluster %s: no endpoints on default branch", ref))
	}
	ci, pf, err := p.resolvePG(ctx, p.kube, eps[0])
	if err != nil {
		return fail(err)
	}
	epPF = pf
	product := p.cfg.Product
	if v := p.detectProduct(ctx, ci); v != "" {
		product = v
	}
	env := &Env{
		controllerURL: url,
		versions:      core.DetectedVersions{Product: product, YB: p.detectYB(ctx, ci)},
		pg:            ci,
		prov:          p,
		poolID:        poolID,
		clusterID:     proj.ID,
		endpointID:    eps[0].ID,
		endpointAddr:  eps[0].Address,
		ownCluster:    false,
		pfs:           nonNilPFs(epPF),
		kube:          p.kube,
		logf:          p.logf,
		handles:       core.EnvHandles{Namespace: p.cfg.Namespace, Cluster: proj.ID, Endpoint: eps[0].ID},
	}
	env.capturePGPod(ctx)
	return env, nil
}

// Teardown deletes the per-test cluster (cascade) and closes the per-test pg
// port-forward. The shared controller forward is provider-owned and left up.
func (p *Provider) Teardown(ctx context.Context, env core.Environment) error {
	e, ok := env.(*Env)
	if !ok {
		return fmt.Errorf("sharedk8s teardown: unexpected environment type %T", env)
	}
	if e.own != nil {
		return p.teardownDedicated(e)
	}
	var firstErr error
	if e.ownCluster {
		if err := p.deleteCluster(ctx, e); err != nil {
			firstErr = err
			p.log("sharedk8s: teardown of cluster %s failed: %v", e.clusterID, err)
		}
	}
	e.pgMu.Lock()
	pfs := e.pfs // Close is idempotent; leave the handles in place for callers inspecting them
	e.pgMu.Unlock()
	closeAll(pfs)
	closeAll(e.takeExtraPFs())
	if e.ownCtrl != nil {
		e.ownCtrl.reset() // closes this Env's own controller forward
	}
	return firstErr
}

// deleteCluster cascade-deletes the cluster over the shared controller forward,
// health-checking it first (get re-establishes a dead forward) and retrying once
// after a forced reconnect when the delete fails connection-refused — the
// signature of a forward that died between provisioning and teardown.
func (p *Provider) deleteCluster(ctx context.Context, e *Env) error {
	cl, _, err := p.ctrl.get(ctx)
	if err != nil {
		return fmt.Errorf("delete cluster %s: controller unavailable: %w", e.clusterID, err)
	}
	if _, err := cl.DeleteClusterAndWait(ctx, e.poolID, e.clusterID); err == nil {
		return nil
	} else if !isConnRefused(err) {
		return fmt.Errorf("delete cluster %s: %w", e.clusterID, err)
	}
	p.ctrl.reset()
	cl, _, err = p.ctrl.get(ctx)
	if err != nil {
		return fmt.Errorf("delete cluster %s: reconnect controller: %w", e.clusterID, err)
	}
	if _, err := cl.DeleteClusterAndWait(ctx, e.poolID, e.clusterID); err != nil {
		return fmt.Errorf("delete cluster %s (after reconnect): %w", e.clusterID, err)
	}
	return nil
}

func (p *Provider) waitControllerReady(ctx context.Context, cl *controller.Client) error {
	return p.waitController(ctx, cl, p.cfg.ReadyTimeout, true, func(ctx context.Context, cl *controller.Client) error {
		return cl.Readyz(ctx)
	})
}

// SharedInfraHealthy verifies the standing control plane can still do what the
// next test will ask of it: answer readyz, serve a registry-backed read, and
// COMPLETE A TASK. Reads alone are not enough — in run 0b3ce69b readyz and
// ListStoragePools both answered in under a second while every following test
// died creating a cluster, because task execution was what the nemesis had
// wedged.
//
// No hang capture: a probe that runs during and after backing chaos would
// SIGQUIT a controller that is merely waiting on a flattened backing, which is
// the kill #598 narrowed this to avoid. The budget is the post-chaos recovery
// one the faulting suites themselves allow, not the provisioning readiness one.
func (p *Provider) SharedInfraHealthy(ctx context.Context) error {
	cl, _, err := p.ctrl.get(ctx)
	if err != nil {
		return fmt.Errorf("shared infra health: reach controller: %w", err)
	}
	return p.waitController(ctx, cl, p.sharedInfraRecoveryBudget(), false, func(ctx context.Context, cl *controller.Client) error {
		if err := cl.Readyz(ctx); err != nil {
			return err
		}
		if _, err := cl.ListStoragePools(ctx, 1, 0); err != nil {
			return err
		}
		return p.canaryCluster(ctx, cl)
	})
}

// SharedInfraGone reports the SHARED control plane's namespace missing; nothing in the product can delete it.
func (p *Provider) SharedInfraGone(ctx context.Context) (string, bool) {
	// A stack that never deployed has no namespace to lose, and an all-dedicated
	// run never deploys one — without this every failure there reads as deleted.
	if p.sharedApp() == nil && p.deploy {
		return "", false
	}
	ns := p.kube.Namespace()
	if ns == "" {
		return "", false
	}
	if p.sharedGone.Load() {
		return "control-plane namespace " + ns, true
	}
	where := "control-plane namespace " + ns
	phase, err := p.kube.GetClusterScoped(ctx, "namespace", ns, "{.status.phase}")
	switch {
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "not found"):
	// Terminating counts: the delete is irreversible and every write is already refused.
	case err == nil && strings.EqualFold(phase, "Terminating"):
	default:
		return "", false
	}
	p.sharedGone.Store(true)
	return where, true
}

// canaryCluster runs one create/delete round trip — the first thing every test
// does — on its own bounded budget so a wedged task pipeline fails the probe
// instead of each later test's provisioning timeout.
func (p *Provider) canaryCluster(ctx context.Context, cl *controller.Client) error {
	cctx, cancel := context.WithTimeout(ctx, sharedInfraCanaryBudget)
	defer cancel()
	user, err := p.ensureUser(cctx, cl)
	if err != nil {
		return fmt.Errorf("canary: %w", err)
	}
	poolID, err := p.ensurePool(cctx, cl, user)
	if err != nil {
		return fmt.Errorf("canary: %w", err)
	}
	name := "ampqa-health-" + randHex()
	if _, err := cl.CreateClusterAndWait(cctx, poolID, controller.CreateClusterRequest{Name: name}); err != nil {
		return fmt.Errorf("canary: create cluster %s: %w", name, err)
	}
	cluster, err := p.findCluster(cctx, cl, poolID, name)
	if err != nil {
		return fmt.Errorf("canary: %w", err)
	}
	if _, err := cl.DeleteClusterAndWait(cctx, poolID, cluster.ID); err != nil {
		return fmt.Errorf("canary: delete cluster %s: %w", name, err)
	}
	return nil
}

// sharedInfraRecoveryBudget is how long a nemesis-flattened control plane has to
// come back before the run treats it as dead.
func (p *Provider) sharedInfraRecoveryBudget() time.Duration {
	if p.cfg.SharedInfraRecovery > 0 {
		return p.cfg.SharedInfraRecovery
	}
	return DefaultSharedInfraRecovery
}

func (p *Provider) waitController(ctx context.Context, cl *controller.Client, budget time.Duration, captureHang bool, probe func(context.Context, *controller.Client) error) error {
	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var last error
	started := time.Now()
	reconnected := false
	for {
		if last = probe(wctx, cl); last == nil {
			return nil
		}
		// A pod replacement silently invalidates a direct (pod-IP) controller
		// address, and polling the dead IP for the whole budget reads exactly
		// like a hang. Halfway through, re-resolve the address once and keep
		// polling whatever answers — in run #213 the replacement pod was healthy
		// within a minute while the old address stayed black-holed for twenty.
		if !reconnected && time.Since(started) >= budget/2 {
			reconnected = true
			if ncl, nurl, err := p.ctrl.reconnect(ctx); err != nil {
				p.log("sharedk8s: controller unreachable (%v) and re-resolving its address failed: %v", last, err)
			} else {
				p.log("sharedk8s: controller unreachable (%v) — address re-resolved to %s", last, nurl)
				cl = ncl
			}
		}
		select {
		case <-wctx.Done():
			if captureHang {
				p.captureControllerHang(ctx, last)
			}
			return fmt.Errorf("controller not ready: %w (last: %v)", wctx.Err(), last)
		case <-time.After(time.Second):
		}
	}
}

// captureControllerHang preserves the evidence of an up-but-hung controller —
// the state that post-mortem logs cannot show, because they end at the hang.
// A Go process's only goroutine dump is SIGQUIT, which also exits it; that is
// the operator's move anyway (the pod is useless and the restart may heal it),
// and the dump lands in the PREVIOUS container's logs, which every failure
// collection already gathers.
//
// Fires only for a CONFIRMED hang. Connection-refused means the process is
// down and its previous logs already tell that story. And an unreachable
// address is only a hang if it still belongs to the live pod: the exec targets
// the deployment's CURRENT pod, while the client may be dialing the previous
// one — in run #213 a pod replacement left the framework dialing a dead pod IP,
// and this function SIGQUIT'd the healthy replacement twenty times, each kill
// manufacturing the very unreadiness it was diagnosing.
func (p *Provider) captureControllerHang(ctx context.Context, last error) {
	if p.kube == nil || last == nil || isConnRefused(last) {
		return
	}
	// Detached from the caller's (likely exhausted) context: the capture is the
	// last useful thing this environment does.
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The staleness check needs both sides: the host the client dialed and the
	// live pod's IP. A loopback host is our own port-forward, which targets the
	// live pod by construction; anything unknown keeps the dump (diagnostics
	// beat silence), anything provably stale skips the kill.
	if host := urlHost(p.ctrl.currentURL()); host != "" && !isLoopbackURL(p.ctrl.currentURL()) {
		podIP, err := p.kube.GetSelector(cctx, "pod", "app="+DefaultControllerService, "{.items[0].status.podIP}")
		if err == nil && podIP != "" && podIP != host {
			p.log("sharedk8s: controller unreachable at %s but the live pod is %s — a stale address, not a hang; skipping the SIGQUIT", host, podIP)
			return
		}
	}
	p.log("sharedk8s: controller readyz hung (%v) — sending SIGQUIT for a goroutine dump (pod will restart)", last)
	run := p.kube.ForPod("deployment/"+DefaultControllerService, "")
	if res, err := run.Run(cctx, "kill -QUIT 1"); err != nil {
		p.log("sharedk8s: hang capture failed: %v (%s)", err, strings.TrimSpace(res.Stderr))
		return
	}
	// Give the runtime a moment to flush the dump before collection reads
	// --previous; the restart itself is kubelet's business.
	select {
	case <-cctx.Done():
	case <-time.After(hangDumpFlushDelay):
	}
}

// hangDumpFlushDelay is how long the capture waits for the exiting runtime to
// flush its dump; a var so the unit test does not sleep for real.
var hangDumpFlushDelay = 3 * time.Second

// ensureUser pins the configured user id (idempotent) or creates a fresh user.
func (p *Provider) ensureUser(ctx context.Context, cl *controller.Client) (string, error) {
	u, err := cl.CreateUser(ctx, controller.CreateUserRequest{ID: p.cfg.UserID})
	if err != nil {
		return "", fmt.Errorf("ensure user: %w", err)
	}
	return u.ID, nil
}

// ensurePool resolves the pool by name (creating it if absent), idempotently
// and safely under concurrency. In deploy mode the control plane starts with no
// pool, so every parallel test's Provision races to create the "ampqa" pool;
// the loser gets a name-conflict, which is tolerated by re-scanning for the pool
// the winner just created (the same shape as CREATE DATABASE IF NOT EXISTS).
func (p *Provider) ensurePool(ctx context.Context, cl *controller.Client, ownerUserID string) (string, error) {
	if p.cfg.PoolID != "" {
		return p.cfg.PoolID, nil
	}
	if id, err := p.findPoolByName(ctx, cl); err != nil {
		return "", err
	} else if id != "" {
		return id, nil
	}
	pool, err := cl.CreateStoragePool(ctx, controller.CreateStoragePoolRequest{Name: p.cfg.PoolName, OwnerUserID: ownerUserID})
	if err != nil {
		if isAlreadyExists(err) {
			if id, ferr := p.findPoolByName(ctx, cl); ferr == nil && id != "" {
				return id, nil
			}
		}
		return "", fmt.Errorf("create pool %s: %w", p.cfg.PoolName, err)
	}
	return pool.ID, nil
}

// findPoolByName scans the paged pool list for p.cfg.PoolName, returning its id
// or "" when absent.
func (p *Provider) findPoolByName(ctx context.Context, cl *controller.Client) (string, error) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListStoragePools(ctx, page, offset)
		if err != nil {
			return "", fmt.Errorf("list pools: %w", err)
		}
		for _, pool := range list.StoragePools {
			if pool.Name == p.cfg.PoolName {
				return pool.ID, nil
			}
		}
		if list.Count < page || offset+list.Count >= list.Total {
			return "", nil
		}
	}
}

// createCluster creates a cluster (with its default branch) on the control
// plane and returns it plus the default branch id. The amp controller
// occasionally answers a concurrent create with a transient 5xx — most often a
// deadlock in its own backing DB when several clusters' branch recipes are
// recorded at once (deploy mode starts every parallel test provisioning at once)
// — so it retries with backoff and a FRESH cluster name each attempt (a failed
// create can leave the old name half-populated).
func (p *Provider) createCluster(ctx context.Context, cl *controller.Client, poolID string, seed *core.Seed) (controller.Cluster, string, error) {
	var lastErr error
	for attempt := 0; attempt <= provisionRetries; attempt++ {
		if attempt > 0 && !backoff(ctx, attempt) {
			return controller.Cluster{}, "", fmt.Errorf("create cluster: %w (last: %v)", ctx.Err(), lastErr)
		}
		name := fmt.Sprintf("ampqa-p-%x-%d", uint64(seed.Value()), p.nameCounter.Add(1))
		if _, err := cl.CreateClusterAndWait(ctx, poolID, controller.CreateClusterRequest{Name: name}); err != nil {
			lastErr = fmt.Errorf("create cluster %s: %w", name, err)
			if transientController(err) {
				p.log("sharedk8s: %v — retrying with a fresh name (attempt %d/%d)", lastErr, attempt+1, provisionRetries)
				continue
			}
			return controller.Cluster{}, "", lastErr
		}
		proj, err := p.findCluster(ctx, cl, poolID, name)
		if err != nil {
			return controller.Cluster{}, "", err
		}
		return proj, proj.DefaultBranch.BranchID, nil
	}
	return controller.Cluster{}, "", lastErr
}

// createEndpoint returns a live read_write endpoint on the branch. The
// controller's create_endpoint is not idempotent: an endpoint helm-deploy race
// ("services yb-ep-… not found") can leave a partial endpoint RECORD that a
// re-create then rejects as a conflict. So each attempt first REUSES any
// existing read_write endpoint on the branch (the racing deploy usually
// finishes moments later, and the downstream Postgres-readiness wait validates
// it), and only creates one when none exists — retrying on a transient 5xx or a
// record conflict.
func (p *Provider) createEndpoint(ctx context.Context, cl *controller.Client, poolID, clusterID, branchID string) (controller.Endpoint, error) {
	var lastErr error
	for attempt := 0; attempt <= provisionRetries; attempt++ {
		if attempt > 0 {
			if !backoff(ctx, attempt) {
				return controller.Endpoint{}, fmt.Errorf("create endpoint: %w (last: %v)", ctx.Err(), lastErr)
			}
			// A prior attempt left a partial endpoint record (the non-idempotent
			// create + helm race); reuse it rather than re-create into a conflict.
			if ep, ok := p.existingEndpoint(ctx, cl, poolID, clusterID, branchID); ok {
				return ep, nil
			}
		}
		if _, err := cl.CreateEndpointAndWait(ctx, poolID, clusterID, branchID, controller.CreateEndpointRequest{Role: "read_write"}); err != nil {
			lastErr = fmt.Errorf("create endpoint: %w", err)
			if transientController(err) || isAlreadyExists(err) {
				p.log("sharedk8s: %v — reusing/retrying endpoint (attempt %d/%d)", lastErr, attempt+1, provisionRetries)
				continue
			}
			return controller.Endpoint{}, lastErr
		}
		return p.pickEndpoint(ctx, cl, poolID, clusterID, branchID)
	}
	return controller.Endpoint{}, lastErr
}

// existingEndpoint returns a read_write endpoint already present on the branch,
// or ok=false when none exists (list errors are treated as "none", so the caller
// proceeds to create).
func (p *Provider) existingEndpoint(ctx context.Context, cl *controller.Client, poolID, clusterID, branchID string) (controller.Endpoint, bool) {
	eps, err := cl.ListAllEndpoints(ctx, poolID, clusterID, branchID)
	if err != nil || len(eps) == 0 {
		return controller.Endpoint{}, false
	}
	for _, ep := range eps {
		if ep.Role == "read_write" {
			return ep, true
		}
	}
	return eps[0], true
}

// backoff sleeps an exponential, jittered interval before the given retry
// attempt (1-based), returning false if ctx is cancelled first. The jitter
// de-synchronizes parallel provisions so their retries don't re-collide on the
// controller's contended DDL path.
func backoff(ctx context.Context, attempt int) bool {
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	d += time.Duration(mrand.Int63n(int64(time.Second)))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// transientController reports whether err is a retryable controller-side 5xx:
// a backing-DB deadlock during concurrent cluster/branch creation, or the
// endpoint helm-deploy race, both of which clear on a fresh attempt.
func transientController(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "status 500") || strings.Contains(s, " 500:") || strings.Contains(s, "status 503") {
		return true
	}
	return strings.Contains(s, "deadlock") || (strings.Contains(s, "not found") && strings.Contains(s, "yb-ep-"))
}

func (p *Provider) findCluster(ctx context.Context, cl *controller.Client, poolID, name string) (controller.Cluster, error) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListClusters(ctx, poolID, page, offset)
		if err != nil {
			return controller.Cluster{}, fmt.Errorf("list clusters: %w", err)
		}
		for _, proj := range list.Clusters {
			if proj.Name == name {
				return proj, nil
			}
		}
		if list.Count < page || offset+list.Count >= list.Total {
			return controller.Cluster{}, fmt.Errorf("created cluster %q not found", name)
		}
	}
}

// pickEndpoint returns the branch's read_write endpoint (falling back to the
// first).
func (p *Provider) pickEndpoint(ctx context.Context, cl *controller.Client, poolID, clusterID, branchID string) (controller.Endpoint, error) {
	eps, err := cl.ListAllEndpoints(ctx, poolID, clusterID, branchID)
	if err != nil {
		return controller.Endpoint{}, err
	}
	if len(eps) == 0 {
		return controller.Endpoint{}, fmt.Errorf("no endpoints after create")
	}
	for _, ep := range eps {
		if ep.Role == "read_write" {
			return ep, nil
		}
	}
	return eps[0], nil
}

// resolvePG turns an endpoint address into a pg.ConnInfo, port-forwarding the
// endpoint Service through kubectl when a context is configured (the address is
// then cluster-internal).
func (p *Provider) resolvePG(ctx context.Context, kube *k8s.Kubectl, ep controller.Endpoint) (pg.ConnInfo, *k8s.PortForward, error) {
	host, port := splitAddr(ep.Address, p.cfg.EndpointPort)
	ci := pg.ConnInfo{User: computeUser, Database: computeDB}
	if kube == nil {
		ci.Host, ci.Port = host, port
		return ci, nil, nil
	}
	// Assert the compute actually came up before port-forwarding to it. The
	// controller's CreateEndpoint returns once the endpoint RECORD exists, but
	// its pod may not be Ready yet — and `kubectl port-forward svc/...` exits 1
	// the instant the Service has no ready backing pod. Waiting on the workload
	// turns a transient start-up race into a bounded wait, and a genuinely
	// never-Ready compute into a clear failure instead of an opaque "port-forward
	// not ready". Same assertion the restart path already makes.
	if err := ampstack.WaitEndpointWorkloadReady(ctx, kube, ep.ID, computeRestartReadyTimeout); err != nil {
		return pg.ConnInfo{}, nil, fmt.Errorf("endpoint %s compute never became ready: %w", ep.ID, err)
	}
	local := p.nextLocalPort()
	pf, err := kube.PortForward(ctx, "svc/yb-ep-"+ep.ID, local, port)
	if err != nil {
		return pg.ConnInfo{}, nil, fmt.Errorf("port-forward endpoint %s: %w", ep.ID, err)
	}
	ci.Host, ci.Port = "127.0.0.1", pf.LocalPort()
	return ci, pf, nil
}

// Env is a live shared-control-plane environment (one cluster + endpoint).
type Env struct {
	controllerURL string
	versions      core.DetectedVersions
	pg            pg.ConnInfo
	handles       core.EnvHandles

	prov         *Provider
	poolID       string
	clusterID    string
	endpointID   string
	endpointAddr string // controller-reported endpoint address; re-resolved after a compute restart
	ownCluster   bool
	pfs          []*k8s.PortForward
	kube         *k8s.Kubectl
	own          *stack // the stack this DedicatedStack env brought up; nil on the shared control plane
	logf         func(format string, args ...any)

	// extraPFs are per-endpoint pg forwards opened by ConnectEndpoint (the
	// functional amp-feature flow attaching to this standing control plane creates
	// several endpoints of its own). Guarded because co-tenant tests share one Env
	// and call ConnectEndpoint concurrently; all are closed in Teardown.
	extraMu  sync.Mutex
	extraPFs []*k8s.PortForward
	// extraPG is AttestPGConnDrop's baseline for those endpoints, keyed by the
	// address the caller dials.
	extraPG map[string]*extraEndpoint

	// pgMu serializes rebuilds of the endpoint forward and guards pg/pfs. One
	// Env serves many co-tenant tests, so a refused connection is noticed by
	// several at once.
	pgMu sync.Mutex

	// pgPod is AttestPGConnDrop's baseline (conndrop.go), guarded by pgMu.
	pgPod *pgPodSnap
	// ownAddrs are the addresses THIS endpoint has been reachable at, superseded
	// ones included: a co-tenant may rebuild the forward before a drop on the old
	// local port is attested. Guarded by pgMu.
	ownAddrs map[string]bool
	// pgProbe and pgReady are test seams over the postmaster query and the wait.
	pgProbe func(ctx context.Context, ci pg.ConnInfo) (string, error)
	pgReady func(ctx context.Context, ci pg.ConnInfo) error

	// samplers holds the per-test server-metrics samplers (servermetrics.go).
	samplers serverSamplers

	// ownCtrl is THIS environment's controller forward, when the control plane is
	// reached by kubectl. Per-environment rather than per-run so a dead forward,
	// and the revival that replaces it, touch only the tests using this Env.
	// Sharing one forward across a run means one test's recovery closes the port
	// its neighbours are mid-request on, and they fail before recovering too.
	// Nil in direct-URL and deploy modes, which have no forward of their own.
	ownCtrl *controllerConn

	// Memoized backing-storage access (see YBStorage). Resolving it may open a
	// port-forward, so co-tenant tests must not each open their own; the result —
	// including a failure — is computed once. Any forward is registered with
	// addExtraPF, so Teardown closes it with the rest.
	storageMu   sync.Mutex
	storageCI   pg.ConnInfo
	storageErr  error
	storageDone bool
	// storagePF is the memoized result's forward (nil when the backing is dialled
	// directly). An idle forward can be dropped by the cluster's proxy path
	// between the capability check at test start and the storage phase that
	// actually dials it, so YBStorage re-resolves instead of returning a dead one.
	storagePF *k8s.PortForward
}

type extraEndpoint struct {
	ep   controller.Endpoint
	ci   pg.ConnInfo
	snap *pgPodSnap
}

// addExtraPF records a port-forward ConnectEndpoint opened, so Teardown closes it.
func (e *Env) addExtraPF(pf *k8s.PortForward) {
	e.extraMu.Lock()
	e.extraPFs = append(e.extraPFs, pf)
	e.extraMu.Unlock()
}

// takeExtraPFs returns and clears the recorded extra forwards (Teardown closes them).
func (e *Env) takeExtraPFs() []*k8s.PortForward {
	e.extraMu.Lock()
	defer e.extraMu.Unlock()
	pfs := e.extraPFs
	e.extraPFs = nil
	return pfs
}

// AmpController returns a live amp controller client over the standing control
// plane, re-establishing the provider-owned forward as needed. It lets the
// functional amp-feature flow ATTACH to this control plane instead of deploying its
// own — one half of the ampControlPlane handle env/functional type-asserts for.
func (e *Env) AmpController(ctx context.Context) (*controller.Client, error) {
	if e.own != nil {
		cl, _, err := e.own.app.ControllerClient(ctx)
		return cl, err
	}
	if e.ownCtrl != nil {
		cl, _, err := e.ownCtrl.get(ctx)
		return cl, err
	}
	cl, _, err := e.prov.ctrl.get(ctx)
	return cl, err
}

// AmpPoolID is the storage pool the provider resolved/created on the standing
// control plane; an attaching test creates its cluster-per-test on it.
func (e *Env) AmpPoolID() string { return e.poolID }

// AmpKube is the kubectl scoped to the control-plane namespace, where endpoint
// compute pods live — the compute-restart feature rolls pods through it. Nil when
// the controller is reached by a direct URL (no context), in which case that
// feature cannot run.
func (e *Env) AmpKube() *k8s.Kubectl { return e.kube }

// ReapPlan names everything this provider stood up: the amp control-plane
// namespace and whatever backing it provisioned. Serializable on purpose — a
// killed run leaves it behind for `ampqa env rm --plan`.
func (p *Provider) ReapPlan() *reap.Plan {
	if !p.deploy {
		return nil
	}
	out := &reap.Plan{}
	if p.backingPlan != nil {
		out.Cloud = p.backingPlan.Cloud
		out.Namespaces = append(out.Namespaces, p.backingPlan.Namespaces...)
	}
	if ns := p.ampCfg.Namespace; ns != "" {
		out.Namespaces = append(out.Namespaces, reap.NSReap{
			Kind: reap.KindAmp, KubeContext: p.ampCfg.Context, Namespace: ns,
		})
	}
	for _, st := range p.liveStacks() {
		st.addToPlan(out)
	}
	p.backingMu.Lock()
	b := p.backing
	p.backingMu.Unlock()
	if b != nil {
		if bp := b.Plan(); bp != nil {
			out.Namespaces = append(out.Namespaces, bp.Namespaces...)
		}
	}
	out.Namespaces = dedupeNS(out.Namespaces)
	if out.Empty() {
		return nil
	}
	return out
}

func dedupeNS(in []reap.NSReap) []reap.NSReap {
	seen := map[string]bool{}
	out := in[:0]
	for _, n := range in {
		k := n.Kind + "/" + n.KubeContext + "/" + n.Namespace
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
	}
	return out
}

// BackingKube is a kube handle scoped to the namespace the backing
// YugabyteDB's pods actually run in — its own for an external backing that
// told us (a co-located ybk8s deploy, a provisioned YBA universe), the amp
// namespace for the built-in yugabyted. It is what backing-directed chaos and
// diagnostics must aim at; the amp namespace holds no tserver when the backing
// lives elsewhere.
func (e *Env) BackingKube() *k8s.Kubectl {
	if e.kube == nil {
		return nil
	}
	return e.kube.InNamespace(e.backingNamespace())
}

// YBAUniverse exposes the YBA control plane behind this env's BACKING and the
// universe it manages, when the backing is a YBA-provisioned universe. It lets a
// test maintain the YugabyteDB under amp — upgrade it, change its gflags — the
// way an operator does. A nil maintainer means the backing is not a universe.
func (e *Env) YBAUniverse() (universe.YBAMaintainer, string) {
	u, ok := e.backingUniverseEnv().(interface {
		YBAUniverse() (universe.YBAMaintainer, string)
	})
	if !ok {
		return nil, ""
	}
	return u.YBAUniverse()
}

// OwnsUniverse is true unless the backing is a universe this run attached to.
func (e *Env) OwnsUniverse() bool {
	if u, ok := e.backingUniverseEnv().(interface{ OwnsUniverse() bool }); ok {
		return u.OwnsUniverse()
	}
	return true
}

// KubeUniverse locates the backing universe's namespace, for a caller that needs
// to run something beside it. Empty when the backing is not a kubernetes
// universe.
func (e *Env) KubeUniverse() (kubeContext, kubeconfig, namespace string) {
	u, ok := e.backingUniverseEnv().(interface {
		KubeUniverse() (string, string, string)
	})
	if !ok {
		return "", "", ""
	}
	return u.KubeUniverse()
}

// InClusterAddrs are the backing universe's tserver addresses as reached from
// inside the cluster. Empty when the backing is not a universe.
func (e *Env) InClusterAddrs() []string {
	u, ok := e.backingUniverseEnv().(interface{ InClusterAddrs() []string })
	if !ok {
		return nil
	}
	return u.InClusterAddrs()
}

func (e *Env) backingUniverseEnv() core.Environment {
	if e.own != nil {
		return e.own.backingUniverse
	}
	if e.prov == nil {
		return nil
	}
	return e.prov.backingUniverse
}

// EndpointInClusterAddr is where a workload running IN this cluster reaches a
// tenant's endpoint: the compute Service's own DNS name. ConnectEndpoint gives
// the coordinator a port-forward instead, which a pod cannot use — a client pod
// beside amp must dial the Service directly.
func (e *Env) EndpointInClusterAddr(ep controller.Endpoint) (host string, port int) {
	ns := ""
	if e.kube != nil {
		ns = e.kube.Namespace()
	}
	port = e.prov.cfg.EndpointPort
	if _, p := splitAddr(ep.Address, port); p != 0 {
		port = p
	}
	return fmt.Sprintf("yb-ep-%s.%s.svc.cluster.local", ep.ID, ns), port
}

// EndpointCredentials are the role and database an endpoint's compute serves.
func (e *Env) EndpointCredentials() (user, database string) { return computeUser, computeDB }

// ConnectEndpoint opens a pooled SQL connection to an amp endpoint on the standing
// control plane, port-forwarding the endpoint Service through kubectl when it is
// cluster-internal (context mode). The forward is owned by the Env (recorded and
// closed in Teardown), bounded by the handful of endpoints a feature test creates,
// so callers close only the returned client. It waits for the endpoint's Postgres
// to accept connections first (a freshly-created endpoint's compute may still be
// starting); the wait is bounded by the caller's ctx, so a probe against a retired
// endpoint (Service gone) fails fast rather than blocking.
func (e *Env) ConnectEndpoint(ctx context.Context, ep controller.Endpoint) (*pg.Client, error) {
	ci, pf, err := e.prov.resolvePG(ctx, e.kube, ep)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok && pf != nil {
			_ = pf.Close()
		}
	}()
	if err := e.prov.waitPG(ctx, ci, e.prov.cfg.PGReadyTimeout); err != nil {
		return nil, fmt.Errorf("endpoint %s postgres not ready: %w", ep.ID, err)
	}
	client, err := pg.Connect(ctx, ci)
	if err != nil {
		return nil, fmt.Errorf("connect endpoint %s: %w", ep.ID, err)
	}
	if pf != nil {
		e.addExtraPF(pf)
	}
	e.recordExtraEndpoint(ctx, ep, ci)
	ok = true
	return client, nil
}

// Best effort: a nil snap makes AttestPGConnDrop decline for this endpoint
// rather than judge it against another one's pod.
func (e *Env) recordExtraEndpoint(ctx context.Context, ep controller.Endpoint, ci pg.ConnInfo) {
	entry := &extraEndpoint{ep: ep, ci: ci}
	if e.kube != nil {
		snap, err := e.endpointPodStateFor(ctx, ep.ID)
		if err != nil {
			e.log("sharedk8s: pg pod snapshot for endpoint %s not captured: %v", ep.ID, err)
		} else if start, serr := e.postmasterStart(ctx, ci); serr != nil {
			e.log("sharedk8s: postmaster start time for endpoint %s not captured: %v", ep.ID, serr)
		} else {
			snap.postmasterStart = start
			entry.snap = &snap
		}
	}
	e.extraMu.Lock()
	if e.extraPG == nil {
		e.extraPG = map[string]*extraEndpoint{}
	}
	e.extraPG[ci.Addr()] = entry
	e.extraMu.Unlock()
}

func (e *Env) extraEndpointFor(addr string) *extraEndpoint {
	e.extraMu.Lock()
	defer e.extraMu.Unlock()
	return e.extraPG[addr]
}

// PGEndpoint is connection info for the endpoint's compute Postgres.
func (e *Env) PGEndpoint() pg.ConnInfo {
	e.pgMu.Lock()
	defer e.pgMu.Unlock()
	return e.pg
}

// YBStorage is connection info for the backing YugabyteDB's amp storage database
// — the one holding the `amp_storage` schema the data path writes. Tests that
// assert on what actually landed in storage need it; the endpoint's own compute
// Postgres (PGEndpoint) cannot see it.
//
// A backing that lives in a cluster — the built-in yugabyted, a co-located
// YB-in-k8s deploy, a YBA universe — is reached over a port-forward into its own
// namespace; only a backing owned entirely elsewhere is dialled. The result is
// memoized, so co-tenant tests sharing this Env resolve it once. Any forward is
// owned by the Env and closes in Teardown.
func (e *Env) YBStorage(ctx context.Context) (pg.ConnInfo, error) {
	e.storageMu.Lock()
	defer e.storageMu.Unlock()
	if e.storageDone && (e.storagePF == nil || e.storagePF.Alive()) {
		return e.storageCI, e.storageErr
	}
	if e.storageDone {
		e.log("sharedk8s: storage port-forward %s died, re-establishing", e.storageCI.Addr())
	}
	e.storageDone = true
	e.storageCI, e.storageErr = e.resolveStorage(ctx)
	return e.storageCI, e.storageErr
}

// RefreshYBStorage rebuilds the storage forward after the CALLER's connection to
// it was refused, and returns the fresh connection info.
//
// It exists because process liveness is not the test: kubectl keeps running
// while its tunnel is dead — a tserver replaced by a rescheduling or a nemesis
// leaves a forward whose local port refuses every connection — so YBStorage
// alone would hand the same corpse back until the caller's budget ran out. As on
// the endpoint path, what authorises the rebuild is a refused loopback
// connection, which cannot have carried an answer from the database and so
// cannot hide a real fault. A backing that is dialled directly has no forward of
// ours and answers ErrNoStorageForward.
func (e *Env) RefreshYBStorage(ctx context.Context) (pg.ConnInfo, error) {
	e.storageMu.Lock()
	defer e.storageMu.Unlock()
	if !e.storageDone || e.storagePF == nil {
		return pg.ConnInfo{}, ErrNoStorageForward
	}
	// A co-tenant may have rebuilt it while this caller waited for the lock.
	if pgForwardAccepts(ctx, e.storageCI) {
		return e.storageCI, e.storageErr
	}
	old := e.storagePF
	e.storageCI, e.storageErr = e.resolveStorage(ctx)
	if old != nil {
		e.addExtraPF(old) // retired, not closed: a co-tenant may be mid-query on it
	}
	return e.storageCI, e.storageErr
}

// resolveStorage does the work behind YBStorage, caller holding storageMu.
func (e *Env) resolveStorage(ctx context.Context) (pg.ConnInfo, error) {
	e.storagePF = nil
	access, err := e.storageAccess()
	if err != nil {
		return pg.ConnInfo{}, err
	}
	if access.ForwardTarget == "" {
		return access.Conn, nil
	}
	if e.kube == nil {
		return pg.ConnInfo{}, fmt.Errorf("reaching an in-cluster backing store needs a kube context")
	}
	kube := e.kube
	if ns := access.ForwardNamespace; ns != "" {
		kube = kube.InNamespace(ns)
	}
	pf, err := kube.PortForward(ctx, access.ForwardTarget, e.prov.nextLocalPort(), access.RemotePort)
	if err != nil {
		return pg.ConnInfo{}, fmt.Errorf("port-forward backing store %s: %w", access.ForwardTarget, err)
	}
	e.addExtraPF(pf)
	e.storagePF = pf
	ci := access.Conn
	ci.Host, ci.Port = "127.0.0.1", pf.LocalPort()
	return ci, nil
}

// ExecInEndpoint runs a shell command inside the endpoint's amp container and
// returns its stdout. The storage daemons' management HTTP ports are
// container-local in every shape amp ships and are never published, so reaching
// them needs an in-container hop.
func (e *Env) ExecInEndpoint(ctx context.Context, script string) (string, error) {
	if e.kube == nil {
		return "", fmt.Errorf("sharedk8s ExecInEndpoint requires a kube context (endpoint pods are cluster-internal)")
	}
	pod, err := e.computePodName(ctx)
	if err != nil {
		return "", fmt.Errorf("find endpoint %s compute pod: %w", e.endpointID, err)
	}
	res, err := e.kube.ForPod(pod, endpointContainer).Run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("exec in endpoint %s pod %s: %w", e.endpointID, pod, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("exec in endpoint %s pod %s exited %d: %s", e.endpointID, pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// RestartCompute rolls the endpoint's compute pod and blocks until the endpoint
// serves again — the compute-restart capability the restart regression tests
// gate on. It deletes the endpoint's
// compute pod (its workload reschedules a fresh one on the shared external
// YugabyteDB storage), waits for the workload to report ready, then
// re-establishes the Service port-forward (the rolled pod's forward is dead and
// its address changed) so PGEndpoint() points at the live rescheduled pod. Only
// available in context mode: the compute pods are cluster-internal, so a
// direct-URL control plane cannot roll them.
func (e *Env) RestartCompute(ctx context.Context) error {
	if e.kube == nil {
		return fmt.Errorf("sharedk8s RestartCompute requires a kube context (endpoint compute pods are cluster-internal)")
	}
	pod, err := e.computePodName(ctx)
	if err != nil {
		return fmt.Errorf("find endpoint %s compute pod: %w", e.endpointID, err)
	}
	if err := e.kube.Delete(ctx, "pod", pod); err != nil {
		return fmt.Errorf("delete endpoint %s compute pod %s: %w", e.endpointID, pod, err)
	}
	if err := ampstack.WaitEndpointWorkloadReady(ctx, e.kube, e.endpointID, computeRestartReadyTimeout); err != nil {
		return fmt.Errorf("endpoint %s compute did not recover after restart: %w", e.endpointID, err)
	}

	// The rolled pod's Service port-forward is dead; open a fresh one and repoint
	// e.pg at it before waiting on Postgres readiness.
	ci, pf, err := e.prov.resolvePG(ctx, e.kube, controller.Endpoint{ID: e.endpointID, Address: e.endpointAddr})
	if err != nil {
		return fmt.Errorf("re-resolve endpoint %s after restart: %w", e.endpointID, err)
	}
	if err := e.prov.waitPG(ctx, ci, e.prov.cfg.PGReadyTimeout); err != nil {
		if pf != nil {
			_ = pf.Close()
		}
		return fmt.Errorf("endpoint %s postgres not ready after restart: %w", e.endpointID, err)
	}
	e.pgMu.Lock()
	e.swapPG(ci, pf)
	e.pgMu.Unlock()
	e.capturePGPod(ctx)
	return nil
}

// EndpointPodSelector is the label selector matching this env's endpoint pods —
// the app.kubernetes.io/instance label the endpoint's Deployment stamps on them.
// Exported so a test can point a pod-level app (e.g. apps/metrics) at the
// endpoint without knowing how the chart labels it.
func (e *Env) EndpointPodSelector() string {
	return endpointPodSelector(e.endpointID)
}

func endpointPodSelector(id string) string {
	return "app.kubernetes.io/instance=yb-ep-" + id
}

// computePodName returns the endpoint's Running compute pod (falling back to the
// first pod during a reschedule), matched by the app.kubernetes.io/instance
// label the endpoint's Deployment stamps on its pods.
func (e *Env) computePodName(ctx context.Context) (string, error) {
	return e.computePodNameFor(ctx, e.endpointID)
}

func (e *Env) computePodNameFor(ctx context.Context, id string) (string, error) {
	sel := endpointPodSelector(id)
	names, err := e.kube.GetSelector(ctx, "pod", sel, "{.items[*].metadata.name}")
	if err != nil {
		return "", err
	}
	phases, err := e.kube.GetSelector(ctx, "pod", sel, "{.items[*].status.phase}")
	if err != nil {
		return "", err
	}
	nf, pf := strings.Fields(names), strings.Fields(phases)
	for i := range nf {
		if i < len(pf) && pf[i] == "Running" {
			return nf[i], nil
		}
	}
	if len(nf) > 0 {
		return nf[0], nil
	}
	return "", fmt.Errorf("no pod found for endpoint %s (selector %q)", id, sel)
}

// ErrPGForwardAlive says the endpoint's port-forward is up, so a Postgres
// failure through it is the endpoint's, not the harness's. Callers that retry a
// dead forward MUST stop on this and report the original error — recovering
// here would turn a product outage into a passing test.
var ErrPGForwardAlive = errors.New("pg port-forward is alive")

// ErrNoStorageForward is returned when the backing store is reached directly
// rather than through a forward of ours — there is nothing here to rebuild.
var ErrNoStorageForward = errors.New("backing store is dialled directly, not forwarded")

// RefreshPGEndpoint re-establishes the endpoint's port-forward when THIS
// process's kubectl forward has died — the local port then refuses connections
// and every dial fails for reasons that have nothing to do with the endpoint
// (observed 2026-07-28: features/workloadgen_verify burned its whole readiness
// budget on `dial 127.0.0.1: connection refused` while the endpoint itself was
// serving its co-tenant fine).
//
// It is deliberately NOT a retry helper. The caller may only reach it with a
// REFUSED connection to a forward this env owns; an env with no forward answers
// ErrPGForwardAlive. It re-resolves and swaps the forward but does not wait for
// Postgres — the caller owns that, so an endpoint that is genuinely down still
// fails on its own terms, one readiness budget later.
func (e *Env) RefreshPGEndpoint(ctx context.Context) (pg.ConnInfo, error) {
	// One rebuild at a time, and co-tenant callers that queued behind it get the
	// forward it produced. Without this, every co-tenant test that noticed the
	// same dead forward opened its own and closed the others' — the failure they
	// were recovering from, caused by the recovery.
	e.pgMu.Lock()
	defer e.pgMu.Unlock()

	// A direct tailnet/in-cluster address has no forward at all, so a failure
	// there is the endpoint's and nothing here can help.
	if len(e.pfs) == 0 {
		return pg.ConnInfo{}, ErrPGForwardAlive
	}
	// A co-tenant may have rebuilt it while this caller waited for the lock. The
	// local port answering IS the test: this caller's refusal predates the
	// rebuild, so replacing a tunnel that now accepts would cut the neighbour's
	// live connections for nothing.
	if pgForwardAccepts(ctx, e.pg) {
		return e.pg, nil
	}
	// Process liveness is NOT the test. kubectl can keep running while its tunnel
	// is dead — the pod behind a Service forward gets replaced and every later
	// dial of the local port is refused although the child never exits (observed
	// 2026-07-29 on features/pg_delta_chain: "connection refused … port-forward
	// not re-established: pg port-forward is alive"). What authorises the rebuild
	// is the caller's refused connection: a loopback port with nothing listening
	// cannot have carried an answer from the endpoint, so re-establishing it
	// cannot hide a product failure — the caller waits again afterwards and any
	// real fault surfaces then.
	if e.prov == nil {
		return pg.ConnInfo{}, fmt.Errorf("sharedk8s: no provider to re-resolve the endpoint")
	}
	ci, pf, err := e.prov.resolvePG(ctx, e.kube, controller.Endpoint{ID: e.endpointID, Address: e.endpointAddr})
	if err != nil {
		return pg.ConnInfo{}, fmt.Errorf("re-resolve endpoint %s after a dead port-forward: %w", e.endpointID, err)
	}
	e.swapPGRetiring(ci, pf)
	return ci, nil
}

// pgForwardAccepts reports whether something is listening on the endpoint's
// current local address. It is a TCP probe, not a readiness check: it separates
// "this forward is gone" from "someone already replaced it".
func pgForwardAccepts(ctx context.Context, ci pg.ConnInfo) bool {
	if ci.Host == "" || ci.Port == 0 {
		return false
	}
	dctx, cancel := context.WithTimeout(ctx, forwardProbeTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", ci.Addr())
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// forwardProbeTimeout bounds the loopback probe above. It only ever dials
// 127.0.0.1, so it answers immediately or not at all.
const forwardProbeTimeout = 2 * time.Second

// swapPG repoints the Env at a freshly-resolved connection + port-forward and
// closes the superseded one. For a deliberate roll (RestartCompute) the old
// forward's pod is gone and nobody can still be using it. Caller holds pgMu.
func (e *Env) swapPG(ci pg.ConnInfo, pf *k8s.PortForward) {
	old := e.pfs
	e.rememberOwnAddr(ci)
	e.pg = ci
	e.pfs = nonNilPFs(pf)
	closeAll(old)
}

// swapPGRetiring is the same swap for a RECOVERY, where the superseded forward
// is retired rather than closed: one Env serves co-tenant tests, and a
// neighbour may be mid-query on the old local port — killing it turns this
// test's recovery into their failure (the lesson the controller forward already
// learned; see ownCtrl). Retired forwards close with the rest at Teardown.
// Caller holds pgMu.
func (e *Env) swapPGRetiring(ci pg.ConnInfo, pf *k8s.PortForward) {
	old := e.pfs
	e.rememberOwnAddr(ci)
	e.pg = ci
	e.pfs = nonNilPFs(pf)
	for _, o := range old {
		e.addExtraPF(o)
	}
}

// Caller holds pgMu.
func (e *Env) rememberOwnAddr(ci pg.ConnInfo) {
	if e.ownAddrs == nil {
		e.ownAddrs = map[string]bool{}
	}
	if e.pg.Host != "" {
		e.ownAddrs[e.pg.Addr()] = true
	}
	e.ownAddrs[ci.Addr()] = true
}

// ControllerURL is the current controller root URL. It tracks the shared
// forward's live address (which changes if the forward was re-established),
// falling back to the URL captured at provision time.
func (e *Env) ControllerURL() string {
	if e.own != nil {
		return e.controllerURL
	}
	if e.prov != nil {
		if u := e.prov.ctrl.currentURL(); u != "" {
			return u
		}
	}
	return e.controllerURL
}
func (e *Env) Versions() core.DetectedVersions { return e.versions }
func (e *Env) Handles() core.EnvHandles        { return e.handles }

// Exec returns nil: the shared shape drives the product over REST + libpq, not a
// pod shell (chaos, which needs one, runs only on exclusive stacks).
func (e *Env) Exec() exec.RemoteExecutor { return nil }

// Collect gathers the namespace's failure evidence — events, node state, wide
// listings, and every pod's current and previous container logs and describe —
// plus the backing's own namespace when it has one. No-op without a kube
// context; best-effort throughout, so a failed probe never replaces the failure
// it was called to explain.
func (e *Env) Collect(ctx context.Context, into core.ArtifactSink) error {
	if e.kube == nil {
		return nil
	}
	// The controller log keeps its own stable name: it is the first thing every
	// triage reads, and the diagnostics tree buries it per-container.
	if logs, err := e.kube.Logs(ctx, "deployment/"+DefaultControllerService, "", 0); err != nil {
		e.log("sharedk8s collect: controller logs: %v", err)
	} else if err := into.Add("controller/controller.log", "log", []byte(logs)); err != nil {
		return err
	}

	// Everything else comes from the shared collector, which this env used to do
	// without: it logged the CURRENT pod of its OWN endpoint and nothing more. So
	// a compute that crashed and was replaced took its cause with it (no
	// --previous, and the dead pod is gone from the pod list), a pod that never
	// scheduled produced no evidence at all (no events, no describe — run
	// 1c907adb could not tell an unschedulable pod from a crash-looping one), and
	// the endpoints a TEST creates for itself were never collected, which is
	// every multi-endpoint test in the perf and stress suites.
	diag.CollectK8sDiagnostics(ctx, e.kube, e.kube.Namespace(), sinkFn(into.Add), e.logf)

	// The backing YugabyteDB, when it lives in its own namespace: a failure the
	// amp side reports ("writes stopped", "pod not ready") is regularly the
	// backing's, and its pods are not in the amp namespace to be collected above.
	if ns := e.backingNamespace(); ns != "" && ns != e.kube.Namespace() {
		diag.CollectUniverseK8sDiagnostics(ctx, e.kube.InNamespace(ns), ns, sinkFn(into.Add), e.logf)
	}
	return nil
}

// sinkFn adapts core.ArtifactSink's Add to the apps.ArtifactSink the diagnostics
// collector takes (identical method, different package).
type sinkFn func(name, kind string, content []byte) error

func (f sinkFn) Add(name, kind string, content []byte) error { return f(name, kind, content) }

func (e *Env) log(format string, args ...any) {
	if e.logf != nil {
		e.logf(format, args...)
	}
}

// --- helpers ---

func closeAll(pfs []*k8s.PortForward) {
	for _, pf := range pfs {
		if pf != nil {
			_ = pf.Close()
		}
	}
}

// randHex returns 8 hex chars of randomness for a unique owned namespace name.
func randHex() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// firstNonEmptyStr returns the first non-empty argument, or "".
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// nonNilPFs wraps a possibly-nil forward as the Env's port-forward slice.
func nonNilPFs(pf *k8s.PortForward) []*k8s.PortForward {
	if pf == nil {
		return nil
	}
	return []*k8s.PortForward{pf}
}

// isConnRefused reports whether err is a connection-refused-class transport
// error — the symptom of a dead local port-forward.
func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return strings.Contains(err.Error(), "connection refused")
}

// isLoopbackURL reports whether rawURL points at the local loopback interface
// (127.0.0.0/8, ::1, or "localhost") — i.e. a process-local kubectl
// port-forward that no other process can reach.
// urlHost is the bare host of a URL ("" when unparseable or empty).
func urlHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func isLoopbackURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// isAlreadyExists reports whether err is a name-conflict from a concurrent
// create (controller 409 / "conflict" / "already exists").
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already exists") || strings.Contains(s, "conflict") || strings.Contains(s, "409")
}

// splitAddr parses host:port, defaulting the port when absent/unparseable.
func splitAddr(addr string, defPort int) (string, int) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", defPort
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, defPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, defPort
	}
	return host, port
}

// detectYBVersion best-effort live-detects the backing version via SELECT
// version() against the endpoint's compute Postgres, extracting the
// YugabyteDB build YSQL embeds in the response ("PostgreSQL ...-YB-X.Y.Z.W-bN
// ..." — core.ExtractYBVersionMarker) rather than recording the raw prose
// string, so the result is Compare-able by VersionSpec.Qualifies instead of
// silently parsing to garbage. Returns "" on any failure (connect, query, or
// no marker — the compute Postgres is not always a direct YSQL connection) —
// the sentinel Qualifies already treats as "don't enforce this bound".
// detectBackingYB reads the YugabyteDB version from the backing store this
// provider deployed, in the pod when the backing lives in the cluster. "" when
// the stack is attached (its backing is not ours to name) or the probe misses.
// sharedApp is the run-scoped control plane, nil until deployed or in attach mode.
func (p *Provider) sharedApp() *ampstack.App {
	p.ctrl.mu.Lock()
	defer p.ctrl.mu.Unlock()
	if !p.deploy {
		return nil
	}
	return p.app
}

// backingYB asks a deployed stack's backing for its own version marker.
func backingYB(ctx context.Context, app *ampstack.App) string {
	if app == nil {
		return ""
	}
	raw := app.BackingVersion(ctx)
	if raw == "" {
		return ""
	}
	v, ok := core.ExtractYBVersionMarker(raw)
	if !ok {
		return ""
	}
	return v
}

func detectYBVersion(ctx context.Context, ci pg.ConnInfo) string {
	raw, ok := queryVersion(ctx, ci)
	if !ok {
		return ""
	}
	v, ok := core.ExtractYBVersionMarker(raw)
	if !ok {
		return ""
	}
	return v
}

// detectProductVersion best-effort live-detects the amp product build via the
// SAME SELECT version() the compute endpoint answers: there is no amp
// controller version endpoint (checked apps/controller — only /healthz,
// /readyz), so the compute Postgres's own reported version is the only live
// signal reachable. It is accepted only when the raw response is ITSELF a
// bare version (core.LooksLikeVersion) rather than free-form Postgres banner
// text, so a live-detection miss falls back to the configured/static product
// cleanly instead of recording prose. Returns "" on any failure or mismatch.
func detectProductVersion(ctx context.Context, ci pg.ConnInfo) string {
	raw, ok := queryVersion(ctx, ci)
	if !ok || !core.LooksLikeVersion(raw) {
		return ""
	}
	return raw
}

// queryVersion opens a short-lived connection to ci and runs SELECT
// version(), the one live query both detectYBVersion and detectProductVersion
// derive their result from.
func queryVersion(ctx context.Context, ci pg.ConnInfo) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client, err := pg.Connect(cctx, ci)
	if err != nil {
		return "", false
	}
	defer client.Close()
	v, err := client.QueryString(cctx, "SELECT version()")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(v), true
}
