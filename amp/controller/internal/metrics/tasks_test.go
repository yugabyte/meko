package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Gathered from the registry rather than read off the counter, because what is
// under test is that the series is exposed at 0 before any reclaim: an alert on
// a series that only appears once something has gone wrong has nothing to fire on
// while it is healthy.
func TestTasksReclaimedIsExposedAtZeroFromStartup(t *testing.T) {
	m := New()
	const name = "amp_controller_tasks_reclaimed_total"
	header := `
# HELP amp_controller_tasks_reclaimed_total Tasks taken over from another controller process that left them running: it died holding their leases, or shut down with them in flight.
# TYPE amp_controller_tasks_reclaimed_total counter
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(header+name+" 0\n"), name); err != nil {
		t.Fatalf("before any reclaim: %v", err)
	}

	m.TaskReclaimed()
	m.TaskReclaimed()
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(header+name+" 2\n"), name); err != nil {
		t.Fatalf("after two reclaims: %v", err)
	}
}

func TestTaskReclaimedToleratesNilMetrics(t *testing.T) {
	var m *Metrics
	m.TaskReclaimed()
}
