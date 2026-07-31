package core_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/fake"
)

// setupTest builds a setup-capable test whose Setup records that it ran, returns
// a standing env, and hands back a teardown that increments teardowns when run.
func setupTest(name string, setupRan, bodyRan, teardowns *atomic.Int64) core.Test {
	return core.Test{
		Name:     name,
		Suite:    "t",
		Category: core.Functional,
		Env:      core.EnvSpec{Sharing: core.Persistent, Shape: core.Universe},
		Setup: func(t *core.T) (*core.SetupResult, error) {
			setupRan.Add(1)
			return &core.SetupResult{
				Env: &core.StandingEnv{
					Name:     "standing-" + name,
					Shape:    "fake",
					Provider: "fake",
					Ref:      "fake-env-1",
					Info:     map[string]string{"ysql_dsn": "host=fake"},
				},
				Teardown: func(context.Context) error { teardowns.Add(1); return nil },
			}, nil
		},
		Run: func(t *core.T) error { bodyRan.Add(1); return nil },
	}
}

// TestSetupOneLeavesEnvUp: SetupOne runs Setup, returns the standing env, and
// does NOT run the body or the teardown — the environment is left running.
func TestSetupOneLeavesEnvUp(t *testing.T) {
	var setupRan, bodyRan, teardowns atomic.Int64
	test := setupTest("setup/leave_up", &setupRan, &bodyRan, &teardowns)

	result, sr := core.SetupOne(context.Background(), test, core.RunOptions{})
	if result.Status != core.StatusPassed {
		t.Fatalf("status = %s, want passed (%s)", result.Status, result.ErrorSummary)
	}
	if setupRan.Load() != 1 {
		t.Errorf("setup ran %d times, want 1", setupRan.Load())
	}
	if bodyRan.Load() != 0 {
		t.Errorf("body ran %d times, want 0 (setup must not run the body)", bodyRan.Load())
	}
	if teardowns.Load() != 0 {
		t.Errorf("teardown ran %d times, want 0 (env must be left up)", teardowns.Load())
	}
	if sr == nil || sr.Env == nil {
		t.Fatalf("no standing env returned")
	}
	if sr.Env.Ref != "fake-env-1" || sr.Env.Provider != "fake" {
		t.Errorf("standing env = %+v, want provider=fake ref=fake-env-1", sr.Env)
	}
	if sr.Env.Info["ysql_dsn"] != "host=fake" {
		t.Errorf("connection info not carried through: %+v", sr.Env.Info)
	}
}

// TestSetupOneRejectsNonSetupTest: a test without a Setup phase is a usage/
// framework error, reported broken, never passed.
func TestSetupOneRejectsNonSetupTest(t *testing.T) {
	test := core.Test{Name: "setup/no_setup", Suite: "t", Category: core.Functional}
	result, sr := core.SetupOne(context.Background(), test, core.RunOptions{})
	if result.Status != core.StatusBroken {
		t.Fatalf("status = %s, want broken", result.Status)
	}
	if sr != nil {
		t.Errorf("standing env returned for a non-setup test: %+v", sr)
	}
}

// TestRunUsesSetupAndTearsDown: the SAME Setup, run through the normal RunOne
// lifecycle, runs the body AND the teardown — nothing is left running. This is
// the shared-provisioning contract: run and setup call the same Setup, only run
// reaps it.
func TestRunUsesSetupAndTearsDown(t *testing.T) {
	var setupRan, bodyRan, teardowns atomic.Int64
	test := setupTest("setup/run_reaps", &setupRan, &bodyRan, &teardowns)

	result := core.RunOne(context.Background(), test, core.RunOptions{
		Seed:     core.NewRunSeed(1, true),
		Provider: &fake.Provider{},
	})
	if result.Status != core.StatusPassed {
		t.Fatalf("status = %s, want passed (%s)", result.Status, result.ErrorSummary)
	}
	if setupRan.Load() != 1 || bodyRan.Load() != 1 {
		t.Errorf("setup=%d body=%d, want 1/1 (run calls Setup then the body)", setupRan.Load(), bodyRan.Load())
	}
	if teardowns.Load() != 1 {
		t.Errorf("teardown ran %d times, want 1 (run reaps what Setup created)", teardowns.Load())
	}
}

// TestSetupOneSetupErrorIsEnvFailed: a Setup that fails to provision is an
// environment failure, not a test failure.
func TestSetupOneSetupErrorIsEnvFailed(t *testing.T) {
	test := core.Test{
		Name:     "setup/prov_fail",
		Suite:    "t",
		Category: core.Functional,
		Setup: func(t *core.T) (*core.SetupResult, error) {
			return nil, context.DeadlineExceeded
		},
	}
	result, sr := core.SetupOne(context.Background(), test, core.RunOptions{})
	if result.Status != core.StatusEnvFailed {
		t.Fatalf("status = %s, want env_failed", result.Status)
	}
	if sr != nil {
		t.Errorf("standing env returned despite setup failure: %+v", sr)
	}
}
