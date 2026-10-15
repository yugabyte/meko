package core_test

// Canonical-defaults verification: the embedded configs/default.yaml is the
// one place every framework default lives. This file asserts two things:
//
//  1. Every config-reading call site in the framework that has a genuine
//     default (as opposed to a required, project-specific value like
//     env.k8s.context) finds that default in the embedded file, not just in
//     its own inline fallback constant. This is the "registry of knobs":
//     extend it whenever a new default.yaml key is added.
//  2. The load precedence, embedded → -c → -jc → -o, and the
//     --no-default-config escape hatch, behaves as documented.
//
// It lives in core_test (not core) so it can import the leaf provider
// packages (sched, the apps) that themselves import
// core, without an import cycle.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	awscloud "github.com/yugabyte/meko/meko/test/functional/framework/apps/cloud/aws"
	gcpcloud "github.com/yugabyte/meko/meko/test/functional/framework/apps/cloud/gcp"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoload"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/metrics"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yba"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/ybastack"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/ybpod"
	"github.com/yugabyte/meko/meko/test/functional/framework/artifact"
	"github.com/yugabyte/meko/meko/test/functional/framework/chaos"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/sched"
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
	if got := cfg.GetInt("sched.cluster_slots.capacity", -1); got != sched.DefaultClusterSlots {
		t.Errorf("sched.cluster_slots.capacity = %d, want %d", got, sched.DefaultClusterSlots)
	}
	if got := cfg.GetInt("sched.per_env_parallelism", -1); got != sched.DefaultPerEnvParallelism {
		t.Errorf("sched.per_env_parallelism = %d, want %d", got, sched.DefaultPerEnvParallelism)
	}
	if got := cfg.GetInt("sched.env_retries", -1); got != core.DefaultEnvRetries {
		t.Errorf("sched.env_retries = %d, want %d", got, core.DefaultEnvRetries)
	}
	if got := cfg.GetDuration("sched.env_retry_backoff", -1); got != core.DefaultEnvRetryBackoff {
		t.Errorf("sched.env_retry_backoff = %s, want %s", got, core.DefaultEnvRetryBackoff)
	}
	weights := map[string]int{
		"single_stack":    1,
		"dedicated_stack": 2,
		"remote":          1,
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
		"timeouts.teardown":     core.DefaultTeardownTimeout,
		"timeouts.collect":      core.DefaultCollectTimeout,

		"timeouts.provision_dedicated": core.DefaultDedicatedProvisionTimeout,
	}
	for key, want := range timeouts {
		if got := cfg.GetDuration(key, -1); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	// timeouts.provision is deliberately NOT supplied by the embedded defaults:
	// unset is what lets ResolveProvisionTimeout scale it with the database
	// size, while a value set anywhere overrides the scaling verbatim.
	if got := cfg.GetDuration("timeouts.provision", -1); got != -1 {
		t.Errorf("timeouts.provision = %s, want unset (the scaled default must apply)", got)
	}
	// The default database is RF3, which adds its nodes' share to the budget.
	want := core.DefaultProvisionTimeout + 2*core.ProvisionPerExtraNode
	if got := core.ResolveProvisionTimeout(cfg); got != want {
		t.Errorf("ResolveProvisionTimeout(defaults) = %s, want %s", got, want)
	}
	if got := core.ResolveProvisionMultiplier(cfg); got != 1 {
		t.Errorf("timeouts.provision_multiplier = %v, want 1 (the shipped default must not scale anything)", got)
	}

	// And through the resolution helpers actually used by lifecycle/scheduler.
	for _, cat := range []core.Category{core.Functional, core.Stress, core.Perf, core.LongRunning} {
		if got, want := core.ResolveCategoryTimeout(cat, cfg), core.CategoryDefaultTimeout(cat); got != want {
			t.Errorf("ResolveCategoryTimeout(%s) = %s, want %s", cat, got, want)
		}
	}
	if got := core.ResolveProvisionTimeout(cfg); got != want {
		t.Errorf("ResolveProvisionTimeout = %s, want %s", got, want)
	}
	if got := core.ResolveTeardownTimeout(cfg); got != core.DefaultTeardownTimeout {
		t.Errorf("ResolveTeardownTimeout = %s, want %s", got, core.DefaultTeardownTimeout)
	}
	if got := core.ResolveCollectTimeout(cfg); got != core.DefaultCollectTimeout {
		t.Errorf("ResolveCollectTimeout = %s, want %s", got, core.DefaultCollectTimeout)
	}

	// --- db: the stack's database ---
	if got := cfg.GetString("db.provider", "__MISSING__"); got != "yugabyted" {
		t.Errorf("db.provider = %q, want yugabyted", got)
	}
	if got := cfg.GetInt("db.rf", 0); got != 3 {
		t.Errorf("db.rf = %d, want 3", got)
	}
	if got := cfg.GetString(core.StackOverrideKey, "__MISSING__"); got != "" {
		t.Errorf("%s = %q, want present and empty (tests keep their declared shape)", core.StackOverrideKey, got)
	}
	if got := core.ResolveProvisionTimeoutFor(cfg, core.EnvSpec{Shape: core.DedicatedStack}); got != core.DefaultDedicatedProvisionTimeout {
		t.Errorf("ResolveProvisionTimeoutFor(dedicated) = %s, want %s", got, core.DefaultDedicatedProvisionTimeout)
	}

	// --- owner: the yb_owner tag/label value + resource name prefix, stamped
	// on every cloud resource across aws/gcp/yba (see instructions above
	// default.yaml's `owner:` key). ---
	if got := cfg.GetString("owner", "__MISSING__"); got != awscloud.DefaultOwner {
		t.Errorf("owner = %q, want %q", got, awscloud.DefaultOwner)
	}
	// name_prefix is declared empty, and empty means owner.
	for _, key := range []string{"apps.cloud.aws.name_prefix", "apps.cloud.gcp.name_prefix"} {
		if got := cfg.GetString(key, "__MISSING__"); got != "" {
			t.Errorf("%s = %q, want \"\" (owner-derived at read time)", key, got)
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
	// master_gflags/tserver_gflags are documented-empty but must be PRESENT, and
	// every provider's builder must carry the backing contract's tserver flags
	// through from the embedded defaults: features/backing_config_contract reads
	// them back off the live universe and fails without them.
	for _, key := range []string{"apps.yba.universe.master_gflags", "apps.yba.universe.tserver_gflags"} {
		if got := cfg.GetMap(key, nil); got == nil {
			t.Errorf("%s missing from the embedded defaults", key)
		} else if len(got) != 0 {
			t.Errorf("%s = %v, want present and empty", key, got)
		}
	}
	for name, tserver := range map[string]map[string]string{
		"aws": uspec.TServerGFlags,
		"gcp": yba.BuildGCPUniverseSpec(cfg).TServerGFlags,
		"k8s": yba.BuildK8sUniverseSpec(cfg).TServerGFlags,
	} {
		for flag, want := range yba.DefaultTServerGFlags {
			if tserver[flag] != want {
				t.Errorf("%s universe TServerGFlags[%s] = %q, want %q", name, flag, tserver[flag], want)
			}
		}
	}
	// The VM builders stay YBC-off: there WaitForYbcServer pings a yb-controller
	// nothing installs, and the create task fails with the nodes already up.
	if uspec.EnableYbc {
		t.Errorf("yba universe EnableYbc = true, want false")
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
	// provider_type defaults to kubernetes: a YBA-provisioned RF=3 universe
	// co-located in the stack's cluster.
	if got := cfg.GetString("apps.yba.provider_type", "__MISSING__"); got != "kubernetes" {
		t.Errorf("apps.yba.provider_type = %q, want kubernetes", got)
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
	// stress/backing_backup_restore skips without YBC, and the universe is
	// created once per run, so the kubernetes default has to carry it, the
	// chart brings yb-controller, so WaitForYbcServer has something to ping.
	if !k8sUni.EnableYbc {
		t.Errorf("k8s universe EnableYbc = false, want true")
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
	if gcpUni.EnableYbc {
		t.Errorf("gcp universe EnableYbc = true, want false")
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
		"apps.yba.gcp.keypair_name":       yba.DefaultGCPKeyPairName,
		"apps.yba.gcp.keypair_file":       yba.DefaultGCPKeyPairFile,
	}
	for key, want := range ybaGCPKeys {
		if got := cfg.GetString(key, "__MISSING__"); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// image is documented-empty but must be PRESENT.
	for _, key := range []string{"apps.yba.gcp.image"} {
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
	if k8sCfg.TLS != ybk8s.DefaultTLSEnabled {
		t.Errorf("ybk8s TLS = %v, want %v", k8sCfg.TLS, ybk8s.DefaultTLSEnabled)
	}
	if k8sCfg.TLSPermissive != ybk8s.DefaultTLSPermissive {
		t.Errorf("ybk8s TLSPermissive = %v, want %v", k8sCfg.TLSPermissive, ybk8s.DefaultTLSPermissive)
	}
	if k8sCfg.TLSIssuerKind != ybk8s.DefaultTLSIssuerKind || k8sCfg.TLSIssuerName != ybk8s.DefaultTLSIssuerName {
		t.Errorf("ybk8s TLS issuer = %q/%q, want %q/%q", k8sCfg.TLSIssuerKind, k8sCfg.TLSIssuerName, ybk8s.DefaultTLSIssuerKind, ybk8s.DefaultTLSIssuerName)
	}
	if k8sCfg.TLSIssuerGroup != ybk8s.DefaultTLSIssuerGroup {
		t.Errorf("ybk8s TLSIssuerGroup = %q, want %q", k8sCfg.TLSIssuerGroup, ybk8s.DefaultTLSIssuerGroup)
	}
	if k8sCfg.TLSStorageRole != ybk8s.DefaultTLSStorageRole {
		t.Errorf("ybk8s TLSStorageRole = %q, want %q", k8sCfg.TLSStorageRole, ybk8s.DefaultTLSStorageRole)
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
		"apps.ybpod.base_image_pull_secret", "apps.ybpod.tarball_url", "apps.ybpod.tarball_secret",
	} {
		if got := cfg.GetString(key, "__MISSING__"); got != "" {
			t.Errorf("%s = %q, want present and empty", key, got)
		}
	}

	// apps.yugabyte.* / apps.yugabyted.* are deliberately NOT documented here:
	// no code reads them from config. They are code constants, not framework
	// knobs.

	// --- artifact / report: documented-empty keys must still be *present*
	// (lookup succeeds with ""), distinguishing "off by design" from "key
	// missing entirely", GetString only returns the sentinel on a lookup
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
	// itself owns exactly one version, yugabytedb, and no artifacts.
	resolved := cfg.Resolved()
	if _, ok := resolved["versions"]; !ok {
		t.Errorf("versions{} section missing from embedded defaults")
	}
	if _, ok := resolved["artifacts"]; !ok {
		t.Errorf("artifacts{} section missing from embedded defaults")
	}

	specs, err := artifact.Resolve(cfg)
	if err != nil {
		t.Fatalf("artifact.Resolve: %v", err)
	}
	if len(specs) != 0 {
		t.Errorf("no artifacts are declared by default, got %v", specs)
	}
	wantVersion := cfg.GetString("versions.yugabytedb", "")
	if wantVersion == "" {
		t.Fatalf("versions.yugabytedb missing from embedded defaults")
	}
	if base := core.BaseVersion(wantVersion); base == "" || base == wantVersion {
		t.Fatalf("versions.yugabytedb %q must carry a -bNNN build suffix", wantVersion)
	}

	// --- apps.metrics (background server-metrics sampler) ---
	mcfg := metrics.FromConfig(cfg)
	if !mcfg.Enabled {
		t.Errorf("metrics Enabled = false, want true (collection defaults on)")
	}
	if mcfg.SampleInterval != metrics.DefaultSampleInterval {
		t.Errorf("metrics SampleInterval = %s, want %s", mcfg.SampleInterval, metrics.DefaultSampleInterval)
	}
	if mcfg.MaxPoints != metrics.DefaultMaxPoints {
		t.Errorf("metrics MaxPoints = %d, want %d", mcfg.MaxPoints, metrics.DefaultMaxPoints)
	}
	if mcfg.TServerPort != metrics.DefaultTServerPort || mcfg.MasterPort != metrics.DefaultMasterPort {
		t.Errorf("metrics tserver/master port = %d/%d, want %d/%d",
			mcfg.TServerPort, mcfg.MasterPort, metrics.DefaultTServerPort, metrics.DefaultMasterPort)
	}
	// The collect lists are the whole point of the app being config-driven, and
	// they carry the display titles: the embedded default must BE the code
	// default, field for field, not merely non-empty.
	if !slices.Equal(mcfg.Collect, metrics.DefaultCollect) {
		t.Errorf("metrics Collect = %+v, want %+v", mcfg.Collect, metrics.DefaultCollect)
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
	if chCfg.StallPattern != chDefault.StallPattern {
		t.Errorf("chaos stall.pattern = %q, want %q", chCfg.StallPattern, chDefault.StallPattern)
	}
	if chCfg.FreezeHelperImage != chDefault.FreezeHelperImage {
		t.Errorf("chaos freeze.helper_image = %q, want %q", chCfg.FreezeHelperImage, chDefault.FreezeHelperImage)
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

// Every PVC-creating app defaults to a WaitForFirstConsumer class; an Immediate
// one (GKE's `standard`) zone-pins the pod before it is scheduled.
func TestPVCStorageClassDefaultsAreWaitForFirstConsumer(t *testing.T) {
	const wfc = "standard-rwo"
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	classes := map[string]string{
		"apps.ybastack.storage_class": ybastack.DefaultStorageClass,
		"apps.ybk8s.storage_class":    ybk8s.DefaultStorageClass,
		"apps.yba.k8s.storage_class":  yba.DefaultK8sStorageClass,
	}
	for key, constant := range classes {
		if constant != wfc {
			t.Errorf("%s code default = %q, want %q (Immediate binding zone-pins the pod)", key, constant, wfc)
		}
		if got := cfg.GetString(key, "__MISSING__"); got != constant {
			t.Errorf("%s = %q, want %q", key, got, constant)
		}
	}
}

// apps.mekoload keeps an inline fallback beside each key, so the two have to say the same.
func TestMekoloadDefaultsMatchTheCode(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	fromYAML, err := mekoload.FromConfig(cfg, "")
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	fromCode, err := mekoload.FromConfig(core.NewConfig(), "")
	if err != nil {
		t.Fatalf("FromConfig(empty): %v", err)
	}
	if fromYAML != fromCode {
		t.Errorf("default.yaml and the code disagree:\n yaml %+v\n code %+v", fromYAML, fromCode)
	}
	if fromYAML.Binary != "" || fromYAML.Source != "" {
		t.Errorf("a binary or a source is set by default: %+v", fromYAML)
	}
}

// The meko stack's defaults live in default.yaml and nowhere else: the code reads
// them with no inline fallback, so a key missing there is an empty value here.
func TestMekostackDefaults(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ms, err := mekostack.FromConfig(cfg)
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if ms.Release != "meko" || strings.Join(ms.ValuesFiles, ",") != "values-gcp.yaml,values-dev.yaml,values-byoc.yaml,values-byoc-bundled-db.yaml" {
		t.Errorf("release/values_files = %q / %v", ms.Release, ms.ValuesFiles)
	}
	if !mekostack.ValidTag(ms.ImageVersion) {
		t.Errorf("env.mekostack.image_version = %q, want a tag", ms.ImageVersion)
	}
	if _, tag, err := mekostack.SplitRef(ms.DatabaseImage); err != nil || !mekostack.ValidTag(tag) || !strings.HasPrefix(ms.DatabaseImage, mekostack.QuayNamespace+"/") {
		t.Errorf("env.mekostack.images.database = %q (tag %q, %v), want a quay.io/yugabyte reference", ms.DatabaseImage, tag, err)
	}
	for _, c := range mekostack.Components {
		if got := cfg.GetString("env.mekostack.images."+c.Key+".tag", "__MISSING__"); got != "" {
			t.Errorf("env.mekostack.images.%s.tag = %q, want present and empty (image_version applies)", c.Key, got)
		}
		if ms.Tag(c.Key) != ms.ImageVersion {
			t.Errorf("%s tag = %q, want image_version %q", c.Key, ms.Tag(c.Key), ms.ImageVersion)
		}
	}
	if ms.APIReplicas != 3 || !ms.RagWorkers || ms.OpenAIKey == "" || ms.S3Bucket == "" || ms.S3Region == "" {
		t.Errorf("replicas/rag/openai/s3 = %d %v %q %q %q", ms.APIReplicas, ms.RagWorkers, ms.OpenAIKey, ms.S3Bucket, ms.S3Region)
	}
	if ms.ProvisionTimeout != 40*time.Minute || ms.ReadyTimeout <= 0 || ms.RolloutTimeout <= 0 {
		t.Errorf("timeouts provision/ready/rollout = %s/%s/%s", ms.ProvisionTimeout, ms.ReadyTimeout, ms.RolloutTimeout)
	}
	if got := cfg.GetDuration("env.mekostack.timeouts.deploy", 0); got < ms.ProvisionTimeout+ms.ReadyTimeout {
		t.Errorf("env.mekostack.timeouts.deploy = %s, want room for provision plus ready", got)
	}
	if ms.Reach != mekostack.ReachTailnet || ms.TTL != 12*time.Hour {
		t.Errorf("reach/ttl = %s/%s", ms.Reach, ms.TTL)
	}
	if ms.HashPartitions != 8 {
		t.Errorf("env.mekostack.hash_partitions = %d, want 8", ms.HashPartitions)
	}
	if ms.Auth != mekostack.AuthCognitoMock {
		t.Errorf("env.mekostack.auth = %q, want %q", ms.Auth, mekostack.AuthCognitoMock)
	}
	if ms.CognitoMock.Region == "" || ms.CognitoMock.TokenTTL != 24*time.Hour {
		t.Errorf("env.mekostack.cognito_mock = %+v, want a region and a day of token life", ms.CognitoMock)
	}
	if ms.Mocks.Image != "mirror.gcr.io/library/alpine:3.20" || ms.Mocks.Arch != "amd64" || ms.Mocks.Binary != "" {
		t.Errorf("env.mekostack.mocks = %+v, want mirror.gcr.io/library/alpine:3.20, amd64 and no prebuilt binary", ms.Mocks)
	}
	if got := cfg.GetString("env.mekostack.quay_secret", "__MISSING__"); got != "quay" {
		t.Errorf("env.mekostack.quay_secret = %q, want quay", got)
	}
	for _, key := range []string{
		"env.mekostack.quay_user", "env.mekostack.quay_token", "env.mekostack.s3.public_endpoint",
		"env.mekostack.values_dir", "env.mekostack.diag_dir",
		"env.k8s.context", "env.k8s.namespace", "env.k8s.remote_url", "env.k8s.remote_mcp_url",
	} {
		if got := cfg.GetString(key, "__MISSING__"); got != "" {
			t.Errorf("%s = %q, want present and empty", key, got)
		}
	}
	if !filepath.IsAbs(ms.ChartPath) {
		t.Errorf("chart path %q was not resolved to an absolute path", ms.ChartPath)
	}
}
