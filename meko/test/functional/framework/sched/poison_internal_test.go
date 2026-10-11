package sched

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

type probeProvider struct {
	core.EnvProvider
	err error
}

func (p *probeProvider) SharesInfraAcrossTests() bool             { return true }
func (p *probeProvider) SharedInfraHealthy(context.Context) error { return p.err }

func newPoisonScheduler(probe error) *scheduler {
	s := &scheduler{infraGateArmed: true}
	s.opts.Provider = &probeProvider{err: probe}
	s.opts.Seed = core.NewRunSeed(1, true)
	return s
}

func faultingTest(name string) core.Test {
	return core.Test{Name: name, Env: core.EnvSpec{Sharing: core.Exclusive, FaultsSharedInfra: true}}
}

// A failed faulting test whose stack never came back poisons the run, and the
// tests that follow name it rather than each rediscovering it on a timeout.
func TestPoisonNamesTheCulpritAfterUnrecoveredFault(t *testing.T) {
	s := newPoisonScheduler(fmt.Errorf("controller not ready"))

	s.noteInfraOutcome(context.Background(), faultingTest("chaos"), &core.Result{Status: core.StatusFailed})

	res := s.failIfInfraPoisoned(core.Test{Name: "after"})
	if res == nil || res.Status != core.StatusEnvFailed {
		t.Fatalf("a later test must env_fail once the run is poisoned, got %v", res)
	}
	if !strings.Contains(res.ErrorSummary, "chaos") {
		t.Errorf("the summary must name the culprit: %q", res.ErrorSummary)
	}
}

// A stack that answers healthy leaves the run alone; so does a passing faulting
// test, whose own shared_infra_recovered step already probed.
func TestPoisonHoldsOffWhenTheStackServes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		probe  error
		status core.Status
	}{
		{"healthy probe", nil, core.StatusFailed},
		{"passed fault test", fmt.Errorf("never probed"), core.StatusPassed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newPoisonScheduler(tc.probe)
			s.noteInfraOutcome(context.Background(), faultingTest("chaos"), &core.Result{Status: tc.status})
			if res := s.failIfInfraPoisoned(core.Test{Name: "after"}); res != nil {
				t.Fatalf("the run must not be poisoned: %s", res.ErrorSummary)
			}
		})
	}
}

// A cancelled run must not spend the probe budget on its way out.
func TestPoisonSkipsProbeOnCancelledRun(t *testing.T) {
	s := newPoisonScheduler(fmt.Errorf("controller not ready"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.noteInfraOutcome(ctx, faultingTest("chaos"), &core.Result{Status: core.StatusFailed})
	if res := s.failIfInfraPoisoned(core.Test{Name: "after"}); res != nil {
		t.Fatalf("a cancelled run must not poison: %s", res.ErrorSummary)
	}
}

type rebuildProvider struct {
	probeProvider
	rebuildErr error
	rebuilds   int
}

func (p *rebuildProvider) RebuildSharedInfra(context.Context) error {
	p.rebuilds++
	return p.rebuildErr
}

// A stack that stays down after a fault is rebuilt, and the run goes on.
func TestUnrecoveredFaultRebuildsTheStack(t *testing.T) {
	p := &rebuildProvider{probeProvider: probeProvider{err: fmt.Errorf("api not serving")}}
	s := &scheduler{infraGateArmed: true}
	s.opts.Provider, s.opts.Seed = p, core.NewRunSeed(1, true)

	s.noteInfraOutcome(context.Background(), faultingTest("chaos"), &core.Result{Status: core.StatusFailed})

	if p.rebuilds != 1 || s.sharedGen.Load() != 1 {
		t.Fatalf("rebuilds = %d, generation = %d, want 1 and 1", p.rebuilds, s.sharedGen.Load())
	}
	if res := s.failIfInfraPoisoned(core.Test{Name: "after"}); res != nil {
		t.Fatalf("a rebuilt stack must not poison the run: %s", res.ErrorSummary)
	}
}

func TestFailedRebuildPoisonsWithBothErrors(t *testing.T) {
	p := &rebuildProvider{probeProvider: probeProvider{err: fmt.Errorf("api not serving")}, rebuildErr: fmt.Errorf("migrations wedged")}
	s := &scheduler{infraGateArmed: true}
	s.opts.Provider, s.opts.Seed = p, core.NewRunSeed(1, true)

	s.noteInfraOutcome(context.Background(), faultingTest("chaos"), &core.Result{Status: core.StatusFailed})

	res := s.failIfInfraPoisoned(core.Test{Name: "after"})
	if res == nil || !strings.Contains(res.ErrorSummary, "api not serving") || !strings.Contains(res.ErrorSummary, "migrations wedged") {
		t.Fatalf("want a poison naming the outage and the failed rebuild, got %v", res)
	}
}
