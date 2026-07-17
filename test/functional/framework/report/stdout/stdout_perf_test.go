package stdout

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// TestStdoutRendersPerfDatapoints: the finished-test line is followed by one
// perf line per datapoint, showing the baseline comparison and a REGRESSED
// marker where a datapoint crossed its threshold.
func TestStdoutRendersPerfDatapoints(t *testing.T) {
	var buf bytes.Buffer
	s := NewWriter(&buf, false)
	res := &core.Result{
		TestName: "perf/tpcc",
		Status:   core.StatusFailed,
		Perf: []core.Datapoint{
			{Name: "tpcc.throughput", Value: 700, Unit: "req/s", Direction: core.HigherIsBetter,
				Baseline: &core.BaselineComparison{Value: 1000, DeltaPct: -30, Threshold: 10, Regressed: true}},
			{Name: "tpcc.latency_p99", Value: 42, Unit: "ms", Direction: core.LowerIsBetter,
				Baseline: &core.BaselineComparison{Value: 40, DeltaPct: 5, Threshold: 20, Regressed: false}},
			{Name: "tpcc.first_run", Value: 5, Unit: "req/s", Direction: core.HigherIsBetter},
		},
	}
	if err := s.ResultEvent(core.ResultEvent{Kind: core.EventTestFinished, TestName: res.TestName, Result: res}); err != nil {
		t.Fatalf("ResultEvent: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"perf: tpcc.throughput 700 req/s",
		"REGRESSED",
		"perf: tpcc.latency_p99 42 ms",
		"baseline 40",
		"no baseline",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\n---\n%s", want, out)
		}
	}
}
