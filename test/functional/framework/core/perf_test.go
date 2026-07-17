package core

import (
	"context"
	"testing"
)

// captureEmitter records every emitted event for assertions.
type captureEmitter struct{ events []ResultEvent }

func (c *captureEmitter) emit(ev ResultEvent) { c.events = append(c.events, ev) }

// TestDatapointRecordedAndEmitted: T.Datapoint appends the datapoint to the
// result's Perf slice (preserving the baseline comparison) and emits a matching
// EventDatapoint so live sinks see it too.
func TestDatapointRecordedAndEmitted(t *testing.T) {
	res := &Result{}
	cap := &captureEmitter{}
	seed := NewRunSeed(1, true).ForTest("perf/rec")
	tt := newT(context.Background(), nil, seed, nil, nil, res, cap)

	tt.Datapoint(Datapoint{Name: "tpcc.throughput", Value: 1234.5, Unit: "req/s", Direction: HigherIsBetter})
	tt.Datapoint(Datapoint{
		Name: "tpcc.latency_p99", Value: 42, Unit: "ms", Direction: LowerIsBetter,
		Baseline: &BaselineComparison{Value: 40, DeltaPct: 5, Threshold: 20, Regressed: false},
	})

	if len(res.Perf) != 2 {
		t.Fatalf("recorded %d datapoints, want 2", len(res.Perf))
	}
	if res.Perf[0].Name != "tpcc.throughput" || res.Perf[0].Value != 1234.5 || res.Perf[0].Direction != HigherIsBetter {
		t.Errorf("datapoint[0] = %+v, want throughput 1234.5 higher-is-better", res.Perf[0])
	}
	if b := res.Perf[1].Baseline; b == nil || b.Value != 40 || b.Regressed {
		t.Errorf("datapoint[1] baseline = %+v, want {40 ... not regressed}", b)
	}

	var emitted int
	for _, ev := range cap.events {
		if ev.Kind != EventDatapoint {
			continue
		}
		emitted++
		if ev.Datapoint == nil {
			t.Errorf("EventDatapoint carried a nil Datapoint")
		}
	}
	if emitted != 2 {
		t.Errorf("emitted %d EventDatapoint events, want 2", emitted)
	}
}
