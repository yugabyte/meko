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

// TestDatapointEventCarriesIdentity: once SetPerfIdentity has run, every emitted
// EventDatapoint carries the identity so a live sink can store the datapoint
// with its load signature. Datapoints emitted before it carry none.
func TestDatapointEventCarriesIdentity(t *testing.T) {
	res := &Result{}
	cap := &captureEmitter{}
	seed := NewRunSeed(1, true).ForTest("perf/ident")
	tt := newT(context.Background(), nil, seed, nil, nil, res, cap)

	tt.Datapoint(Datapoint{Name: "early", Value: 1})
	tt.SetPerfIdentity(PerfIdentity{LoadSignature: "abc123", YBImage: "yb:tag"})
	tt.Datapoint(Datapoint{Name: "late", Value: 2})

	var got []ResultEvent
	for _, ev := range cap.events {
		if ev.Kind == EventDatapoint {
			got = append(got, ev)
		}
	}
	if len(got) != 2 {
		t.Fatalf("emitted %d datapoint events, want 2", len(got))
	}
	if got[0].PerfIdentity != nil {
		t.Errorf("datapoint emitted before SetPerfIdentity carried %+v, want nil", got[0].PerfIdentity)
	}
	if got[1].PerfIdentity == nil || got[1].PerfIdentity.LoadSignature != "abc123" {
		t.Errorf("datapoint identity = %+v, want load_signature abc123", got[1].PerfIdentity)
	}
}
