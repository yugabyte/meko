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
//     endpointConnInfo, unchanged). On a cluster whose pod/service ranges are
//     NOT routable from the coordinator neither address answers, and this mode
//     then continues over a kubectl port-forward rather than failing the run.
//   - portforward: the historical kubectl port-forward path (held from Deploy
//     through Teardown/Close, reconnected on death), kept for forcing the
//     forward unconditionally, without the direct probes.
//
// It exists so the control-plane lifecycle is an App like everything else the
// framework drives, not raw kubectl/helm in an ad-hoc script: tasks
// (deploy-amp/status-amp/teardown-amp) and the exclusivek8s env provider both
// compose it rather than duplicating the bring-up. kubectl/helm are shelled
// ambient-authed binaries (KISS, no client-go), reached through
// exec/k8s.Kubectl.
//
// The chart is deployed as the chart wrote it: nothing here overrides
// rbac.nodeRead or rbac.clusterScoped or edits the rendered docs, so a run
// installs the RBAC a real install would get. That default includes a
// cluster-scoped node read (a ClusterRole + ClusterRoleBinding named after the
// namespace), which a namespace delete leaves behind — Teardown and Reap sweep
// it, see deleteControllerClusterRBAC. Because the
// currently-deployed controller image predates the fix that stopped passing
// helm --create-namespace, HelmShim (default on) installs a tiny PATH-wrapper
// that strips that flag until a controller image built without it ships.
package ampstack

import (
	"context"
	"fmt"
	"log"
	"maps"
	"net"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// Defaults.
const (
	DefaultPullSecretName = "pull-secret"
	DefaultYBImage        = "yugabytedb/yugabyte:latest"
	// DefaultYBImageRegistry is the registry+repo the built-in yugabyted image
	// is derived from (as "<registry>:<version>") when env.ampstack.yb_image is
	// unset. quay itest is the only registry with unreleased builds (Docker Hub
	// carries released ones only); it is private, so pulling needs its credential.
	DefaultYBImageRegistry = "quay.io/yugabyte/yugabyte-itest"
	DefaultYBStorage       = "20Gi"
	// DefaultYBStorageClass must bind WaitForFirstConsumer: an Immediate class
	// (GKE's default `standard`) provisions the disk first and zone-pins the pod.
	DefaultYBStorageClass = "standard-rwo"
	// DefaultYBReadyTimeout bounds the built-in yugabyted's Ready wait: a cold
	// start pays a node add + multi-GB image pull before its own bootstrap.
	DefaultYBReadyTimeout = 20 * time.Minute
	// DefaultControllerReadyTimeout: small image, and the node is already warm.
	DefaultControllerReadyTimeout = 5 * time.Minute
	// DefaultControllerRollTimeout bounds a roll of an ALREADY-deployed controller.
	// Unlike the initial deploy it cannot assume a warm node: a long-lived env may
	// have scaled its pool down, so the new pod can pay a node add plus image pull.
	DefaultControllerRollTimeout = 20 * time.Minute
	DefaultControllerService     = "amp-controller"
	DefaultControllerName        = "amp-controller"
	DefaultControllerContainer   = "controller"
	DefaultControllerPort        = 8080
	DefaultHelmRelease           = "yb-amp-controller"
	DefaultDBName                = "yb_storage"
	DefaultECRRegistry           = "454529406029.dkr.ecr.us-west-2.amazonaws.com"
	// Registry flavors for env.ampstack.registry — where bare image refs
	// resolve and which auth lands in the pull secret. Quay is the default:
	// release and custom builds publish to quay.io/yugabyte/amp-*.
	RegistryQuay = "quay"
	RegistryECR  = "ecr"
	RegistryGHCR = "ghcr"
	// Flat amp repos under the quay/ghcr org namespaces.
	QuayNamespace = "quay.io/yugabyte"
	GHCRRegistry  = "ghcr.io"
	GHCRNamespace = "ghcr.io/yugabyte"
	// QuayRegistry is the quay host the amp release/custom images live under
	// (quay.io/yugabyte/amp-{controller,pg,endpoint}).
	QuayRegistry           = "quay.io"
	DefaultRegion          = "us-west-2"
	DefaultControllerImage = "amp/controller:tailnet-mig0010"
	DefaultEndpointImage   = "amp/endpoint:phase1-20260703-154251"
	// Default pins for the flat-named registries (quay/ghcr). Bumped as new
	// builds are published; custom-7f195e43e51f is the current build whose
	// controller folds the storage-controller in-process (#280) and so matches
	// the shipped chart. Earlier tags (custom-4f1ca53, custom-0e2131d) predate
	// #280 and crash-loop on "Insecure config" (separate storage_controller
	// subprocess), so the controller never reaches Available.
	DefaultQuayControllerImage = "amp-controller:custom-7f195e43e51f"
	DefaultQuayEndpointImage   = "amp-endpoint:custom-7f195e43e51f"
	DefaultEndpointServiceType = "ClusterIP"
	DefaultControllerExpose    = "none"
	DefaultBackingUser         = "yugabyte"
	DefaultBackingSSLMode      = "disable"

	// ybMaintenanceDB is the always-present database the external-backing yb_storage
	// CREATE DATABASE connects to (yb_storage may not exist yet).
	ybMaintenanceDB = "yugabyte"

	// ybPGPort is YugabyteDB's YSQL port, the same on the built-in yugabyted and
	// on every external backing this framework has met.
	ybPGPort = 5433

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
	// and talks to it directly — no kubectl port-forward. When neither address
	// answers (a cluster whose pod/service ranges aren't routable from the
	// coordinator) it falls back to a kubectl port-forward.
	ReachTailnet ControllerReachMode = "tailnet"
	// ReachPortForward forces the kubectl port-forward path (held from Deploy
	// through Teardown/Close, reconnected on death) without probing the direct
	// addresses first.
	ReachPortForward ControllerReachMode = "portforward"
	// ReachExec forces the exec relay (see relay.go) without probing the
	// direct addresses first.
	ReachExec ControllerReachMode = "exec"
)

// controllerCrashLogLines is how much of a crash-looping controller's log is
// pulled into the deploy error. Enough for a startup failure (config, migration,
// backing-store connection) without burying the error in a build log.
const controllerCrashLogLines = 80

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
	// KubeNamespace is the namespace the backing's own pods live in, when the
	// backing is co-located in THIS cluster. Set it and the one-shot yb_storage
	// CREATE runs via `kubectl exec` in a tserver instead of dialling
	// AdminHostPorts from here — which needs a route into the pod CIDR that this
	// coordinator may simply not have. "" → dial (a genuinely remote cluster).
	KubeNamespace string
	// KubePod selects the pod to exec the CREATE in; "" → yb-tserver-0.
	KubePod string
}

// TLS is the chart's `tls.*` block plus the endpoint TLS mode: the controller
// gets a Certificate (CN = the backing user) and the CA bundle mounted, and
// renders one (CN = StorageRole) into every endpoint. The issuer and the bundle
// ConfigMap are the trust root deploy/tls/trust-root.sh stands up.
type TLS struct {
	Enabled           bool
	IssuerKind        string // "" → ClusterIssuer
	IssuerName        string // "" → amp-ca
	IssuerGroup       string // "" → cert-manager.io
	CABundleConfigMap string // "" → amp-ca-bundle
	StorageRole       string // the role endpoints connect as; "" → amp_storage
}

// Mounted by the controller chart when tls.enabled; the DSN names them.
const (
	tlsCAPath   = "/etc/amp/tls/ca/ca.crt"
	tlsCertPath = "/etc/amp/tls/client/tls.crt"
	tlsKeyPath  = "/etc/amp/tls/client/tls.key"
)

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

	// EndpointMemoryGiBPerVCPU is the memory-per-vCPU shape the deployed control
	// plane holds endpoints to. The product default is 8, and a suite whose point
	// is population cannot pay it — scale_tenants alone would reserve hundreds of
	// GiB — so these clusters run a lower one. 0 leaves the chart's own default.
	EndpointMemoryGiBPerVCPU int

	YBImage   string
	YBStorage string
	// YBStorageClass is the built-in yugabyted PVC's class, separate from
	// StorageClass (the endpoints') which defaults to emptyDir — no PVC at all.
	YBStorageClass string
	// YBMasterGflags / YBTserverGflags are CSV flag=value lists forwarded to
	// the built-in yugabyted's --master_flags / --tserver_flags. Builtin
	// backing only — external clusters own their flags. The
	// env.ampstack.yb_gflags knob feeds both sides.
	YBMasterGflags  string
	YBTserverGflags string
	StorageClass    string // endpoint /data PVC class; "" → emptyDir
	DBName          string

	// Backing selects the control-plane YugabyteDB. Zero value → the built-in
	// single-node yugabyted (current behaviour); external mode wires the
	// controller + endpoints at a cluster owned elsewhere (no yugabyted deployed,
	// never torn down here).
	Backing Backing

	// EndpointNodeSelector pins every endpoint pod the deployed control plane
	// creates, as a "k=v,k=v" CSV of node labels. Reaches the pods through the
	// chart's controller.endpoint.nodeSelector, which the controller forwards as
	// AMP_DEPLOY_NODE_SELECTOR.
	EndpointNodeSelector string
	// TLS puts the controller and every endpoint on cert-manager client
	// certificates against an external backing that demands them (a ybk8s
	// deploy with apps.ybk8s.tls.enabled). Off by default.
	TLS TLS

	// DeployTimeout is the controller's own endpoint helm --wait budget
	// (AMP_DEPLOY_TIMEOUT). Empty = the binary default (10m), which a multi-node
	// backing's first endpoint (node scale-up + image pull) does not fit.
	DeployTimeout string

	HelmShim   bool // true → install the --create-namespace-stripping helm shim
	MCPEnabled bool
	UIEnabled  bool

	// Registry picks where bare image refs resolve and which auth the pull
	// secret carries: RegistryQuay (default), RegistryECR or RegistryGHCR.
	Registry string

	ECRRegistry    string // ECR host (used when Registry is "ecr" or a ref points there); "" → DefaultECRRegistry
	Region         string
	PullSecretName string
	// PullSecretDockerConfigB64, when set, is used as the pull secret's
	// .dockerconfigjson verbatim (skips the aws ecr get-login-password path).
	PullSecretDockerConfigB64 string
	// GHCRUser/GHCRToken are GHCR pull credentials. GHCRToken empty falls
	// back to the local `gh auth token` (the gh CLI's stored login) when a
	// ghcr image needs pulling; GHCRUser defaults to "oauth" (ghcr accepts
	// any username with a valid token).
	GHCRUser  string
	GHCRToken string

	// QuayUser/QuayToken are quay.io robot credentials, added to the pull
	// secret alongside the minted ECR entry so envs can pull the
	// quay.io/yugabyte/amp-* images (custom/release builds). Empty = no quay auth.
	QuayUser  string
	QuayToken string

	ControllerService   string
	ControllerContainer string
	ControllerName      string
	ControllerPort      int
	EndpointServiceType string
	ControllerExpose    string
	HelmRelease         string

	// ControllerReach selects how the controller is reached once deployed:
	// ReachTailnet (default, "" also means tailnet) resolves its cluster-internal
	// address directly and port-forwards only if neither direct address answers;
	// ReachPortForward always uses the kubectl port-forward path.
	ControllerReach ControllerReachMode

	// RelayImage is the HTTP relay pod's image; "" → DefaultRelayImage.
	RelayImage string

	LocalPort int // controller port-forward local port; 0 → ephemeral

	YBReadyTimeout         time.Duration
	ControllerReadyTimeout time.Duration
	ControllerRollTimeout  time.Duration

	// ExtraNamespaceLabels are merged onto the namespace ampstack creates (in
	// addition to ampqa=1 and the managed-by label) — e.g. a reaper timestamp.
	ExtraNamespaceLabels map[string]string
}

// StackInfo is what a successful Deploy returns.
type StackInfo struct {
	Namespace         string
	ControllerService string
	ControllerPort    int
	ControllerURL     string // tailnet: the direct address; portforward (or a tailnet fallback): via the held forward
	Images            map[string]string
	Forward           *k8s.PortForward // owned by the App (released on Close); nil when a direct address was resolved
	PortForwardCmd    string           // human hint to re-establish the forward; "" when no forward is held
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
	ghcrToken func(ctx context.Context) (string, error)
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
	// execReach records that Deploy settled on the exec relay: controller HTTP
	// rides `kubectl exec` + curl through the relay pod, and there is no
	// forward and no coordinator-routable URL.
	execReach bool
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
	registry := get.GetString("env.ampstack.registry", RegistryQuay)
	defController, defEndpoint := DefaultQuayControllerImage, DefaultQuayEndpointImage
	if registry == RegistryECR {
		defController, defEndpoint = DefaultControllerImage, DefaultEndpointImage
	}
	return Config{
		Context:         get.GetString("env.ampstack.context", ""),
		Namespace:       get.GetString("env.ampstack.namespace", ""),
		Registry:        registry,
		ControllerImage: image("controller", "controller_image", defController),
		EndpointImage:   image("endpoint", "endpoint_image", defEndpoint),
		MCPImage:        image("mcp", "mcp_image", ""),
		UIImage:         image("ui", "ui_image", ""),
		ControllerChart: get.GetString("env.ampstack.controller_chart", ""),
		YBImage:         resolveYBImage(get),
		YBStorage:       get.GetString("env.ampstack.yb_storage", DefaultYBStorage),
		YBStorageClass:  get.GetString("env.ampstack.yb_storage_class", DefaultYBStorageClass),
		YBMasterGflags: joinGflags(get.GetString("env.ampstack.yb_gflags", ""),
			get.GetString("env.ampstack.yb_master_gflags", "")),
		YBTserverGflags: joinGflags(get.GetString("env.ampstack.yb_gflags", ""),
			get.GetString("env.ampstack.yb_tserver_gflags", "")),
		StorageClass: get.GetString("env.ampstack.storage_class", ""),
		// 2, not the product's 8: these clusters pack many small endpoints, and
		// the shapes the perf and scale suites were built around predate the
		// floor. Raise it to exercise the product's own shape, at the cost of
		// what those suites can fit.
		EndpointMemoryGiBPerVCPU: get.GetInt("env.ampstack.endpoint_memory_gib_per_vcpu", 2),
		DBName:                   get.GetString("env.ampstack.db_name", DefaultDBName),
		Backing: Backing{
			Mode:           BackingMode(get.GetString("env.ampstack.backing.mode", string(BackingBuiltinYugabyted))),
			HostPorts:      get.GetString("env.ampstack.backing.host_ports", ""),
			User:           get.GetString("env.ampstack.backing.user", ""),
			DBName:         get.GetString("env.ampstack.backing.db_name", ""),
			Password:       get.GetString("env.ampstack.backing.password", ""),
			SSLMode:        get.GetString("env.ampstack.backing.sslmode", ""),
			AdminHostPorts: get.GetString("env.ampstack.backing.admin_host_ports", ""),
		},
		TLS: TLS{
			Enabled:           get.GetBool("env.ampstack.tls.enabled", false),
			IssuerKind:        get.GetString("env.ampstack.tls.issuer_kind", "ClusterIssuer"),
			IssuerName:        get.GetString("env.ampstack.tls.issuer_name", "amp-ca"),
			IssuerGroup:       get.GetString("env.ampstack.tls.issuer_group", "cert-manager.io"),
			CABundleConfigMap: get.GetString("env.ampstack.tls.ca_configmap", "amp-ca-bundle"),
			StorageRole:       get.GetString("env.ampstack.tls.storage_role", "amp_storage"),
		},
		DeployTimeout:             get.GetString("env.ampstack.deploy_timeout", "20m"),
		EndpointNodeSelector:      get.GetString("env.ampstack.endpoint_node_selector", ""),
		HelmShim:                  get.GetBool("env.ampstack.helm_shim", true),
		MCPEnabled:                get.GetBool("env.ampstack.mcp_enabled", false),
		UIEnabled:                 get.GetBool("env.ampstack.ui_enabled", false),
		ECRRegistry:               get.GetString("env.ampstack.ecr_registry", DefaultECRRegistry),
		Region:                    get.GetString("env.ampstack.region", DefaultRegion),
		PullSecretName:            get.GetString("env.ampstack.pull_secret_name", DefaultPullSecretName),
		PullSecretDockerConfigB64: get.GetString("env.ampstack.pull_secret_b64", ""),
		QuayUser:                  get.GetString("env.ampstack.quay_user", ""),
		QuayToken:                 get.GetString("env.ampstack.quay_token", ""),
		GHCRUser:                  get.GetString("env.ampstack.ghcr_user", ""),
		GHCRToken:                 get.GetString("env.ampstack.ghcr_token", ""),
		ControllerService:         get.GetString("env.ampstack.controller_service", DefaultControllerService),
		ControllerContainer:       get.GetString("env.ampstack.controller_container", DefaultControllerContainer),
		ControllerName:            get.GetString("env.ampstack.controller_name", DefaultControllerName),
		ControllerPort:            get.GetInt("env.ampstack.controller_port", DefaultControllerPort),
		EndpointServiceType:       get.GetString("env.ampstack.endpoint_service_type", DefaultEndpointServiceType),
		ControllerExpose:          get.GetString("env.ampstack.controller_expose", DefaultControllerExpose),
		HelmRelease:               get.GetString("env.ampstack.helm_release", DefaultHelmRelease),
		ControllerReach:           ControllerReachMode(get.GetString("env.ampstack.controller_reach", string(ReachTailnet))),
		RelayImage:                get.GetString("env.ampstack.relay_image", DefaultRelayImage),
		LocalPort:                 get.GetInt("env.ampstack.local_port", 0),
		YBReadyTimeout:            get.GetDuration("env.ampstack.yb_ready_timeout", DefaultYBReadyTimeout),
		ControllerReadyTimeout:    get.GetDuration("env.ampstack.controller_ready_timeout", DefaultControllerReadyTimeout),
		ControllerRollTimeout:     get.GetDuration("env.ampstack.controller_roll_timeout", DefaultControllerRollTimeout),
	}
}

// resolveYBImage picks the built-in yugabyted image. A fully-qualified
// env.ampstack.yb_image always wins. Otherwise it is derived version-aware as
// "<env.ampstack.yb_image_registry>:<version>", where version is versions.yb
// (the run-level override) or versions.yugabytedb. When neither an explicit
// image nor a registry+version is available it falls back to DefaultYBImage.
func resolveYBImage(get Getter) string {
	if img := get.GetString("env.ampstack.yb_image", ""); img != "" {
		return img
	}
	registry := get.GetString("env.ampstack.yb_image_registry", DefaultYBImageRegistry)
	version := get.GetString("versions.yb", "")
	if version == "" {
		version = get.GetString("versions.yugabytedb", "")
	}
	if registry != "" && version != "" {
		return registry + ":" + version
	}
	return DefaultYBImage
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
		ghcrToken: defaultGHCRToken,
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
	if cfg.YBStorageClass == "" {
		cfg.YBStorageClass = DefaultYBStorageClass
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
		cfg.YBReadyTimeout = DefaultYBReadyTimeout
	}
	if cfg.ControllerReadyTimeout == 0 {
		cfg.ControllerReadyTimeout = DefaultControllerReadyTimeout
	}
	// A zero duration reaches kubectl as --timeout=0, which means wait forever.
	if cfg.ControllerRollTimeout <= 0 {
		cfg.ControllerRollTimeout = DefaultControllerRollTimeout
	}
}

// --- injection setters (composition + tests) ---

// WithKube replaces the namespace-bound kubectl (carrying its runner + starter),
// so a caller (env/exclusivek8s) or a test can drive the app through an injected
// transport. The kube's namespace should match cfg.Namespace.
func (a *App) WithKube(k *k8s.Kubectl) *App {
	if k != nil && a.logf != nil {
		k.WithLogf(a.logf)
	}
	a.kube = k
	return a
}

// Kube returns the app's namespace-bound kubectl (carrying its runner + starter),
// so a caller can gather failure diagnostics from the amp namespace before
// Teardown deletes it.
func (a *App) Kube() *k8s.Kubectl { return a.kube }

// WithHelmRunner overrides the runner used for `helm template`.
func (a *App) WithHelmRunner(r k8s.Runner) *App { a.helmRun = r; return a }

// WithGHCRToken overrides the ghcr token resolver (default: `gh auth token`).
func (a *App) WithGHCRToken(f func(ctx context.Context) (string, error)) *App {
	a.ghcrToken = f
	return a
}

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
func (a *App) WithLogf(f func(format string, args ...any)) *App {
	a.logf = f
	if a.kube != nil {
		a.kube.WithLogf(f)
	}
	return a
}

func (a *App) log(format string, args ...any) {
	if a.logf != nil {
		a.logf(format, args...)
	}
}

// Namespace is the stack's namespace.
func (a *App) Namespace() string { return a.cfg.Namespace }

// BackingNamespace is the namespace the backing YugabyteDB's own pods run in: an
// external backing that told us (a co-located ybk8s deploy, a provisioned YBA
// universe) reports its own; the built-in yugabyted shares this stack's. It is
// what a metrics sampler must be pointed at — the amp namespace holds no tserver
// when the backing lives elsewhere.
func (a *App) BackingNamespace() string {
	if ns := strings.TrimSpace(a.cfg.Backing.KubeNamespace); ns != "" {
		return ns
	}
	return a.cfg.Namespace
}

// registryPrefix is where bare image refs resolve, per the Registry knob.
func (c Config) registryPrefix() string {
	switch c.Registry {
	case RegistryECR:
		return c.ECRRegistry
	case RegistryGHCR:
		return GHCRNamespace
	default:
		return QuayNamespace
	}
}

// Images returns the resolved image refs the stack uses.
func (a *App) images() map[string]string {
	prefix := a.cfg.registryPrefix()
	m := map[string]string{
		"controller": withRegistry(a.cfg.ControllerImage, prefix),
		"endpoint":   withRegistry(a.cfg.EndpointImage, prefix),
	}
	if !a.externalBacking() {
		m["yugabyted"] = a.cfg.YBImage
	}
	if a.cfg.MCPEnabled && a.cfg.MCPImage != "" {
		m["mcp"] = withRegistry(a.cfg.MCPImage, prefix)
	}
	if a.cfg.UIEnabled && a.cfg.UIImage != "" {
		m["ui"] = withRegistry(a.cfg.UIImage, prefix)
	}
	return m
}

// needsECR reports whether any resolved image the stack deploys lives in the
// configured ECR registry. An empty ref means the chart default, whose
// registry is unknown here — counted against the SELECTED registry.
func (a *App) needsECR() bool {
	return a.needsRegistry(a.cfg.ECRRegistry+"/", a.cfg.Registry == RegistryECR)
}

// needsGHCR reports whether any deployed image lives on ghcr.
func (a *App) needsGHCR() bool {
	return a.needsRegistry(GHCRRegistry+"/", a.cfg.Registry == RegistryGHCR)
}

func (a *App) needsRegistry(prefix string, selected bool) bool {
	for _, img := range a.images() {
		if img == "" && selected {
			return true
		}
		if img != "" && strings.HasPrefix(img, prefix) {
			return true
		}
	}
	return false
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
	pullSecretApplied, err := a.ensurePullSecret(ctx)
	if err != nil {
		return StackInfo{}, err
	}

	// 3+4. YB backing store: either deploy a built-in yugabyted and create
	// yb_storage in-cluster, or ensure yb_storage on the external cluster. The
	// StatefulSet references the pull secret when one was applied so a private
	// YB image (e.g. a quay.io/yugabyte/yugabyte-itest preview build) pulls; a
	// pull secret on a public image is harmless.
	if a.externalBacking() {
		if err := a.ensureExternalStorageDB(ctx); err != nil {
			return StackInfo{}, err
		}
	} else {
		if err := a.kube.Apply(ctx, yugabytedManifest(a.cfg.Namespace, a.cfg.YBImage, a.cfg.YBStorage, a.cfg.YBStorageClass, a.cfg.YBMasterGflags, a.cfg.YBTserverGflags, pullSecretNameIf(pullSecretApplied, a.cfg.PullSecretName))); err != nil {
			return StackInfo{}, fmt.Errorf("apply yugabyted: %w", err)
		}
		if err := a.kube.WaitFor(ctx, "statefulset/"+yugabytedStatefulSet, "jsonpath={.status.readyReplicas}=1", a.cfg.YBReadyTimeout); err != nil {
			return StackInfo{}, fmt.Errorf("yugabyted never became ready: %w", err)
		}
		if err := a.createStorageDB(ctx); err != nil {
			return StackInfo{}, err
		}
	}

	// 5. controller (helm-template + filter + shim-inject + ONE apply, or a raw
	// manifest). The shim is folded into the rendered Deployment before the
	// apply: patching afterwards created a second ReplicaSet, and its pod raced
	// the first one's startup migrations against the fresh database.
	if err := a.deployController(ctx); err != nil {
		return StackInfo{}, err
	}

	// 7. wait controller Available.
	if err := a.kube.WaitFor(ctx, "deployment/"+a.cfg.ControllerName, "condition=Available", a.cfg.ControllerReadyTimeout); err != nil {
		return StackInfo{}, fmt.Errorf("controller deployment never became available: %w%s", err, a.controllerWaitDiagnostics(ctx))
	}

	// 8. reach the controller (tailnet: resolve its direct address; portforward:
	// the kubectl forward) + full readiness poll.
	url, pf, err := a.reachController(ctx)
	if err != nil {
		return StackInfo{}, err
	}
	if err := a.waitReady(ctx, a.clientFor(url), a.cfg.ControllerReadyTimeout); err != nil {
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

// controllerWaitDiagnostics gathers, best-effort, WHY the controller Deployment
// never went Available — its status conditions (ProgressDeadlineExceeded,
// MinimumReplicasUnavailable, …) and each controller pod's phase + container
// waiting/terminated reason (CrashLoopBackOff, ImagePullBackOff, …). This is the
// only capture: the pods are torn down right after the failure. Alongside the
// jsonpath reads it collects namespace EVENTS and the controller's own logs,
// because a pod that is up but not ready is explained by neither status nor
// silence — a failing readiness probe is reported only as an event. Neither
// echoes the env block holding the backing DSN the way `kubectl describe` would,
// and a password that reached the config joins the Masker, which every sink
// applies to this string (AGENTS.md §7). Runs on a short independent deadline so
// a cancelled/expired parent ctx still yields evidence; returns "" (never an
// error) when nothing could be read.
func (a *App) controllerWaitDiagnostics(ctx context.Context) string {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	var b strings.Builder
	if conds, err := a.kube.Get(dctx, "deployment", a.cfg.ControllerName,
		`{range .status.conditions[*]}{.type}={.status} ({.reason}) {.message}{"\n"}{end}`); err == nil {
		if conds = strings.TrimSpace(conds); conds != "" {
			b.WriteString("\ndeployment conditions:\n" + conds)
		}
	}
	if pods, err := a.kube.GetSelector(dctx, "pods", "app="+a.cfg.ControllerName,
		`{range .items[*]}{.metadata.name} phase={.status.phase}{range .status.containerStatuses[*]} {.name}: ready={.ready} restarts={.restartCount} state={.state}{end}{"\n"}{end}`); err == nil {
		if pods = strings.TrimSpace(pods); pods != "" {
			b.WriteString("\ncontroller pods:\n" + pods)
		}
	}
	// Why a pod that is UP is not READY is never in its status or its logs — a
	// failing readiness probe is reported only as an event, and a pod stuck
	// before its first log line has nothing else to say.
	if ev, err := a.kube.Events(dctx); err == nil {
		if ev = strings.TrimSpace(ev); ev != "" {
			b.WriteString("\nnamespace events:\n" + ev + "\n")
		}
	} else {
		fmt.Fprintf(&b, "\nnamespace events unavailable: %v\n", err)
	}
	b.WriteString(a.controllerCrashLogs(dctx))
	return b.String()
}

// controllerCrashLogs appends what the controller actually said before it died.
// Pod phase and restart counts prove a crash-loop but never explain it, and the
// namespace is torn down with the run — so without the logs inline in this error
// the failure is unexplainable after the fact. The previous instance's logs come
// first (the crash that started the loop); the current instance follows for a
// pod still on its first attempt.
func (a *App) controllerCrashLogs(ctx context.Context) string {
	pods, err := a.kube.ListPods(ctx)
	if err != nil {
		return fmt.Sprintf("\ncontroller logs unavailable: list pods: %v\n", err)
	}
	var b strings.Builder
	matched := 0
	for _, pod := range pods {
		if !strings.HasPrefix(pod, a.cfg.ControllerName+"-") {
			continue
		}
		matched++
		// A previous instance only exists once something restarted, so its
		// absence is normal and not worth reporting; the CURRENT instance's
		// logs are the ones whose absence has to be explained.
		if prev, err := a.kube.LogsAllPrevious(ctx, pod, controllerCrashLogLines); err == nil {
			if prev = strings.TrimSpace(prev); prev != "" {
				fmt.Fprintf(&b, "\n%s logs (previous instance, last %d lines):\n%s\n",
					pod, controllerCrashLogLines, prev)
			}
		}
		cur, err := a.kube.LogsAll(ctx, pod, controllerCrashLogLines)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "\n%s logs unavailable: %v\n", pod, err)
		case strings.TrimSpace(cur) == "":
			// Silence from a pod that is up is itself the finding — it means the
			// process has not reached its first log line, not that nothing broke.
			fmt.Fprintf(&b, "\n%s produced no log output at all\n", pod)
		default:
			fmt.Fprintf(&b, "\n%s logs (current instance, last %d lines):\n%s\n",
				pod, controllerCrashLogLines, strings.TrimSpace(cur))
		}
	}
	if matched == 0 {
		fmt.Fprintf(&b, "\nno pod named %s-* among %v\n", a.cfg.ControllerName, pods)
	}
	return b.String()
}

// reachController establishes how the controller will be reached for the rest
// of the stack's lifetime: tailnet mode resolves its direct cluster-internal
// address and only port-forwards when neither direct address answers;
// portforward mode opens the kubectl forward straight away.
func (a *App) reachController(ctx context.Context) (string, *k8s.PortForward, error) {
	if a.cfg.ControllerReach == ReachPortForward {
		return a.forwardController(ctx)
	}
	if a.cfg.ControllerReach == ReachExec {
		if err := a.ensureRelay(ctx); err != nil {
			return "", nil, err
		}
		a.setExecReach()
		return a.execControllerURL(), nil, nil
	}
	addr, directErr := a.resolveControllerAddr(ctx)
	if directErr == nil {
		return fmt.Sprintf("http://%s:%d", addr, a.cfg.ControllerPort), nil, nil
	}
	// No direct route from the coordinator. The exec relay is the reliable
	// fallback — a port-forward is a long-lived local process that dies
	// silently mid-run; each exec is one API-server round trip with no local
	// state. The forward remains only as the last resort when the relay pod
	// itself cannot come up.
	a.log("ampstack: %v; continuing over the exec relay", directErr)
	if relayErr := a.ensureRelay(ctx); relayErr == nil {
		a.setExecReach()
		return a.execControllerURL(), nil, nil
	} else {
		a.log("ampstack: %v; continuing over a kubectl port-forward", relayErr)
	}
	url, pf, err := a.forwardController(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("controller unreachable directly (%v) and the port-forward fallback failed: %w", directErr, err)
	}
	return url, pf, nil
}

func (a *App) setExecReach() {
	a.pfMu.Lock()
	a.execReach = true
	a.pfMu.Unlock()
}

func (a *App) isExecReach() bool {
	a.pfMu.Lock()
	defer a.pfMu.Unlock()
	return a.execReach
}

// clientFor is reach-aware: over the exec relay the url is an in-cluster
// address only curl can dial, so the client must carry the exec transport.
func (a *App) clientFor(url string) *controller.Client {
	if a.isExecReach() {
		return a.execClient()
	}
	return a.newClient(url)
}

// forwardController opens the kubectl port-forward to the controller Service and
// returns the local URL plus the handle the caller must hold and Close.
func (a *App) forwardController(ctx context.Context) (string, *k8s.PortForward, error) {
	pf, err := a.kube.PortForward(ctx, "svc/"+a.cfg.ControllerService, a.cfg.LocalPort, a.cfg.ControllerPort)
	if err != nil {
		return "", nil, fmt.Errorf("port-forward controller: %w", err)
	}
	return "http://" + pf.LocalAddr(), pf, nil
}

// resolveControllerAddr resolves the controller's stable, cluster-internal
// address for tailnet mode: the controller Service's ClusterIP when it
// answers /healthz, else the controller pod's IP — pod IPs on a tailnet-joined
// cluster are reachable directly from the coordinator (the same way an amp
// endpoint's pod IP already is). The error names every address tried, because
// it is what the caller logs before falling back to a port-forward.
func (a *App) resolveControllerAddr(ctx context.Context) (string, error) {
	clusterIP := "<unresolved>"
	if ip, err := a.kube.Get(ctx, "svc", a.cfg.ControllerService, "{.spec.clusterIP}"); err == nil {
		if ip = strings.TrimSpace(ip); ip != "" && ip != "None" {
			clusterIP = ip
			if a.probeDirectHealthz(ctx, ip) {
				return ip, nil
			}
		}
	}
	podIP, err := a.kube.GetSelector(ctx, "pod", "app="+a.cfg.ControllerName, "{.items[0].status.podIP}")
	if err != nil {
		return "", fmt.Errorf("controller ClusterIP %s unreachable; pod IP lookup failed: %w", clusterIP, err)
	}
	if podIP = strings.TrimSpace(podIP); podIP == "" {
		return "", fmt.Errorf("controller ClusterIP %s unreachable and no controller pod found (selector app=%s)", clusterIP, a.cfg.ControllerName)
	}
	if !a.probeDirectHealthz(ctx, podIP) {
		return "", fmt.Errorf("controller not reachable at ClusterIP %s nor pod IP %s on port %d", clusterIP, podIP, a.cfg.ControllerPort)
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
// pre-encoded value → skip (no secret needed, e.g. public images). Returns
// whether a secret was actually applied, so the caller can wire it into the
// built-in yugabyted StatefulSet's imagePullSecrets when the YB image is private.
func (a *App) ensurePullSecret(ctx context.Context) (bool, error) {
	b64 := a.cfg.PullSecretDockerConfigB64
	if b64 == "" {
		auths := map[string]registryAuth{}
		// quay robot creds (quay.io/yugabyte/amp-{controller,pg,endpoint} —
		// where custom/release builds land) sit alongside the minted ECR
		// entry in the same secret.
		if a.cfg.QuayUser != "" && a.cfg.QuayToken != "" {
			auths[QuayRegistry] = registryAuth{User: a.cfg.QuayUser, Password: a.cfg.QuayToken}
		}
		if a.needsGHCR() {
			token := a.cfg.GHCRToken
			if token == "" {
				t, err := a.ghcrToken(ctx)
				if err != nil {
					return false, fmt.Errorf("resolve ghcr token: set env.ampstack.ghcr_token or log in with `gh auth login`: %w", err)
				}
				token = t
			}
			user := a.cfg.GHCRUser
			if user == "" {
				user = "oauth"
			}
			auths[GHCRRegistry] = registryAuth{User: user, Password: token}
		}
		// ECR is only touched when a deployed image actually lives there: a
		// pure-quay (or public-image) run needs no AWS identity at all.
		if a.cfg.ECRRegistry != "" && a.needsECR() {
			token, err := a.ecrToken(ctx, a.cfg.ECRRegistry, a.cfg.Region)
			if err != nil {
				return false, fmt.Errorf("resolve ECR token: %w", err)
			}
			auths[a.cfg.ECRRegistry] = registryAuth{User: "AWS", Password: token}
		}
		if len(auths) == 0 {
			return false, nil
		}
		b64 = dockerConfigJSONB64(auths)
	}
	if err := a.kube.Apply(ctx, pullSecretManifest(a.cfg.Namespace, a.cfg.PullSecretName, b64)); err != nil {
		return false, fmt.Errorf("apply pull secret: %w", err)
	}
	return true, nil
}

// RefreshPullSecret re-mints the ECR docker-registry pull secret and re-applies
// it to the env namespace, healing a long-standing control plane whose amp images
// stopped pulling after the ~12h ECR login-token TTL. It verifies the namespace
// is ampqa-managed first (the same guard as Reap), so it only ever touches an
// ampqa-owned env. When roll is true it also rolls the controller Deployment so
// it re-pulls immediately instead of waiting out ImagePullBackOff. A public-image
// env (no ecr_registry, no pull_secret_b64) has nothing to refresh and errors.
func (a *App) RefreshPullSecret(ctx context.Context, roll bool) error {
	if a.cfg.ECRRegistry == "" && a.cfg.PullSecretDockerConfigB64 == "" && (a.cfg.QuayUser == "" || a.cfg.QuayToken == "") && !a.needsGHCR() {
		return fmt.Errorf("nothing to refresh: no ecr_registry, quay creds, ghcr, or pull_secret_b64 configured (amp images are public)")
	}
	label, err := a.kube.Get(ctx, "namespace", a.cfg.Namespace, "{.metadata.labels."+labelPath(ManagedByLabel)+"}")
	if err != nil {
		return fmt.Errorf("read managed-by label on %s: %w", a.cfg.Namespace, err)
	}
	if label != ManagedByValue {
		return fmt.Errorf("refusing to touch namespace %s: not ampqa-managed (%s=%q, want %q)",
			a.cfg.Namespace, ManagedByLabel, label, ManagedByValue)
	}
	if _, err := a.ensurePullSecret(ctx); err != nil {
		return err
	}
	if roll {
		if err := a.kube.Patch(ctx, "deployment", a.cfg.ControllerName, "strategic", rolloutRestartPatch()); err != nil {
			return fmt.Errorf("roll controller %s: %w", a.cfg.ControllerName, err)
		}
		// condition=Available stays true on the old ReplicaSet through a rolling
		// update, so only a rollout wait can see the new pod fail to come up.
		if err := a.kube.RolloutStatus(ctx, "deployment", a.cfg.ControllerName, a.cfg.ControllerRollTimeout); err != nil {
			return fmt.Errorf("rolled controller %s did not come back up within %s (the refreshed pull secret may still not pull): %w%s",
				a.cfg.ControllerName, a.cfg.ControllerRollTimeout, err, a.controllerWaitDiagnostics(ctx))
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

// deployController renders the chart (helm template) and applies it as rendered —
// or applies a raw ControllerManifest verbatim when one is set.
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
	manifest := rendered
	if strings.TrimSpace(manifest) == "" {
		return nil
	}
	if a.cfg.HelmShim {
		if err := a.kube.Apply(ctx, shimConfigMapManifest(a.cfg.Namespace)); err != nil {
			return fmt.Errorf("apply helm shim configmap: %w", err)
		}
		if manifest, err = injectShim(manifest, a.cfg.ControllerName, a.cfg.ControllerContainer); err != nil {
			return err
		}
	}
	if err := a.kube.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("apply controller manifest: %w", err)
	}
	return nil
}

// UpgradeImages re-renders and re-applies the control plane with new controller
// and/or endpoint images (an empty argument leaves that image as it is), then
// blocks until the controller rollout completes.
func (a *App) UpgradeImages(ctx context.Context, controllerImage, endpointImage string) error {
	if controllerImage == "" && endpointImage == "" {
		return fmt.Errorf("ampstack: UpgradeImages needs at least one image")
	}
	if controllerImage != "" {
		a.cfg.ControllerImage = controllerImage
	}
	if endpointImage != "" {
		a.cfg.EndpointImage = endpointImage
	}
	if err := a.deployController(ctx); err != nil {
		return err
	}
	// Deploy's condition=Available wait stays true on the OLD ReplicaSet for the
	// whole rolling update, so a crash-looping new image would sail past it.
	if err := a.kube.RolloutStatus(ctx, "deployment", a.cfg.ControllerName, a.cfg.ControllerRollTimeout); err != nil {
		return fmt.Errorf("controller rollout to %s never completed: %w%s",
			a.images()["controller"], err, a.controllerWaitDiagnostics(ctx))
	}
	return nil
}

// chartCapabilityCRDs are the CRDs the chart gates documents on through
// .Capabilities.APIVersions. `helm template` has no cluster to ask, so those
// gates are false unless the group/version is passed with --api-versions.
var chartCapabilityCRDs = []struct {
	crd   string
	group string
}{
	{crd: "opentelemetrycollectors.opentelemetry.io", group: "opentelemetry.io"},
	// Not a document gate but a precondition: with TLS on and the chart not
	// installing cert-manager, the render fails unless the CRD is declared.
	{crd: "certificates.cert-manager.io", group: "cert-manager.io"},
}

// renderAPIVersions returns the group/versions to declare to `helm template` so
// the render matches what the cluster can reconcile. An absent CRD contributes
// nothing; a failed lookup is treated as absent rather than failing the deploy.
func (a *App) renderAPIVersions(ctx context.Context) []string {
	if a.kube == nil {
		return nil
	}
	cluster := a.kube.InNamespace("") // CRDs are cluster-scoped
	var out []string
	for _, c := range chartCapabilityCRDs {
		served, err := cluster.Get(ctx, "crd", c.crd,
			`{range .spec.versions[?(@.served)]}{.name}{"\n"}{end}`)
		if err != nil {
			if a.logf != nil {
				a.logf("ampstack: %s not found, rendering the chart without %s (%v)", c.crd, c.group, err)
			}
			continue
		}
		for _, v := range strings.Fields(served) {
			out = append(out, c.group+"/"+v)
		}
	}
	sort.Strings(out)
	return out
}

// helmTemplate renders the controller chart to a manifest string.
func (a *App) helmTemplate(ctx context.Context) (string, error) {
	if err := a.helmDependencyBuild(ctx); err != nil {
		return "", err
	}
	argv := a.helmTemplateArgs(a.renderAPIVersions(ctx))
	if a.logf != nil {
		a.logf("exec: %s", k8s.CmdString(argv))
	}
	res, err := a.helmRun(ctx, nil, argv)
	if a.logf != nil {
		a.logf("exec done (exit %d):%s%s", res.ExitCode, k8s.LogTail("out", res.Stdout), k8s.LogTail("err", res.Stderr))
	}
	if err != nil {
		return "", fmt.Errorf("helm template: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("helm template exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// helmDependencyBuild fetches the chart's subcharts. The controller chart
// declares cert-manager, and helm refuses to render a chart whose declared
// dependency is missing from charts/ even when its condition is false.
func (a *App) helmDependencyBuild(ctx context.Context) error {
	argv := []string{"helm", "dependency", "build", a.cfg.ControllerChart}
	if a.logf != nil {
		a.logf("exec: %s", k8s.CmdString(argv))
	}
	res, err := a.helmRun(ctx, nil, argv)
	if err != nil {
		return fmt.Errorf("helm dependency build: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("helm dependency build exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// helmTemplateArgs builds the `helm template` argv mirroring the --set flags
// deploy/controller/redeploy.sh uses (namespace-scoped: controller Service
// stays ClusterIP-only via controller.service.type=none).
func (a *App) helmTemplateArgs(apiVersions []string) []string {
	prefix := a.cfg.registryPrefix()
	ctrlRepo, ctrlTag := splitImageRef(withRegistry(a.cfg.ControllerImage, prefix))
	epRepo, epTag := splitImageRef(withRegistry(a.cfg.EndpointImage, prefix))

	sets := map[string]string{
		"imagePullSecrets[0].name":             a.cfg.PullSecretName,
		"controller.deployTimeout":             a.cfg.DeployTimeout,
		"controller.image.repository":          ctrlRepo,
		"controller.image.tag":                 ctrlTag,
		"controller.endpoint.image.repository": epRepo,
		"controller.endpoint.image.tag":        epTag,
		"controller.endpoint.ybUser":           a.endpointUser(),
		"controller.endpoint.ybDbname":         a.storageDBName(),
		"controller.endpoint.service.type":     a.cfg.EndpointServiceType,
		"controller.endpoint.storageClass":     a.cfg.StorageClass,
		"controller.endpoint.memoryGiBPerVCPU": strconv.Itoa(a.cfg.EndpointMemoryGiBPerVCPU),
		"controller.service.type":              a.cfg.ControllerExpose,
		"mcp.enabled":                          boolStr(a.cfg.MCPEnabled),
		"ui.enabled":                           boolStr(a.cfg.UIEnabled),
		// cert-manager belongs to the cluster, not to this release: a run
		// deploys many stacks onto one, and two owners collide on its
		// cluster-scoped objects.
		"certManager.install": "false",
	}
	if a.cfg.MCPEnabled && a.cfg.MCPImage != "" {
		r, t := splitImageRef(withRegistry(a.cfg.MCPImage, prefix))
		sets["mcp.image.repository"] = r
		sets["mcp.image.tag"] = t
	}
	if a.cfg.UIEnabled && a.cfg.UIImage != "" {
		r, t := splitImageRef(withRegistry(a.cfg.UIImage, prefix))
		sets["ui.image.repository"] = r
		sets["ui.image.tag"] = t
	}
	maps.Copy(sets, a.tlsHelmSets())

	// --include-crds: helm template omits a chart's crds/ directory by default, so
	// without it the rendered stack carries no traefik.io CRDs and an ingress_*
	// endpoint's release fails at apply.
	argv := []string{"helm", "template", a.cfg.HelmRelease, a.cfg.ControllerChart, "--namespace", a.cfg.Namespace, "--include-crds"}
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
	for i, pair := range endpointNodeSelectorPairs(a.cfg.EndpointNodeSelector) {
		argv = append(argv, "--set-string", fmt.Sprintf("controller.endpoint.nodeSelector[%d]=%s", i, helmEscape(pair)))
	}
	argv = append(argv, "--set-string", "controller.endpoint.ybHostPorts="+helmEscape(a.backingHostPorts()))
	argv = append(argv, "--set-string", "controller.databaseUrl="+helmEscape(a.dbURL()))
	for _, v := range apiVersions {
		argv = append(argv, "--api-versions", v)
	}
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

// dbURL is the controller's own backing DSN (AMP_DATABASE_URL). In external
// mode it carries every host:port of the CSV, not just the first, so the
// controller can fail over to another tserver the way the endpoints already do
// — pgx expands a multi-host DSN into one fallback per host and dials them in
// order. Taking only the first entry single-homed the control plane onto one
// tserver pod for no reason.
//
// Every entry is a name, never a pod IP: a reschedule invalidates an IP, and
// re-resolving the name on the next dial is what lands the reconnect on the
// replacement pod. Built-in mode has a single yugabyted pod, so there is
// nothing to list.
//
// This is hardening, not a fix for a seen outage: the backings these suites run
// against today are single-tserver, so the CSV has one entry and this changes
// nothing for them. The two-minute control-plane stall in #961 was a pool
// holding dead sockets, and is fixed controller-side.
func (a *App) dbURL() string {
	if a.cfg.TLS.Enabled {
		// The controller authenticates with the certificate the chart mounts;
		// no password, whatever the backing block says. The host list still
		// depends on which backing this is: Backing.HostPorts is empty on the
		// built-in path, and an empty list here would produce a DSN with no
		// host at all rather than falling through to the ybFQDN form.
		hosts := hostPortList(a.cfg.Backing.HostPorts)
		if !a.externalBacking() {
			hosts = fmt.Sprintf("%s:5433", a.ybFQDN())
		}
		return fmt.Sprintf("postgres://%s@%s/%s?sslmode=verify-full&sslrootcert=%s&sslcert=%s&sslkey=%s",
			a.backingUser(), hosts, a.storageDBName(), tlsCAPath, tlsCertPath, tlsKeyPath)
	}
	if a.externalBacking() {
		return backingDSN(hostPortList(a.cfg.Backing.HostPorts), a.backingUser(), a.cfg.Backing.Password, a.storageDBName(), orDefault(a.cfg.Backing.SSLMode, DefaultBackingSSLMode))
	}
	return fmt.Sprintf("postgres://%s@%s:5433/%s?sslmode=%s", DefaultBackingUser, a.ybFQDN(), a.cfg.DBName, DefaultBackingSSLMode)
}

// endpointUser is the YSQL role endpoints connect as: the storage role under
// TLS (its certificate's CN), else the backing user.
func (a *App) endpointUser() string {
	if a.cfg.TLS.Enabled {
		return orDefault(a.cfg.TLS.StorageRole, "amp_storage")
	}
	return a.backingUser()
}

// tlsHelmSets is the controller chart's `tls.*` block and the endpoint TLS
// mode; empty when TLS is off.
func (a *App) tlsHelmSets() map[string]string {
	if !a.cfg.TLS.Enabled {
		return nil
	}
	return map[string]string{
		"tls.enabled":           "true",
		"tls.issuerRef.kind":    orDefault(a.cfg.TLS.IssuerKind, "ClusterIssuer"),
		"tls.issuerRef.name":    orDefault(a.cfg.TLS.IssuerName, "amp-ca"),
		"tls.issuerRef.group":   orDefault(a.cfg.TLS.IssuerGroup, "cert-manager.io"),
		"tls.caBundleConfigMap": orDefault(a.cfg.TLS.CABundleConfigMap, "amp-ca-bundle"),
		// The CN of the controller's certificate has to be the role its DSN
		// logs in as: the HBA this stack installs authenticates by CN, so a
		// configured backing user with the chart's default CN cannot connect.
		"tls.controllerUser":            a.backingUser(),
		"controller.endpoint.ybTlsMode": "verify-full",
	}
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
	// Co-located: run it where the database is. Dialling the backing from here
	// needs a route into the pod CIDR, and where that is absent every attempt
	// times out against a healthy cluster — three call sites failed that way on
	// 0.260810.0 while `select 1` answered from inside the tserver. The built-in
	// yugabyted path has always created its database in-pod; this is the same.
	if ns := strings.TrimSpace(a.cfg.Backing.KubeNamespace); ns != "" {
		return a.ensureStorageDBInCluster(ctx, ns)
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

// ensureStorageDBInCluster creates the amp storage database by exec-ing into a
// tserver of the co-located backing, reusing the same idempotent script the
// built-in yugabyted path runs.
func (a *App) ensureStorageDBInCluster(ctx context.Context, namespace string) error {
	pod := a.backingPod()
	kube := a.kube.InNamespace(namespace).ForPod(pod, backingContainer)
	res, err := kube.Run(ctx, createStorageDBScript(a.storageDBName()))
	if err != nil {
		return fmt.Errorf("ensure %s in %s/%s: %w", a.storageDBName(), namespace, pod, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("ensure %s in %s/%s exited %d: %s",
			a.storageDBName(), namespace, pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// defaultBackingPod / backingContainer name where a co-located YB-in-k8s backing
// keeps a tserver able to run ysqlsh.
const (
	defaultBackingPod = "yb-tserver-0"
	backingContainer  = "yb-tserver"
)

// backingPod is the co-located backing's tserver pod: the one the store named,
// else the first of its StatefulSet.
func (a *App) backingPod() string {
	if pod := strings.TrimSpace(a.cfg.Backing.KubePod); pod != "" {
		return pod
	}
	return defaultBackingPod
}

// BackingVersion live-reads the backing YugabyteDB's `select version()`. It runs
// the query where the database is — in the pod for the built-in yugabyted and
// for a co-located store, dialling only a backing that named no kube location —
// because the address the controller uses is in-cluster and this process may
// have no route to it at all. Best-effort: "" on any failure, never fatal.
func (a *App) BackingVersion(ctx context.Context) string {
	access := a.StorageAccess()
	if access.ForwardTarget == "" {
		return queryBackingVersion(ctx, access.Conn)
	}
	pod, container, ns := yugabytedPod, "", ""
	if a.externalBacking() {
		pod, container, ns = a.backingPod(), backingContainer, access.ForwardNamespace
	}
	kube := a.kube
	if ns != "" {
		kube = kube.InNamespace(ns)
	}
	res, err := kube.ForPod(pod, container).Run(ctx, selectVersionScript())
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
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

// probeHealthz checks the controller over whatever reach Deploy settled on: the
// held forward when there is one (alive, else re-opened for the probe), else the
// resolved direct URL — and, when no direct address answers, a short-lived
// forward, the same fallback reachController takes. A probe is a one-off status
// check, not the long-lived compute-phase path — see ControllerClient for the
// reconnecting accessor that path uses.
func (a *App) probeHealthz(ctx context.Context) error {
	if a.isExecReach() {
		return a.execClient().Healthz(ctx)
	}
	a.pfMu.Lock()
	pf := a.pf
	a.pfMu.Unlock()
	switch {
	case pf != nil && pf.Alive():
		return a.newClient("http://" + pf.LocalAddr()).Healthz(ctx)
	case pf == nil && a.cfg.ControllerReach != ReachPortForward:
		url, err := a.directControllerURL(ctx)
		if err == nil {
			return a.newClient(url).Healthz(ctx)
		}
		a.log("ampstack: %v; probing over a short-lived kubectl port-forward", err)
	}
	tmp, err := a.kube.PortForward(ctx, "svc/"+a.cfg.ControllerService, a.cfg.LocalPort, a.cfg.ControllerPort)
	if err != nil {
		return err
	}
	defer tmp.Close()
	return a.newClient("http://" + tmp.LocalAddr()).Healthz(ctx)
}

// directControllerURL returns the reach URL Deploy settled on, resolving a fresh
// direct address (ClusterIP → pod-IP fallback) if called before Deploy has run.
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

// ControllerClient returns a live controller client for whatever reach Deploy
// settled on. With a direct address (tailnet mode, the default) this is the
// stable URL Deploy resolved — a ClusterIP or pod IP, nothing to reconnect.
// Whenever a forward is held — portforward mode, or tailnet falling back on an
// unroutable cluster — it re-establishes that forward first if the underlying
// kubectl process has died (Alive() false); the forward is meant to survive
// the whole compute phase (Deploy through Teardown), not just Deploy's own
// health check, since kubectl port-forward is known-flaky over a long-running
// test. Either way, any caller about to make a controller call should fetch
// the client through here rather than caching a URL from Deploy's StackInfo.
// Safe for concurrent use.
func (a *App) ControllerClient(ctx context.Context) (*controller.Client, string, error) {
	a.pfMu.Lock()
	defer a.pfMu.Unlock()
	if a.execReach {
		return a.execClient(), a.execControllerURL(), nil
	}
	if a.pf == nil && a.cfg.ControllerReach != ReachPortForward {
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

// ReconnectController drops the cached controller address and re-establishes it
// with Deploy's own resolve-then-forward chain. A direct address can be a pod
// IP (the chart's Service may not be routable from outside the cluster), and a
// pod replacement silently invalidates it — run #213 kept dialing a dead pod IP
// for twenty minutes while a healthy replacement served traffic. Reaching the
// stack has to survive that, not stay pinned to a pod that no longer exists.
func (a *App) ReconnectController(ctx context.Context) (*controller.Client, string, error) {
	a.pfMu.Lock()
	if a.pf != nil {
		_ = a.pf.Close()
		a.pf = nil
	}
	a.controllerURL = ""
	a.pfMu.Unlock()

	url, pf, err := a.reachController(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("reconnect controller: %w", err)
	}
	a.pfMu.Lock()
	a.pf = pf
	a.controllerURL = url
	a.pfMu.Unlock()
	return a.clientFor(url), url, nil
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
	return reapNamespace(ctx, a.clusterKube(), a.cfg.Namespace)
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
	return reapNamespace(ctx, kube, namespace)
}

// reapNamespace is the one teardown behind Teardown and Reap: refuse a namespace
// without the managed-by label, sweep the controller chart's cluster-scoped RBAC,
// delete the namespace.
func reapNamespace(ctx context.Context, kube *k8s.Kubectl, namespace string) error {
	label, err := kube.Get(ctx, "namespace", namespace, "{.metadata.labels."+labelPath(ManagedByLabel)+"}")
	if err != nil {
		return fmt.Errorf("read managed-by label on %s: %w", namespace, err)
	}
	if label != ManagedByValue {
		return fmt.Errorf("refusing to delete namespace %s: not ampqa-managed (%s=%q, want %q)",
			namespace, ManagedByLabel, label, ManagedByValue)
	}
	if err := deleteControllerClusterRBAC(ctx, kube, namespace); err != nil {
		return err
	}
	if err := kube.DeleteNamespace(ctx, namespace); err != nil {
		return fmt.Errorf("delete namespace %s: %w", namespace, err)
	}
	return nil
}

// controllerChartSelector matches every object the yb-amp-controller chart
// renders, including its cluster-scoped RBAC.
const controllerChartSelector = "app.kubernetes.io/name=yb-amp-controller"

// deleteControllerClusterRBAC removes the ClusterRoleBindings the controller
// chart rendered for namespace, and the ClusterRoles they bind. The chart's
// default rbac.nodeRead=true renders one pair per install, named
// "<namespace>-<controller>-nodes", and a namespace delete does not touch
// cluster-scoped kinds — so without this every run leaves a pair behind. The
// binding's subject namespace is what ties it to this install; the label keeps
// the sweep to the chart's own objects.
func deleteControllerClusterRBAC(ctx context.Context, kube *k8s.Kubectl, namespace string) error {
	out, err := kube.GetSelector(ctx, "clusterrolebinding", controllerChartSelector,
		`{range .items[*]}{.metadata.name}{" "}{.roleRef.name}{" "}{.subjects[*].namespace}{"\n"}{end}`)
	if err != nil {
		return fmt.Errorf("list the controller chart's clusterrolebindings: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !slices.Contains(f[2:], namespace) {
			continue
		}
		if err := kube.Delete(ctx, "clusterrolebinding", f[0]); err != nil {
			return fmt.Errorf("delete clusterrolebinding %s: %w", f[0], err)
		}
		if err := kube.Delete(ctx, "clusterrole", f[1]); err != nil {
			return fmt.Errorf("delete clusterrole %s: %w", f[1], err)
		}
	}
	return nil
}

// endpointNodeSelectorPairs reads a "k=v,k=v" CSV into the chart's list form,
// sorted so the rendered manifest is stable.
func endpointNodeSelectorPairs(csv string) []string {
	var out []string
	for _, pair := range strings.Split(csv, ",") {
		pair = strings.TrimSpace(pair)
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			continue
		}
		out = append(out, pair)
	}
	sort.Strings(out)
	return out
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

// pullSecretNameIf returns name when applied is true, else "" — so the built-in
// yugabyted StatefulSet only carries imagePullSecrets when a secret was actually
// applied to the namespace.
func pullSecretNameIf(applied bool, name string) string {
	if applied {
		return name
	}
	return ""
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

// hostPortList normalises a CSV of tserver addresses into the authority a
// multi-host postgres:// URL takes: every entry as "host:port", ports defaulted
// to 5433, joined by commas. pgx splits that back into parallel host/port lists
// and builds one fallback per entry, giving libpq-style failover across the
// whole CSV. Empty entries are dropped; an empty CSV yields "", which external
// mode already refuses before any DSN is rendered.
func hostPortList(csv string) string {
	var out []string
	for _, entry := range strings.Split(csv, ",") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		host, port := firstHostPort(entry)
		out = append(out, net.JoinHostPort(host, strconv.Itoa(port)))
	}
	return strings.Join(out, ",")
}

// backingDSN renders a postgres:// URL for the controller's AMP_DATABASE_URL.
// authority is one or more comma-separated "host:port" (see hostPortList).
// Password, when set, is included; sslmode defaults to disable.
func backingDSN(authority, user, password, dbName, sslMode string) string {
	userInfo := user
	if password != "" {
		userInfo = user + ":" + password
	}
	return fmt.Sprintf("postgres://%s@%s/%s?sslmode=%s", userInfo, authority, dbName, orDefault(sslMode, DefaultBackingSSLMode))
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
// defaultGHCRToken asks the local gh CLI for its stored token — the default
// auth for ghcr images when no explicit ghcr_token is configured.
func defaultGHCRToken(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func defaultECRToken(ctx context.Context, _ /*registry*/, region string) (string, error) {
	cmd := exec.CommandContext(ctx, "aws", "ecr", "get-login-password", "--region", region)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("aws ecr get-login-password: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
