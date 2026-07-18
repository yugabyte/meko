package ybk8s

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// --- pure helpers ---

func TestNamespaceFor(t *testing.T) {
	if got := namespaceFor("ampqa-yb", "run7"); got != "ampqa-yb-run7" {
		t.Errorf("namespaceFor = %q", got)
	}
	if got := namespaceFor("", "run7"); got != "ampqa-yb-run7" {
		t.Errorf("namespaceFor default prefix = %q", got)
	}
}

func TestChartS3URL(t *testing.T) {
	got := chartS3URL("2025.2.1.0-b141")
	want := "s3://releases.yugabyte.com/2025.2.1.0-b141/helm/yugabyte-2025.2.1.tgz"
	if got != want {
		t.Errorf("chartS3URL = %q, want %q", got, want)
	}
	if s := shortVersion("2025.2.1.0-b141"); s != "2025.2.1" {
		t.Errorf("shortVersion = %q, want 2025.2.1", s)
	}
	if s := shortVersion("2.20"); s != "2.20" {
		t.Errorf("shortVersion short input = %q", s)
	}
}

func TestHelmReleaseFor(t *testing.T) {
	if r := helmReleaseFor("", "ampqa-yb-run7"); r != "ampqa-yb-run7" {
		t.Errorf("helmReleaseFor derived = %q", r)
	}
	if r := helmReleaseFor("custom", "ns"); r != "custom" {
		t.Errorf("helmReleaseFor explicit = %q", r)
	}
}

func TestFirstHostPort(t *testing.T) {
	if h, p := FirstHostPort("10.1.2.3:5433,10.1.2.4:5433"); h != "10.1.2.3" || p != 5433 {
		t.Errorf("FirstHostPort = %s/%d", h, p)
	}
	if h, p := FirstHostPort("bare-host"); h != "bare-host" || p != DefaultYSQLPort {
		t.Errorf("FirstHostPort bare = %s/%d, want default port", h, p)
	}
}

func TestFromConfigChartAndImageFallback(t *testing.T) {
	cfg := FromConfig(fakeGet{m: map[string]any{"versions.yugabytedb": "2025.2.1.0"}})
	if cfg.ChartVersion != "2025.2.1.0" {
		t.Errorf("ChartVersion fallback = %q", cfg.ChartVersion)
	}
	if cfg.ImageTag != "2025.2.1.0" {
		t.Errorf("ImageTag fallback to chart version = %q", cfg.ImageTag)
	}
	// Explicit chart_version + image_tag win.
	cfg2 := FromConfig(fakeGet{m: map[string]any{
		"apps.ybk8s.chart_version": "2025.2.1.0-b141",
		"apps.ybk8s.image_tag":     "quay-tag-9",
		"versions.yugabytedb":      "2025.2.1.0",
	}})
	if cfg2.ChartVersion != "2025.2.1.0-b141" || cfg2.ImageTag != "quay-tag-9" {
		t.Errorf("explicit chart/image = %q/%q", cfg2.ChartVersion, cfg2.ImageTag)
	}
}

func TestApplyDefaultsClampsReplicasToRF(t *testing.T) {
	cfg := Config{ChartVersion: "v", ReplicationFactor: 3}
	applyDefaults(&cfg)
	if cfg.MasterReplicas != 3 || cfg.TServerReplicas != 3 {
		t.Errorf("replicas = %d/%d, want 3/3 (clamped to RF)", cfg.MasterReplicas, cfg.TServerReplicas)
	}
	// Explicit replica counts survive.
	cfg2 := Config{ChartVersion: "v", ReplicationFactor: 3, MasterReplicas: 1, TServerReplicas: 5}
	applyDefaults(&cfg2)
	if cfg2.MasterReplicas != 1 || cfg2.TServerReplicas != 5 {
		t.Errorf("explicit replicas overridden = %d/%d", cfg2.MasterReplicas, cfg2.TServerReplicas)
	}
	// Zero RF → default 1.
	cfg3 := Config{ChartVersion: "v"}
	applyDefaults(&cfg3)
	if cfg3.ReplicationFactor != 1 || cfg3.MasterReplicas != 1 || cfg3.TServerReplicas != 1 {
		t.Errorf("default RF cheap = rf%d m%d t%d, want 1/1/1", cfg3.ReplicationFactor, cfg3.MasterReplicas, cfg3.TServerReplicas)
	}
}

// --- Deploy happy path (RF1 default), fakes only ---

func TestDeployRF1DefaultBuildsBothPaths(t *testing.T) {
	chart := makeChartTGZ(t)
	rr := &recordRunner{podIPs: "10.1.2.3", nsLabel: ManagedByValue}

	cfg := FromConfig(fakeGet{m: map[string]any{}})
	cfg.ChartVersion = "2025.2.1.0-b141"

	info, err := New(cfg, "run7").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-yb-run7"}).WithRunner(rr.run)).
		WithHelmRunner(rr.run).
		WithDownload(func(context.Context, string, string) (string, error) { return chart, nil }).
		WithLogf(nil).
		Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// (a) in-cluster DSN for amp — the tserver Service's cluster-internal DNS.
	if info.InClusterHostPorts != "yb-tserver-service.ampqa-yb-run7.svc.cluster.local:5433" {
		t.Errorf("InClusterHostPorts = %q", info.InClusterHostPorts)
	}
	// (b) tailnet endpoint for the framework — the tserver pod IP on :5433.
	if len(info.TailnetAddrs) != 1 || info.TailnetAddrs[0] != "10.1.2.3:5433" {
		t.Errorf("TailnetAddrs = %v, want [10.1.2.3:5433]", info.TailnetAddrs)
	}
	if info.Namespace != "ampqa-yb-run7" || info.HelmRelease != "ampqa-yb-run7" {
		t.Errorf("namespace/release = %q/%q", info.Namespace, info.HelmRelease)
	}
	if info.Version != "2025.2.1.0-b141" || info.User != DefaultUser {
		t.Errorf("version/user = %q/%q", info.Version, info.User)
	}

	// The helm install argv carried the RF1 replica counts, the RF gflag, the
	// image, the storage class, ClusterIP-only services, --wait and the namespace.
	helm := rr.find("helm", "install")
	if helm == nil {
		t.Fatalf("no helm install call recorded; calls: %v", rr.calls)
	}
	joined := strings.Join(helm, " ")
	for _, want := range []string{
		"--wait", "--timeout", "ampqa-yb-run7",
		"replicas.master=1", "replicas.tserver=1", "gflags.master.replication_factor=1",
		"Image.repository=yugabytedb/yugabyte", "Image.tag=2025.2.1.0-b141",
		"storage.tserver.storageClass=standard", "enableLoadBalancer=false",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("helm install missing %q in: %s", want, joined)
		}
	}
}

func TestDeployRF3SetsThreeReplicas(t *testing.T) {
	chart := makeChartTGZ(t)
	rr := &recordRunner{podIPs: "10.1.2.3 10.1.2.4 10.1.2.5", nsLabel: ManagedByValue}

	cfg := FromConfig(fakeGet{m: map[string]any{"apps.ybk8s.replication_factor": 3}})
	cfg.ChartVersion = "2025.2.1.0-b141"

	info, err := New(cfg, "run7").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-yb-run7"}).WithRunner(rr.run)).
		WithHelmRunner(rr.run).
		WithDownload(func(context.Context, string, string) (string, error) { return chart, nil }).
		WithLogf(nil).
		Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(info.TailnetAddrs) != 3 {
		t.Errorf("TailnetAddrs = %v, want 3 tserver addresses", info.TailnetAddrs)
	}
	helm := rr.find("helm", "install")
	joined := strings.Join(helm, " ")
	for _, want := range []string{"replicas.master=3", "replicas.tserver=3", "gflags.master.replication_factor=3"} {
		if !strings.Contains(joined, want) {
			t.Errorf("RF3 helm install missing %q in: %s", want, joined)
		}
	}
	// The readiness wait targets the tserver statefulset at 3 ready replicas.
	if w := rr.find("wait", "statefulset/yb-tserver"); w == nil || !strings.Contains(strings.Join(w, " "), "readyReplicas}=3") {
		t.Errorf("readiness wait not at 3 replicas; calls: %v", rr.calls)
	}
}

func TestDeployRequiresChartVersion(t *testing.T) {
	rr := &recordRunner{}
	d := New(Config{}, "run7").
		WithKube(k8s.New(k8s.Config{}).WithRunner(rr.run)).
		WithHelmRunner(rr.run).
		WithLogf(nil)
	d.cfg.ChartVersion = ""
	if _, err := d.Deploy(context.Background()); err == nil {
		t.Fatalf("expected error without a chart version")
	}
}

// --- private-registry image-pull secret ---

func TestDockerConfigJSONB64(t *testing.T) {
	// The quay/user:pass form: auth = base64("<user>:<pass>") base64'd WHOLE,
	// NOT the ECR base64("AWS:"+token) form.
	b64, err := dockerConfigJSONB64("quay.io", "proj:tok")
	if err != nil {
		t.Fatalf("dockerConfigJSONB64: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode dockerconfigjson: %v", err)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth  string `json:"auth"`
			Email string `json:"email"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal dockerconfigjson: %v", err)
	}
	want := base64.StdEncoding.EncodeToString([]byte("proj:tok"))
	if cfg.Auths["quay.io"].Auth != want {
		t.Errorf("auths[quay.io].auth = %q, want base64(token) %q", cfg.Auths["quay.io"].Auth, want)
	}
	// Empty registry defaults to quay.io.
	b2, err := dockerConfigJSONB64("", "proj:tok")
	if err != nil {
		t.Fatalf("dockerConfigJSONB64 default registry: %v", err)
	}
	raw2, _ := base64.StdEncoding.DecodeString(b2)
	if !strings.Contains(string(raw2), "quay.io") {
		t.Errorf("empty registry did not default to quay.io: %s", raw2)
	}
	// Empty token is an error (never emit a credential-less secret).
	if _, err := dockerConfigJSONB64("quay.io", "  "); err == nil {
		t.Errorf("expected error for empty token")
	}
}

// TestDeployAppliesPullSecretWhenConfigured: with image_pull_secret set and a
// resolver wired, Deploy resolves the token, applies the dockerconfigjson Secret
// into the namespace, and points the chart at it via Image.pullSecretName.
func TestDeployAppliesPullSecretWhenConfigured(t *testing.T) {
	chart := makeChartTGZ(t)
	rr := &recordRunner{podIPs: "10.1.2.3", nsLabel: ManagedByValue}

	cfg := FromConfig(fakeGet{m: map[string]any{
		"apps.ybk8s.image_pull_secret": "quay_images",
		"apps.ybk8s.image_registry":    "quay.io/yugabyte/yugabyte-itest",
	}})
	cfg.ChartVersion = "2026.1.1.0-b48"
	cfg.ImageTag = "2026.1.1.0-b48"

	resolvedName := ""
	_, err := New(cfg, "run7").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-yb-run7"}).WithRunner(rr.run)).
		WithHelmRunner(rr.run).
		WithDownload(func(context.Context, string, string) (string, error) { return chart, nil }).
		WithLogf(nil).
		WithSecretResolver(func(name string) (string, error) { resolvedName = name; return "proj:tok", nil }).
		Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	if resolvedName != "quay_images" {
		t.Errorf("resolver called with %q, want quay_images", resolvedName)
	}
	// The dockerconfigjson Secret was applied to the run's namespace under the
	// default object name, carrying the docker-registry type and the encoded auth.
	if !rr.appliedContains("type: kubernetes.io/dockerconfigjson") {
		t.Errorf("no dockerconfigjson pull secret applied; calls: %v", rr.calls)
	}
	if !rr.appliedContains("name: yb-pull-secret") || !rr.appliedContains("namespace: ampqa-yb-run7") {
		t.Errorf("pull secret not applied under expected name/namespace")
	}
	wantB64, _ := dockerConfigJSONB64("quay.io", "proj:tok")
	if !rr.appliedContains(".dockerconfigjson: " + wantB64) {
		t.Errorf("pull secret body did not match the built dockerconfigjson")
	}
	// The chart is pointed at the in-namespace secret.
	helm := rr.find("helm", "install")
	if helm == nil || !strings.Contains(strings.Join(helm, " "), "Image.pullSecretName=yb-pull-secret") {
		t.Errorf("helm install missing Image.pullSecretName; calls: %v", rr.calls)
	}
}

// TestDeployPullSecretNameAndRegistryConfigurable: the object name and the
// registry the token authenticates are driven by config.
func TestDeployPullSecretNameAndRegistryConfigurable(t *testing.T) {
	chart := makeChartTGZ(t)
	rr := &recordRunner{podIPs: "10.1.2.3", nsLabel: ManagedByValue}

	cfg := FromConfig(fakeGet{m: map[string]any{
		"apps.ybk8s.image_pull_secret":      "reg_token",
		"apps.ybk8s.image_pull_registry":    "registry.example.com",
		"apps.ybk8s.image_pull_secret_name": "custom-pull",
	}})
	cfg.ChartVersion = "2026.1.1.0-b48"
	cfg.ImageTag = "2026.1.1.0-b48"

	_, err := New(cfg, "run7").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-yb-run7"}).WithRunner(rr.run)).
		WithHelmRunner(rr.run).
		WithDownload(func(context.Context, string, string) (string, error) { return chart, nil }).
		WithLogf(nil).
		WithSecretResolver(func(string) (string, error) { return "proj:tok", nil }).
		Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !rr.appliedContains("name: custom-pull") {
		t.Errorf("pull secret not applied under configured name custom-pull")
	}
	wantB64, _ := dockerConfigJSONB64("registry.example.com", "proj:tok")
	if !rr.appliedContains(".dockerconfigjson: " + wantB64) {
		t.Errorf("pull secret body did not use the configured registry")
	}
	helm := rr.find("helm", "install")
	if helm == nil || !strings.Contains(strings.Join(helm, " "), "Image.pullSecretName=custom-pull") {
		t.Errorf("helm install missing Image.pullSecretName=custom-pull")
	}
}

// TestDeployNoPullSecretByDefault: the default (empty image_pull_secret) path is
// unchanged — no resolver call, no Secret applied, no Image.pullSecretName set —
// even when a resolver is wired.
func TestDeployNoPullSecretByDefault(t *testing.T) {
	chart := makeChartTGZ(t)
	rr := &recordRunner{podIPs: "10.1.2.3", nsLabel: ManagedByValue}

	cfg := FromConfig(fakeGet{m: map[string]any{}})
	cfg.ChartVersion = "2025.2.1.0-b141"

	resolverCalled := false
	_, err := New(cfg, "run7").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-yb-run7"}).WithRunner(rr.run)).
		WithHelmRunner(rr.run).
		WithDownload(func(context.Context, string, string) (string, error) { return chart, nil }).
		WithLogf(nil).
		WithSecretResolver(func(string) (string, error) { resolverCalled = true; return "x", nil }).
		Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if resolverCalled {
		t.Errorf("resolver called on the public-image path")
	}
	if rr.appliedContains("type: kubernetes.io/dockerconfigjson") {
		t.Errorf("a pull secret was applied on the public-image path")
	}
	helm := rr.find("helm", "install")
	if helm != nil && strings.Contains(strings.Join(helm, " "), "Image.pullSecretName") {
		t.Errorf("public-image helm install set Image.pullSecretName")
	}
}

// TestDeployPullSecretRequiresResolver: a configured image_pull_secret with no
// resolver wired is a clear Deploy error (not a silent public-image fallback).
func TestDeployPullSecretRequiresResolver(t *testing.T) {
	chart := makeChartTGZ(t)
	rr := &recordRunner{podIPs: "10.1.2.3", nsLabel: ManagedByValue}

	cfg := FromConfig(fakeGet{m: map[string]any{"apps.ybk8s.image_pull_secret": "quay_images"}})
	cfg.ChartVersion = "2026.1.1.0-b48"
	cfg.ImageTag = "2026.1.1.0-b48"

	_, err := New(cfg, "run7").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-yb-run7"}).WithRunner(rr.run)).
		WithHelmRunner(rr.run).
		WithDownload(func(context.Context, string, string) (string, error) { return chart, nil }).
		WithLogf(nil).
		Deploy(context.Background())
	if err == nil {
		t.Fatalf("expected error when image_pull_secret is set but no resolver is wired")
	}
}

// --- Teardown ---

func TestTeardownDeletesNamespaceAndPVCs(t *testing.T) {
	rr := &recordRunner{
		nsLabel: ManagedByValue,
		pvcs:    "yb-master-data-yb-master-0 yb-tserver-data-yb-tserver-0",
	}
	d := New(Config{}, "run7").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-yb-run7"}).WithRunner(rr.run)).
		WithHelmRunner(rr.run).
		WithLogf(nil)
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !rr.saw("uninstall", "ampqa-yb-run7") {
		t.Errorf("teardown did not helm-uninstall the release; calls: %v", rr.calls)
	}
	// The StatefulSet PVCs (the leak vector) are explicitly deleted, found via
	// the chart-stamped release label — NOT app=yb-master/app=yb-tserver, which
	// the chart's volumeClaimTemplates never set (see deletePVCs).
	if !rr.saw("get", "pvc", "-l", "release=ampqa-yb-run7") {
		t.Errorf("teardown did not list PVCs by release label; calls: %v", rr.calls)
	}
	if !rr.saw("delete", "persistentvolumeclaim", "yb-master-data-yb-master-0") {
		t.Errorf("master PVC not deleted; calls: %v", rr.calls)
	}
	if !rr.saw("delete", "persistentvolumeclaim", "yb-tserver-data-yb-tserver-0") {
		t.Errorf("tserver PVC not deleted; calls: %v", rr.calls)
	}
	if !rr.saw("delete", "namespace", "ampqa-yb-run7") {
		t.Errorf("teardown did not delete the namespace; calls: %v", rr.calls)
	}
}

func TestTeardownRefusesForeignNamespace(t *testing.T) {
	rr := &recordRunner{nsLabel: "someone-else", pvcs: "yb-tserver-data-yb-tserver-0"}
	d := New(Config{}, "run7").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-yb-run7"}).WithRunner(rr.run)).
		WithHelmRunner(rr.run).
		WithLogf(nil)
	if err := d.Teardown(context.Background()); err == nil {
		t.Fatalf("expected refusal to touch an unlabeled namespace")
	}
	// A foreign namespace is never touched: no PVC delete, no namespace delete,
	// no helm uninstall.
	if rr.saw("delete", "namespace", "ampqa-yb-run7") {
		t.Errorf("teardown deleted a foreign-labeled namespace; calls: %v", rr.calls)
	}
	if rr.saw("delete", "persistentvolumeclaim", "yb-tserver-data-yb-tserver-0") {
		t.Errorf("teardown deleted a foreign namespace's PVC; calls: %v", rr.calls)
	}
	if rr.saw("uninstall", "ampqa-yb-run7") {
		t.Errorf("teardown helm-uninstalled in a foreign namespace; calls: %v", rr.calls)
	}
}

// --- test doubles ---

// fakeGet is a minimal Getter backed by a dotted-key map.
type fakeGet struct{ m map[string]any }

func (g fakeGet) GetString(key, def string) string {
	if v, ok := g.m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}
func (g fakeGet) GetInt(key string, def int) int {
	if v, ok := g.m[key]; ok {
		if n, ok := v.(int); ok {
			return n
		}
	}
	return def
}
func (g fakeGet) GetBool(key string, def bool) bool {
	if v, ok := g.m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}
func (g fakeGet) GetDuration(key string, def time.Duration) time.Duration {
	if v, ok := g.m[key]; ok {
		if d, ok := v.(time.Duration); ok {
			return d
		}
	}
	return def
}

// recordRunner is a scripted k8s.Runner: it records every argv and answers the
// reads Deploy/Teardown make (tserver pod IPs, namespace managed-by label, PVC
// names by selector); everything else (apply, helm, wait, delete) succeeds with
// empty output.
type recordRunner struct {
	mu      sync.Mutex
	calls   [][]string
	stdins  []string // stdin body per call (parallel to calls); "" when none
	podIPs  string   // space-separated tserver pod IPs
	nsLabel string
	pvcs    string // space-separated PVC names returned for the release selector
}

func (r *recordRunner) run(_ context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
	body := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		body = string(b)
	}
	r.mu.Lock()
	r.calls = append(r.calls, argv)
	r.stdins = append(r.stdins, body)
	r.mu.Unlock()
	j := strings.Join(argv, " ")
	switch {
	case strings.Contains(j, " get pod ") && strings.Contains(j, "status.podIP"):
		return execpkg.Result{Stdout: r.podIPs, ExitCode: 0}, nil
	case strings.Contains(j, " get pvc ") && strings.Contains(j, "release="):
		return execpkg.Result{Stdout: r.pvcs, ExitCode: 0}, nil
	case strings.Contains(j, " get namespace "):
		return execpkg.Result{Stdout: r.nsLabel, ExitCode: 0}, nil
	default:
		return execpkg.Result{ExitCode: 0}, nil
	}
}

func (r *recordRunner) find(sub ...string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if containsAll(c, sub) {
			return c
		}
	}
	return nil
}

func (r *recordRunner) saw(sub ...string) bool { return r.find(sub...) != nil }

// appliedContains reports whether any `kubectl apply -f -` call carried a stdin
// manifest containing sub (used to assert what Deploy applied to the namespace).
func (r *recordRunner) appliedContains(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, c := range r.calls {
		if containsAll(c, []string{"apply", "-f", "-"}) && strings.Contains(r.stdins[i], sub) {
			return true
		}
	}
	return false
}

func containsAll(argv, subs []string) bool {
	for _, s := range subs {
		found := false
		for _, a := range argv {
			if a == s {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// makeChartTGZ writes a minimal yugabyte chart tarball (a top-level chart dir
// with a Chart.yaml) and returns its path.
func makeChartTGZ(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yugabyte-2025.2.1.tgz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tgz: %v", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"yugabyte/Chart.yaml":  "name: yugabyte\nversion: 2025.2.1\n",
		"yugabyte/values.yaml": "Image:\n  repository: yugabytedb/yugabyte\n  tag: latest\n",
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gz: %v", err)
	}
	return path
}
