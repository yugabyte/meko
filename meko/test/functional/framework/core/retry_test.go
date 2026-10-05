package core_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/fake"
)

// attemptSink keeps EVERY finished result; recordingSink keeps only the latest.
type attemptSink struct {
	mu       sync.Mutex
	finished []*core.Result
}

func (s *attemptSink) RunStarted(core.RunInfo) error { return nil }

func (s *attemptSink) ResultEvent(ev core.ResultEvent) error {
	if ev.Kind == core.EventTestFinished && ev.Result != nil {
		s.mu.Lock()
		s.finished = append(s.finished, ev.Result)
		s.mu.Unlock()
	}
	return nil
}

func (s *attemptSink) RunFinished(core.RunSummary) error { return nil }

func zeroBackoffConfig(t *testing.T, retries string) *core.Config {
	t.Helper()
	cfg, err := core.LoadConfig(nil, nil, []string{"sched.env_retry_backoff=0s", "sched.env_retries=int:" + retries})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// envFailAttempt env-fails with msg for the first `failures` calls, then passes.
func envFailAttempt(calls *int, failures int, msg string) core.RunAttempt {
	return func(ctx context.Context, test core.Test, opts core.RunOptions) *core.Result {
		*calls++
		body := func(*core.T) error { return core.AsEnvFailure(errors.New(msg)) }
		if *calls > failures {
			body = func(*core.T) error { return nil }
		}
		test.Run = body
		return core.RunOne(ctx, test, opts)
	}
}

func TestRetryOnceOnSchedulingFailureThenPasses(t *testing.T) {
	calls := 0
	opts := core.RunOptions{
		Seed: core.NewRunSeed(1, true), Config: zeroBackoffConfig(t, "1"), Provider: &fake.Provider{},
	}
	res := core.RunWithEnvRetry(context.Background(), core.Test{Name: "perf/tpcc"}, opts,
		envFailAttempt(&calls, 1, "0/10 nodes are available: 4 Insufficient cpu"))

	if calls != 2 {
		t.Errorf("attempts = %d, want 2 (one retry)", calls)
	}
	if res.Status != core.StatusPassed {
		t.Errorf("final status = %s, want passed", res.Status)
	}
	if res.Attempt != 2 || res.RetryReason != "env: pod unschedulable (insufficient cpu)" {
		t.Errorf("final attempt = %d reason = %q", res.Attempt, res.RetryReason)
	}
}

// env_retries bounds the chain, a real capacity outage must not retry forever.
func TestRetryStopsAfterTheConfiguredCount(t *testing.T) {
	calls := 0
	opts := core.RunOptions{
		Seed: core.NewRunSeed(1, true), Config: zeroBackoffConfig(t, "1"), Provider: &fake.Provider{},
	}
	res := core.RunWithEnvRetry(context.Background(), core.Test{Name: "perf/tpcc"}, opts,
		envFailAttempt(&calls, 99, "pod unschedulable"))

	if calls != 2 {
		t.Errorf("attempts = %d, want 2 (initial + 1 retry)", calls)
	}
	if res.Status != core.StatusEnvFailed || res.Attempt != 2 {
		t.Errorf("final = %s attempt %d, want env_failed attempt 2", res.Status, res.Attempt)
	}
}

// TestRetryKnobOff: sched.env_retries=0 restores the single-attempt behaviour.
func TestRetryKnobOff(t *testing.T) {
	calls := 0
	opts := core.RunOptions{
		Seed: core.NewRunSeed(1, true), Config: zeroBackoffConfig(t, "0"), Provider: &fake.Provider{},
	}
	res := core.RunWithEnvRetry(context.Background(), core.Test{Name: "perf/tpcc"}, opts,
		envFailAttempt(&calls, 99, "pod unschedulable"))

	if calls != 1 {
		t.Errorf("attempts = %d, want 1 (retries disabled)", calls)
	}
	if res.Attempt != 1 || res.RetryReason != "" {
		t.Errorf("attempt = %d reason = %q, want a clean first attempt", res.Attempt, res.RetryReason)
	}
}

// TestNoRetryOnProductFailure / OnNonSchedulingEnvFailure: the narrow gate.
func TestNoRetryOnProductFailure(t *testing.T) {
	calls := 0
	opts := core.RunOptions{
		Seed: core.NewRunSeed(1, true), Config: zeroBackoffConfig(t, "1"), Provider: &fake.Provider{},
	}
	res := core.RunWithEnvRetry(context.Background(), core.Test{Name: "features/x"}, opts,
		func(ctx context.Context, test core.Test, o core.RunOptions) *core.Result {
			calls++
			test.Run = func(*core.T) error { return errors.New("consistency verify failed on 3/10 tables") }
			return core.RunOne(ctx, test, o)
		})
	if calls != 1 {
		t.Errorf("attempts = %d, want 1, a product failure must never retry", calls)
	}
	if res.Status != core.StatusFailed {
		t.Errorf("status = %s, want failed", res.Status)
	}
}

func TestNoRetryOnNonSchedulingEnvFailure(t *testing.T) {
	calls := 0
	opts := core.RunOptions{
		Seed: core.NewRunSeed(1, true), Config: zeroBackoffConfig(t, "1"), Provider: &fake.Provider{},
	}
	core.RunWithEnvRetry(context.Background(), core.Test{Name: "features/x"}, opts,
		envFailAttempt(&calls, 99, "create universe: status 401: unauthorized"))
	if calls != 1 {
		t.Errorf("attempts = %d, want 1, an auth failure must never retry", calls)
	}
}

// Portal contract: own result per attempt, one retry_group, retry_reason from 2 on.
func TestRetryMetadataGroupsAttempts(t *testing.T) {
	sink := &attemptSink{}
	calls := 0
	opts := core.RunOptions{
		Seed: core.NewRunSeed(1, true), Config: zeroBackoffConfig(t, "1"),
		Provider: &fake.Provider{}, Sink: sink,
	}
	core.RunWithEnvRetry(context.Background(), core.Test{Name: "perf/tpcc"}, opts,
		envFailAttempt(&calls, 1, "0/10 nodes are available: 4 Insufficient cpu"))

	if len(sink.finished) != 2 {
		t.Fatalf("reported %d results, want 2 (every attempt is reported)", len(sink.finished))
	}
	first, second := sink.finished[0], sink.finished[1]
	if first.RetryGroup == "" || first.RetryGroup != second.RetryGroup {
		t.Errorf("retry_group = %q / %q, want one non-empty shared group", first.RetryGroup, second.RetryGroup)
	}
	if first.Attempt != 1 || second.Attempt != 2 {
		t.Errorf("attempts = %d / %d, want 1 / 2", first.Attempt, second.Attempt)
	}
	if first.RetryReason != "" {
		t.Errorf("attempt 1 retry_reason = %q, want empty", first.RetryReason)
	}
	if second.RetryReason != "env: pod unschedulable (insufficient cpu)" {
		t.Errorf("attempt 2 retry_reason = %q", second.RetryReason)
	}
	if first.Status != core.StatusEnvFailed || second.Status != core.StatusPassed {
		t.Errorf("statuses = %s / %s, want env_failed / passed", first.Status, second.Status)
	}
	// No superseded flag: the portal derives "current" from the highest attempt.
	if first.Status == second.Status {
		t.Error("the earlier attempt must keep its own status, not be rewritten")
	}
}

// Results produced outside a retry chain still satisfy the portal contract.
func TestOneShotResultCarriesAttemptOne(t *testing.T) {
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error { return nil }}, &fake.Provider{}, core.DetectedVersions{})
	if r.Attempt != 1 || r.RetryGroup == "" || r.RetryReason != "" {
		t.Errorf("attempt=%d group=%q reason=%q, want attempt 1 with a group and no reason", r.Attempt, r.RetryGroup, r.RetryReason)
	}
}

// A test that env-failed then passed counts ONCE, as passed.
func TestRunCountsUseFinalAttemptOnly(t *testing.T) {
	sink := &attemptSink{}
	var mu sync.Mutex
	seen := 0
	test := core.Test{Name: "perf/tpcc", Run: func(*core.T) error {
		mu.Lock()
		defer mu.Unlock()
		seen++
		if seen == 1 {
			return core.EnvFail("0/10 nodes are available: 4 Insufficient cpu")
		}
		return nil
	}}
	summary, results := core.RunTests(context.Background(), []core.Test{test}, core.RunOptions{
		Parallel: 1, Seed: core.NewRunSeed(1, true), Config: zeroBackoffConfig(t, "1"),
		Provider: &fake.Provider{}, Sink: sink,
	})

	if len(sink.finished) != 2 {
		t.Fatalf("reported %d results, want 2 attempts reported", len(sink.finished))
	}
	if len(results) != 1 || results[0].Status != core.StatusPassed {
		t.Fatalf("returned results = %+v, want the single final passed attempt", results)
	}
	if summary.Total != 1 {
		t.Errorf("summary.Total = %d, want 1", summary.Total)
	}
	if summary.Counts[core.StatusPassed] != 1 || summary.Counts[core.StatusEnvFailed] != 0 {
		t.Errorf("counts = %v, want exactly one passed and no env_failed", summary.Counts)
	}
}

// TestFinalAttemptsKeepsOnePerChain: the filter every totals-producing sink uses.
func TestFinalAttemptsKeepsOnePerChain(t *testing.T) {
	mk := func(name, group string, n int, st core.Status) *core.Result {
		return &core.Result{TestName: name, RetryGroup: group, Attempt: n, Status: st}
	}
	in := []*core.Result{
		mk("a", "g1", 1, core.StatusEnvFailed),
		mk("b", "g2", 1, core.StatusPassed),
		mk("a", "g1", 2, core.StatusPassed),
		mk("c", "", 1, core.StatusFailed), // ungrouped (legacy record) passes through
	}
	out := core.FinalAttempts(in)
	if len(out) != 3 {
		t.Fatalf("kept %d results, want 3", len(out))
	}
	// Position preserved (first appearance), status from the final attempt.
	if out[0].TestName != "a" || out[0].Attempt != 2 || out[0].Status != core.StatusPassed {
		t.Errorf("out[0] = %+v, want a attempt 2 passed in its original slot", out[0])
	}
	if out[1].TestName != "b" || out[2].TestName != "c" {
		t.Errorf("order changed: %s, %s", out[1].TestName, out[2].TestName)
	}
	// An out-of-order earlier attempt must not clobber the later one.
	if got := core.FinalAttempts([]*core.Result{mk("a", "g1", 2, core.StatusPassed), mk("a", "g1", 1, core.StatusEnvFailed)}); got[0].Attempt != 2 {
		t.Errorf("kept attempt %d, want the highest", got[0].Attempt)
	}
}
