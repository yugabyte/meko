package core_test

// Canonical-defaults verification: the embedded configs/default.yaml is the
// one place every framework default lives. This file asserts two things:
//
//  1. Every config-reading call site in the framework that has a genuine
//     default (as opposed to a required, project-specific value like
//     env.docker.host) finds that default in the embedded file — not just in
//     its own inline fallback constant. This is the "registry of knobs":
//     extend it whenever a new default.yaml key is added.
//  2. The load precedence — embedded → -c → -jc → -o, and the
//     --no-default-config escape hatch — behaves as documented.
//
// It lives in core_test (not core) so it can import the leaf provider
// packages (docker, sharedk8s, exclusivek8s, sched) that themselves import
// core, without an import cycle.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/benchbase"
	awscloud "github.com/yugabyte/amp/test/functional/framework/apps/cloud/aws"
	gcpcloud "github.com/yugabyte/amp/test/functional/framework/apps/cloud/gcp"
	"github.com/yugabyte/amp/test/functional/framework/apps/sampleapp"
	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybastack"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybpod"
	"github.com/yugabyte/amp/test/functional/framework/artifact"
	"github.com/yugabyte/amp/test/functional/framework/chaos"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/docker"
	"github.com/yugabyte/amp/test/functional/framework/env/exclusivek8s"
	"github.com/yugabyte/amp/test/functional/framework/env/sharedk8s"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
	"github.com/yugabyte/amp/test/functional/framework/env/vm"
	"github.com/yugabyte/amp/test/functional/framework/sched"
	"github.com/yugabyte/amp/test/functional/suites/features"
	"github.com/yugabyte/amp/test/functional/suites/perf"
)

// TestEmbeddedDefaultsSupplyEveryKnownKey is the every-key-present registry:
// loading with no -c/-jc/-o at all must reproduce every code-level default
// constant via its config key.
func TestEmbeddedDefaultsSupplyEveryKnownKey(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// --- sched ---
	if got := cfg.GetInt("sched.capacity", -1); got != sched.DefaultCapacity {
		t.Errorf("sched.capacity = %d, want %d", got, sched.DefaultCapacity)
	}
	if got := cfg.GetInt("sched.parallel", -1); got != 1 {
		t.Errorf("sched.parallel = %d, want 1", got)
	}
	if got := cfg.GetInt("sched.per_env_parallelism", -1); got != sched.DefaultPerEnvParallelism {
		t.Errorf("sched.per_env_parallelism = %d, want %d", got, sched.DefaultPerEnvParallelism)
	}
	weights := map[string]int{
		"single_container":     1,
		"shared_control_plane": 1,
		"dedicated_stack":      2,
		"remote":               1,
		"universe":             1,
	}
	for shape, want := range weights {
		if got := cfg.GetInt("sched.weights."+shape, -1); got != want {
			t.Errorf("sched.weights.%s = %d, want %d", shape, got, want)
		}
	}

	// --- timeouts ---
	timeouts := map[string]time.Duration{
		"timeouts.functional":   core.DefaultFunctionalTimeout,
		"timeouts.stress":       core.DefaultStressTimeout,
		"timeouts.perf":         core.DefaultPerfTimeout,
		"timeouts.long_running": core.DefaultLongRunningTimeout,
		"timeouts.provision":    core.DefaultProvisionTimeout,
		"timeouts.teardown":     core.DefaultTeardownTimeout,
		"timeouts.collect":      core.DefaultCollectTimeout,
	}
	for key, want := range timeouts {
		if got := cfg.GetDuration(key, -1); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	// And through the resolution helpers actually used by lifecycle/scheduler.
	for _, cat := range []core.Category{core.Functional, core.Stress, core.Perf, core.LongRunning} {
		if got, want := core.ResolveCategoryTimeout(cat, cfg), core.CategoryDefaultTimeout(cat); got != want {
			t.Errorf("ResolveCategoryTimeout(%s) = %s, want %s", cat, got, want)
		}
	}
	if got := core.ResolveProvisionTimeout(cfg); got != core.DefaultProvisionTimeout {
		t.Errorf("ResolveProvisionTimeout = %s, want %s", got, core.DefaultProvisionTimeout)
	}
	if got := core.ResolveTeardownTimeout(cfg); got != core.DefaultTeardownTimeout {
		t.Errorf("ResolveTeardownTimeout = %s, want %s", got, core.DefaultTeardownTimeout)
	}
	if got := core.ResolveCollectTimeout(cfg); got != core.DefaultCollectTimeout {
		t.Errorf("ResolveCollectTimeout = %s, want %s", got, core.DefaultCollectTimeout)
	}

	// --- env.docker ---
	dcfg := docker.FromConfig(cfg)
	if dcfg.YBImage != docker.DefaultYBImage {
		t.Errorf("docker YBImage = %q, want %q", dcfg.YBImage, docker.DefaultYBImage)
	}
	if dcfg.ComputePortBase != docker.DefaultComputePortBase {
		t.Errorf("docker ComputePortBase = %d, want %d", dcfg.ComputePortBase, docker.DefaultComputePortBase)
	}
	if dcfg.YBPortBase != docker.DefaultYBPortBase {
		t.Errorf("docker YBPortBase = %d, want %d", dcfg.YBPortBase, docker.DefaultYBPortBase)
	}
	if !dcfg.YBShared {
		t.Errorf("docker YBShared = false, want true")
	}

	// --- env.sharedk8s ---
	scfg := sharedk8s.FromConfig(cfg)
	if scfg.ControllerService != sharedk8s.DefaultControllerService {
		t.Errorf("sharedk8s ControllerService = %q, want %q", scfg.ControllerService, sharedk8s.DefaultControllerService)
	}
	if scfg.ControllerPort != sharedk8s.DefaultControllerPort {
		t.Errorf("sharedk8s ControllerPort = %d, want %d", scfg.ControllerPort, sharedk8s.DefaultControllerPort)
	}
	if scfg.EndpointPort != sharedk8s.DefaultEndpointPort {
		t.Errorf("sharedk8s EndpointPort = %d, want %d", scfg.EndpointPort, sharedk8s.DefaultEndpointPort)
	}
	if scfg.LocalPortBase != sharedk8s.DefaultLocalPortBase {
		t.Errorf("sharedk8s LocalPortBase = %d, want %d", scfg.LocalPortBase, sharedk8s.DefaultLocalPortBase)
	}
	if scfg.PoolName != sharedk8s.DefaultPoolName {
		t.Errorf("sharedk8s PoolName = %q, want %q", scfg.PoolName, sharedk8s.DefaultPoolName)
	}
	if scfg.ReadyTimeout != 2*time.Minute {
		t.Errorf("sharedk8s ReadyTimeout = %s, want 2m", scfg.ReadyTimeout)
	}
	if scfg.PGReadyTimeout != 3*time.Minute {
		t.Errorf("sharedk8s PGReadyTimeout = %s, want 3m", scfg.PGReadyTimeout)
	}

	// --- env.ampstack ---
	acfg := ampstack.FromConfig(cfg)
	if acfg.ControllerImage != ampstack.DefaultQuayControllerImage {
		t.Errorf("ampstack ControllerImage = %q, want %q", acfg.ControllerImage, ampstack.DefaultQuayControllerImage)
	}
	if acfg.EndpointImage != ampstack.DefaultQuayEndpointImage {
		t.Errorf("ampstack EndpointImage = %q, want %q", acfg.EndpointImage, ampstack.DefaultQuayEndpointImage)
	}
	if acfg.YBImage != ampstack.DefaultYBImage {
		t.Errorf("ampstack YBImage = %q, want %q", acfg.YBImage, ampstack.DefaultYBImage)
	}
	if acfg.YBStorage != ampstack.DefaultYBStorage {
		t.Errorf("ampstack YBStorage = %q, want %q", acfg.YBStorage, ampstack.DefaultYBStorage)
	}
	if acfg.DBName != ampstack.DefaultDBName {
		t.Errorf("ampstack DBName = %q, want %q", acfg.DBName, ampstack.DefaultDBName)
	}
	if acfg.Backing.Mode != ampstack.BackingBuiltinYugabyted {
		t.Errorf("ampstack Backing.Mode = %q, want %q", acfg.Backing.Mode, ampstack.BackingBuiltinYugabyted)
	}
	if acfg.Backing.HostPorts != "" {
		t.Errorf("ampstack Backing.HostPorts = %q, want empty (external-only)", acfg.Backing.HostPorts)
	}
	if acfg.Backing.User != "" || acfg.Backing.DBName != "" || acfg.Backing.Password != "" || acfg.Backing.SSLMode != "" || acfg.Backing.AdminHostPorts != "" {
		t.Errorf("ampstack Backing external fields should default empty, got %+v", acfg.Backing)
	}
	if acfg.RBACClusterScoped {
		t.Errorf("ampstack RBACClusterScoped = true, want false (namespace-scoped default)")
	}
	if !acfg.HelmShim {
		t.Errorf("ampstack HelmShim = false, want true (default on until fixed image ships)")
	}
	if acfg.ECRRegistry != ampstack.DefaultECRRegistry {
		t.Errorf("ampstack ECRRegistry = %q, want %q", acfg.ECRRegistry, ampstack.DefaultECRRegistry)
	}
	if acfg.Region != ampstack.DefaultRegion {
		t.Errorf("ampstack Region = %q, want %q", acfg.Region, ampstack.DefaultRegion)
	}
	if acfg.PullSecretName != ampstack.DefaultPullSecretName {
		t.Errorf("ampstack PullSecretName = %q, want %q", acfg.PullSecretName, ampstack.DefaultPullSecretName)
	}
	if acfg.ControllerService != ampstack.DefaultControllerService {
		t.Errorf("ampstack ControllerService = %q, want %q", acfg.ControllerService, ampstack.DefaultControllerService)
	}
	if acfg.ControllerContainer != ampstack.DefaultControllerContainer {
		t.Errorf("ampstack ControllerContainer = %q, want %q", acfg.ControllerContainer, ampstack.DefaultControllerContainer)
	}
	if acfg.ControllerName != ampstack.DefaultControllerName {
		t.Errorf("ampstack ControllerName = %q, want %q", acfg.ControllerName, ampstack.DefaultControllerName)
	}
	if acfg.ControllerPort != ampstack.DefaultControllerPort {
		t.Errorf("ampstack ControllerPort = %d, want %d", acfg.ControllerPort, ampstack.DefaultControllerPort)
	}
	if acfg.EndpointServiceType != ampstack.DefaultEndpointServiceType {
		t.Errorf("ampstack EndpointServiceType = %q, want %q", acfg.EndpointServiceType, ampstack.DefaultEndpointServiceType)
	}
	if acfg.ControllerExpose != ampstack.DefaultControllerExpose {
		t.Errorf("ampstack ControllerExpose = %q, want %q", acfg.ControllerExpose, ampstack.DefaultControllerExpose)
	}
	if acfg.HelmRelease != ampstack.DefaultHelmRelease {
		t.Errorf("ampstack HelmRelease = %q, want %q", acfg.HelmRelease, ampstack.DefaultHelmRelease)
	}
	if acfg.ControllerReach != ampstack.ReachTailnet {
		t.Errorf("ampstack ControllerReach = %q, want %q (tailnet default, no port-forward)", acfg.ControllerReach, ampstack.ReachTailnet)
	}
	if acfg.LocalPort != 0 {
		t.Errorf("ampstack LocalPort = %d, want 0", acfg.LocalPort)
	}
	if acfg.YBReadyTimeout != 15*time.Minute {
		t.Errorf("ampstack YBReadyTimeout = %s, want 15m", acfg.YBReadyTimeout)
	}
	if acfg.ControllerReadyTimeout != 5*time.Minute {
		t.Errorf("ampstack ControllerReadyTimeout = %s, want 5m", acfg.ControllerReadyTimeout)
	}

	// --- env.exclusivek8s ---
	xcfg := exclusivek8s.FromConfig(cfg)
	if xcfg.NamespacePrefix != exclusivek8s.DefaultNamespacePrefix {
		t.Errorf("exclusivek8s NamespacePrefix = %q, want %q", xcfg.NamespacePrefix, exclusivek8s.DefaultNamespacePrefix)
	}
	if xcfg.PullSecretName != exclusivek8s.DefaultPullSecretName {
		t.Errorf("exclusivek8s PullSecretName = %q, want %q", xcfg.PullSecretName, exclusivek8s.DefaultPullSecretName)
	}
	if xcfg.YBImage != exclusivek8s.DefaultYBImage {
		t.Errorf("exclusivek8s YBImage = %q, want %q", xcfg.YBImage, exclusivek8s.DefaultYBImage)
	}
	if xcfg.YBStorage != exclusivek8s.DefaultYBStorage {
		t.Errorf("exclusivek8s YBStorage = %q, want %q", xcfg.YBStorage, exclusivek8s.DefaultYBStorage)
	}
	if xcfg.HelmRelease != exclusivek8s.DefaultHelmRelease {
		t.Errorf("exclusivek8s HelmRelease = %q, want %q", xcfg.HelmRelease, exclusivek8s.DefaultHelmRelease)
	}
	if xcfg.ControllerService != exclusivek8s.DefaultControllerService {
		t.Errorf("exclusivek8s ControllerService = %q, want %q", xcfg.ControllerService, exclusivek8s.DefaultControllerService)
	}
	if xcfg.ControllerPort != exclusivek8s.DefaultControllerPort {
		t.Errorf("exclusivek8s ControllerPort = %d, want %d", xcfg.ControllerPort, exclusivek8s.DefaultControllerPort)
	}
	if xcfg.ControllerReach != "" {
		t.Errorf("exclusivek8s ControllerReach = %q, want empty (inherits ampstack's tailnet default)", xcfg.ControllerReach)
	}
	if xcfg.LocalPortBase != exclusivek8s.DefaultLocalPortBase {
		t.Errorf("exclusivek8s LocalPortBase = %d, want %d", xcfg.LocalPortBase, exclusivek8s.DefaultLocalPortBase)
	}
	if xcfg.YBReadyTimeout != 15*time.Minute {
		t.Errorf("exclusivek8s YBReadyTimeout = %s, want 15m", xcfg.YBReadyTimeout)
	}
	if xcfg.ReadyTimeout != 5*time.Minute {
		t.Errorf("exclusivek8s ReadyTimeout = %s, want 5m", xcfg.ReadyTimeout)
	}

	// --- env.universe ---
	ucfg := universe.FromConfig(cfg)
	if ucfg.Mode != "" {
		t.Errorf("universe Mode = %q, want empty (provider off by default)", ucfg.Mode)
	}
	if ucfg.Owned {
		t.Errorf("universe Owned = true, want false (never delete an attached universe)")
	}
	if ucfg.YSQLPort != universe.DefaultYSQLPort {
		t.Errorf("universe YSQLPort = %d, want %d", ucfg.YSQLPort, universe.DefaultYSQLPort)
	}
	if !ucfg.InsecureTLS {
		t.Errorf("universe InsecureTLS = false, want true (YBA is commonly self-signed)")
	}
	if ucfg.RF != 0 {
		t.Errorf("universe RF = %d, want 0", ucfg.RF)
	}
	if ucfg.Configured() {
		t.Errorf("universe Configured() = true with no mode set, want false")
	}

	// --- owner: the yb_owner tag/label value + resource name prefix, stamped
	// on every cloud resource across aws/gcp/yba (see instructions above
	// default.yaml's `owner:` key). ---
	if got := cfg.GetString("owner", "__MISSING__"); got != awscloud.DefaultOwner {
		t.Errorf("owner = %q, want %q", got, awscloud.DefaultOwner)
	}
	// name_prefix keys are deliberately ABSENT from default.yaml (not even as
	// ""): their default is derived from `owner` at FromConfig read time, and a
	// present-but-empty key would short-circuit that fallback.
	for _, key := range []string{"apps.cloud.aws.name_prefix", "apps.cloud.gcp.name_prefix"} {
		const missingSentinel = "__DERIVED_FROM_OWNER_NOT_YAML__"
		if got := cfg.GetString(key, missingSentinel); got != missingSentinel {
			t.Errorf("%s = %q, want absent from embedded defaults (derived from owner instead)", key, got)
		}
	}

	// --- apps.cloud.aws ---
	awsCfg := awscloud.FromConfig(cfg)
	if awsCfg.Region != awscloud.DefaultRegion {
		t.Errorf("aws Region = %q, want %q", awsCfg.Region, awscloud.DefaultRegion)
	}
	if awsCfg.Subnet != awscloud.DefaultSubnet {
		t.Errorf("aws Subnet = %q, want %q", awsCfg.Subnet, awscloud.DefaultSubnet)
	}
	if strings.Join(awsCfg.SecurityGroups, ",") != strings.Join(awscloud.DefaultSecurityGroups, ",") {
		t.Errorf("aws SecurityGroups = %v, want %v", awsCfg.SecurityGroups, awscloud.DefaultSecurityGroups)
	}
	if awsCfg.KeyName != awscloud.DefaultKeyName {
		t.Errorf("aws KeyName = %q, want %q", awsCfg.KeyName, awscloud.DefaultKeyName)
	}
	if awsCfg.KeyFile != awscloud.DefaultKeyFile {
		t.Errorf("aws KeyFile = %q, want %q", awsCfg.KeyFile, awscloud.DefaultKeyFile)
	}
	if awsCfg.AMI != awscloud.DefaultAMI {
		t.Errorf("aws AMI = %q, want %q", awsCfg.AMI, awscloud.DefaultAMI)
	}
	if awsCfg.InstanceType != awscloud.DefaultInstanceType {
		t.Errorf("aws InstanceType = %q, want %q", awsCfg.InstanceType, awscloud.DefaultInstanceType)
	}
	if awsCfg.SSHUser != awscloud.DefaultSSHUser {
		t.Errorf("aws SSHUser = %q, want %q", awsCfg.SSHUser, awscloud.DefaultSSHUser)
	}
	if awsCfg.Owner != awscloud.DefaultOwner {
		t.Errorf("aws Owner = %q, want %q", awsCfg.Owner, awscloud.DefaultOwner)
	}
	if awsCfg.NamePrefix != awscloud.DefaultOwner {
		t.Errorf("aws NamePrefix = %q, want %q (owner-derived)", awsCfg.NamePrefix, awscloud.DefaultOwner)
	}
	if awsCfg.Reach != awscloud.DefaultReach {
		t.Errorf("aws Reach = %q, want %q", awsCfg.Reach, awscloud.DefaultReach)
	}
	if awsCfg.SSHWaitTimeout != awscloud.DefaultSSHWaitTimeout {
		t.Errorf("aws SSHWaitTimeout = %v, want %v", awsCfg.SSHWaitTimeout, awscloud.DefaultSSHWaitTimeout)
	}

	// --- apps.cloud.gcp ---
	gcpCfg := gcpcloud.FromConfig(cfg)
	if gcpCfg.Project != gcpcloud.DefaultProject {
		t.Errorf("gcp Project = %q, want %q", gcpCfg.Project, gcpcloud.DefaultProject)
	}
	if gcpCfg.Zone != gcpcloud.DefaultZone {
		t.Errorf("gcp Zone = %q, want %q", gcpCfg.Zone, gcpcloud.DefaultZone)
	}
	if gcpCfg.Region != gcpcloud.DefaultRegion {
		t.Errorf("gcp Region = %q, want %q", gcpCfg.Region, gcpcloud.DefaultRegion)
	}
	if gcpCfg.Network != gcpcloud.DefaultNetwork {
		t.Errorf("gcp Network = %q, want %q", gcpCfg.Network, gcpcloud.DefaultNetwork)
	}
	if gcpCfg.Subnetwork != gcpcloud.DefaultSubnetwork {
		t.Errorf("gcp Subnetwork = %q, want %q", gcpCfg.Subnetwork, gcpcloud.DefaultSubnetwork)
	}
	if gcpCfg.MachineType != gcpcloud.DefaultMachineType {
		t.Errorf("gcp MachineType = %q, want %q", gcpCfg.MachineType, gcpcloud.DefaultMachineType)
	}
	if gcpCfg.SSHUser != gcpcloud.DefaultSSHUser {
		t.Errorf("gcp SSHUser = %q, want %q", gcpCfg.SSHUser, gcpcloud.DefaultSSHUser)
	}
	if gcpCfg.Owner != gcpcloud.DefaultOwner {
		t.Errorf("gcp Owner = %q, want %q", gcpCfg.Owner, gcpcloud.DefaultOwner)
	}
	if gcpCfg.NamePrefix != gcpcloud.DefaultOwner {
		t.Errorf("gcp NamePrefix = %q, want %q (owner-derived)", gcpCfg.NamePrefix, gcpcloud.DefaultOwner)
	}
	if gcpCfg.Reach != gcpcloud.DefaultReach {
		t.Errorf("gcp Reach = %q, want %q", gcpCfg.Reach, gcpcloud.DefaultReach)
	}
	if gcpCfg.SSHWaitTimeout != gcpcloud.DefaultSSHWaitTimeout {
		t.Errorf("gcp SSHWaitTimeout = %v, want %v", gcpCfg.SSHWaitTimeout, gcpcloud.DefaultSSHWaitTimeout)
	}

	// --- env.vm (cloud-agnostic host env; node knobs aliased from env.awsvm.*) ---
	vmCfg := vm.FromConfig(cfg)
	if vmCfg.Cloud != vm.DefaultCloud {
		t.Errorf("vm Cloud = %q, want %q", vmCfg.Cloud, vm.DefaultCloud)
	}
	if vmCfg.NodeCount != vm.DefaultNodeCount {
		t.Errorf("vm NodeCount = %d, want %d", vmCfg.NodeCount, vm.DefaultNodeCount)
	}
	if vmCfg.InstanceType != "" {
		t.Errorf("vm InstanceType = %q, want empty (inherit driver default)", vmCfg.InstanceType)
	}
	if !vmCfg.Configured() {
		t.Errorf("vm Configured() = false, want true (defaults make it usable)")
	}

	// --- functional (amp-on-backing knobs) ---
	fcfg := features.FromConfig(cfg)
	if fcfg.Enabled {
		t.Errorf("functional Enabled = true, want false (self-provisioning tests are opt-in)")
	}
	if fcfg.Nodes != features.DefaultNodes {
		t.Errorf("functional Nodes = %d, want %d", fcfg.Nodes, features.DefaultNodes)
	}
	if fcfg.InstanceType != "" {
		t.Errorf("functional InstanceType = %q, want empty (inherit aws default)", fcfg.InstanceType)
	}
	if fcfg.Advertise != features.DefaultAdvertise {
		t.Errorf("functional Advertise = %q, want %q", fcfg.Advertise, features.DefaultAdvertise)
	}
	if fcfg.NumDBs != features.DefaultNumDBs {
		t.Errorf("functional NumDBs = %d, want %d", fcfg.NumDBs, features.DefaultNumDBs)
	}
	if fcfg.TablesPerDB != features.DefaultTablesPerDB {
		t.Errorf("functional TablesPerDB = %d, want %d", fcfg.TablesPerDB, features.DefaultTablesPerDB)
	}
	if fcfg.RowsPerTable != features.DefaultRowsPerTable {
		t.Errorf("functional RowsPerTable = %d, want %d", fcfg.RowsPerTable, features.DefaultRowsPerTable)
	}
	if fcfg.Tablets != features.DefaultTablets {
		t.Errorf("functional Tablets = %d, want %d", fcfg.Tablets, features.DefaultTablets)
	}
	if fcfg.Concurrency != features.DefaultConcurrency {
		t.Errorf("functional Concurrency = %d, want %d", fcfg.Concurrency, features.DefaultConcurrency)
	}
	if fcfg.PgVersion != features.DefaultPgVersion {
		t.Errorf("functional PgVersion = %d, want %d", fcfg.PgVersion, features.DefaultPgVersion)
	}
	if fcfg.EndpointUser != features.DefaultEndpointUser {
		t.Errorf("functional EndpointUser = %q, want %q", fcfg.EndpointUser, features.DefaultEndpointUser)
	}
	if fcfg.EndpointPassword != features.DefaultEndpointPassword {
		t.Errorf("functional EndpointPassword = %q, want %q", fcfg.EndpointPassword, features.DefaultEndpointPassword)
	}
	if fcfg.EndpointDB != features.DefaultEndpointDB {
		t.Errorf("functional EndpointDB = %q, want %q", fcfg.EndpointDB, features.DefaultEndpointDB)
	}
	if fcfg.EndpointSSLMode != "" {
		t.Errorf("functional EndpointSSLMode = %q, want empty (apps/pg default)", fcfg.EndpointSSLMode)
	}
	if fcfg.ClusterRF != features.DefaultClusterRF {
		t.Errorf("functional ClusterRF = %d, want %d", fcfg.ClusterRF, features.DefaultClusterRF)
	}
	if fcfg.ClusterYSQLPort != features.DefaultClusterYSQLPort {
		t.Errorf("functional ClusterYSQLPort = %d, want %d", fcfg.ClusterYSQLPort, features.DefaultClusterYSQLPort)
	}
	if fcfg.ClusterInstallDir != "" || fcfg.ClusterDataDir != "" || fcfg.ClusterBaseDir != "" {
		t.Errorf("functional cluster install/data/base dir should default empty, got %q/%q/%q",
			fcfg.ClusterInstallDir, fcfg.ClusterDataDir, fcfg.ClusterBaseDir)
	}
	if !fcfg.ClusterDeploy {
		t.Errorf("functional ClusterDeploy = false, want true (bare awsvm AMI needs the artifact-deployed install by default)")
	}
	if fcfg.ClusterDeployDir != features.DefaultClusterDeployDir {
		t.Errorf("functional ClusterDeployDir = %q, want %q", fcfg.ClusterDeployDir, features.DefaultClusterDeployDir)
	}

	// --- apps.yba ---
	uspec := yba.BuildUniverseSpec(cfg)
	if uspec.NamePrefix != yba.DefaultUniverseNamePrefix {
		t.Errorf("yba universe NamePrefix = %q, want %q", uspec.NamePrefix, yba.DefaultUniverseNamePrefix)
	}
	if uspec.NumNodes != yba.DefaultNumNodes {
		t.Errorf("yba universe NumNodes = %d, want %d", uspec.NumNodes, yba.DefaultNumNodes)
	}
	if uspec.ReplicationFactor != yba.DefaultReplicationFactor {
		t.Errorf("yba universe ReplicationFactor = %d, want %d", uspec.ReplicationFactor, yba.DefaultReplicationFactor)
	}
	if uspec.InstanceType != yba.DefaultInstanceType {
		t.Errorf("yba universe InstanceType = %q, want %q", uspec.InstanceType, yba.DefaultInstanceType)
	}
	if uspec.Region != yba.DefaultUniverseRegion {
		t.Errorf("yba universe Region = %q, want %q", uspec.Region, yba.DefaultUniverseRegion)
	}
	if uspec.VolumeSizeGB != yba.DefaultVolumeSizeGB {
		t.Errorf("yba universe VolumeSizeGB = %d, want %d", uspec.VolumeSizeGB, yba.DefaultVolumeSizeGB)
	}
	if uspec.InstanceTags["yb_owner"] != yba.DefaultOwner {
		t.Errorf("yba universe InstanceTags[yb_owner] = %q, want %q", uspec.InstanceTags["yb_owner"], yba.DefaultOwner)
	}
	if uspec.VolumeCount != yba.DefaultVolumeCount {
		t.Errorf("yba universe VolumeCount = %d, want %d", uspec.VolumeCount, yba.DefaultVolumeCount)
	}
	if want := cfg.GetString("versions.yugabytedb", ""); uspec.YBSoftwareVersion != want {
		t.Errorf("yba universe YBSoftwareVersion = %q, want fallback to versions.yugabytedb %q", uspec.YBSoftwareVersion, want)
	}
	ybaProviderKeys := map[string]string{
		"apps.yba.aws.provider_name":     yba.DefaultProviderName,
		"apps.yba.aws.ssh_user":          yba.DefaultProviderSSHUser,
		"apps.yba.aws.arch":              yba.DefaultArch,
		"apps.yba.aws.keypair_name":      yba.DefaultKeyPairName,
		"apps.yba.aws.keypair_file":      yba.DefaultKeyPairFile,
		"apps.yba.aws.access_key_secret": yba.DefaultAccessKeySecret,
		"apps.yba.aws.secret_key_secret": yba.DefaultSecretKeySecret,
	}
	for key, want := range ybaProviderKeys {
		if got := cfg.GetString(key, "__MISSING__"); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if regions := cfg.GetMapSlice("apps.yba.aws.regions", nil); len(regions) == 0 {
		t.Errorf("apps.yba.aws.regions is empty in embedded defaults, want at least one region")
	}
	// base_url/customer_uuid/api_token_secret are documented-empty (required to
	// be set by the caller), but must be PRESENT so the key exists.
	for _, key := range []string{"apps.yba.base_url", "apps.yba.customer_uuid", "apps.yba.api_token_secret", "apps.yba.universe.yb_version"} {
		if got := cfg.GetString(key, "__MISSING__"); got != "" {
			t.Errorf("%s = %q, want present and empty", key, got)
		}
	}
	// provider_type defaults to aws (the AWS backing is the unchanged default).
	if got := cfg.GetString("apps.yba.provider_type", "__MISSING__"); got != "aws" {
		t.Errorf("apps.yba.provider_type = %q, want aws", got)
	}

	// --- apps.yba.k8s (kubernetes provider + universe spec) ---
	k8sUni := yba.BuildK8sUniverseSpec(cfg)
	if k8sUni.ProviderCode != "kubernetes" {
		t.Errorf("k8s universe ProviderCode = %q, want kubernetes", k8sUni.ProviderCode)
	}
	if k8sUni.NumNodes != yba.DefaultK8sNumNodes || k8sUni.ReplicationFactor != yba.DefaultK8sReplication {
		t.Errorf("k8s universe nodes/RF = %d/%d, want %d/%d", k8sUni.NumNodes, k8sUni.ReplicationFactor, yba.DefaultK8sNumNodes, yba.DefaultK8sReplication)
	}
	if k8sUni.K8sStorageClass != yba.DefaultK8sStorageClass {
		t.Errorf("k8s universe StorageClass = %q, want %q", k8sUni.K8sStorageClass, yba.DefaultK8sStorageClass)
	}
	if k8sUni.VolumeSizeGB != yba.DefaultK8sVolumeSizeGB {
		t.Errorf("k8s universe VolumeSizeGB = %d, want %d", k8sUni.VolumeSizeGB, yba.DefaultK8sVolumeSizeGB)
	}
	if k8sUni.MasterCPUCores != yba.DefaultK8sMasterCPUCores || k8sUni.MasterMemGiB != yba.DefaultK8sMasterMemGiB {
		t.Errorf("k8s master resources = %d cpu / %d gib, want %d/%d", k8sUni.MasterCPUCores, k8sUni.MasterMemGiB, yba.DefaultK8sMasterCPUCores, yba.DefaultK8sMasterMemGiB)
	}
	if k8sUni.TServerCPUCores != yba.DefaultK8sTServerCPUCores || k8sUni.TServerMemGiB != yba.DefaultK8sTServerMemGiB {
		t.Errorf("k8s tserver resources = %d cpu / %d gib, want %d/%d", k8sUni.TServerCPUCores, k8sUni.TServerMemGiB, yba.DefaultK8sTServerCPUCores, yba.DefaultK8sTServerMemGiB)
	}
	if k8sUni.Region != yba.DefaultK8sRegion {
		t.Errorf("k8s universe Region = %q, want %q", k8sUni.Region, yba.DefaultK8sRegion)
	}
	ybaK8sKeys := map[string]string{
		"apps.yba.k8s.provider_name":    yba.DefaultK8sProviderName,
		"apps.yba.k8s.kube_provider":    yba.DefaultK8sProvider,
		"apps.yba.k8s.service_account":  yba.DefaultK8sServiceAccount,
		"apps.yba.k8s.image_registry":   yba.DefaultK8sImageRegistry,
		"apps.yba.k8s.pull_secret_name": yba.DefaultK8sPullSecretName,
		"apps.yba.k8s.storage_class":    yba.DefaultK8sStorageClass,
		"apps.yba.k8s.region":           yba.DefaultK8sRegion,
	}
	for key, want := range ybaK8sKeys {
		if got := cfg.GetString(key, "__MISSING__"); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// kube_domain/kubeconfig_secret/pull_secret_secret are documented-empty but
	// must be PRESENT.
	for _, key := range []string{"apps.yba.k8s.kube_domain", "apps.yba.k8s.kubeconfig_secret", "apps.yba.k8s.pull_secret_secret"} {
		if got := cfg.GetString(key, "__MISSING__"); got != "" {
			t.Errorf("%s = %q, want present and empty", key, got)
		}
	}
	if zones := cfg.GetMapSlice("apps.yba.k8s.zones", nil); len(zones) == 0 {
		t.Errorf("apps.yba.k8s.zones is empty in embedded defaults, want at least one zone")
	}

	// --- apps.yba.gcp (gcp provider + universe spec) ---
	gcpUni := yba.BuildGCPUniverseSpec(cfg)
	if gcpUni.ProviderCode != "gcp" {
		t.Errorf("gcp universe ProviderCode = %q, want gcp", gcpUni.ProviderCode)
	}
	if gcpUni.InstanceType != yba.DefaultGCPInstanceType {
		t.Errorf("gcp universe InstanceType = %q, want %q", gcpUni.InstanceType, yba.DefaultGCPInstanceType)
	}
	if gcpUni.NumNodes != yba.DefaultGCPNumNodes || gcpUni.ReplicationFactor != yba.DefaultGCPReplication {
		t.Errorf("gcp universe nodes/RF = %d/%d, want %d/%d", gcpUni.NumNodes, gcpUni.ReplicationFactor, yba.DefaultGCPNumNodes, yba.DefaultGCPReplication)
	}
	if gcpUni.StorageType != yba.DefaultGCPStorageType {
		t.Errorf("gcp universe StorageType = %q, want %q", gcpUni.StorageType, yba.DefaultGCPStorageType)
	}
	if gcpUni.VolumeSizeGB != yba.DefaultGCPVolumeSizeGB {
		t.Errorf("gcp universe VolumeSizeGB = %d, want %d", gcpUni.VolumeSizeGB, yba.DefaultGCPVolumeSizeGB)
	}
	if gcpUni.Region != yba.DefaultGCPRegion {
		t.Errorf("gcp universe Region = %q, want %q", gcpUni.Region, yba.DefaultGCPRegion)
	}
	ybaGCPKeys := map[string]string{
		"apps.yba.gcp.provider_name":      yba.DefaultGCPProviderName,
		"apps.yba.gcp.project":            yba.DefaultGCPProject,
		"apps.yba.gcp.network":            yba.DefaultGCPNetwork,
		"apps.yba.gcp.subnetwork":         yba.DefaultGCPSubnetwork,
		"apps.yba.gcp.region":             yba.DefaultGCPRegion,
		"apps.yba.gcp.zone":               yba.DefaultGCPZone,
		"apps.yba.gcp.ssh_user":           yba.DefaultGCPSSHUser,
		"apps.yba.gcp.arch":               yba.DefaultGCPArch,
		"apps.yba.gcp.credentials_secret": yba.DefaultGCPCredentialsSecret,
		"apps.yba.gcp.instance_type":      yba.DefaultGCPInstanceType,
		"apps.yba.gcp.storage_type":       yba.DefaultGCPStorageType,
	}
	for key, want := range ybaGCPKeys {
		if got := cfg.GetString(key, "__MISSING__"); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// keypair_name/keypair_file/image are documented-empty but must be PRESENT.
	for _, key := range []string{"apps.yba.gcp.keypair_name", "apps.yba.gcp.keypair_file", "apps.yba.gcp.image"} {
		if got := cfg.GetString(key, "__MISSING__"); got != "" {
			t.Errorf("%s = %q, want present and empty", key, got)
		}
	}
	if got := cfg.GetInt("apps.yba.gcp.ssh_port", -1); got != yba.DefaultGCPSSHPort {
		t.Errorf("apps.yba.gcp.ssh_port = %d, want %d", got, yba.DefaultGCPSSHPort)
	}

	// --- apps.ybastack ---
	ybCfg := ybastack.FromConfig(cfg)
	if ybCfg.NamespacePrefix != ybastack.DefaultNamespacePrefix {
		t.Errorf("ybastack NamespacePrefix = %q, want %q", ybCfg.NamespacePrefix, ybastack.DefaultNamespacePrefix)
	}
	if ybCfg.ImageRegistry != ybastack.DefaultImageRegistry {
		t.Errorf("ybastack ImageRegistry = %q, want %q", ybCfg.ImageRegistry, ybastack.DefaultImageRegistry)
	}
	if ybCfg.YBImageRegistry != ybastack.DefaultYBImageRegistry {
		t.Errorf("ybastack YBImageRegistry = %q, want %q", ybCfg.YBImageRegistry, ybastack.DefaultYBImageRegistry)
	}
	if ybCfg.QuaySecret != ybastack.DefaultQuaySecret {
		t.Errorf("ybastack QuaySecret = %q, want %q", ybCfg.QuaySecret, ybastack.DefaultQuaySecret)
	}
	if ybCfg.PullSecretName != ybastack.DefaultPullSecretName {
		t.Errorf("ybastack PullSecretName = %q, want %q", ybCfg.PullSecretName, ybastack.DefaultPullSecretName)
	}
	if ybCfg.ServiceAccount != ybastack.DefaultServiceAccount {
		t.Errorf("ybastack ServiceAccount = %q, want %q", ybCfg.ServiceAccount, ybastack.DefaultServiceAccount)
	}
	if ybCfg.StorageClass != ybastack.DefaultStorageClass {
		t.Errorf("ybastack StorageClass = %q, want %q", ybCfg.StorageClass, ybastack.DefaultStorageClass)
	}
	if ybCfg.Reach != ybastack.DefaultReach {
		t.Errorf("ybastack Reach = %q, want %q", ybCfg.Reach, ybastack.DefaultReach)
	}
	if ybCfg.ServicePort != ybastack.DefaultServicePort {
		t.Errorf("ybastack ServicePort = %d, want %d", ybCfg.ServicePort, ybastack.DefaultServicePort)
	}
	if ybCfg.HelmTimeout != ybastack.DefaultHelmTimeout {
		t.Errorf("ybastack HelmTimeout = %s, want %s", ybCfg.HelmTimeout, ybastack.DefaultHelmTimeout)
	}
	if !ybCfg.InsecureTLS {
		t.Errorf("ybastack InsecureTLS = false, want true (default)")
	}
	if ybCfg.BootstrapName != ybastack.DefaultBootstrapName {
		t.Errorf("ybastack BootstrapName = %q, want %q", ybCfg.BootstrapName, ybastack.DefaultBootstrapName)
	}
	if ybCfg.BootstrapEmail != ybastack.DefaultBootstrapEmail {
		t.Errorf("ybastack BootstrapEmail = %q, want %q", ybCfg.BootstrapEmail, ybastack.DefaultBootstrapEmail)
	}
	if ybCfg.BootstrapPassword != ybastack.DefaultBootstrapPassword {
		t.Errorf("ybastack BootstrapPassword = %q, want %q", ybCfg.BootstrapPassword, ybastack.DefaultBootstrapPassword)
	}
	if ybCfg.BootstrapTimeout != ybastack.DefaultBootstrapTimeout {
		t.Errorf("ybastack BootstrapTimeout = %s, want %s", ybCfg.BootstrapTimeout, ybastack.DefaultBootstrapTimeout)
	}
	// chart_version is documented-empty (falls back to versions.yugabytedb, which
	// FromConfig resolves), so ChartVersion is the yugabytedb default here.
	if want := cfg.GetString("versions.yugabytedb", ""); ybCfg.ChartVersion != want {
		t.Errorf("ybastack ChartVersion = %q, want fallback to versions.yugabytedb %q", ybCfg.ChartVersion, want)
	}
	// service_name/pod_selector/helm_release/rbac_dir/kubecontext/kubeconfig are
	// documented-empty (derived or ambient), but must be PRESENT.
	for _, key := range []string{
		"apps.ybastack.service_name", "apps.ybastack.pod_selector", "apps.ybastack.helm_release",
		"apps.ybastack.rbac_dir", "apps.ybastack.kubecontext", "apps.ybastack.kubeconfig",
		"apps.ybastack.chart_version",
	} {
		if got := cfg.GetString(key, "__MISSING__"); got != "" {
			t.Errorf("%s = %q, want present and empty", key, got)
		}
	}

	// --- apps.ybk8s ---
	k8sCfg := ybk8s.FromConfig(cfg)
	if k8sCfg.NamespacePrefix != ybk8s.DefaultNamespacePrefix {
		t.Errorf("ybk8s NamespacePrefix = %q, want %q", k8sCfg.NamespacePrefix, ybk8s.DefaultNamespacePrefix)
	}
	if k8sCfg.ImageRegistry != ybk8s.DefaultImageRegistry {
		t.Errorf("ybk8s ImageRegistry = %q, want %q", k8sCfg.ImageRegistry, ybk8s.DefaultImageRegistry)
	}
	if k8sCfg.ReplicationFactor != ybk8s.DefaultReplicationFactor {
		t.Errorf("ybk8s ReplicationFactor = %d, want %d", k8sCfg.ReplicationFactor, ybk8s.DefaultReplicationFactor)
	}
	if k8sCfg.StorageClass != ybk8s.DefaultStorageClass {
		t.Errorf("ybk8s StorageClass = %q, want %q", k8sCfg.StorageClass, ybk8s.DefaultStorageClass)
	}
	if k8sCfg.StorageSize != ybk8s.DefaultStorageSize {
		t.Errorf("ybk8s StorageSize = %q, want %q", k8sCfg.StorageSize, ybk8s.DefaultStorageSize)
	}
	if k8sCfg.ServiceName != ybk8s.DefaultServiceName {
		t.Errorf("ybk8s ServiceName = %q, want %q", k8sCfg.ServiceName, ybk8s.DefaultServiceName)
	}
	if k8sCfg.YSQLPort != ybk8s.DefaultYSQLPort {
		t.Errorf("ybk8s YSQLPort = %d, want %d", k8sCfg.YSQLPort, ybk8s.DefaultYSQLPort)
	}
	if k8sCfg.MasterCPU != ybk8s.DefaultMasterCPU || k8sCfg.MasterMemory != ybk8s.DefaultMasterMemory {
		t.Errorf("ybk8s master resources = %q/%q, want %q/%q", k8sCfg.MasterCPU, k8sCfg.MasterMemory, ybk8s.DefaultMasterCPU, ybk8s.DefaultMasterMemory)
	}
	if k8sCfg.TServerCPU != ybk8s.DefaultTServerCPU || k8sCfg.TServerMemory != ybk8s.DefaultTServerMemory {
		t.Errorf("ybk8s tserver resources = %q/%q, want %q/%q", k8sCfg.TServerCPU, k8sCfg.TServerMemory, ybk8s.DefaultTServerCPU, ybk8s.DefaultTServerMemory)
	}
	if k8sCfg.Reach != ybk8s.DefaultReach {
		t.Errorf("ybk8s Reach = %q, want %q", k8sCfg.Reach, ybk8s.DefaultReach)
	}
	if k8sCfg.ImagePullRegistry != ybk8s.DefaultImagePullRegistry {
		t.Errorf("ybk8s ImagePullRegistry = %q, want %q", k8sCfg.ImagePullRegistry, ybk8s.DefaultImagePullRegistry)
	}
	if k8sCfg.ImagePullSecretName != ybk8s.DefaultImagePullSecretName {
		t.Errorf("ybk8s ImagePullSecretName = %q, want %q", k8sCfg.ImagePullSecretName, ybk8s.DefaultImagePullSecretName)
	}
	if k8sCfg.HelmTimeout != ybk8s.DefaultHelmTimeout {
		t.Errorf("ybk8s HelmTimeout = %s, want %s", k8sCfg.HelmTimeout, ybk8s.DefaultHelmTimeout)
	}
	// master_replicas/tserver_replicas default 0 → applyDefaults clamps to
	// ReplicationFactor; FromConfig leaves them 0 here (the raw config default).
	if k8sCfg.MasterReplicas != 0 || k8sCfg.TServerReplicas != 0 {
		t.Errorf("ybk8s replicas = %d/%d, want 0/0 (clamped to RF at deploy)", k8sCfg.MasterReplicas, k8sCfg.TServerReplicas)
	}
	// chart_version/image_tag are documented-empty (fall back to
	// versions.yugabytedb / chart_version), which FromConfig resolves.
	if want := cfg.GetString("versions.yugabytedb", ""); k8sCfg.ChartVersion != want {
		t.Errorf("ybk8s ChartVersion = %q, want fallback to versions.yugabytedb %q", k8sCfg.ChartVersion, want)
	}
	if want := cfg.GetString("versions.yugabytedb", ""); k8sCfg.ImageTag != want {
		t.Errorf("ybk8s ImageTag = %q, want fallback to chart_version %q", k8sCfg.ImageTag, want)
	}
	for _, key := range []string{
		"apps.ybk8s.kubecontext", "apps.ybk8s.kubeconfig", "apps.ybk8s.chart_version",
		"apps.ybk8s.image_tag", "apps.ybk8s.helm_release", "apps.ybk8s.image_pull_secret",
	} {
		if got := cfg.GetString(key, "__MISSING__"); got != "" {
			t.Errorf("%s = %q, want present and empty", key, got)
		}
	}

	// --- apps.ybpod ---
	podCfg := ybpod.FromConfig(cfg)
	if podCfg.NamespacePrefix != ybpod.DefaultNamespacePrefix {
		t.Errorf("ybpod NamespacePrefix = %q, want %q", podCfg.NamespacePrefix, ybpod.DefaultNamespacePrefix)
	}
	if podCfg.BaseImage != ybpod.DefaultBaseImage {
		t.Errorf("ybpod BaseImage = %q, want %q", podCfg.BaseImage, ybpod.DefaultBaseImage)
	}
	if podCfg.Arch != ybpod.DefaultArch {
		t.Errorf("ybpod Arch = %q, want %q", podCfg.Arch, ybpod.DefaultArch)
	}
	if podCfg.InstallDir != ybpod.DefaultInstallDir || podCfg.BaseDir != ybpod.DefaultBaseDir {
		t.Errorf("ybpod dirs = %q/%q, want %q/%q", podCfg.InstallDir, podCfg.BaseDir, ybpod.DefaultInstallDir, ybpod.DefaultBaseDir)
	}
	if podCfg.ServiceName != ybpod.DefaultServiceName {
		t.Errorf("ybpod ServiceName = %q, want %q", podCfg.ServiceName, ybpod.DefaultServiceName)
	}
	if podCfg.YSQLPort != ybpod.DefaultYSQLPort {
		t.Errorf("ybpod YSQLPort = %d, want %d", podCfg.YSQLPort, ybpod.DefaultYSQLPort)
	}
	if podCfg.ReadyTimeout != ybpod.DefaultReadyTimeout {
		t.Errorf("ybpod ReadyTimeout = %s, want %s", podCfg.ReadyTimeout, ybpod.DefaultReadyTimeout)
	}
	// version empty → fall back to versions.yugabytedb.
	if want := cfg.GetString("versions.yugabytedb", ""); podCfg.Version != want {
		t.Errorf("ybpod Version = %q, want fallback to versions.yugabytedb %q", podCfg.Version, want)
	}
	for _, key := range []string{
		"apps.ybpod.kubecontext", "apps.ybpod.kubeconfig", "apps.ybpod.version",
		"apps.ybpod.base_image_pull_secret",
	} {
		if got := cfg.GetString(key, "__MISSING__"); got != "" {
			t.Errorf("%s = %q, want present and empty", key, got)
		}
	}

	// apps.yugabyte.* / apps.yugabyted.* are deliberately NOT documented here:
	// no code reads them from config — the functional amp-on-backing provisioners
	// build the yugabyte/yugabyted Spec directly from functional.cluster.* plus
	// the apps' own Default* constants. They are code constants, not framework
	// knobs.

	// --- artifact / report: documented-empty keys must still be *present*
	// (lookup succeeds with ""), distinguishing "off by design" from "key
	// missing entirely" — GetString only returns the sentinel on a lookup
	// miss.
	const sentinel = "__MISSING__"
	for _, key := range []string{
		"artifact.cache_dir", "report.out_dir", "report.junit_path",
		"apps.cloud.aws.access_key_secret", "apps.cloud.aws.secret_key_secret",
		"apps.cloud.gcp.image", "apps.cloud.gcp.image_project",
		"apps.cloud.gcp.key_file", "apps.cloud.gcp.public_key_file",
		"apps.cloud.gcp.service_account_secret",
	} {
		if got := cfg.GetString(key, sentinel); got != "" {
			t.Errorf("%s = %q, want present and empty (got sentinel or a stray value)", key, got)
		}
	}

	// --- report retention caps ---
	if got := cfg.GetInt("report.max_steps_retained", -1); got != core.DefaultMaxStepsRetained {
		t.Errorf("report.max_steps_retained = %d, want %d", got, core.DefaultMaxStepsRetained)
	}
	if got := cfg.GetInt("report.max_metrics_retained", -1); got != core.DefaultMaxMetricsRetained {
		t.Errorf("report.max_metrics_retained = %d, want %d", got, core.DefaultMaxMetricsRetained)
	}
	if got := cfg.GetString("report.collect_diagnostics", "__MISSING__"); got != core.DefaultCollectDiagnostics {
		t.Errorf("report.collect_diagnostics = %q, want %q", got, core.DefaultCollectDiagnostics)
	}

	// --- secrets: the section must be present (an empty map) so NewSecrets
	// always parses a real map and the redaction path has a subtree to redact.
	if _, ok := cfg.Resolved()["secrets"]; !ok {
		t.Errorf("secrets{} section missing from embedded defaults")
	}
	if secrets, err := core.NewSecrets(cfg); err != nil {
		t.Errorf("NewSecrets on embedded defaults: %v", err)
	} else if names := secrets.Names(); len(names) != 0 {
		t.Errorf("embedded secrets should be empty, got %v", names)
	}

	// --- versions{} / artifacts{}: both sections must be present (not absent)
	// so artifact.Resolve always sees a real map, never nil. The framework
	// itself owns exactly one entry — yugabytedb, for the functional suite's
	// amp-on-backing bare-AMI cluster deploy — everything else is per-project.
	resolved := cfg.Resolved()
	if _, ok := resolved["versions"]; !ok {
		t.Errorf("versions{} section missing from embedded defaults")
	}
	if _, ok := resolved["artifacts"]; !ok {
		t.Errorf("artifacts{} section missing from embedded defaults")
	}

	// --- artifacts.yugabytedb: resolves to the exact ARM release the
	// functional amp-on-backing cluster provisioners deploy onto the bare awsvm AMI.
	specs, err := artifact.Resolve(cfg)
	if err != nil {
		t.Fatalf("artifact.Resolve: %v", err)
	}
	yb, ok := specs["yugabytedb"]
	if !ok {
		t.Fatalf("artifacts.yugabytedb not resolved: %v", specs)
	}
	// versions.yugabytedb is ONE build-suffixed string; the artifact derives the
	// full form ({yugabytedb}) for the filename and the bare form
	// ({yugabytedb_base}) for the release directory.
	wantVersion := cfg.GetString("versions.yugabytedb", "")
	if wantVersion == "" {
		t.Fatalf("versions.yugabytedb missing from embedded defaults")
	}
	wantBase := core.BaseVersion(wantVersion)
	if wantBase == "" || wantBase == wantVersion {
		t.Fatalf("versions.yugabytedb %q must carry a -bNNN build suffix", wantVersion)
	}
	if want := "yugabyte-" + wantBase; yb.Version != want {
		t.Errorf("artifacts.yugabytedb version = %q, want %q (the tarball's real extracted directory name)", yb.Version, want)
	}
	wantGlob := "yugabyte-" + wantVersion + "-el8-aarch64.tar.gz"
	if yb.GlobPath != wantGlob {
		t.Errorf("artifacts.yugabytedb glob_path = %q, want %q", yb.GlobPath, wantGlob)
	}
	if want := "https://software.yugabyte.com/releases/" + wantBase + "/" + wantGlob; yb.URL != want {
		t.Errorf("artifacts.yugabytedb url = %q, want %q", yb.URL, want)
	}
	if yb.Type != artifact.TypeHTTP {
		t.Errorf("artifacts.yugabytedb type = %q, want http", yb.Type)
	}
	if !yb.RemoteDownload || !yb.RemoteUnzip {
		t.Errorf("artifacts.yugabytedb remote_download/remote_unzip = %v/%v, want true/true", yb.RemoteDownload, yb.RemoteUnzip)
	}
	if yb.Target != "server" {
		t.Errorf("artifacts.yugabytedb target = %q, want %q", yb.Target, "server")
	}

	// --- apps.sampleapp (Java sample-apps workload driver) ---
	sacfg := sampleapp.FromConfig(cfg)
	if sacfg.Workload != sampleapp.DefaultWorkload {
		t.Errorf("sampleapp Workload = %q, want %q", sacfg.Workload, sampleapp.DefaultWorkload)
	}
	if sacfg.NumTables != sampleapp.DefaultNumTables {
		t.Errorf("sampleapp NumTables = %d, want %d", sacfg.NumTables, sampleapp.DefaultNumTables)
	}
	if sacfg.NumValueColumns != 0 || sacfg.NumThreadsRead != 0 || sacfg.NumThreadsWrite != 0 {
		t.Errorf("sampleapp num_value_columns/num_threads_* should default 0, got %d/%d/%d",
			sacfg.NumValueColumns, sacfg.NumThreadsRead, sacfg.NumThreadsWrite)
	}
	if sacfg.RunTime != 0 {
		t.Errorf("sampleapp RunTime = %s, want 0 (run until stopped)", sacfg.RunTime)
	}
	if sacfg.UUID != "" {
		t.Errorf("sampleapp UUID = %q, want empty", sacfg.UUID)
	}
	if sacfg.SkipWorkload || sacfg.SkipDDL {
		t.Errorf("sampleapp skip_workload/skip_ddl should default false, got %v/%v", sacfg.SkipWorkload, sacfg.SkipDDL)
	}
	if sacfg.YSQLPort != sampleapp.DefaultYSQLPort || sacfg.YCQLPort != sampleapp.DefaultYCQLPort {
		t.Errorf("sampleapp ysql/ycql port = %d/%d, want %d/%d",
			sacfg.YSQLPort, sacfg.YCQLPort, sampleapp.DefaultYSQLPort, sampleapp.DefaultYCQLPort)
	}

	// --- apps.benchbase (BenchBase benchmark driver) ---
	bbcfg := benchbase.FromConfig(cfg)
	if bbcfg.Benchmark != benchbase.DefaultBenchmark {
		t.Errorf("benchbase Benchmark = %q, want %q", bbcfg.Benchmark, benchbase.DefaultBenchmark)
	}
	if bbcfg.Warehouses != benchbase.DefaultWarehouses {
		t.Errorf("benchbase Warehouses = %d, want %d", bbcfg.Warehouses, benchbase.DefaultWarehouses)
	}
	if bbcfg.Terminals != benchbase.DefaultTerminals {
		t.Errorf("benchbase Terminals = %d, want %d", bbcfg.Terminals, benchbase.DefaultTerminals)
	}
	if bbcfg.Duration != benchbase.DefaultDuration {
		t.Errorf("benchbase Duration = %s, want %s", bbcfg.Duration, benchbase.DefaultDuration)
	}
	if bbcfg.Scalefactor != 0 {
		t.Errorf("benchbase Scalefactor = %d, want 0 (fall back to warehouses)", bbcfg.Scalefactor)
	}
	if bbcfg.Rate != benchbase.DefaultRate {
		t.Errorf("benchbase Rate = %q, want %q", bbcfg.Rate, benchbase.DefaultRate)
	}

	// --- artifacts.sampleapp: byte-checks the resolved github_release matches
	// the stress framework's `stress-sample-app-tool` (src/configs/artifacts.conf).
	saVer := cfg.GetString("versions.sampleapp", "")
	if saVer != "1.4.90" {
		t.Errorf("versions.sampleapp = %q, want 1.4.90 (the stress-pinned jar version)", saVer)
	}
	saSpec, ok := specs["sampleapp"]
	if !ok {
		t.Fatalf("artifacts.sampleapp not resolved: %v", specs)
	}
	if want := "yb-stress-sample-apps-" + saVer + ".jar"; saSpec.GlobPath != want {
		t.Errorf("artifacts.sampleapp glob_path = %q, want %q", saSpec.GlobPath, want)
	}
	if want := "https://api.github.com/repos/yugabyte/yb-stress-test/releases/tags/ssa_" + saVer; saSpec.URL != want {
		t.Errorf("artifacts.sampleapp url = %q, want %q", saSpec.URL, want)
	}
	if saSpec.Type != artifact.TypeGithubRelease {
		t.Errorf("artifacts.sampleapp type = %q, want github_release", saSpec.Type)
	}
	if saSpec.Secret != "gh_package" {
		t.Errorf("artifacts.sampleapp secret = %q, want gh_package", saSpec.Secret)
	}
	if saSpec.Target != "client" {
		t.Errorf("artifacts.sampleapp target = %q, want client", saSpec.Target)
	}

	// --- artifacts.benchbase: byte-checks the resolved public http release
	// matches the stress framework's `benchbase` (src/configs/artifacts.conf).
	bbVer := cfg.GetString("versions.benchbase", "")
	if bbVer != "1.12" {
		t.Errorf("versions.benchbase = %q, want 1.12 (the stress-pinned release)", bbVer)
	}
	bbSpec, ok := specs["benchbase"]
	if !ok {
		t.Fatalf("artifacts.benchbase not resolved: %v", specs)
	}
	if bbSpec.GlobPath != "benchbase-yugabyte.tgz" {
		t.Errorf("artifacts.benchbase glob_path = %q, want benchbase-yugabyte.tgz", bbSpec.GlobPath)
	}
	if want := "https://github.com/yugabyte/benchbase/releases/download/" + bbVer + "/benchbase-yugabyte.tgz"; bbSpec.URL != want {
		t.Errorf("artifacts.benchbase url = %q, want %q", bbSpec.URL, want)
	}
	if bbSpec.Type != artifact.TypeHTTP {
		t.Errorf("artifacts.benchbase type = %q, want http", bbSpec.Type)
	}
	if bbSpec.Secret != "" {
		t.Errorf("artifacts.benchbase secret = %q, want empty (public release)", bbSpec.Secret)
	}
	if !bbSpec.RemoteDownload || !bbSpec.RemoteUnzip {
		t.Errorf("artifacts.benchbase remote_download/remote_unzip = %v/%v, want true/true", bbSpec.RemoteDownload, bbSpec.RemoteUnzip)
	}
	if bbSpec.Target != "client" {
		t.Errorf("artifacts.benchbase target = %q, want client", bbSpec.Target)
	}

	// --- chaos (fault-injection engine) ---
	chCfg := chaos.FromConfig(cfg)
	chDefault := chaos.DefaultConfig()
	if len(chCfg.Actions) != 0 {
		t.Errorf("chaos actions = %v, want empty (all registered actions)", chCfg.Actions)
	}
	if chCfg.OwnerPrefix != chDefault.OwnerPrefix {
		t.Errorf("chaos owner_prefix = %q, want %q", chCfg.OwnerPrefix, chDefault.OwnerPrefix)
	}
	if chCfg.NetDevice != chDefault.NetDevice {
		t.Errorf("chaos net_device = %q, want %q", chCfg.NetDevice, chDefault.NetDevice)
	}
	if len(chCfg.PartitionPorts) != len(chDefault.PartitionPorts) {
		t.Errorf("chaos partition_ports = %v, want %v", chCfg.PartitionPorts, chDefault.PartitionPorts)
	} else {
		for i, p := range chDefault.PartitionPorts {
			if chCfg.PartitionPorts[i] != p {
				t.Errorf("chaos partition_ports[%d] = %d, want %d", i, chCfg.PartitionPorts[i], p)
			}
		}
	}
	if chCfg.DelayMs != chDefault.DelayMs || chCfg.LossPercent != chDefault.LossPercent {
		t.Errorf("chaos delay_ms/loss_percent = %d/%d, want %d/%d", chCfg.DelayMs, chCfg.LossPercent, chDefault.DelayMs, chDefault.LossPercent)
	}
	if chCfg.ProcessPattern != chDefault.ProcessPattern || chCfg.ServiceUnit != chDefault.ServiceUnit {
		t.Errorf("chaos process_pattern/service_unit = %q/%q, want %q/%q", chCfg.ProcessPattern, chCfg.ServiceUnit, chDefault.ProcessPattern, chDefault.ServiceUnit)
	}
	if strings.Join(chCfg.NodeUnits, ",") != strings.Join(chDefault.NodeUnits, ",") {
		t.Errorf("chaos node_units = %v, want %v", chCfg.NodeUnits, chDefault.NodeUnits)
	}
	if chCfg.HoldMin != chDefault.HoldMin || chCfg.HoldMax != chDefault.HoldMax {
		t.Errorf("chaos hold_min/max = %s/%s, want %s/%s", chCfg.HoldMin, chCfg.HoldMax, chDefault.HoldMin, chDefault.HoldMax)
	}
	if chCfg.GapMin != chDefault.GapMin || chCfg.GapMax != chDefault.GapMax {
		t.Errorf("chaos gap_min/max = %s/%s, want %s/%s", chCfg.GapMin, chCfg.GapMax, chDefault.GapMin, chDefault.GapMax)
	}
	if chCfg.WaitTimeout != chDefault.WaitTimeout || chCfg.RecoverTimeout != chDefault.RecoverTimeout {
		t.Errorf("chaos wait/recover timeout = %s/%s, want %s/%s", chCfg.WaitTimeout, chCfg.RecoverTimeout, chDefault.WaitTimeout, chDefault.RecoverTimeout)
	}

	// --- stress suite (load_sustain + combined) ---
	if got := cfg.GetDuration("stress.duration", -1); got != 60*time.Second {
		t.Errorf("stress.duration = %s, want 60s", got)
	}
	if cfg.GetBool("stress.combined.enabled", true) {
		t.Errorf("stress.combined.enabled = true, want false (opt-in)")
	}
	stressInts := map[string]int{
		"stress.combined.rf":                   3,
		"stress.combined.sample_tables":        2,
		"stress.combined.sample_value_columns": 10,
		"stress.combined.sample_threads_read":  1,
		"stress.combined.sample_threads_write": 2,
		"stress.combined.benchbase_warehouses": 1,
		"stress.combined.benchbase_terminals":  2,
		"stress.combined.verify_tables":        4,
		"stress.combined.verify_rows":          500,
	}
	for key, want := range stressInts {
		if got := cfg.GetInt(key, -1); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	stressStrs := map[string]string{
		"stress.combined.client_image": "eclipse-temurin:21-jdk",
		"stress.combined.remote_dir":   "/opt/ampqa",
		"stress.combined.jar_secret":   "gh_package",
	}
	for key, want := range stressStrs {
		if got := cfg.GetString(key, "__MISSING__"); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// client_pull_secret is documented-empty but must be PRESENT.
	if got := cfg.GetString("stress.combined.client_pull_secret", "__MISSING__"); got != "" {
		t.Errorf("stress.combined.client_pull_secret = %q, want present and empty", got)
	}
	stressDurs := map[string]time.Duration{
		"stress.combined.run_time":       3 * time.Minute,
		"stress.combined.ready_timeout":  5 * time.Minute,
		"stress.combined.verify_timeout": 5 * time.Minute,
		"stress.combined.health_timeout": 5 * time.Minute,
	}
	for key, want := range stressDurs {
		if got := cfg.GetDuration(key, -1); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	if actions := cfg.GetStringSlice("stress.combined.chaos_actions", nil); strings.Join(actions, ",") != "pod_delete,process_kill,process_restart" {
		t.Errorf("stress.combined.chaos_actions = %v, want [pod_delete process_kill process_restart]", actions)
	}

	// --- perf suite (perf/tpcc) ---
	if cfg.GetBool("perf.enabled", true) {
		t.Errorf("perf.enabled = true, want false (opt-in)")
	}
	if cfg.GetBool("perf.update_baseline", true) {
		t.Errorf("perf.update_baseline = true, want false")
	}
	perfInts := map[string]int{
		"perf.rf":         perf.DefaultRF,
		"perf.warehouses": perf.DefaultWarehouses,
		"perf.terminals":  perf.DefaultTerminals,
	}
	for key, want := range perfInts {
		if got := cfg.GetInt(key, -1); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	perfStrs := map[string]string{
		"perf.client_image":  perf.DefaultClientImage,
		"perf.remote_dir":    perf.DefaultRemoteDir,
		"perf.max_heap":      perf.DefaultMaxHeap,
		"perf.baseline_path": perf.DefaultBaselinePath,
	}
	for key, want := range perfStrs {
		if got := cfg.GetString(key, "__MISSING__"); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// client_pull_secret is documented-empty but must be PRESENT.
	if got := cfg.GetString("perf.client_pull_secret", "__MISSING__"); got != "" {
		t.Errorf("perf.client_pull_secret = %q, want present and empty", got)
	}
	perfFloats := map[string]float64{
		"perf.throughput_drop_pct": perf.DefaultThroughputDropPct,
		"perf.latency_rise_pct":    perf.DefaultLatencyRisePct,
	}
	for key, want := range perfFloats {
		if got := cfg.GetFloat(key, -1); got != want {
			t.Errorf("%s = %g, want %g", key, got, want)
		}
	}
	perfDurs := map[string]time.Duration{
		"perf.ready_timeout":  perf.DefaultReadyTimeout,
		"perf.health_timeout": perf.DefaultHealthTimeout,
	}
	for key, want := range perfDurs {
		if got := cfg.GetDuration(key, -1); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}

	// --- perf.bench.* (the multi-iteration benchmark harness) ---
	if got := cfg.GetInt("perf.bench.iterations", -1); got != perf.DefaultBenchIterations {
		t.Errorf("perf.bench.iterations = %d, want %d", got, perf.DefaultBenchIterations)
	}
	if got := cfg.GetInt("perf.bench.min_iterations", -1); got != perf.DefaultBenchMinIterations {
		t.Errorf("perf.bench.min_iterations = %d, want %d", got, perf.DefaultBenchMinIterations)
	}
	if got := cfg.GetBool("perf.bench.discard_first", false); got != perf.DefaultBenchDiscardFirst {
		t.Errorf("perf.bench.discard_first = %v, want %v", got, perf.DefaultBenchDiscardFirst)
	}
	benchDurs := map[string]time.Duration{
		"perf.bench.warmup":               perf.DefaultBenchWarmup,
		"perf.bench.measurement_duration": perf.DefaultBenchMeasurementDuration,
		"perf.bench.cooldown":             perf.DefaultBenchCooldown,
		"perf.bench.sample_interval":      perf.DefaultBenchSampleInterval,
	}
	for key, want := range benchDurs {
		if got := cfg.GetDuration(key, -1); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}

	// --- perf/pgbench sub-tier (perf.pgbench.*) ---
	pgbenchInts := map[string]int{
		"perf.pgbench.scale":   perf.DefaultPgbenchScale,
		"perf.pgbench.clients": perf.DefaultPgbenchClients,
		"perf.pgbench.jobs":    perf.DefaultPgbenchJobs,
	}
	for key, want := range pgbenchInts {
		if got := cfg.GetInt(key, -1); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	if got := cfg.GetDuration("perf.pgbench.duration", -1); got != perf.DefaultPgbenchDuration {
		t.Errorf("perf.pgbench.duration = %s, want %s", got, perf.DefaultPgbenchDuration)
	}

	// --- perf/sysbench sub-tier (perf.sysbench.*) ---
	if got := cfg.GetString("perf.sysbench.script", "__MISSING__"); got != perf.DefaultSysbenchScript {
		t.Errorf("perf.sysbench.script = %q, want %q", got, perf.DefaultSysbenchScript)
	}
	sysbenchInts := map[string]int{
		"perf.sysbench.tables":     perf.DefaultSysbenchTables,
		"perf.sysbench.table_size": perf.DefaultSysbenchTableSize,
		"perf.sysbench.threads":    perf.DefaultSysbenchThreads,
	}
	for key, want := range sysbenchInts {
		if got := cfg.GetInt(key, -1); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}

	// --- perf.history.* (the historical run-tracking store + trend chart) ---
	if got := cfg.GetBool("perf.history.enabled", false); got != perf.DefaultHistoryEnabled {
		t.Errorf("perf.history.enabled = %v, want %v", got, perf.DefaultHistoryEnabled)
	}
	if got := cfg.GetString("perf.history.path", "__MISSING__"); got != perf.DefaultHistoryPath {
		t.Errorf("perf.history.path = %q, want %q", got, perf.DefaultHistoryPath)
	}
	if got := cfg.GetInt("perf.history.max_runs", -1); got != perf.DefaultHistoryMaxRuns {
		t.Errorf("perf.history.max_runs = %d, want %d", got, perf.DefaultHistoryMaxRuns)
	}

	// --- perf.compare.* (the A/B variant comparison) ---
	if got := cfg.GetBool("perf.compare.fail_on_regression", true); got != false {
		t.Errorf("perf.compare.fail_on_regression = %v, want false (report, don't gate)", got)
	}
	if got := cfg.GetMapSlice("perf.compare.variants", []map[string]any{{"sentinel": "x"}}); len(got) != 0 {
		t.Errorf("perf.compare.variants = %v, want empty by default (single-run path)", got)
	}
	if got := cfg.GetString("perf.compare.mode", "__MISSING__"); got != perf.CompareModeAll {
		t.Errorf("perf.compare.mode = %q, want %q (N-way comparison by default)", got, perf.CompareModeAll)
	}
	if got := cfg.GetString("perf.compare.bisect.metric", "__MISSING__"); got != perf.DefaultBisectMetric {
		t.Errorf("perf.compare.bisect.metric = %q, want %q", got, perf.DefaultBisectMetric)
	}
	if got := cfg.GetFloat("perf.compare.bisect.threshold_pct", -1); got != perf.DefaultBisectThresholdPct {
		t.Errorf("perf.compare.bisect.threshold_pct = %v, want %v", got, perf.DefaultBisectThresholdPct)
	}

	// --- artifacts.pgbench: the YB release tarball delivered to the perf client
	// pod for ysql_bench (YB's pgbench fork). Resolves centos-x86_64 off
	// versions.yugabytedb; its version is the tarball's real extracted directory.
	pbSpec, ok := specs["pgbench"]
	if !ok {
		t.Fatalf("artifacts.pgbench not resolved: %v", specs)
	}
	if want := "yugabyte-" + wantBase; pbSpec.Version != want {
		t.Errorf("artifacts.pgbench version = %q, want %q (the tarball's real extracted directory)", pbSpec.Version, want)
	}
	if want := "yugabyte-" + wantVersion + "-centos-x86_64.tar.gz"; pbSpec.GlobPath != want {
		t.Errorf("artifacts.pgbench glob_path = %q, want %q", pbSpec.GlobPath, want)
	}
	if want := "s3://releases.yugabyte.com/" + wantVersion + "/yugabyte-" + wantVersion + "-centos-x86_64.tar.gz"; pbSpec.URL != want {
		t.Errorf("artifacts.pgbench url = %q, want %q", pbSpec.URL, want)
	}
	if pbSpec.Type != artifact.TypeS3 {
		t.Errorf("artifacts.pgbench type = %q, want s3", pbSpec.Type)
	}
	if !pbSpec.RemoteUnzip {
		t.Errorf("artifacts.pgbench remote_unzip = false, want true")
	}
	if pbSpec.Target != "client" {
		t.Errorf("artifacts.pgbench target = %q, want client", pbSpec.Target)
	}
}

// TestOwnerOverrideFlowsToCloudResources proves `-o owner=<name>` (a top-level
// config override) reaches every place a cloud resource is ownership-tagged:
// the aws yb_owner tag + instance name prefix, the gcp yb_owner label +
// instance name prefix, and the yba universe instanceTags yb_owner. This is
// the knob that replaces the old hardcoded per-person username.
func TestOwnerOverrideFlowsToCloudResources(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{"owner=alice"})
	if err != nil {
		t.Fatalf("load with owner override: %v", err)
	}

	awsCfg := awscloud.FromConfig(cfg)
	if awsCfg.Owner != "alice" {
		t.Errorf("aws Owner = %q, want alice", awsCfg.Owner)
	}
	if awsCfg.NamePrefix != "alice" {
		t.Errorf("aws NamePrefix = %q, want alice (owner-derived)", awsCfg.NamePrefix)
	}

	gcpCfg := gcpcloud.FromConfig(cfg)
	if gcpCfg.Owner != "alice" {
		t.Errorf("gcp Owner = %q, want alice", gcpCfg.Owner)
	}
	if gcpCfg.NamePrefix != "alice" {
		t.Errorf("gcp NamePrefix = %q, want alice (owner-derived)", gcpCfg.NamePrefix)
	}

	uspec := yba.BuildUniverseSpec(cfg)
	if uspec.InstanceTags["yb_owner"] != "alice" {
		t.Errorf("yba universe InstanceTags[yb_owner] = %q, want alice", uspec.InstanceTags["yb_owner"])
	}

	// apps.cloud.aws.name_prefix / apps.cloud.gcp.name_prefix, when explicitly
	// set, still win over the owner-derived default.
	cfgWithPrefix, err := core.LoadConfig(nil, nil, []string{
		"owner=alice",
		"apps.cloud.aws.name_prefix=custom-prefix",
		"apps.cloud.gcp.name_prefix=custom-prefix",
	})
	if err != nil {
		t.Fatalf("load with owner + name_prefix overrides: %v", err)
	}
	if got := awscloud.FromConfig(cfgWithPrefix).NamePrefix; got != "custom-prefix" {
		t.Errorf("aws NamePrefix override = %q, want custom-prefix", got)
	}
	if got := gcpcloud.FromConfig(cfgWithPrefix).NamePrefix; got != "custom-prefix" {
		t.Errorf("gcp NamePrefix override = %q, want custom-prefix", got)
	}
}

func writeYAML(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// TestEmbeddedDefaultsLoadWithNoUserConfig confirms LoadConfig(nil, nil, nil)
// works standalone and returns the embedded values (a focused smoke test; the
// full registry is TestEmbeddedDefaultsSupplyEveryKnownKey above).
func TestEmbeddedDefaultsLoadWithNoUserConfig(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.GetInt("sched.capacity", -1); got != sched.DefaultCapacity {
		t.Errorf("sched.capacity = %d, want %d", got, sched.DefaultCapacity)
	}
}

// TestUserConfigOverridesOneKeyInheritsRest: a user -c file overriding a
// single nested key must leave every sibling key at its embedded default.
func TestUserConfigOverridesOneKeyInheritsRest(t *testing.T) {
	dir := t.TempDir()
	user := writeYAML(t, dir, "user.yaml", "sched:\n  capacity: 3\n")

	cfg, err := core.LoadConfig([]string{user}, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.GetInt("sched.capacity", -1); got != 3 {
		t.Errorf("sched.capacity = %d, want 3 (user override)", got)
	}
	if got := cfg.GetInt("sched.per_env_parallelism", -1); got != sched.DefaultPerEnvParallelism {
		t.Errorf("sched.per_env_parallelism = %d, want %d (inherited from embedded defaults)", got, sched.DefaultPerEnvParallelism)
	}
	if got := cfg.GetInt("sched.weights.dedicated_stack", -1); got != 2 {
		t.Errorf("sched.weights.dedicated_stack = %d, want 2 (inherited)", got)
	}
	if got := cfg.GetDuration("timeouts.stress", -1); got != core.DefaultStressTimeout {
		t.Errorf("timeouts.stress = %s, want %s (inherited)", got, core.DefaultStressTimeout)
	}
}

// TestOverrideBeatsUserFileBeatsEmbedded exercises the full precedence chain
// on one key: embedded (8) → -c file (3) → -o override (1), and -o must win.
func TestOverrideBeatsUserFileBeatsEmbedded(t *testing.T) {
	dir := t.TempDir()
	user := writeYAML(t, dir, "user.yaml", "sched:\n  capacity: 3\n")

	embeddedOnly, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load embedded-only: %v", err)
	}
	if got := embeddedOnly.GetInt("sched.capacity", -1); got != 8 {
		t.Fatalf("precondition: embedded sched.capacity = %d, want 8", got)
	}

	fileOnly, err := core.LoadConfig([]string{user}, nil, nil)
	if err != nil {
		t.Fatalf("load with user file: %v", err)
	}
	if got := fileOnly.GetInt("sched.capacity", -1); got != 3 {
		t.Fatalf("user file sched.capacity = %d, want 3", got)
	}

	withOverride, err := core.LoadConfig([]string{user}, nil, []string{"sched.capacity=int:1"})
	if err != nil {
		t.Fatalf("load with override: %v", err)
	}
	if got := withOverride.GetInt("sched.capacity", -1); got != 1 {
		t.Errorf("sched.capacity = %d, want 1 (-o beats -c beats embedded)", got)
	}
}

// TestNoDefaultConfigYieldsEmptyBase is the --no-default-config escape hatch:
// LoadConfigNoDefaults must not see any embedded key, only what a user
// supplies.
func TestNoDefaultConfigYieldsEmptyBase(t *testing.T) {
	cfg, err := core.LoadConfigNoDefaults(nil, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.GetInt("sched.capacity", -1); got != -1 {
		t.Errorf("sched.capacity = %d, want -1 (no embedded layer, key absent)", got)
	}
	if len(cfg.Resolved()) != 0 {
		t.Errorf("Resolved() = %v, want empty map with no defaults and no user config", cfg.Resolved())
	}

	dir := t.TempDir()
	user := writeYAML(t, dir, "user.yaml", "sched:\n  capacity: 3\n")
	withFile, err := core.LoadConfigNoDefaults([]string{user}, nil, nil)
	if err != nil {
		t.Fatalf("load with user file: %v", err)
	}
	if got := withFile.GetInt("sched.capacity", -1); got != 3 {
		t.Errorf("sched.capacity = %d, want 3 (user file still applies, only the embedded layer is skipped)", got)
	}
	// A sibling key with no user-supplied value and no embedded layer must be
	// genuinely absent, not silently backfilled.
	if got := withFile.GetInt("sched.per_env_parallelism", -1); got != -1 {
		t.Errorf("sched.per_env_parallelism = %d, want -1 (embedded layer skipped)", got)
	}
}

// TestDefaultBaseNotCorruptedByOverrides guards the bug this file's helpers
// found during implementation: Config.set mutates nested maps in place, so
// merging the shared package-level embedded base by reference (instead of
// deep-copying it) let one call's -o override permanently leak into every
// later LoadConfig call in the process.
func TestDefaultBaseNotCorruptedByOverrides(t *testing.T) {
	polluted, err := core.LoadConfig(nil, nil, []string{"artifacts.leaky.glob_path=x", "sched.capacity=int:999"})
	if err != nil {
		t.Fatalf("load with overrides: %v", err)
	}
	if got := polluted.GetInt("sched.capacity", -1); got != 999 {
		t.Fatalf("precondition: override didn't apply, got %d", got)
	}

	clean, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load clean: %v", err)
	}
	if got := clean.GetInt("sched.capacity", -1); got != sched.DefaultCapacity {
		t.Errorf("sched.capacity leaked across calls: got %d, want %d", got, sched.DefaultCapacity)
	}
	resolved := clean.Resolved()
	artifacts, _ := resolved["artifacts"].(map[string]any)
	if _, ok := artifacts["leaky"]; ok {
		t.Errorf("artifacts.leaky leaked into a later LoadConfig call's embedded base")
	}
}
