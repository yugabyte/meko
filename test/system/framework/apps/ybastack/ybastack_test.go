package ybastack

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

	"github.com/yugabyte/yb-amp/test/system/framework/apps/yba"
	execpkg "github.com/yugabyte/yb-amp/test/system/framework/exec"
	"github.com/yugabyte/yb-amp/test/system/framework/exec/k8s"
)

// --- pure helpers ---

func TestNamespaceFor(t *testing.T) {
	if got := namespaceFor("ampqa-yba", "run7"); got != "ampqa-yba-run7" {
		t.Errorf("namespaceFor = %q", got)
	}
	if got := namespaceFor("", "run7"); got != "ampqa-yba-run7" {
		t.Errorf("namespaceFor default prefix = %q", got)
	}
}

func TestChartS3URL(t *testing.T) {
	got := chartS3URL("2025.2.1.0-b141")
	want := "s3://releases.yugabyte.com/2025.2.1.0-b141/helm/yugaware-2025.2.1.tgz"
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

func TestPatchValuesYAML(t *testing.T) {
	in := "image:\n  repository: quay.io/yugabyte/yugaware\n  tag: latest\nimagePullSecrets:\n  pullSecret: old-secret\n"
	out := patchValuesYAML(in, "quay.io/yugabyte/yugaware-itest", "quay-secret")
	if !strings.Contains(out, "repository: quay.io/yugabyte/yugaware-itest") {
		t.Errorf("repository not patched:\n%s", out)
	}
	if !strings.Contains(out, "pullSecret: quay-secret") {
		t.Errorf("pullSecret not patched:\n%s", out)
	}
	if strings.Contains(out, "old-secret") {
		t.Errorf("old pullSecret still present:\n%s", out)
	}
}

func TestSortedSetStringArgsEscapesAndSorts(t *testing.T) {
	args := sortedSetStringArgs(map[string]string{
		"b.key": "has,comma",
		"a.key": "plain",
	})
	// Sorted by key: a.key before b.key; comma escaped in the value.
	want := []string{"--set-string", "a.key=plain", "--set-string", `b.key=has\,comma`}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestSuffixClusterRoleBindings(t *testing.T) {
	manifest := `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: universe-management
rules: []
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: universe-management-binding
subjects:
  - kind: ServiceAccount
    name: yw-sa
    namespace: <SA_NAMESPACE>
roleRef:
  kind: ClusterRole
  name: universe-management
`
	filled := replaceRBACPlaceholders(manifest, "ampqa-yba-run7", "yw-sa")
	out, names, err := suffixClusterRoleBindings(filled, "ampqa-yba-run7")
	if err != nil {
		t.Fatalf("suffix: %v", err)
	}
	if len(names) != 1 || names[0] != "universe-management-binding-ampqa-yba-run7" {
		t.Fatalf("names = %v", names)
	}
	if !strings.Contains(out, "universe-management-binding-ampqa-yba-run7") {
		t.Errorf("binding not renamed:\n%s", out)
	}
	// The ClusterRole (not a binding) keeps its original name.
	if strings.Contains(out, "universe-management-ampqa-yba-run7\n") {
		t.Errorf("ClusterRole name wrongly suffixed:\n%s", out)
	}
	// Placeholder was filled before suffixing.
	if strings.Contains(out, "<SA_NAMESPACE>") {
		t.Errorf("placeholder not replaced:\n%s", out)
	}
}

func TestQuayDockerConfigJSONB64(t *testing.T) {
	// token form → build a dockerconfigjson for the registry host.
	b64, err := quayDockerConfigJSONB64("QVdTOnRva2Vu", "quay.io")
	if err != nil {
		t.Fatalf("token form: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(b64)
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode built dockerconfigjson: %v", err)
	}
	if cfg.Auths["quay.io"].Auth != "QVdTOnRva2Vu" {
		t.Errorf("auth = %q", cfg.Auths["quay.io"].Auth)
	}

	// already-dockerconfigjson form → base64 verbatim.
	dcj := `{"auths":{"quay.io":{"auth":"abc"}}}`
	b64b, err := quayDockerConfigJSONB64(dcj, "quay.io")
	if err != nil {
		t.Fatalf("dcj form: %v", err)
	}
	rawb, _ := base64.StdEncoding.DecodeString(b64b)
	if string(rawb) != dcj {
		t.Errorf("verbatim = %q, want %q", rawb, dcj)
	}

	if _, err := quayDockerConfigJSONB64("  ", "quay.io"); err == nil {
		t.Errorf("expected error on empty secret value")
	}
}

func TestRegistryHost(t *testing.T) {
	if h := registryHost("quay.io/yugabyte/yugaware-itest"); h != "quay.io" {
		t.Errorf("registryHost = %q", h)
	}
	if h := registryHost("quay.io"); h != "quay.io" {
		t.Errorf("registryHost no-slash = %q", h)
	}
}

func TestHelmReleaseFor(t *testing.T) {
	if r := helmReleaseFor("", "ampqa-yba-run7"); r != "ampqa-yba-run7-yugaware" {
		t.Errorf("helmReleaseFor derived = %q", r)
	}
	if r := helmReleaseFor("custom", "ns"); r != "custom" {
		t.Errorf("helmReleaseFor explicit = %q", r)
	}
}

func TestFromConfigChartVersionFallback(t *testing.T) {
	cfg := FromConfig(fakeGet{m: map[string]any{"versions.yugabytedb": "2025.2.1.0"}})
	if cfg.ChartVersion != "2025.2.1.0" {
		t.Errorf("ChartVersion fallback = %q", cfg.ChartVersion)
	}
	cfg2 := FromConfig(fakeGet{m: map[string]any{
		"apps.ybastack.chart_version": "2.25.1.0-b9",
		"versions.yugabytedb":         "2025.2.1.0",
	}})
	if cfg2.ChartVersion != "2.25.1.0-b9" {
		t.Errorf("explicit chart_version = %q", cfg2.ChartVersion)
	}
}

// --- Deploy happy-path with everything real shelled-out injected as fakes ---

func TestDeployAndTeardown(t *testing.T) {
	chart := makeChartTGZ(t)
	rr := &recordRunner{svcIP: "10.9.8.7", nsLabel: ManagedByValue}

	cfg := FromConfig(fakeGet{m: map[string]any{}})
	cfg.ChartVersion = "2025.2.1.0-b141"
	cfg.Reach = ReachLoadBalancer // avoids the AppVersion probe (no network in tests)

	resolve := func(name string) (string, error) {
		if name != "quay_images" {
			t.Fatalf("resolveSecret called with %q, want quay_images", name)
		}
		return "QVdTOnRva2Vu", nil
	}

	d := New(cfg, "run7", resolve).
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-yba-run7"}).WithRunner(rr.run)).
		WithHelmRunner(rr.run).
		WithDownload(func(_ context.Context, _ /*s3URL*/, _ /*destDir*/ string) (string, error) { return chart, nil }).
		WithBootstrap(func(_ context.Context, _ *yba.Client, creds yba.BootstrapCreds, _ time.Duration) (yba.BootstrapResult, error) {
			if creds.Email != DefaultBootstrapEmail {
				t.Errorf("bootstrap email = %q", creds.Email)
			}
			return yba.BootstrapResult{APIToken: "durable-tok", CustomerUUID: "cust-1", Version: "2025.2.1.0-b141"}, nil
		}).
		WithLogf(nil)

	info, err := d.Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if info.URL != "http://10.9.8.7:80" {
		t.Errorf("URL = %q", info.URL)
	}
	if info.APIToken != "durable-tok" || info.CustomerUUID != "cust-1" {
		t.Errorf("token/customer = %q/%q", info.APIToken, info.CustomerUUID)
	}
	if info.Namespace != "ampqa-yba-run7" || info.HelmRelease != "ampqa-yba-run7-yugaware" {
		t.Errorf("namespace/release = %q/%q", info.Namespace, info.HelmRelease)
	}
	if info.YBImageRegistry != DefaultYBImageRegistry {
		t.Errorf("YBImageRegistry = %q", info.YBImageRegistry)
	}

	// The helm install argv carried --wait, the timeout, and the storageClass set.
	helm := rr.find("helm", "install")
	if helm == nil {
		t.Fatalf("no helm install call recorded")
	}
	joined := strings.Join(helm, " ")
	for _, want := range []string{"--wait", "--timeout", "yugaware.storageClass=standard", "ampqa-yba-run7"} {
		if !strings.Contains(joined, want) {
			t.Errorf("helm install missing %q in: %s", want, joined)
		}
	}

	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !rr.saw("delete", "namespace", "ampqa-yba-run7") {
		t.Errorf("teardown did not delete the namespace; calls: %v", rr.calls)
	}
}

func TestDeployRequiresChartVersion(t *testing.T) {
	rr := &recordRunner{}
	d := New(Config{}, "run7", func(string) (string, error) { return "x", nil }).
		WithKube(k8s.New(k8s.Config{}).WithRunner(rr.run)).
		WithHelmRunner(rr.run).
		WithLogf(nil)
	d.cfg.ChartVersion = ""
	if _, err := d.Deploy(context.Background()); err == nil {
		t.Fatalf("expected error without a chart version")
	}
}

// TestResolveURLTailnetPodIPUsesTargetPort pins the reach bug fix: over the
// tailnet the ClusterIP is not routable, so resolveURL falls back to the pod IP
// — and it must reach the pod on the Service's targetPort (the container port),
// not the Service port. clusterIP "None" skips the (network) probe branch.
func TestResolveURLTailnetPodIPUsesTargetPort(t *testing.T) {
	rr := &recordRunner{clusterIP: "None", podIP: "10.1.2.3", targetPort: "9000"}
	d := New(Config{Reach: ReachTailnet, ServicePort: 80}, "run7", func(string) (string, error) { return "x", nil }).
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-yba-run7"}).WithRunner(rr.run)).
		WithLogf(nil)
	url, err := d.resolveURL(context.Background())
	if err != nil {
		t.Fatalf("resolveURL: %v", err)
	}
	if url != "http://10.1.2.3:9000" {
		t.Errorf("URL = %q, want http://10.1.2.3:9000", url)
	}
}

func TestTeardownDeletesSuffixedCRBs(t *testing.T) {
	rr := &recordRunner{nsLabel: ManagedByValue}
	d := New(Config{}, "run7", func(string) (string, error) { return "x", nil }).
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-yba-run7"}).WithRunner(rr.run)).
		WithLogf(nil)
	// Simulate what applyRBAC records; also include a foreign-namespace binding
	// that Teardown must NOT touch.
	d.createdCRBs = []string{"universe-management-binding-ampqa-yba-run7", "foreign-binding-other-ns"}
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !rr.saw("delete", "clusterrolebinding", "universe-management-binding-ampqa-yba-run7") {
		t.Errorf("own CRB not deleted; calls: %v", rr.calls)
	}
	if rr.saw("delete", "clusterrolebinding", "foreign-binding-other-ns") {
		t.Errorf("foreign CRB wrongly deleted; calls: %v", rr.calls)
	}
	// The chart's own cluster-scoped RBAC (named after the helm release) must be
	// reaped too — a namespace delete does not catch cluster-scoped objects.
	if !rr.saw("delete", "clusterrolebinding", "ampqa-yba-run7-yugaware") {
		t.Errorf("chart ClusterRoleBinding not deleted; calls: %v", rr.calls)
	}
	if !rr.saw("delete", "clusterrole", "ampqa-yba-run7-yugaware") {
		t.Errorf("chart ClusterRole not deleted; calls: %v", rr.calls)
	}
}

func TestTeardownRefusesUnlabeledNamespace(t *testing.T) {
	rr := &recordRunner{nsLabel: "someone-else"}
	d := New(Config{}, "run7", func(string) (string, error) { return "x", nil }).
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-yba-run7"}).WithRunner(rr.run)).
		WithLogf(nil)
	if err := d.Teardown(context.Background()); err == nil {
		t.Fatalf("expected refusal to delete an unlabeled namespace")
	}
	if rr.saw("delete", "namespace", "ampqa-yba-run7") {
		t.Errorf("teardown deleted a foreign-labeled namespace; calls: %v", rr.calls)
	}
}

// TestReapUsesRecordedHelmRelease: `ampqa env rm` reconstructs the reap from the
// registry record, which now carries the REAL helm release. The yugaware chart's
// cluster-scoped RBAC is named after that release, and a namespace delete never
// reclaims cluster-scoped objects — so a custom helm_release must be reaped by
// its real name, else the ClusterRole/Binding leaks.
func TestReapUsesRecordedHelmRelease(t *testing.T) {
	rr := &recordRunner{nsLabel: ManagedByValue}
	d := reapDeployer("gke_ctx", "/kc", "ampqa-yba-run7", "my-custom-release", rr.run, nil)
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !rr.saw("delete", "clusterrolebinding", "my-custom-release") {
		t.Errorf("reap must delete the custom-release ClusterRoleBinding; calls: %v", rr.calls)
	}
	if !rr.saw("delete", "clusterrole", "my-custom-release") {
		t.Errorf("reap must delete the custom-release ClusterRole; calls: %v", rr.calls)
	}
	if rr.saw("delete", "clusterrolebinding", "ampqa-yba-run7-yugaware") {
		t.Errorf("reap deleted the namespace-derived release instead of the custom one (RBAC leak); calls: %v", rr.calls)
	}
}

// TestReapEmptyReleaseDerivesFromNamespace: an unset helm_release falls back to
// the namespace-derived default (the standing-env case), unchanged.
func TestReapEmptyReleaseDerivesFromNamespace(t *testing.T) {
	rr := &recordRunner{nsLabel: ManagedByValue}
	d := reapDeployer("gke_ctx", "/kc", "ampqa-yba-run7", "", rr.run, nil)
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !rr.saw("delete", "clusterrolebinding", "ampqa-yba-run7-yugaware") {
		t.Errorf("empty release must derive the namespace default; calls: %v", rr.calls)
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
// two reads Deploy/Teardown make (svc load-balancer ip, namespace managed-by
// label); everything else (apply, helm, delete) succeeds with empty output.
type recordRunner struct {
	mu         sync.Mutex
	calls      [][]string
	svcIP      string
	clusterIP  string
	podIP      string
	targetPort string
	nsLabel    string
}

func (r *recordRunner) run(_ context.Context, _ io.Reader, argv []string) (execpkg.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, argv)
	r.mu.Unlock()
	j := strings.Join(argv, " ")
	switch {
	case strings.Contains(j, " get svc ") && strings.Contains(j, "loadBalancer"):
		return execpkg.Result{Stdout: r.svcIP, ExitCode: 0}, nil
	case strings.Contains(j, " get svc ") && strings.Contains(j, "targetPort"):
		return execpkg.Result{Stdout: r.targetPort, ExitCode: 0}, nil
	case strings.Contains(j, " get svc ") && strings.Contains(j, "clusterIP"):
		return execpkg.Result{Stdout: r.clusterIP, ExitCode: 0}, nil
	case strings.Contains(j, " get pod ") && strings.Contains(j, "status.podIP"):
		return execpkg.Result{Stdout: r.podIP, ExitCode: 0}, nil
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

// makeChartTGZ writes a minimal yugaware chart tarball (a top-level chart dir
// with a values.yaml) and returns its path.
func makeChartTGZ(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yugaware-2025.2.1.tgz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tgz: %v", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"yugaware/Chart.yaml":  "name: yugaware\nversion: 2025.2.1\n",
		"yugaware/values.yaml": "image:\n  repository: quay.io/yugabyte/yugaware\n  pullSecret: yugabyte-k8s-pull-secret\n",
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
