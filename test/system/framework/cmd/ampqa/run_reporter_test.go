package main

import (
	"sync/atomic"
	"testing"

	"github.com/yugabyte/yb-amp/test/system/framework/core"
)

// countingSink records how many result events it received. It is fast, so as a
// critical sink it never blocks the fan-out.
type countingSink struct{ results atomic.Int64 }

func (c *countingSink) RunStarted(core.RunInfo) error      { return nil }
func (c *countingSink) ResultEvent(core.ResultEvent) error { c.results.Add(1); return nil }
func (c *countingSink) RunFinished(core.RunSummary) error  { return nil }

// blockingSink stalls on its first result event until release is closed,
// modelling a stuck/slow live sink so its bounded queue overflows.
type blockingSink struct {
	release <-chan struct{}
	got     atomic.Int64
}

func (b *blockingSink) RunStarted(core.RunInfo) error { return nil }
func (b *blockingSink) ResultEvent(core.ResultEvent) error {
	if b.got.Add(1) == 1 {
		<-b.release
	}
	return nil
}
func (b *blockingSink) RunFinished(core.RunSummary) error { return nil }

// TestBuildReporterKeepsResultSinksCritical proves the run's reporter wiring
// treats pass/fail records as critical: with a stuck non-critical progress sink
// forcing queue overflow (Dropped() > 0), the critical result sink still
// receives every result event — none silently lost.
func TestBuildReporterKeepsResultSinksCritical(t *testing.T) {
	release := make(chan struct{})
	progress := &blockingSink{release: release}
	result := &countingSink{}

	const queue = 4
	r := buildReporter(queue, progress, result)

	const n = 200
	for i := 0; i < n; i++ {
		_ = r.ResultEvent(core.ResultEvent{})
	}
	if r.Dropped() == 0 {
		close(release)
		r.Close()
		t.Fatal("expected the stuck non-critical progress sink to drop events, got 0")
	}

	close(release)
	r.Close()

	if got := result.results.Load(); got != n {
		t.Fatalf("critical result sink lost %d of %d result events", int64(n)-got, n)
	}
}

// TestRunDataSurfacesDroppedCounts proves the run's JSON envelope surfaces
// dropped-record counts so an agent can't read "green" while data was dropped.
func TestRunDataSurfacesDroppedCounts(t *testing.T) {
	results := []*core.Result{
		{StepsDropped: 3, MetricsDropped: 2},
		{StepsDropped: 1},
		nil,
	}
	d := runData(1, 2, core.RunSummary{Total: 2}, results, 7)

	if got := d["reporter_dropped"].(int64); got != 7 {
		t.Errorf("reporter_dropped = %d, want 7", got)
	}
	if got := d["steps_dropped"].(int); got != 4 {
		t.Errorf("steps_dropped = %d, want 4", got)
	}
	if got := d["metrics_dropped"].(int); got != 2 {
		t.Errorf("metrics_dropped = %d, want 2", got)
	}
}
