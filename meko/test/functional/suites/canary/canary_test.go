package canary_test

import (
	"context"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/fake"
	"github.com/yugabyte/meko/meko/test/functional/suites/canary"
)

func TestEveryCanaryLandsWithItsIntendedStatus(t *testing.T) {
	tests, err := core.Default().Filter("canary", "", nil)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	cfg, err := core.LoadConfig(nil, nil, []string{"versions.product=1.0.0"})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	prov := &fake.Provider{FailLabelSubstr: canary.EnvFailSubstr}
	_, results := core.RunTests(context.Background(), tests, core.RunOptions{
		Parallel:         1,
		Seed:             core.NewRunSeed(1, true),
		Config:           cfg,
		Provider:         prov,
		DetectedVersions: core.DetectedVersions{Product: "1.0.0"},
	})

	want := map[string]core.Status{
		"canary/pass":               core.StatusPassed,
		"canary/fail":               core.StatusFailed,
		"canary/panic":              core.StatusBroken,
		"canary/hang":               core.StatusTimedOut,
		"canary/provision_env_fail": core.StatusEnvFailed,
		"canary/skip_version":       core.StatusSkipped,
	}
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d", len(results), len(want))
	}
	for _, r := range results {
		if r.Status != want[r.TestName] {
			t.Errorf("%s = %s, want %s", r.TestName, r.Status, want[r.TestName])
		}
	}
}
