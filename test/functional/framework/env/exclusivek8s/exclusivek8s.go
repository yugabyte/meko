// Package exclusivek8s is the namespace-per-test dedicated-stack provider
// (architecture §5.2): each test gets its own k8s namespace with its own
// YugabyteDB StatefulSet and its own amp controller. The whole control-plane
// bring-up — labelled namespace, pull secret, yugabyted, yb_storage, the
// controller, and the health-checked port-forward — is the apps/ampstack App;
// this provider composes it (one App per namespace) rather than duplicating the
// steps, and adds only the per-test concerns: a unique seeded namespace name,
// lease-friendly local-port allocation, and wrapping the result in a
// core.Environment. Teardown is a single label-guarded namespace delete
// (ampstack refuses any namespace it did not label), which the reaper can also
// perform. kubectl/helm are shelled ambient-authed binaries (KISS, no client-go).
package exclusivek8s

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybdeploy"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// Defaults.
const (
	DefaultNamespacePrefix   = "ampqa"
	DefaultPullSecretName    = "pull-secret"
	DefaultYBImage           = "yugabytedb/yugabyte:latest"
	DefaultYBStorage         = "20Gi"
	DefaultControllerService = "amp-controller"
	DefaultControllerPort    = 8080
	DefaultLocalPortBase     = 18080
	DefaultHelmRelease       = "amp-controller"
)

// Config is the env.exclusivek8s.* configuration.
type Config struct {
	Context           string
	NamespacePrefix   string
	PullSecretB64     string // dockerconfigjson (base64); "" → skip the pull secret
	PullSecretName    string
	YBImage           string
	YBStorage         string
	HelmChart         string // helm chart ref/path; set → deploy via helm
	HelmRelease       string
	HelmValues        []string // extra --set key=value entries
	ControllerImage   string   // convenience full image ref; optional
	ManifestPath      string   // controller manifest applied when HelmChart is unset
	ControllerService string
	ControllerPort    int
	// ControllerReach selects how the per-namespace controller is reached; see
	// apps/ampstack.ControllerReachMode. "" (the default) inherits ampstack's own
	// default (tailnet: direct cluster-internal address, no port-forward).
	ControllerReach ampstack.ControllerReachMode
	LocalPortBase   int
	YBReadyTimeout  time.Duration
	ReadyTimeout    time.Duration
	Product         string
}

// FromConfig reads env.exclusivek8s.* from a framework Config.
func FromConfig(c *core.Config) Config {
	return Config{
		Context:           c.GetString("env.exclusivek8s.context", ""),
		NamespacePrefix:   c.GetString("env.exclusivek8s.namespace_prefix", DefaultNamespacePrefix),
		PullSecretB64:     c.GetString("env.exclusivek8s.pull_secret_b64", ""),
		PullSecretName:    c.GetString("env.exclusivek8s.pull_secret_name", DefaultPullSecretName),
		YBImage:           c.GetString("env.exclusivek8s.yb_image", DefaultYBImage),
		YBStorage:         c.GetString("env.exclusivek8s.yb_storage", DefaultYBStorage),
		HelmChart:         c.GetString("env.exclusivek8s.helm_chart", ""),
		HelmRelease:       c.GetString("env.exclusivek8s.helm_release", DefaultHelmRelease),
		HelmValues:        c.GetStringSlice("env.exclusivek8s.helm_values", nil),
		ControllerImage:   c.GetString("env.exclusivek8s.controller_image", ""),
		ManifestPath:      c.GetString("env.exclusivek8s.controller_manifest", ""),
		ControllerService: c.GetString("env.exclusivek8s.controller_service", DefaultControllerService),
		ControllerPort:    c.GetInt("env.exclusivek8s.controller_port", DefaultControllerPort),
		ControllerReach:   ampstack.ControllerReachMode(c.GetString("env.exclusivek8s.controller_reach", "")),
		LocalPortBase:     c.GetInt("env.exclusivek8s.local_port_base", DefaultLocalPortBase),
		YBReadyTimeout:    c.GetDuration("env.exclusivek8s.yb_ready_timeout", 15*time.Minute),
		ReadyTimeout:      c.GetDuration("env.exclusivek8s.controller_ready_timeout", 5*time.Minute),
		Product:           c.GetString("versions.product", ""),
	}
}

// Configured reports whether a target cluster context is set.
func (c Config) Configured() bool { return c.Context != "" }

// Provider provisions namespace-per-test dedicated stacks by composing ampstack.
type Provider struct {
	cfg     Config
	kube    *k8s.Kubectl
	helmRun k8s.Runner
	logf    func(format string, args ...any)

	// Injected in tests; defaults wired in New. newClient/waitReady are handed
	// to each per-namespace ampstack App; readManifest reads the controller
	// manifest file for the no-chart path. detectYB live-detects the stack's
	// own backing YugabyteDB version once Deploy succeeds.
	newClient    func(url string) *controller.Client
	waitReady    func(ctx context.Context, cl *controller.Client, timeout time.Duration) error
	readManifest func(path string) (string, error)
	detectYB     func(ctx context.Context, ci pg.ConnInfo) string

	// ybDeployCfg is the raw framework config, injected via WithYBDeployConfig so
	// the per-test deploy selector (apps/ybdeploy) can read env.yb_deploy and
	// the sub-app configs. nil → the built-in yugabyted (no external backing).
	ybDeployCfg *core.Config

	portCounter atomic.Int64
	nsCounter   atomic.Int64
}

// WithYBDeployConfig injects the raw framework config the per-test deploy
// selector reads (env.yb_deploy plus the yb_k8s/yba sub-app knobs). Without it the
// provider always uses ampstack's built-in yugabyted.
func (p *Provider) WithYBDeployConfig(c *core.Config) *Provider {
	p.ybDeployCfg = c
	return p
}

// New builds a Provider from cfg.
func New(cfg Config) *Provider {
	applyDefaults(&cfg)
	p := &Provider{
		cfg:          cfg,
		kube:         k8s.New(k8s.Config{Context: cfg.Context}),
		helmRun:      k8s.OSRun,
		logf:         log.Printf,
		newClient:    func(url string) *controller.Client { return controller.New(url) },
		readManifest: readFile,
		detectYB:     detectYBVersion,
	}
	return p
}

func applyDefaults(cfg *Config) {
	if cfg.NamespacePrefix == "" {
		cfg.NamespacePrefix = DefaultNamespacePrefix
	}
	if cfg.PullSecretName == "" {
		cfg.PullSecretName = DefaultPullSecretName
	}
	if cfg.YBImage == "" {
		cfg.YBImage = DefaultYBImage
	}
	if cfg.YBStorage == "" {
		cfg.YBStorage = DefaultYBStorage
	}
	if cfg.HelmRelease == "" {
		cfg.HelmRelease = DefaultHelmRelease
	}
	if cfg.ControllerService == "" {
		cfg.ControllerService = DefaultControllerService
	}
	if cfg.ControllerPort == 0 {
		cfg.ControllerPort = DefaultControllerPort
	}
	if cfg.LocalPortBase == 0 {
		cfg.LocalPortBase = DefaultLocalPortBase
	}
	if cfg.YBReadyTimeout == 0 {
		cfg.YBReadyTimeout = 15 * time.Minute
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 5 * time.Minute
	}
}

func (p *Provider) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Provision stands up a dedicated stack in a fresh namespace (via ampstack) and
// returns an environment whose controller is reachable via a port-forward.
func (p *Provider) Provision(ctx context.Context, spec core.EnvSpec, seed *core.Seed) (core.Environment, error) {
	ns := fmt.Sprintf("%s-%x-%d", p.cfg.NamespacePrefix, uint64(seed.Value()), p.nsCounter.Add(1))
	nsKube := k8s.New(k8s.Config{Context: p.cfg.Context, Namespace: ns}).
		WithRunner(p.kube.Runner()).WithStarter(p.kube.StarterFn())

	ampCfg, err := p.ampstackConfig(ns)
	if err != nil {
		return nil, err
	}

	// Per-test deploy selection: the env.yb_deploy knob (or the test's preference)
	// may swap ampstack's built-in yugabyted for an external cluster this call
	// provisions. Its teardown chains into the env's Teardown (amp first, backing
	// after — LIFO).
	var ybDeployTeardown func(context.Context) error
	if p.ybDeployCfg != nil {
		mode := ybdeploy.Resolve(p.ybDeployCfg, spec)
		if mode != ybdeploy.ModeBuiltin {
			store, td, berr := ybdeploy.Provision(ctx, p.ybDeployCfg, mode, ns, p.logf)
			if berr != nil {
				return nil, fmt.Errorf("provision %s deployment: %w", mode, berr)
			}
			if store != nil {
				ampCfg.Backing = ybbacking.ExternalBacking(store)
			}
			ybDeployTeardown = td
		}
	}

	app := ampstack.New(ampCfg).
		WithKube(nsKube).
		WithHelmRunner(p.helmRun).
		WithClientFactory(p.newClient).
		WithLogf(p.logf)
	if p.waitReady != nil {
		app.WithReadyFunc(p.waitReady)
	}

	info, err := app.Deploy(ctx)
	if err != nil {
		_ = app.Teardown(context.Background())
		if ybDeployTeardown != nil {
			_ = ybDeployTeardown(context.Background())
		}
		return nil, err
	}

	// Live-detect the dedicated stack's own backing YugabyteDB version now that
	// it is up (architecture §4.9: read from the live environment, not assumed
	// from config). "" on any failure — Qualifies' documented "don't enforce
	// this bound" sentinel — never blocks the run.
	yb := p.detectYB(ctx, app.BackingConnInfo())
	p.log("exclusivek8s: namespace %s backing YB version detected=%q (live=%v)", ns, yb, yb != "")

	return &Env{
		app:              app,
		controllerURL:    info.ControllerURL,
		versions:         core.DetectedVersions{Product: p.cfg.Product, YB: yb},
		namespace:        ns,
		handles:          core.EnvHandles{Namespace: ns},
		logf:             p.logf,
		ybDeployTeardown: ybDeployTeardown,
	}, nil
}

// ampstackConfig translates the provider's per-test config into an ampstack
// Config for one namespace. exclusivek8s owns its own namespace, so it keeps the
// chart-default cluster-scoped RBAC and no helm shim; it never mints an ECR
// secret (ECRRegistry left empty), applying only a pre-encoded pull secret when
// one is configured, matching the pre-composition behavior.
func (p *Provider) ampstackConfig(ns string) (ampstack.Config, error) {
	local := p.cfg.LocalPortBase + int(p.portCounter.Add(1)-1)
	cfg := ampstack.Config{
		Context:                   p.cfg.Context,
		Namespace:                 ns,
		ControllerChart:           p.cfg.HelmChart,
		ControllerImage:           p.cfg.ControllerImage,
		ExtraHelmSet:              p.cfg.HelmValues,
		YBImage:                   p.cfg.YBImage,
		YBStorage:                 p.cfg.YBStorage,
		RBACClusterScoped:         true,
		HelmShim:                  false,
		ECRRegistry:               "", // exclusivek8s uses a pre-encoded secret, never mints via ECR
		PullSecretName:            p.cfg.PullSecretName,
		PullSecretDockerConfigB64: p.cfg.PullSecretB64,
		ControllerService:         p.cfg.ControllerService,
		ControllerPort:            p.cfg.ControllerPort,
		ControllerReach:           p.cfg.ControllerReach,
		HelmRelease:               p.cfg.HelmRelease,
		LocalPort:                 local,
		YBReadyTimeout:            p.cfg.YBReadyTimeout,
		ControllerReadyTimeout:    p.cfg.ReadyTimeout,
		ExtraNamespaceLabels:      map[string]string{"ampqa-created": strconv.FormatInt(time.Now().UTC().Unix(), 10)},
	}
	if p.cfg.HelmChart == "" && p.cfg.ManifestPath != "" {
		m, err := p.readManifest(p.cfg.ManifestPath)
		if err != nil {
			return ampstack.Config{}, fmt.Errorf("read controller manifest %s: %w", p.cfg.ManifestPath, err)
		}
		cfg.ControllerManifest = m
	}
	return cfg, nil
}

// Teardown deletes the namespace (cascading the whole stack) and releases the
// port-forward, delegating both to the composed ampstack App.
func (p *Provider) Teardown(ctx context.Context, env core.Environment) error {
	e, ok := env.(*Env)
	if !ok {
		return fmt.Errorf("exclusivek8s teardown: unexpected environment type %T", env)
	}
	_ = e.app.Close(ctx)
	// LIFO: the amp stack (provisioned last) tears down first, then its backing.
	ampErr := e.app.Teardown(ctx)
	if ampErr != nil {
		p.log("exclusivek8s: teardown namespace %s failed: %v", e.namespace, ampErr)
	}
	if e.ybDeployTeardown != nil {
		if berr := e.ybDeployTeardown(ctx); berr != nil {
			p.log("exclusivek8s: teardown backing for %s failed: %v", e.namespace, berr)
			if ampErr == nil {
				ampErr = berr
			}
		}
	}
	return ampErr
}

// Attach is not supported: a dedicated stack is provisioned per test, and remote
// attach lives in env/remote.
func (p *Provider) Attach(ctx context.Context, ref string) (core.Environment, error) {
	return nil, fmt.Errorf("exclusivek8s does not support attach; use a remote/attach provider")
}

// Env is a live dedicated-stack environment backed by an ampstack App.
type Env struct {
	app           *ampstack.App
	controllerURL string
	versions      core.DetectedVersions
	namespace     string
	handles       core.EnvHandles
	logf          func(format string, args ...any)
	// ybDeployTeardown reaps an externally-provisioned deployment (nil for the
	// built-in yugabyted); run AFTER the amp stack in Teardown (LIFO).
	ybDeployTeardown func(context.Context) error
}

func (e *Env) ControllerURL() string           { return e.controllerURL }
func (e *Env) Versions() core.DetectedVersions { return e.versions }
func (e *Env) Handles() core.EnvHandles        { return e.handles }
func (e *Env) Exec() exec.RemoteExecutor       { return nil }

// Collect gathers the stack's controller + yugabyted logs (via the App).
func (e *Env) Collect(ctx context.Context, into core.ArtifactSink) error {
	return e.app.Collect(ctx, into)
}

// detectYBVersion best-effort live-detects the backing version via SELECT
// version(), extracting the YugabyteDB build YSQL embeds in the response
// ("PostgreSQL ...-YB-X.Y.Z.W-bN ..." — core.ExtractYBVersionMarker) rather
// than recording the raw prose string, so the result is Compare-able by
// VersionSpec.Qualifies instead of silently parsing to garbage. Returns "" on
// any failure (connect, query, or no marker) — the sentinel Qualifies already
// treats as "don't enforce this bound".
func detectYBVersion(ctx context.Context, ci pg.ConnInfo) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client, err := pg.Connect(cctx, ci)
	if err != nil {
		return ""
	}
	defer client.Close()
	raw, err := client.QueryString(cctx, "SELECT version()")
	if err != nil {
		return ""
	}
	v, ok := core.ExtractYBVersionMarker(raw)
	if !ok {
		return ""
	}
	return v
}
