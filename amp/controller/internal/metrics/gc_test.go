package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The GC counters are written with a fixed number of label values, and a
// mismatch against the CounterVec's labels panics at call time rather than
// failing to compile — in the sweeper's own goroutine, on a real orphan. This
// test is what makes that a build failure instead.
func TestGCOrphansCountsByResourceAndOutcome(t *testing.T) {
	m := New()
	m.GCOrphans("cluster", 2, 1, 0)
	m.GCOrphans("endpoint", 1, 0, 3)

	want := `
# HELP amp_controller_gc_orphans_total Failed-create orphans seen by the GC sweeper, by resource (cluster, endpoint) and outcome: enqueued (teardown recorded), skipped (no longer reclaimable), failed (left for the next sweep).
# TYPE amp_controller_gc_orphans_total counter
amp_controller_gc_orphans_total{outcome="enqueued",resource="cluster"} 2
amp_controller_gc_orphans_total{outcome="enqueued",resource="endpoint"} 1
amp_controller_gc_orphans_total{outcome="failed",resource="cluster"} 0
amp_controller_gc_orphans_total{outcome="failed",resource="endpoint"} 3
amp_controller_gc_orphans_total{outcome="skipped",resource="cluster"} 1
amp_controller_gc_orphans_total{outcome="skipped",resource="endpoint"} 0
`
	if err := testutil.CollectAndCompare(m.gcOrphans, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected metrics: %v", err)
	}
}

// One sweep is one observation however many resource kinds it swept, which is
// what makes a flat orphans_total against a rising sweeps_total mean "running and
// idle" rather than "stuck".
func TestGCSweptCountsOneSweepPerPass(t *testing.T) {
	m := New()
	m.GCSwept(2 * time.Second)
	m.GCSwept(time.Second)

	if got := testutil.ToFloat64(m.gcSweeps); got != 2 {
		t.Errorf("sweeps_total = %v, want 2", got)
	}
}

// A nil *Metrics is the --metrics=false case, which every recorder must absorb.
func TestGCRecordersTolerateNilMetrics(t *testing.T) {
	var m *Metrics
	m.GCSwept(time.Second)
	m.GCOrphans("cluster", 1, 1, 1)
}
