package sched_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/fake"
	"github.com/yugabyte/meko/meko/test/functional/framework/sched"
)

func sharedTest(name string, run func(*core.T) error) core.Test {
	return core.Test{
		Name:     name,
		Suite:    "s",
		Category: core.Functional,
		Env:      core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Run:      run,
	}
}

func exclusiveTest(name string, run func(*core.T) error) core.Test {
	return core.Test{
		Name:     name,
		Suite:    "s",
		Category: core.Functional,
		Env:      core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleStack},
		Run:      run,
	}
}

// TestSchedulerWorkedExample is the architecture §4.4 shape: 16 SHARED tests
// (per_env_parallelism 8 → ≤2 shared envs) + 6 EXCLUSIVE tests, capacity 6. It
// asserts capacity is never exceeded, all complete passed, and the shared-env
// bound holds.
func TestSchedulerWorkedExample(t *testing.T) {
	p := &fake.Provider{}
	var tests []core.Test
	for i := 0; i < 16; i++ {
		tests = append(tests, sharedTest(fmt.Sprintf("f%d", i), func(*core.T) error { return nil }))
	}
	for i := 0; i < 6; i++ {
		tests = append(tests, exclusiveTest(fmt.Sprintf("s%d", i), func(*core.T) error { return nil }))
	}

	summary, results := sched.Run(context.Background(), tests, sched.Options{
		RunOptions:        core.RunOptions{Parallel: 32, Seed: core.NewRunSeed(1, true), Provider: p},
		Capacity:          6,
		PerEnvParallelism: 8,
	})

	if summary.Counts[core.StatusPassed] != 22 {
		t.Fatalf("passed = %d, want 22 (%+v)", summary.Counts[core.StatusPassed], summary.Counts)
	}
	for i, r := range results {
		if r == nil || r.Status != core.StatusPassed {
			t.Fatalf("result %d not passed: %+v", i, r)
		}
	}
	if p.PeakLive() > 6 {
		t.Fatalf("peak live envs = %d, exceeds capacity 6", p.PeakLive())
	}
	if p.SharedProvisionCount() > 2 {
		t.Fatalf("shared envs = %d, want <=2", p.SharedProvisionCount())
	}
	// 6 exclusive envs + at most 2 shared envs.
	if p.ProvisionCount() != p.TeardownCount() {
		t.Fatalf("provisions %d != teardowns %d (leak)", p.ProvisionCount(), p.TeardownCount())
	}
}

// TestSchedulerSharedTeardownOncePerEnvAfterLastCoTenant asserts each shared env
// is torn down exactly once, and only after its last co-tenant finished, using
// a synchronization point (a live-co-tenant counter keyed by env id), not sleeps.
func TestSchedulerSharedTeardownOncePerEnv(t *testing.T) {
	var mu sync.Mutex
	active := map[string]int{}
	teardowns := map[string]int{}
	var violation string

	p := &fake.Provider{}
	p.OnTeardown = func(e *fake.Env) {
		mu.Lock()
		defer mu.Unlock()
		if active[e.ID()] != 0 {
			violation = fmt.Sprintf("teardown of %s with %d co-tenants still active", e.ID(), active[e.ID()])
		}
		teardowns[e.ID()]++
	}

	body := func(t *core.T) error {
		id := t.Env.Handles().Namespace
		mu.Lock()
		active[id]++
		mu.Unlock()
		defer func() {
			mu.Lock()
			active[id]--
			mu.Unlock()
		}()
		return nil
	}

	var tests []core.Test
	for i := 0; i < 10; i++ {
		tests = append(tests, sharedTest(fmt.Sprintf("t%d", i), body))
	}
	summary, _ := sched.Run(context.Background(), tests, sched.Options{
		RunOptions:        core.RunOptions{Parallel: 16, Seed: core.NewRunSeed(2, true), Provider: p},
		Capacity:          4,
		PerEnvParallelism: 4,
	})

	if summary.Counts[core.StatusPassed] != 10 {
		t.Fatalf("passed = %d, want 10", summary.Counts[core.StatusPassed])
	}
	if violation != "" {
		t.Fatalf("teardown ordering violation: %s", violation)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(teardowns) == 0 {
		t.Fatalf("no shared envs torn down")
	}
	for id, n := range teardowns {
		if n != 1 {
			t.Fatalf("env %s torn down %d times, want exactly 1", id, n)
		}
	}
}

// TestSchedulerExclusiveSharedOverlap proves exclusive and shared tests run
// concurrently: one of each rendezvous, each blocking until the other has
// started. If the scheduler serialized them, the rendezvous would never
// complete and the test would fail on the failsafe timeout.
func TestSchedulerExclusiveSharedOverlap(t *testing.T) {
	exclusiveStarted := make(chan struct{})
	sharedStarted := make(chan struct{})
	fail := make(chan string, 2)

	tests := []core.Test{
		exclusiveTest("x", func(t *core.T) error {
			close(exclusiveStarted)
			select {
			case <-sharedStarted:
			case <-time.After(3 * time.Second):
				fail <- "exclusive never observed shared running"
			}
			return nil
		}),
		sharedTest("h", func(t *core.T) error {
			close(sharedStarted)
			select {
			case <-exclusiveStarted:
			case <-time.After(3 * time.Second):
				fail <- "shared never observed exclusive running"
			}
			return nil
		}),
	}

	summary, _ := sched.Run(context.Background(), tests, sched.Options{
		RunOptions:        core.RunOptions{Parallel: 4, Seed: core.NewRunSeed(3, true), Provider: &fake.Provider{}},
		Capacity:          4,
		PerEnvParallelism: 4,
	})
	select {
	case msg := <-fail:
		t.Fatalf("no overlap: %s", msg)
	default:
	}
	if summary.Counts[core.StatusPassed] != 2 {
		t.Fatalf("passed = %d, want 2", summary.Counts[core.StatusPassed])
	}
}

func TestSchedulerExclusiveEnvFailByLabel(t *testing.T) {
	p := &fake.Provider{FailLabelSubstr: "env_fail"}
	tests := []core.Test{
		exclusiveTest("canary/provision_env_fail", func(*core.T) error { return nil }),
		exclusiveTest("ok", func(*core.T) error { return nil }),
	}
	_, results := sched.Run(context.Background(), tests, sched.Options{
		RunOptions:        core.RunOptions{Parallel: 4, Seed: core.NewRunSeed(4, true), Provider: p},
		Capacity:          4,
		PerEnvParallelism: 4,
	})
	if results[0].Status != core.StatusEnvFailed {
		t.Fatalf("env-fail test status = %s, want env_failed", results[0].Status)
	}
	if results[1].Status != core.StatusPassed {
		t.Fatalf("ok test status = %s, want passed", results[1].Status)
	}
}

func TestSchedulerFailedSharedStackBlocksOnlyItsTests(t *testing.T) {
	const reason = "environment prepare failed: migrations wedged"
	ran := map[string]bool{}
	var mu sync.Mutex
	body := func(name string) func(*core.T) error {
		return func(*core.T) error {
			mu.Lock()
			ran[name] = true
			mu.Unlock()
			return nil
		}
	}
	offStack := func(name string, sharing core.Sharing, shape core.Shape) core.Test {
		return core.Test{Name: name, Suite: "s", Category: core.Functional,
			Env: core.EnvSpec{Sharing: sharing, Shape: shape}, Run: body(name)}
	}
	tests := []core.Test{
		sharedTest("on_stack", body("on_stack")),
		exclusiveTest("on_stack_exclusive", body("on_stack_exclusive")),
		offStack("cluster_only", core.Exclusive, core.ClusterOnly),
		offStack("remote", core.Shared, core.Remote),
	}
	_, results := sched.Run(context.Background(), tests, sched.Options{
		RunOptions: core.RunOptions{
			Parallel: 4, Seed: core.NewRunSeed(9, true), Provider: &fake.Provider{},
			SharedStackFailed: reason,
		},
		Capacity:          4,
		PerEnvParallelism: 4,
	})
	for _, r := range results[:2] {
		if r.Status != core.StatusEnvFailed || r.ErrorSummary != reason {
			t.Errorf("%s = %s %q, want env_failed %q", r.TestName, r.Status, r.ErrorSummary, reason)
		}
	}
	for _, r := range results[2:] {
		if r.Status != core.StatusPassed {
			t.Errorf("%s = %s, want passed", r.TestName, r.Status)
		}
	}
	if ran["on_stack"] || ran["on_stack_exclusive"] {
		t.Errorf("a test on the failed stack ran its body: %v", ran)
	}
}

func TestSchedulerNoProviderSkips(t *testing.T) {
	tests := []core.Test{
		sharedTest("a", func(*core.T) error { return nil }),
		exclusiveTest("b", func(*core.T) error { return nil }),
	}
	_, results := sched.Run(context.Background(), tests, sched.Options{
		RunOptions: core.RunOptions{
			Parallel:                  2,
			Seed:                      core.NewRunSeed(5, true),
			ProviderUnavailableReason: "env not configured",
		},
		Capacity:          4,
		PerEnvParallelism: 4,
	})
	for _, r := range results {
		if r.Status != core.StatusSkipped {
			t.Fatalf("%s status = %s, want skipped", r.TestName, r.Status)
		}
	}
}

// TestSchedulerCancelledContextStillTearsDown is the graceful-shutdown contract
// (AGENTS.md §5): once a signal cancels the root context, simulated here by
// cancelling the instant the environment is provisioned, the scheduler stops
// taking new work and the in-flight test STILL runs its teardown, so provisioned
// infrastructure is never orphaned by a non-clean exit. This is the behavior
// `cmd/mekoqa` enables by threading its signal-cancelled context into sched.Run.
func TestSchedulerCancelledContextStillTearsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &fake.Provider{}
	p.OnProvision = func(*fake.Env) { cancel() }
	test := exclusiveTest("cancel", func(tt *core.T) error {
		<-tt.Context.Done() // a well-behaved body observes the cancellation and returns
		return tt.Context.Err()
	})

	_, results := sched.Run(ctx, []core.Test{test}, sched.Options{
		RunOptions:        core.RunOptions{Parallel: 1, Seed: core.NewRunSeed(1, true), Provider: p},
		Capacity:          1,
		PerEnvParallelism: 1,
	})

	if p.ProvisionCount() != 1 {
		t.Fatalf("provisioned %d, want 1", p.ProvisionCount())
	}
	if p.TeardownCount() != 1 {
		t.Fatalf("teardown ran %d times, want 1, a cancelled run must still tear down", p.TeardownCount())
	}
	if results[0] == nil {
		t.Fatalf("cancelled test produced no result")
	}
}

// The packing scheduler retries like the worker pool, counting the test ONCE.
func TestSchedulerRetriesSchedulingEnvFailureOnce(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{"sched.env_retry_backoff=0s"})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var mu sync.Mutex
	attempts := 0
	body := func(*core.T) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			return core.EnvFail("0/10 nodes are available: 4 Insufficient cpu")
		}
		return nil
	}
	summary, results := sched.Run(context.Background(), []core.Test{exclusiveTest("perf/tpcc", body)}, sched.Options{
		RunOptions:        core.RunOptions{Parallel: 1, Seed: core.NewRunSeed(9, true), Config: cfg, Provider: &fake.Provider{}},
		Capacity:          2,
		PerEnvParallelism: 2,
	})
	if attempts != 2 {
		t.Fatalf("body ran %d times, want 2 (one retry)", attempts)
	}
	if len(results) != 1 || results[0].Status != core.StatusPassed || results[0].Attempt != 2 {
		t.Fatalf("results = %+v, want a single passed attempt 2", results)
	}
	if summary.Total != 1 || summary.Counts[core.StatusPassed] != 1 || summary.Counts[core.StatusEnvFailed] != 0 {
		t.Errorf("counts = %v (total %d), want exactly one passed", summary.Counts, summary.Total)
	}
}

// sharedInfraProvider arms the FaultsSharedInfra gate.
type sharedInfraProvider struct{ fake.Provider }

func (p *sharedInfraProvider) SharesInfraAcrossTests() bool { return true }

func faultTest(name string, run func(*core.T) error) core.Test {
	tc := exclusiveTest(name, run)
	tc.Env.FaultsSharedInfra = true
	return tc
}

// A fault test must never run beside another: it breaks what they all use.
func TestSchedulerRunsSharedInfraFaultTestsAlone(t *testing.T) {
	var mu sync.Mutex
	live, peakBesideFault := 0, 0
	faulting := 0

	body := func(fault bool) func(*core.T) error {
		return func(*core.T) error {
			mu.Lock()
			live++
			if fault {
				faulting++
			}
			// Any peer seen while a fault test is live is a violation.
			if faulting > 0 && live > faulting {
				peakBesideFault = live
			}
			if faulting > 1 {
				peakBesideFault = live
			}
			mu.Unlock()

			time.Sleep(20 * time.Millisecond)

			mu.Lock()
			live--
			if fault {
				faulting--
			}
			mu.Unlock()
			return nil
		}
	}

	var tests []core.Test
	for i := 0; i < 8; i++ {
		tests = append(tests, sharedTest(fmt.Sprintf("f%d", i), body(false)))
	}
	for i := 0; i < 3; i++ {
		tests = append(tests, faultTest(fmt.Sprintf("chaos%d", i), body(true)))
	}
	for i := 0; i < 4; i++ {
		tests = append(tests, exclusiveTest(fmt.Sprintf("x%d", i), body(false)))
	}

	summary, results := sched.Run(context.Background(), tests, sched.Options{
		RunOptions:        core.RunOptions{Parallel: 8, Seed: core.NewRunSeed(1, true), Provider: &sharedInfraProvider{}},
		Capacity:          8,
		PerEnvParallelism: 4,
	})

	if summary.Counts[core.StatusPassed] != len(tests) {
		t.Fatalf("passed = %d, want %d (%+v)", summary.Counts[core.StatusPassed], len(tests), summary.Counts)
	}
	for i, r := range results {
		if r == nil || r.Status != core.StatusPassed {
			t.Fatalf("result %d not passed: %+v", i, r)
		}
	}
	if peakBesideFault != 0 {
		t.Errorf("%d tests ran alongside a shared-infra fault test, want it to run alone", peakBesideFault)
	}
}

// No shared infrastructure: no run pays for isolation it does not need.
func TestSchedulerFaultFlagIsInertOnUnsharedProvider(t *testing.T) {
	var mu sync.Mutex
	live, peak := 0, 0
	body := func(*core.T) error {
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		live--
		mu.Unlock()
		return nil
	}

	var tests []core.Test
	for i := 0; i < 4; i++ {
		tests = append(tests, faultTest(fmt.Sprintf("chaos%d", i), body))
	}

	summary, _ := sched.Run(context.Background(), tests, sched.Options{
		RunOptions:        core.RunOptions{Parallel: 4, Seed: core.NewRunSeed(1, true), Provider: &fake.Provider{}},
		Capacity:          4,
		PerEnvParallelism: 4,
	})
	if summary.Counts[core.StatusPassed] != len(tests) {
		t.Fatalf("passed = %d, want %d", summary.Counts[core.StatusPassed], len(tests))
	}
	if peak < 2 {
		t.Errorf("peak concurrency = %d, want >1: the gate must not serialise a provider that shares nothing", peak)
	}
}

// unhealthyInfraProvider reports the shared stack as dead after the fault.
type unhealthyInfraProvider struct {
	sharedInfraProvider
	err error
}

func (p *unhealthyInfraProvider) SharedInfraHealthy(context.Context) error { return p.err }

// The regression that matters: a fault the stack recovered from must leave the
// rest of the run alone.
func TestSchedulerHealthyProbeDoesNotPoisonRun(t *testing.T) {
	tests := []core.Test{
		faultTest("chaos", func(*core.T) error { return fmt.Errorf("boom") }),
		exclusiveTest("after", func(*core.T) error { return nil }),
	}
	_, results := sched.Run(context.Background(), tests, sched.Options{
		Capacity: 4, PerEnvParallelism: 1,
		RunOptions: core.RunOptions{Parallel: 1, Seed: core.NewRunSeed(1, true), Provider: &unhealthyInfraProvider{}},
	})
	if results[1].Status != core.StatusPassed {
		t.Fatalf("a recovered stack must leave later tests alone, got %s (%s)", results[1].Status, results[1].ErrorSummary)
	}
}

func lockedTest(name, key string, sharing core.Sharing, run func(*core.T) error) core.Test {
	return core.Test{
		Name:     name,
		Suite:    "s",
		Category: core.Functional,
		Env:      core.EnvSpec{Sharing: sharing, Shape: core.SingleStack, SharedLock: key},
		Run:      run,
	}
}

// The regression: three upgrade tests drive one controller-global fleet upgrade,
// and the controller 409s the second (result 7883b585).
func TestSchedulerSerializesTestsSharingALock(t *testing.T) {
	var mu sync.Mutex
	var live, peak int
	body := func(*core.T) error {
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		live--
		mu.Unlock()
		return nil
	}
	tests := []core.Test{
		lockedTest("x1", "fleet", core.Exclusive, body),
		lockedTest("x2", "fleet", core.Exclusive, body),
		lockedTest("s1", "fleet", core.Shared, body),
		lockedTest("s2", "fleet", core.Shared, body),
	}
	summary, _ := sched.Run(context.Background(), tests, sched.Options{
		RunOptions: core.RunOptions{Parallel: 8, Seed: core.NewRunSeed(9, true), Provider: &fake.Provider{}},
		Capacity:   8,
	})
	if peak != 1 {
		t.Errorf("peak concurrency under one lock key = %d, want 1", peak)
	}
	if summary.Counts[core.StatusPassed] != len(tests) {
		t.Errorf("passed = %d, want %d", summary.Counts[core.StatusPassed], len(tests))
	}
}

// An empty key must not serialize anything.
func TestSchedulerLeavesUnlockedTestsConcurrent(t *testing.T) {
	start := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var live, peak int
	body := func(*core.T) error {
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		n := live
		mu.Unlock()
		if n >= 2 {
			once.Do(func() { close(start) })
		}
		select {
		case <-start:
		case <-time.After(5 * time.Second):
		}
		mu.Lock()
		live--
		mu.Unlock()
		return nil
	}
	tests := []core.Test{
		exclusiveTest("e1", body),
		exclusiveTest("e2", body),
	}
	sched.Run(context.Background(), tests, sched.Options{
		RunOptions: core.RunOptions{Parallel: 4, Seed: core.NewRunSeed(10, true), Provider: &fake.Provider{}},
		Capacity:   4,
	})
	if peak < 2 {
		t.Errorf("peak concurrency without a lock key = %d, want 2", peak)
	}
}

type rebuildingProvider struct {
	sharedInfraProvider
	mu      sync.Mutex
	healthy bool
}

type genEnv struct {
	core.Environment
	rebuilt bool
}

func (p *rebuildingProvider) Provision(ctx context.Context, spec core.EnvSpec, seed *core.Seed) (core.Environment, error) {
	env, err := p.sharedInfraProvider.Provision(ctx, spec, seed)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return &genEnv{Environment: env, rebuilt: p.healthy}, nil
}

func (p *rebuildingProvider) Teardown(ctx context.Context, env core.Environment) error {
	return p.sharedInfraProvider.Teardown(ctx, env.(*genEnv).Environment)
}

func (p *rebuildingProvider) SharedInfraHealthy(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.healthy {
		return nil
	}
	return fmt.Errorf("api not serving")
}

func (p *rebuildingProvider) RebuildSharedInfra(context.Context) error {
	p.mu.Lock()
	p.healthy = true
	p.mu.Unlock()
	return nil
}

// A shared test that breaks the shared stack costs only itself: the stack is
// rebuilt and the tests after it run on a newly provisioned environment.
func TestSchedulerRebuildsASharedStackAFaultLeftDown(t *testing.T) {
	var chaosDone atomic.Bool
	chaos := sharedTest("chaos", func(*core.T) error {
		chaosDone.Store(true)
		return fmt.Errorf("boom")
	})
	chaos.Env.FaultsSharedInfra = true
	after := sharedTest("after", func(tt *core.T) error {
		if chaosDone.Load() && !tt.Env.(*genEnv).rebuilt {
			return fmt.Errorf("ran on the broken stack's environment")
		}
		return nil
	})
	for _, perEnv := range []int{1, 4} {
		chaosDone.Store(false)
		_, results := sched.Run(context.Background(), []core.Test{chaos, after}, sched.Options{
			Capacity: 4, PerEnvParallelism: perEnv,
			RunOptions: core.RunOptions{Parallel: 1, Seed: core.NewRunSeed(1, true), Provider: &rebuildingProvider{}},
		})
		if results[1].Status != core.StatusPassed {
			t.Fatalf("perEnv %d: the test after the fault = %s (%s), want passed", perEnv, results[1].Status, results[1].ErrorSummary)
		}
	}
}
