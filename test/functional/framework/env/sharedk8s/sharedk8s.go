// Package sharedk8s is the project-per-test environment provider (architecture
// §5.1): it drives a standing amp control plane over the controller REST API to
// carve out an isolated project + endpoint for each test, and tears the project
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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec/k8s"
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
	// computeRestartReadyTimeout bounds waiting for the endpoint's Deployment to
	// report Available again after its compute pod is rolled.
	computeRestartReadyTimeout = 5 * time.Minute
	// provisionRetries is how many extra attempts project/endpoint creation makes
	// on a transient controller 5xx (concurrent-DDL deadlock, endpoint helm race).
	provisionRetries = 4
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
	Product           string // recorded as the product version (no version endpoint exists)

	// Deploy selects self-building k8s: when true this provider OWNS one
	// run-scoped amp control plane, deploying the same ampstack.App exclusivek8s
	// builds into an owned ampqa-labelled namespace and carving projects out of
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
		ControllerURL:     controllerURL,
		Context:           ctx,
		Namespace:         namespace,
		ControllerService: c.GetString("env.sharedk8s.controller_service", DefaultControllerService),
		ControllerPort:    c.GetInt("env.sharedk8s.controller_port", DefaultControllerPort),
		EndpointPort:      c.GetInt("env.sharedk8s.endpoint_port", DefaultEndpointPort),
		LocalPortBase:     c.GetInt("env.sharedk8s.local_port_base", DefaultLocalPortBase),
		UserID:            c.GetString("env.sharedk8s.user_id", ""),
		PoolName:          c.GetString("env.sharedk8s.pool_name", DefaultPoolName),
		PoolID:            c.GetString("env.sharedk8s.pool_id", ""),
		ReadyTimeout:      c.GetDuration("env.sharedk8s.controller_ready_timeout", 2*time.Minute),
		PGReadyTimeout:    c.GetDuration("env.sharedk8s.pg_ready_timeout", 3*time.Minute),
		Product:           c.GetString("versions.product", ""),
		Deploy:            deploy,
		DeployTimeout:     c.GetDuration("env.sharedk8s.deploy_timeout", amp.YBReadyTimeout+amp.ControllerReadyTimeout+2*time.Minute),
		AmpStack:          amp,
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

// Provider provisions project-per-test environments on a shared control plane.
type Provider struct {
	cfg  Config
	kube *k8s.Kubectl
	logf func(format string, args ...any)

	// Injected in tests; defaults wired in New.
	newClient     func(url string) *controller.Client
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

	ctrl controllerConn // one long-lived controller forward shared by all tests

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
	cc.cl = p.newClient(cc.url)
	return cc.cl, cc.url, nil
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

// Prepare eagerly deploys the run-scoped control plane (deploy mode) so it is up
// BEFORE the scheduler starts provisioning per-test projects — otherwise the
// first test's short provision deadline would have to absorb the whole
// control-plane bring-up. A no-op in attach mode. The `run` command calls this
// (via an optional-interface assertion) before sched.Run.
func (p *Provider) Prepare(ctx context.Context) error {
	if !p.deploy {
		return nil
	}
	_, _, err := p.ctrl.get(ctx)
	return err
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
	if p.deploy && p.app != nil {
		bg := context.Background()
		_ = p.app.Close(bg)
		return p.app.Teardown(bg)
	}
	p.ctrl.close()
	return nil
}

func defaultNewClient(url string) *controller.Client {
	return controller.New(url)
}

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

// Provision creates a project + endpoint on the shared control plane and returns
// a live environment bound to the endpoint's Postgres.
func (p *Provider) Provision(ctx context.Context, spec core.EnvSpec, seed *core.Seed) (core.Environment, error) {
	cl, url, err := p.ctrl.get(ctx)
	if err != nil {
		return nil, err
	}
	var epPF *k8s.PortForward // per-test pg forward, owned by the Env once returned
	fail := func(e error) (core.Environment, error) {
		if epPF != nil {
			_ = epPF.Close()
		}
		// A dead shared controller forward mid-provision otherwise surfaces only
		// as opaque connection-refused API errors; name it explicitly.
		if !p.ctrl.alive() {
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

	proj, branchID, err := p.createProject(ctx, cl, poolID, seed)
	if err != nil {
		return fail(err)
	}

	ep, err := p.createEndpoint(ctx, cl, poolID, proj.ID, branchID)
	if err != nil {
		return fail(err)
	}

	ci, pf, err := p.resolvePG(ctx, ep)
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
	yb := p.detectYB(ctx, ci)
	p.log("sharedk8s: versions detected — product=%q (live=%v) yb=%q (live=%v)", product, productLive, yb, yb != "")
	env := &Env{
		controllerURL: url,
		versions:      core.DetectedVersions{Product: product, YB: yb},
		pg:            ci,
		prov:          p,
		poolID:        poolID,
		projectID:     proj.ID,
		endpointID:    ep.ID,
		endpointAddr:  ep.Address,
		ownProject:    true,
		pfs:           nonNilPFs(epPF),
		kube:          p.kube,
		logf:          p.logf,
		handles: core.EnvHandles{
			Namespace: p.cfg.Namespace,
			Project:   proj.ID,
			Endpoint:  ep.ID,
		},
	}
	return env, nil
}

// Attach treats ref as a project id on the configured pool: it reads the project
// and its first endpoint and returns an environment that will NOT delete the
// project on teardown.
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
	proj, err := cl.GetProject(ctx, poolID, ref)
	if err != nil {
		return fail(fmt.Errorf("attach project %s: %w", ref, err))
	}
	branchID := proj.DefaultBranch.BranchID
	eps, err := cl.ListAllEndpoints(ctx, poolID, proj.ID, branchID)
	if err != nil {
		return fail(err)
	}
	if len(eps) == 0 {
		return fail(fmt.Errorf("attach project %s: no endpoints on default branch", ref))
	}
	ci, pf, err := p.resolvePG(ctx, eps[0])
	if err != nil {
		return fail(err)
	}
	epPF = pf
	product := p.cfg.Product
	if v := p.detectProduct(ctx, ci); v != "" {
		product = v
	}
	return &Env{
		controllerURL: url,
		versions:      core.DetectedVersions{Product: product, YB: p.detectYB(ctx, ci)},
		pg:            ci,
		prov:          p,
		poolID:        poolID,
		projectID:     proj.ID,
		endpointID:    eps[0].ID,
		endpointAddr:  eps[0].Address,
		ownProject:    false,
		pfs:           nonNilPFs(epPF),
		kube:          p.kube,
		logf:          p.logf,
		handles:       core.EnvHandles{Namespace: p.cfg.Namespace, Project: proj.ID, Endpoint: eps[0].ID},
	}, nil
}

// Teardown deletes the per-test project (cascade) and closes the per-test pg
// port-forward. The shared controller forward is provider-owned and left up.
func (p *Provider) Teardown(ctx context.Context, env core.Environment) error {
	e, ok := env.(*Env)
	if !ok {
		return fmt.Errorf("sharedk8s teardown: unexpected environment type %T", env)
	}
	var firstErr error
	if e.ownProject {
		if err := p.deleteProject(ctx, e); err != nil {
			firstErr = err
			p.log("sharedk8s: teardown of project %s failed: %v", e.projectID, err)
		}
	}
	closeAll(e.pfs)
	closeAll(e.takeExtraPFs())
	return firstErr
}

// deleteProject cascade-deletes the project over the shared controller forward,
// health-checking it first (get re-establishes a dead forward) and retrying once
// after a forced reconnect when the delete fails connection-refused — the
// signature of a forward that died between provisioning and teardown.
func (p *Provider) deleteProject(ctx context.Context, e *Env) error {
	cl, _, err := p.ctrl.get(ctx)
	if err != nil {
		return fmt.Errorf("delete project %s: controller unavailable: %w", e.projectID, err)
	}
	if _, err := cl.DeleteProjectAndWait(ctx, e.poolID, e.projectID); err == nil {
		return nil
	} else if !isConnRefused(err) {
		return fmt.Errorf("delete project %s: %w", e.projectID, err)
	}
	p.ctrl.reset()
	cl, _, err = p.ctrl.get(ctx)
	if err != nil {
		return fmt.Errorf("delete project %s: reconnect controller: %w", e.projectID, err)
	}
	if _, err := cl.DeleteProjectAndWait(ctx, e.poolID, e.projectID); err != nil {
		return fmt.Errorf("delete project %s (after reconnect): %w", e.projectID, err)
	}
	return nil
}

func (p *Provider) waitControllerReady(ctx context.Context, cl *controller.Client) error {
	wctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
	defer cancel()
	var last error
	for {
		if last = cl.Readyz(wctx); last == nil {
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("controller not ready: %w (last: %v)", wctx.Err(), last)
		case <-time.After(time.Second):
		}
	}
}

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

// createProject creates a project (with its default branch) on the control
// plane and returns it plus the default branch id. The amp controller
// occasionally answers a concurrent create with a transient 5xx — most often a
// deadlock in its own backing DB when several projects' branch recipes are
// recorded at once (deploy mode starts every parallel test provisioning at once)
// — so it retries with backoff and a FRESH project name each attempt (a failed
// create can leave the old name half-populated).
func (p *Provider) createProject(ctx context.Context, cl *controller.Client, poolID string, seed *core.Seed) (controller.Project, string, error) {
	var lastErr error
	for attempt := 0; attempt <= provisionRetries; attempt++ {
		if attempt > 0 && !backoff(ctx, attempt) {
			return controller.Project{}, "", fmt.Errorf("create project: %w (last: %v)", ctx.Err(), lastErr)
		}
		name := fmt.Sprintf("ampqa-p-%x-%d", uint64(seed.Value()), p.nameCounter.Add(1))
		if _, err := cl.CreateProjectAndWait(ctx, poolID, controller.CreateProjectRequest{Name: name}); err != nil {
			lastErr = fmt.Errorf("create project %s: %w", name, err)
			if transientController(err) {
				p.log("sharedk8s: %v — retrying with a fresh name (attempt %d/%d)", lastErr, attempt+1, provisionRetries)
				continue
			}
			return controller.Project{}, "", lastErr
		}
		proj, err := p.findProject(ctx, cl, poolID, name)
		if err != nil {
			return controller.Project{}, "", err
		}
		return proj, proj.DefaultBranch.BranchID, nil
	}
	return controller.Project{}, "", lastErr
}

// createEndpoint returns a live read_write endpoint on the branch. The
// controller's create_endpoint is not idempotent: an endpoint helm-deploy race
// ("services yb-ep-… not found") can leave a partial endpoint RECORD that a
// re-create then rejects as a conflict. So each attempt first REUSES any
// existing read_write endpoint on the branch (the racing deploy usually
// finishes moments later, and the downstream Postgres-readiness wait validates
// it), and only creates one when none exists — retrying on a transient 5xx or a
// record conflict.
func (p *Provider) createEndpoint(ctx context.Context, cl *controller.Client, poolID, projectID, branchID string) (controller.Endpoint, error) {
	var lastErr error
	for attempt := 0; attempt <= provisionRetries; attempt++ {
		if attempt > 0 {
			if !backoff(ctx, attempt) {
				return controller.Endpoint{}, fmt.Errorf("create endpoint: %w (last: %v)", ctx.Err(), lastErr)
			}
			// A prior attempt left a partial endpoint record (the non-idempotent
			// create + helm race); reuse it rather than re-create into a conflict.
			if ep, ok := p.existingEndpoint(ctx, cl, poolID, projectID, branchID); ok {
				return ep, nil
			}
		}
		if _, err := cl.CreateEndpointAndWait(ctx, poolID, projectID, branchID, controller.CreateEndpointRequest{Role: "read_write"}); err != nil {
			lastErr = fmt.Errorf("create endpoint: %w", err)
			if transientController(err) || isAlreadyExists(err) {
				p.log("sharedk8s: %v — reusing/retrying endpoint (attempt %d/%d)", lastErr, attempt+1, provisionRetries)
				continue
			}
			return controller.Endpoint{}, lastErr
		}
		return p.pickEndpoint(ctx, cl, poolID, projectID, branchID)
	}
	return controller.Endpoint{}, lastErr
}

// existingEndpoint returns a read_write endpoint already present on the branch,
// or ok=false when none exists (list errors are treated as "none", so the caller
// proceeds to create).
func (p *Provider) existingEndpoint(ctx context.Context, cl *controller.Client, poolID, projectID, branchID string) (controller.Endpoint, bool) {
	eps, err := cl.ListAllEndpoints(ctx, poolID, projectID, branchID)
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
// a backing-DB deadlock during concurrent project/branch creation, or the
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

func (p *Provider) findProject(ctx context.Context, cl *controller.Client, poolID, name string) (controller.Project, error) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListProjects(ctx, poolID, page, offset)
		if err != nil {
			return controller.Project{}, fmt.Errorf("list projects: %w", err)
		}
		for _, proj := range list.Projects {
			if proj.Name == name {
				return proj, nil
			}
		}
		if list.Count < page || offset+list.Count >= list.Total {
			return controller.Project{}, fmt.Errorf("created project %q not found", name)
		}
	}
}

// pickEndpoint returns the branch's read_write endpoint (falling back to the
// first).
func (p *Provider) pickEndpoint(ctx context.Context, cl *controller.Client, poolID, projectID, branchID string) (controller.Endpoint, error) {
	eps, err := cl.ListAllEndpoints(ctx, poolID, projectID, branchID)
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
func (p *Provider) resolvePG(ctx context.Context, ep controller.Endpoint) (pg.ConnInfo, *k8s.PortForward, error) {
	host, port := splitAddr(ep.Address, p.cfg.EndpointPort)
	ci := pg.ConnInfo{User: computeUser, Database: computeDB}
	if p.kube == nil {
		ci.Host, ci.Port = host, port
		return ci, nil, nil
	}
	local := p.nextLocalPort()
	pf, err := p.kube.PortForward(ctx, "svc/yb-ep-"+ep.ID, local, port)
	if err != nil {
		return pg.ConnInfo{}, nil, fmt.Errorf("port-forward endpoint %s: %w", ep.ID, err)
	}
	ci.Host, ci.Port = "127.0.0.1", pf.LocalPort()
	return ci, pf, nil
}

// Env is a live shared-control-plane environment (one project + endpoint).
type Env struct {
	controllerURL string
	versions      core.DetectedVersions
	pg            pg.ConnInfo
	handles       core.EnvHandles

	prov         *Provider
	poolID       string
	projectID    string
	endpointID   string
	endpointAddr string // controller-reported endpoint address; re-resolved after a compute restart
	ownProject   bool
	pfs          []*k8s.PortForward
	kube         *k8s.Kubectl
	logf         func(format string, args ...any)

	// extraPFs are per-endpoint pg forwards opened by ConnectEndpoint (the
	// functional amp-feature flow attaching to this standing control plane creates
	// several endpoints of its own). Guarded because co-tenant tests share one Env
	// and call ConnectEndpoint concurrently; all are closed in Teardown.
	extraMu  sync.Mutex
	extraPFs []*k8s.PortForward
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
	cl, _, err := e.prov.ctrl.get(ctx)
	return cl, err
}

// AmpPoolID is the storage pool the provider resolved/created on the standing
// control plane; an attaching test creates its project-per-test on it.
func (e *Env) AmpPoolID() string { return e.poolID }

// AmpKube is the kubectl scoped to the control-plane namespace, where endpoint
// compute pods live — the compute-restart feature rolls pods through it. Nil when
// the controller is reached by a direct URL (no context), in which case that
// feature cannot run.
func (e *Env) AmpKube() *k8s.Kubectl { return e.kube }

// ConnectEndpoint opens a pooled SQL connection to an amp endpoint on the standing
// control plane, port-forwarding the endpoint Service through kubectl when it is
// cluster-internal (context mode). The forward is owned by the Env (recorded and
// closed in Teardown), bounded by the handful of endpoints a feature test creates,
// so callers close only the returned client. It waits for the endpoint's Postgres
// to accept connections first (a freshly-created endpoint's compute may still be
// starting); the wait is bounded by the caller's ctx, so a probe against a retired
// endpoint (Service gone) fails fast rather than blocking.
func (e *Env) ConnectEndpoint(ctx context.Context, ep controller.Endpoint) (*pg.Client, error) {
	ci, pf, err := e.prov.resolvePG(ctx, ep)
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
	ok = true
	return client, nil
}

// PGAccessor lets pg-based tests reach the endpoint (docker env's convention).
func (e *Env) PGEndpoint() pg.ConnInfo { return e.pg }

// RestartCompute rolls the endpoint's compute pod and blocks until the endpoint
// serves again — the same compute-restart capability the docker env provides, so
// the restart regression tests run unchanged here. It deletes the endpoint's
// compute pod (its Deployment reschedules a fresh one on the shared external
// YugabyteDB storage), waits for the Deployment to become Available, then
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
	deploy := "deployment/yb-ep-" + e.endpointID
	if err := e.kube.WaitFor(ctx, deploy, "condition=Available", computeRestartReadyTimeout); err != nil {
		return fmt.Errorf("endpoint %s compute did not recover after restart: %w", e.endpointID, err)
	}

	// The rolled pod's Service port-forward is dead; open a fresh one and repoint
	// e.pg at it before waiting on Postgres readiness.
	ci, pf, err := e.prov.resolvePG(ctx, controller.Endpoint{ID: e.endpointID, Address: e.endpointAddr})
	if err != nil {
		return fmt.Errorf("re-resolve endpoint %s after restart: %w", e.endpointID, err)
	}
	if err := e.prov.waitPG(ctx, ci, e.prov.cfg.PGReadyTimeout); err != nil {
		if pf != nil {
			_ = pf.Close()
		}
		return fmt.Errorf("endpoint %s postgres not ready after restart: %w", e.endpointID, err)
	}
	e.swapPG(ci, pf)
	return nil
}

// computePodName returns the endpoint's Running compute pod (falling back to the
// first pod during a reschedule), matched by the app.kubernetes.io/instance
// label the endpoint's Deployment stamps on its pods.
func (e *Env) computePodName(ctx context.Context) (string, error) {
	sel := "app.kubernetes.io/instance=yb-ep-" + e.endpointID
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
	return "", fmt.Errorf("no pod found for endpoint %s (selector %q)", e.endpointID, sel)
}

// swapPG repoints the Env at a freshly-resolved connection + port-forward and
// closes the superseded forward(s).
func (e *Env) swapPG(ci pg.ConnInfo, pf *k8s.PortForward) {
	old := e.pfs
	e.pg = ci
	e.pfs = nonNilPFs(pf)
	closeAll(old)
}

// ControllerURL is the current controller root URL. It tracks the shared
// forward's live address (which changes if the forward was re-established),
// falling back to the URL captured at provision time.
func (e *Env) ControllerURL() string {
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

// Collect gathers controller and endpoint pod logs when a kube context is
// configured; otherwise it is a no-op. Best-effort — failures are logged.
func (e *Env) Collect(ctx context.Context, into core.ArtifactSink) error {
	if e.kube == nil {
		return nil
	}
	if logs, err := e.kube.Logs(ctx, "deployment/"+DefaultControllerService, "", 0); err != nil {
		e.log("sharedk8s collect: controller logs: %v", err)
	} else if err := into.Add("controller/controller.log", "log", []byte(logs)); err != nil {
		return err
	}
	pods, err := e.kube.ListPods(ctx)
	if err != nil {
		e.log("sharedk8s collect: list pods: %v", err)
		return nil
	}
	for _, pod := range pods {
		if !strings.Contains(pod, "yb-ep-"+e.endpointID) {
			continue
		}
		logs, err := e.kube.Logs(ctx, pod, "", 0)
		if err != nil {
			e.log("sharedk8s collect: endpoint logs %s: %v", pod, err)
			continue
		}
		if err := into.Add(pod+"/endpoint.log", "log", []byte(logs)); err != nil {
			return err
		}
	}
	return nil
}

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
