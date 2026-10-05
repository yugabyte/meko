package core_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

type guardedEnv struct {
	collectingEnv
	findings []core.GuardFinding
}

func (guardedEnv) GuardMark(context.Context) (any, error) { return "mark", nil }
func (g guardedEnv) GuardCheck(_ context.Context, mark any) ([]core.GuardFinding, error) {
	if mark != "mark" {
		return nil, nil
	}
	return g.findings, nil
}

type guardedProvider struct{ env guardedEnv }

func (p guardedProvider) Provision(context.Context, core.EnvSpec, *core.Seed) (core.Environment, error) {
	return p.env, nil
}
func (p guardedProvider) Attach(context.Context, string) (core.Environment, error) {
	return p.env, nil
}
func (guardedProvider) Teardown(context.Context, core.Environment) error { return nil }

func runGuarded(t *testing.T, test core.Test, findings ...core.GuardFinding) *core.Result {
	t.Helper()
	cfg, err := core.LoadConfigNoDefaults(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, results := core.RunTests(context.Background(), []core.Test{test},
		core.RunOptions{Parallel: 1, Seed: core.NewRunSeed(1, true), Config: cfg,
			Provider: guardedProvider{guardedEnv{findings: findings}}})
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	return results[0]
}

var restart = core.GuardFinding{Kind: core.GuardPodRestart, Workload: "meko-api-server-a/meko-api-server", Detail: "restarted 1 time(s), last exit OOMKilled", Fatal: true}

// A green body on a stack whose api-server got OOM-killed meanwhile is not a
// pass: the guard is what turns that into a failure.
func TestGuardFailsAPassingBodyOnAFatalFinding(t *testing.T) {
	r := runGuarded(t, core.Test{Name: "t", Run: func(*core.T) error { return nil }}, restart)
	if r.Status != core.StatusFailed || !strings.Contains(r.ErrorSummary, "OOMKilled") {
		t.Errorf("status %s summary %q, want failed naming the restart", r.Status, r.ErrorSummary)
	}
}

func TestGuardRecordsToleratedAndNonFatalFindings(t *testing.T) {
	errs := core.GuardFinding{Kind: core.GuardErrorLogs, Workload: "app=x", Detail: "3 ERROR line(s)"}
	r := runGuarded(t, core.Test{Name: "t", Tolerates: []string{core.GuardPodRestart}, Run: func(*core.T) error { return nil }}, restart, errs)
	if r.Status != core.StatusPassed {
		t.Fatalf("status %s (%s), want passed", r.Status, r.ErrorSummary)
	}
	var found string
	for _, s := range r.Steps {
		if s.Name == "service_guard" {
			found, _ = s.Properties["findings"].(string)
		}
	}
	if !strings.Contains(found, "OOMKilled") || !strings.Contains(found, "3 ERROR line(s)") {
		t.Errorf("service_guard findings = %q, want both recorded", found)
	}
}

func TestGuardOff(t *testing.T) {
	cfg, err := core.LoadConfigNoDefaults(nil, nil, []string{"guard.enabled=false"})
	if err != nil {
		t.Fatal(err)
	}
	_, results := core.RunTests(context.Background(), []core.Test{{Name: "t", Run: func(*core.T) error { return nil }}},
		core.RunOptions{Parallel: 1, Seed: core.NewRunSeed(1, true), Config: cfg,
			Provider: guardedProvider{guardedEnv{findings: []core.GuardFinding{restart}}}})
	if results[0].Status != core.StatusPassed {
		t.Errorf("guard.enabled=false still failed the test: %s", results[0].ErrorSummary)
	}
}

func runPair(t *testing.T, a, b core.Test) []*core.Result {
	t.Helper()
	cfg, err := core.LoadConfigNoDefaults(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, results := core.RunTests(context.Background(), []core.Test{a, b},
		core.RunOptions{Parallel: 2, Seed: core.NewRunSeed(1, true), Config: cfg,
			Provider: guardedProvider{guardedEnv{findings: []core.GuardFinding{restart}}}})
	return results
}

func overlapping() func(*core.T) error {
	var wg sync.WaitGroup
	wg.Add(2)
	return func(*core.T) error {
		wg.Done()
		wg.Wait()
		return nil
	}
}

// A co-tenant that tolerates restarts may have caused this one, so the other
// body records it as concurrent instead of failing.
func TestGuardExcusesWhatAnOverlappingBodyTolerates(t *testing.T) {
	body := overlapping()
	results := runPair(t, core.Test{Name: "a", Run: body}, core.Test{Name: "b", Tolerates: []string{core.GuardPodRestart}, Run: body})
	for _, r := range results {
		if r.Status != core.StatusPassed {
			t.Errorf("%s = %s (%s), want passed", r.TestName, r.Status, r.ErrorSummary)
		}
	}
	for _, s := range results[0].Steps {
		if s.Name == "service_guard" && !strings.Contains(fmt.Sprint(s.Properties["concurrent"]), "OOMKilled") {
			t.Errorf("a: concurrent = %v, want the restart recorded", s.Properties["concurrent"])
		}
	}
}

// Overlap alone excuses nothing: with four tests per stack nearly every body
// overlaps another, and a crash would then fail no test.
func TestGuardFailsOverlappingBodiesThatToleratedNothing(t *testing.T) {
	body := overlapping()
	for _, r := range runPair(t, core.Test{Name: "a", Run: body}, core.Test{Name: "b", Run: body}) {
		if r.Status != core.StatusFailed {
			t.Errorf("%s = %s, want failed", r.TestName, r.Status)
		}
	}
}

// A skipped body never reaches the check, and its window must not stay open
// to excuse every later body on the stack.
func TestGuardSkipClosesItsWindow(t *testing.T) {
	cfg, err := core.LoadConfigNoDefaults(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := guardedEnv{findings: []core.GuardFinding{restart}}
	run := func(test core.Test) *core.Result {
		_, rs := core.RunTests(context.Background(), []core.Test{test},
			core.RunOptions{Parallel: 1, Seed: core.NewRunSeed(1, true), Config: cfg, Provider: guardedProvider{env}})
		return rs[0]
	}
	run(core.Test{Name: "skipper", Tolerates: []string{core.GuardPodRestart}, Run: func(t *core.T) error { t.Skip("no"); return nil }})
	if r := run(core.Test{Name: "later", Run: func(*core.T) error { return nil }}); r.Status != core.StatusFailed {
		t.Errorf("later = %s, want failed on the restart", r.Status)
	}
}
