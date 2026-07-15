package yba

import (
	"context"
	"fmt"
	"net/http"
)

// Kubernetes provider + universe support. This mirrors the AWS path (provider.go
// / universe_create.go) for the cheapest fully-in-cluster YBA mode: a
// YBA-managed universe standing up ON the same k8s cluster the YBA itself runs
// in (co-located in GKE), so no cloud VMs are launched and no cross-cloud egress
// is paid.
//
// A kubernetes provider is created against YBA's dedicated V1 UI-only endpoint
// POST …/providers/kubernetes (KubernetesProviderFormData), NOT the generic
// /providers path the AWS provider uses: the k8s form carries UPPERCASE
// env-style config maps (KUBECONFIG_* / STORAGE_CLASS) at the provider and
// zone levels, and it returns the created Provider synchronously (a uuid), not a
// task to poll. Payload shapes mirror the stress framework's
// yugaware_app.provider_k8s_setup (src/apps/yugaware/yugaware_app.py:3455) and
// the server model KubernetesInfo / KubernetesRegionInfo
// (managed/.../models/helpers/provider/{,region/}Kubernetes*Info.java) +
// KubernetesProviderFormData.
//
// The universe userIntent uses providerType=kubernetes with k8s resource specs
// (master/tserverK8SNodeResourceSpec: cpuCoreCount/memoryGib) and
// deviceInfo.storageClass — NEVER instanceType/AMI/keypair
// (UniverseDefinitionTaskParams.UserIntent.K8SNodeResourceSpec:1234-1256).

// Kubernetes provider/universe defaults. core/defaults_test.go asserts the
// embedded default.yaml agrees with these.
const (
	// DefaultK8sProviderName is the idempotent identity — an existing kubernetes
	// provider with this name is reused rather than duplicated.
	DefaultK8sProviderName = "ampqa-k8s"
	// DefaultK8sProvider is the KUBECONFIG_PROVIDER hint (the cluster flavor).
	DefaultK8sProvider = "gke"
	// DefaultK8sServiceAccount is the KUBECONFIG_SERVICE_ACCOUNT YBA uses in the
	// universe namespaces it creates.
	DefaultK8sServiceAccount = "yugabyte-helm"
	// DefaultK8sImageRegistry is the universe DB image registry (quay itest) —
	// KUBECONFIG_IMAGE_REGISTRY. k8s universes pull their image from here, not a
	// downloaded tarball release.
	DefaultK8sImageRegistry = "quay.io/yugabyte/yugabyte-itest"
	// DefaultK8sPullSecretName is the in-cluster image-pull Secret name YBA
	// references (KUBECONFIG_IMAGE_PULL_SECRET_NAME).
	DefaultK8sPullSecretName = "quay-secret"
	// DefaultK8sPullSecretFile is the KUBECONFIG_PULL_SECRET_NAME filename the
	// pull-secret content is stored under.
	DefaultK8sPullSecretFile = "pull_secret.yaml"
	// DefaultK8sStorageClass is the k8s StorageClass the universe PVCs bind to
	// (deviceInfo.storageClass + zone STORAGE_CLASS).
	DefaultK8sStorageClass = "standard"
	// DefaultK8sRegion / DefaultK8sZone are the co-located GKE region/zone (the
	// shared dev project's us-west1, matching apps.cloud.gcp).
	DefaultK8sRegion = "us-west1"
	DefaultK8sZone   = "us-west1-b"
	// DefaultK8sNumNodes / DefaultK8sReplication are the cheap RF1 default (1
	// tserver), overridable for RF3.
	DefaultK8sNumNodes    = 1
	DefaultK8sReplication = 1
	// DefaultK8sVolumeSizeGB is the per-node PVC size (small — cheap).
	DefaultK8sVolumeSizeGB = 50
	// Default master/tserver K8SNodeResourceSpec cpu (cores) + memory (GiB) —
	// small but within YBA's K8SNodeResourceSpec bounds (MIN_CPU 0.5, MIN_MEMORY
	// 2.0).
	DefaultK8sMasterCPUCores  = 2
	DefaultK8sMasterMemGiB    = 4
	DefaultK8sTServerCPUCores = 2
	DefaultK8sTServerMemGiB   = 4
)

// K8sProviderSpec is a kubernetes-provider-create request in typed form. Values
// come from apps.yba.k8s.* (BuildK8sProviderSpec), with the kubeconfig and
// pull-secret resolved from named secrets. KubeConfigContent/PullSecretContent
// are credential material — never logged, never recorded.
type K8sProviderSpec struct {
	Name           string // idempotent identity — an existing provider with this name is reused
	Provider       string // KUBECONFIG_PROVIDER ("gke")
	ServiceAccount string // KUBECONFIG_SERVICE_ACCOUNT
	ImageRegistry  string // KUBECONFIG_IMAGE_REGISTRY (universe DB image registry)
	PullSecretName string // KUBECONFIG_IMAGE_PULL_SECRET_NAME (in-cluster pull-secret name)
	PullSecretFile string // KUBECONFIG_PULL_SECRET_NAME (filename the content is stored under)
	StorageClass   string // zone STORAGE_CLASS (universe PVC storage class)
	KubeDomain     string // KUBE_DOMAIN (zone-level; "" omits it → cluster.local)

	// KubeConfigContent is the RAW kubeconfig YAML (KUBECONFIG_CONTENT). Empty →
	// YBA uses its own in-cluster service account (use_in_cluster). Sensitive.
	KubeConfigContent string
	// PullSecretContent is the pull-secret body (KUBECONFIG_PULL_SECRET_CONTENT)
	// YBA copies into the universe namespaces it creates. Empty → only the
	// in-cluster pull-secret NAME is referenced. Sensitive.
	PullSecretContent string

	Regions []K8sRegionSpec
}

// K8sRegionSpec is one provider region and its zones.
type K8sRegionSpec struct {
	Code  string
	Name  string
	Zones []K8sZoneSpec
}

// K8sZoneSpec is one zone in a region. The storage class / kubeconfig / domain
// are provider-wide (applied to every zone from the spec), so a zone carries
// only its identity.
type K8sZoneSpec struct {
	Code string
	Name string
}

// BuildK8sProviderSpec reads apps.yba.k8s.* into a K8sProviderSpec, resolving the
// kubeconfig and pull-secret by named secret. The kubeconfig is required unless
// apps.yba.k8s.use_in_cluster is true (YBA runs in-cluster and uses its own
// service account); the pull-secret is optional (empty → only the in-cluster
// secret NAME is referenced).
func BuildK8sProviderSpec(c ConfigGetter, resolve SecretResolver) (K8sProviderSpec, error) {
	var kubeConfig string
	if !c.GetBool("apps.yba.k8s.use_in_cluster", false) {
		name := c.GetString("apps.yba.k8s.kubeconfig_secret", "")
		if name == "" {
			return K8sProviderSpec{}, fmt.Errorf("apps.yba.k8s.kubeconfig_secret is required unless apps.yba.k8s.use_in_cluster is true")
		}
		if resolve == nil {
			return K8sProviderSpec{}, fmt.Errorf("apps.yba.k8s: no secret resolver to resolve kubeconfig_secret %q", name)
		}
		v, err := resolve(name)
		if err != nil {
			return K8sProviderSpec{}, fmt.Errorf("apps.yba.k8s kubeconfig_secret %q: %w", name, err)
		}
		kubeConfig = v
	}

	var pullSecret string
	if name := c.GetString("apps.yba.k8s.pull_secret_secret", ""); name != "" {
		if resolve == nil {
			return K8sProviderSpec{}, fmt.Errorf("apps.yba.k8s: no secret resolver to resolve pull_secret_secret %q", name)
		}
		v, err := resolve(name)
		if err != nil {
			return K8sProviderSpec{}, fmt.Errorf("apps.yba.k8s pull_secret_secret %q: %w", name, err)
		}
		pullSecret = v
	}

	region := c.GetString("apps.yba.k8s.region", DefaultK8sRegion)
	return K8sProviderSpec{
		Name:              c.GetString("apps.yba.k8s.provider_name", DefaultK8sProviderName),
		Provider:          c.GetString("apps.yba.k8s.kube_provider", DefaultK8sProvider),
		ServiceAccount:    c.GetString("apps.yba.k8s.service_account", DefaultK8sServiceAccount),
		ImageRegistry:     c.GetString("apps.yba.k8s.image_registry", DefaultK8sImageRegistry),
		PullSecretName:    c.GetString("apps.yba.k8s.pull_secret_name", DefaultK8sPullSecretName),
		PullSecretFile:    DefaultK8sPullSecretFile,
		StorageClass:      c.GetString("apps.yba.k8s.storage_class", DefaultK8sStorageClass),
		KubeDomain:        c.GetString("apps.yba.k8s.kube_domain", ""),
		KubeConfigContent: kubeConfig,
		PullSecretContent: pullSecret,
		Regions:           []K8sRegionSpec{{Code: region, Name: region, Zones: readK8sZones(c)}},
	}, nil
}

// readK8sZones reads apps.yba.k8s.zones (a list of {code[,name]}) into zone
// specs, defaulting to the single DefaultK8sZone when none are configured.
func readK8sZones(c ConfigGetter) []K8sZoneSpec {
	out := make([]K8sZoneSpec, 0)
	for _, zm := range c.GetMapSlice("apps.yba.k8s.zones", nil) {
		code := mapStr(zm, "code")
		if code == "" {
			continue
		}
		name := mapStr(zm, "name")
		if name == "" {
			name = code
		}
		out = append(out, K8sZoneSpec{Code: code, Name: name})
	}
	if len(out) == 0 {
		out = append(out, K8sZoneSpec{Code: DefaultK8sZone, Name: DefaultK8sZone})
	}
	return out
}

// BuildK8sUniverseSpec reads the k8s universe shape from apps.yba.k8s.* (with the
// shared name_prefix and version resolution from apps.yba.universe.*). The
// ProviderCode is kubernetes so BuildConfigureRequest emits the k8s userIntent.
func BuildK8sUniverseSpec(c ConfigGetter) UniverseSpec {
	version := c.GetString("apps.yba.universe.yb_version", "")
	if version == "" {
		version = c.GetString("versions.yugabytedb", "")
	}
	return UniverseSpec{
		NamePrefix:        c.GetString("apps.yba.universe.name_prefix", DefaultUniverseNamePrefix),
		ProviderCode:      "kubernetes",
		Region:            c.GetString("apps.yba.k8s.region", DefaultK8sRegion),
		NumNodes:          c.GetInt("apps.yba.k8s.num_nodes", DefaultK8sNumNodes),
		ReplicationFactor: c.GetInt("apps.yba.k8s.replication_factor", DefaultK8sReplication),
		YBSoftwareVersion: version,
		VolumeSizeGB:      c.GetInt("apps.yba.k8s.volume_size_gb", DefaultK8sVolumeSizeGB),
		VolumeCount:       c.GetInt("apps.yba.k8s.volume_count", DefaultVolumeCount),
		K8sStorageClass:   c.GetString("apps.yba.k8s.storage_class", DefaultK8sStorageClass),
		MasterCPUCores:    c.GetInt("apps.yba.k8s.master_cpu", DefaultK8sMasterCPUCores),
		MasterMemGiB:      c.GetInt("apps.yba.k8s.master_mem_gib", DefaultK8sMasterMemGiB),
		TServerCPUCores:   c.GetInt("apps.yba.k8s.tserver_cpu", DefaultK8sTServerCPUCores),
		TServerMemGiB:     c.GetInt("apps.yba.k8s.tserver_mem_gib", DefaultK8sTServerMemGiB),
		EnableYbc:         c.GetBool("apps.yba.universe.enable_ybc", false),
	}
}

// k8sProviderPayload builds the KubernetesProviderFormData body: the top-level
// UPPERCASE config map (registry + pull-secret), and regionList → zoneList each
// carrying its own UPPERCASE config (storage class + raw kubeconfig content).
// The kubeconfig/pull-secret content is credential material — this function only
// assembles the payload; it never logs it.
func k8sProviderPayload(spec K8sProviderSpec) map[string]any {
	config := map[string]any{
		"KUBECONFIG_PROVIDER":               spec.Provider,
		"KUBECONFIG_SERVICE_ACCOUNT":        spec.ServiceAccount,
		"KUBECONFIG_IMAGE_REGISTRY":         spec.ImageRegistry,
		"KUBECONFIG_IMAGE_PULL_SECRET_NAME": spec.PullSecretName,
	}
	if spec.PullSecretContent != "" {
		file := spec.PullSecretFile
		if file == "" {
			file = DefaultK8sPullSecretFile
		}
		config["KUBECONFIG_PULL_SECRET_NAME"] = file
		config["KUBECONFIG_PULL_SECRET_CONTENT"] = spec.PullSecretContent
	}

	regions := make([]any, 0, len(spec.Regions))
	for _, r := range spec.Regions {
		zones := make([]any, 0, len(r.Zones))
		for _, z := range r.Zones {
			name := z.Name
			if name == "" {
				name = z.Code
			}
			zcfg := map[string]any{
				"STORAGE_CLASS":   spec.StorageClass,
				"KUBECONFIG_NAME": "kubeconfig",
			}
			if spec.KubeConfigContent != "" {
				zcfg["KUBECONFIG_CONTENT"] = spec.KubeConfigContent
			}
			if spec.KubeDomain != "" {
				zcfg["KUBE_DOMAIN"] = spec.KubeDomain
			}
			zones = append(zones, map[string]any{"code": z.Code, "name": name, "config": zcfg})
		}
		name := r.Name
		if name == "" {
			name = r.Code
		}
		regions = append(regions, map[string]any{"code": r.Code, "name": name, "zoneList": zones})
	}

	return map[string]any{
		"code":       "kubernetes",
		"name":       spec.Name,
		"config":     config,
		"regionList": regions,
	}
}

// AddK8sProvider creates a kubernetes provider via the dedicated V1 UI-only
// endpoint POST …/providers/kubernetes (CloudProviderUiOnlyController
// .createKubernetes), which returns the created Provider synchronously (a uuid)
// rather than a task to poll. It returns the new provider's UUID. A rare build
// that returns a task instead is tolerated (the task is waited and its resource
// uuid used).
func (c *Client) AddK8sProvider(ctx context.Context, spec K8sProviderSpec) (string, error) {
	if spec.Name == "" {
		return "", fmt.Errorf("add k8s provider: name is required")
	}
	if len(spec.Regions) == 0 {
		return "", fmt.Errorf("add k8s provider %q: at least one region is required", spec.Name)
	}
	var out struct {
		UUID         string `json:"uuid"`
		ResourceUUID string `json:"resourceUUID"`
		TaskUUID     string `json:"taskUUID"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, c.custURL("/providers/kubernetes"), k8sProviderPayload(spec), &out); err != nil {
		return "", fmt.Errorf("create k8s provider %q: %w", spec.Name, err)
	}
	if out.TaskUUID != "" {
		if _, err := c.WaitTask(ctx, out.TaskUUID); err != nil {
			return "", fmt.Errorf("create k8s provider %q: %w", spec.Name, err)
		}
	}
	uuid := out.UUID
	if uuid == "" {
		uuid = out.ResourceUUID
	}
	if uuid == "" {
		return "", fmt.Errorf("create k8s provider %q returned no provider uuid", spec.Name)
	}
	return uuid, nil
}

// EnsureK8sProvider returns the UUID of a kubernetes provider named spec.Name,
// creating it only if none exists. Idempotent reuse is core infra hygiene
// (AGENTS.md §8): a second run reuses the provider instead of duplicating it. A
// provider-list failure is a hard error (not treated as "absent"), so a
// transient error never triggers a spurious create.
func (c *Client) EnsureK8sProvider(ctx context.Context, spec K8sProviderSpec) (string, error) {
	providers, err := c.GetProviders(ctx)
	if err != nil {
		return "", fmt.Errorf("ensure k8s provider %q: list providers: %w", spec.Name, err)
	}
	for _, p := range providers {
		if p.Name == spec.Name {
			c.log("yba: reusing existing provider %q (%s)", p.Name, p.UUID)
			return p.UUID, nil
		}
	}
	c.log("yba: creating kubernetes provider %q (registry %s)", spec.Name, spec.ImageRegistry)
	return c.AddK8sProvider(ctx, spec)
}
