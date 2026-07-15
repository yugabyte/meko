package sched_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/core"
	"github.com/yugabyte/yb-amp/test/system/framework/env/fake"
	"github.com/yugabyte/yb-amp/test/system/framework/sched"
)

func sharedTest(name string, run func(*core.T) error) core.Test {
	return core.Test{
		Name:     name,
		Suite:    "s",
		Category: core.Functional,
		Env:      core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Run:      run,
	}
}

func exclusiveTest(name string, run func(*core.T) error) core.Test {
	return core.Test{
		Name:     name,
		Suite:    "s",
		Category: core.Functional,
		Env:      core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleContainer},
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
// is torn down exactly once, and only after its last co-tenant finished — using
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
// (AGENTS.md §5): once a signal cancels the root context — simulated here by
// cancelling the instant the environment is provisioned — the scheduler stops
// taking new work and the in-flight test STILL runs its teardown, so provisioned
// infrastructure is never orphaned by a non-clean exit. This is the behavior
// `cmd/ampqa` enables by threading its signal-cancelled context into sched.Run.
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
		t.Fatalf("teardown ran %d times, want 1 — a cancelled run must still tear down", p.TeardownCount())
	}
	if results[0] == nil {
		t.Fatalf("cancelled test produced no result")
	}
}
