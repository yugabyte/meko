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

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// renderedChart is a canned `helm template` output: a ServiceAccount + Role +
// RoleBinding + Deployment plus a cluster-scoped ClusterRole/ClusterRoleBinding,
// so a test can assert every rendered doc is applied as the chart wrote it.
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
spec:
  template:
    spec:
      containers:
        - name: controller
          image: amp/controller:test
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

// TestControllerForwardHealsItselfWhenItsProcessDies: the kubectl process
// backing the held forward exits on its own (the real bug — "process already
// finished"). The forward re-establishes itself on the SAME local port, so the
// handle never goes dead, the URL the caller already holds keeps working, and
// ampstack has nothing to reconnect. This is the failure that produced
// "controller unreachable" while the controller was healthy.
func TestControllerForwardHealsItselfWhenItsProcessDies(t *testing.T) {
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
	if !waitUntil(func() bool { return info.Forward.Restarts() > 0 }, 5*time.Second) {
		t.Fatal("a dead kubectl must be re-established by the forward itself")
	}
	if !info.Forward.Alive() {
		t.Error("the handle must stay alive across the restart")
	}

	cl, url, err := a.ControllerClient(context.Background())
	if err != nil {
		t.Fatalf("ControllerClient: %v", err)
	}
	if url != info.ControllerURL {
		t.Errorf("url = %q, want the original %q — a self-healed forward keeps its port", url, info.ControllerURL)
	}
	if cl == nil || cl.BaseURL() != url {
		t.Errorf("client base url = %q, want %q", cl.BaseURL(), url)
	}

	// A second call must not open anything further: the forward is alive.
	before := df.count()
	if _, url2, err := a.ControllerClient(context.Background()); err != nil || url2 != url {
		t.Errorf("ControllerClient(again) = (%q, %v), want the same url with no error", url2, err)
	}
	if df.count() != before {
		t.Errorf("port-forward opened again (%d → %d); an alive forward must be reused", before, df.count())
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

// TestDeployTailnetFallsBackToExecRelayWhenNoDirectAddress asserts the
// production case that used to ride a kubectl port-forward (GKE amp-dev: the
// coordinator has no route to the cluster's pod range nor its services range):
// neither the ClusterIP nor the pod IP answers, so tailnet mode continues over
// the exec relay — a pod, not a local process that can die mid-run — and no
// forward is opened at all.
func TestDeployTailnetFallsBackToExecRelayWhenNoDirectAddress(t *testing.T) {
	rec := newRecorder()
	rec.on("get svc amp-controller", exec.Result{Stdout: "34.118.224.5"})
	rec.on("get pod -l app=amp-controller", exec.Result{Stdout: "10.21.128.7"})
	bad := healthzServer(false)
	defer bad.Close()

	a := testApp(Config{HelmShim: false, ControllerReach: ReachTailnet}, rec)
	a.WithClientFactory(func(string) *controller.Client { return controller.New(bad.URL) })
	var logged []string
	a.WithLogf(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })

	info, err := a.Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if info.ControllerURL != "http://amp-controller:8080" {
		t.Errorf("controller url = %q, want the in-cluster service address the relay dials", info.ControllerURL)
	}
	if info.Forward != nil {
		t.Error("the exec relay must not open a port-forward")
	}
	calls := rec.snapshot()
	if !hasStdin(calls, relayPodName) {
		t.Error("the relay pod manifest was never applied")
	}
	if !hasCall(calls, "wait pod/"+relayPodName) {
		t.Error("Deploy must wait for the relay pod to be Ready")
	}
	if hasCall(calls, "port-forward svc/amp-controller") {
		t.Error("no port-forward may be opened when the relay is up")
	}
	joined := strings.Join(logged, "\n")
	for _, want := range []string{"34.118.224.5", "10.21.128.7", "exec relay"} {
		if !strings.Contains(joined, want) {
			t.Errorf("fallback log %q must mention %q", joined, want)
		}
	}

	// The relay is the reach from here on: a client fetch reports the
	// in-cluster address, never an unreachable direct URL.
	_, url, err := a.ControllerClient(context.Background())
	if err != nil {
		t.Fatalf("ControllerClient: %v", err)
	}
	if url != "http://amp-controller:8080" {
		t.Errorf("ControllerClient url = %q, want the relay's in-cluster address", url)
	}
}

// hasStdin reports whether any recorded call carried sub on its stdin.
func hasStdin(calls []call, sub string) bool {
	for _, c := range calls {
		if strings.Contains(c.stdin, sub) {
			return true
		}
	}
	return false
}

// TestDeployFallsBackToPortForwardWhenTheRelayCannotStart pins the last
// resort: no direct address AND the relay pod cannot come up (image pull,
// quota) — only then does the old kubectl port-forward path carry the run.
func TestDeployFallsBackToPortForwardWhenTheRelayCannotStart(t *testing.T) {
	rec := newRecorder()
	rec.on("get svc amp-controller", exec.Result{Stdout: "34.118.224.5"})
	rec.on("get pod -l app=amp-controller", exec.Result{Stdout: "10.21.128.7"})
	rec.on("wait pod/"+relayPodName, exec.Result{ExitCode: 1, Stderr: "timed out waiting for the condition"})
	bad := healthzServer(false)
	defer bad.Close()
	good := healthzServer(true)
	defer good.Close()

	a := testApp(Config{HelmShim: false, ControllerReach: ReachTailnet}, rec)
	a.WithClientFactory(func(url string) *controller.Client {
		if strings.Contains(url, "127.0.0.1") {
			return controller.New(good.URL)
		}
		return controller.New(bad.URL)
	})

	info, err := a.Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if info.ControllerURL != "http://127.0.0.1:18080" {
		t.Errorf("controller url = %q, want the port-forward local address", info.ControllerURL)
	}
	if info.Forward == nil {
		t.Error("the last-resort fallback must hand the caller the port-forward to close")
	}
}

// TestDeployTailnetFallbackErrorCarriesBothFailures asserts that when the
// port-forward fallback ALSO fails, the single returned error carries both the
// direct addresses tried and the forward's own failure — triage in one read.
func TestDeployTailnetFallbackErrorCarriesBothFailures(t *testing.T) {
	rec := newRecorder()
	rec.on("get svc amp-controller", exec.Result{Stdout: "34.118.224.5"})
	rec.on("get pod -l app=amp-controller", exec.Result{Stdout: "10.21.128.7"})
	bad := healthzServer(false)
	defer bad.Close()

	rec.on("wait pod/"+relayPodName, exec.Result{ExitCode: 1, Stderr: "relay image unpullable"})
	a := testApp(Config{HelmShim: false, ControllerReach: ReachTailnet}, rec)
	a.WithClientFactory(func(string) *controller.Client { return controller.New(bad.URL) })
	a.WithKube(k8s.New(k8s.Config{Context: "test-ctx", Namespace: "ampqa-test"}).
		WithRunner(rec.run).
		WithStarter(func(context.Context, []string) (k8s.Process, error) {
			return nil, fmt.Errorf("kubectl not permitted here")
		}))

	_, err := a.Deploy(context.Background())
	if err == nil {
		t.Fatal("deploy must fail when neither the direct addresses nor the port-forward work")
	}
	for _, want := range []string{"34.118.224.5", "10.21.128.7", "kubectl not permitted here"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
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

func TestDeployAppliesTheChartAsRendered(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{HelmShim: false}, rec)
	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	applied := appliedManifests(rec.snapshot())
	if !anyContains(applied, "kind: Role") || !anyContains(applied, "kind: Deployment") {
		t.Error("the rendered Role and controller Deployment must be applied")
	}
	// Nothing is edited out and nothing is overridden: whether the install is
	// cluster-scoped is the chart's call, and rbac.clusterScoped is never set.
	if !anyContains(applied, "kind: ClusterRole") {
		t.Error("docs the chart rendered must be applied as rendered, not filtered")
	}
	if hasCall(rec.snapshot(), "rbac.clusterScoped") {
		t.Error("helm template must not set rbac.clusterScoped at all")
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
	// The shim rides INSIDE the applied Deployment — one apply, one ReplicaSet,
	// one migrator. A post-apply patch would spawn a second pod that races the
	// first one's startup migrations.
	if hasCall(calls, "patch deployment") {
		t.Error("the shim must be injected pre-apply, not patched afterwards")
	}
	var deployDoc string
	for _, c := range appliedManifests(calls) {
		if strings.Contains(c, "kind: Deployment") && strings.Contains(c, "amp-controller") {
			deployDoc = c
		}
	}
	for _, want := range []string{shimConfigMapName, shimMountPath, shimPATH} {
		if !strings.Contains(deployDoc, want) {
			t.Errorf("applied Deployment lacks shim marker %q", want)
		}
	}
	// The shim strips --create-namespace and delegates to the real helm.
	var shim string
	for _, c := range appliedManifests(calls) {
		// The Deployment now references the shim by name too — the ConfigMap is
		// the doc that actually carries the script.
		if strings.Contains(c, "kind: ConfigMap") && strings.Contains(c, shimConfigMapName) {
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
	if anyContains(appliedManifests(calls), shimMountPath) {
		t.Error("helm_shim=false must not inject the shim into the Deployment")
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
		// A non-colocated external cluster: several tservers, addressed by
		// names the coordinator can actually resolve. Names rather than IPs
		// because an IP is invalidated by a reschedule while a name re-resolves
		// on the next dial.
		//
		// Deliberately NOT the per-pod .svc.cluster.local shape. ybbacking never
		// yields that alone — every constructor that produces it (FromYBK8s,
		// FromYBAUniverse, FromProvisionedUniverse) also sets KubeNamespace and
		// a coordinator-reachable AdminHostPorts, because an in-cluster name is
		// unresolvable from here. Putting one here without those would send
		// ensureExternalStorageDB down the dial path against an address the
		// coordinator cannot reach — the failure TestStorageAccessKubeLocated-
		// Forwards exists to forbid. That co-located shape is covered there;
		// this fixture drives the dial path, so it uses a dialable address.
		cfg.Backing = Backing{Mode: BackingExternal, HostPorts: "yb-0.db.example.com:5433,yb-1.db.example.com:5433,yb-2.db.example.com:5433"}
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
	if wantHost := "yb-0.db.example.com"; got.ci.Host != wantHost || got.ci.Port != 5433 {
		t.Errorf("ensureDB target = %s:%d, want %s:5433", got.ci.Host, got.ci.Port, wantHost)
	}
	if got.ci.Database != ybMaintenanceDB {
		t.Errorf("ensureDB maintenance database = %q, want %q", got.ci.Database, ybMaintenanceDB)
	}
	// The controller helm render points endpoints at the external CSV, passed
	// via --set-string with commas escaped (a raw --set/--set-string value
	// would have the CSV's commas split into bogus extra key=value pairs) —
	// the "\," between each host:port entry is the round-trip check.
	if !hasCall(calls, `--set-string controller.endpoint.ybHostPorts=yb-0.db.example.com:5433\,yb-1.db.example.com:5433\,yb-2.db.example.com:5433`) {
		t.Errorf("helm template must set controller.endpoint.ybHostPorts to the escaped external CSV via --set-string\ncalls: %v", calls)
	}
	// The controller's own DSN carries every CSV host, not just the first, so a
	// rescheduled tserver is a failover rather than an outage (meko#961). Same
	// --set-string comma escaping as ybHostPorts above.
	if !hasCall(calls, `--set-string controller.databaseUrl=postgres://yugabyte@yb-0.db.example.com:5433\,yb-1.db.example.com:5433\,yb-2.db.example.com:5433/yb_storage?sslmode=disable`) {
		t.Errorf("helm template must set controller.databaseUrl to the multi-host external DSN\ncalls: %v", calls)
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

// TLS on: the controller chart gets the tls.* block and the endpoint TLS mode,
// endpoints connect as the storage role, and the controller's DSN names the
// mounted certificate instead of a password.
func TestDeployTLSSetsChartValuesAndCertificateDSN(t *testing.T) {
	rec := newRecorder()
	ens := &ensureRec{}
	a := externalApp(Config{HelmShim: false, TLS: TLS{Enabled: true, IssuerName: "qa-ca"}}, rec, ens)

	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	calls := rec.snapshot()
	for _, want := range []string{
		"--set tls.enabled=true",
		"--set tls.issuerRef.kind=ClusterIssuer",
		"--set tls.issuerRef.name=qa-ca",
		"--set tls.issuerRef.group=cert-manager.io",
		"--set tls.caBundleConfigMap=amp-ca-bundle",
		"--set controller.endpoint.ybTlsMode=verify-full",
		"--set controller.endpoint.ybUser=amp_storage",
		`--set-string controller.databaseUrl=postgres://yugabyte@yb-0.db.example.com:5433\,yb-1.db.example.com:5433\,yb-2.db.example.com:5433/yb_storage?sslmode=verify-full&sslrootcert=/etc/amp/tls/ca/ca.crt&sslcert=/etc/amp/tls/client/tls.crt&sslkey=/etc/amp/tls/client/tls.key`,
	} {
		if !hasCall(calls, want) {
			t.Errorf("helm template must carry %q\ncalls: %v", want, calls)
		}
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
		// The real input: a CSV of per-pod StatefulSet DNS, first entry wins.
		{"ybampqa-fkzo-yb-tserver-0.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:5433,ybampqa-fkzo-yb-tserver-1.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:5433", "ybampqa-fkzo-yb-tserver-0.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local", 5433},
		{"db.example.com:6000", "db.example.com", 6000},
		{"barehost", "barehost", 5433},
		{" spaced:5433 , x:1", "spaced", 5433},
		// A bracketed literal is the one address form that parses differently,
		// so it stays covered even though we deploy with names.
		{"[::1]:5433", "::1", 5433},
	}
	for _, c := range cases {
		h, p := firstHostPort(c.in)
		if h != c.host || p != c.port {
			t.Errorf("firstHostPort(%q) = (%q,%d), want (%q,%d)", c.in, h, p, c.host, c.port)
		}
	}
}

func TestHostPortList(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// The real input: every per-pod DNS entry survives, in order.
		{"ybampqa-fkzo-yb-tserver-0.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:5433,ybampqa-fkzo-yb-tserver-1.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:5433", "ybampqa-fkzo-yb-tserver-0.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:5433,ybampqa-fkzo-yb-tserver-1.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:5433"},
		// A name with no port takes the YSQL default, so a CSV that mixes the
		// two still renders a uniform authority.
		{"ybampqa-fkzo-yb-tserver-0.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local,ybampqa-fkzo-yb-tserver-1.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:6000", "ybampqa-fkzo-yb-tserver-0.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:5433,ybampqa-fkzo-yb-tserver-1.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:6000"},
		// Bare hosts and odd ports both normalise to host:port.
		{"a,b:6000", "a:5433,b:6000"},
		{" spaced:5433 , x:1 ", "spaced:5433,x:1"},
		// Empty entries are dropped rather than emitting a ":5433" host.
		{"a:5433,,b:5433,", "a:5433,b:5433"},
		{"", ""},
		{"[::1]:5433", "[::1]:5433"},
	}
	for _, c := range cases {
		if got := hostPortList(c.in); got != c.want {
			t.Errorf("hostPortList(%q) = %q, want %q", c.in, got, c.want)
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

// The chart's default rbac.nodeRead renders a ClusterRole + ClusterRoleBinding
// per install, and a namespace delete leaves cluster-scoped kinds behind.
func TestTeardownSweepsControllerClusterRBAC(t *testing.T) {
	rec := newRecorder()
	rec.on("get namespace", exec.Result{Stdout: ManagedByValue})
	rec.on("get clusterrolebinding", exec.Result{Stdout: "" +
		"ampqa-test-amp-controller-nodes ampqa-test-amp-controller-nodes ampqa-test\n" +
		"other-amp-controller-nodes other-amp-controller-nodes other\n" +
		"ampqa-test-amp-controller-namespaces ampqa-test-amp-controller-namespaces ampqa-test\n"})
	a := testApp(Config{}, rec)
	if err := a.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	calls := rec.snapshot()
	for _, want := range []string{
		"get clusterrolebinding -l " + controllerChartSelector,
		"delete clusterrolebinding ampqa-test-amp-controller-nodes",
		"delete clusterrole ampqa-test-amp-controller-nodes",
		"delete clusterrolebinding ampqa-test-amp-controller-namespaces",
		"delete clusterrole ampqa-test-amp-controller-namespaces",
		"delete namespace ampqa-test",
	} {
		if !hasCall(calls, want) {
			t.Errorf("missing %q\ncalls: %v", want, calls)
		}
	}
	if hasCall(calls, "other-amp-controller-nodes") {
		t.Errorf("Teardown touched another namespace's RBAC\ncalls: %v", calls)
	}
}

func TestReapSweepsControllerClusterRBAC(t *testing.T) {
	rec := newRecorder()
	rec.on("get namespace", exec.Result{Stdout: ManagedByValue})
	rec.on("get clusterrolebinding", exec.Result{Stdout: "ns1-amp-controller-nodes ns1-amp-controller-nodes ns1\n"})
	kube := k8s.New(k8s.Config{}).WithRunner(rec.run)
	if err := reapNamespace(context.Background(), kube, "ns1"); err != nil {
		t.Fatalf("reap: %v", err)
	}
	calls := rec.snapshot()
	for _, want := range []string{"delete clusterrolebinding ns1-amp-controller-nodes", "delete clusterrole ns1-amp-controller-nodes", "delete namespace ns1"} {
		if !hasCall(calls, want) {
			t.Errorf("missing %q\ncalls: %v", want, calls)
		}
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
	csv := "ybampqa-fkzo-yb-tserver-0.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:5433,ybampqa-fkzo-yb-tserver-1.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:5433"
	escaped := helmEscape(csv)
	if want := `ybampqa-fkzo-yb-tserver-0.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:5433\,ybampqa-fkzo-yb-tserver-1.ybampqa-fkzo-yb-tservers.yb-dev-ampqa-5e2503.svc.cluster.local:5433`; escaped != want {
		t.Errorf("helmEscape(%q) = %q, want %q", csv, escaped, want)
	}
	if strings.Count(escaped, `\,`) != 1 {
		t.Errorf("expected exactly one escaped comma in %q", escaped)
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

// mapGetter is a minimal Getter backed by a string map, for resolveYBImage's
// precedence table (only string keys matter here).
type mapGetter map[string]string

func (g mapGetter) GetString(key, def string) string {
	if v, ok := g[key]; ok {
		return v
	}
	return def
}
func (g mapGetter) GetInt(_ string, def int) int                          { return def }
func (g mapGetter) GetBool(_ string, def bool) bool                       { return def }
func (g mapGetter) GetDuration(_ string, def time.Duration) time.Duration { return def }

// TestResolveYBImagePrecedence: a fully-qualified yb_image wins; else the
// registry+version derivation ({registry}:{versions.yb||DefaultYBVersion});
// else the DefaultYBImage fallback when the registry is empty.
func TestResolveYBImagePrecedence(t *testing.T) {
	cases := []struct {
		name string
		m    mapGetter
		want string
	}{
		{
			"explicit yb_image wins over registry+version",
			mapGetter{"env.ampstack.yb_image": "quay.io/x/yb:custom", "env.ampstack.yb_image_registry": "yugabytedb/yugabyte", "versions.yugabytedb": "2025.2.1.0-b141"},
			"quay.io/x/yb:custom",
		},
		{
			// versions.yugabytedb is the awsvm provisioners' LTS pin and must NOT
			// reach amp's yugabyted: it predates ThinClientService.
			"versions.yugabytedb is ignored; the amp default is used",
			mapGetter{"env.ampstack.yb_image_registry": "yugabytedb/yugabyte", "versions.yugabytedb": "2025.2.1.0-b141"},
			"yugabytedb/yugabyte:" + DefaultYBVersion,
		},
		{
			"versions.yb override beats the amp default (preview registry)",
			mapGetter{"env.ampstack.yb_image_registry": "quay.io/yugabyte/yugabyte-itest", "versions.yb": "2.25.1.0-b99", "versions.yugabytedb": "2025.2.1.0-b141"},
			"quay.io/yugabyte/yugabyte-itest:2.25.1.0-b99",
		},
		{
			"no registry → DefaultYBImage",
			mapGetter{"env.ampstack.yb_image_registry": ""},
			DefaultYBImage,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveYBImage(tc.m); got != tc.want {
				t.Errorf("resolveYBImage = %q, want %q", got, tc.want)
			}
		})
	}
}

// A crash-looping controller must carry its OWN logs into the deploy error:
// phase and restart counts prove the loop, only the log says why, and the
// namespace is gone by the time anyone reads the build.
func TestControllerWaitDiagnosticsCarriesCrashLogs(t *testing.T) {
	rec := &recorder{}
	rec.on("get pods -o name", exec.Result{Stdout: "pod/amp-controller-765fbbdc5c-mt65p\npod/yugabyted-0\n"})
	rec.on("logs amp-controller-765fbbdc5c-mt65p --all-containers --prefix --previous",
		exec.Result{Stdout: "apply migrations: boom\n"})
	rec.on("logs amp-controller-765fbbdc5c-mt65p --all-containers --prefix",
		exec.Result{Stdout: "starting controller\n"})
	a := testApp(Config{ControllerName: "amp-controller"}, rec)

	got := a.controllerWaitDiagnostics(context.Background())

	if !strings.Contains(got, "apply migrations: boom") {
		t.Errorf("diagnostics lack the previous instance's log:\n%s", got)
	}
	if !strings.Contains(got, "amp-controller-765fbbdc5c-mt65p logs (previous instance") {
		t.Errorf("diagnostics do not label the crashed pod's log:\n%s", got)
	}
	if !strings.Contains(got, "starting controller") {
		t.Errorf("diagnostics lack the current instance's log:\n%s", got)
	}
	if strings.Contains(got, "yugabyted-0 logs") {
		t.Errorf("diagnostics pulled logs for a pod that is not the controller:\n%s", got)
	}
}

// A co-located backing's storage database is created IN the cluster. Dialling it
// from here needs a route into the pod CIDR, and where that is absent every
// attempt times out against a healthy cluster: three call sites failed that way
// on 0.260810.0 while `select 1` answered from inside the tserver.
func TestColocatedBackingCreatesItsStorageDBInCluster(t *testing.T) {
	rec := newRecorder()
	a := New(Config{
		Namespace: "ampqa-cp-1",
		Backing: Backing{
			Mode:           BackingExternal,
			HostPorts:      "yb-tserver-0.yb-tservers.ampqa-yb-1.svc.cluster.local:5433",
			AdminHostPorts: "10.21.139.6:5433",
			KubeNamespace:  "ampqa-yb-1",
			User:           "yugabyte",
		},
	}).WithKube(k8s.New(k8s.Config{Namespace: "ampqa-cp-1"}).WithRunner(rec.run))

	if err := a.ensureExternalStorageDB(context.Background()); err != nil {
		t.Fatalf("ensureExternalStorageDB: %v", err)
	}

	var execs []string
	for _, c := range rec.snapshot() {
		if strings.Contains(c.argv, "exec") {
			execs = append(execs, c.argv)
		}
		if strings.Contains(c.argv, "10.21.139.6") {
			t.Errorf("dialled the admin host-ports instead of exec-ing in the cluster: %s", c.argv)
		}
	}
	if len(execs) != 1 {
		t.Fatalf("ran %d exec commands, want 1: %v", len(execs), execs)
	}
	for _, want := range []string{"-n ampqa-yb-1", "yb-tserver-0", "CREATE DATABASE", "command -v ysqlsh"} {
		if !strings.Contains(execs[0], want) {
			t.Errorf("exec is missing %q: %s", want, execs[0])
		}
	}
	// The tserver container's workdir is not /home/yugabyte, so an unresolved
	// relative path exits 127 — which already burned one loop iteration.
	if strings.Contains(execs[0], `"$(bin/ysqlsh`) {
		t.Errorf("the script assumes where ysqlsh lives: %s", execs[0])
	}
}

// A genuinely remote backing has no pods here, so it keeps dialling.
func TestRemoteBackingStillDialsItsAdminHostPorts(t *testing.T) {
	a := New(Config{
		Namespace: "ampqa-cp-1",
		Backing: Backing{
			Mode:           BackingExternal,
			HostPorts:      "yb.example.com:5433",
			AdminHostPorts: "yb.example.com:5433",
			User:           "yugabyte",
		},
	})
	if ns := a.cfg.Backing.KubeNamespace; ns != "" {
		t.Fatalf("KubeNamespace = %q, want empty for a remote backing", ns)
	}
}

// lastHelmCmd returns the argv of the most recent `helm template` invocation.
func lastHelmCmd(calls []call) string {
	last := ""
	for _, c := range calls {
		if strings.Contains(c.argv, "helm template") {
			last = c.argv
		}
	}
	return last
}

// TestUpgradeImagesRerendersAndWaitsRollout: an in-place upgrade re-renders the
// chart with both new images, applies what was rendered, and waits on the
// controller rollout.
func TestUpgradeImagesRerendersAndWaitsRollout(t *testing.T) {
	rec := newRecorder()
	// Keyed on the new tag, so the assertion below proves the APPLIED manifest is
	// the one rendered from the upgraded images.
	rec.on("controller.image.tag=v2", exec.Result{
		Stdout: strings.ReplaceAll(renderedChart, "amp/controller:test", QuayNamespace+"/amp-controller:v2"),
	})
	a := testApp(Config{
		Registry:        RegistryQuay,
		ControllerImage: "amp-controller:v1",
		EndpointImage:   "amp-endpoint:v1",
		HelmShim:        false,
	}, rec)

	if err := a.UpgradeImages(context.Background(), "amp-controller:v2", "amp-endpoint:v2"); err != nil {
		t.Fatalf("UpgradeImages: %v", err)
	}
	calls := rec.snapshot()

	helmCmd := lastHelmCmd(calls)
	if !strings.Contains(helmCmd, "controller.image.tag=v2") {
		t.Errorf("controller image not re-rendered: %s", helmCmd)
	}
	if !strings.Contains(helmCmd, "controller.endpoint.image.tag=v2") {
		t.Errorf("endpoint image not re-rendered: %s", helmCmd)
	}
	if !anyContains(appliedManifests(calls), QuayNamespace+"/amp-controller:v2") {
		t.Errorf("applied manifests carry no new controller image: %v", appliedManifests(calls))
	}
	if !hasCall(calls, "rollout status deployment/"+DefaultControllerName) {
		t.Error("no rollout wait issued: a crash-looping new image would pass unnoticed")
	}
}

// An empty argument leaves that image untouched.
func TestUpgradeImagesEmptyArgKeepsImage(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{
		Registry:        RegistryQuay,
		ControllerImage: "amp-controller:v1",
		EndpointImage:   "amp-endpoint:v1",
		HelmShim:        false,
	}, rec)

	if err := a.UpgradeImages(context.Background(), "amp-controller:v2", ""); err != nil {
		t.Fatalf("UpgradeImages: %v", err)
	}
	if a.cfg.EndpointImage != "amp-endpoint:v1" {
		t.Errorf("endpoint image = %q, want it unchanged", a.cfg.EndpointImage)
	}
	helmCmd := lastHelmCmd(rec.snapshot())
	if !strings.Contains(helmCmd, "controller.image.tag=v2") {
		t.Errorf("controller image not upgraded: %s", helmCmd)
	}
	if !strings.Contains(helmCmd, "controller.endpoint.image.tag=v1") {
		t.Errorf("endpoint image changed: %s", helmCmd)
	}
}

// A rollout that never completes must fail the upgrade, not be swallowed.
func TestUpgradeImagesRolloutFailureIsAnError(t *testing.T) {
	rec := newRecorder()
	rec.on("rollout status", exec.Result{ExitCode: 1, Stderr: "error: timed out waiting for the condition"})
	a := testApp(Config{
		Registry:        RegistryQuay,
		ControllerImage: "amp-controller:v1",
		EndpointImage:   "amp-endpoint:v1",
		HelmShim:        false,
	}, rec)

	err := a.UpgradeImages(context.Background(), "amp-controller:v2", "")
	if err == nil {
		t.Fatal("a stalled rollout must fail the upgrade")
	}
	if !strings.Contains(err.Error(), "rollout") {
		t.Errorf("error = %q, want it to name the rollout", err)
	}
}

func TestUpgradeImagesWithoutAnyImageErrors(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{HelmShim: false}, rec)
	if err := a.UpgradeImages(context.Background(), "", ""); err == nil {
		t.Fatal("want an error when no image is given")
	}
	if hasCall(rec.snapshot(), "apply") {
		t.Error("must not re-apply anything when no image changes")
	}
}

// A sampler (and anything else looking for the backing's pods) must be pointed
// at the namespace those pods are actually in. The amp namespace holds no
// tserver when the backing is a co-located ybk8s deploy or a provisioned YBA
// universe, which is how server metrics came back empty for every such run.
func TestBackingNamespacePrefersTheBackingsOwn(t *testing.T) {
	external := New(Config{Namespace: "ampqa-cp-1", Backing: Backing{
		Mode: BackingExternal, KubeNamespace: "yb-dev-ampqa-1",
	}})
	if got := external.BackingNamespace(); got != "yb-dev-ampqa-1" {
		t.Errorf("BackingNamespace = %q, want the backing's own namespace", got)
	}
	// The built-in yugabyted shares the stack's namespace, so that is the answer
	// when the backing names none.
	builtin := New(Config{Namespace: "ampqa-cp-2"})
	if got := builtin.BackingNamespace(); got != "ampqa-cp-2" {
		t.Errorf("BackingNamespace = %q, want the stack namespace", got)
	}
	blank := New(Config{Namespace: "ampqa-cp-3", Backing: Backing{KubeNamespace: "   "}})
	if got := blank.BackingNamespace(); got != "ampqa-cp-3" {
		t.Errorf("BackingNamespace = %q, want the stack namespace for a blank value", got)
	}
}

// Without the declaration a gated document renders as nothing, so the collectors
// go silently absent on a cluster that runs the operator.
func TestRenderDeclaresTheAPIVersionsTheClusterServes(t *testing.T) {
	rec := newRecorder()
	rec.on("get crd opentelemetrycollectors.opentelemetry.io", exec.Result{Stdout: "v1beta1\nv1alpha1\n"})
	a := testApp(Config{HelmShim: false}, rec)

	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	calls := rec.snapshot()

	// The CRD lookup is cluster-scoped: -n would make it fail on a real cluster.
	var probe string
	for _, c := range calls {
		if strings.Contains(c.argv, "get crd opentelemetrycollectors.opentelemetry.io") {
			probe = c.argv
		}
	}
	if probe == "" {
		t.Fatalf("the render must ask the cluster for the otel CRD, calls: %v", calls)
	}
	if strings.Contains(probe, " -n ") {
		t.Errorf("the CRD probe must not be namespaced, got %q", probe)
	}

	var render string
	for _, c := range calls {
		if strings.Contains(c.argv, "helm template") {
			render = c.argv
		}
	}
	for _, want := range []string{
		"--api-versions opentelemetry.io/v1alpha1",
		"--api-versions opentelemetry.io/v1beta1",
	} {
		if !strings.Contains(render, want) {
			t.Errorf("render must declare %s, got %q", want, render)
		}
	}
}

// No operator: the chart skips the collectors and the deploy still succeeds.
func TestRenderDeclaresNothingWhenTheCRDIsAbsent(t *testing.T) {
	rec := newRecorder()
	rec.on("get crd opentelemetrycollectors.opentelemetry.io", exec.Result{ExitCode: 1, Stderr: "Error from server (NotFound)"})
	a := testApp(Config{HelmShim: false}, rec)

	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("an absent optional CRD must not fail the deploy: %v", err)
	}
	for _, c := range rec.snapshot() {
		if strings.Contains(c.argv, "helm template") && strings.Contains(c.argv, "--api-versions") {
			t.Errorf("no --api-versions should be declared when no CRD is served, got %q", c.argv)
		}
	}
}

func TestEndpointNodeSelectorPairs(t *testing.T) {
	got := endpointNodeSelectorPairs("z=1, node.kubernetes.io/instance-type=e2-standard-8")
	want := []string{"node.kubernetes.io/instance-type=e2-standard-8", "z=1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("pairs = %v, want %v (sorted for a stable render)", got, want)
	}
	for _, bad := range []string{"", "novalue", "=v", "k=", ","} {
		if len(endpointNodeSelectorPairs(bad)) != 0 {
			t.Errorf("%q must yield no pairs, got %v", bad, endpointNodeSelectorPairs(bad))
		}
	}
}

func TestDeployBuiltinYugabytedCarriesStreamCompression(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{YBStreamCompression: "gzip", YBTserverGflags: "stream_compression_algo=2"}, rec)
	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	var sts string
	for _, m := range appliedManifests(rec.snapshot()) {
		if strings.Contains(m, "kind: StatefulSet") {
			sts = m
		}
	}
	if !strings.Contains(sts, `--master_flags="enable_stream_compression=true,stream_compression_algo=0"`) {
		t.Errorf("yugabyted master flags lack compression:\n%s", sts)
	}
	if !strings.Contains(sts, "gzip_stream_compression_level=1,stream_compression_algo=1,stream_compression_algo=2") {
		t.Errorf("an explicit tserver gflag must come after the compression default:\n%s", sts)
	}
}

func TestDeployRejectsAnUnknownStreamCompression(t *testing.T) {
	a := testApp(Config{YBStreamCompression: "lz4"}, newRecorder())
	if _, err := a.Deploy(context.Background()); err == nil || !strings.Contains(err.Error(), "env.yb_stream_compression") {
		t.Fatalf("Deploy = %v, want the unknown mode named", err)
	}
}
