// Package config loads amp controller configuration from environment
// variables, with command-line flags taking precedence.
package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LogConfig controls the structured logger.
type LogConfig struct {
	Level  string // debug | info | warn | error
	Format string // json | text
}

// DBConfig points at the YBDB/PostgreSQL state store.
type DBConfig struct {
	URL    string // YSQL connection string (required)
	Schema string // schema that holds the control-plane tables
}

// TaskConfig tunes the task framework worker.
type TaskConfig struct {
	PollInterval time.Duration
	LeaseTTL     time.Duration
	// MaxConcurrent caps how many tasks the worker executes at once; it only
	// claims a task when a slot is free, so it never holds a lease on a task it
	// can't run.
	MaxConcurrent int
	// ShutdownGrace bounds how long Run waits for in-flight tasks to finish after
	// its context is cancelled; past it, Run returns and the still-running tasks
	// are left to be reclaimed (their leases expire), so a misbehaving step can't
	// wedge process exit.
	ShutdownGrace time.Duration
}

// StorageControllerConfig configures the storage controller that the controller
// runs as a managed subprocess (amp-controller.md §11.1). The
// storage controller is a managed service: the controller spawns it, supervises
// it (restart on crash, clean stop on shutdown), and is its only caller.
type StorageControllerConfig struct {
	Enabled      bool          // run the storage controller as a managed subprocess
	BinPath      string        // path to the storage_controller binary
	ListenAddr   string        // host:port the storage controller serves HTTP on
	DatabaseURL  string        // universe connection for the storage controller; defaults to DB.URL
	Dev          bool          // pass --dev (run without auth); dev/local only
	ExtraArgs    []string      // extra raw args appended to the invocation (programmatic extension point; not flag-wired)
	StartTimeout time.Duration // how long to wait for the first readiness probe to pass
	StopTimeout  time.Duration // grace period after SIGTERM before SIGKILL
}

// DeployConfig configures how the controller provisions compute endpoints
// (amp-controller.md §11, issue #108). Runtime-specific deploy logic lives
// behind the deploy.Deployer interface (principle 2/3); Driver selects the
// implementation, and this block carries both drivers' deployment
// configuration (§9). It is a no-op unless Enabled.
type DeployConfig struct {
	Enabled bool   // enable the endpoint deployer
	Driver  string // deployer driver: "docker" (default) or "kubernetes" (Helm)

	// --- Kubernetes (Helm) runtime ---
	Namespace      string // namespace endpoints are deployed into
	HelmBin        string // helm binary; bare name resolved via PATH, a path used verbatim
	ChartPath      string // path to the yb-light-endpoint Helm chart
	KubeconfigPath string // KUBECONFIG for helm; empty = default discovery (in-cluster or ~/.kube)

	EndpointImageRepo  string            // endpoint container image repository
	EndpointImageTag   string            // endpoint container image tag
	PullSecret         string            // imagePullSecrets name in the namespace (empty = none)
	ComputePort        int               // compute Postgres port the endpoint Service publishes
	ServiceType        string            // endpoint Service type: "ClusterIP" (default), "NodePort", or "LoadBalancer"
	ServiceAnnotations map[string]string // annotations on the endpoint Service (e.g. GKE internal-LB)

	// StorageClass provisions the endpoint's data volume (/data: pgdata, LFC,
	// logs) as a PersistentVolumeClaim of that class — a network block volume
	// (EBS on AWS, Persistent Disk on GCP). Empty falls back to an ephemeral
	// emptyDir (no persistent volume). DiskSize is that PVC's requested size.
	StorageClass string
	DiskSize     string

	// --- Docker runtime ---
	EndpointImage  string // single-container endpoint image to run
	Network        string // shared Docker network for the controller + its endpoints
	ExternalHost   string // host a client uses to reach a published compute port
	SupervisorPath string // dev-only: bind-mount a host supervisor.py into endpoints (empty = bundled)

	// StorageControllerURL is the URL deployed endpoints use to reach the
	// controller-managed storage controller (e.g. http://host.docker.internal:50051).
	// When set, the endpoint registers its pageserver + safekeeper, takes its
	// generation from the attach-hook, and registers the timeline — the
	// storage-controller-coordinated path (controller-project-create.md). Empty = the
	// endpoint runs storage-controller-free with a pinned generation.
	StorageControllerURL string

	// Bootstrap initdb's the (already-recorded) tenant/timeline when an endpoint
	// is deployed (the recipe model), instead of attaching an existing one. The
	// first endpoint for a timeline materializes its storage this way; there is
	// no separate create-database flow yet. Honored by both drivers. Off by default.
	Bootstrap bool

	// The universe the deployed endpoints use as their durable backend. This is
	// the address the pods/containers reach (which may differ from the
	// controller's own --database-url network path to the same YugabyteDB).
	BackingYBHostPorts string // host:port[,...] of the YB tservers, as the endpoints reach them
	BackingYBUser      string
	BackingYBDBName    string
	// Password delivery, in precedence order: a pre-existing Secret
	// (BackingYBExistingSecret) keeps the plaintext off the controller (k8s only);
	// an inline BackingYBPassword is the dev/test fallback; empty = passwordless.
	BackingYBExistingSecret    string
	BackingYBExistingSecretKey string
	BackingYBPassword          string

	Timeout time.Duration // how long to wait for a deploy/retire to converge (helm --wait / container ready)
}

// Deploy drivers.
const (
	DriverKubernetes = "kubernetes"
	DriverDocker     = "docker"
)

// Config is the fully-resolved controller configuration.
type Config struct {
	ListenAddr        string
	SigningKeyPath    string // optional; controller generates an ephemeral key if unset
	Log               LogConfig
	DB                DBConfig
	Task              TaskConfig
	StorageController StorageControllerConfig
	Deploy            DeployConfig
}

var schemaRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// dns1123Re matches a Kubernetes DNS-1123 label (lowercase alphanumeric and
// '-', starting and ending alphanumeric); length is checked separately.
var dns1123Re = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Load resolves configuration from args (flags) layered over the environment.
func Load(args []string) (*Config, error) {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	c := &Config{}

	fs.StringVar(&c.ListenAddr, "listen", env("AMP_LISTEN", ":8080"), "HTTP listen address")
	fs.StringVar(&c.DB.URL, "database-url", env("AMP_DATABASE_URL", ""), "YBDB/PostgreSQL connection string (required)")
	fs.StringVar(&c.DB.Schema, "db-schema", env("AMP_DB_SCHEMA", "controller"), "schema that holds the control-plane tables")
	fs.StringVar(&c.Log.Level, "log-level", env("AMP_LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	fs.StringVar(&c.Log.Format, "log-format", env("AMP_LOG_FORMAT", "json"), "log format: json|text")
	fs.StringVar(&c.SigningKeyPath, "signing-key", env("AMP_SIGNING_KEY_PATH", ""), "path to the control-plane signing key (optional)")
	fs.DurationVar(&c.Task.PollInterval, "task-poll-interval", envDuration("AMP_TASK_POLL_INTERVAL", 2*time.Second), "task worker poll interval")
	fs.DurationVar(&c.Task.LeaseTTL, "task-lease-ttl", envDuration("AMP_TASK_LEASE_TTL", 30*time.Second), "task ownership lease TTL")
	fs.IntVar(&c.Task.MaxConcurrent, "task-max-concurrent", envInt("AMP_TASK_MAX_CONCURRENT", 50), "max tasks the worker executes concurrently")
	fs.DurationVar(&c.Task.ShutdownGrace, "task-shutdown-grace", envDuration("AMP_TASK_SHUTDOWN_GRACE", 10*time.Second), "max time to wait for in-flight tasks to finish on shutdown")

	sc := &c.StorageController
	fs.BoolVar(&sc.Enabled, "storage-controller", envBool("AMP_STORAGE_CONTROLLER", false), "run the storage controller as a managed subprocess (§11.1)")
	fs.StringVar(&sc.BinPath, "storage-controller-bin", env("AMP_STORAGE_CONTROLLER_BIN", "storage_controller"), "storage_controller binary: a bare name is resolved next to the controller binary (the bundle), then PATH; a path is used verbatim")
	fs.StringVar(&sc.ListenAddr, "storage-controller-listen", env("AMP_STORAGE_CONTROLLER_LISTEN", "127.0.0.1:50051"), "host:port the managed storage controller serves on")
	fs.StringVar(&sc.DatabaseURL, "storage-controller-database-url", env("AMP_STORAGE_CONTROLLER_DATABASE_URL", ""), "universe connection for the storage controller (defaults to --database-url)")
	fs.BoolVar(&sc.Dev, "storage-controller-dev", envBool("AMP_STORAGE_CONTROLLER_DEV", false), "run the storage controller in --dev mode (no auth; dev/local only)")
	fs.DurationVar(&sc.StartTimeout, "storage-controller-start-timeout", envDuration("AMP_STORAGE_CONTROLLER_START_TIMEOUT", 30*time.Second), "how long to wait for the storage controller to become ready")
	fs.DurationVar(&sc.StopTimeout, "storage-controller-stop-timeout", envDuration("AMP_STORAGE_CONTROLLER_STOP_TIMEOUT", 10*time.Second), "grace period after SIGTERM before the storage controller is killed")

	d := &c.Deploy
	fs.BoolVar(&d.Enabled, "deploy", envBool("AMP_DEPLOY", false), "enable the endpoint deployer (§11, #108)")
	fs.StringVar(&d.Driver, "deploy-driver", env("AMP_DEPLOY_DRIVER", DriverDocker), "deployer driver: docker (default) or kubernetes (Helm)")
	fs.StringVar(&d.Namespace, "deploy-namespace", env("AMP_DEPLOY_NAMESPACE", ""), "Kubernetes namespace endpoints are deployed into")
	fs.StringVar(&d.HelmBin, "helm-bin", env("AMP_HELM_BIN", "helm"), "helm binary: a bare name is resolved on PATH, a path is used verbatim")
	fs.StringVar(&d.ChartPath, "deploy-chart", env("AMP_DEPLOY_CHART", "deploy/helm/yb-light-endpoint"), "path to the yb-light-endpoint Helm chart")
	fs.StringVar(&d.KubeconfigPath, "kubeconfig", env("AMP_KUBECONFIG", ""), "KUBECONFIG for helm (empty = default discovery / in-cluster)")
	fs.StringVar(&d.EndpointImageRepo, "endpoint-image-repo", env("AMP_ENDPOINT_IMAGE_REPO", ""), "endpoint container image repository")
	fs.StringVar(&d.EndpointImageTag, "endpoint-image-tag", env("AMP_ENDPOINT_IMAGE_TAG", "latest"), "endpoint container image tag")
	fs.StringVar(&d.PullSecret, "deploy-pull-secret", env("AMP_DEPLOY_PULL_SECRET", ""), "imagePullSecrets name in the namespace (empty = none)")
	fs.IntVar(&d.ComputePort, "deploy-compute-port", envInt("AMP_DEPLOY_COMPUTE_PORT", 55433), "compute Postgres port the endpoint Service publishes")
	fs.StringVar(&d.ServiceType, "deploy-service-type", env("AMP_DEPLOY_SERVICE_TYPE", "ClusterIP"), "k8s: endpoint Service type — ClusterIP (default), NodePort, or LoadBalancer")
	var serviceAnnotationsRaw string
	fs.StringVar(&serviceAnnotationsRaw, "deploy-service-annotations", env("AMP_DEPLOY_SERVICE_ANNOTATIONS", ""), "k8s: comma-separated key=value annotations for the endpoint Service, e.g. internal-LB annotations (values cannot contain commas)")
	fs.StringVar(&d.StorageClass, "deploy-storage-class", env("AMP_DEPLOY_STORAGE_CLASS", ""), "k8s: storage class for the endpoint data volume — provisions a PersistentVolumeClaim (EBS/PD). Empty = ephemeral emptyDir (no persistent volume)")
	fs.StringVar(&d.DiskSize, "deploy-disk-size", env("AMP_DEPLOY_DISK_SIZE", "10Gi"), "k8s: requested size of the endpoint data volume when --deploy-storage-class is set")
	fs.StringVar(&d.BackingYBHostPorts, "deploy-yb-host-ports", env("AMP_DEPLOY_YB_HOST_PORTS", ""), "backing YugabyteDB host:port[,...] as the deployed pods reach it")
	fs.StringVar(&d.BackingYBUser, "deploy-yb-user", env("AMP_DEPLOY_YB_USER", "yugabyte"), "backing YugabyteDB user for deployed endpoints")
	fs.StringVar(&d.BackingYBDBName, "deploy-yb-dbname", env("AMP_DEPLOY_YB_DBNAME", "yb_storage"), "backing YugabyteDB database for deployed endpoints")
	fs.StringVar(&d.BackingYBExistingSecret, "deploy-yb-existing-secret", env("AMP_DEPLOY_YB_EXISTING_SECRET", ""), "name of a pre-existing Secret holding the backing YugabyteDB password (preferred over --deploy-yb-password)")
	fs.StringVar(&d.BackingYBExistingSecretKey, "deploy-yb-existing-secret-key", env("AMP_DEPLOY_YB_EXISTING_SECRET_KEY", "password"), "key within --deploy-yb-existing-secret holding the password")
	fs.StringVar(&d.BackingYBPassword, "deploy-yb-password", env("AMP_DEPLOY_YB_PASSWORD", ""), "backing YugabyteDB password, inline (dev/test; prefer --deploy-yb-existing-secret). Empty = passwordless/trust auth")
	fs.DurationVar(&d.Timeout, "deploy-timeout", envDuration("AMP_DEPLOY_TIMEOUT", 10*time.Minute), "how long to wait for a deploy/retire to converge")
	// Docker driver (--deploy-driver docker).
	fs.StringVar(&d.EndpointImage, "deploy-endpoint-image", env("AMP_DEPLOY_ENDPOINT_IMAGE", "yb-light/neon-endpoint:dev"), "docker: single-container endpoint image to run")
	fs.StringVar(&d.Network, "deploy-network", env("AMP_DEPLOY_NETWORK", ""), "docker: shared network for the controller + its endpoints (so they reach each other by name)")
	fs.StringVar(&d.ExternalHost, "deploy-external-host", env("AMP_DEPLOY_EXTERNAL_HOST", "127.0.0.1"), "docker: host a client uses to reach a published compute port")
	fs.StringVar(&d.SupervisorPath, "deploy-supervisor-path", env("AMP_DEPLOY_SUPERVISOR_PATH", ""), "docker: dev-only host path to supervisor.py to bind-mount into endpoints (empty = bundled)")
	fs.BoolVar(&d.Bootstrap, "deploy-bootstrap", envBool("AMP_DEPLOY_BOOTSTRAP", false), "dev/e2e — initdb the recorded tenant/timeline on endpoint deploy instead of attaching an existing one (the recipe model; no separate create-database flow yet); honored by both drivers")
	fs.StringVar(&d.StorageControllerURL, "deploy-storage-controller-url", env("AMP_DEPLOY_STORAGE_CONTROLLER_URL", ""), "URL deployed endpoints use to reach the managed storage controller; empty = endpoint runs storage-controller-free (Kubernetes: typically the controller's in-cluster Service URL, e.g. http://amp-controller.<ns>.svc.cluster.local:50051)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	d.ServiceAnnotations = parseKeyValueList(serviceAnnotationsRaw)

	// The universe connection is required configuration: the controller refuses
	// to start without it (amp-controller.md §9).
	if c.DB.URL == "" {
		return nil, fmt.Errorf("database URL is required (set --database-url or AMP_DATABASE_URL)")
	}
	if !schemaRe.MatchString(c.DB.Schema) {
		return nil, fmt.Errorf("invalid db schema %q: must be a plain SQL identifier", c.DB.Schema)
	}
	if c.Task.PollInterval <= 0 {
		return nil, fmt.Errorf("task poll interval must be positive")
	}
	if c.Task.LeaseTTL <= 0 {
		return nil, fmt.Errorf("task lease TTL must be positive")
	}
	if c.Task.MaxConcurrent < 1 {
		return nil, fmt.Errorf("task max concurrent must be at least 1")
	}
	if c.Task.ShutdownGrace <= 0 {
		return nil, fmt.Errorf("task shutdown grace must be positive")
	}
	if err := validateStorageController(sc, c.DB.URL); err != nil {
		return nil, err
	}
	if err := validateDeploy(d); err != nil {
		return nil, err
	}
	return c, nil
}

// parseKeyValueList parses "k1=v1,k2=v2" into a map (nil if empty). The first
// "=" splits each entry, so keys may contain "/" and "." (as Kubernetes
// annotation keys do); blank or keyless entries are skipped.
func parseKeyValueList(s string) map[string]string {
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		m[k] = strings.TrimSpace(v)
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// validateDeploy validates the endpoint deployer config for the selected
// driver. It is a no-op unless the deployer is enabled.
func validateDeploy(d *DeployConfig) error {
	if !d.Enabled {
		return nil
	}
	if d.Timeout <= 0 {
		return fmt.Errorf("deploy timeout must be positive")
	}
	// The backing YugabyteDB the endpoints connect to is required for both
	// drivers (it is what the launched compute attaches).
	if d.BackingYBHostPorts == "" {
		return fmt.Errorf("backing YugabyteDB host:port is required when --deploy is set")
	}
	switch d.Driver {
	case DriverKubernetes:
		return validateDeployKubernetes(d)
	case DriverDocker:
		if d.EndpointImage == "" {
			return fmt.Errorf("docker endpoint image is required when --deploy-driver=docker")
		}
		return nil
	default:
		return fmt.Errorf("invalid deploy driver %q (want %q or %q)", d.Driver, DriverDocker, DriverKubernetes)
	}
}

func validateDeployKubernetes(d *DeployConfig) error {
	if d.Namespace == "" {
		return fmt.Errorf("deploy namespace is required when --deploy is set")
	}
	if len(d.Namespace) > 63 || !dns1123Re.MatchString(d.Namespace) {
		return fmt.Errorf("invalid deploy namespace %q: must be a DNS-1123 label", d.Namespace)
	}
	if d.ChartPath == "" {
		return fmt.Errorf("deploy chart path is required when --deploy is set")
	}
	abs, err := filepath.Abs(d.ChartPath)
	if err != nil {
		return fmt.Errorf("resolve deploy chart path %q: %w", d.ChartPath, err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("deploy chart path %q is not a directory", d.ChartPath)
	}
	d.ChartPath = abs
	if d.EndpointImageRepo == "" {
		return fmt.Errorf("endpoint image repository is required when --deploy is set")
	}
	if d.EndpointImageTag == "" {
		return fmt.Errorf("endpoint image tag is required when --deploy is set")
	}
	if d.ComputePort < 1 || d.ComputePort > 65535 {
		return fmt.Errorf("deploy compute port %d out of range", d.ComputePort)
	}
	// Only the Service types the deployer resolves an address for are valid;
	// reject typos up front rather than failing late at `kubectl apply`.
	switch d.ServiceType {
	case "", "ClusterIP":
		d.ServiceType = "ClusterIP"
	case "NodePort", "LoadBalancer":
	default:
		return fmt.Errorf(`invalid deploy service type %q (want "ClusterIP", "NodePort", or "LoadBalancer")`, d.ServiceType)
	}
	// A storage class provisions a PVC, which needs a requested size; without a
	// class the data volume is an ephemeral emptyDir and the size is unused.
	if d.StorageClass != "" && d.DiskSize == "" {
		return fmt.Errorf("deploy disk size is required when --deploy-storage-class is set")
	}
	return nil
}

// validateStorageController fills defaults and validates the managed
// storage-controller config. It is a no-op unless the service is enabled.
func validateStorageController(sc *StorageControllerConfig, universeURL string) error {
	if !sc.Enabled {
		return nil
	}
	if sc.DatabaseURL == "" {
		// Both persist to the same universe over YSQL (§11); reuse the
		// controller's connection unless one is given explicitly.
		sc.DatabaseURL = universeURL
	}
	if sc.BinPath == "" {
		return fmt.Errorf("storage controller binary path is required when --storage-controller is set")
	}
	if _, _, err := net.SplitHostPort(sc.ListenAddr); err != nil {
		return fmt.Errorf("invalid storage controller listen address %q: %w", sc.ListenAddr, err)
	}
	if sc.StartTimeout <= 0 || sc.StopTimeout <= 0 {
		return fmt.Errorf("storage controller start/stop timeouts must be positive")
	}
	return nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
