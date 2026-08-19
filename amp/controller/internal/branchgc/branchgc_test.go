package branchgc

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yugabyte/amp/controller/internal/config"
)

// TestRunReturnsImmediatelyWhenDisabled pins the contract cmd/controller relies
// on: it starts the reaper unconditionally, exactly as it starts gc.Sweeper, so
// honouring --branch-gc=false is Run's job. Were the gate dropped, a controller
// with the reaper switched off would tick and delete anyway.
//
// The nil pool is the assertion. A gated Run cannot reach it; a Run that ticks
// panics rather than quietly passing.
func TestRunReturnsImmediatelyWhenDisabled(t *testing.T) {
	r := New(nil, config.BranchGCConfig{Enabled: false, Interval: time.Millisecond},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	done := make(chan struct{})
	go func() { r.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return with Enabled=false")
	}
}

// TestNextIntervalAdaptsToTheTickOutcome pins the three-state cadence: base
// while reclaiming, the floor while a backlog remains, doubling to the ceiling
// while there is nothing due.
func TestNextIntervalAdaptsToTheTickOutcome(t *testing.T) {
	base := 30 * time.Second

	if got := nextInterval(base, tickStats{BranchesReclaimed: 1, RowsDeleted: 100}, 0); got != base {
		t.Errorf("a reclaiming tick: got %v, want %v", got, base)
	}
	if got := nextInterval(base, tickStats{RowsDeleted: 20000, BudgetSpent: true}, 0); got != 5*time.Second {
		t.Errorf("a budget-capped tick: got %v, want 5s", got)
	}
	// Branches whose data was already gone delete no rows, but the tick reaped
	// them — it must not read as idle.
	if got := nextInterval(base, tickStats{BranchesReclaimed: 3}, 0); got != base {
		t.Errorf("a tick reclaiming empty branches: got %v, want %v", got, base)
	}

	// A short base hits the absolute floor rather than a sixth of itself, and
	// never waits longer than the base.
	if got := nextInterval(time.Second, tickStats{RowsDeleted: 5, BudgetSpent: true}, 0); got != time.Second {
		t.Errorf("capped tick on a 1s base: got %v, want 1s", got)
	}
	if got := nextInterval(500*time.Millisecond, tickStats{RowsDeleted: 5, BudgetSpent: true}, 0); got != 500*time.Millisecond {
		t.Errorf("capped tick on a 500ms base: got %v, want 500ms", got)
	}

	// An idle run doubles from the base up to the ceiling, and stays there.
	var idle time.Duration
	for _, want := range []time.Duration{60 * time.Second, 2 * time.Minute, 4 * time.Minute, 4 * time.Minute} {
		got := nextInterval(base, tickStats{}, idle)
		if got != want {
			t.Fatalf("idle backoff from %v: got %v, want %v", idle, got, want)
		}
		idle = got
	}

	// A capped tick takes the floor however long the loop had been backed off.
	if got := nextInterval(base, tickStats{BudgetSpent: true}, idle); got != 5*time.Second {
		t.Errorf("a capped tick after backoff: got %v, want 5s", got)
	}
}
