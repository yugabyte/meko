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

	"github.com/yugabyte/amp/controller/pkg/pglease"
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
	// RenewInterval is how often the task's lease is renewed, so a step that runs
	// longer than LeaseTTL (e.g. a helm deploy that waits minutes for a pod) is not
	// reclaimed and run concurrently. The renewal is pglease's, not a heartbeat of
	// the worker's, so this reaches pglease.Config.RenewEvery — which requires
	// RenewInterval < LeaseTTL minus pglease's safety margin, a stricter bound than
	// "shorter than the TTL". Validate enforces the stricter one so a rejected pair
	// fails at startup here rather than inside NewPgLocks.
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
	// LockWait bounds how long a step blocks on a resource lock another task
	// holds. Past it the task fails cleanly rather than hanging, so a lock nothing
	// ever releases costs one task instead of every task queued behind it.
	LockWait time.Duration
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
	// the tablets live branches are ingesting into. Together with the tick cadence
	// they are the whole of the pacing: rows, not branches, are what the cluster
	// feels. The cadence is adaptive (see branchgc.nextInterval), so the sustained
	// ceiling is RowsPerTick per Interval/6, not per Interval.
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

	// MemoryGiBPerVCPU is the memory-to-CPU shape an endpoint must be asked for,
	// in GiB of RAM per vCPU. It does two jobs: it fills in the memory for a
	// request that names only CPU, and it is the floor a request naming both must
	// meet — less memory per core is rejected, not quietly accepted.
	//
	// A floor rather than a fixed shape: more memory per core is a legitimate ask
	// (a workload whose working set does not fit), while less is the shape that
	// produces an endpoint which starts and then cannot hold its own cache.
	// 0 = the built-in 8 GiB/vCPU. Set it for a pool built to a deliberately
	// different memory-per-core shape.
	MemoryGiBPerVCPU int
	// DiskSizeOverride is a flat data-volume size applied to every endpoint
	// whatever its CPU, bypassing both the per-vCPU ratio and the DiskSize
	// floor. Empty (the default) keeps the CPU-proportional policy. This is for
	// test clusters, where nodes are too small to back the ratio and the disk is
	// scratch nothing will fill (#602) — not for prod.
	DiskSizeOverride string

	// --- Docker runtime ---
	EndpointImage string // single-container endpoint image to run
	Network       string // shared Docker network for the controller + its endpoints
	ExternalHost  string // host a client uses to reach a published compute port

	// ControllerURL is the URL deployed endpoints use to reach the
	// controller-managed controller (e.g. http://host.docker.internal:50051).
	// When set, the endpoint runs the controller-coordinated path
	// (controller-cluster-create.md). Empty = the endpoint runs
	// controller-free. Either way the pageserver reads its own branch row.
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

// VoyagerConfig configures the optional Voyager database-migration feature
// (yb-amp/migration). It is entirely disabled unless DatabaseURL is set: an
// empty DatabaseURL keeps the controller's behavior identical to before the
// feature existed (the migration APIs answer 501). When set, the controller
// connects to the central Voyager control database, applies its schema, and
// launches offline-migration jobs through the configured deploy driver.
type VoyagerConfig struct {
	DatabaseURL  string // controller's connection to the central Voyager control database (enables the migration APIs); launched Voyager jobs reach the same database at this URL
	Image        string // Voyager job container image (required when enabled)
	Namespace    string // k8s: namespace Voyager Jobs run in; defaults to the deploy namespace
	PVCSize      string // k8s: size of each Voyager job's work PersistentVolumeClaim
	StorageClass string // k8s: storage class for the Voyager job PVC; defaults to the deploy storage class
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
	Voyager        VoyagerConfig
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

// Exported because branchgc's plan test must EXPLAIN the batch size production
// uses: below ~27 ids a Tid Scan wins on cost alone and the test proves nothing (#970).
const DefaultBranchGCRowsPerDelete = 5000

var schemaRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// dns1123Re matches a Kubernetes DNS-1123 label (lowercase alphanumeric and
// '-', starting and ending alphanumeric); length is checked separately.
var dns1123Re = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// byteQuantityRe matches a Kubernetes storage quantity ("10Gi", "512Mi", "2G",
// "1073741824") — the subset deploy.ParseByteQuantity accepts. A typo here would
// otherwise be discovered as a mis-sized volume at endpoint-create time.
var byteQuantityRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|k|K|M|G|T|P)?$`)

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
	fs.DurationVar(&c.Task.RenewInterval, "task-lease-renew-interval", envDuration("AMP_TASK_LEASE_RENEW_INTERVAL", 10*time.Second), "how often a running task's lease is renewed (must be < task-lease-ttl minus a 2s safety margin)")
	fs.IntVar(&c.Task.MaxConcurrent, "task-max-concurrent", envInt("AMP_TASK_MAX_CONCURRENT", 50), "max tasks the worker executes concurrently")
	fs.BoolVar(&c.GC.Enabled, "gc-enabled", envBool("AMP_GC_ENABLED", true), "sweep failed-create orphan clusters and endpoints and enqueue their teardown")
	fs.DurationVar(&c.GC.Interval, "gc-interval", envDuration("AMP_GC_INTERVAL", 5*time.Minute), "how often the orphan sweeper runs")
	fs.DurationVar(&c.GC.Grace, "gc-grace", envDuration("AMP_GC_GRACE", 30*time.Minute), "how long a failed-create orphan is left alone before teardown, so a fresh failure can be inspected and a manual retry is not raced")
	fs.IntVar(&c.GC.BatchLimit, "gc-batch-limit", envInt("AMP_GC_BATCH_LIMIT", 20), "max orphans one sweep enqueues per resource kind")
	fs.DurationVar(&c.Task.ShutdownGrace, "task-shutdown-grace", envDuration("AMP_TASK_SHUTDOWN_GRACE", 10*time.Second), "max time to wait for in-flight tasks to finish on shutdown")
	fs.DurationVar(&c.Task.LockWait, "task-lock-wait", envDuration("AMP_TASK_LOCK_WAIT", 10*time.Minute), "max time a task waits for a resource lock (e.g. an endpoint) held by another task before failing")

	fs.BoolVar(&c.BranchGC.Enabled, "branch-gc", envBool("AMP_BRANCH_GC", true), "run the branch-drop reaper: reclaim a dropped branch's amp_storage rows once its retention window has passed")
	fs.DurationVar(&c.BranchGC.Interval, "branch-gc-interval", envDuration("AMP_BRANCH_GC_INTERVAL", 30*time.Second), "branch-drop reaper base tick cadence; the loop adapts around it — interval/6 while a reap is capped by its row budget, doubling to 8x while nothing is due (see internal/branchgc)")
	fs.DurationVar(&c.BranchGC.Retention, "branch-gc-retention", envDuration("AMP_BRANCH_GC_RETENTION", defaultGCRetention), "how long a dropped branch's data is kept before it is reclaimed; measured from the drop and stamped into reap_after by the reaper, so a change moves only deadlines not yet stamped")
	fs.IntVar(&c.BranchGC.RowsPerDelete, "branch-gc-rows-per-delete", envInt("AMP_BRANCH_GC_ROWS_PER_DELETE", DefaultBranchGCRowsPerDelete), "at most this many rows are named by one DELETE, so no reap is one large distributed transaction")
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
	fs.IntVar(&d.MemoryGiBPerVCPU, "deploy-memory-gib-per-vcpu", envInt("AMP_DEPLOY_MEMORY_GIB_PER_VCPU", 0), "GiB of endpoint memory per vCPU: fills in the memory when a request names only CPU, and is the floor every request must meet (0 = the built-in 8 GiB/vCPU)")
	fs.StringVar(&d.DiskSizeOverride, "deploy-disk-size-override", env("AMP_DEPLOY_DISK_SIZE_OVERRIDE", ""), "k8s: flat endpoint data volume size (e.g. 10Gi) for every endpoint regardless of CPU, bypassing the per-vCPU ratio and --deploy-disk-size floor; empty = CPU-proportional sizing. For test clusters whose nodes cannot back the ratio")
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

	// Voyager database-migration feature (yb-amp/migration). Disabled unless
	// --voyager-database-url is set.
	v := &c.Voyager
	fs.StringVar(&v.DatabaseURL, "voyager-database-url", env("AMP_VOYAGER_DATABASE_URL", ""), "connection string to the central Voyager control database; empty = the Voyager migration APIs are disabled (501)")
	fs.StringVar(&v.Image, "voyager-image", env("AMP_VOYAGER_IMAGE", ""), "Voyager job container image (required when the Voyager feature is enabled)")
	fs.StringVar(&v.Namespace, "voyager-namespace", env("AMP_VOYAGER_NAMESPACE", ""), "k8s: namespace Voyager migration Jobs run in; empty = the deploy namespace")
	fs.StringVar(&v.PVCSize, "voyager-pvc-size", env("AMP_VOYAGER_PVC_SIZE", "20Gi"), "k8s: size of each Voyager job's work PersistentVolumeClaim")
	fs.StringVar(&v.StorageClass, "voyager-storage-class", env("AMP_VOYAGER_STORAGE_CLASS", ""), "k8s: storage class for the Voyager job PVC; empty = the deploy storage class")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	d.ServiceAnnotations = parseKeyValueList(serviceAnnotationsRaw)
	d.NodeSelector = parseKeyValueList(nodeSelectorRaw)

	// The Voyager Kubernetes placement inherits the deploy defaults when not
	// overridden, so a single-database deployment need only set
	// --voyager-database-url and --voyager-image.
	if v.Namespace == "" {
		v.Namespace = d.Namespace
	}
	if v.StorageClass == "" {
		v.StorageClass = d.StorageClass
	}

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
	// The whole pglease range, not just positivity: a TTL it rejects would otherwise
	// pass here and abort startup inside NewPgLocks, with an error naming a package
	// the operator never configured.
	if c.Task.LeaseTTL < pglease.MinTTL || c.Task.LeaseTTL > pglease.MaxTTL {
		return nil, fmt.Errorf("task lease TTL (%s) must be in [%s, %s]",
			c.Task.LeaseTTL, pglease.MinTTL, pglease.MaxTTL)
	}
	if c.Task.RenewInterval <= 0 {
		return nil, fmt.Errorf("task lease renew interval must be positive")
	}
	// pglease.Config.Validate wants RenewEvery < TTL - SafetyMargin, so checking
	// only against the TTL would accept a pair that then aborts startup inside
	// NewPgLocks with an error about a package the operator did not configure.
	if margin := c.Task.LeaseTTL - pglease.DefaultSafetyMargin; c.Task.RenewInterval >= margin {
		return nil, fmt.Errorf(
			"task lease renew interval (%s) must be shorter than the lease TTL minus the %s safety margin (%s)",
			c.Task.RenewInterval, pglease.DefaultSafetyMargin, margin)
	}
	if c.Task.LockWait <= 0 {
		// Zero would reach PgLocks.AcquireResource as an already-expired context, so
		// every task with a locking step would fail its acquire immediately and be
		// durably failed, with nothing at startup to point at the cause.
		return nil, fmt.Errorf("task lock wait must be positive")
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
	if err := validateVoyager(v, d.Driver); err != nil {
		return nil, err
	}
	if err := validateDeploy(d); err != nil {
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
	// The flat override sizes every endpoint's volume, so an unparseable value
	// has to fail startup rather than silently fall back to the ratio.
	if d.DiskSizeOverride != "" && !byteQuantityRe.MatchString(d.DiskSizeOverride) {
		return fmt.Errorf("invalid deploy disk size override %q (want a k8s quantity, e.g. 10Gi)", d.DiskSizeOverride)
	}
	return nil
}

// validateVoyager validates the optional Voyager migration configuration for the
// selected deploy driver. An empty DatabaseURL means the feature is disabled, so
// the function is a no-op and the controller's default behavior is unchanged.
func validateVoyager(v *VoyagerConfig, deployDriver string) error {
	if v.DatabaseURL == "" {
		return nil
	}
	if v.Image == "" {
		return fmt.Errorf("Voyager image is required when --voyager-database-url is set (set --voyager-image or AMP_VOYAGER_IMAGE)")
	}
	// Voyager jobs are launched through the endpoint deploy driver, so the only
	// runtimes that can run them are the two launching drivers.
	switch deployDriver {
	case DriverKubernetes:
		if len(v.Namespace) > 63 || !dns1123Re.MatchString(v.Namespace) {
			return fmt.Errorf("invalid Voyager namespace %q: must be a DNS-1123 label (set --voyager-namespace or --deploy-namespace)", v.Namespace)
		}
		if v.PVCSize == "" {
			return fmt.Errorf("Voyager PVC size is required for the kubernetes driver")
		}
	case DriverDocker:
		// The Docker runner uses named volumes on the shared network; no
		// namespace or PVC applies.
	default:
		return fmt.Errorf("invalid deploy driver %q for Voyager migrations (want %q or %q)", deployDriver, DriverKubernetes, DriverDocker)
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
