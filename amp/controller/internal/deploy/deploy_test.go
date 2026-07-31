package deploy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// testKubernetesConfig returns a fully-populated KubernetesConfig for the
// Helm-deployer value-building tests.
func testKubernetesConfig() KubernetesConfig {
	return KubernetesConfig{
		Namespace:         "endpoints",
		HelmBin:           "helm",
		ChartPath:         "/charts/yb-amp-endpoint",
		EndpointImageRepo: "registry.example.com/yb-amp-endpoint",
		EndpointImageTag:  "v1.2.3",
		ComputePort:       55433,
		Timeout:           10 * time.Minute,
	}
}

// testEndpointSpec returns an EndpointSpec as the API handler builds it: the
// storage branch id is already a 32-hex string, and the YB connection is populated.
func testEndpointSpec() EndpointSpec {
	return EndpointSpec{
		Name:      "yb-ep-92331eedafb2a0ef3b2d724d649bee78",
		BranchID:  "92331eedafb2a0ef3b2d724d649bee78",
		PgVersion: 15,
		YB: YBConn{
			HostPorts: "yb-tserver:5433",
			User:      "yugabyte",
			DBName:    "yb_storage",
			Password:  "s3cret",
		},
	}
}

func TestKubernetesValues(t *testing.T) {
	cfg := testKubernetesConfig()
	cfg.PullSecret = "regcred"
	k := NewKubernetesDeployer(cfg, nil)

	spec := testEndpointSpec()
	release := spec.Name
	v := k.values(spec, release)

	if v.FullnameOverride != release {
		t.Errorf("FullnameOverride = %q, want %q", v.FullnameOverride, release)
	}
	if v.Image.Repository != cfg.EndpointImageRepo {
		t.Errorf("Image.Repository = %q, want %q", v.Image.Repository, cfg.EndpointImageRepo)
	}
	if v.Image.Tag != cfg.EndpointImageTag {
		t.Errorf("Image.Tag = %q, want %q", v.Image.Tag, cfg.EndpointImageTag)
	}
	wantSecrets := []map[string]string{{"name": "regcred"}}
	if !reflect.DeepEqual(v.ImagePullSecrets, wantSecrets) {
		t.Errorf("ImagePullSecrets = %v, want %v", v.ImagePullSecrets, wantSecrets)
	}
	if v.Yugabyte.HostPorts != spec.YB.HostPorts {
		t.Errorf("Yugabyte.HostPorts = %q, want %q", v.Yugabyte.HostPorts, spec.YB.HostPorts)
	}
	if v.Yugabyte.User != spec.YB.User {
		t.Errorf("Yugabyte.User = %q, want %q", v.Yugabyte.User, spec.YB.User)
	}
	if v.Yugabyte.Dbname != spec.YB.DBName {
		t.Errorf("Yugabyte.Dbname = %q, want %q", v.Yugabyte.Dbname, spec.YB.DBName)
	}
	if v.Yugabyte.Password != spec.YB.Password {
		t.Errorf("Yugabyte.Password = %q, want %q", v.Yugabyte.Password, spec.YB.Password)
	}
	if v.Endpoint.PgVersion != "15" {
		t.Errorf("Endpoint.PgVersion = %q, want %q", v.Endpoint.PgVersion, "15")
	}
	if v.Endpoint.ComputePort != cfg.ComputePort {
		t.Errorf("Endpoint.ComputePort = %d, want %d", v.Endpoint.ComputePort, cfg.ComputePort)
	}
	if v.Endpoint.BranchId != spec.BranchID {
		t.Errorf("Endpoint.BranchId = %q, want %q", v.Endpoint.BranchId, spec.BranchID)
	}
}

func TestKubernetesValuesPgVersionDefault(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)
	for _, pg := range []int{0, -1, -100} {
		spec := testEndpointSpec()
		spec.PgVersion = pg
		v := k.values(spec, spec.Name)
		if v.Endpoint.PgVersion != "18" {
			t.Errorf("PgVersion=%d -> Endpoint.PgVersion = %q, want default %q", pg, v.Endpoint.PgVersion, "17")
		}
	}
}

func TestKubernetesValuesExistingSecret(t *testing.T) {
	cfg := testKubernetesConfig()
	cfg.BackingYBExistingSecret = "yb-creds"
	cfg.BackingYBExistingSecretKey = "password"
	k := NewKubernetesDeployer(cfg, nil)

	v := k.values(testEndpointSpec(), "yb-ep-rel")
	if v.Yugabyte.ExistingSecret != "yb-creds" {
		t.Errorf("Yugabyte.ExistingSecret = %q, want yb-creds", v.Yugabyte.ExistingSecret)
	}
	if v.Yugabyte.ExistingSecretKey != "password" {
		t.Errorf("Yugabyte.ExistingSecretKey = %q, want password", v.Yugabyte.ExistingSecretKey)
	}
	// With an existing Secret the inline password is not emitted.
	if v.Yugabyte.Password != "" {
		t.Errorf("Yugabyte.Password = %q, want empty when an existing Secret is set", v.Yugabyte.Password)
	}
}

func TestKubernetesValuesNoPullSecret(t *testing.T) {
	cfg := testKubernetesConfig() // PullSecret left empty
	k := NewKubernetesDeployer(cfg, nil)

	v := k.values(testEndpointSpec(), "yb-ep-rel")
	if v.ImagePullSecrets != nil {
		t.Errorf("ImagePullSecrets = %v, want nil when PullSecret is empty", v.ImagePullSecrets)
	}

	// And it must be omitted from the marshaled values document entirely.
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(data, &asMap); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}
	if _, present := asMap["imagePullSecrets"]; present {
		t.Errorf("imagePullSecrets should be omitted from %s", data)
	}
}

func TestKubernetesValuesResourcesDefault(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)

	// A spec that omits CPU/Memory reserves the 2-core / 4Gi default, matching
	// the chart's own values.yaml default.
	v := k.values(testEndpointSpec(), "yb-ep-rel")
	if v.Endpoint.Resources.CPU != "2" {
		t.Errorf("Endpoint.Resources.CPU = %q, want default %q", v.Endpoint.Resources.CPU, "2")
	}
	if v.Endpoint.Resources.Memory != "4Gi" {
		t.Errorf("Endpoint.Resources.Memory = %q, want default %q", v.Endpoint.Resources.Memory, "4Gi")
	}
}

func TestKubernetesValuesResourcesOverride(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)

	spec := testEndpointSpec()
	spec.CPU = "8"
	spec.Memory = "16Gi"
	v := k.values(spec, spec.Name)
	if v.Endpoint.Resources.CPU != "8" {
		t.Errorf("Endpoint.Resources.CPU = %q, want %q", v.Endpoint.Resources.CPU, "8")
	}
	if v.Endpoint.Resources.Memory != "16Gi" {
		t.Errorf("Endpoint.Resources.Memory = %q, want %q", v.Endpoint.Resources.Memory, "16Gi")
	}
}

func TestKubernetesValuesServiceType(t *testing.T) {
	cfg := testKubernetesConfig()
	cfg.ServiceType = "NodePort"
	cfg.ServiceAnnotations = map[string]string{"example.com/k": "v"}
	k := NewKubernetesDeployer(cfg, nil)

	v := k.values(testEndpointSpec(), "yb-ep-rel")
	if v.Service.Type != "NodePort" {
		t.Errorf("Service.Type = %q, want NodePort", v.Service.Type)
	}
	if v.Service.Annotations["example.com/k"] != "v" {
		t.Errorf("Service.Annotations = %v, want example.com/k=v", v.Service.Annotations)
	}
}

func TestKubernetesValuesNodeSelector(t *testing.T) {
	cfg := testKubernetesConfig()
	cfg.NodeSelector = map[string]string{"cloud.google.com/gke-ephemeral-storage-local-ssd": "true"}
	k := NewKubernetesDeployer(cfg, nil)

	v := k.values(testEndpointSpec(), "yb-ep-rel")
	if v.NodeSelector["cloud.google.com/gke-ephemeral-storage-local-ssd"] != "true" {
		t.Errorf("NodeSelector = %v, want the local-NVMe-SSD label", v.NodeSelector)
	}

	// When unset it must be omitted from the marshaled values entirely, so the
	// endpoint chart's own default (schedule anywhere) applies.
	v2 := NewKubernetesDeployer(testKubernetesConfig(), nil).values(testEndpointSpec(), "yb-ep-rel")
	data, err := json.Marshal(v2)
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(data, &asMap); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}
	if _, present := asMap["nodeSelector"]; present {
		t.Errorf("nodeSelector should be omitted when unset, got %s", data)
	}
}

func TestKubernetesValuesPersistence(t *testing.T) {
	cfg := testKubernetesConfig()
	cfg.StorageClass = "yb-standard"
	cfg.DiskSize = "20Gi"
	k := NewKubernetesDeployer(cfg, nil)

	v := k.values(testEndpointSpec(), "yb-ep-rel")
	if v.Persistence == nil {
		t.Fatal("Persistence = nil, want it populated when StorageClass is set")
	}
	if v.Persistence.StorageClass != "yb-standard" {
		t.Errorf("Persistence.StorageClass = %q, want yb-standard", v.Persistence.StorageClass)
	}
	if v.Persistence.Size != "20Gi" {
		t.Errorf("Persistence.Size = %q, want 20Gi", v.Persistence.Size)
	}
}

func TestKubernetesValuesNoPersistence(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil) // StorageClass empty

	v := k.values(testEndpointSpec(), "yb-ep-rel")
	if v.Persistence != nil {
		t.Errorf("Persistence = %v, want nil when StorageClass is empty", v.Persistence)
	}
	// The key must be omitted from the marshaled values document entirely so the
	// chart keeps its emptyDir default.
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(data, &asMap); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}
	if _, present := asMap["persistence"]; present {
		t.Errorf("persistence should be omitted from %s", data)
	}
}

func TestParseServiceNodePort(t *testing.T) {
	body := []byte(`{"spec":{"ports":[
		{"name":"pg-compute","port":55433,"nodePort":31234},
		{"name":"ps-http","port":9898,"nodePort":31999}
	]}}`)
	if got := parseServiceNodePort(body, 55433); got != 31234 {
		t.Errorf("parseServiceNodePort(compute) = %d, want 31234", got)
	}
	if got := parseServiceNodePort(body, 12345); got != 0 {
		t.Errorf("parseServiceNodePort(no match) = %d, want 0", got)
	}
	if got := parseServiceNodePort([]byte("not json"), 55433); got != 0 {
		t.Errorf("parseServiceNodePort(bad json) = %d, want 0", got)
	}
}

func TestParseANodeIP(t *testing.T) {
	// A non-Ready node is skipped; an ExternalIP is preferred over an InternalIP.
	body := []byte(`{"items":[
		{"status":{"conditions":[{"type":"Ready","status":"False"}],"addresses":[{"type":"ExternalIP","address":"9.9.9.9"}]}},
		{"status":{"conditions":[{"type":"Ready","status":"True"}],"addresses":[{"type":"InternalIP","address":"10.0.0.5"},{"type":"ExternalIP","address":"34.1.2.3"}]}}
	]}`)
	if got := parseANodeIP(body); got != "34.1.2.3" {
		t.Errorf("parseANodeIP = %q, want 34.1.2.3", got)
	}
	// Internal-only Ready node falls back to its InternalIP.
	internalOnly := []byte(`{"items":[{"status":{"conditions":[{"type":"Ready","status":"True"}],"addresses":[{"type":"InternalIP","address":"10.150.0.7"}]}}]}`)
	if got := parseANodeIP(internalOnly); got != "10.150.0.7" {
		t.Errorf("parseANodeIP(internal) = %q, want 10.150.0.7", got)
	}
	// No Ready node yields no address.
	notReady := []byte(`{"items":[{"status":{"conditions":[{"type":"Ready","status":"False"}],"addresses":[{"type":"InternalIP","address":"10.0.0.9"}]}}]}`)
	if got := parseANodeIP(notReady); got != "" {
		t.Errorf("parseANodeIP(not ready) = %q, want empty", got)
	}
}

func TestKubernetesAddress(t *testing.T) {
	cfg := testKubernetesConfig()
	k := NewKubernetesDeployer(cfg, nil)
	release := "yb-ep-92331eedafb2a0ef3b2d724d649bee78"

	want := fmt.Sprintf("%s.%s.svc.cluster.local:%d", release, cfg.Namespace, cfg.ComputePort)
	if got := k.address(release); got != want {
		t.Errorf("address(%s) = %q, want %q", release, got, want)
	}
}

// EndpointSafekeeperURL addresses the endpoint's safekeeper at the same stable
// host the controller dials (CONTROLLER_REACHABLE_HOST): the Service DNS in k8s,
// the container name on Docker — both at the fixed safekeeper HTTP port.
func TestEndpointSafekeeperURL(t *testing.T) {
	id := "yb-ep-92331eedafb2a0ef3b2d724d649bee78"

	cfg := testKubernetesConfig()
	k := NewKubernetesDeployer(cfg, nil)
	wantK8s := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", id, cfg.Namespace, safekeeperHTTPPort)
	if got := k.EndpointSafekeeperURL(id); got != wantK8s {
		t.Errorf("kubernetes EndpointSafekeeperURL(%s) = %q, want %q", id, got, wantK8s)
	}

	d := NewDockerDeployer(DockerConfig{}, nil)
	wantDocker := fmt.Sprintf("http://%s:%d", id, safekeeperHTTPPort)
	if got := d.EndpointSafekeeperURL(id); got != wantDocker {
		t.Errorf("docker EndpointSafekeeperURL(%s) = %q, want %q", id, got, wantDocker)
	}
}
