// Package ampstack is the amp control-plane App: it stands up (and tears down)
// a full amp control plane inside a single Kubernetes namespace, the way
// deploy/up.sh does by hand — a labelled namespace, an ECR image-pull secret, a
// single-node yugabyted StatefulSet, the yb_storage database, and the amp
// controller (rendered from the yb-amp-controller Helm chart and applied) —
// then health-checks it.
//
// The controller is reached in one of two ways (ControllerReach):
//   - tailnet (the default): resolve the controller's cluster-internal address
//     (its Service ClusterIP, falling back to its pod IP) and talk to it
//     directly — no kubectl port-forward, so there is no long-lived local
//     process that can die mid-test. This exists because kubectl port-forward
//     to the controller proved fundamentally flaky over a long-running test
//     (dying during the long native workload and even early in the amp compute
//     phase) even after a reconnect-on-death fix; cluster-internal addresses are
//     reachable directly from this coordinator over the Yugabyte tailnet — the
//     same way the amp endpoint's address already is (see foundation's
//     endpointConnInfo, unchanged).
//   - portforward: the historical kubectl port-forward path (held from Deploy
//     through Teardown/Close, reconnected on death), kept as an explicit
//     fallback for clusters where the controller's cluster-internal address
//     isn't reachable from the coordinator.
//
// It exists so the control-plane lifecycle is an App like everything else the
// framework drives, not raw kubectl/helm in an ad-hoc script: tasks
// (deploy-amp/status-amp/teardown-amp) and the exclusivek8s env provider both
// compose it rather than duplicating the bring-up. kubectl/helm are shelled
// ambient-authed binaries (KISS, no client-go), reached through
// exec/k8s.Kubectl.
//
// Namespace-scoped by design: with RBACClusterScoped=false NO cluster-scoped
// object (ClusterRole/ClusterRoleBinding) is ever created — those docs are both
// gated off in the chart and filtered out of the rendered manifest here — so
// the controller runs with only namespace RBAC on a shared cluster. Because the
// currently-deployed controller image predates the fix that stopped passing
// helm --create-namespace, HelmShim (default on) installs a tiny PATH-wrapper
// that strips that flag until a controller image built without it ships.
package ampstack

import (
	"context"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/controller"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/system/framework/exec/k8s"
)

// Defaults.
const (
	DefaultPullSecretName      = "pull-secret"
	DefaultYBImage             = "yugabytedb/yugabyte:latest"
	DefaultYBStorage           = "20Gi"
	DefaultControllerService   = "amp-controller"
	DefaultControllerName      = "amp-controller"
	DefaultControllerContainer = "controller"
	DefaultControllerPort      = 8080
	DefaultHelmRelease         = "yb-amp-controller"
	DefaultDBName              = "yb_storage"
	DefaultECRRegistry         = "454529406029.dkr.ecr.us-west-2.amazonaws.com"
	DefaultRegion              = "us-west-2"
	DefaultControllerImage     = "amp/controller:tailnet-mig0010"
	DefaultEndpointImage       = "amp/endpoint:phase1-20260703-154251"
	DefaultEndpointServiceType = "ClusterIP"
	DefaultControllerExpose    = "none"
	DefaultBackingUser         = "yugabyte"
	DefaultBackingSSLMode      = "disable"

	// ybMaintenanceDB is the always-present database the external-backing yb_storage
	// CREATE DATABASE connects to (yb_storage may not exist yet).
	ybMaintenanceDB = "yugabyte"

	// controllerProbeRetries/controllerProbeDelay bound the quick readiness
	// check resolveControllerAddr runs against a candidate direct address: just
	// enough to ride out the brief gap between the Deployment reporting
	// Available and the process actually listening, not a substitute for the
	// full waitReady poll that follows.
	controllerProbeRetries = 3
	controllerProbeDelay   = 200 * time.Millisecond
)

// ControllerReachMode selects how the App (and its ControllerClient) reaches
// the deployed controller.
type ControllerReachMode string

const (
	// ReachTailnet (the default) resolves the controller's stable
	// cluster-internal address (Service ClusterIP, falling back to the pod IP)
	// and talks to it directly — no kubectl port-forward.
	ReachTailnet ControllerReachMode = "tailnet"
	// ReachPortForward keeps the historical kubectl port-forward path (held
	// from Deploy through Teardown/Close, reconnected on death).
	ReachPortForward ControllerReachMode = "portforward"
)

// BackingMode selects where the amp control plane's YugabyteDB backing store
// lives.
type BackingMode string

const (
	// BackingBuiltinYugabyted (the default) has ampstack deploy its own
	// single-node yugabyted StatefulSet in-namespace and wire the controller at
	// it — the historical behaviour.
	BackingBuiltinYugabyted BackingMode = "builtin_yugabyted"
	// BackingExternal points the controller + endpoints at a YugabyteDB owned
	// elsewhere: no yugabyted is deployed, and Teardown never touches it.
	BackingExternal BackingMode = "external"
)

// Backing describes the YugabyteDB backing store for the control plane. The
// zero value (Mode "") means builtin_yugabyted. For external mode HostPorts (a
// CSV of tserver host:port) is required; User/DBName/SSLMode default to
// yugabyte/yb_storage/disable and Password is optional.
type Backing struct {
	Mode      BackingMode
	HostPorts string // external: CSV of tserver host:port the controller/endpoints connect at (YB_HOST_PORTS)
	User      string // external: YB role; "" → yugabyte
	DBName    string // external: amp storage database; "" → yb_storage
	Password  string // external: YB password (optional)
	SSLMode   string // external: YB sslmode; "" → disable
	// AdminHostPorts is the coordinator-reachable CSV used ONLY for the one-shot
	// yb_storage CREATE DATABASE. It exists for co-located YB-in-k8s, where the
	// controller's HostPorts is an in-cluster .svc.cluster.local name this
	// coordinator cannot resolve; "" → HostPorts (the normal external case).
	AdminHostPorts string
}

// Config configures a control-plane stack. Namespace is required; a few fields
// (Context, images, storage class, ECR registry) default sensibly and the rest
// carry the deploy-script defaults.
type Config struct {
	Context   string // kube-context; "" → current context
	Namespace string // target namespace (required)

	// Images. Each is a full "registry/repo:tag" ref; a bare "repo:tag" is
	// prefixed with ECRRegistry when it has no registry host. Empty → default.
	ControllerImage string
	EndpointImage   string
	MCPImage        string
	UIImage         string

	ControllerChart    string // path to the yb-amp-controller Helm chart (required for a helm deploy)
	ControllerManifest string // raw controller manifest applied verbatim instead of helm-templating (test/override escape hatch)

	// ExtraHelmSet are additional "key=value" pairs appended as `--set` flags to
	// the `helm template` render (after the built-in flags, so they can override).
	ExtraHelmSet []string

	YBImage      string
	YBStorage    string
	StorageClass string // endpoint /data PVC class; "" → emptyDir
	DBName       string

	// Backing selects the control-plane YugabyteDB. Zero value → the built-in
	// single-node yugabyted (current behaviour); external mode wires the
	// controller + endpoints at a cluster owned elsewhere (no yugabyted deployed,
	// never torn down here).
	Backing Backing

	RBACClusterScoped bool // false → no ClusterRole/ClusterRoleBinding is ever created
	HelmShim          bool // true → install the --create-namespace-stripping helm shim
	MCPEnabled        bool
	UIEnabled         bool

	ECRRegistry    string // registry host for the pull secret; "" → DefaultECRRegistry
	Region         string
	PullSecretName string
	// PullSecretDockerConfigB64, when set, is used as the pull secret's
	// .dockerconfigjson verbatim (skips the aws ecr get-login-password path).
	PullSecretDockerConfigB64 string

	ControllerService   string
	ControllerContainer string
	ControllerName      string
	ControllerPort      int
	EndpointServiceType string
	ControllerExpose    string
	HelmRelease         string

	// ControllerReach selects how the controller is reached once deployed:
	// ReachTailnet (default, "" also means tailnet) resolves its cluster-internal
	// address directly; ReachPortForward keeps the kubectl port-forward path.
	ControllerReach ControllerReachMode

	LocalPort int // controller port-forward local port (portforward mode only); 0 → ephemeral

	YBReadyTimeout         time.Duration
	ControllerReadyTimeout time.Duration

	// ExtraNamespaceLabels are merged onto the namespace ampstack creates (in
	// addition to ampqa=1 and the managed-by label) — e.g. a reaper timestamp.
	ExtraNamespaceLabels map[string]string
}

// StackInfo is what a successful Deploy returns.
type StackInfo struct {
	Namespace         string
	ControllerService string
	ControllerPort    int
	ControllerURL     string // tailnet: the direct address; portforward: via the held forward
	Images            map[string]string
	Forward           *k8s.PortForward // portforward mode only (owned by the App; released on Close); nil in tailnet mode
	PortForwardCmd    string           // human hint to re-establish the forward; "" in tailnet mode (no forward)
	Backing           string           // human description of the YB backing store
}

// StackStatus is a point-in-time readiness snapshot.
type StackStatus struct {
	Namespace       string
	Pods            map[string]string // pod name → phase
	YugabytedReady  bool
	ControllerReady bool
	Healthy         bool // controller /healthz answered 200
}

// App is a control-plane stack bound to one namespace.
type App struct {
	cfg     Config
	kube    *k8s.Kubectl // namespace-bound
	helmRun k8s.Runner
	logf    func(format string, args ...any)

	// Injected in tests / by composition; defaults wired in New.
	ecrToken  func(ctx context.Context, registry, region string) (string, error)
	newClient func(url string) *controller.Client
	waitReady func(ctx context.Context, cl *controller.Client, timeout time.Duration) error
	ensureDB  func(ctx context.Context, ci pg.ConnInfo, dbName string) error // external-backing yb_storage create

	pfMu sync.Mutex // guards pf/controllerURL: Deploy sets them, ControllerClient may replace pf, Close/probeHealthz read them

	// pf is the controller port-forward, held from Deploy through Teardown/Close
	// (never closed in between). Portforward mode only — always nil in tailnet
	// mode, so Close/Teardown naturally have nothing to close there.
	pf *k8s.PortForward
	// controllerURL is the direct URL Deploy resolved in tailnet mode (a stable
	// ClusterIP or pod IP — nothing to reconnect). Portforward mode leaves this
	// empty; the URL is derived from pf instead.
	controllerURL string
}

var _ apps.App = (*App)(nil)

// FromConfig reads env.ampstack.* into a Config. Image refs prefer a versions{}
// pin (versions.controller/endpoint/mcp/ui) when present, else the explicit
// env.ampstack.*_image, else the built-in default. cfg is the framework config;
// getters are passed as small closures so this package does not import core.
func FromConfig(get Getter) Config {
	image := func(versionKey, imageKey, def string) string {
		if v := get.GetString("versions."+versionKey, ""); v != "" {
			return v
		}
		return get.GetString("env.ampstack."+imageKey, def)
	}
	return Config{
		Context:         get.GetString("env.ampstack.context", ""),
		Namespace:       get.GetString("env.ampstack.namespace", ""),
		ControllerImage: image("controller", "controller_image", DefaultControllerImage),
		EndpointImage:   image("endpoint", "endpoint_image", DefaultEndpointImage),
		MCPImage:        image("mcp", "mcp_image", ""),
		UIImage:         image("ui", "ui_image", ""),
		ControllerChart: get.GetString("env.ampstack.controller_chart", ""),
		YBImage:         get.GetString("env.ampstack.yb_image", DefaultYBImage),
		YBStorage:       get.GetString("env.ampstack.yb_storage", DefaultYBStorage),
		StorageClass:    get.GetString("env.ampstack.storage_class", ""),
		DBName:          get.GetString("env.ampstack.db_name", DefaultDBName),
		Backing: Backing{
			Mode:           BackingMode(get.GetString("env.ampstack.backing.mode", string(BackingBuiltinYugabyted))),
			HostPorts:      get.GetString("env.ampstack.backing.host_ports", ""),
			User:           get.GetString("env.ampstack.backing.user", ""),
			DBName:         get.GetString("env.ampstack.backing.db_name", ""),
			Password:       get.GetString("env.ampstack.backing.password", ""),
			SSLMode:        get.GetString("env.ampstack.backing.sslmode", ""),
			AdminHostPorts: get.GetString("env.ampstack.backing.admin_host_ports", ""),
		},
		RBACClusterScoped:         get.GetBool("env.ampstack.rbac_cluster_scoped", false),
		HelmShim:                  get.GetBool("env.ampstack.helm_shim", true),
		MCPEnabled:                get.GetBool("env.ampstack.mcp_enabled", false),
		UIEnabled:                 get.GetBool("env.ampstack.ui_enabled", false),
		ECRRegistry:               get.GetString("env.ampstack.ecr_registry", DefaultECRRegistry),
		Region:                    get.GetString("env.ampstack.region", DefaultRegion),
		PullSecretName:            get.GetString("env.ampstack.pull_secret_name", DefaultPullSecretName),
		PullSecretDockerConfigB64: get.GetString("env.ampstack.pull_secret_b64", ""),
		ControllerService:         get.GetString("env.ampstack.controller_service", DefaultControllerService),
		ControllerContainer:       get.GetString("env.ampstack.controller_container", DefaultControllerContainer),
		ControllerName:            get.GetString("env.ampstack.controller_name", DefaultControllerName),
		ControllerPort:            get.GetInt("env.ampstack.controller_port", DefaultControllerPort),
		EndpointServiceType:       get.GetString("env.ampstack.endpoint_service_type", DefaultEndpointServiceType),
		ControllerExpose:          get.GetString("env.ampstack.controller_expose", DefaultControllerExpose),
		HelmRelease:               get.GetString("env.ampstack.helm_release", DefaultHelmRelease),
		ControllerReach:           ControllerReachMode(get.GetString("env.ampstack.controller_reach", string(ReachTailnet))),
		LocalPort:                 get.GetInt("env.ampstack.local_port", 0),
		YBReadyTimeout:            get.GetDuration("env.ampstack.yb_ready_timeout", 15*time.Minute),
		ControllerReadyTimeout:    get.GetDuration("env.ampstack.controller_ready_timeout", 5*time.Minute),
	}
}

// Getter is the minimal typed-config surface FromConfig needs; *core.Config
// satisfies it structurally (same method set), so this package does not import
// core (mirroring every other apps/* package).
type Getter interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetBool(key string, def bool) bool
	GetDuration(key string, def time.Duration) time.Duration
}

// New builds an App for cfg using the real kubectl/helm binaries.
func New(cfg Config) *App {
	applyDefaults(&cfg)
	a := &App{
		cfg:       cfg,
		kube:      k8s.New(k8s.Config{Context: cfg.Context, Namespace: cfg.Namespace}),
		helmRun:   k8s.OSRun,
		logf:      log.Printf,
		ecrToken:  defaultECRToken,
		newClient: func(url string) *controller.Client { return controller.New(url) },
		ensureDB:  defaultEnsureExternalDB,
	}
	a.waitReady = a.pollHealthz
	return a
}

func applyDefaults(cfg *Config) {
	if cfg.YBImage == "" {
		cfg.YBImage = DefaultYBImage
	}
	if cfg.YBStorage == "" {
		cfg.YBStorage = DefaultYBStorage
	}
	if cfg.DBName == "" {
		cfg.DBName = DefaultDBName
	}
	if cfg.Backing.Mode == "" {
		cfg.Backing.Mode = BackingBuiltinYugabyted
	}
	if cfg.Backing.User == "" {
		cfg.Backing.User = DefaultBackingUser
	}
	if cfg.Backing.DBName == "" {
		cfg.Backing.DBName = DefaultDBName
	}
	if cfg.Backing.SSLMode == "" {
		cfg.Backing.SSLMode = DefaultBackingSSLMode
	}
	// ECRRegistry is deliberately NOT defaulted here: an explicit "" means "no
	// pull secret" (a caller like env/exclusivek8s supplies a pre-encoded secret
	// and never mints via ECR). FromConfig applies DefaultECRRegistry for the
	// task/standalone path.
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	if cfg.PullSecretName == "" {
		cfg.PullSecretName = DefaultPullSecretName
	}
	if cfg.ControllerService == "" {
		cfg.ControllerService = DefaultControllerService
	}
	if cfg.ControllerContainer == "" {
		cfg.ControllerContainer = DefaultControllerContainer
	}
	if cfg.ControllerName == "" {
		cfg.ControllerName = DefaultControllerName
	}
	if cfg.ControllerPort == 0 {
		cfg.ControllerPort = DefaultControllerPort
	}
	if cfg.EndpointServiceType == "" {
		cfg.EndpointServiceType = DefaultEndpointServiceType
	}
	if cfg.ControllerExpose == "" {
		cfg.ControllerExpose = DefaultControllerExpose
	}
	if cfg.HelmRelease == "" {
		cfg.HelmRelease = DefaultHelmRelease
	}
	if cfg.ControllerReach == "" {
		cfg.ControllerReach = ReachTailnet
	}
	if cfg.YBReadyTimeout == 0 {
		cfg.YBReadyTimeout = 15 * time.Minute
	}
	if cfg.ControllerReadyTimeout == 0 {
		cfg.ControllerReadyTimeout = 5 * time.Minute
	}
}

// --- injection setters (composition + tests) ---

// WithKube replaces the namespace-bound kubectl (carrying its runner + starter),
// so a caller (env/exclusivek8s) or a test can drive the app through an injected
// transport. The kube's namespace should match cfg.Namespace.
func (a *App) WithKube(k *k8s.Kubectl) *App { a.kube = k; return a }

// Kube returns the app's namespace-bound kubectl (carrying its runner + starter),
// so a caller can gather failure diagnostics from the amp namespace before
// Teardown deletes it.
func (a *App) Kube() *k8s.Kubectl { return a.kube }

// WithHelmRunner overrides the runner used for `helm template`.
func (a *App) WithHelmRunner(r k8s.Runner) *App { a.helmRun = r; return a }

// WithECRToken overrides the ECR login-token resolver.
func (a *App) WithECRToken(f func(ctx context.Context, registry, region string) (string, error)) *App {
	a.ecrToken = f
	return a
}

// WithClientFactory overrides how a controller client is built from a URL.
func (a *App) WithClientFactory(f func(url string) *controller.Client) *App {
	a.newClient = f
	return a
}

// WithReadyFunc overrides the controller readiness poll (tests inject a no-op).
func (a *App) WithReadyFunc(f func(ctx context.Context, cl *controller.Client, timeout time.Duration) error) *App {
	a.waitReady = f
	return a
}

// WithEnsureDB overrides how the external-backing yb_storage database is
// ensured (tests inject a recorder; the default connects via apps/pg).
func (a *App) WithEnsureDB(f func(ctx context.Context, ci pg.ConnInfo, dbName string) error) *App {
	a.ensureDB = f
	return a
}

// WithLogf overrides the logger (nil silences).
func (a *App) WithLogf(f func(format string, args ...any)) *App { a.logf = f; return a }

func (a *App) log(format string, args ...any) {
	if a.logf != nil {
		a.logf(format, args...)
	}
}

// Namespace is the stack's namespace.
func (a *App) Namespace() string { return a.cfg.Namespace }

// Images returns the resolved image refs the stack uses.
func (a *App) images() map[string]string {
	m := map[string]string{
		"controller": withRegistry(a.cfg.ControllerImage, a.cfg.ECRRegistry),
		"endpoint":   withRegistry(a.cfg.EndpointImage, a.cfg.ECRRegistry),
	}
	if !a.externalBacking() {
		m["yugabyted"] = a.cfg.YBImage
	}
	if a.cfg.MCPEnabled && a.cfg.MCPImage != "" {
		m["mcp"] = withRegistry(a.cfg.MCPImage, a.cfg.ECRRegistry)
	}
	if a.cfg.UIEnabled && a.cfg.UIImage != "" {
		m["ui"] = withRegistry(a.cfg.UIImage, a.cfg.ECRRegistry)
	}
	return m
}

// Deploy stands the whole stack up idempotently (mirrors deploy/up.sh,
// namespace-scoped) and returns the live StackInfo. On any step failure it
// leaves what it created in place (Teardown removes it) and returns the error.
func (a *App) Deploy(ctx context.Context) (StackInfo, error) {
	if a.cfg.Namespace == "" {
		return StackInfo{}, fmt.Errorf("ampstack: namespace is required")
	}

	// 1. namespace (labelled so Teardown/reaper can recognize it).
	if err := a.kube.CreateNamespace(ctx, a.cfg.Namespace, namespaceLabels(a.cfg.ExtraNamespaceLabels)); err != nil {
		return StackInfo{}, fmt.Errorf("create namespace %s: %w", a.cfg.Namespace, err)
	}

	// 2. ECR image-pull secret.
	if err := a.ensurePullSecret(ctx); err != nil {
		return StackInfo{}, err
	}

	// 3+4. YB backing store: either deploy a built-in yugabyted and create
	// yb_storage in-cluster, or ensure yb_storage on the external cluster.
	if a.externalBacking() {
		if err := a.ensureExternalStorageDB(ctx); err != nil {
			return StackInfo{}, err
		}
	} else {
		if err := a.kube.Apply(ctx, yugabytedManifest(a.cfg.Namespace, a.cfg.YBImage, a.cfg.YBStorage, a.cfg.StorageClass)); err != nil {
			return StackInfo{}, fmt.Errorf("apply yugabyted: %w", err)
		}
		if err := a.kube.WaitFor(ctx, "statefulset/"+yugabytedStatefulSet, "jsonpath={.status.readyReplicas}=1", a.cfg.YBReadyTimeout); err != nil {
			return StackInfo{}, fmt.Errorf("yugabyted never became ready: %w", err)
		}
		if err := a.createStorageDB(ctx); err != nil {
			return StackInfo{}, err
		}
	}

	// 5. controller (helm-template + filter + apply, or a raw manifest).
	if err := a.deployController(ctx); err != nil {
		return StackInfo{}, err
	}

	// 6. helm shim (until a controller image without --create-namespace ships).
	if a.cfg.HelmShim {
		if err := a.installShim(ctx); err != nil {
			return StackInfo{}, err
		}
	}

	// 7. wait controller Available.
	if err := a.kube.WaitFor(ctx, "deployment/"+a.cfg.ControllerName, "condition=Available", a.cfg.ControllerReadyTimeout); err != nil {
		return StackInfo{}, fmt.Errorf("controller deployment never became available: %w", err)
	}

	// 8. reach the controller (tailnet: resolve its direct address; portforward:
	// the kubectl forward) + full readiness poll.
	url, pf, err := a.reachController(ctx)
	if err != nil {
		return StackInfo{}, err
	}
	if err := a.waitReady(ctx, a.newClient(url), a.cfg.ControllerReadyTimeout); err != nil {
		if pf != nil {
			_ = pf.Close()
		}
		return StackInfo{}, fmt.Errorf("controller never became healthy: %w", err)
	}
	a.pfMu.Lock()
	a.pf = pf
	a.controllerURL = url
	a.pfMu.Unlock()

	pfCmd := ""
	if pf != nil {
		pfCmd = a.portForwardCmd()
	}
	return StackInfo{
		Namespace:         a.cfg.Namespace,
		ControllerService: a.cfg.ControllerService,
		ControllerPort:    a.cfg.ControllerPort,
		ControllerURL:     url,
		Images:            a.images(),
		Forward:           pf,
		PortForwardCmd:    pfCmd,
		Backing:           a.backingDescribe(),
	}, nil
}

// reachController establishes how the controller will be reached for the rest
// of the stack's lifetime: tailnet mode resolves its direct cluster-internal
// address (no forward at all); portforward mode opens the kubectl forward.
func (a *App) reachController(ctx context.Context) (string, *k8s.PortForward, error) {
	if a.cfg.ControllerReach == ReachPortForward {
		pf, err := a.kube.PortForward(ctx, "svc/"+a.cfg.ControllerService, a.cfg.LocalPort, a.cfg.ControllerPort)
		if err != nil {
			return "", nil, fmt.Errorf("port-forward controller: %w", err)
		}
		return "http://" + pf.LocalAddr(), pf, nil
	}
	addr, err := a.resolveControllerAddr(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("resolve controller address: %w", err)
	}
	return fmt.Sprintf("http://%s:%d", addr, a.cfg.ControllerPort), nil, nil
}

// resolveControllerAddr resolves the controller's stable, cluster-internal
// address for tailnet mode: the controller Service's ClusterIP when it
// answers /healthz, else the controller pod's IP — pod IPs on this cluster are
// reachable directly from the coordinator over the tailnet (the same way an
// amp endpoint's pod IP already is), so the fallback never needs a
// port-forward either.
func (a *App) resolveControllerAddr(ctx context.Context) (string, error) {
	if ip, err := a.kube.Get(ctx, "svc", a.cfg.ControllerService, "{.spec.clusterIP}"); err == nil {
		if ip = strings.TrimSpace(ip); ip != "" && ip != "None" && a.probeDirectHealthz(ctx, ip) {
			return ip, nil
		}
	}
	podIP, err := a.kube.GetSelector(ctx, "pod", "app="+a.cfg.ControllerName, "{.items[0].status.podIP}")
	if err != nil {
		return "", fmt.Errorf("controller ClusterIP unreachable; pod IP lookup failed: %w", err)
	}
	if podIP = strings.TrimSpace(podIP); podIP == "" {
		return "", fmt.Errorf("controller ClusterIP unreachable and no controller pod found (selector app=%s)", a.cfg.ControllerName)
	}
	if !a.probeDirectHealthz(ctx, podIP) {
		return "", fmt.Errorf("controller not reachable at its ClusterIP nor its pod IP %s", podIP)
	}
	return podIP, nil
}

// probeDirectHealthz retries GET /healthz against addr:ControllerPort a few
// times (controllerProbeRetries/-Delay), riding out the brief gap between the
// Deployment reporting Available and the process actually listening. It is a
// quick fail-fast check, not a substitute for the full waitReady poll that
// follows once an address is chosen.
func (a *App) probeDirectHealthz(ctx context.Context, addr string) bool {
	url := fmt.Sprintf("http://%s:%d", addr, a.cfg.ControllerPort)
	for attempt := 0; ; attempt++ {
		if a.newClient(url).Healthz(ctx) == nil {
			return true
		}
		if attempt >= controllerProbeRetries {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(controllerProbeDelay):
		}
	}
}

func (a *App) portForwardCmd() string {
	ctxArg := ""
	if a.cfg.Context != "" {
		ctxArg = "--context " + a.cfg.Context + " "
	}
	return fmt.Sprintf("kubectl %s-n %s port-forward svc/%s %d:%d",
		ctxArg, a.cfg.Namespace, a.cfg.ControllerService, a.cfg.ControllerPort, a.cfg.ControllerPort)
}

// ensurePullSecret applies the docker-registry pull secret, sourcing the
// dockerconfigjson from PullSecretDockerConfigB64 when given, else minting a
// fresh ECR login token (deploy/ecr-creds.sh). No registry configured and no
// pre-encoded value → skip (no secret needed, e.g. public images).
func (a *App) ensurePullSecret(ctx context.Context) error {
	b64 := a.cfg.PullSecretDockerConfigB64
	if b64 == "" {
		if a.cfg.ECRRegistry == "" {
			return nil
		}
		token, err := a.ecrToken(ctx, a.cfg.ECRRegistry, a.cfg.Region)
		if err != nil {
			return fmt.Errorf("resolve ECR token: %w", err)
		}
		b64 = dockerConfigJSONB64(a.cfg.ECRRegistry, token)
	}
	if err := a.kube.Apply(ctx, pullSecretManifest(a.cfg.Namespace, a.cfg.PullSecretName, b64)); err != nil {
		return fmt.Errorf("apply pull secret: %w", err)
	}
	return nil
}

// RefreshPullSecret re-mints the ECR docker-registry pull secret and re-applies
// it to the env namespace, healing a long-standing control plane whose amp images
// stopped pulling after the ~12h ECR login-token TTL. It verifies the namespace
// is ampqa-managed first (the same guard as Reap), so it only ever touches an
// ampqa-owned env. When roll is true it also rolls the controller Deployment so
// it re-pulls immediately instead of waiting out ImagePullBackOff. A public-image
// env (no ecr_registry, no pull_secret_b64) has nothing to refresh and errors.
func (a *App) RefreshPullSecret(ctx context.Context, roll bool) error {
	if a.cfg.ECRRegistry == "" && a.cfg.PullSecretDockerConfigB64 == "" {
		return fmt.Errorf("nothing to refresh: no ecr_registry or pull_secret_b64 configured (amp images are public)")
	}
	label, err := a.kube.Get(ctx, "namespace", a.cfg.Namespace, "{.metadata.labels."+labelPath(ManagedByLabel)+"}")
	if err != nil {
		return fmt.Errorf("read managed-by label on %s: %w", a.cfg.Namespace, err)
	}
	if label != ManagedByValue {
		return fmt.Errorf("refusing to touch namespace %s: not ampqa-managed (%s=%q, want %q)",
			a.cfg.Namespace, ManagedByLabel, label, ManagedByValue)
	}
	if err := a.ensurePullSecret(ctx); err != nil {
		return err
	}
	if roll {
		if err := a.kube.Patch(ctx, "deployment", a.cfg.ControllerName, "strategic", rolloutRestartPatch()); err != nil {
			return fmt.Errorf("roll controller %s: %w", a.cfg.ControllerName, err)
		}
	}
	return nil
}

// rolloutRestartPatch is a strategic-merge patch that bumps a pod-template
// annotation, triggering a Deployment rollout — the same mechanism as
// `kubectl rollout restart`, forcing a re-pull with the refreshed secret.
func rolloutRestartPatch() string {
	return fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"ampqa.dev/restartedAt":%q}}}}}`,
		time.Now().UTC().Format(time.RFC3339Nano))
}

// createStorageDB execs into yugabyted-0 to ensure the yb_storage database
// exists before the controller rolls out (its db.Connect targets it at startup).
func (a *App) createStorageDB(ctx context.Context) error {
	res, err := a.kube.ForPod(yugabytedPod, "").Run(ctx, createStorageDBScript(a.cfg.DBName))
	if err != nil {
		return fmt.Errorf("create %s: %w", a.cfg.DBName, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("create %s exited %d: %s", a.cfg.DBName, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// deployController renders the chart (helm template), filters cluster-scoped
// RBAC when RBACClusterScoped is false, and applies the result — or applies a
// raw ControllerManifest verbatim when one is set.
func (a *App) deployController(ctx context.Context) error {
	if a.cfg.ControllerManifest != "" {
		if err := a.kube.Apply(ctx, a.cfg.ControllerManifest); err != nil {
			return fmt.Errorf("apply controller manifest: %w", err)
		}
		return nil
	}
	if a.cfg.ControllerChart == "" {
		return fmt.Errorf("ampstack: neither controller_chart nor controller_manifest configured")
	}
	rendered, err := a.helmTemplate(ctx)
	if err != nil {
		return err
	}
	manifest := filterManifests(rendered, !a.cfg.RBACClusterScoped)
	if strings.TrimSpace(manifest) == "" {
		return nil
	}
	if err := a.kube.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("apply controller manifest: %w", err)
	}
	return nil
}

// helmTemplate renders the controller chart to a manifest string.
func (a *App) helmTemplate(ctx context.Context) (string, error) {
	argv := a.helmTemplateArgs()
	res, err := a.helmRun(ctx, nil, argv)
	if err != nil {
		return "", fmt.Errorf("helm template: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("helm template exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// helmTemplateArgs builds the `helm template` argv mirroring the --set flags
// deploy/controller/redeploy.sh uses (namespace-scoped: controller Service
// stays ClusterIP-only via controller.service.type=none).
func (a *App) helmTemplateArgs() []string {
	ctrlRepo, ctrlTag := splitImageRef(withRegistry(a.cfg.ControllerImage, a.cfg.ECRRegistry))
	epRepo, epTag := splitImageRef(withRegistry(a.cfg.EndpointImage, a.cfg.ECRRegistry))

	sets := map[string]string{
		"imagePullSecrets[0].name":             a.cfg.PullSecretName,
		"controller.image.repository":          ctrlRepo,
		"controller.image.tag":                 ctrlTag,
		"controller.endpoint.image.repository": epRepo,
		"controller.endpoint.image.tag":        epTag,
		"controller.endpoint.ybUser":           a.backingUser(),
		"controller.endpoint.ybDbname":         a.storageDBName(),
		"controller.endpoint.service.type":     a.cfg.EndpointServiceType,
		"controller.endpoint.storageClass":     a.cfg.StorageClass,
		"controller.service.type":              a.cfg.ControllerExpose,
		"rbac.clusterScoped":                   boolStr(a.cfg.RBACClusterScoped),
		"mcp.enabled":                          boolStr(a.cfg.MCPEnabled),
		"ui.enabled":                           boolStr(a.cfg.UIEnabled),
	}
	if a.cfg.MCPEnabled && a.cfg.MCPImage != "" {
		r, t := splitImageRef(withRegistry(a.cfg.MCPImage, a.cfg.ECRRegistry))
		sets["mcp.image.repository"] = r
		sets["mcp.image.tag"] = t
	}
	if a.cfg.UIEnabled && a.cfg.UIImage != "" {
		r, t := splitImageRef(withRegistry(a.cfg.UIImage, a.cfg.ECRRegistry))
		sets["ui.image.repository"] = r
		sets["ui.image.tag"] = t
	}

	argv := []string{"helm", "template", a.cfg.HelmRelease, a.cfg.ControllerChart, "--namespace", a.cfg.Namespace}
	if a.cfg.Context != "" {
		argv = append(argv, "--kube-context", a.cfg.Context)
	}
	argv = append(argv, sortedSetArgs("--set", sets)...)
	for _, s := range a.cfg.ExtraHelmSet {
		argv = append(argv, "--set", s)
	}
	// ybHostPorts (an external CSV of "host:port,host:port,...") and the DSN
	// can carry commas, which helm's --set/--set-string grammar splits on;
	// --set-string additionally keeps helm from coercing the value (numeric
	// host:port digits, etc). Both go through helmEscape so any literal comma
	// survives as one value instead of being torn into bogus assignments.
	argv = append(argv, "--set-string", "controller.endpoint.ybHostPorts="+helmEscape(a.backingHostPorts()))
	argv = append(argv, "--set-string", "controller.databaseUrl="+helmEscape(a.dbURL()))
	return argv
}

func (a *App) ybFQDN() string {
	return fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", yugabytedStatefulSet, yugabytedStatefulSet, a.cfg.Namespace)
}

// externalBacking reports whether the control plane sits on a YugabyteDB owned
// elsewhere (no in-namespace yugabyted deployed or torn down).
func (a *App) externalBacking() bool { return a.cfg.Backing.Mode == BackingExternal }

// storageDBName is the amp storage database: the external backing DBName in
// external mode, else the built-in DBName.
func (a *App) storageDBName() string {
	if a.externalBacking() {
		return orDefault(a.cfg.Backing.DBName, DefaultDBName)
	}
	return a.cfg.DBName
}

// backingUser is the YB role the controller + endpoints connect as.
func (a *App) backingUser() string {
	if a.externalBacking() {
		return orDefault(a.cfg.Backing.User, DefaultBackingUser)
	}
	return DefaultBackingUser
}

// backingHostPorts is the CSV of tserver host:port the controller hands to
// endpoints: the external CSV in external mode, else the built-in yugabyted
// pod FQDN.
func (a *App) backingHostPorts() string {
	if a.externalBacking() {
		return a.cfg.Backing.HostPorts
	}
	return a.ybFQDN() + ":5433"
}

// adminHostPorts is the coordinator-reachable CSV for the one-shot yb_storage
// CREATE DATABASE: Backing.AdminHostPorts when set (co-located YB-in-k8s, where
// HostPorts is an in-cluster DNS name the coordinator can't resolve), else the
// controller's HostPorts (the normal external case).
func (a *App) adminHostPorts() string {
	if a.cfg.Backing.AdminHostPorts != "" {
		return a.cfg.Backing.AdminHostPorts
	}
	return a.cfg.Backing.HostPorts
}

// dbURL is the controller's own backing DSN (AMP_DATABASE_URL). It targets the
// external cluster's first host:port in external mode, else the built-in
// yugabyted pod.
func (a *App) dbURL() string {
	if a.externalBacking() {
		host, port := firstHostPort(a.cfg.Backing.HostPorts)
		return backingDSN(host, port, a.backingUser(), a.cfg.Backing.Password, a.storageDBName(), orDefault(a.cfg.Backing.SSLMode, DefaultBackingSSLMode))
	}
	return fmt.Sprintf("postgres://%s@%s:5433/%s?sslmode=%s", DefaultBackingUser, a.ybFQDN(), a.cfg.DBName, DefaultBackingSSLMode)
}

// backingDescribe is a one-line human summary of the backing store.
func (a *App) backingDescribe() string {
	if a.externalBacking() {
		return fmt.Sprintf("external YugabyteDB %s (db %s)", a.cfg.Backing.HostPorts, a.storageDBName())
	}
	return fmt.Sprintf("built-in yugabyted (db %s)", a.cfg.DBName)
}

// ensureExternalStorageDB connects to the external cluster's maintenance
// database and creates the amp storage database if absent (idempotent). It
// never touches the external cluster beyond this CREATE DATABASE.
func (a *App) ensureExternalStorageDB(ctx context.Context) error {
	if strings.TrimSpace(a.cfg.Backing.HostPorts) == "" {
		return fmt.Errorf("ampstack: external backing requires backing.host_ports")
	}
	host, port := firstHostPort(a.adminHostPorts())
	ci := pg.ConnInfo{
		Host:     host,
		Port:     port,
		User:     a.backingUser(),
		Password: a.cfg.Backing.Password,
		Database: ybMaintenanceDB,
		SSLMode:  orDefault(a.cfg.Backing.SSLMode, DefaultBackingSSLMode),
	}
	if err := a.ensureDB(ctx, ci, a.storageDBName()); err != nil {
		return fmt.Errorf("ensure %s on external backing %s: %w", a.storageDBName(), a.cfg.Backing.HostPorts, err)
	}
	return nil
}

// installShim applies the shim ConfigMap and patches the controller deployment
// to mount it and prepend its dir to PATH.
func (a *App) installShim(ctx context.Context) error {
	if err := a.kube.Apply(ctx, shimConfigMapManifest(a.cfg.Namespace)); err != nil {
		return fmt.Errorf("apply helm shim configmap: %w", err)
	}
	if err := a.kube.Patch(ctx, "deployment", a.cfg.ControllerName, "strategic", shimDeploymentPatch(a.cfg.ControllerContainer)); err != nil {
		return fmt.Errorf("patch controller with helm shim: %w", err)
	}
	return nil
}

// Status reports pod/deployment readiness plus a controller /healthz probe. It
// opens a short-lived port-forward for the probe (or reuses the held one).
func (a *App) Status(ctx context.Context) (StackStatus, error) {
	st := StackStatus{Namespace: a.cfg.Namespace, Pods: map[string]string{}}

	pods, err := a.kube.ListPods(ctx)
	if err != nil {
		return st, fmt.Errorf("list pods: %w", err)
	}
	for _, pod := range pods {
		phase, perr := a.kube.Get(ctx, "pod", pod, "{.status.phase}")
		if perr != nil {
			phase = "Unknown"
		}
		st.Pods[pod] = phase
	}

	if a.externalBacking() {
		st.YugabytedReady = true // no in-namespace yugabyted; the external store is owned elsewhere
	} else if ready, _ := a.kube.Get(ctx, "statefulset", yugabytedStatefulSet, "{.status.readyReplicas}"); ready == "1" {
		st.YugabytedReady = true
	}
	if ready, _ := a.kube.Get(ctx, "deployment", a.cfg.ControllerName, "{.status.readyReplicas}"); ready != "" && ready != "0" {
		st.ControllerReady = true
	}

	st.Healthy = a.probeHealthz(ctx) == nil
	return st, nil
}

// probeHealthz checks the controller in whichever mode is configured: tailnet
// hits the resolved direct URL (falling back to a fresh resolve if Deploy
// hasn't run yet); portforward reuses the held forward when alive, else opens
// a short-lived one (a probe is a one-off status check, not the long-lived
// compute-phase path — see ControllerClient for the reconnecting accessor that
// path uses).
func (a *App) probeHealthz(ctx context.Context) error {
	if a.cfg.ControllerReach != ReachPortForward {
		url, err := a.directControllerURL(ctx)
		if err != nil {
			return err
		}
		return a.newClient(url).Healthz(ctx)
	}
	a.pfMu.Lock()
	pf := a.pf
	a.pfMu.Unlock()
	if pf != nil && pf.Alive() {
		return a.newClient("http://" + pf.LocalAddr()).Healthz(ctx)
	}
	tmp, err := a.kube.PortForward(ctx, "svc/"+a.cfg.ControllerService, a.cfg.LocalPort, a.cfg.ControllerPort)
	if err != nil {
		return err
	}
	defer tmp.Close()
	return a.newClient("http://" + tmp.LocalAddr()).Healthz(ctx)
}

// directControllerURL returns the tailnet URL Deploy resolved, resolving fresh
// (ClusterIP → pod-IP fallback) if called before Deploy has run.
func (a *App) directControllerURL(ctx context.Context) (string, error) {
	a.pfMu.Lock()
	url := a.controllerURL
	a.pfMu.Unlock()
	if url != "" {
		return url, nil
	}
	addr, err := a.resolveControllerAddr(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve controller address: %w", err)
	}
	return fmt.Sprintf("http://%s:%d", addr, a.cfg.ControllerPort), nil
}

// ControllerClient returns a live controller client for whichever reach mode
// is configured. In tailnet mode (default) this is the stable direct URL
// Deploy resolved — a ClusterIP or pod IP, nothing to reconnect. In
// portforward mode it re-establishes the held forward first if the underlying
// kubectl process has died (Alive() false); the forward is meant to survive
// the whole compute phase (Deploy through Teardown), not just Deploy's own
// health check, since kubectl port-forward is known-flaky over a long-running
// test. Either way, any caller about to make a controller call should fetch
// the client through here rather than caching a URL from Deploy's StackInfo.
// Safe for concurrent use.
func (a *App) ControllerClient(ctx context.Context) (*controller.Client, string, error) {
	a.pfMu.Lock()
	defer a.pfMu.Unlock()
	if a.cfg.ControllerReach != ReachPortForward {
		if a.controllerURL == "" {
			return nil, "", fmt.Errorf("ampstack: controller address not resolved (Deploy must run first)")
		}
		return a.newClient(a.controllerURL), a.controllerURL, nil
	}
	if a.pf == nil || !a.pf.Alive() {
		if a.pf != nil {
			_ = a.pf.Close()
			a.pf = nil
		}
		pf, err := a.kube.PortForward(ctx, "svc/"+a.cfg.ControllerService, a.cfg.LocalPort, a.cfg.ControllerPort)
		if err != nil {
			return nil, "", fmt.Errorf("reconnect controller port-forward: %w", err)
		}
		a.pf = pf
	}
	url := "http://" + a.pf.LocalAddr()
	return a.newClient(url), url, nil
}

// Collect gathers the controller and yugabyted logs, best-effort.
func (a *App) Collect(ctx context.Context, into apps.ArtifactSink) error {
	targets := []struct{ name, target, container string }{
		{"controller", "deployment/" + a.cfg.ControllerName, a.cfg.ControllerContainer},
	}
	if !a.externalBacking() {
		targets = append(targets, struct{ name, target, container string }{"yugabyted", "statefulset/" + yugabytedStatefulSet, ""})
	}
	if a.cfg.MCPEnabled {
		targets = append(targets, struct{ name, target, container string }{"mcp", "deployment/amp-controller-mcp", ""})
	}
	if a.cfg.UIEnabled {
		targets = append(targets, struct{ name, target, container string }{"ui", "deployment/amp-controller-ui", ""})
	}
	for _, t := range targets {
		logs, err := a.kube.Logs(ctx, t.target, t.container, 0)
		if err != nil {
			a.log("ampstack collect: logs %s: %v", t.target, err)
			continue
		}
		if err := into.Add(t.name+".log", "log", []byte(logs)); err != nil {
			return err
		}
	}
	return nil
}

// Teardown deletes the namespace — but ONLY after confirming it carries the
// ampqa managed-by label, so a mistargeted namespace is never destroyed.
func (a *App) Teardown(ctx context.Context) error {
	if a.cfg.Namespace == "" {
		return fmt.Errorf("ampstack teardown: no namespace")
	}
	cluster := a.clusterKube()
	label, err := cluster.Get(ctx, "namespace", a.cfg.Namespace, "{.metadata.labels."+labelPath(ManagedByLabel)+"}")
	if err != nil {
		return fmt.Errorf("read managed-by label on %s: %w", a.cfg.Namespace, err)
	}
	if label != ManagedByValue {
		return fmt.Errorf("refusing to delete namespace %s: not ampqa-managed (%s=%q, want %q)",
			a.cfg.Namespace, ManagedByLabel, label, ManagedByValue)
	}
	if err := cluster.DeleteNamespace(ctx, a.cfg.Namespace); err != nil {
		return fmt.Errorf("delete namespace %s: %w", a.cfg.Namespace, err)
	}
	return nil
}

// Reap deletes an ampstack namespace given only its kube settings + name — the
// separate-process `ampqa env rm` path, which has no live App (the in-memory
// teardown closure is gone). It runs the SAME managed-by-guarded namespace
// delete as App.Teardown, so there is one teardown implementation.
// kubeContext/kubeconfig target the cluster the stack was deployed on.
func Reap(ctx context.Context, kubeContext, kubeconfig, namespace string) error {
	if namespace == "" {
		return fmt.Errorf("ampstack reap: empty namespace")
	}
	kube := k8s.New(k8s.Config{Context: kubeContext}).WithRunner(k8s.KubeconfigRunner(kubeconfig))
	label, err := kube.Get(ctx, "namespace", namespace, "{.metadata.labels."+labelPath(ManagedByLabel)+"}")
	if err != nil {
		return fmt.Errorf("read managed-by label on %s: %w", namespace, err)
	}
	if label != ManagedByValue {
		return fmt.Errorf("refusing to delete namespace %s: not ampqa-managed (%s=%q, want %q)",
			namespace, ManagedByLabel, label, ManagedByValue)
	}
	if err := kube.DeleteNamespace(ctx, namespace); err != nil {
		return fmt.Errorf("delete namespace %s: %w", namespace, err)
	}
	return nil
}

// Signal is a no-op for the control-plane stack (nothing to pause/stop short of
// a Teardown); present so App satisfies apps.App.
func (a *App) Signal(ctx context.Context, sig apps.AppSignal) error { return nil }

// WaitReady re-checks controller health (apps.App).
func (a *App) WaitReady(ctx context.Context) error { return a.probeHealthz(ctx) }

// Close releases the held controller port-forward.
func (a *App) Close(ctx context.Context) error {
	a.pfMu.Lock()
	pf := a.pf
	a.pf = nil
	a.pfMu.Unlock()
	if pf != nil {
		return pf.Close()
	}
	return nil
}

// clusterKube derives a namespace-unbound kubectl sharing this app's runner and
// starter, for cluster-scoped operations (namespace get/delete).
func (a *App) clusterKube() *k8s.Kubectl {
	return k8s.New(k8s.Config{Context: a.cfg.Context}).
		WithRunner(a.kube.Runner()).
		WithStarter(a.kube.StarterFn())
}

// pollHealthz polls the controller's /healthz until it answers or timeout.
func (a *App) pollHealthz(ctx context.Context, cl *controller.Client, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		if last = cl.Healthz(wctx); last == nil {
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("healthz never succeeded: %w (last: %v)", wctx.Err(), last)
		case <-time.After(2 * time.Second):
		}
	}
}

// --- pure helpers ---

// filterManifests optionally drops ClusterRole/ClusterRoleBinding docs (robust
// `---`-split + top-level kind check), so no cluster-scoped object is ever
// applied on a namespace-scoped install.
func filterManifests(rendered string, dropClusterScoped bool) string {
	if !dropClusterScoped {
		return rendered
	}
	docs := splitYAMLDocs(rendered)
	kept := make([]string, 0, len(docs))
	for _, d := range docs {
		if strings.TrimSpace(d) == "" {
			continue
		}
		switch manifestKind(d) {
		case "ClusterRole", "ClusterRoleBinding":
			continue
		}
		kept = append(kept, strings.TrimSpace(d))
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n---\n") + "\n"
}

// splitYAMLDocs splits a multi-doc manifest on lines that are exactly "---".
func splitYAMLDocs(s string) []string {
	var docs []string
	var cur []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "---" {
			docs = append(docs, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return append(docs, strings.Join(cur, "\n"))
}

// manifestKind returns the top-level (column-0) kind of one YAML doc, or "".
func manifestKind(doc string) string {
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "kind:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "kind:"))
		}
	}
	return ""
}

// withRegistry prefixes a bare "repo[:tag]" (no registry host) with registry.
// A ref that already has a registry host (a dot or colon before the first
// slash) or is empty is returned unchanged.
func withRegistry(ref, registry string) string {
	if ref == "" || registry == "" {
		return ref
	}
	first := ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first = ref[:i]
	} else {
		// No slash at all → a bare repo; prefix it.
		return registry + "/" + ref
	}
	if strings.ContainsAny(first, ".:") {
		return ref // already registry-qualified
	}
	return registry + "/" + ref
}

// labelPath escapes dots in a label key for a kubectl jsonpath segment.
func labelPath(key string) string {
	return strings.ReplaceAll(key, ".", `\.`)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// orDefault returns v when non-empty, else def.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// firstHostPort parses the first "host:port" of a CSV, defaulting the port to
// 5433 when absent or unparsable.
func firstHostPort(csv string) (string, int) {
	first := csv
	if i := strings.IndexByte(csv, ','); i >= 0 {
		first = csv[:i]
	}
	first = strings.TrimSpace(first)
	host, portStr, err := net.SplitHostPort(first)
	if err != nil {
		return first, 5433
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, 5433
	}
	return host, port
}

// backingDSN renders a postgres:// URL for the controller's AMP_DATABASE_URL.
// Password, when set, is included; sslmode defaults to disable.
func backingDSN(host string, port int, user, password, dbName, sslMode string) string {
	userInfo := user
	if password != "" {
		userInfo = user + ":" + password
	}
	return fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=%s", userInfo, host, port, dbName, orDefault(sslMode, DefaultBackingSSLMode))
}

// pgQuoteIdent double-quotes a SQL identifier, escaping embedded quotes.
func pgQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// defaultEnsureExternalDB connects to ci (a maintenance database on the
// external cluster) and creates dbName if it does not already exist. It is
// idempotent and tolerates a racy concurrent create.
func defaultEnsureExternalDB(ctx context.Context, ci pg.ConnInfo, dbName string) error {
	cl, err := pg.Connect(ctx, ci)
	if err != nil {
		return fmt.Errorf("connect %s: %w", ci.Addr(), err)
	}
	defer cl.Close()
	n, err := cl.QueryInt(ctx, "SELECT count(*) FROM pg_database WHERE datname=$1", dbName)
	if err != nil {
		return fmt.Errorf("check database %s: %w", dbName, err)
	}
	if n > 0 {
		return nil
	}
	if _, err := cl.Exec(ctx, "CREATE DATABASE "+pgQuoteIdent(dbName)); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return fmt.Errorf("create database %s: %w", dbName, err)
	}
	return nil
}

// defaultECRToken shells `aws ecr get-login-password` for the pull-secret token,
// mirroring deploy/ecr-creds.sh's ambient-cred resolution.
func defaultECRToken(ctx context.Context, _ /*registry*/, region string) (string, error) {
	cmd := exec.CommandContext(ctx, "aws", "ecr", "get-login-password", "--region", region)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("aws ecr get-login-password: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
