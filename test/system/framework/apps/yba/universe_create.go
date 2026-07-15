package yba

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Universe creation from a typed spec, via YBA's two-call configure-then-create
// flow: POST /universe_configure with a minimal clusters[].userIntent and
// clusterOperation:CREATE — YBA fills in nodeDetailsSet, placementInfo, ports,
// and other derived fields — then POST the FILLED config to /universes to plan
// the async create. This keeps the framework supplying only what it decides
// (num nodes, RF, instance type, region, version, volume) instead of
// hand-building YBA's entire internal universe document.

// UniverseSpec is a universe-create request in typed form. RegionUUIDs are
// provider region UUIDs (resolve via Client.RegionUUIDsForCodes); the rest come
// from apps.yba.universe.* (BuildUniverseSpec).
type UniverseSpec struct {
	NamePrefix string // "" → DefaultUniverseNamePrefix
	RunTag     string // stamped into the universe name so it is identifiable/reapable

	ProviderUUID  string
	ProviderCode  string   // "aws"
	AccessKeyCode string   // provider's SSH access-key code → userIntent.accessKeyCode
	Region        string   // region code (used to resolve RegionUUIDs)
	RegionUUIDs   []string // provider region UUIDs → userIntent.regionList

	InstanceType      string
	NumCores          int // vCPU of InstanceType, registered on the provider
	MemSizeGB         int // RAM (GiB) of InstanceType, registered on the provider
	NumNodes          int
	ReplicationFactor int
	YBSoftwareVersion string
	ReleaseURL        string // tarball URL to import the DB release from if absent
	VolumeSizeGB      int
	VolumeCount       int
	StorageType       string            // "" → DefaultStorageType (GP3)
	DiskIops          int               // GP3/IO-class only; YBA requires it for GP3
	Throughput        int               // GP3 only (MB/s)
	InstanceTags      map[string]string // userIntent.instanceTags on every node
	EnableYbc         bool              // ybc.provider.enabled — off skips WaitForYbcServer

	// Kubernetes-only (ProviderCode "kubernetes"): the PVC storage class and the
	// master/tserver K8SNodeResourceSpec (cpu cores + memory GiB). The AWS path
	// ignores these; the k8s path ignores InstanceType/AccessKeyCode/AMI/keypair
	// and the AWS storage fields (storageType/diskIops/throughput).
	K8sStorageClass string
	MasterCPUCores  int
	MasterMemGiB    int
	TServerCPUCores int
	TServerMemGiB   int
}

// DefaultOwner is the yb_owner tag value when neither the top-level `owner`
// config key nor an apps.yba.universe.instance_tags override is set. A neutral
// default so the framework never bakes in a specific person's username;
// override per run with -o owner=<name>.
const DefaultOwner = "ampqa"

// DefaultInstanceTags are the mandatory ownership tags every universe node
// carries besides yb_owner (sourced from the `owner` config key — see
// instanceTagsFromConfig) — mirrors apps/cloud/aws mandatoryTags (yb_task/
// yb_dept/autoclean/ampqa). The dev-account governance SCP conditions
// RunInstances on these, so a universe missing them is denied at launch.
// Overridable/extendable via apps.yba.universe.instance_tags; the per-run
// ampqa-run tag is added at build time.
var DefaultInstanceTags = map[string]string{
	"yb_task":   "stress-dev",
	"yb_dept":   "qa",
	"autoclean": "true",
	"ampqa":     "1",
}

// instanceTagsFromConfig merges apps.yba.universe.instance_tags over yb_owner
// (from the top-level `owner` config key) plus the mandatory
// DefaultInstanceTags (config wins on key collisions).
func instanceTagsFromConfig(c ConfigGetter) map[string]string {
	tags := make(map[string]string, len(DefaultInstanceTags)+1)
	tags["yb_owner"] = c.GetString("owner", DefaultOwner)
	for k, v := range DefaultInstanceTags {
		tags[k] = v
	}
	for k, v := range c.GetMap("apps.yba.universe.instance_tags", nil) {
		if s, ok := v.(string); ok {
			tags[k] = s
		}
	}
	return tags
}

// BuildUniverseSpec reads apps.yba.universe.* into a UniverseSpec. The software
// version falls back from apps.yba.universe.yb_version to versions.yugabytedb.
// The provider UUID, provider code, and region UUIDs are NOT read here — the
// caller resolves them against the live provider and fills them in.
func BuildUniverseSpec(c ConfigGetter) UniverseSpec {
	version := c.GetString("apps.yba.universe.yb_version", "")
	if version == "" {
		version = c.GetString("versions.yugabytedb", "")
	}
	return UniverseSpec{
		NamePrefix:        c.GetString("apps.yba.universe.name_prefix", DefaultUniverseNamePrefix),
		Region:            c.GetString("apps.yba.universe.region", DefaultUniverseRegion),
		InstanceType:      c.GetString("apps.yba.universe.instance_type", DefaultInstanceType),
		NumCores:          c.GetInt("apps.yba.universe.instance_num_cores", DefaultInstanceNumCores),
		MemSizeGB:         c.GetInt("apps.yba.universe.instance_mem_gb", DefaultInstanceMemGB),
		NumNodes:          c.GetInt("apps.yba.universe.num_nodes", DefaultNumNodes),
		ReplicationFactor: c.GetInt("apps.yba.universe.replication_factor", DefaultReplicationFactor),
		YBSoftwareVersion: version,
		ReleaseURL:        releaseURLFromConfig(c),
		VolumeSizeGB:      c.GetInt("apps.yba.universe.volume_size_gb", DefaultVolumeSizeGB),
		VolumeCount:       c.GetInt("apps.yba.universe.volume_count", DefaultVolumeCount),
		StorageType:       c.GetString("apps.yba.universe.storage_type", DefaultStorageType),
		DiskIops:          c.GetInt("apps.yba.universe.disk_iops", DefaultDiskIops),
		Throughput:        c.GetInt("apps.yba.universe.throughput", DefaultThroughput),
		InstanceTags:      instanceTagsFromConfig(c),
		EnableYbc:         c.GetBool("apps.yba.universe.enable_ybc", false),
	}
}

// UniverseName is the tagged universe name: "<prefix>-<runTag>" (prefix
// defaults to "ampqa"), so a created universe is always identifiable and
// reapable by its ampqa prefix.
func (s UniverseSpec) UniverseName() string {
	prefix := s.NamePrefix
	if prefix == "" {
		prefix = DefaultUniverseNamePrefix
	}
	if s.RunTag != "" {
		return prefix + "-" + s.RunTag
	}
	return prefix
}

// BuildConfigureRequest builds the create request body: a single PRIMARY
// cluster carrying the userIntent the framework decides (YBA fills the rest).
// It dispatches on ProviderCode — a kubernetes universe uses k8s resource specs
// + a storage class and NO instanceType/keypair, a gcp universe uses a GCE
// machine type + persistent-disk device info, an AWS universe uses the instance
// type + EBS device info. Pure and side-effect-free, so it is unit-tested
// directly.
func BuildConfigureRequest(s UniverseSpec) map[string]any {
	var userIntent map[string]any
	switch s.ProviderCode {
	case "kubernetes":
		userIntent = k8sUserIntent(s)
	case "gcp":
		userIntent = gcpUserIntent(s)
	default:
		userIntent = awsUserIntent(s)
	}
	return map[string]any{
		"clusters": []any{
			map[string]any{
				"clusterType": "PRIMARY",
				"userIntent":  userIntent,
			},
		},
		"currentClusterType": "PRIMARY",
		"clusterOperation":   "CREATE",
	}
}

// awsUserIntent builds the userIntent for an AWS (or other VM cloud) universe:
// an instance type, EBS deviceInfo (storage type + GP3/IO iops/throughput), the
// provider SSH access key, and the ownership instance tags.
func awsUserIntent(s UniverseSpec) map[string]any {
	volumes := s.VolumeCount
	if volumes <= 0 {
		volumes = DefaultVolumeCount
	}
	storageType := s.StorageType
	if storageType == "" {
		storageType = DefaultStorageType
	}
	deviceInfo := map[string]any{
		"volumeSize":  s.VolumeSizeGB,
		"numVolumes":  volumes,
		"storageType": storageType,
	}
	// GP3 (and the provisioned-IOPS IO classes) require an explicit diskIops;
	// GP3 also takes a throughput. YBA 400s on GP3 without them. Other storage
	// types (e.g. the gp2 baseline) must NOT carry these fields.
	switch storageType {
	case "GP3":
		deviceInfo["diskIops"] = s.DiskIops
		deviceInfo["throughput"] = s.Throughput
	case "IO1", "IO2":
		deviceInfo["diskIops"] = s.DiskIops
	}
	regionList := make([]any, 0, len(s.RegionUUIDs))
	for _, u := range s.RegionUUIDs {
		regionList = append(regionList, u)
	}
	userIntent := map[string]any{
		"universeName":      s.UniverseName(),
		"provider":          s.ProviderUUID,
		"providerType":      s.ProviderCode,
		"regionList":        regionList,
		"instanceType":      s.InstanceType,
		"numNodes":          s.NumNodes,
		"replicationFactor": s.ReplicationFactor,
		"ybSoftwareVersion": s.YBSoftwareVersion,
		"assignPublicIP":    true,
		"useTimeSync":       true,
		"enableYSQL":        true,
		"enableYCQL":        true,
		"deviceInfo":        deviceInfo,
	}
	// accessKeyCode ties the universe to the provider's SSH keypair; without it
	// YBA launches nodes with KeyName=None and the cloud SDK rejects the call.
	if s.AccessKeyCode != "" {
		userIntent["accessKeyCode"] = s.AccessKeyCode
	}
	// instanceTags are stamped onto every launched node (and its RunInstances
	// TagSpecifications). The governance SCP conditions ec2:RunInstances on the
	// ownership tags, so they are mandatory. YBA's request binder accepts the map
	// form. The per-run ampqa-run tag makes the nodes reapable by run.
	if len(s.InstanceTags) > 0 || s.RunTag != "" {
		tags := make(map[string]any, len(s.InstanceTags)+1)
		for k, v := range s.InstanceTags {
			tags[k] = v
		}
		if s.RunTag != "" {
			tags["ampqa-run"] = s.RunTag
		}
		userIntent["instanceTags"] = tags
	}
	return userIntent
}

// gcpUserIntent builds the userIntent for a gcp universe: providerType gcp, a
// GCE machine type as instanceType, a persistent-disk deviceInfo
// (storageType=Persistent, storageClass=standard — no diskIops/throughput and no
// region ybImage, unlike AWS), the provider SSH access key, and GCP-sanitized
// ownership labels (GCE labels allow only lowercase/digits/_/-). Fields mirror
// the stress framework's gcp userIntent (yugaware_app.py:2560) and are verified
// against managed/.../cloud/PublicCloudConstants.java (Persistent is a valid GCP
// StorageType).
func gcpUserIntent(s UniverseSpec) map[string]any {
	volumes := s.VolumeCount
	if volumes <= 0 {
		volumes = DefaultGCPVolumeCount
	}
	storageType := s.StorageType
	if storageType == "" {
		storageType = DefaultGCPStorageType
	}
	regionList := make([]any, 0, len(s.RegionUUIDs))
	for _, u := range s.RegionUUIDs {
		regionList = append(regionList, u)
	}
	userIntent := map[string]any{
		"universeName":      s.UniverseName(),
		"provider":          s.ProviderUUID,
		"providerType":      "gcp",
		"regionList":        regionList,
		"instanceType":      s.InstanceType,
		"numNodes":          s.NumNodes,
		"replicationFactor": s.ReplicationFactor,
		"ybSoftwareVersion": s.YBSoftwareVersion,
		"assignPublicIP":    true,
		"useTimeSync":       true,
		"enableYSQL":        true,
		"enableYCQL":        true,
		"deviceInfo": map[string]any{
			"volumeSize":   s.VolumeSizeGB,
			"numVolumes":   volumes,
			"storageType":  storageType,
			"storageClass": DefaultGCPStorageClass,
		},
	}
	if s.AccessKeyCode != "" {
		userIntent["accessKeyCode"] = s.AccessKeyCode
	}
	// GCE labels reject uppercase and most punctuation, so the ownership tags must
	// be sanitized (mirrors the stress framework) or node launch fails on invalid
	// labels.
	if len(s.InstanceTags) > 0 || s.RunTag != "" {
		raw := make(map[string]string, len(s.InstanceTags)+1)
		for k, v := range s.InstanceTags {
			raw[k] = v
		}
		if s.RunTag != "" {
			raw["ampqa-run"] = s.RunTag
		}
		userIntent["instanceTags"] = toAnyMap(sanitizeGCPLabels(raw))
	}
	return userIntent
}

// toAnyMap widens a string map to map[string]any for a userIntent field.
func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// k8sUserIntent builds the userIntent for a kubernetes universe: providerType
// kubernetes, master/tserver K8SNodeResourceSpec (cpu cores + memory GiB), a
// deviceInfo carrying only volume size/count + the storage class, and NO
// instanceType/accessKeyCode/instanceTags (k8s launches pods, not tagged EC2).
// Fields and shapes mirror the stress framework's k8s userIntent
// (yugaware_app.py:2584) and the server model UserIntent.K8SNodeResourceSpec.
func k8sUserIntent(s UniverseSpec) map[string]any {
	volumes := s.VolumeCount
	if volumes <= 0 {
		volumes = DefaultVolumeCount
	}
	storageClass := s.K8sStorageClass
	if storageClass == "" {
		storageClass = DefaultK8sStorageClass
	}
	masterCPU, masterMem := s.MasterCPUCores, s.MasterMemGiB
	if masterCPU <= 0 {
		masterCPU = DefaultK8sMasterCPUCores
	}
	if masterMem <= 0 {
		masterMem = DefaultK8sMasterMemGiB
	}
	tserverCPU, tserverMem := s.TServerCPUCores, s.TServerMemGiB
	if tserverCPU <= 0 {
		tserverCPU = DefaultK8sTServerCPUCores
	}
	if tserverMem <= 0 {
		tserverMem = DefaultK8sTServerMemGiB
	}
	regionList := make([]any, 0, len(s.RegionUUIDs))
	for _, u := range s.RegionUUIDs {
		regionList = append(regionList, u)
	}
	return map[string]any{
		"universeName":      s.UniverseName(),
		"provider":          s.ProviderUUID,
		"providerType":      "kubernetes",
		"regionList":        regionList,
		"numNodes":          s.NumNodes,
		"replicationFactor": s.ReplicationFactor,
		"ybSoftwareVersion": s.YBSoftwareVersion,
		// k8s and Azure do not assign a public IP at the node level.
		"assignPublicIP": false,
		"useTimeSync":    true,
		"enableYSQL":     true,
		"enableYCQL":     true,
		"deviceInfo": map[string]any{
			"volumeSize":   s.VolumeSizeGB,
			"numVolumes":   volumes,
			"storageClass": storageClass,
		},
		"masterK8SNodeResourceSpec":  map[string]any{"cpuCoreCount": masterCPU, "memoryGib": masterMem},
		"tserverK8SNodeResourceSpec": map[string]any{"cpuCoreCount": tserverCPU, "memoryGib": tserverMem},
	}
}

// CreateUniverseFromSpec creates the universe via the single-call
// POST /universes/clusters (configure+create in one — UniverseClustersController
// .createAllClusters), which reliably returns a YBPTask{taskUUID, resourceUUID=
// universeUUID} to wait on. It does NOT wait for the task; the caller polls the
// task and then WaitUniverseReady.
func (c *Client) CreateUniverseFromSpec(ctx context.Context, s UniverseSpec) (TaskRef, error) {
	if s.ProviderUUID == "" {
		return TaskRef{}, fmt.Errorf("create universe %q: provider uuid is required", s.UniverseName())
	}
	var ref TaskRef
	if _, err := c.doJSON(ctx, http.MethodPost, c.custURL("/universes/clusters"), BuildConfigureRequest(s), &ref); err != nil {
		return TaskRef{}, fmt.Errorf("create universe %q: %w", s.UniverseName(), err)
	}
	c.log("yba: creating universe %q (task %s, universe %s)", s.UniverseName(), ref.TaskUUID, ref.ResourceUUID)
	return ref, nil
}

// WaitUniverseReady polls the universe until it is no longer updating, then
// confirms the last update SUCCEEDED — the authoritative "backing is healthy"
// signal (UniverseUpdateSucceeded set updateInProgress=false, updateSucceeded=
// true). Guards against a create task reporting done before the universe's flags
// settle, so provision never passes on a half-built backing store.
func (c *Client) WaitUniverseReady(ctx context.Context, ref string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := c.PollInitial
	if backoff <= 0 {
		backoff = DefaultPollInitial
	}
	for {
		u, err := c.GetUniverse(ctx, ref)
		if err != nil {
			return fmt.Errorf("wait universe %q ready: %w", ref, err)
		}
		if !u.UniverseDetails.UpdateInProgress {
			if !u.UniverseDetails.UpdateSucceeded {
				return fmt.Errorf("universe %q update did not succeed (updateSucceeded=false)", ref)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("universe %q still updating after %s", ref, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > c.PollMax && c.PollMax > 0 {
			backoff = c.PollMax
		}
	}
}
