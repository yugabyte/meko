package docker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/system/framework/core"
	"github.com/yugabyte/yb-amp/test/system/framework/exec"
)

// scriptExec is a scripted fake transport: records commands, answers by
// substring match, and provides a no-op Close.
type scriptExec struct {
	mu       sync.Mutex
	commands []string
	rules    []rule
	closed   bool
}

type rule struct {
	match string
	res   exec.Result
}

func (s *scriptExec) on(match string, res exec.Result) *scriptExec {
	s.rules = append(s.rules, rule{match, res})
	return s
}

func (s *scriptExec) Run(_ context.Context, cmd string) (exec.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, cmd)
	for _, r := range s.rules {
		if strings.Contains(cmd, r.match) {
			return r.res, nil
		}
	}
	return exec.Result{ExitCode: 0, Stdout: "cid\n"}, nil
}

func (s *scriptExec) Copy(context.Context, string, string) error { return nil }
func (s *scriptExec) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *scriptExec) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.commands))
	copy(out, s.commands)
	return out
}

// happyExec answers inspect/probe/version so provisioning completes in one pass.
func happyExec() *scriptExec {
	s := &scriptExec{}
	s.on("docker inspect", exec.Result{ExitCode: 0, Stdout: "true\n"}).
		on("SELECT 1", exec.Result{ExitCode: 0, Stdout: "1\n"}).
		on("yugabyted version", exec.Result{ExitCode: 0, Stdout: "2.25.0.0\n"})
	return s
}

func newTestProvider(cfg Config, tr *scriptExec) *Provider {
	p := New(cfg)
	p.logf = nil // silence best-effort logs
	p.dial = func(context.Context, Config) (transport, error) { return tr, nil }
	// No real Postgres to dial in a unit test; default to "no live product
	// version" (falls back to the image tag) unless a test overrides this.
	p.detectProduct = func(context.Context, pg.ConnInfo) string { return "" }
	return p
}

func baseConfig() Config {
	return Config{
		Host:          "10.0.0.5",
		EndpointImage: "amp/endpoint:phase1",
		YBImage:       "yugabytedb/yugabyte:latest",
		NameToken:     "tkn",
		YBShared:      true,
	}
}

func TestProvisionSharedHappyPath(t *testing.T) {
	tr := happyExec()
	p := newTestProvider(baseConfig(), tr)

	env, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.SingleContainer}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	e := env.(*Env)
	if e.versions.Product != "phase1" {
		t.Errorf("product version = %q, want phase1", e.versions.Product)
	}
	if e.versions.YB != "2.25.0.0" {
		t.Errorf("yb version = %q, want 2.25.0.0", e.versions.YB)
	}
	if e.pg.Port != DefaultComputePortBase {
		t.Errorf("compute port = %d, want %d", e.pg.Port, DefaultComputePortBase)
	}

	cmds := tr.recorded()
	// Shared YB already running (inspect→true), so no YB run; endpoint runs.
	assertOrder(t, cmds,
		"docker inspect",
		"docker run -d --name 'ampqa-ep-tkn-0'",
	)
	for _, c := range cmds {
		if strings.Contains(c, "docker run") && strings.Contains(c, "ampqa-yb") {
			t.Errorf("shared YB already running must not be re-run: %s", c)
		}
	}
	// Endpoint command carries the required contract env vars.
	epRun := findCmd(cmds, "docker run -d --name 'ampqa-ep-tkn-0'")
	for _, want := range []string{
		"DEV_PROVISION_SCHEMA=true",
		"COMPUTE_PORT=55433",
		"YB_HOST_PORTS=host.docker.internal:5433",
		"-p 55433:55433",
		"--add-host host.docker.internal:host-gateway",
	} {
		if !strings.Contains(epRun, want) {
			t.Errorf("endpoint run missing %q:\n%s", want, epRun)
		}
	}
}

// TestProvisionProductVersionPrefersLiveDetection: when the compute Postgres
// answers SELECT version() with a bare version (simulated here via the
// detectProduct seam — the fake stands in for a live pg connection), the env
// records that live-detected build instead of the static image tag.
func TestProvisionProductVersionPrefersLiveDetection(t *testing.T) {
	tr := happyExec()
	p := newTestProvider(baseConfig(), tr)
	p.detectProduct = func(context.Context, pg.ConnInfo) string { return "1.2.3-b45" }

	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := env.(*Env).versions.Product; got != "1.2.3-b45" {
		t.Errorf("product version = %q, want the live-detected 1.2.3-b45 (not the image tag)", got)
	}
}

// TestProvisionYBVersionFallsBackOnUnparseableDetection: a live yugabyted
// output that isn't a bare version (build changed the command's shape, or it
// errored oddly) must fall back to "" rather than recording garbage — the
// sentinel core.VersionSpec.Qualifies treats as "don't enforce this bound".
func TestProvisionYBVersionFallsBackOnUnparseableDetection(t *testing.T) {
	tr := &scriptExec{}
	tr.on("docker inspect", exec.Result{ExitCode: 0, Stdout: "true\n"}).
		on("SELECT 1", exec.Result{ExitCode: 0, Stdout: "1\n"}).
		on("yugabyted version", exec.Result{ExitCode: 0, Stdout: "unexpected banner text\n"})
	p := newTestProvider(baseConfig(), tr)

	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := env.(*Env).versions.YB; got != "" {
		t.Errorf("yb version = %q, want empty (unparseable detection must fall back, not record prose)", got)
	}
}

func TestTeardownSharedLeavesYB(t *testing.T) {
	tr := happyExec()
	p := newTestProvider(baseConfig(), tr)
	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	cmds := tr.recorded()
	if !hasCmd(cmds, "docker rm -f 'ampqa-ep-tkn-0'") {
		t.Errorf("teardown must remove the endpoint")
	}
	if hasCmd(cmds, "docker rm -f 'ampqa-yb-tkn-shared'") {
		t.Errorf("shared YB must NOT be removed on teardown")
	}
	if !tr.closed {
		t.Errorf("transport must be closed on teardown")
	}
}

func TestTeardownPerTestRemovesYB(t *testing.T) {
	tr := happyExec()
	cfg := baseConfig()
	cfg.YBShared = false
	p := newTestProvider(cfg, tr)
	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	cmds := tr.recorded()
	// Per-test mode always starts a fresh YB.
	if !hasCmd(cmds, "docker run -d --name 'ampqa-yb-tkn-0'") {
		t.Errorf("per-test mode must start a fresh YB: %v", cmds)
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	cmds = tr.recorded()
	if !hasCmd(cmds, "docker rm -f 'ampqa-yb-tkn-0'") {
		t.Errorf("per-test YB must be removed on teardown")
	}
}

func TestCollectFetchesBothLogs(t *testing.T) {
	tr := happyExec()
	p := newTestProvider(baseConfig(), tr)
	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	sink := &recordSink{}
	if err := env.Collect(context.Background(), sink); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(sink.names) != 2 {
		t.Fatalf("want 2 collected logs, got %v", sink.names)
	}
	if sink.names[0] != "ampqa-ep-tkn-0/docker.log" || sink.names[1] != "ampqa-yb-tkn-shared/docker.log" {
		t.Errorf("unexpected collected names: %v", sink.names)
	}
}

func TestAllocCollisionFree(t *testing.T) {
	p := New(baseConfig())
	const n = 20
	names := map[string]bool{}
	ports := map[int]bool{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	dupes := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := p.alloc()
			mu.Lock()
			defer mu.Unlock()
			if names[a.endpointName] || ports[a.computePort] {
				dupes++
			}
			names[a.endpointName] = true
			ports[a.computePort] = true
		}()
	}
	wg.Wait()
	if dupes != 0 {
		t.Errorf("allocation collided %d times", dupes)
	}
	if len(names) != n || len(ports) != n {
		t.Errorf("want %d distinct names/ports, got %d/%d", n, len(names), len(ports))
	}
}

func TestProvisionRequiresEndpointImage(t *testing.T) {
	cfg := baseConfig()
	cfg.EndpointImage = ""
	p := newTestProvider(cfg, happyExec())
	if _, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true)); err == nil {
		t.Errorf("expected error when endpoint image unset")
	}
}

// TestProvisionRollbackStopsStartedContainers: when a container fails to start
// after an earlier one already started, Provision StopRm's the started one
// (no leaked containers) and closes the transport — the partial-provision
// rollback that previously only Close()d the connection and leaked the YB.
func TestProvisionRollbackStopsStartedContainers(t *testing.T) {
	tr := &scriptExec{}
	tr.on("docker inspect", exec.Result{ExitCode: 0, Stdout: "true\n"}). // YB container reports running for WaitHealthy
										on("SELECT 1", exec.Result{ExitCode: 0, Stdout: "1\n"}).
										on("ampqa-ep-tkn-0", exec.Result{ExitCode: 1, Stderr: "boom"})
	cfg := baseConfig()
	cfg.YBShared = false // per-test YB: this call started it, so rollback must reap it
	p := newTestProvider(cfg, tr)

	if _, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true)); err == nil {
		t.Fatal("provision must fail when the endpoint container fails to start")
	}
	cmds := tr.recorded()
	if !hasCmd(cmds, "docker run -d --name 'ampqa-yb-tkn-0'") {
		t.Fatalf("the YB container should have been started before the endpoint failed: %v", cmds)
	}
	if !hasCmd(cmds, "docker rm -f 'ampqa-yb-tkn-0'") {
		t.Errorf("rollback must StopRm the started YB container, got: %v", cmds)
	}
	if !tr.closed {
		t.Errorf("transport must be closed after a failed provision")
	}
}

// TestProvisionRollbackKeepsSharedYB: a shared YB that was ALREADY running is not
// this call's to reap — a failed provision must NOT StopRm it.
func TestProvisionRollbackKeepsSharedYB(t *testing.T) {
	tr := &scriptExec{}
	tr.on("docker inspect", exec.Result{ExitCode: 0, Stdout: "true\n"}). // shared YB already up
										on("ampqa-ep-tkn-0", exec.Result{ExitCode: 1, Stderr: "boom"})
	p := newTestProvider(baseConfig(), tr) // YBShared true

	if _, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true)); err == nil {
		t.Fatal("provision must fail when the endpoint container fails to start")
	}
	if hasCmd(tr.recorded(), "docker rm -f 'ampqa-yb-tkn-shared'") {
		t.Errorf("a pre-existing shared YB must NOT be reaped by a provision rollback")
	}
}

// TestRestartComputeRestartsEndpointAndWaits: RestartCompute issues an in-place
// `docker restart` of the endpoint container and then blocks on the readiness
// wait against its own PGEndpoint — the compute-restart capability the restart
// regression tests depend on.
func TestRestartComputeRestartsEndpointAndWaits(t *testing.T) {
	tr := happyExec()
	p := newTestProvider(baseConfig(), tr)
	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	e := env.(*Env)

	waited := false
	e.waitReady = func(_ context.Context, got pg.ConnInfo, _ time.Duration) error {
		waited = true
		if got != e.pg {
			t.Errorf("waitReady got %+v, want the endpoint's PGEndpoint %+v", got, e.pg)
		}
		return nil
	}
	if err := e.RestartCompute(context.Background()); err != nil {
		t.Fatalf("RestartCompute: %v", err)
	}
	if !hasCmd(tr.recorded(), "docker restart 'ampqa-ep-tkn-0'") {
		t.Errorf("RestartCompute must restart the endpoint container, got: %v", tr.recorded())
	}
	if !waited {
		t.Error("RestartCompute must wait for the endpoint's Postgres to serve again")
	}
}

// TestRestartComputeFailsFastOnRestartError: a failed container restart is
// returned as an error and the readiness wait is not reached.
func TestRestartComputeFailsFastOnRestartError(t *testing.T) {
	tr := happyExec()
	tr.on("docker restart", exec.Result{ExitCode: 1, Stderr: "no such container"})
	p := newTestProvider(baseConfig(), tr)
	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	e := env.(*Env)
	e.waitReady = func(context.Context, pg.ConnInfo, time.Duration) error {
		return fmt.Errorf("waitReady must not run when the restart failed")
	}
	if err := e.RestartCompute(context.Background()); err == nil {
		t.Fatal("RestartCompute must return an error when the container restart fails")
	}
}

func TestConfigured(t *testing.T) {
	if (Config{}).Configured() {
		t.Errorf("empty config must not be configured")
	}
	if !(Config{Host: "h"}).Configured() {
		t.Errorf("host set must be configured")
	}
}

type recordSink struct {
	names []string
}

func (r *recordSink) Add(name, _ string, _ []byte) error {
	r.names = append(r.names, name)
	return nil
}

func assertOrder(t *testing.T, cmds []string, wantSubstrings ...string) {
	t.Helper()
	idx := 0
	for _, c := range cmds {
		if idx < len(wantSubstrings) && strings.Contains(c, wantSubstrings[idx]) {
			idx++
		}
	}
	if idx != len(wantSubstrings) {
		t.Errorf("commands not in expected order; matched %d/%d\ngot: %v\nwant order: %v",
			idx, len(wantSubstrings), cmds, wantSubstrings)
	}
}

func hasCmd(cmds []string, sub string) bool {
	for _, c := range cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func findCmd(cmds []string, sub string) string {
	for _, c := range cmds {
		if strings.Contains(c, sub) {
			return c
		}
	}
	return ""
}
