package jsonfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// TestJSONFileRecordsCharts: the per-result JSON round-trips the Charts a test
// emitted (kind, labels, series, points) so `ampqa report --from` can re-render
// them without re-running the test.
func TestJSONFileRecordsCharts(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.RunStarted(core.RunInfo{StartedAt: time.Now(), Seed: 7}); err != nil {
		t.Fatalf("RunStarted: %v", err)
	}
	res := &core.Result{
		TestName: "perf/pgbench",
		Suite:    "perf",
		Status:   core.StatusPassed,
		Charts: []core.Chart{{
			Title: "TPS over time", Kind: core.ChartLine, XLabel: "elapsed (s)", YLabel: "tps",
			Series: []core.ChartSeries{{Name: "iter 1", Points: []core.ChartPoint{{X: 10, Y: 452}, {X: 20, Y: 501}}}},
		}},
	}
	if err := s.ResultEvent(core.ResultEvent{Kind: core.EventTestFinished, TestName: res.TestName, Result: res}); err != nil {
		t.Fatalf("ResultEvent: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(s.Dir(), "perf_pgbench.json"))
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	// Unmarshal back into a core.Result — the exact path `ampqa report --from`
	// takes — so this asserts a true round-trip, not just the on-disk shape.
	var got core.Result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Charts) != 1 {
		t.Fatalf("Charts in JSON = %d, want 1", len(got.Charts))
	}
	c := got.Charts[0]
	if c.Title != "TPS over time" || c.Kind != core.ChartLine || c.YLabel != "tps" {
		t.Errorf("chart = %+v, want the line 'TPS over time' with y=tps", c)
	}
	if len(c.Series) != 1 || len(c.Series[0].Points) != 2 {
		t.Fatalf("series = %+v, want one series of two points", c.Series)
	}
	if p := c.Series[0].Points[1]; p.X != 20 || p.Y != 501 {
		t.Errorf("point[1] = %+v, want {20 501}", p)
	}
}
