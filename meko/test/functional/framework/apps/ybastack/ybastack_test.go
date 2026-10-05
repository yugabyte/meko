package ybastack

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yba"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	execpkg "github.com/yugabyte/meko/meko/test/functional/framework/exec"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

// --- pure helpers ---

func TestNamespaceFor(t *testing.T) {
	if got := namespaceFor("mekoqa-yba", "run7"); got != "mekoqa-yba-run7" {
		t.Errorf("namespaceFor = %q", got)
	}
	if got := namespaceFor("", "run7"); got != "mekoqa-yba-run7" {
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
	filled := replaceRBACPlaceholders(manifest, "mekoqa-yba-run7", "yw-sa")
	out, names, err := suffixClusterRoleBindings(filled, "mekoqa-yba-run7")
	if err != nil {
		t.Fatalf("suffix: %v", err)
	}
	if len(names) != 1 || names[0] != "universe-management-binding-mekoqa-yba-run7" {
		t.Fatalf("names = %v", names)
	}
	if !strings.Contains(out, "universe-management-binding-mekoqa-yba-run7") {
		t.Errorf("binding not renamed:\n%s", out)
	}
	// The ClusterRole (not a binding) keeps its original name.
	if strings.Contains(out, "universe-management-mekoqa-yba-run7\n") {
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
	if r := helmReleaseFor("", "mekoqa-yba-run7"); r != "mekoqa-yba-run7-yugaware" {
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
		WithKube(k8s.New(k8s.Config{Namespace: "mekoqa-yba-run7"}).WithRunner(rr.run)).
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
	if info.Namespace != "mekoqa-yba-run7" || info.HelmRelease != "mekoqa-yba-run7-yugaware" {
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
	for _, want := range []string{"--wait", "--timeout", "yugaware.storageClass=standard", "mekoqa-yba-run7"} {
		if !strings.Contains(joined, want) {
			t.Errorf("helm install missing %q in: %s", want, joined)
		}
	}

	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !rr.saw("delete", "namespace", "mekoqa-yba-run7") {
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

// The pod-IP path must reach the pod on the Service's targetPort (the container
// port), not the Service port, and only when that address ANSWERS.
// clusterIP "None" skips the ClusterIP branch.
func TestReachTailnetPodIPUsesTargetPort(t *testing.T) {
	rr := &recordRunner{clusterIP: "None", podIP: "10.1.2.3", targetPort: "9000"}
	d := New(Config{Reach: ReachTailnet, ServicePort: 80}, "run7", func(string) (string, error) { return "x", nil }).
		WithKube(k8s.New(k8s.Config{Namespace: "mekoqa-yba-run7"}).WithRunner(rr.run)).
		WithLogf(nil)
	var probed []string
	d.probeURL = func(_ context.Context, url string) bool { probed = append(probed, url); return true }
	url, err := d.reachPlatform(context.Background())
	if err != nil {
		t.Fatalf("reachPlatform: %v", err)
	}
	if url != "http://10.1.2.3:9000" {
		t.Errorf("URL = %q, want http://10.1.2.3:9000", url)
	}
	if len(probed) != 1 || probed[0] != "http://10.1.2.3:9000" {
		t.Errorf("probed %v, want exactly the pod URL on the targetPort", probed)
	}
	if d.forwarded() {
		t.Error("a reachable direct address must not open a port-forward")
	}
}

// The regression that cost three runs: on itest-dev neither the ClusterIP nor the
// pod IP answers (the pod CIDR collides with an advertised tailnet route, so the
// dial TIMES OUT). Handing that address to bootstrap anyway burned its whole
// 10-minute budget and the caller then downgraded to yugabyted. Now an
// unanswering pod IP falls through to a port-forward.
func TestReachFallsBackToPortForwardWhenNoDirectAddressAnswers(t *testing.T) {
	rr := &recordRunner{clusterIP: "34.118.225.179", podIP: "10.21.137.3", targetPort: "9000"}
	starter, _ := fakePF(15551)
	kube := k8s.New(k8s.Config{Namespace: "mekoqa-yba-run7"}).WithRunner(rr.run).WithStarter(starter)
	d := New(Config{Reach: ReachTailnet, ServicePort: 80}, "run7", func(string) (string, error) { return "x", nil }).
		WithKube(kube).WithLogf(nil)
	d.probeURL = func(context.Context, string) bool { return false }

	url, err := d.reachPlatform(context.Background())
	if err != nil {
		t.Fatalf("reachPlatform: %v", err)
	}
	if url != "http://127.0.0.1:15551" {
		t.Errorf("URL = %q, want the forwarded loopback address", url)
	}
	if !d.forwarded() {
		t.Fatal("the forward handle was not kept, so Teardown cannot close it")
	}
	// Both dead addresses must be named, or the log says nothing about why.
	if _, err := d.resolveDirectURL(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "34.118.225.179") || !strings.Contains(err.Error(), "10.21.137.3:9000") {
		t.Errorf("direct error %v does not name both addresses tried", err)
	}
	// A forwarded platform hands its callers a reviving transport; a directly
	// reached one must not (there is nothing to revive).
	if d.infoTransport() == nil {
		t.Error("a forwarded platform must expose a transport for Info")
	}
}

// A client built for a forwarded YBA must survive the forward dying: one
// re-forward and a retry, not a failed run.
func TestRevivingTransportRetriesOnceOnRefused(t *testing.T) {
	var live atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":"2026.1.0.0"}`))
	}))
	defer srv.Close()
	liveHost := strings.TrimPrefix(srv.URL, "http://")

	var hostCalls atomic.Int32
	tr := &revivingTransport{host: func(_ context.Context, force bool) (string, error) {
		hostCalls.Add(1)
		if force {
			live.Store(true)
		}
		if live.Load() {
			return liveHost, nil
		}
		return "127.0.0.1:1", nil // refuses
	}}
	res, err := (&http.Client{Transport: tr}).Get("http://placeholder/api/v1/app_version")
	if err != nil {
		t.Fatalf("request through a revived forward: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if n := hostCalls.Load(); n != 2 {
		t.Errorf("host resolved %d times, want 2 (once refused, once re-forwarded)", n)
	}
}

// Teardown owns the forward: its kubectl process is background-rooted precisely
// so the Deploy ctx cannot kill it, which means nothing else ever will.
func TestTeardownClosesThePortForward(t *testing.T) {
	rr := &recordRunner{nsLabel: ManagedByValue, clusterIP: "None", podIP: "10.21.137.3", targetPort: "9000"}
	starter, procs := fakePF(15552)
	kube := k8s.New(k8s.Config{Namespace: "mekoqa-yba-run7"}).WithRunner(rr.run).WithStarter(starter)
	d := New(Config{Reach: ReachTailnet, ServicePort: 80}, "run7", func(string) (string, error) { return "x", nil }).
		WithKube(kube).WithLogf(nil)
	d.probeURL = func(context.Context, string) bool { return false }
	if _, err := d.reachPlatform(context.Background()); err != nil {
		t.Fatalf("reachPlatform: %v", err)
	}
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if len(procs()) != 1 {
		t.Fatalf("expected exactly one forward process, got %d", len(procs()))
	}
	select {
	case <-procs()[0].killed:
	case <-time.After(2 * time.Second):
		t.Error("the port-forward process survived Teardown")
	}
	if d.forwarded() {
		t.Error("Teardown left the forward handle in place")
	}
}

func TestTeardownDeletesSuffixedCRBs(t *testing.T) {
	rr := &recordRunner{nsLabel: ManagedByValue}
	d := New(Config{}, "run7", func(string) (string, error) { return "x", nil }).
		WithKube(k8s.New(k8s.Config{Namespace: "mekoqa-yba-run7"}).WithRunner(rr.run)).
		WithLogf(nil)
	// Simulate what applyRBAC records; also include a foreign-namespace binding
	// that Teardown must NOT touch.
	d.createdCRBs = []string{"universe-management-binding-mekoqa-yba-run7", "foreign-binding-other-ns"}
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !rr.saw("delete", "clusterrolebinding", "universe-management-binding-mekoqa-yba-run7") {
		t.Errorf("own CRB not deleted; calls: %v", rr.calls)
	}
	if rr.saw("delete", "clusterrolebinding", "foreign-binding-other-ns") {
		t.Errorf("foreign CRB wrongly deleted; calls: %v", rr.calls)
	}
	// The chart's own cluster-scoped RBAC (named after the helm release) must be
	// reaped too, a namespace delete does not catch cluster-scoped objects.
	if !rr.saw("delete", "clusterrolebinding", "mekoqa-yba-run7-yugaware") {
		t.Errorf("chart ClusterRoleBinding not deleted; calls: %v", rr.calls)
	}
	if !rr.saw("delete", "clusterrole", "mekoqa-yba-run7-yugaware") {
		t.Errorf("chart ClusterRole not deleted; calls: %v", rr.calls)
	}
}

func TestTeardownRefusesUnlabeledNamespace(t *testing.T) {
	rr := &recordRunner{nsLabel: "someone-else"}
	d := New(Config{}, "run7", func(string) (string, error) { return "x", nil }).
		WithKube(k8s.New(k8s.Config{Namespace: "mekoqa-yba-run7"}).WithRunner(rr.run)).
		WithLogf(nil)
	if err := d.Teardown(context.Background()); err == nil {
		t.Fatalf("expected refusal to delete an unlabeled namespace")
	}
	if rr.saw("delete", "namespace", "mekoqa-yba-run7") {
		t.Errorf("teardown deleted a foreign-labeled namespace; calls: %v", rr.calls)
	}
}

// TestReapUsesRecordedHelmRelease: `mekoqa env rm` reconstructs the reap from the
// registry record, which now carries the REAL helm release. The yugaware chart's
// cluster-scoped RBAC is named after that release, and a namespace delete never
// reclaims cluster-scoped objects, so a custom helm_release must be reaped by
// its real name, else the ClusterRole/Binding leaks.
func TestReapUsesRecordedHelmRelease(t *testing.T) {
	rr := &recordRunner{nsLabel: ManagedByValue}
	d := reapDeployer("gke_ctx", "/kc", "mekoqa-yba-run7", "my-custom-release", rr.run, nil)
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !rr.saw("delete", "clusterrolebinding", "my-custom-release") {
		t.Errorf("reap must delete the custom-release ClusterRoleBinding; calls: %v", rr.calls)
	}
	if !rr.saw("delete", "clusterrole", "my-custom-release") {
		t.Errorf("reap must delete the custom-release ClusterRole; calls: %v", rr.calls)
	}
	if rr.saw("delete", "clusterrolebinding", "mekoqa-yba-run7-yugaware") {
		t.Errorf("reap deleted the namespace-derived release instead of the custom one (RBAC leak); calls: %v", rr.calls)
	}
}

// TestReapEmptyReleaseDerivesFromNamespace: an unset helm_release falls back to
// the namespace-derived default (the standing-env case), unchanged.
func TestReapEmptyReleaseDerivesFromNamespace(t *testing.T) {
	rr := &recordRunner{nsLabel: ManagedByValue}
	d := reapDeployer("gke_ctx", "/kc", "mekoqa-yba-run7", "", rr.run, nil)
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !rr.saw("delete", "clusterrolebinding", "mekoqa-yba-run7-yugaware") {
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

// fakePF is a Starter yielding forwards that announce `port` and stay alive until
// killed, plus an accessor for the processes it handed out (so a test can prove
// Teardown killed one). kubectl's real forward behaves the same way: it prints
// the "Forwarding from" line and then runs until signalled.
func fakePF(port int) (k8s.Starter, func() []*livePFProc) {
	var mu sync.Mutex
	var procs []*livePFProc
	starter := func(context.Context, []string) (k8s.Process, error) {
		pr, pw := io.Pipe()
		go func() { _, _ = io.WriteString(pw, fmt.Sprintf("Forwarding from 127.0.0.1:%d -> 80\n", port)) }()
		p := &livePFProc{out: pr, w: pw, killed: make(chan struct{})}
		mu.Lock()
		procs = append(procs, p)
		mu.Unlock()
		return p, nil
	}
	return starter, func() []*livePFProc {
		mu.Lock()
		defer mu.Unlock()
		return append([]*livePFProc(nil), procs...)
	}
}

type livePFProc struct {
	out    io.Reader
	w      *io.PipeWriter
	killed chan struct{}
	once   sync.Once
}

func (p *livePFProc) Stdout() io.Reader { return p.out }
func (p *livePFProc) Kill() error {
	p.once.Do(func() {
		close(p.killed)
		_ = p.w.Close()
	})
	return nil
}
func (p *livePFProc) Wait() error { <-p.killed; return nil }

// hangUpListener accepts and immediately closes, which is exactly what kubectl
// looks like once its pod is gone: the dial succeeds and the request EOFs.
func hangUpListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln
}

// The 0.260902.0 stress failure: kubectl stays up and keeps accepting on the
// local port, but its pod is gone, so every request EOFs. A pinned EOF used to
// go unretried and never healed, backing_maintenance and then backing_nemesis
// died 15 minutes apart on the same forward.
func TestRevivingTransportRetriesReadsOnEOF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"2026.1.0.0"}`))
	}))
	defer srv.Close()
	liveHost := strings.TrimPrefix(srv.URL, "http://")
	dead := hangUpListener(t)

	var live atomic.Bool
	var hostCalls atomic.Int32
	tr := &revivingTransport{host: func(_ context.Context, force bool) (string, error) {
		hostCalls.Add(1)
		if force {
			live.Store(true)
		}
		if live.Load() {
			return liveHost, nil
		}
		return dead.Addr().String(), nil
	}}
	res, err := (&http.Client{Transport: tr}).Get("http://placeholder/api/v1/app_version")
	if err != nil {
		t.Fatalf("GET through a forward that EOFs: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if n := hostCalls.Load(); n != 2 {
		t.Errorf("host resolved %d times, want 2 (once EOF, once re-forwarded)", n)
	}
}

// An EOF means the connection was established, so YBA may already have applied
// the write. Restart, gflag-upgrade, backup and PITR creates all ride this
// transport and none are deduped, so a write is never replayed on one, it is
// reported, and the break is remembered for the next request.
func TestRevivingTransportDoesNotReplayWritesOnEOF(t *testing.T) {
	dead := hangUpListener(t)
	var forced atomic.Int32
	var broken atomic.Bool
	tr := &revivingTransport{
		host: func(_ context.Context, force bool) (string, error) {
			if force {
				forced.Add(1)
			}
			return dead.Addr().String(), nil
		},
		markBroken: func() { broken.Store(true) },
	}
	_, err := (&http.Client{Transport: tr}).Post("http://placeholder/api/v1/universes/abc/upgrade/restart",
		"application/json", strings.NewReader(`{}`))
	if err == nil {
		t.Fatal("a write that broke mid-flight must be reported, not silently retried")
	}
	if !strings.Contains(err.Error(), "may already have been applied") {
		t.Errorf("error %v does not say the write was not replayed", err)
	}
	if !core.IsEnvFailure(err) {
		t.Errorf("error %v is not an env failure", err)
	}
	if n := forced.Load(); n != 0 {
		t.Errorf("the write was re-sent %d time(s), want 0", n)
	}
	if !broken.Load() {
		t.Error("the break must be recorded so the next request rebuilds the forward first")
	}
}

// A refused dial never reached YBA, so a write may go again, this is the path
// that was already safe before EOF joined the set.
func TestRevivingTransportReplaysWritesOnRefused(t *testing.T) {
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	var live atomic.Bool
	tr := &revivingTransport{host: func(_ context.Context, force bool) (string, error) {
		if force {
			live.Store(true)
		}
		if live.Load() {
			return strings.TrimPrefix(srv.URL, "http://"), nil
		}
		return "127.0.0.1:1", nil // refuses
	}}
	res, err := (&http.Client{Transport: tr}).Post("http://placeholder/api/v1/universes/clusters",
		"application/json", strings.NewReader(`{"name":"mekoqa-1"}`))
	if err != nil {
		t.Fatalf("POST over a refused forward: %v", err)
	}
	defer res.Body.Close()
	if n := seen.Load(); n != 1 {
		t.Errorf("yba saw %d copies of the write, want 1", n)
	}
}

// A yugaware pod that has not been rescheduled yet fails the re-forward itself,
// which is a state the next attempt can find fixed, not a reason to give up on
// the first try.
func TestRevivingTransportRetriesWhileTheForwardCannotBeRebuilt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"2026.1.0.0"}`))
	}))
	defer srv.Close()
	dead := hangUpListener(t)

	var calls atomic.Int32
	tr := &revivingTransport{host: func(_ context.Context, force bool) (string, error) {
		n := calls.Add(1)
		switch {
		case n == 1:
			return dead.Addr().String(), nil // the broken forward
		case n < 4:
			return "", fmt.Errorf("port-forward svc/yugaware not ready: no ready endpoint") // still rescheduling
		default:
			return strings.TrimPrefix(srv.URL, "http://"), nil
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://placeholder/api/v1/app_version", nil)
	res, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("request across a pod reschedule: %v", err)
	}
	defer res.Body.Close()
	if n := calls.Load(); n != 4 {
		t.Errorf("host resolved %d times, want 4 (one break, two un-rebuildable, one good)", n)
	}
}

// A forward that cannot be brought back must surface as an env failure, not as
// a bare transport error the suite reports as a product defect.
func TestRevivingTransportGivesUpAsEnvFailure(t *testing.T) {
	dead := hangUpListener(t)
	tr := &revivingTransport{host: func(context.Context, bool) (string, error) {
		return dead.Addr().String(), nil
	}}
	// Short on purpose: the client timeout that ends a request mid-loop is the
	// common give-up path (30s in defaultNewClient), and it must be an env
	// failure too, not a product verdict.
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://placeholder/api/v1/app_version", nil)
	_, err := (&http.Client{Transport: tr}).Do(req)
	if err == nil {
		t.Fatal("a permanently broken forward must fail the request")
	}
	if !core.IsEnvFailure(err) {
		t.Errorf("error %v is not an env failure", err)
	}
}

// A YBA that answers, with any status, is not a broken forward, and retrying
// it would double every write the suite makes.
func TestRevivingTransportDoesNotRetryAnAnsweringYBA(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	tr := &revivingTransport{host: func(context.Context, bool) (string, error) {
		return strings.TrimPrefix(srv.URL, "http://"), nil
	}}
	res, err := (&http.Client{Transport: tr}).Get("http://placeholder/api/v1/app_version")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want the 500 the server sent", res.StatusCode)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("server saw %d requests, want 1", n)
	}
}

func TestIsDeadForward(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"refused", errors.New(`Post "http://127.0.0.1:38757/x": dial tcp: connection refused`), true},
		{"eof", errors.New(`Get "http://127.0.0.1:38757/api/v1/tasks/abc": EOF`), true},
		{"wrapped io.EOF", fmt.Errorf("read task: %w", io.EOF), true},
		{"unexpected eof", errors.New("unexpected EOF"), true},
		{"reset", errors.New("read tcp 127.0.0.1:38757: connection reset by peer"), true},
		{"pooled conn died", errors.New("http: server closed idle connection"), true},
		{"timeout", errors.New(`Get "http://10.21.137.3:9000/x": context deadline exceeded`), false},
		{"http error", errors.New("yba returned 500: internal server error"), false},
		{"nil", nil, false},
	} {
		if got := isDeadForward(tc.err); got != tc.want {
			t.Errorf("%s: isDeadForward(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
