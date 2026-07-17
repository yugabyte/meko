package ampstack

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec/k8s"
)

// renderedChart is a canned `helm template` output: a ServiceAccount + Role +
// RoleBinding + Deployment plus the cluster-scoped ClusterRole/ClusterRoleBinding
// the chart emits when rbac.clusterScoped is on — so a test can assert the
// cluster-scoped docs are filtered out on a namespace-scoped install.
const renderedChart = `apiVersion: v1
kind: ServiceAccount
metadata:
  name: amp-controller
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: amp-controller
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: amp-controller
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: amp-controller
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ns-amp-controller-namespaces
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ns-amp-controller-namespaces
`

// call is one recorded kubectl/helm invocation with its stdin (the applied
// manifest, for apply -f -).
type call struct {
	argv  string
	stdin string
}

// recorder scripts kubectl/helm: it records every invocation (argv + stdin),
// answers by substring rule, and hands PortForward a canned readiness line.
type recorder struct {
	mu    sync.Mutex
	calls []call
	rules []rule
}

type rule struct {
	match string
	res   exec.Result
}

func newRecorder() *recorder { return &recorder{} }

func (r *recorder) on(match string, res exec.Result) *recorder {
	r.rules = append(r.rules, rule{match: match, res: res})
	return r
}

func (r *recorder) run(_ context.Context, stdin io.Reader, argv []string) (exec.Result, error) {
	joined := strings.Join(argv, " ")
	in := ""
	if stdin != nil {
		if b, err := io.ReadAll(stdin); err == nil {
			in = string(b)
		}
	}
	r.mu.Lock()
	r.calls = append(r.calls, call{argv: joined, stdin: in})
	r.mu.Unlock()
	for _, rl := range r.rules {
		if strings.Contains(joined, rl.match) {
			return rl.res, nil
		}
	}
	return exec.Result{ExitCode: 0}, nil
}

func (r *recorder) start(_ context.Context, argv []string) (k8s.Process, error) {
	r.mu.Lock()
	r.calls = append(r.calls, call{argv: strings.Join(argv, " ")})
	r.mu.Unlock()
	return &fakeProc{out: strings.NewReader("Forwarding from 127.0.0.1:18080 -> 8080\n")}, nil
}

func (r *recorder) snapshot() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]call, len(r.calls))
	copy(out, r.calls)
	return out
}

type fakeProc struct{ out io.Reader }

func (p *fakeProc) Stdout() io.Reader { return p.out }
func (p *fakeProc) Kill() error       { return nil }
func (p *fakeProc) Wait() error       { return nil }

// diesProc is a long-lived fake port-forward process: it emits its readiness
// line then blocks (stdout stays open, Alive() stays true) until closed. Its
// die() simulates kubectl exiting on its own — the real-world failure this
// fix targets — as opposed to Kill(), which is what the app calls deliberately
// on Close/Teardown. Both funnel through the same close so either unblocks the
// PortForward's watch goroutine (EOF on stdout) and reaps it as dead.
type diesProc struct {
	pr   *io.PipeReader
	pw   *io.PipeWriter
	done chan struct{}
	once sync.Once
}

func newDiesProc(line string) *diesProc {
	pr, pw := io.Pipe()
	p := &diesProc{pr: pr, pw: pw, done: make(chan struct{})}
	go func() { _, _ = io.WriteString(pw, line) }()
	return p
}

func (p *diesProc) Stdout() io.Reader { return p.pr }
func (p *diesProc) close() {
	p.once.Do(func() {
		close(p.done)
		_ = p.pw.Close()
	})
}
func (p *diesProc) Kill() error { p.close(); return nil }
func (p *diesProc) Wait() error { <-p.done; return nil }

// waitUntil polls cond until it is true or d elapses, returning the final
// evaluation (so a caller gets an accurate failure message either way).
func waitUntil(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

func testApp(cfg Config, rec *recorder) *App {
	cfg.Context = "test-ctx"
	if cfg.Namespace == "" {
		cfg.Namespace = "ampqa-test"
	}
	if cfg.ControllerChart == "" && cfg.ControllerManifest == "" {
		cfg.ControllerChart = "./chart"
	}
	if cfg.ECRRegistry == "" && cfg.PullSecretDockerConfigB64 == "" {
		cfg.ECRRegistry = DefaultECRRegistry // exercise the mint path by default
	}
	if cfg.Registry == "" {
		// The pre-registry-knob tests in this file exercise the ECR pipeline
		// (bare refs resolving into ECR, minted pull secret). Registry-aware
		// tests set the knob explicitly.
		cfg.Registry = RegistryECR
	}
	if cfg.ControllerReach == "" {
		// Every pre-existing test in this file predates ControllerReach and
		// exercises the port-forward pipeline specifically; pin it here so the
		// package default flipping to tailnet doesn't change their behavior.
		// Tailnet-mode tests set ControllerReach explicitly.
		cfg.ControllerReach = ReachPortForward
	}
	a := New(cfg)
	a.logf = nil
	kube := k8s.New(k8s.Config{Context: cfg.Context, Namespace: cfg.Namespace}).WithRunner(rec.run).WithStarter(rec.start)
	a.WithKube(kube).WithHelmRunner(rec.run)
	a.WithReadyFunc(func(context.Context, *controller.Client, time.Duration) error { return nil })
	a.WithECRToken(func(context.Context, string, string) (string, error) { return "faketoken", nil })
	// helm template renders the canned chart.
	rec.on("helm template", exec.Result{Stdout: renderedChart})
	return a
}

// appliedManifests returns the stdin of every `apply -f -` call.
func appliedManifests(calls []call) []string {
	var out []string
	for _, c := range calls {
		if strings.Contains(c.argv, "apply -f -") {
			out = append(out, c.stdin)
		}
	}
	return out
}

func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func hasCall(calls []call, sub string) bool {
	for _, c := range calls {
		if strings.Contains(c.argv, sub) {
			return true
		}
	}
	return false
}

func TestDeploySequence(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{HelmShim: false}, rec)

	info, err := a.Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if info.ControllerURL != "http://127.0.0.1:18080" {
		t.Errorf("controller url = %q", info.ControllerURL)
	}
	if info.Namespace != "ampqa-test" {
		t.Errorf("namespace = %q", info.Namespace)
	}
	if info.Forward == nil {
		t.Error("StackInfo.Forward should be set")
	}

	calls := rec.snapshot()
	assertOrder(t, calls,
		"apply -f -",                      // namespace
		"apply -f -",                      // pull secret
		"apply -f -",                      // yugabyted
		"wait statefulset/yugabyted",      // yb ready
		"exec yugabyted-0",                // yb_storage
		"helm template yb-amp-controller", // render controller
		"apply -f -",                      // controller manifest
		"wait deployment/amp-controller",  // controller available
		"port-forward svc/amp-controller", // controller forward
	)
	// The namespace carries the managed-by label.
	if !anyContains(appliedManifests(calls), ManagedByLabel) {
		t.Error("namespace should carry the ampqa-managed-by label")
	}
}

// dyingForwards is a kubectl Starter double that hands out long-lived
// diesProc instances (one per port-forward invocation, each on a distinct
// ephemeral-looking local port) and records them so a test can kill one
// externally — simulating kubectl exiting on its own, not via the app's Close.
type dyingForwards struct {
	mu       sync.Mutex
	basePort int
	procs    []*diesProc
}

func wireDyingForwards(a *App, basePort int) *dyingForwards {
	df := &dyingForwards{basePort: basePort}
	a.kube.WithStarter(func(_ context.Context, argv []string) (k8s.Process, error) {
		df.mu.Lock()
		defer df.mu.Unlock()
		port := df.basePort + len(df.procs)
		p := newDiesProc(fmt.Sprintf("Forwarding from 127.0.0.1:%d -> 8080\n", port))
		df.procs = append(df.procs, p)
		return p, nil
	})
	return df
}

func (df *dyingForwards) count() int {
	df.mu.Lock()
	defer df.mu.Unlock()
	return len(df.procs)
}

// killLatest simulates the most-recently-started kubectl process exiting on
// its own (e.g. crashing) — never through the app's Kill/Close path.
func (df *dyingForwards) killLatest() {
	df.mu.Lock()
	defer df.mu.Unlock()
	if len(df.procs) > 0 {
		df.procs[len(df.procs)-1].close()
	}
}

// TestControllerForwardSurvivesDeployNotClosedAtHealthCheck asserts the
// controller port-forward Deploy opens for its own health check is still
// alive afterwards (not closed once the health check passes) — it must live
// through the whole compute phase, released only by Close/Teardown.
func TestControllerForwardSurvivesDeployNotClosedAtHealthCheck(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{HelmShim: false}, rec)
	df := wireDyingForwards(a, 18080)

	info, err := a.Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if info.Forward == nil || !info.Forward.Alive() {
		t.Fatal("controller forward must still be alive right after Deploy")
	}
	// ControllerClient must reuse it rather than opening a second forward.
	if _, _, err := a.ControllerClient(context.Background()); err != nil {
		t.Fatalf("ControllerClient: %v", err)
	}
	if n := df.count(); n != 1 {
		t.Errorf("port-forward opened %d times, want 1 (Deploy's forward must be reused, alive)", n)
	}
}

// TestControllerClientReconnectsWhenForwardDies: once the kubectl process
// backing the held forward exits on its own (the real bug — "process already
// finished" — not via the app's own Close), ControllerClient must detect it
// via Alive() and transparently re-establish a new forward on a new local
// port, handing back a client pointed at the new URL.
func TestControllerClientReconnectsWhenForwardDies(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{HelmShim: false}, rec)
	df := wireDyingForwards(a, 18080)

	info, err := a.Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if info.ControllerURL != "http://127.0.0.1:18080" {
		t.Fatalf("controller url = %q, want the first ephemeral port", info.ControllerURL)
	}

	// The kubectl process backing the held forward dies on its own.
	df.killLatest()
	if !waitUntil(func() bool { return !info.Forward.Alive() }, time.Second) {
		t.Fatal("forward must be observed dead once its process exits")
	}

	cl, url, err := a.ControllerClient(context.Background())
	if err != nil {
		t.Fatalf("ControllerClient did not reconnect: %v", err)
	}
	if url == info.ControllerURL {
		t.Errorf("reconnect should bind a new local port, still got %q", url)
	}
	if cl == nil || cl.BaseURL() != url {
		t.Errorf("client base url = %q, want %q", cl.BaseURL(), url)
	}
	if n := df.count(); n != 2 {
		t.Errorf("port-forward opened %d times, want 2 (initial + reconnect)", n)
	}

	// A second call with the reconnected forward still alive must not
	// reconnect again.
	if _, url2, err := a.ControllerClient(context.Background()); err != nil || url2 != url {
		t.Errorf("ControllerClient(again) = (%q, %v), want the same reconnected url with no error", url2, err)
	}
	if n := df.count(); n != 2 {
		t.Errorf("port-forward opened %d times, want still 2 (alive forward must be reused)", n)
	}
}

// healthzServer is an httptest server answering ok/not-ok for GET /healthz.
func healthzServer(ok bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
}

// TestDeployTailnetResolvesClusterIPNoPortForward asserts the default
// (tailnet) reach mode resolves the controller Service's ClusterIP, builds a
// direct "http://<ip>:<port>" URL, and never shells kubectl port-forward —
// the whole point of the fix (kubectl port-forward to the controller was
// fundamentally flaky over a long-running test).
func TestDeployTailnetResolvesClusterIPNoPortForward(t *testing.T) {
	rec := newRecorder()
	rec.on("get svc amp-controller", exec.Result{Stdout: "10.1.2.3"})
	srv := healthzServer(true)
	defer srv.Close()

	a := testApp(Config{HelmShim: false, ControllerReach: ReachTailnet}, rec)
	a.WithClientFactory(func(string) *controller.Client { return controller.New(srv.URL) })

	info, err := a.Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if info.ControllerURL != "http://10.1.2.3:8080" {
		t.Errorf("controller url = %q, want http://10.1.2.3:8080", info.ControllerURL)
	}
	if info.Forward != nil {
		t.Error("tailnet mode must not hold a port-forward")
	}
	if info.PortForwardCmd != "" {
		t.Errorf("tailnet mode must not surface a port-forward hint, got %q", info.PortForwardCmd)
	}
	calls := rec.snapshot()
	if hasCall(calls, "port-forward") {
		t.Error("tailnet mode must never invoke kubectl port-forward")
	}
	if !hasCall(calls, "get svc amp-controller") {
		t.Error("tailnet mode must resolve the controller Service ClusterIP")
	}

	cl, url, err := a.ControllerClient(context.Background())
	if err != nil {
		t.Fatalf("ControllerClient: %v", err)
	}
	if url != "http://10.1.2.3:8080" || cl == nil {
		t.Errorf("ControllerClient = (%v, %q), want a client on http://10.1.2.3:8080", cl, url)
	}
	if hasCall(rec.snapshot(), "port-forward") {
		t.Error("ControllerClient in tailnet mode must never invoke kubectl port-forward")
	}
}

// TestDeployTailnetFallsBackToPodIPWhenClusterIPUnhealthy asserts that when the
// controller Service ClusterIP doesn't answer /healthz, resolution falls back
// to the controller pod's IP (proven reachable over the tailnet the same way
// an amp endpoint pod IP already is) rather than ever opening a port-forward.
func TestDeployTailnetFallsBackToPodIPWhenClusterIPUnhealthy(t *testing.T) {
	rec := newRecorder()
	rec.on("get svc amp-controller", exec.Result{Stdout: "10.1.2.3"})
	rec.on("get pod -l app=amp-controller", exec.Result{Stdout: "10.9.9.9"})
	bad := healthzServer(false)
	defer bad.Close()
	good := healthzServer(true)
	defer good.Close()

	a := testApp(Config{HelmShim: false, ControllerReach: ReachTailnet}, rec)
	a.WithClientFactory(func(url string) *controller.Client {
		if strings.Contains(url, "10.1.2.3") {
			return controller.New(bad.URL)
		}
		return controller.New(good.URL)
	})

	info, err := a.Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if info.ControllerURL != "http://10.9.9.9:8080" {
		t.Errorf("controller url = %q, want the pod-IP fallback http://10.9.9.9:8080", info.ControllerURL)
	}
	calls := rec.snapshot()
	if !hasCall(calls, "get pod -l app=amp-controller") {
		t.Error("an unhealthy ClusterIP must fall back to resolving the controller pod IP")
	}
	if hasCall(calls, "port-forward") {
		t.Error("the pod-IP fallback must never invoke kubectl port-forward")
	}
}

// TestDeployPortforwardModeStillUsesForward asserts the explicit portforward
// reach mode is unaffected by the tailnet default: it still opens (and holds)
// the kubectl port-forward, exactly as before this change.
func TestDeployPortforwardModeStillUsesForward(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{HelmShim: false, ControllerReach: ReachPortForward}, rec)

	info, err := a.Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if info.ControllerURL != "http://127.0.0.1:18080" {
		t.Errorf("controller url = %q, want the port-forward local address", info.ControllerURL)
	}
	if info.Forward == nil {
		t.Error("portforward mode must hold a port-forward")
	}
	if !hasCall(rec.snapshot(), "port-forward svc/amp-controller") {
		t.Error("portforward mode must invoke kubectl port-forward")
	}
}

func TestDeployFiltersClusterScopedRBACWhenNamespaceScoped(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{RBACClusterScoped: false, HelmShim: false}, rec)
	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	applied := appliedManifests(rec.snapshot())
	if anyContains(applied, "kind: ClusterRole") {
		t.Error("ClusterRole/ClusterRoleBinding must be filtered out when rbac_cluster_scoped=false")
	}
	// The namespace-scoped Role and the Deployment must survive.
	if !anyContains(applied, "kind: Role") || !anyContains(applied, "kind: Deployment") {
		t.Error("namespace-scoped Role and the controller Deployment must still be applied")
	}
	// And helm was told clusterScoped=false.
	if !hasCall(rec.snapshot(), "rbac.clusterScoped=false") {
		t.Error("helm template must set rbac.clusterScoped=false")
	}
}

func TestDeployKeepsClusterScopedRBACWhenClusterScoped(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{RBACClusterScoped: true, HelmShim: false}, rec)
	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	applied := appliedManifests(rec.snapshot())
	if !anyContains(applied, "kind: ClusterRole") || !anyContains(applied, "kind: ClusterRoleBinding") {
		t.Error("ClusterRole/ClusterRoleBinding must be applied when rbac_cluster_scoped=true")
	}
	if !hasCall(rec.snapshot(), "rbac.clusterScoped=true") {
		t.Error("helm template must set rbac.clusterScoped=true")
	}
}

func TestDeployInstallsHelmShimWhenEnabled(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{HelmShim: true}, rec)
	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	calls := rec.snapshot()
	if !anyContains(appliedManifests(calls), shimConfigMapName) {
		t.Error("helm_shim=true must apply the shim ConfigMap")
	}
	if !hasCall(calls, "patch deployment amp-controller") {
		t.Error("helm_shim=true must patch the controller deployment")
	}
	// The shim strips --create-namespace and delegates to the real helm.
	var shim string
	for _, c := range appliedManifests(calls) {
		if strings.Contains(c, shimConfigMapName) {
			shim = c
		}
	}
	if !strings.Contains(shim, "--create-namespace") || !strings.Contains(shim, "/usr/local/bin/helm") {
		t.Errorf("shim ConfigMap missing the create-namespace strip / REAL_HELM:\n%s", shim)
	}
}

func TestDeploySkipsHelmShimWhenDisabled(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{HelmShim: false}, rec)
	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	calls := rec.snapshot()
	if anyContains(appliedManifests(calls), shimConfigMapName) {
		t.Error("helm_shim=false must not apply the shim ConfigMap")
	}
	if hasCall(calls, "patch deployment") {
		t.Error("helm_shim=false must not patch the controller deployment")
	}
}

func TestDeployMintsECRPullSecretWhenNoB64(t *testing.T) {
	rec := newRecorder()
	tokenCalled := false
	a := testApp(Config{HelmShim: false}, rec)
	a.WithECRToken(func(context.Context, string, string) (string, error) {
		tokenCalled = true
		return "sekret-token", nil
	})
	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !tokenCalled {
		t.Error("Deploy must resolve an ECR token when no pre-encoded pull secret is given")
	}
	if !anyContains(appliedManifests(rec.snapshot()), "kubernetes.io/dockerconfigjson") {
		t.Error("Deploy must apply a docker-registry pull secret")
	}
}

func TestDeployUsesPreEncodedPullSecret(t *testing.T) {
	rec := newRecorder()
	tokenCalled := false
	a := testApp(Config{HelmShim: false, PullSecretDockerConfigB64: "cHJlLWVuY29kZWQ="}, rec)
	a.WithECRToken(func(context.Context, string, string) (string, error) {
		tokenCalled = true
		return "", nil
	})
	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if tokenCalled {
		t.Error("a pre-encoded pull secret must skip the aws ecr get-login-password path")
	}
	if !anyContains(appliedManifests(rec.snapshot()), "cHJlLWVuY29kZWQ=") {
		t.Error("the pre-encoded dockerconfigjson must be applied verbatim")
	}
}

func TestDeployRequiresControllerSource(t *testing.T) {
	rec := newRecorder()
	cfg := Config{Namespace: "ampqa-test", Context: "test-ctx", HelmShim: false}
	a := New(cfg)
	a.logf = nil
	kube := k8s.New(k8s.Config{Context: "test-ctx", Namespace: "ampqa-test"}).WithRunner(rec.run).WithStarter(rec.start)
	a.WithKube(kube).WithHelmRunner(rec.run).
		WithReadyFunc(func(context.Context, *controller.Client, time.Duration) error { return nil }).
		WithECRToken(func(context.Context, string, string) (string, error) { return "t", nil })
	if _, err := a.Deploy(context.Background()); err == nil {
		t.Error("Deploy must fail with neither controller_chart nor controller_manifest")
	}
}

func TestDeployRawManifestSkipsHelm(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{ControllerManifest: "kind: Deployment\nmetadata:\n  name: amp-controller\n", HelmShim: false}, rec)
	// ControllerManifest set → chart cleared so helm is never shelled.
	a.cfg.ControllerChart = ""
	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if hasCall(rec.snapshot(), "helm template") {
		t.Error("a raw controller manifest must not shell helm template")
	}
	if !anyContains(appliedManifests(rec.snapshot()), "name: amp-controller") {
		t.Error("the raw controller manifest must be applied")
	}
}

// ensureRec records the external-backing CREATE DATABASE calls.
type ensureRec struct {
	mu    sync.Mutex
	calls []struct {
		ci     pg.ConnInfo
		dbName string
	}
}

func (e *ensureRec) fn(_ context.Context, ci pg.ConnInfo, dbName string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, struct {
		ci     pg.ConnInfo
		dbName string
	}{ci, dbName})
	return nil
}

func (e *ensureRec) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func externalApp(cfg Config, rec *recorder, ens *ensureRec) *App {
	if cfg.Backing.Mode == "" {
		cfg.Backing = Backing{Mode: BackingExternal, HostPorts: "10.0.0.1:5433,10.0.0.2:5433,10.0.0.3:5433"}
	}
	a := testApp(cfg, rec)
	a.WithEnsureDB(ens.fn)
	return a
}

func TestDeployExternalBackingSkipsYugabytedAndEnsuresDB(t *testing.T) {
	rec := newRecorder()
	ens := &ensureRec{}
	a := externalApp(Config{HelmShim: false}, rec, ens)

	info, err := a.Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	calls := rec.snapshot()
	// No yugabyted StatefulSet apply, no statefulset wait, no in-pod db create.
	if anyContains(appliedManifests(calls), "kind: StatefulSet") {
		t.Error("external backing must NOT apply the yugabyted StatefulSet")
	}
	if hasCall(calls, "wait statefulset/yugabyted") {
		t.Error("external backing must NOT wait on the yugabyted statefulset")
	}
	if hasCall(calls, "exec yugabyted-0") {
		t.Error("external backing must NOT exec into a yugabyted pod to create the DB")
	}
	// The external yb_storage was ensured against the first CSV host.
	if ens.count() != 1 {
		t.Fatalf("ensureDB called %d times, want 1", ens.count())
	}
	got := ens.calls[0]
	if got.dbName != DefaultDBName {
		t.Errorf("ensureDB dbName = %q, want %q", got.dbName, DefaultDBName)
	}
	if got.ci.Host != "10.0.0.1" || got.ci.Port != 5433 {
		t.Errorf("ensureDB target = %s:%d, want 10.0.0.1:5433", got.ci.Host, got.ci.Port)
	}
	if got.ci.Database != ybMaintenanceDB {
		t.Errorf("ensureDB maintenance database = %q, want %q", got.ci.Database, ybMaintenanceDB)
	}
	// The controller helm render points endpoints at the external CSV, passed
	// via --set-string with commas escaped (a raw --set/--set-string value
	// would have the CSV's commas split into bogus extra key=value pairs) —
	// the "\," between each host:port entry is the round-trip check.
	if !hasCall(calls, `--set-string controller.endpoint.ybHostPorts=10.0.0.1:5433\,10.0.0.2:5433\,10.0.0.3:5433`) {
		t.Errorf("helm template must set controller.endpoint.ybHostPorts to the escaped external CSV via --set-string\ncalls: %v", calls)
	}
	if !hasCall(calls, "controller.databaseUrl=postgres://yugabyte@10.0.0.1:5433/yb_storage?sslmode=disable") {
		t.Errorf("helm template must set controller.databaseUrl to the external DSN\ncalls: %v", calls)
	}
	// The controller still deploys and the port-forward/health still run.
	if info.ControllerURL != "http://127.0.0.1:18080" {
		t.Errorf("controller url = %q", info.ControllerURL)
	}
	if !strings.Contains(info.Backing, "external") {
		t.Errorf("StackInfo.Backing = %q, want it to describe the external store", info.Backing)
	}
	if _, ok := a.images()["yugabyted"]; ok {
		t.Error("external backing must not report a yugabyted image")
	}
}

func TestDeployExternalBackingCarriesUserPasswordSSL(t *testing.T) {
	rec := newRecorder()
	ens := &ensureRec{}
	a := externalApp(Config{
		HelmShim: false,
		Backing: Backing{
			Mode:      BackingExternal,
			HostPorts: "db.example.com:5433",
			User:      "amp",
			Password:  "s3cret",
			DBName:    "amp_store",
			SSLMode:   "require",
		},
	}, rec, ens)
	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	got := ens.calls[0]
	if got.ci.User != "amp" || got.ci.Password != "s3cret" || got.ci.SSLMode != "require" {
		t.Errorf("ensureDB conn = %+v, want user=amp password=s3cret sslmode=require", got.ci)
	}
	if got.dbName != "amp_store" {
		t.Errorf("ensureDB dbName = %q, want amp_store", got.dbName)
	}
	calls := rec.snapshot()
	if !hasCall(calls, "controller.databaseUrl=postgres://amp:s3cret@db.example.com:5433/amp_store?sslmode=require") {
		t.Errorf("databaseUrl must carry user/password/db/sslmode\ncalls: %v", calls)
	}
	if !hasCall(calls, "controller.endpoint.ybUser=amp") || !hasCall(calls, "controller.endpoint.ybDbname=amp_store") {
		t.Error("helm template must set endpoint ybUser/ybDbname from the external backing")
	}
}

func TestDeployExternalBackingRequiresHostPorts(t *testing.T) {
	rec := newRecorder()
	ens := &ensureRec{}
	a := externalApp(Config{HelmShim: false, Backing: Backing{Mode: BackingExternal}}, rec, ens)
	if _, err := a.Deploy(context.Background()); err == nil {
		t.Error("external backing with no host_ports must fail")
	}
	if ens.count() != 0 {
		t.Error("ensureDB must not run without host_ports")
	}
}

func TestDeployBuiltinBackingNeverEnsuresExternalDB(t *testing.T) {
	rec := newRecorder()
	ens := &ensureRec{}
	a := testApp(Config{HelmShim: false}, rec)
	a.WithEnsureDB(ens.fn)
	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if ens.count() != 0 {
		t.Error("builtin backing must never touch an external cluster (ensureDB)")
	}
	// Regression: builtin still deploys the yugabyted StatefulSet in-pod.
	if !anyContains(appliedManifests(rec.snapshot()), "kind: StatefulSet") {
		t.Error("builtin backing must still apply the yugabyted StatefulSet")
	}
}

func TestExternalBackingTeardownOnlyDeletesNamespace(t *testing.T) {
	rec := newRecorder()
	ens := &ensureRec{}
	rec.on("get namespace", exec.Result{Stdout: ManagedByValue})
	a := externalApp(Config{}, rec, ens)
	if err := a.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !hasCall(rec.snapshot(), "delete namespace ampqa-test") {
		t.Error("teardown must delete the managed namespace")
	}
	if ens.count() != 0 {
		t.Error("teardown must never touch the external backing store")
	}
}

func TestCollectExternalBackingSkipsYugabytedLogs(t *testing.T) {
	rec := newRecorder()
	ens := &ensureRec{}
	rec.on("logs deployment/amp-controller", exec.Result{Stdout: "controller log line\n"})
	a := externalApp(Config{}, rec, ens)
	sink := &mapSink{blobs: map[string][]byte{}}
	if err := a.Collect(context.Background(), sink); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, ok := sink.blobs["yugabyted.log"]; ok {
		t.Error("external backing must not collect a yugabyted log")
	}
	if hasCall(rec.snapshot(), "logs statefulset/yugabyted") {
		t.Error("external backing must not request yugabyted logs")
	}
}

func TestFirstHostPort(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port int
	}{
		{"10.0.0.1:5433,10.0.0.2:5433", "10.0.0.1", 5433},
		{"db.example.com:6000", "db.example.com", 6000},
		{"barehost", "barehost", 5433},
		{" spaced:5433 , x:1", "spaced", 5433},
	}
	for _, c := range cases {
		h, p := firstHostPort(c.in)
		if h != c.host || p != c.port {
			t.Errorf("firstHostPort(%q) = (%q,%d), want (%q,%d)", c.in, h, p, c.host, c.port)
		}
	}
}

func TestTeardownRefusesUnmanagedNamespace(t *testing.T) {
	rec := newRecorder()
	// get namespace label returns empty → not ampqa-managed.
	rec.on("get namespace", exec.Result{Stdout: ""})
	a := testApp(Config{}, rec)
	if err := a.Teardown(context.Background()); err == nil {
		t.Error("Teardown must refuse a namespace lacking the ampqa-managed-by label")
	}
	if hasCall(rec.snapshot(), "delete namespace") {
		t.Error("Teardown must not delete an unmanaged namespace")
	}
}

func TestTeardownDeletesManagedNamespace(t *testing.T) {
	rec := newRecorder()
	rec.on("get namespace", exec.Result{Stdout: ManagedByValue})
	a := testApp(Config{}, rec)
	if err := a.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !hasCall(rec.snapshot(), "delete namespace ampqa-test") {
		t.Errorf("Teardown must delete the managed namespace\ncalls: %v", rec.snapshot())
	}
}

func TestStatusMapsReadiness(t *testing.T) {
	rec := newRecorder()
	rec.on("get pods -o name", exec.Result{Stdout: "pod/amp-controller-abc\npod/yugabyted-0\n"})
	rec.on("get pod amp-controller-abc", exec.Result{Stdout: "Running"})
	rec.on("get pod yugabyted-0", exec.Result{Stdout: "Running"})
	rec.on("get statefulset yugabyted", exec.Result{Stdout: "1"})
	rec.on("get deployment amp-controller", exec.Result{Stdout: "1"})

	// A controller that answers /healthz, reached via the injected client.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := testApp(Config{}, rec)
	a.WithClientFactory(func(string) *controller.Client { return controller.New(srv.URL) })

	st, err := a.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.YugabytedReady {
		t.Error("YugabytedReady should be true")
	}
	if !st.ControllerReady {
		t.Error("ControllerReady should be true")
	}
	if !st.Healthy {
		t.Error("Healthy should be true (controller answered /healthz)")
	}
	if st.Pods["amp-controller-abc"] != "Running" {
		t.Errorf("pod phase = %q, want Running", st.Pods["amp-controller-abc"])
	}
}

func TestCollectGathersControllerAndYugabytedLogs(t *testing.T) {
	rec := newRecorder()
	rec.on("logs deployment/amp-controller", exec.Result{Stdout: "controller log line\n"})
	rec.on("logs statefulset/yugabyted", exec.Result{Stdout: "yugabyted log line\n"})
	a := testApp(Config{}, rec)

	sink := &mapSink{blobs: map[string][]byte{}}
	if err := a.Collect(context.Background(), sink); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !strings.Contains(string(sink.blobs["controller.log"]), "controller log line") {
		t.Errorf("controller.log missing: %q", sink.blobs["controller.log"])
	}
	if !strings.Contains(string(sink.blobs["yugabyted.log"]), "yugabyted log line") {
		t.Errorf("yugabyted.log missing: %q", sink.blobs["yugabyted.log"])
	}
}

type mapSink struct{ blobs map[string][]byte }

func (m *mapSink) Add(name, _ string, content []byte) error {
	m.blobs[name] = content
	return nil
}

// --- unit helpers ---

func TestSplitImageRef(t *testing.T) {
	cases := []struct{ in, repo, tag string }{
		{"amp/controller:latest", "amp/controller", "latest"},
		{"454529406029.dkr.ecr.us-west-2.amazonaws.com/amp/controller:20260618", "454529406029.dkr.ecr.us-west-2.amazonaws.com/amp/controller", "20260618"},
		{"amp/controller", "amp/controller", ""},
		{"registry:5000/amp/controller:v1", "registry:5000/amp/controller", "v1"},
	}
	for _, c := range cases {
		repo, tag := splitImageRef(c.in)
		if repo != c.repo || tag != c.tag {
			t.Errorf("splitImageRef(%q) = (%q,%q), want (%q,%q)", c.in, repo, tag, c.repo, c.tag)
		}
	}
}

func TestWithRegistry(t *testing.T) {
	reg := "454529406029.dkr.ecr.us-west-2.amazonaws.com"
	if got := withRegistry("amp/controller:latest", reg); got != reg+"/amp/controller:latest" {
		t.Errorf("bare repo not prefixed: %q", got)
	}
	if got := withRegistry(reg+"/amp/controller:latest", reg); got != reg+"/amp/controller:latest" {
		t.Errorf("registry-qualified ref must be unchanged: %q", got)
	}
	if got := withRegistry("yugabytedb/yugabyte:latest", ""); got != "yugabytedb/yugabyte:latest" {
		t.Errorf("empty registry must pass through: %q", got)
	}
}

func TestHelmEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"172.151.19.236:5433,172.151.29.66:5433", `172.151.19.236:5433\,172.151.29.66:5433`},
		{"postgres://amp:s3cret@host:5433/db?sslmode=disable", "postgres://amp:s3cret@host:5433/db?sslmode=disable"},
		{`back\slash`, `back\\slash`},
		{"", ""},
	}
	for _, c := range cases {
		if got := helmEscape(c.in); got != c.want {
			t.Errorf("helmEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Two-node CSV round-trips: the escaped form carries exactly one "\," in
	// place of the separating comma, which helm un-escapes back to "," when
	// it renders the chart.
	csv := "10.0.0.1:5433,10.0.0.2:5433"
	escaped := helmEscape(csv)
	if want := `10.0.0.1:5433\,10.0.0.2:5433`; escaped != want {
		t.Errorf("helmEscape(%q) = %q, want %q", csv, escaped, want)
	}
	if strings.Count(escaped, `\,`) != 1 {
		t.Errorf("expected exactly one escaped comma in %q", escaped)
	}
}

func TestFilterManifestsRobustSplit(t *testing.T) {
	// A doc whose body merely mentions ClusterRole in a comment must survive;
	// only a top-level kind: ClusterRole is dropped.
	in := "kind: ConfigMap\ndata:\n  note: mentions ClusterRole here\n---\nkind: ClusterRole\nmetadata:\n  name: x\n"
	out := filterManifests(in, true)
	if !strings.Contains(out, "ConfigMap") {
		t.Error("ConfigMap doc must survive")
	}
	if strings.Contains(out, "kind: ClusterRole") {
		t.Error("ClusterRole doc must be dropped")
	}
	if strings.Contains(out, "mentions ClusterRole") == false {
		t.Error("the ConfigMap's ClusterRole mention must be preserved")
	}
}

func assertOrder(t *testing.T, got []call, want ...string) {
	t.Helper()
	idx := 0
	for _, g := range got {
		if idx < len(want) && strings.Contains(g.argv, want[idx]) {
			idx++
		}
	}
	if idx != len(want) {
		var argvs []string
		for _, g := range got {
			argvs = append(argvs, g.argv)
		}
		t.Errorf("commands not in expected order; matched %d/%d\ngot:  %v\nwant: %v", idx, len(want), argvs, want)
	}
}
