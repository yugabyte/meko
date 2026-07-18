package core_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/fake"
)

func runOne(t *testing.T, test core.Test, provider core.EnvProvider, detected core.DetectedVersions) *core.Result {
	t.Helper()
	_, results := core.RunTests(context.Background(), []core.Test{test}, core.RunOptions{
		Parallel:         1,
		Seed:             core.NewRunSeed(1, true),
		Config:           core.NewConfig(),
		Provider:         provider,
		DetectedVersions: detected,
	})
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	return results[0]
}

func TestLifecyclePass(t *testing.T) {
	p := &fake.Provider{}
	r := runOne(t, core.Test{Name: "t", Category: core.Functional, Run: func(*core.T) error { return nil }}, p, core.DetectedVersions{})
	if r.Status != core.StatusPassed {
		t.Errorf("status = %s, want passed (%q)", r.Status, r.ErrorSummary)
	}
	if p.TeardownCount() != 1 {
		t.Errorf("teardown ran %d times, want 1", p.TeardownCount())
	}
}

func TestLifecycleReturnedErrorFails(t *testing.T) {
	p := &fake.Provider{}
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error { return fmt.Errorf("boom") }}, p, core.DetectedVersions{})
	if r.Status != core.StatusFailed {
		t.Errorf("status = %s, want failed", r.Status)
	}
	if r.ErrorSummary != "boom" {
		t.Errorf("error summary = %q, want boom", r.ErrorSummary)
	}
	if p.TeardownCount() != 1 {
		t.Errorf("teardown must still run on failure")
	}
}

func TestLifecycleFailHelper(t *testing.T) {
	p := &fake.Provider{}
	r := runOne(t, core.Test{Name: "t", Run: func(tt *core.T) error {
		tt.Failf("assertion %d", 1)
		return nil
	}}, p, core.DetectedVersions{})
	if r.Status != core.StatusFailed {
		t.Errorf("status = %s, want failed via t.Fail", r.Status)
	}
}

func TestLifecyclePanicIsBroken(t *testing.T) {
	p := &fake.Provider{}
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error { panic("kaboom") }}, p, core.DetectedVersions{})
	if r.Status != core.StatusBroken {
		t.Errorf("status = %s, want broken", r.Status)
	}
	if p.TeardownCount() != 1 {
		t.Errorf("teardown must run after a panic")
	}
}

func TestLifecycleProvisionFailIsEnvFailed(t *testing.T) {
	p := &fake.Provider{FailProvision: true}
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error { return nil }}, p, core.DetectedVersions{})
	if r.Status != core.StatusEnvFailed {
		t.Errorf("status = %s, want env_failed", r.Status)
	}
	if p.TeardownCount() != 0 {
		t.Errorf("teardown must NOT run when provisioning failed, ran %d", p.TeardownCount())
	}
}

func TestLifecycleVersionOutOfBoundsSkips(t *testing.T) {
	p := &fake.Provider{}
	test := core.Test{
		Name:     "t",
		Versions: core.VersionSpec{MinProduct: "2025.2.0.0"},
		Run:      func(*core.T) error { return nil },
	}
	r := runOne(t, test, p, core.DetectedVersions{Product: "2025.1.0.0"})
	if r.Status != core.StatusSkipped {
		t.Errorf("status = %s, want skipped", r.Status)
	}
	if r.SkipReason == "" {
		t.Errorf("skip reason should be recorded")
	}
	if p.ProvisionCount() != 0 || p.TeardownCount() != 0 {
		t.Errorf("must skip before provisioning: prov=%d teardown=%d", p.ProvisionCount(), p.TeardownCount())
	}
}

func TestLifecycleSlowTestTimesOut(t *testing.T) {
	p := &fake.Provider{}
	test := core.Test{
		Name:    "t",
		Timeout: 30 * time.Millisecond,
		Run: func(*core.T) error {
			time.Sleep(2 * time.Second)
			return nil
		},
	}
	r := runOne(t, test, p, core.DetectedVersions{})
	if r.Status != core.StatusTimedOut {
		t.Errorf("status = %s, want timed_out", r.Status)
	}
	if p.TeardownCount() != 1 {
		t.Errorf("teardown must run after a timeout")
	}
}

func TestLifecycleLiveVersionsRecorded(t *testing.T) {
	p := &fake.Provider{Versions: core.DetectedVersions{Product: "2025.1.0.0", YB: "2.20.0.0"}}
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error { return nil }}, p, core.DetectedVersions{})
	if r.Versions.Product != "2025.1.0.0" || r.Versions.YB != "2.20.0.0" {
		t.Errorf("live versions not recorded from env: %+v", r.Versions)
	}
}

func TestLifecycleParallelNoRaces(t *testing.T) {
	p := &fake.Provider{}
	const n = 20
	tests := make([]core.Test, n)
	for i := range tests {
		tests[i] = core.Test{
			Name:     fmt.Sprintf("t%d", i),
			Category: core.Functional,
			Run: func(tt *core.T) error {
				return tt.Step("work", func(s *core.StepHandle) error {
					s.Set("i", tt.Seed.Value())
					return nil
				})
			},
		}
	}
	summary, results := core.RunTests(context.Background(), tests, core.RunOptions{
		Parallel: 4,
		Seed:     core.NewRunSeed(1, true),
		Provider: p,
	})
	if summary.Counts[core.StatusPassed] != n {
		t.Errorf("passed = %d, want %d", summary.Counts[core.StatusPassed], n)
	}
	for i, r := range results {
		if r == nil || r.Status != core.StatusPassed {
			t.Errorf("result %d not passed: %+v", i, r)
		}
	}
	if p.TeardownCount() != n {
		t.Errorf("teardown ran %d times, want %d", p.TeardownCount(), n)
	}
}
