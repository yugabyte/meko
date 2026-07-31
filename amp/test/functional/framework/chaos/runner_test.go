package chaos_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/chaos"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// spyAction is a controllable ChaosAction for loop tests: it counts injects and
// recovers, can fail or block on inject, and records which targets it touched.
type spyAction struct {
	injects   atomic.Int32
	recovers  atomic.Int32
	injErr    error
	block     bool // block inject until ctx is cancelled
	mu        sync.Mutex
	seenNames map[string]int
}

func newSpy() *spyAction { return &spyAction{seenNames: map[string]int{}} }

func (s *spyAction) Name() string { return "spy" }

func (s *spyAction) Inject(ctx context.Context, t *chaos.Target) error {
	s.injects.Add(1)
	s.mu.Lock()
	s.seenNames[t.Name()]++
	s.mu.Unlock()
	if s.block {
		<-ctx.Done()
	}
	return s.injErr
}

func (s *spyAction) Recover(context.Context, *chaos.Target) error {
	s.recovers.Add(1)
	return nil
}

// spyRegistry returns a registry whose "spy" action is the given instance
// (shared, so counts accumulate across the whole run).
func spyRegistry(s *spyAction) *chaos.Registry {
	r := chaos.NewRegistry()
	r.Register("spy", func(chaos.Config) chaos.ChaosAction { return s })
	return r
}

func ownedPod(name string) *chaos.Target {
	rec := &kubeRecorder{}
	kube := k8s.New(k8s.Config{Namespace: "ampqa-run1"}).WithRunner(rec.run)
	return chaos.NewPodTarget(name, kube, name, "yb-tserver")
}

func fastCfg() chaos.Config {
	c := chaos.DefaultConfig()
	c.Actions = []string{"spy"}
	c.HoldMin, c.HoldMax = time.Millisecond, time.Millisecond
	c.GapMin, c.GapMax = time.Millisecond, time.Millisecond
	c.RecoverTimeout = time.Second
	return c
}

func TestPlanReproducibleAndSeedSensitive(t *testing.T) {
	targets := []*chaos.Target{ownedPod("a"), ownedPod("b"), ownedPod("c")}

	mk := func(seedVal int64) *chaos.Runner {
		r, err := chaos.NewRunner(core.NewRunSeed(seedVal, true), chaos.DefaultRegistry(), chaos.DefaultConfig(), targets)
		if err != nil {
			t.Fatalf("new runner: %v", err)
		}
		return r
	}

	p1 := mk(42).Plan(30)
	p2 := mk(42).Plan(30)
	if !reflect.DeepEqual(p1, p2) {
		t.Fatalf("same seed produced different plans:\n%v\n%v", p1, p2)
	}
	p3 := mk(99).Plan(30)
	if reflect.DeepEqual(p1, p3) {
		t.Fatal("different seeds produced identical plans")
	}
}

func TestRunnerRecoversEveryFaultOnStop(t *testing.T) {
	spy := newSpy()
	r, err := chaos.NewRunner(core.NewRunSeed(7, true), spyRegistry(spy), fastCfg(), []*chaos.Target{ownedPod("a")})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	r.Stop()

	inj, rec := spy.injects.Load(), spy.recovers.Load()
	if inj == 0 {
		t.Fatal("expected at least one injection")
	}
	if inj != rec {
		t.Errorf("injects=%d recovers=%d — every fault must be recovered", inj, rec)
	}
}

func TestRecoverRunsEvenWhenInjectFails(t *testing.T) {
	spy := newSpy()
	spy.injErr = errors.New("inject boom")
	r, err := chaos.NewRunner(core.NewRunSeed(7, true), spyRegistry(spy), fastCfg(), []*chaos.Target{ownedPod("a")})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	r.Stop()

	if spy.injects.Load() == 0 || spy.recovers.Load() != spy.injects.Load() {
		t.Errorf("failed injects must still be recovered: injects=%d recovers=%d",
			spy.injects.Load(), spy.recovers.Load())
	}
}

func TestRecoverRunsWhenInjectCancelledByStop(t *testing.T) {
	spy := newSpy()
	spy.block = true // inject blocks until the run context is cancelled
	cfg := fastCfg()
	cfg.HoldMin, cfg.HoldMax = time.Hour, time.Hour // never reached; inject blocks first
	r, err := chaos.NewRunner(core.NewRunSeed(7, true), spyRegistry(spy), cfg, []*chaos.Target{ownedPod("a")})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // let the loop enter the blocking inject
	r.Stop()                          // cancels inject; recovery must still run

	if spy.injects.Load() == 0 {
		t.Fatal("expected the loop to enter inject")
	}
	if spy.recovers.Load() != spy.injects.Load() {
		t.Errorf("cancelled inject must still be recovered: injects=%d recovers=%d",
			spy.injects.Load(), spy.recovers.Load())
	}
}

func TestOwnershipGuardRefusesForeignNamespace(t *testing.T) {
	spy := newSpy()
	rec := &kubeRecorder{}
	kube := k8s.New(k8s.Config{Namespace: "someone-elses-ns"}).WithRunner(rec.run)
	foreign := chaos.NewPodTarget("victim", kube, "victim", "yb-tserver")

	r, err := chaos.NewRunner(core.NewRunSeed(7, true), spyRegistry(spy), fastCfg(), []*chaos.Target{foreign})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	if err := r.Start(); err == nil {
		t.Fatal("expected Start to refuse a foreign-namespace target")
	}
	if spy.injects.Load() != 0 {
		t.Errorf("nothing must be injected against a refused target, got %d", spy.injects.Load())
	}
}

func TestLoopOnlyTouchesGivenTargets(t *testing.T) {
	spy := newSpy()
	r, err := chaos.NewRunner(core.NewRunSeed(7, true), spyRegistry(spy), fastCfg(), []*chaos.Target{ownedPod("only-me")})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	r.Stop()

	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.seenNames) != 1 || spy.seenNames["only-me"] == 0 {
		t.Errorf("loop touched targets other than the one provided: %v", spy.seenNames)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	spy := newSpy()
	r, err := chaos.NewRunner(core.NewRunSeed(7, true), spyRegistry(spy), fastCfg(), []*chaos.Target{ownedPod("a")})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	r.Stop()
	r.Stop() // second Stop must not panic or block
}

func TestNewRunnerValidatesInputs(t *testing.T) {
	seed := core.NewRunSeed(1, true)
	if _, err := chaos.NewRunner(nil, nil, chaos.DefaultConfig(), []*chaos.Target{ownedPod("a")}); err == nil {
		t.Error("expected error for nil seed")
	}
	if _, err := chaos.NewRunner(seed, nil, chaos.DefaultConfig(), nil); err == nil {
		t.Error("expected error for no targets")
	}
	cfg := chaos.DefaultConfig()
	cfg.Actions = []string{"does-not-exist"}
	if _, err := chaos.NewRunner(seed, chaos.NewRegistry(), cfg, []*chaos.Target{ownedPod("a")}); err == nil {
		t.Error("expected error for unknown action")
	}
}

func TestDefaultRegistryHasAllActions(t *testing.T) {
	names := chaos.DefaultRegistry().Names()
	want := []string{
		chaos.ActionNetworkPartition, chaos.ActionNetworkSlowdown, chaos.ActionNodeRestart,
		chaos.ActionNodeStop, chaos.ActionPacketLoss, chaos.ActionPodDelete,
		chaos.ActionProcessKill, chaos.ActionProcessRestart,
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("DefaultRegistry missing action %q (have %v)", w, names)
		}
	}
	// Names must be sorted for reproducible "all actions" selection.
	if !sortedStrings(names) {
		t.Errorf("Names() not sorted: %v", names)
	}
}

func sortedStrings(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}
