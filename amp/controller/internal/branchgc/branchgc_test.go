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
