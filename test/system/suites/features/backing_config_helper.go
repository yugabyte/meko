// This file carries the amp-on-backing tests of the functional suite: heavy,
// exclusive, end-to-end tests that, for one YugabyteDB backing store, provision
// the cluster, run a meaningful workload directly on it (layer 1, YB-native),
// deploy amp on that store (ampstack external mode), run a meaningful workload
// through amp's compute (layer 2), assert cross-layer sanity, and tear
// everything down as one unit — nothing left running.
//
// The tests differ only in how the backing cluster is produced (a yugabyted
// cluster on AWS instances, a raw master+tserver cluster on AWS instances, a
// YBA universe, an owned provisioned YBA, or a co-located YB-in-k8s cluster).
// Everything else — the guaranteed-cleanup lifecycle, both workload layers, the
// cross-layer checks — is shared in backing.go / workload.go.
//
// Gating: these tests self-provision real infrastructure, so they SKIP cleanly
// unless functional.enabled is set AND the config each test needs is present.
// A plain `ampqa run --suite functional` (no config) selects the "none"
// provider and every amp-on-backing test skips before its body; a `--dry-run`
// reaches the body and skips on the config gate. Only an explicit
// functional.enabled=true with a real config (the wave-D live run) provisions
// anything.
package features

import (
	"fmt"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/ampstack"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/yba"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/ybastack"
	"github.com/yugabyte/yb-amp/test/system/framework/core"
	"github.com/yugabyte/yb-amp/test/system/framework/env/vm"
)

// Cheap defaults: a small run is inexpensive. Overridable under functional.*.
const (
	DefaultNodes        = 3
	DefaultAdvertise    = "private" // peers/YSQL advertise on the private IP (in-VPC coordinator)
	DefaultNumDBs       = 4
	DefaultTablesPerDB  = 3
	DefaultRowsPerTable = 2000
	DefaultTablets      = 3
	DefaultConcurrency  = 4
	DefaultPgVersion    = 0 // 0 → let the controller choose the endpoint's PG version

	// DefaultEndpointUser/Password/DB: every amp compute endpoint (docker
	// single-container and controller-created alike) provisions the same
	// cloud_admin superuser with this default password (deploy/README.md,
	// deploy/endpoint/supervisor.py COMPUTE_PASSWORD) — the controller API
	// neither sets nor returns endpoint credentials, so this is the real
	// default, not a placeholder.
	DefaultEndpointUser     = "cloud_admin"
	DefaultEndpointPassword = "cloud_admin"
	DefaultEndpointDB       = "postgres"

	DefaultClusterRF       = 0    // 0 → clamp to the node count
	DefaultClusterYSQLPort = 5433 // YugabyteDB libpq port

	// DefaultClusterDeploy: the awsvm AMI ships bare (no YugabyteDB installed),
	// so a live run needs the artifact-deployed install by default.
	DefaultClusterDeploy    = true
	DefaultClusterDeployDir = "/home/ec2-user/yb-software"
)

// teardownBudget bounds the whole LIFO teardown stack, independent of the test
// body's deadline (AGENTS.md §5 — teardown always runs on its own budget).
const teardownBudget = 20 * time.Minute

// Config is the resolved functional.* configuration plus the reused YBA
// connection knobs (env.universe.yba.*). The awsvm/ampstack configs are built
// from the same *core.Config at provision time (reusing their FromConfig), so
// this struct carries only the amp-on-backing-specific scalars.
type Config struct {
	Enabled bool

	// Backing-cluster shape.
	Nodes        int
	InstanceType string // "" → inherit the aws default
	Advertise    string // "private" | "public": which IP nodes advertise / YSQL is reached at

	// Workload shape (both layers).
	NumDBs       int
	TablesPerDB  int
	RowsPerTable int
	Tablets      int
	Concurrency  int
	PgVersion    int // amp project/endpoint PG version (0 → controller default)

	// amp compute endpoint connection (layer 2). Defaults to the real
	// cloud_admin/cloud_admin/postgres credentials every amp endpoint
	// provisions — see DefaultEndpointUser.
	EndpointUser     string
	EndpointPassword string
	EndpointDB       string
	EndpointSSLMode  string // "" → apps/pg default (disable)

	// Backing-cluster runtime layout (yugabyted/raw-yugabyte apps).
	ClusterInstallDir string // yugabyte install root on each node ("" → binaries on PATH)
	ClusterDataDir    string // fs data dir on each node ("" → app default)
	ClusterBaseDir    string // yugabyted --base_dir on each node ("" → app default, yugabyted.DefaultBaseDir)
	ClusterRF         int    // replication factor (0 → clamp to node count)
	ClusterYSQLPort   int    // libpq port
	// ClusterDeploy, when true and ClusterInstallDir is empty, delivers the
	// versions.yugabytedb/artifacts.yugabytedb release onto every node (the
	// apps' own Spec.Deploy seam) before cluster start — the bare-AMI path. An
	// explicit ClusterInstallDir always wins (pre-baked AMI), skipping deploy.
	ClusterDeploy    bool
	ClusterDeployDir string // remote parent dir the release unpacks under

	// YBA target (reused from env.universe.yba.*), for amp_on_yba_universe.
	YBAURL        string
	YBACustomer   string
	YBAToken      string
	UniverseRef   string // universe name or UUID to attach
	YBAInsecure   bool
	YBAOwned      bool           // true → the yba test may create/delete the universe
	YBACreateSpec map[string]any // owned + no ref → CreateUniverse spec (functional.yba.create_spec)
}

// FromConfig reads functional.* (and the reused env.universe.yba.* / versions.*)
// into a Config.
func FromConfig(c *core.Config) Config {
	return Config{
		Enabled:      c.GetBool("functional.enabled", false),
		Nodes:        c.GetInt("functional.nodes", DefaultNodes),
		InstanceType: c.GetString("functional.instance_type", ""),
		Advertise:    c.GetString("functional.advertise", DefaultAdvertise),
		NumDBs:       c.GetInt("functional.dbs", DefaultNumDBs),
		TablesPerDB:  c.GetInt("functional.tables_per_db", DefaultTablesPerDB),
		RowsPerTable: c.GetInt("functional.rows_per_table", DefaultRowsPerTable),
		Tablets:      c.GetInt("functional.tablets", DefaultTablets),
		Concurrency:  c.GetInt("functional.concurrency", DefaultConcurrency),
		PgVersion:    c.GetInt("functional.pg_version", DefaultPgVersion),

		EndpointUser:     c.GetString("functional.amp.endpoint_user", DefaultEndpointUser),
		EndpointPassword: c.GetString("functional.amp.endpoint_password", DefaultEndpointPassword),
		EndpointDB:       c.GetString("functional.amp.endpoint_db", DefaultEndpointDB),
		EndpointSSLMode:  c.GetString("functional.amp.endpoint_sslmode", ""),

		ClusterInstallDir: c.GetString("functional.cluster.install_dir", ""),
		ClusterDataDir:    c.GetString("functional.cluster.data_dir", ""),
		ClusterBaseDir:    c.GetString("functional.cluster.base_dir", ""),
		ClusterRF:         c.GetInt("functional.cluster.rf", DefaultClusterRF),
		ClusterYSQLPort:   c.GetInt("functional.cluster.ysql_port", DefaultClusterYSQLPort),
		ClusterDeploy:     c.GetBool("functional.cluster.deploy", DefaultClusterDeploy),
		ClusterDeployDir:  c.GetString("functional.cluster.deploy_dir", DefaultClusterDeployDir),

		YBAURL:        c.GetString("env.universe.yba.url", ""),
		YBACustomer:   c.GetString("env.universe.yba.customer_uuid", ""),
		YBAToken:      c.GetString("env.universe.yba.api_token", ""),
		UniverseRef:   c.GetString("env.universe.yba.universe", ""),
		YBAInsecure:   c.GetBool("env.universe.yba.insecure_tls", true),
		YBAOwned:      c.GetBool("env.universe.owned", false),
		YBACreateSpec: c.GetMap("functional.yba.create_spec", nil),
	}
}

// ampConfigured reports whether amp can be deployed: ampstack needs a controller
// chart (or a raw manifest) to stand the control plane up. Common to all three
// tests — amp deploy is the layer-2 target.
func ampConfigured(c *core.Config) bool {
	a := ampstack.FromConfig(c)
	return a.ControllerChart != "" || a.ControllerManifest != ""
}

// gateAWS returns a skip reason (and false) unless the AWS-backed foundation
// tests may run: enabled, and amp is deployable. AWS itself is always usable
// via the shared dev-VPC defaults, so no AWS gate is needed.
func (cfg Config) gateAWS(c *core.Config) (string, bool) {
	if !cfg.Enabled {
		return "functional.enabled is not set (self-provisioning tests are opt-in)", false
	}
	if !ampConfigured(c) {
		return "amp not deployable: set env.ampstack.controller_chart (or controller_manifest)", false
	}
	return "", true
}

// gateYBA returns a skip reason (and false) unless the YBA-universe foundation
// test may run: enabled, amp deployable, and a YBA target reachable (url +
// customer + token) with either a universe to attach or owned+a create spec.
func (cfg Config) gateYBA(c *core.Config) (string, bool) {
	if r, ok := cfg.gateAWS(c); !ok {
		return r, false
	}
	if cfg.YBAURL == "" || cfg.YBACustomer == "" || cfg.YBAToken == "" {
		return "YBA not configured: set env.universe.yba.url, customer_uuid and api_token", false
	}
	if cfg.UniverseRef == "" && !(cfg.YBAOwned && len(cfg.YBACreateSpec) > 0) {
		return "no universe target: set env.universe.yba.universe to attach, or env.universe.owned + functional.yba.create_spec to create", false
	}
	return "", true
}

// gateYBAProvisioned returns a skip reason (and false) unless the
// stand-up-our-own-YBA foundation test may run: enabled, amp deployable, a k8s
// cluster to deploy the YBA into (apps.ybastack.kubecontext or kubeconfig set),
// and the deploy/provider secrets resolvable. The provider secrets differ by
// apps.yba.provider_type: the aws backing needs the AWS access/secret keys, the
// kubernetes backing needs a kubeconfig secret (unless it runs in-cluster) and
// no AWS creds at all. It never resolves a value into the reason — only reports
// which named secret failed — so credentials never leak into a skip line.
func (cfg Config) gateYBAProvisioned(c *core.Config) (string, bool) {
	if r, ok := cfg.gateAWS(c); !ok {
		return r, false
	}
	if c.GetString("apps.ybastack.kubecontext", "") == "" && c.GetString("apps.ybastack.kubeconfig", "") == "" {
		return "yba deploy needs a k8s cluster: set apps.ybastack.kubecontext or apps.ybastack.kubeconfig", false
	}
	secrets, err := core.NewSecrets(c)
	if err != nil {
		return "secrets not configured: " + err.Error(), false
	}
	quay := c.GetString("apps.ybastack.quay_secret", ybastack.DefaultQuaySecret)
	if _, err := secrets.Value(quay); err != nil {
		return fmt.Sprintf("quay pull secret %q not resolvable: declare it under secrets:", quay), false
	}
	switch c.GetString("apps.yba.provider_type", "aws") {
	case "kubernetes":
		// The kubernetes backing runs the universe in the same cluster; it needs a
		// kubeconfig secret unless YBA uses its own in-cluster service account. No
		// AWS creds are required.
		if !c.GetBool("apps.yba.k8s.use_in_cluster", false) {
			kubeSecret := c.GetString("apps.yba.k8s.kubeconfig_secret", "")
			if kubeSecret == "" {
				return "kubernetes universe needs a kubeconfig: set apps.yba.k8s.kubeconfig_secret (or apps.yba.k8s.use_in_cluster=true)", false
			}
			if _, err := secrets.Value(kubeSecret); err != nil {
				return fmt.Sprintf("k8s kubeconfig secret %q not resolvable: declare it under secrets:", kubeSecret), false
			}
		}
		return "", true
	case "gcp":
		// The gcp backing launches GCE VMs; it needs the service-account key secret
		// (used both for the provider and to auth the teardown reap). No AWS creds /
		// kubeconfig are required.
		credSecret := c.GetString("apps.yba.gcp.credentials_secret", yba.DefaultGCPCredentialsSecret)
		if credSecret == "" {
			return "gcp universe needs a service-account key: set apps.yba.gcp.credentials_secret", false
		}
		if _, err := secrets.Value(credSecret); err != nil {
			return fmt.Sprintf("gcp service-account key secret %q not resolvable: declare it under secrets:", credSecret), false
		}
		return "", true
	}
	accessName := c.GetString("apps.yba.aws.access_key_secret", yba.DefaultAccessKeySecret)
	if _, err := secrets.Value(accessName); err != nil {
		return fmt.Sprintf("aws access-key secret %q not resolvable: declare it under secrets:", accessName), false
	}
	secretName := c.GetString("apps.yba.aws.secret_key_secret", yba.DefaultSecretKeySecret)
	if _, err := secrets.Value(secretName); err != nil {
		return fmt.Sprintf("aws secret-key secret %q not resolvable: declare it under secrets:", secretName), false
	}
	return "", true
}

// gateYBK8s returns a skip reason (and false) unless the co-located
// YB-in-k8s foundation test may run: enabled, amp deployable, and a k8s cluster
// to deploy into (apps.ybk8s.kubecontext/kubeconfig, or the reused
// env.ampstack.context). No cloud VM credentials are needed — it is pure k8s.
// For genuine co-location the amp deploy must target the SAME cluster, so set
// env.ampstack.context to match apps.ybk8s.kubecontext (or leave both empty →
// the current context).
func (cfg Config) gateYBK8s(c *core.Config) (string, bool) {
	if r, ok := cfg.gateAWS(c); !ok {
		return r, false
	}
	if c.GetString("apps.ybk8s.kubecontext", "") == "" &&
		c.GetString("apps.ybk8s.kubeconfig", "") == "" &&
		c.GetString("env.ampstack.context", "") == "" {
		return "yb-in-k8s needs a k8s cluster: set apps.ybk8s.kubecontext or apps.ybk8s.kubeconfig (or env.ampstack.context)", false
	}
	return "", true
}

// vmConfig builds the cloud-agnostic VM env config, overriding the node count
// and instance type from functional.*. The cloud selector and per-cloud
// instance settings come from env.vm.* / apps.cloud.<cloud>.* via vm.FromConfig;
// the concrete driver (with any explicit credentials resolved) is built
// separately by provisionVMHost via cloud.New.
func (cfg Config) vmConfig(c *core.Config) vm.Config {
	vc := vm.FromConfig(c)
	if cfg.Nodes > 0 {
		vc.NodeCount = cfg.Nodes
	}
	if cfg.InstanceType != "" {
		vc.InstanceType = cfg.InstanceType
	}
	return vc
}
