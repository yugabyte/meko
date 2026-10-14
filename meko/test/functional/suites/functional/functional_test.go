package functional_test

import (
	"context"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/fake"
	_ "github.com/yugabyte/meko/meko/test/functional/suites/functional"
)

func TestEveryFunctionalTestIsInTheRegressionSet(t *testing.T) {
	all, err := core.Default().Filter("functional", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	tagged, err := core.Default().Filter("functional", "", []string{"regression"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 35 || len(tagged) != len(all) {
		t.Fatalf("%d functional tests, %d tagged regression, want 35 and 35", len(all), len(tagged))
	}
}

// With a provider that has no meko stack, a stack test skips. It must not fail
// and it must not pass.
func TestStackTestsSkipOnAProviderWithoutAStack(t *testing.T) {
	all, err := core.Default().Filter("functional", "", []string{"regression"})
	if err != nil {
		t.Fatal(err)
	}
	var tests []core.Test
	for _, tc := range all {
		if len(tc.Tags) == 2 {
			tests = append(tests, tc)
		}
	}
	if len(tests) != 30 {
		t.Fatalf("selected %d stack tests, want 30", len(tests))
	}
	for _, tc := range tests {
		if tc.Env.Shape != core.SingleStack {
			t.Errorf("%s: shape %s, want SingleStack", tc.Name, tc.Env.Shape)
		}
	}
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, results := core.RunTests(context.Background(), tests, core.RunOptions{
		Parallel: 1, Seed: core.NewRunSeed(1, true), Config: cfg, Provider: &fake.Provider{},
	})
	for _, r := range results {
		if r.Status != core.StatusSkipped {
			t.Errorf("%s = %s (%s), want skipped", r.TestName, r.Status, r.ErrorSummary)
		}
	}
}
