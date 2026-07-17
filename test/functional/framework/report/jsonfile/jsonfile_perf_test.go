package jsonfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// TestJSONFileRecordsPerfDatapoints: the per-result JSON carries the Perf
// datapoints (name/value/unit/direction + baseline comparison).
func TestJSONFileRecordsPerfDatapoints(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.RunStarted(core.RunInfo{StartedAt: time.Now(), Seed: 7}); err != nil {
		t.Fatalf("RunStarted: %v", err)
	}
	res := &core.Result{
		TestName: "perf/tpcc",
		Suite:    "perf",
		Status:   core.StatusPassed,
		Perf: []core.Datapoint{
			{Name: "tpcc.throughput", Value: 1000, Unit: "req/s", Direction: core.HigherIsBetter,
				Baseline: &core.BaselineComparison{Value: 950, DeltaPct: 5.3, Threshold: 10, Regressed: false}},
		},
	}
	if err := s.ResultEvent(core.ResultEvent{Kind: core.EventTestFinished, TestName: res.TestName, Result: res}); err != nil {
		t.Fatalf("ResultEvent: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(s.Dir(), "perf_tpcc.json"))
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var doc struct {
		Perf []struct {
			Name      string
			Value     float64
			Unit      string
			Direction string
			Baseline  *struct {
				Value     float64
				DeltaPct  float64
				Regressed bool
			}
		}
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Perf) != 1 {
		t.Fatalf("Perf datapoints in JSON = %d, want 1", len(doc.Perf))
	}
	dp := doc.Perf[0]
	if dp.Name != "tpcc.throughput" || dp.Value != 1000 || dp.Unit != "req/s" || dp.Direction != "higher_is_better" {
		t.Errorf("datapoint = %+v, want throughput 1000 req/s higher_is_better", dp)
	}
	if dp.Baseline == nil || dp.Baseline.Value != 950 || dp.Baseline.Regressed {
		t.Errorf("baseline = %+v, want {950 ... not regressed}", dp.Baseline)
	}
}
