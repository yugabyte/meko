package yba

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func sampleK8sProviderSpec() K8sProviderSpec {
	return K8sProviderSpec{
		Name:              "ampqa-k8s",
		Provider:          "gke",
		ServiceAccount:    "yugabyte-helm",
		ImageRegistry:     "quay.io/yugabyte/yugabyte-itest",
		PullSecretName:    "quay-secret",
		PullSecretFile:    "pull_secret.yaml",
		PullSecretContent: "apiVersion: v1\nkind: Secret\n# pull secret body",
		KubeConfigContent: "apiVersion: v1\nkind: Config\n# kubeconfig body",
		KubeDomain:        "cluster.local",
		StorageClass:      "standard",
		Regions: []K8sRegionSpec{{
			Code: "us-west1", Name: "us-west1",
			Zones: []K8sZoneSpec{{Code: "us-west1-b", Name: "us-west1-b"}},
		}},
	}
}

// TestAddK8sProviderPayloadShape asserts the kubernetes provider-create body
// carries code=kubernetes, the UPPERCASE provider config (registry + pull
// secret), and regionList → zoneList → config (storage class + raw kubeconfig
// content + kube domain) — the KubernetesProviderFormData shape YBA expects.
func TestAddK8sProviderPayloadShape(t *testing.T) {
	f := newFake()
	defer f.close()

	var body map[string]any
	f.on("POST /api/v1/customers/cust1/providers/kubernetes", func(w http.ResponseWriter, r *http.Request) {
		body = captureBody(t, r)
		writeJSON(w, http.StatusOK, map[string]any{"uuid": "k8s-prov-1"})
	})

	uuid, err := f.client().AddK8sProvider(context.Background(), sampleK8sProviderSpec())
	if err != nil {
		t.Fatalf("add k8s provider: %v", err)
	}
	if uuid != "k8s-prov-1" {
		t.Fatalf("uuid = %q, want k8s-prov-1 (synchronous provider create returns a uuid, not a task)", uuid)
	}

	if body["code"] != "kubernetes" || body["name"] != "ampqa-k8s" {
		t.Errorf("code/name = %v/%v", body["code"], body["name"])
	}
	config, _ := body["config"].(map[string]any)
	if config["KUBECONFIG_IMAGE_REGISTRY"] != "quay.io/yugabyte/yugabyte-itest" {
		t.Errorf("KUBECONFIG_IMAGE_REGISTRY = %v", config["KUBECONFIG_IMAGE_REGISTRY"])
	}
	if config["KUBECONFIG_IMAGE_PULL_SECRET_NAME"] != "quay-secret" {
		t.Errorf("KUBECONFIG_IMAGE_PULL_SECRET_NAME = %v", config["KUBECONFIG_IMAGE_PULL_SECRET_NAME"])
	}
	if config["KUBECONFIG_PULL_SECRET_CONTENT"] != sampleK8sProviderSpec().PullSecretContent {
		t.Errorf("KUBECONFIG_PULL_SECRET_CONTENT not passed through")
	}
	if config["KUBECONFIG_PULL_SECRET_NAME"] != "pull_secret.yaml" {
		t.Errorf("KUBECONFIG_PULL_SECRET_NAME = %v, want the pull-secret filename", config["KUBECONFIG_PULL_SECRET_NAME"])
	}
	if config["KUBECONFIG_PROVIDER"] != "gke" || config["KUBECONFIG_SERVICE_ACCOUNT"] != "yugabyte-helm" {
		t.Errorf("provider/service-account = %v/%v", config["KUBECONFIG_PROVIDER"], config["KUBECONFIG_SERVICE_ACCOUNT"])
	}

	regions, _ := body["regionList"].([]any)
	if len(regions) != 1 {
		t.Fatalf("regionList = %v, want 1", regions)
	}
	region0, _ := regions[0].(map[string]any)
	if region0["code"] != "us-west1" {
		t.Errorf("region code = %v", region0["code"])
	}
	zones, _ := region0["zoneList"].([]any)
	if len(zones) != 1 {
		t.Fatalf("zoneList = %v, want 1", zones)
	}
	zone0, _ := zones[0].(map[string]any)
	if zone0["code"] != "us-west1-b" {
		t.Errorf("zone code = %v", zone0["code"])
	}
	zcfg, _ := zone0["config"].(map[string]any)
	if zcfg["STORAGE_CLASS"] != "standard" {
		t.Errorf("zone STORAGE_CLASS = %v", zcfg["STORAGE_CLASS"])
	}
	if zcfg["KUBECONFIG_CONTENT"] != sampleK8sProviderSpec().KubeConfigContent {
		t.Errorf("zone KUBECONFIG_CONTENT not passed through")
	}
	if zcfg["KUBECONFIG_NAME"] != "kubeconfig" {
		t.Errorf("zone KUBECONFIG_NAME = %v", zcfg["KUBECONFIG_NAME"])
	}
	if zcfg["KUBE_DOMAIN"] != "cluster.local" {
		t.Errorf("zone KUBE_DOMAIN = %v", zcfg["KUBE_DOMAIN"])
	}
}

// TestAddK8sProviderInClusterOmitsKubeconfig: an empty KubeConfigContent (the
// in-cluster service-account path) leaves KUBECONFIG_CONTENT out of the zone
// config, and an empty pull-secret content leaves the pull-secret keys out.
func TestAddK8sProviderInClusterOmitsKubeconfig(t *testing.T) {
	f := newFake()
	defer f.close()
	var body map[string]any
	f.on("POST /api/v1/customers/cust1/providers/kubernetes", func(w http.ResponseWriter, r *http.Request) {
		body = captureBody(t, r)
		writeJSON(w, http.StatusOK, map[string]any{"uuid": "k8s-prov-2"})
	})

	spec := sampleK8sProviderSpec()
	spec.KubeConfigContent = "" // in-cluster SA
	spec.PullSecretContent = "" // rely on the in-cluster secret NAME only
	spec.KubeDomain = ""
	if _, err := f.client().AddK8sProvider(context.Background(), spec); err != nil {
		t.Fatalf("add k8s provider: %v", err)
	}
	config, _ := body["config"].(map[string]any)
	if _, ok := config["KUBECONFIG_PULL_SECRET_CONTENT"]; ok {
		t.Errorf("KUBECONFIG_PULL_SECRET_CONTENT present with empty content")
	}
	zone0 := body["regionList"].([]any)[0].(map[string]any)["zoneList"].([]any)[0].(map[string]any)
	zcfg, _ := zone0["config"].(map[string]any)
	if _, ok := zcfg["KUBECONFIG_CONTENT"]; ok {
		t.Errorf("KUBECONFIG_CONTENT present with in-cluster (empty content) spec")
	}
	if _, ok := zcfg["KUBE_DOMAIN"]; ok {
		t.Errorf("KUBE_DOMAIN present with empty domain")
	}
}

// TestEnsureK8sProviderReusesExisting: a provider with the configured name is
// reused (no create call), mirroring the AWS idempotency.
func TestEnsureK8sProviderReusesExisting(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/providers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{
			map[string]any{"uuid": "k8s-existing", "code": "kubernetes", "name": "ampqa-k8s"},
		})
	})
	f.on("POST /api/v1/customers/cust1/providers/kubernetes", func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("create endpoint hit despite existing k8s provider")
		writeJSON(w, http.StatusOK, map[string]any{"uuid": "x"})
	})
	uuid, err := f.client().EnsureK8sProvider(context.Background(), sampleK8sProviderSpec())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if uuid != "k8s-existing" {
		t.Errorf("uuid = %q, want k8s-existing (reused)", uuid)
	}
	if sawRequest(f, "POST /api/v1/customers/cust1/providers/kubernetes") {
		t.Errorf("create endpoint was hit; expected reuse")
	}
}

// TestEnsureK8sProviderCreatesWhenAbsent: no matching provider → the k8s create
// endpoint is hit and its uuid returned.
func TestEnsureK8sProviderCreatesWhenAbsent(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/providers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{map[string]any{"uuid": "other", "code": "aws", "name": "someone-aws"}})
	})
	var created bool
	f.on("POST /api/v1/customers/cust1/providers/kubernetes", func(w http.ResponseWriter, _ *http.Request) {
		created = true
		writeJSON(w, http.StatusOK, map[string]any{"uuid": "k8s-new"})
	})
	uuid, err := f.client().EnsureK8sProvider(context.Background(), sampleK8sProviderSpec())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if uuid != "k8s-new" || !created {
		t.Errorf("uuid=%q created=%v, want k8s-new/true", uuid, created)
	}
}

// TestAddProviderKubernetesDispatch: the generic AddProvider switch routes
// "kubernetes"/"k8s" to AddK8sProvider (via spec.K8s) and returns the provider
// uuid as the TaskRef ResourceUUID; a missing spec.K8s is a clear error.
func TestAddProviderKubernetesDispatch(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/customers/cust1/providers/kubernetes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"uuid": "k8s-disp"})
	})
	k := sampleK8sProviderSpec()
	for _, cloud := range []string{"kubernetes", "k8s"} {
		ref, err := f.client().AddProvider(context.Background(), ProviderSpec{Cloud: cloud, K8s: &k})
		if err != nil {
			t.Fatalf("cloud %q: %v", cloud, err)
		}
		if ref.ResourceUUID != "k8s-disp" {
			t.Errorf("cloud %q: ResourceUUID = %q, want k8s-disp", cloud, ref.ResourceUUID)
		}
	}
	if _, err := f.client().AddProvider(context.Background(), ProviderSpec{Cloud: "kubernetes"}); err == nil {
		t.Errorf("expected error when spec.K8s is nil")
	}
}

// TestBuildK8sUniverseSpecDefaults: an empty config yields the cheap k8s defaults
// (RF1, single node, standard storage class) with ProviderCode kubernetes, and
// the version falls back to versions.yugabytedb.
func TestBuildK8sUniverseSpecDefaults(t *testing.T) {
	s := BuildK8sUniverseSpec(fakeConfig{m: map[string]any{"versions.yugabytedb": "2025.2.1.0"}})
	if s.ProviderCode != "kubernetes" {
		t.Errorf("ProviderCode = %q, want kubernetes", s.ProviderCode)
	}
	if s.NumNodes != DefaultK8sNumNodes || s.ReplicationFactor != DefaultK8sReplication {
		t.Errorf("nodes/RF = %d/%d, want %d/%d", s.NumNodes, s.ReplicationFactor, DefaultK8sNumNodes, DefaultK8sReplication)
	}
	if s.K8sStorageClass != DefaultK8sStorageClass {
		t.Errorf("K8sStorageClass = %q, want %q", s.K8sStorageClass, DefaultK8sStorageClass)
	}
	if s.MasterCPUCores != DefaultK8sMasterCPUCores || s.TServerMemGiB != DefaultK8sTServerMemGiB {
		t.Errorf("resources = %d cpu / %d mem", s.MasterCPUCores, s.TServerMemGiB)
	}
	if s.Region != DefaultK8sRegion {
		t.Errorf("Region = %q, want %q", s.Region, DefaultK8sRegion)
	}
	if s.YBSoftwareVersion != "2025.2.1.0" {
		t.Errorf("version = %q, want fallback to versions.yugabytedb", s.YBSoftwareVersion)
	}
}

// TestBuildK8sProviderSpecResolvesKubeconfig: kubeconfig + pull-secret resolve by
// NAME, and the region/zones come from config.
func TestBuildK8sProviderSpecResolvesKubeconfig(t *testing.T) {
	cfg := fakeConfig{m: map[string]any{
		"apps.yba.k8s.kubeconfig_secret":  "kubeconfig_secret",
		"apps.yba.k8s.pull_secret_secret": "quay_images",
		"apps.yba.k8s.region":             "us-west1",
		"apps.yba.k8s.zones":              []map[string]any{{"code": "us-west1-a"}, {"code": "us-west1-b"}},
	}}
	resolve := func(name string) (string, error) {
		switch name {
		case "kubeconfig_secret":
			return "KUBECONFIG-YAML", nil
		case "quay_images":
			return "PULL-SECRET-YAML", nil
		}
		return "", fmt.Errorf("unknown secret %q", name)
	}
	spec, err := BuildK8sProviderSpec(cfg, resolve)
	if err != nil {
		t.Fatalf("build k8s spec: %v", err)
	}
	if spec.KubeConfigContent != "KUBECONFIG-YAML" {
		t.Errorf("KubeConfigContent = %q", spec.KubeConfigContent)
	}
	if spec.PullSecretContent != "PULL-SECRET-YAML" {
		t.Errorf("PullSecretContent = %q", spec.PullSecretContent)
	}
	if len(spec.Regions) != 1 || spec.Regions[0].Code != "us-west1" {
		t.Fatalf("regions = %+v", spec.Regions)
	}
	if z := spec.Regions[0].Zones; len(z) != 2 || z[0].Code != "us-west1-a" || z[1].Code != "us-west1-b" {
		t.Errorf("zones = %+v", z)
	}
}

// TestBuildK8sProviderSpecInClusterNeedsNoKubeconfig: use_in_cluster=true makes
// the kubeconfig secret optional (YBA uses its own in-cluster service account).
func TestBuildK8sProviderSpecInClusterNeedsNoKubeconfig(t *testing.T) {
	cfg := fakeConfig{m: map[string]any{"apps.yba.k8s.use_in_cluster": true}}
	spec, err := BuildK8sProviderSpec(cfg, func(string) (string, error) { return "", fmt.Errorf("should not be called") })
	if err != nil {
		t.Fatalf("in-cluster build: %v", err)
	}
	if spec.KubeConfigContent != "" {
		t.Errorf("KubeConfigContent = %q, want empty (in-cluster SA)", spec.KubeConfigContent)
	}
}

// TestBuildK8sProviderSpecMissingKubeconfigErrors: without use_in_cluster and
// without a kubeconfig secret, building the spec is a hard error.
func TestBuildK8sProviderSpecMissingKubeconfigErrors(t *testing.T) {
	if _, err := BuildK8sProviderSpec(fakeConfig{m: map[string]any{}}, func(string) (string, error) { return "", nil }); err == nil {
		t.Fatalf("expected error when neither use_in_cluster nor kubeconfig_secret is set")
	}
}
