package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
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

// testEndpointSpec returns an EndpointSpec as the API handler builds it, with
// the YB connection populated.
func testEndpointSpec() EndpointSpec {
	return EndpointSpec{
		Name:      "yb-ep-92331eed-afb2-a0ef-3b2d-724d649bee78",
		BranchID:  uuid.MustParse("92331eed-afb2-a0ef-3b2d-724d649bee78"),
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
	if v.Endpoint.BranchId != spec.BranchID.String() {
		t.Errorf("Endpoint.BranchId = %q, want %q", v.Endpoint.BranchId, spec.BranchID)
	}
}

// The endpoint's version is the chart's image.tag, and an endpoint that asks for
// nothing gets exactly the configured tag — the behaviour before versions were
// per-endpoint. The fallback is resolved here rather than delegated to the chart
// (image.tag has no omitempty, and the chart reads an empty tag as its own
// appVersion), so both directions are asserted on the rendered values.
func TestKubernetesValuesVersionIsTheImageTag(t *testing.T) {
	cfg := testKubernetesConfig()
	k := NewKubernetesDeployer(cfg, nil)

	unset := k.values(testEndpointSpec(), "rel")
	if unset.Image.Tag != cfg.EndpointImageTag {
		t.Errorf("Image.Tag with no version = %q, want the configured %q", unset.Image.Tag, cfg.EndpointImageTag)
	}

	spec := testEndpointSpec()
	spec.Version = "v9.9.9"
	pinned := k.values(spec, "rel")
	if pinned.Image.Tag != "v9.9.9" {
		t.Errorf("Image.Tag with a version = %q, want v9.9.9", pinned.Image.Tag)
	}
	// The version moves the tag only: the repository stays deployer configuration,
	// so an upgrade cannot silently point an endpoint at a different image.
	if pinned.Image.Repository != cfg.EndpointImageRepo {
		t.Errorf("Image.Repository = %q, want the configured %q", pinned.Image.Repository, cfg.EndpointImageRepo)
	}
}

// An unset branch id must reach the chart as "", not as the nil uuid: the chart
// reads an empty branchId as "bootstrap a fresh branch" and the supervisor's
// env_opt discards empty values, and "00000000-…" passes both checks.
func TestKubernetesValuesUnsetBranchIDIsEmpty(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)
	spec := testEndpointSpec()
	spec.BranchID = uuid.Nil
	if got := k.values(spec, spec.Name).Endpoint.BranchId; got != "" {
		t.Errorf("Endpoint.BranchId = %q, want the empty sentinel", got)
	}
	if got := branchIDValue(testEndpointSpec().BranchID); got != testEndpointSpec().BranchID.String() {
		t.Errorf("a set branch id rendered as %q", got)
	}
}

func TestKubernetesValuesExtensions(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)
	spec := testEndpointSpec()
	spec.Extensions = []Extension{
		{Name: "pgvector", Image: "quay.io/yugabyte/amp-ext-pgvector:0.8.2", Preload: false},
		{Name: "pg_cron", Image: "quay.io/yugabyte/amp-ext-pg_cron:1.6.7", Preload: true},
		// Library differs from Name only for documentdb, and it is the value the
		// chart puts in shared_preload_libraries: preloading "documentdb" instead
		// of its two libraries is a `could not access file` FATAL at postmaster
		// start, so it has to survive the hop into chart values.
		// Settings reach the chart as-is: documentdb's name the database
		// compute_ctl creates it in.
		{Name: "documentdb", Image: "quay.io/yugabyte/amp-ext-documentdb:0.116-0-r3", Preload: true,
			Library:  "pg_documentdb_core,pg_documentdb",
			Settings: map[string]string{"cron.database_name": "documentdb"}},
	}
	v := k.values(spec, "yb-ep-rel")
	want := []extensionValues{
		{Name: "pgvector", Image: "quay.io/yugabyte/amp-ext-pgvector:0.8.2", Preload: false},
		{Name: "pg_cron", Image: "quay.io/yugabyte/amp-ext-pg_cron:1.6.7", Preload: true},
		{Name: "documentdb", Image: "quay.io/yugabyte/amp-ext-documentdb:0.116-0-r3", Preload: true,
			Library:  "pg_documentdb_core,pg_documentdb",
			Settings: map[string]string{"cron.database_name": "documentdb"}},
	}
	if !reflect.DeepEqual(v.Endpoint.Extensions, want) {
		t.Fatalf("endpoint.extensions = %+v, want %+v", v.Endpoint.Extensions, want)
	}
}

func TestKubernetesValuesThirdPartyExtensionLayout(t *testing.T) {
	// A third-party image (postgis, from CloudNativePG) lays its tree out
	// differently, bundles the system libraries its .so links, and was built
	// against stock PG18. All four of those reach the chart as values, or the pod
	// mounts a volume it cannot resolve anything in.
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)
	spec := testEndpointSpec()
	spec.Extensions = []Extension{{
		Name:           "postgis",
		Image:          "ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm",
		SharePath:      "share",
		LibPath:        "lib",
		LibraryPath:    []string{"system"},
		EnvPaths:       map[string]string{"PROJ_DATA": "share/proj"},
		ABIExtraExempt: []string{"postgis-3"},
	}}
	v := k.values(spec, "yb-ep-rel")
	want := []extensionValues{{
		Name:           "postgis",
		Image:          "ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm",
		SharePath:      "share",
		LibPath:        "lib",
		LibraryPath:    []string{"system"},
		EnvPaths:       map[string]string{"PROJ_DATA": "share/proj"},
		ABIExtraExempt: []string{"postgis-3"},
	}}
	if !reflect.DeepEqual(v.Endpoint.Extensions, want) {
		t.Fatalf("endpoint.extensions = %+v, want %+v", v.Endpoint.Extensions, want)
	}
}

func TestKubernetesValuesAmpExtensionOmitsLayoutKeys(t *testing.T) {
	// An amp-built extension declares no layout, and the marshaled values must
	// then carry none of the layout keys — the chart's defaults are the amp tree,
	// and an explicit empty string would override them with nothing.
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)
	spec := testEndpointSpec()
	spec.Extensions = []Extension{{Name: "pgvector", Image: "quay.io/yugabyte/amp-ext-pgvector:0.8.2"}}
	data, err := json.Marshal(k.values(spec, "yb-ep-rel"))
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	for _, key := range []string{"sharePath", "libPath", "libraryPath", "envPaths", "abiExtraExempt"} {
		if strings.Contains(string(data), key) {
			t.Errorf("values carry %q for an amp-built extension: %s", key, data)
		}
	}
}

func TestKubernetesValuesNoExtensionsOmitted(t *testing.T) {
	// With no extensions, the key must be omitted entirely so the chart's own
	// no-extensions default applies.
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)
	v := k.values(testEndpointSpec(), "yb-ep-rel")
	if v.Endpoint.Extensions != nil {
		t.Fatalf("endpoint.extensions = %+v, want nil (omitted)", v.Endpoint.Extensions)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	if strings.Contains(string(data), "\"extensions\"") {
		t.Errorf("marshaled values contain an extensions key, want it omitted: %s", data)
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

// TestKubernetesValuesTLS asserts the TLS mode turns into the chart's tls block
// plus a connection block naming the chart's fixed mount paths, and that a
// plaintext config emits neither.
func TestKubernetesValuesTLS(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)
	v := k.values(testEndpointSpec(), "yb-ep-rel")
	if v.TLS != nil || v.Yugabyte.Sslmode != "" || v.Yugabyte.Sslcert != "" {
		t.Errorf("plaintext config rendered TLS values: %+v %+v", v.TLS, v.Yugabyte)
	}

	cfg := testKubernetesConfig()
	cfg.BackingYBTLSMode = "verify-full"
	cfg.BackingYBTLSIssuerKind = "ClusterIssuer"
	cfg.BackingYBTLSIssuerName = "amp-ca"
	cfg.BackingYBTLSIssuerGroup = "cert-manager.io"
	cfg.BackingYBTLSCABundleConfigMap = "amp-ca-bundle"
	cfg.BackingYBTLSCertDuration = "720h"
	cfg.BackingYBTLSCertRenewBefore = "240h"
	v = NewKubernetesDeployer(cfg, nil).values(testEndpointSpec(), "yb-ep-rel")
	if v.TLS == nil || !v.TLS.Enabled {
		t.Fatalf("TLS block not rendered: %+v", v.TLS)
	}
	if v.TLS.IssuerRef != (issuerRefValues{Kind: "ClusterIssuer", Name: "amp-ca", Group: "cert-manager.io"}) {
		t.Errorf("IssuerRef = %+v", v.TLS.IssuerRef)
	}
	if v.TLS.CABundleConfigMap != "amp-ca-bundle" || v.TLS.Certificate != (certificateValue{Duration: "720h", RenewBefore: "240h"}) {
		t.Errorf("TLS = %+v", v.TLS)
	}
	want := yugabyteValues{
		Sslmode:     "verify-full",
		Sslrootcert: endpointTLSCAPath,
		Sslcert:     endpointTLSCertPath,
		Sslkey:      endpointTLSKeyPath,
	}
	got := yugabyteValues{Sslmode: v.Yugabyte.Sslmode, Sslrootcert: v.Yugabyte.Sslrootcert, Sslcert: v.Yugabyte.Sslcert, Sslkey: v.Yugabyte.Sslkey}
	if got != want {
		t.Errorf("connection TLS fields = %+v, want %+v", got, want)
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
	release := "yb-ep-92331eed-afb2-a0ef-3b2d-724d649bee78"

	want := fmt.Sprintf("%s.%s.svc.cluster.local:%d", release, cfg.Namespace, cfg.ComputePort)
	if got := k.address(release); got != want {
		t.Errorf("address(%s) = %q, want %q", release, got, want)
	}
}

// The recorded client address is a Service address in every case, never the
// pod's: a pod IP dies at the next reschedule and takes the connection string the
// console handed out with it (CLOUDGA-36993). The ClusterIP row that is handed an
// external address is the regression pin — the bug was a pod lookup wired into
// that branch.
func TestClientAddress(t *testing.T) {
	const dns = "yb-ep-92331eed.endpoints.svc.cluster.local:55433"
	for _, tc := range []struct {
		name        string
		serviceType string
		external    string
		want        string
	}{
		{"ClusterIP is the Service DNS", "ClusterIP", "", dns},
		{"ClusterIP ignores an external address", "ClusterIP", "10.5.150.209:55433", dns},
		{"LoadBalancer takes its ingress", serviceTypeLoadBalancer, "a1b2.elb.amazonaws.com:55433", "a1b2.elb.amazonaws.com:55433"},
		{"LoadBalancer without one falls back", serviceTypeLoadBalancer, "", dns},
		{"NodePort takes nodeIP:nodePort", serviceTypeNodePort, "34.1.2.3:31234", "34.1.2.3:31234"},
		{"NodePort without one falls back", serviceTypeNodePort, "", dns},
		{"an unset type is the Service DNS", "", "10.5.150.209:55433", dns},
	} {
		if got := clientAddress(tc.serviceType, dns, tc.external); got != tc.want {
			t.Errorf("%s: clientAddress(%q, _, %q) = %q, want %q", tc.name, tc.serviceType, tc.external, got, tc.want)
		}
	}
}

// EndpointSafekeeperURL addresses the endpoint's safekeeper at the same stable
// host the controller dials (CONTROLLER_REACHABLE_HOST): the Service DNS in k8s,
// the container name on Docker — both at the fixed safekeeper HTTP port.
func TestEndpointSafekeeperURL(t *testing.T) {
	id := "yb-ep-92331eed-afb2-a0ef-3b2d-724d649bee78"

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

// The create task's gate is a real round trip to the safekeeper's status route,
// so a 2xx from it — and nothing short of one — lets the task succeed (#724).
func TestAwaitSafekeeper(t *testing.T) {
	// Fast enough that the failure cases don't dominate the package's runtime;
	// several attempts still fit in the budget.
	const (
		budget   = 100 * time.Millisecond
		interval = 10 * time.Millisecond
	)

	t.Run("a serving safekeeper passes on the first attempt", func(t *testing.T) {
		var gotPath string
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			gotPath = r.URL.Path
		}))
		defer srv.Close()

		if err := awaitSafekeeper(context.Background(), srv.URL, budget, interval); err != nil {
			t.Fatalf("awaitSafekeeper = %v, want nil", err)
		}
		if gotPath != safekeeperStatusPath {
			t.Errorf("probed %q, want %q", gotPath, safekeeperStatusPath)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("probed %d times, want 1", n)
		}
	})

	// The window the gate exists for: the pod is Ready and the Service routes, but
	// the safekeeper is not serving yet. It has to be waited out, not failed on.
	t.Run("a safekeeper that starts serving mid-budget passes", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}))
		defer srv.Close()

		if err := awaitSafekeeper(context.Background(), srv.URL, budget, interval); err != nil {
			t.Fatalf("awaitSafekeeper = %v, want nil", err)
		}
		if n := calls.Load(); n != 3 {
			t.Errorf("probed %d times, want 3 (two refusals then a pass)", n)
		}
	})

	// The regression: an address that accepts nothing is what a Service with no
	// backend looks like from here, and it used to satisfy the gate because the
	// Endpoints object said the pod was ready and the Service declared the port.
	t.Run("an address nothing answers on fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		dead := srv.URL
		srv.Close()

		err := awaitSafekeeper(context.Background(), dead, budget, interval)
		if err == nil {
			t.Fatal("awaitSafekeeper on a dead address = nil, want an error")
		}
		// The message has to name what was dialed: the runbook's triage starts there.
		if !strings.Contains(err.Error(), dead+safekeeperStatusPath) {
			t.Errorf("error %q does not name the probed URL", err)
		}
	})

	t.Run("a safekeeper that never serves fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		err := awaitSafekeeper(context.Background(), srv.URL, budget, interval)
		if err == nil {
			t.Fatal("awaitSafekeeper against a 503 = nil, want an error")
		}
		if !strings.Contains(err.Error(), "503") {
			t.Errorf("error %q does not carry the last status", err)
		}
	})

	t.Run("a cancelled context stops the wait", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := awaitSafekeeper(ctx, srv.URL, time.Minute, interval); !errors.Is(err, context.Canceled) {
			t.Errorf("awaitSafekeeper with a cancelled context = %v, want context.Canceled", err)
		}
	})
}

// verifyReady gates the create task on an address only an in-cluster caller can
// resolve, so out of cluster it must be a no-op — otherwise every non-in-cluster
// deploy fails on a name that was never going to resolve.
func TestVerifyReadySkipsOutsideCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	k := NewKubernetesDeployer(testKubernetesConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := k.verifyReady(context.Background(), "yb-ep-1"); err != nil {
		t.Errorf("verifyReady outside a cluster = %v, want nil", err)
	}
}

// TestStartupProbeBudgetTracksDeployTimeout pins the coupling that #960 is
// about: the kubelet must not give up on a bootstrapping compute before the
// deployer does. A budget shorter than the deploy timeout kills the container
// mid-bootstrap, and raising --deploy-timeout achieves nothing because the
// shorter bound still wins.
//
// Budgets are checked through startupProbeBudgetSeconds rather than recomputed
// here: asserting against a formula written twice lets both copies be wrong
// together, which is how the period*(threshold-1) off-by-one survived its first
// round of tests.
func TestStartupProbeBudgetTracksDeployTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		want    int
	}{
		// The binary default, and the chart's standalone default: same budget.
		// With a 2s period, 600s of budget is 300 periods, +1 for the immediate
		// probe = 301.
		{"10m default", 10 * time.Minute, 301},
		// What QA actually runs. The chart's hardcoded 10m would have become the
		// binding constraint here — the whole point of deriving it. 2100s / 2s =
		// 1050 periods, +1 = 1051.
		{"35m as QA sets it", 35 * time.Minute, 1051},
		// Above the floor AND a whole number of periods: 11m is 330 periods
		// exactly (660s / 2s), +1 for the immediate probe = 331. A floor-division
		// mutant gives 331 too, so the case below is the one that pins rounding.
		{"exactly on a period boundary", 11 * time.Minute, 331},
		// 11m1s needs 331 periods (ceil of 661s / 2s), +1 = 332. Floor division
		// yields 331 and a budget of 660s — under the 661s timeout. This is the
		// rounding case.
		{"rounds up past a period boundary", 11*time.Minute + time.Second, 332},
		// A short timeout must not leave a compute worse off than a plain
		// `helm install`.
		{"floored at the chart default", time.Minute, 301},
		{"zero is the floor, not zero", 0, 301},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := startupProbeBudget(tc.timeout)
			if got.FailureThreshold != tc.want {
				t.Errorf("failureThreshold = %d, want %d", got.FailureThreshold, tc.want)
			}
			if budget := startupProbeBudgetSeconds(got); budget < tc.timeout {
				t.Errorf("probe budget %v is under the %v deploy timeout — the kubelet would "+
					"kill the container before the deployer gives up", budget, tc.timeout)
			}
		})
	}
}

// TestStartupProbeBudgetSecondsMatchesTheKubelet states the kubelet's own
// arithmetic once, against hand-computed values, so the helper the other tests
// lean on cannot drift.
//
// The prober calls doProbe immediately on entering its loop and only then waits
// on the period ticker, so N consecutive failures span N-1 periods: with
// period=10 and threshold=61 the 61st failure lands at t=600s, not t=610s.
func TestStartupProbeBudgetSecondsMatchesTheKubelet(t *testing.T) {
	for _, tc := range []struct {
		threshold int
		want      time.Duration
	}{
		{61, 600 * time.Second},
		{211, 2100 * time.Second},
		{2, 10 * time.Second},
	} {
		v := startupProbeValues{PeriodSeconds: 10, FailureThreshold: tc.threshold}
		if got := startupProbeBudgetSeconds(v); got != tc.want {
			t.Errorf("budget(threshold=%d) = %v, want %v", tc.threshold, got, tc.want)
		}
	}
}

// TestStartupProbeOutlastsTheLivenessWindow: liveness is sized for a RUNNING
// compute (30s delay + 15s period * 4 = 90s). The startup budget has to exceed
// that or the change buys nothing — not because liveness would take over
// (it cannot; it stays suspended until the startupProbe first succeeds) but
// because the startupProbe would then be the thing killing the container, and
// sooner than the probe it replaced.
func TestStartupProbeOutlastsTheLivenessWindow(t *testing.T) {
	const livenessWindow = 90 * time.Second // 30 initialDelay + 15 period * 4 threshold
	if budget := startupProbeBudgetSeconds(startupProbeBudget(0)); budget <= livenessWindow {
		t.Errorf("floor budget %v does not outlast the %v liveness window: the startupProbe "+
			"would kill the container earlier than the probe it replaces", budget, livenessWindow)
	}
}

// TestProgressDeadlineOutlastsStartupBudget: the Deployment's give-up clock must
// not fire while the kubelet is still waiting. Kubernetes defaults it to 600s,
// so any startup budget past 10m needs this raised with it or `helm --wait` and
// the harness's condition=Available check call a healthy bootstrap a failure.
func TestProgressDeadlineOutlastsStartupBudget(t *testing.T) {
	for _, timeout := range []time.Duration{0, 10 * time.Minute, 35 * time.Minute} {
		budget := startupProbeBudgetSeconds(startupProbeBudget(timeout))
		deadline := time.Duration(progressDeadlineFor(timeout)) * time.Second
		if deadline <= budget {
			t.Errorf("timeout %v: progressDeadline %v does not outlast the %v startup budget",
				timeout, deadline, budget)
		}
		if deadline < timeout {
			t.Errorf("timeout %v: progressDeadline %v is under the deploy timeout", timeout, deadline)
		}
	}
}

// TestKubernetesValuesStartupProbeFollowsConfiguredTimeout closes the gap the
// pure-function tests leave: they pin startupProbeBudget's arithmetic, but not
// that values() feeds it the DEPLOYER'S timeout. A hardcoded budget passes those
// and still ships the #960 bug, because raising --deploy-timeout would no longer
// move the bound the kubelet enforces.
func TestKubernetesValuesStartupProbeFollowsConfiguredTimeout(t *testing.T) {
	cfg := testKubernetesConfig()
	cfg.Timeout = 35 * time.Minute // what QA runs; the chart's own default is 10m
	k := NewKubernetesDeployer(cfg, nil)

	v := k.values(testEndpointSpec(), "rel")

	want := startupProbeBudget(cfg.Timeout)
	if v.StartupProbe != want {
		t.Errorf("StartupProbe = %+v, want %+v (derived from cfg.Timeout)", v.StartupProbe, want)
	}
	budget := startupProbeBudgetSeconds(v.StartupProbe)
	if budget < cfg.Timeout {
		t.Errorf("probe budget %v is under the configured %v deploy timeout", budget, cfg.Timeout)
	}
	// The bug in miniature: the chart's standalone default would be 10m here.
	if budget == 10*time.Minute {
		t.Error("budget did not move with cfg.Timeout — it is pinned to the chart default")
	}
	// The Deployment's own clock has to move with it, or the rollout is failed
	// while the kubelet is still waiting.
	if deadline := time.Duration(v.ProgressDeadlineSeconds) * time.Second; deadline <= budget {
		t.Errorf("progressDeadline %v does not outlast the %v startup budget", deadline, budget)
	}
}

// values() must pass a per-endpoint connectivity mode and its source ranges to
// the chart: they gate service-lb.yaml, so dropping either silently downgrades
// an nlb_* endpoint to cluster_ip.
func TestKubernetesValuesConnectivity(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)

	spec := testEndpointSpec()
	spec.Connectivity = ConnectivityNLBInternal
	spec.AllowedCIDRs = []string{"10.0.0.0/16", "192.168.1.0/24"}

	v := k.values(spec, "yb-ep-rel")
	if v.Service.Connectivity != ConnectivityNLBInternal {
		t.Errorf("Service.Connectivity = %q, want %q", v.Service.Connectivity, ConnectivityNLBInternal)
	}
	if strings.Join(v.Service.AllowedCidrs, ",") != "10.0.0.0/16,192.168.1.0/24" {
		t.Errorf("Service.AllowedCidrs = %v, want both CIDRs in order", v.Service.AllowedCidrs)
	}
	// The deployment-wide Service type is independent of the per-endpoint choice.
	if v.Service.Type != testKubernetesConfig().ServiceType {
		t.Errorf("Service.Type = %q, want the deployment-wide default unchanged", v.Service.Type)
	}
}

// The default endpoint sends no connectivity, and the chart's gate must then be
// false — an empty string, not "cluster_ip" — so nothing renders the LB Service.
func TestKubernetesValuesConnectivityDefaultsEmpty(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)
	v := k.values(testEndpointSpec(), "yb-ep-rel")
	if v.Service.Connectivity != "" {
		t.Errorf("Service.Connectivity = %q, want empty so service-lb.yaml does not render", v.Service.Connectivity)
	}
	if v.Service.AllowedCidrs != nil {
		t.Errorf("Service.AllowedCidrs = %v, want nil", v.Service.AllowedCidrs)
	}
}

// Only the Kubernetes deployer can honour an nlb_* mode; the API keys its 400 off
// this interface, so the assertion must keep holding.
func TestKubernetesDeployerSupportsConnectivity(t *testing.T) {
	var d Deployer = NewKubernetesDeployer(testKubernetesConfig(), nil)
	cs, ok := d.(ConnectivitySupporter)
	if !ok {
		t.Fatal("KubernetesDeployer must implement ConnectivitySupporter")
	}
	if !cs.SupportsConnectivity() {
		t.Error("SupportsConnectivity() = false, want true")
	}
}

// The two classifiers partition the modes, and the API branches on them: an
// ingress mode must never be mistaken for one that provisions a load balancer of
// its own, since that is the per-endpoint bill shared ingress exists to remove.
func TestConnectivityModeClassifiers(t *testing.T) {
	for _, tc := range []struct {
		mode        string
		ingress     bool
		dedicatedLB bool
	}{
		{ConnectivityClusterIP, false, false},
		{ConnectivityNLBInternal, false, true},
		{ConnectivityNLBExternal, false, true},
		{ConnectivityIngressInternal, true, false},
		{ConnectivityIngressExternal, true, false},
		{"", false, false},
		{"nlb_ingress", false, false}, // neither, not both
	} {
		if got := IsIngress(tc.mode); got != tc.ingress {
			t.Errorf("IsIngress(%q) = %v, want %v", tc.mode, got, tc.ingress)
		}
		if got := IsDedicatedLB(tc.mode); got != tc.dedicatedLB {
			t.Errorf("IsDedicatedLB(%q) = %v, want %v", tc.mode, got, tc.dedicatedLB)
		}
		if IsIngress(tc.mode) && IsDedicatedLB(tc.mode) {
			t.Errorf("%q classified as both ingress and dedicated-LB", tc.mode)
		}
	}
}

// SupportsIngress reports the deployment's switch and nothing else — the API
// keys its 400 off this interface, so an opt-in that defaulted on would deploy
// endpoints nobody can reach.
func TestKubernetesDeployerSupportsIngress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{"off by default", false},
		{"enabled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testKubernetesConfig()
			cfg.IngressEnabled = tc.enabled
			var d Deployer = NewKubernetesDeployer(cfg, nil)
			is, ok := d.(IngressSupporter)
			if !ok {
				t.Fatal("KubernetesDeployer must implement IngressSupporter")
			}
			if got := is.SupportsIngress(); got != tc.enabled {
				t.Errorf("SupportsIngress() = %v, want %v", got, tc.enabled)
			}
		})
	}
}

// The chart cannot render a route it is not told about, so a dropped
// service.ingress is a silently unrouted endpoint — the same failure mode the
// connectivity values test exists for, and the reason both are pinned rather
// than trusted to the struct tags.
func TestKubernetesValuesIngress(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)

	spec := testEndpointSpec()
	spec.Connectivity = ConnectivityIngressInternal
	spec.Ingress = IngressSlot{
		Shard: "pgin-0", Port: 20007,
		DNSName: "ep-abc.int.example", ShardHost: "lb.example", RecordType: "CNAME",
	}
	v := k.values(spec, "yb-ep-1")
	if v.Service.Ingress == nil {
		t.Fatal("service.ingress is nil for an ingress mode; the chart renders no route and the " +
			"endpoint answers nowhere while the API reports the mode")
	}
	got := *v.Service.Ingress
	if got.Shard != "pgin-0" || got.Port != 20007 || got.DNSName != "ep-abc.int.example" {
		t.Errorf("service.ingress = %+v, want the allocated slot", got)
	}
	// The route names the entryPoint the shard declares. Two spellings of the
	// same thing is a route Traefik accepts and never serves.
	if got.EntryPoint != EntryPointName(20007) {
		t.Errorf("entryPoint = %q, want %q", got.EntryPoint, EntryPointName(20007))
	}
	if got.ShardHost != "lb.example" || got.RecordType != "CNAME" {
		t.Errorf("shard address = %q/%q, want it carried for the DNS record", got.ShardHost, got.RecordType)
	}

	// And omitted entirely for a mode with no slot, so the key is absent from the
	// marshaled values rather than present and empty.
	for _, mode := range []string{ConnectivityClusterIP, ConnectivityNLBInternal, ConnectivityNLBExternal} {
		plain := testEndpointSpec()
		plain.Connectivity = mode
		if got := k.values(plain, "yb-ep-1").Service.Ingress; got != nil {
			t.Errorf("mode %q rendered service.ingress = %+v, want nil", mode, got)
		}
	}
}

// EntryPointName is spelled by both charts; if it ever stopped agreeing with the
// shard's `--entryPoints.pg-<port>.address`, every route would name one that does
// not exist — accepted by Traefik, and then silently serving nothing.
func TestEntryPointName(t *testing.T) {
	for _, tc := range []struct {
		port int
		want string
	}{
		{20000, "pg-20000"},
		{20049, "pg-20049"},
		{5432, "pg-5432"},
	} {
		if got := EntryPointName(tc.port); got != tc.want {
			t.Errorf("EntryPointName(%d) = %q, want %q", tc.port, got, tc.want)
		}
	}
}

// PlacesIngress is what opens the API to the ingress_* modes, so it must be true
// exactly where CreateEndpoint can render a route — which is this commit onward.
func TestKubernetesDeployerPlacesIngress(t *testing.T) {
	var d Deployer = NewKubernetesDeployer(testKubernetesConfig(), nil)
	p, ok := d.(IngressPlacer)
	if !ok {
		t.Fatal("KubernetesDeployer must implement IngressPlacer once it renders routes")
	}
	if !p.PlacesIngress() {
		t.Error("PlacesIngress() = false; the API would refuse the ingress_* modes even though " +
			"the deploy path can now place them")
	}
}

// The shard chart writes its entryPoint names in Go template and never calls
// EntryPointName, so the two spellings are held equal here rather than by
// construction. Reading the chart is the point: changing the format string
// builds, renders and installs clean, while every route then names an entryPoint
// that does not exist — accepted by Traefik and silently never served.
func TestEntryPointNameMatchesTheShardChart(t *testing.T) {
	const chart = "../../../deploy/helm/yb-amp-ingress/templates/deployment.yaml"
	b, err := os.ReadFile(chart)
	if err != nil {
		t.Fatalf("read the shard chart: %v", err)
	}
	// The chart renders one entryPoint per port as `--entryPoints.pg-{{ $p }}...`.
	// Turn its literal prefix into what EntryPointName must produce.
	const want = "--entryPoints.pg-{{ $p }}.address=:{{ $p }}"
	if !strings.Contains(string(b), want) {
		t.Fatalf("the shard chart no longer renders %q; if its entryPoint spelling changed, "+
			"EntryPointName must change with it or every route names one that does not exist", want)
	}
	// And the Go side must produce exactly that shape for a concrete port.
	if got := EntryPointName(20007); got != "pg-20007" {
		t.Errorf("EntryPointName(20007) = %q, want pg-20007 to match the chart's pg-{{ $p }}", got)
	}
}

// Publishability is decided by the stored name, not by the caller, because the
// caller differs: a create passes what the allocator returned, while every
// redeploy assembles the spec from the allocation row. When only the create path
// blanked an unpublished name, a resize or extension roll of a zone-less
// endpoint took the bare name as its address and SetEndpointAddress overwrote a
// working <shardHost>:<port> with one that resolves nowhere.
func TestPublishableDNSName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"ep-abc12345.int.pg.example", true},
		{"ep-abc12345", false}, // minted where the family had no zone
		{"", false},
	} {
		if got := PublishableDNSName(tc.name); got != tc.want {
			t.Errorf("PublishableDNSName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The redeploy path is the one that was broken: it renders from the allocation
// row, which stores the bare name, so it must reach the same answer as a create.
func TestIngressValuesAndAddressAgreeOnAnUnpublishedName(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)

	spec := testEndpointSpec()
	spec.Connectivity = ConnectivityIngressInternal
	spec.Ingress = IngressSlot{
		Shard: "pgin-0", Port: 20007,
		DNSName:   "ep-abc12345", // what the row holds on a zone-less deployment
		ShardHost: "lb.example", RecordType: "CNAME",
	}

	// The chart must render no DNS record for a name nothing resolves.
	v := k.values(spec, "yb-ep-1")
	if v.Service.Ingress == nil {
		t.Fatal("service.ingress is nil for an ingress mode")
	}
	if got := v.Service.Ingress.DNSName; got != "" {
		t.Errorf("service.ingress.dnsName = %q, want empty: rendering a DNSEndpoint for an "+
			"unqualified name hands external-dns a record outside any zone", got)
	}

	// And a published name survives untouched.
	spec.Ingress.DNSName = "ep-abc12345.int.pg.example"
	if got := k.values(spec, "yb-ep-1").Service.Ingress.DNSName; got != "ep-abc12345.int.pg.example" {
		t.Errorf("published dnsName = %q, want it carried through", got)
	}
}

// The gateway's tag must be resolved by the controller, not left to the chart:
// an empty tag makes the chart fall back to its own appVersion (0.1.0), which is
// never what CI built. kubernetes.go records that trap for the endpoint image;
// the sidecar shares it, and an unpullable sidecar fails the whole pod.
func TestKubernetesValuesDocumentDBGatewayImage(t *testing.T) {
	cfg := testKubernetesConfig()
	cfg.EndpointImageTag = "commit-abc"
	cfg.DocumentDBGatewayImageRepo = "quay.io/yugabyte/amp-documentdb-gateway"
	k := NewKubernetesDeployer(cfg, nil)

	spec := testEndpointSpec()
	spec.Extensions = []Extension{{Name: "documentdb", Image: "quay.io/yugabyte/amp-ext-documentdb:0.116-0", Preload: true,
		Library: "pg_documentdb_core,pg_documentdb"}}
	v := k.values(spec, "yb-ep-rel")
	if v.DocumentDBGateway == nil {
		t.Fatal("documentdbGateway values absent for an endpoint that mounts documentdb")
	}
	// Empty gateway tag follows the endpoint's, so the pair always comes from one
	// commit rather than the chart's appVersion.
	want := imageValues{Repository: "quay.io/yugabyte/amp-documentdb-gateway", Tag: "commit-abc"}
	if v.DocumentDBGateway.Image != want {
		t.Errorf("gateway image = %+v, want %+v", v.DocumentDBGateway.Image, want)
	}

	// An explicit tag wins over the endpoint's.
	cfg.DocumentDBGatewayImageTag = "gw-v9"
	v = NewKubernetesDeployer(cfg, nil).values(spec, "yb-ep-rel")
	if got := v.DocumentDBGateway.Image.Tag; got != "gw-v9" {
		t.Errorf("gateway tag = %q, want gw-v9", got)
	}
}

// An endpoint without documentdb sends no gateway block at all, so the chart's
// own helper is the only thing deciding whether the sidecar renders.
func TestKubernetesValuesNoDocumentDBGatewayWithoutTheExtension(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)
	spec := testEndpointSpec()
	spec.Extensions = []Extension{{Name: "pgvector", Image: "quay.io/yugabyte/amp-ext-pgvector:0.8.2"}}
	if v := k.values(spec, "yb-ep-rel"); v.DocumentDBGateway != nil {
		t.Errorf("documentdbGateway = %+v, want nil for an endpoint without documentdb", v.DocumentDBGateway)
	}
}

// documentdb_address is the endpoint's own address on the gateway port, which only
// resolves to the sidecar when Address is the pod's. Under NodePort the address
// carries the nodePort allocated for the COMPUTE port and the gateway port gets a
// different one; under an NLB the load balancer publishes pg-compute alone. In
// both cases the derived address names a port nothing listens on, so the API
// must report none.
func TestKubernetesDocumentDBGatewayReachable(t *testing.T) {
	for _, tc := range []struct {
		serviceType  string
		connectivity string
		want         bool
	}{
		{"", "", true},
		{"ClusterIP", "", true},
		{"ClusterIP", ConnectivityClusterIP, true},
		{"ClusterIP", ConnectivityNLBInternal, false},
		{"ClusterIP", ConnectivityNLBExternal, false},
		// The ingress shard routes pg-compute only, like the NLB.
		{"ClusterIP", ConnectivityIngressInternal, false},
		{"ClusterIP", ConnectivityIngressExternal, false},
		{serviceTypeNodePort, "", false},
		{serviceTypeNodePort, ConnectivityClusterIP, false},
		{serviceTypeLoadBalancer, "", false},
	} {
		cfg := testKubernetesConfig()
		cfg.ServiceType = tc.serviceType
		got := NewKubernetesDeployer(cfg, nil).DocumentDBGatewayReachable(tc.connectivity)
		if got != tc.want {
			t.Errorf("DocumentDBGatewayReachable(serviceType=%q, connectivity=%q) = %v, want %v",
				tc.serviceType, tc.connectivity, got, tc.want)
		}
	}
}
