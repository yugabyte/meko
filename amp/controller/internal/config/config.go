// Package config loads amp controller configuration from environment
// variables, with command-line flags taking precedence.
package config

import (
	"flag"
	"fmt"
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
	URL string // YSQL connection string (required)
	// Schema holds the controller's own tables — the write-ahead task log — and
	// the migrator's version table. The resource model (users, pools, clusters,
	// branches, endpoints) lives in a schema of its own, `amp_registry`,
	// hardcoded the way `amp_storage` is — see db.ResourceSchema.
	Schema string
}

// TaskConfig tunes the task framework worker.
type TaskConfig struct {
	PollInterval time.Duration
	LeaseTTL     time.Duration
	// RenewInterval is how often the worker extends the lease of a task it is
	// actively running, so a step that runs longer than LeaseTTL (e.g. a helm
	// deploy that waits minutes for a pod) is not reclaimed and run concurrently
	// by a second goroutine. It must be shorter than LeaseTTL so the lease is
	// refreshed before it lapses.
	RenewInterval time.Duration
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

// BranchGCConfig tunes the branch-drop reaper (internal/branchgc): the loop that
// reclaims a dropped branch's rows from `amp_storage` once its retention window
// has passed. Every knob here bounds the reaper's blast radius rather than its
// throughput — the rows it deletes are garbage with no deadline, and the only
// cost of going slowly is disk.
// Each field's meaning is the flag's help text in Load; what is worth saying here
// is what the help text cannot.
//
// Distinct from GCConfig, which is the failed-create orphan sweeper's
// (internal/gc) — two unrelated loops that both happen to be called GC.
type BranchGCConfig struct {
	Enabled  bool
	Interval time.Duration
	// Retention is snapshotted into `amp_registry.branches.reap_after` the first
	// tick that sees a tombstone, measured from its `dropped_at`. Raising or
	// lowering it therefore moves only deadlines not yet stamped.
	Retention time.Duration
	// Both counts below are CEILINGS, never targets: a DELETE names at most
	// RowsPerDelete rows, and a tick deletes at most RowsPerTick across every
	// branch and table it touches. Each is routinely under-run — a delete is
	// clamped by whatever is left of the tick's budget, and a tick stops the
	// moment that budget is spent.
	//
	// RowsPerDelete keeps a reap out of one large distributed transaction;
	// RowsPerTick is what spreads a big branch over many ticks instead of flooding
	// the tablets live branches are ingesting into. Together they are the whole of
	// the pacing: rows, not branches, are what the cluster feels.
	RowsPerDelete int
	RowsPerTick   int
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
	ChartPath      string // path to the yb-amp-endpoint Helm chart
	KubeconfigPath string // KUBECONFIG for helm; empty = default discovery (in-cluster or ~/.kube)

	EndpointImageRepo  string            // endpoint container image repository
	EndpointImageTag   string            // endpoint container image tag
	ExtensionRegistry  string            // registry the externalized-extension images (amp-ext-*) live under; empty = bare refs
	PullSecret         string            // imagePullSecrets name in the namespace (empty = none)
	ComputePort        int               // compute Postgres port the endpoint Service publishes
	ServiceType        string            // endpoint Service type: "ClusterIP" (default), "NodePort", or "LoadBalancer"
	ServiceAnnotations map[string]string // annotations on the endpoint Service (e.g. GKE internal-LB)
	NodeSelector       map[string]string // node labels endpoint pods must match (e.g. a local-NVMe-SSD pool)
	TailnetSuffix      string            // Tailscale MagicDNS suffix (e.g. "tail5955.ts.net") for deriving endpoint tailnet addresses; empty = disabled

	// StorageClass provisions the endpoint's data volume (/data: pgdata, LFC,
	// logs) as a PersistentVolumeClaim of that class — a network block volume
	// (EBS on AWS, Persistent Disk on GCP). Empty falls back to an ephemeral
	// emptyDir (no persistent volume). DiskSize is that PVC's requested size.
	StorageClass string
	DiskSize     string
	// DiskGiBPerVCPU sizes the endpoint data volume per vCPU. 0 = the built-in
	// 20 GiB/vCPU policy every pool is expected to be provisioned to honour;
	// set it only for a pool built to a deliberately different disk-per-core
	// shape. The controller cannot inspect nodes without cluster-scoped RBAC
	// (#584), so a ratio the pool cannot back shows up as an endpoint that will
	// not schedule.
	DiskGiBPerVCPU int

	// --- Docker runtime ---
	EndpointImage string // single-container endpoint image to run
	Network       string // shared Docker network for the controller + its endpoints
	ExternalHost  string // host a client uses to reach a published compute port

	// ControllerURL is the URL deployed endpoints use to reach the
	// controller-managed controller (e.g. http://host.docker.internal:50051).
	// When set, the endpoint runs the controller-coordinated path
	// (controller-cluster-create.md). Empty = the endpoint runs
	// controller-free. Either way the pageserver claims its own generation.
	ControllerURL string

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
	// DriverLocal keeps endpoint records without launching anything: the compute
	// runs locally, started by whoever asked for it (yb_amp_local spawns its
	// own). See deploy.LocalDeployer.
	DriverLocal = "local"
)

// YCPConfig configures the controller's registration with, and reporting to,
// the YCP fleet control plane (meko api_server --control-plane-mode). It is a
// no-op unless Enabled.
//
// The controller registers itself as an "amp" environment: it exchanges a
// one-time enrollment token for a long-lived, environment-scoped status token
// (persisted so a restart resumes without re-enrolling), then reports the
// storage pools / databases / branches / compute endpoints it owns on a periodic
// heartbeat. It does NOT yet reconcile YCP-declared desired state back into the
// cluster — that is a later phase (ac-ycp-reconcile.md).
type YCPConfig struct {
	Enabled bool // register with and report to YCP
	// URL is the control-plane base URL. It may carry a subpath when the control
	// plane is mounted under one (e.g. https://devcloud.yugabyte.com/ycp); every
	// request appends "/api/v1/…", so a trailing slash is trimmed.
	URL string
	// EnvName is the environment name to enroll as. It must match the name the
	// environment was registered under in YCP (the id it maps to is "amp-"+slug).
	EnvName string
	// EnrollmentToken is the one-time bootstrap token minted at registration.
	// Delivered via a mounted Secret in production. It may be empty on a restart
	// once the status token has been persisted — the controller resumes on that.
	EnrollmentToken string
	Placement       string        // cloud | onprem
	ReportInterval  time.Duration // status heartbeat cadence
}

// Config is the fully-resolved controller configuration.
type Config struct {
	ListenAddr     string
	SigningKeyPath string // optional; controller generates an ephemeral key if unset
	Metrics        bool   // expose Prometheus /metrics and record instrumentation
	Log            LogConfig
	DB             DBConfig
	Task           TaskConfig
	GC             GCConfig
	BranchGC       BranchGCConfig
	Deploy         DeployConfig
	YCP            YCPConfig
}

// GCConfig tunes the failed-create orphan sweeper (internal/gc). Mirrors gc.Config;
// kept separate so the config package does not depend on it.
type GCConfig struct {
	Enabled    bool
	Interval   time.Duration
	Grace      time.Duration
	BatchLimit int
}

// defaultGCRetention is how long a dropped branch's data is kept before the reaper
// reclaims it. It matches the pageserver's default PITR window
// (`DEFAULT_PITR_INTERVAL`, compute/libs/pageserver_api/src/config.rs): that
// window is how far back a LIVE branch's history stays readable, so keeping a
// dropped branch's for the same span makes the drop no more destructive than the
// retention the branch was already running with.
//
// It is a flag rather than a per-branch lookup, because `pitr_interval` is
// pageserver config and the control plane does not record it per branch — so this
// mirrors its default, and a deployment that raises one should raise both.
const defaultGCRetention = 24 * time.Hour

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
	fs.StringVar(&c.DB.Schema, "db-schema", env("AMP_DB_SCHEMA", "controller"), "schema that holds the controller's own tables (the resource model is always in `amp_registry`)")
	fs.StringVar(&c.Log.Level, "log-level", env("AMP_LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	fs.StringVar(&c.Log.Format, "log-format", env("AMP_LOG_FORMAT", "json"), "log format: json|text")
	fs.StringVar(&c.SigningKeyPath, "signing-key", env("AMP_SIGNING_KEY_PATH", ""), "path to the control-plane signing key (optional)")
	fs.BoolVar(&c.Metrics, "metrics", envBool("AMP_METRICS", true), "expose the Prometheus /metrics scrape endpoint and record instrumentation")
	fs.DurationVar(&c.Task.PollInterval, "task-poll-interval", envDuration("AMP_TASK_POLL_INTERVAL", 2*time.Second), "task worker poll interval")
	fs.DurationVar(&c.Task.LeaseTTL, "task-lease-ttl", envDuration("AMP_TASK_LEASE_TTL", 30*time.Second), "task ownership lease TTL")
	fs.DurationVar(&c.Task.RenewInterval, "task-lease-renew-interval", envDuration("AMP_TASK_LEASE_RENEW_INTERVAL", 10*time.Second), "how often the worker renews the lease of a running task (must be < task-lease-ttl)")
	fs.IntVar(&c.Task.MaxConcurrent, "task-max-concurrent", envInt("AMP_TASK_MAX_CONCURRENT", 50), "max tasks the worker executes concurrently")
	fs.BoolVar(&c.GC.Enabled, "gc-enabled", envBool("AMP_GC_ENABLED", true), "sweep failed-create orphan clusters and enqueue their teardown")
	fs.DurationVar(&c.GC.Interval, "gc-interval", envDuration("AMP_GC_INTERVAL", 5*time.Minute), "how often the orphan sweeper runs")
	fs.DurationVar(&c.GC.Grace, "gc-grace", envDuration("AMP_GC_GRACE", 30*time.Minute), "how long a failed-create orphan is left alone before teardown, so a fresh failure can be inspected and a manual retry is not raced")
	fs.IntVar(&c.GC.BatchLimit, "gc-batch-limit", envInt("AMP_GC_BATCH_LIMIT", 20), "max orphans one sweep enqueues")
	fs.DurationVar(&c.Task.ShutdownGrace, "task-shutdown-grace", envDuration("AMP_TASK_SHUTDOWN_GRACE", 10*time.Second), "max time to wait for in-flight tasks to finish on shutdown")

	fs.BoolVar(&c.BranchGC.Enabled, "branch-gc", envBool("AMP_BRANCH_GC", true), "run the branch-drop reaper: reclaim a dropped branch's amp_storage rows once its retention window has passed")
	fs.DurationVar(&c.BranchGC.Interval, "branch-gc-interval", envDuration("AMP_BRANCH_GC_INTERVAL", 30*time.Second), "branch-drop reaper tick cadence; an idle tick is one seek on branches_gc_due, so this is also what sets the delete rate (rows-per-tick / interval)")
	fs.DurationVar(&c.BranchGC.Retention, "branch-gc-retention", envDuration("AMP_BRANCH_GC_RETENTION", defaultGCRetention), "how long a dropped branch's data is kept before it is reclaimed; measured from the drop and stamped into reap_after by the reaper, so a change moves only deadlines not yet stamped")
	fs.IntVar(&c.BranchGC.RowsPerDelete, "branch-gc-rows-per-delete", envInt("AMP_BRANCH_GC_ROWS_PER_DELETE", 5000), "at most this many rows are named by one DELETE, so no reap is one large distributed transaction")
	fs.IntVar(&c.BranchGC.RowsPerTick, "branch-gc-rows-per-tick", envInt("AMP_BRANCH_GC_ROWS_PER_TICK", 20000), "pacing budget: at most this many rows are deleted by one reaper tick, across every branch and table it touches")

	d := &c.Deploy
	fs.BoolVar(&d.Enabled, "deploy", envBool("AMP_DEPLOY", false), "enable the endpoint deployer (§11, #108)")
	fs.StringVar(&d.Driver, "deploy-driver", env("AMP_DEPLOY_DRIVER", DriverDocker), "deployer driver: docker (default), kubernetes (Helm), or local (records only; the compute is started locally by its owner)")
	fs.StringVar(&d.Namespace, "deploy-namespace", env("AMP_DEPLOY_NAMESPACE", ""), "Kubernetes namespace endpoints are deployed into")
	fs.StringVar(&d.HelmBin, "helm-bin", env("AMP_HELM_BIN", "helm"), "helm binary: a bare name is resolved on PATH, a path is used verbatim")
	fs.StringVar(&d.ChartPath, "deploy-chart", env("AMP_DEPLOY_CHART", "deploy/helm/yb-amp-endpoint"), "path to the yb-amp-endpoint Helm chart")
	fs.StringVar(&d.KubeconfigPath, "kubeconfig", env("AMP_KUBECONFIG", ""), "KUBECONFIG for helm (empty = default discovery / in-cluster)")
	fs.StringVar(&d.EndpointImageRepo, "endpoint-image-repo", env("AMP_ENDPOINT_IMAGE_REPO", ""), "endpoint container image repository")
	fs.StringVar(&d.EndpointImageTag, "endpoint-image-tag", env("AMP_ENDPOINT_IMAGE_TAG", "latest"), "endpoint container image tag")
	fs.StringVar(&d.ExtensionRegistry, "extension-registry", env("AMP_EXTENSION_REGISTRY", "quay.io/yugabyte"), "k8s: registry the externalized-extension images (amp-ext-<name>:<version>) are pulled from")
	fs.StringVar(&d.PullSecret, "deploy-pull-secret", env("AMP_DEPLOY_PULL_SECRET", ""), "imagePullSecrets name in the namespace (empty = none)")
	fs.IntVar(&d.ComputePort, "deploy-compute-port", envInt("AMP_DEPLOY_COMPUTE_PORT", 55433), "compute Postgres port the endpoint Service publishes")
	fs.StringVar(&d.ServiceType, "deploy-service-type", env("AMP_DEPLOY_SERVICE_TYPE", "ClusterIP"), "k8s: endpoint Service type — ClusterIP (default), NodePort, or LoadBalancer")
	var serviceAnnotationsRaw string
	fs.StringVar(&serviceAnnotationsRaw, "deploy-service-annotations", env("AMP_DEPLOY_SERVICE_ANNOTATIONS", ""), "k8s: comma-separated key=value annotations for the endpoint Service, e.g. internal-LB annotations (values cannot contain commas)")
	var nodeSelectorRaw string
	fs.StringVar(&nodeSelectorRaw, "deploy-node-selector", env("AMP_DEPLOY_NODE_SELECTOR", ""), "k8s: comma-separated key=value node labels endpoint pods must schedule onto, e.g. a local-NVMe-SSD ephemeral pool (values cannot contain commas)")
	fs.StringVar(&d.TailnetSuffix, "deploy-tailnet-suffix", env("AMP_DEPLOY_TAILNET_SUFFIX", ""), "k8s: Tailscale MagicDNS suffix (e.g. tail5955.ts.net) for deriving each endpoint's tailnet address when its Service is tailnet-exposed (tailscale.com/expose=true); empty = disabled")
	fs.StringVar(&d.StorageClass, "deploy-storage-class", env("AMP_DEPLOY_STORAGE_CLASS", ""), "k8s: storage class for the endpoint data volume — provisions a PersistentVolumeClaim (EBS/PD). Empty = ephemeral emptyDir (no persistent volume)")
	fs.StringVar(&d.DiskSize, "deploy-disk-size", env("AMP_DEPLOY_DISK_SIZE", "10Gi"), "k8s: requested size of the endpoint data volume when --deploy-storage-class is set")
	fs.IntVar(&d.DiskGiBPerVCPU, "deploy-disk-gib-per-vcpu", envInt("AMP_DEPLOY_DISK_GIB_PER_VCPU", 0), "k8s: GiB of endpoint data volume per vCPU (0 = the built-in 20 GiB/vCPU policy; override only for a pool built to a different disk-per-core shape)")
	fs.StringVar(&d.BackingYBHostPorts, "deploy-yb-host-ports", env("AMP_DEPLOY_YB_HOST_PORTS", ""), "backing YugabyteDB host:port[,...] as the deployed pods reach it")
	fs.StringVar(&d.BackingYBUser, "deploy-yb-user", env("AMP_DEPLOY_YB_USER", "yugabyte"), "backing YugabyteDB user for deployed endpoints")
	fs.StringVar(&d.BackingYBDBName, "deploy-yb-dbname", env("AMP_DEPLOY_YB_DBNAME", "yb_storage"), "backing YugabyteDB database for deployed endpoints")
	fs.StringVar(&d.BackingYBExistingSecret, "deploy-yb-existing-secret", env("AMP_DEPLOY_YB_EXISTING_SECRET", ""), "name of a pre-existing Secret holding the backing YugabyteDB password (preferred over --deploy-yb-password)")
	fs.StringVar(&d.BackingYBExistingSecretKey, "deploy-yb-existing-secret-key", env("AMP_DEPLOY_YB_EXISTING_SECRET_KEY", "password"), "key within --deploy-yb-existing-secret holding the password")
	fs.StringVar(&d.BackingYBPassword, "deploy-yb-password", env("AMP_DEPLOY_YB_PASSWORD", ""), "backing YugabyteDB password, inline (dev/test; prefer --deploy-yb-existing-secret). Empty = passwordless/trust auth")
	fs.DurationVar(&d.Timeout, "deploy-timeout", envDuration("AMP_DEPLOY_TIMEOUT", 10*time.Minute), "how long to wait for a deploy/retire to converge")
	// Docker driver (--deploy-driver docker).
	fs.StringVar(&d.EndpointImage, "deploy-endpoint-image", env("AMP_DEPLOY_ENDPOINT_IMAGE", "yb-amp/neon-endpoint:dev"), "docker: single-container endpoint image to run")
	fs.StringVar(&d.Network, "deploy-network", env("AMP_DEPLOY_NETWORK", ""), "docker: shared network for the controller + its endpoints (so they reach each other by name)")
	fs.StringVar(&d.ExternalHost, "deploy-external-host", env("AMP_DEPLOY_EXTERNAL_HOST", "127.0.0.1"), "docker: host a client uses to reach a published compute port")
	fs.StringVar(&d.ControllerURL, "deploy-controller-url", env("AMP_DEPLOY_CONTROLLER_URL", ""), "URL deployed endpoints use to reach the controller; empty = endpoint runs controller-free (Kubernetes: typically the controller's in-cluster Service URL, e.g. http://amp-controller.<ns>.svc.cluster.local:8080)")

	y := &c.YCP
	fs.BoolVar(&y.Enabled, "ycp", envBool("AMP_YCP_ENABLED", false), "register with and report the resource inventory to the YCP fleet control plane")
	fs.StringVar(&y.URL, "ycp-url", env("AMP_YCP_URL", ""), "YCP control-plane base URL (may include a subpath, e.g. https://host/ycp)")
	fs.StringVar(&y.EnvName, "ycp-env-name", env("AMP_YCP_ENV_NAME", ""), "environment name to enroll as (must match the environment registered in YCP)")
	fs.StringVar(&y.EnrollmentToken, "ycp-enrollment-token", env("AMP_YCP_ENROLLMENT_TOKEN", ""), "one-time YCP enrollment token (delivered via a mounted Secret; may be empty once the status token is persisted)")
	fs.StringVar(&y.Placement, "ycp-placement", env("AMP_YCP_PLACEMENT", "cloud"), "environment placement reported to YCP: cloud | onprem")
	fs.DurationVar(&y.ReportInterval, "ycp-report-interval", envDuration("AMP_YCP_REPORT_INTERVAL", 30*time.Second), "how often to report the resource inventory to YCP")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	d.ServiceAnnotations = parseKeyValueList(serviceAnnotationsRaw)
	d.NodeSelector = parseKeyValueList(nodeSelectorRaw)
	y.URL = strings.TrimRight(y.URL, "/")

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
	if c.Task.RenewInterval <= 0 {
		return nil, fmt.Errorf("task lease renew interval must be positive")
	}
	if c.Task.RenewInterval >= c.Task.LeaseTTL {
		return nil, fmt.Errorf("task lease renew interval (%s) must be shorter than the lease TTL (%s)", c.Task.RenewInterval, c.Task.LeaseTTL)
	}
	if c.Task.MaxConcurrent < 1 {
		return nil, fmt.Errorf("task max concurrent must be at least 1")
	}
	if c.Task.ShutdownGrace <= 0 {
		return nil, fmt.Errorf("task shutdown grace must be positive")
	}
	if err := validateBranchGC(&c.BranchGC); err != nil {
		return nil, err
	}
	if err := validateDeploy(d); err != nil {
		return nil, err
	}
	if err := validateYCP(y); err != nil {
		return nil, err
	}
	return c, nil
}

// validateBranchGC validates the branch-drop reaper's knobs, all of which only
// matter while it runs — the reaper is the sole consumer of every one, retention
// included, since it stamps the deadlines itself.
//
// Retention is deliberately allowed to be zero (reap on the next tick); negative
// is not, since it would make every drop instantly due including one whose
// deadline an operator meant to extend.
func validateBranchGC(g *BranchGCConfig) error {
	if !g.Enabled {
		return nil
	}
	if g.Retention < 0 {
		return fmt.Errorf("branch gc retention must not be negative")
	}
	if g.Interval <= 0 {
		return fmt.Errorf("branch gc interval must be positive")
	}
	if g.RowsPerDelete < 1 {
		return fmt.Errorf("branch gc rows per delete must be at least 1")
	}
	if g.RowsPerTick < 1 {
		return fmt.Errorf("branch gc rows per tick must be at least 1")
	}
	return nil
}

// validateYCP validates the YCP registration/reporting config. It is a no-op
// unless enabled. The enrollment token is deliberately NOT required: on a
// restart the controller resumes from the persisted status token, and requiring
// the (short-lived) enrollment token would break that path.
func validateYCP(y *YCPConfig) error {
	if !y.Enabled {
		return nil
	}
	if y.URL == "" {
		return fmt.Errorf("YCP url is required when --ycp is set (set --ycp-url or AMP_YCP_URL)")
	}
	if y.EnvName == "" {
		return fmt.Errorf("YCP env name is required when --ycp is set (set --ycp-env-name or AMP_YCP_ENV_NAME)")
	}
	if y.ReportInterval <= 0 {
		return fmt.Errorf("YCP report interval must be positive")
	}
	return nil
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
	// The local driver launches nothing, so none of what a launch needs — a
	// timeout to converge in, a backing YugabyteDB to attach, an image — applies.
	if d.Driver == DriverLocal {
		return nil
	}
	if d.Timeout <= 0 {
		return fmt.Errorf("deploy timeout must be positive")
	}
	// The backing YugabyteDB the endpoints connect to is required by both
	// launching drivers (it is what the launched compute attaches).
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
		return fmt.Errorf("invalid deploy driver %q (want %q, %q or %q)",
			d.Driver, DriverDocker, DriverKubernetes, DriverLocal)
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
