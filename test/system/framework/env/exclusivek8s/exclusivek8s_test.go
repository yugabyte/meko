package exclusivek8s

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/ampstack"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/controller"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/system/framework/core"
	"github.com/yugabyte/yb-amp/test/system/framework/exec"
	"github.com/yugabyte/yb-amp/test/system/framework/exec/k8s"
)

// recorder records every kubectl and helm argv, answers by substring match, and
// hands PortForward a canned "Forwarding from" stdout. It always reports the
// ampqa managed-by label for namespace gets, so ampstack.Teardown proceeds.
type recorder struct {
	mu    sync.Mutex
	calls []string
	rules map[string]exec.Result
}

func newRecorder() *recorder {
	return &recorder{rules: map[string]exec.Result{
		// ampstack.Teardown reads this label before deleting a namespace.
		"get namespace": {Stdout: "ampstack"},
	}}
}

func (r *recorder) run(_ context.Context, stdin io.Reader, argv []string) (exec.Result, error) {
	joined := strings.Join(argv, " ")
	if stdin != nil {
		if b, err := io.ReadAll(stdin); err == nil && len(b) > 0 {
			joined += " <<< " + string(b)
		}
	}
	r.mu.Lock()
	r.calls = append(r.calls, joined)
	r.mu.Unlock()
	for sub, res := range r.rules {
		if strings.Contains(joined, sub) {
			return res, nil
		}
	}
	return exec.Result{ExitCode: 0}, nil
}

func (r *recorder) start(_ context.Context, argv []string) (k8s.Process, error) {
	r.mu.Lock()
	r.calls = append(r.calls, strings.Join(argv, " "))
	r.mu.Unlock()
	return &fakeProc{out: strings.NewReader("Forwarding from 127.0.0.1:18080 -> 8080\n")}, nil
}

func (r *recorder) argvs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

type fakeProc struct{ out io.Reader }

func (p *fakeProc) Stdout() io.Reader { return p.out }
func (p *fakeProc) Kill() error       { return nil }
func (p *fakeProc) Wait() error       { return nil }

func testProvider(cfg Config, rec *recorder) *Provider {
	cfg.Context = "itest-dev"
	if cfg.ControllerReach == "" {
		// These tests predate ControllerReach and assert the port-forward argv
		// sequence specifically; pin it so ampstack's tailnet default doesn't
		// change their behavior. A tailnet-mode test sets it explicitly.
		cfg.ControllerReach = ampstack.ReachPortForward
	}
	p := New(cfg)
	p.logf = nil
	p.kube = k8s.New(k8s.Config{Context: cfg.Context}).WithRunner(rec.run).WithStarter(rec.start)
	p.helmRun = rec.run
	p.waitReady = func(context.Context, *controller.Client, time.Duration) error { return nil }
	// No real backing YugabyteDB to dial in a unit test; default to "no live
	// detection" unless a test overrides this.
	p.detectYB = func(context.Context, pg.ConnInfo) string { return "" }
	return p
}

func TestProvisionComposesAmpstackHelmSequence(t *testing.T) {
	rec := newRecorder()
	p := testProvider(Config{
		HelmChart:     "./charts/yb-amp-controller",
		PullSecretB64: "ZG9ja2Vy",
	}, rec)

	env, err := p.Provision(context.Background(), core.EnvSpec{Sharing: core.Exclusive}, core.NewRunSeed(42, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if env.ControllerURL() != "http://127.0.0.1:18080" {
		t.Errorf("controller url = %q", env.ControllerURL())
	}
	argvs := rec.argvs()
	assertOrder(t, argvs,
		"apply -f -",                     // namespace
		"apply -f -",                     // pull secret
		"apply -f -",                     // yugabyted
		"wait statefulset/yugabyted",     // ready gate
		"exec yugabyted-0",               // yb_storage db
		"helm template amp-controller",   // controller render (via ampstack)
		"wait deployment/amp-controller", // controller available
		"port-forward svc/amp-controller 18080:8080",
	)
	// The namespace carries the reaper timestamp label and the managed-by label.
	if !containsAny(argvs, "ampqa-created") {
		t.Error("namespace should carry the reaper label (ampqa-created)")
	}
	if !containsAny(argvs, "ampqa-managed-by") {
		t.Error("namespace should carry the ampqa-managed-by label")
	}
}

// TestProvisionYBVersionLiveDetected: once the dedicated stack's own backing
// YugabyteDB is up, its version (simulated here via the detectYB seam,
// standing in for a live pg connection to app.BackingConnInfo()) is recorded
// on the env — the stack has no other source for it (no static "yb version"
// config for this per-test backing).
func TestProvisionYBVersionLiveDetected(t *testing.T) {
	rec := newRecorder()
	p := testProvider(Config{HelmChart: "./charts/yb-amp-controller"}, rec)
	p.detectYB = func(context.Context, pg.ConnInfo) string { return "2.21.1.0-b1" }

	env, err := p.Provision(context.Background(), core.EnvSpec{Sharing: core.Exclusive}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := env.Versions().YB; got != "2.21.1.0-b1" {
		t.Errorf("yb version = %q, want the live-detected 2.21.1.0-b1", got)
	}
}

// TestProvisionYBVersionFallsBackOnDetectionFailure: an unreachable/not-yet-
// ready backing store must not block or fail provisioning — the version is
// simply left empty, the sentinel core.VersionSpec.Qualifies already treats
// as "don't enforce this bound".
func TestProvisionYBVersionFallsBackOnDetectionFailure(t *testing.T) {
	rec := newRecorder()
	p := testProvider(Config{HelmChart: "./charts/yb-amp-controller"}, rec)
	p.detectYB = func(context.Context, pg.ConnInfo) string { return "" }

	env, err := p.Provision(context.Background(), core.EnvSpec{Sharing: core.Exclusive}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := env.Versions().YB; got != "" {
		t.Errorf("yb version = %q, want empty on detection failure", got)
	}
}

func TestProvisionManifestPathWhenNoChart(t *testing.T) {
	rec := newRecorder()
	p := testProvider(Config{ManifestPath: "/tmp/controller.yaml"}, rec)
	p.readManifest = func(string) (string, error) { return "kind: Deployment\n", nil }

	if _, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true)); err != nil {
		t.Fatalf("provision: %v", err)
	}
	for _, a := range rec.argvs() {
		if strings.HasPrefix(a, "helm ") {
			t.Errorf("must not shell helm when no chart configured: %q", a)
		}
	}
	// namespace + yugabyted + manifest = 3 applies (no pull secret configured).
	applies := 0
	for _, a := range rec.argvs() {
		if strings.Contains(a, "apply -f -") {
			applies++
		}
	}
	if applies != 3 {
		t.Errorf("want 3 applies (ns, yugabyted, controller manifest), got %d", applies)
	}
}

func TestProvisionRequiresControllerDeploySource(t *testing.T) {
	rec := newRecorder()
	p := testProvider(Config{}, rec) // no chart, no manifest
	if _, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true)); err == nil {
		t.Error("provision must fail without helm_chart or controller_manifest")
	}
	// The failed provision must have torn its namespace back down.
	if !containsAny(rec.argvs(), "delete namespace") {
		t.Error("failed provision must delete its namespace")
	}
}

func TestTeardownDeletesNamespace(t *testing.T) {
	rec := newRecorder()
	p := testProvider(Config{HelmChart: "chart"}, rec)
	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(9, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	ns := env.(*Env).namespace
	if !containsAny(rec.argvs(), "delete namespace "+ns) {
		t.Errorf("teardown must delete namespace %s\ngot: %v", ns, rec.argvs())
	}
}

func TestAttachUnsupported(t *testing.T) {
	if _, err := New(Config{Context: "x"}).Attach(context.Background(), "ref"); err == nil {
		t.Error("attach must be unsupported for exclusivek8s")
	}
}

func TestConfigured(t *testing.T) {
	if (Config{}).Configured() {
		t.Error("empty config must not be configured")
	}
	if !(Config{Context: "itest-dev"}).Configured() {
		t.Error("context must configure the provider")
	}
}

func assertOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	idx := 0
	for _, g := range got {
		if idx < len(want) && strings.Contains(g, want[idx]) {
			idx++
		}
	}
	if idx != len(want) {
		t.Errorf("commands not in expected order; matched %d/%d\ngot:  %v\nwant: %v", idx, len(want), got, want)
	}
}

func containsAny(got []string, sub string) bool {
	for _, g := range got {
		if strings.Contains(g, sub) {
			return true
		}
	}
	return false
}
