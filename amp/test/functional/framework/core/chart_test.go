package core

import (
	"context"
	"testing"
)

// TestChartRecordedAndEmitted: T.Chart appends the chart to the result's Charts
// slice (preserving kind/series/points) and emits a matching EventChart carrying
// the payload so live sinks see it too — exactly parallel to T.Datapoint.
func TestChartRecordedAndEmitted(t *testing.T) {
	res := &Result{}
	cap := &captureEmitter{}
	seed := NewRunSeed(1, true).ForTest("perf/chart")
	tt := newT(context.Background(), nil, seed, nil, nil, res, cap)

	tt.Chart(Chart{
		Title: "TPS over time", Kind: ChartLine, XLabel: "elapsed (s)", YLabel: "tps",
		Series: []ChartSeries{{Name: "iter 1", Points: []ChartPoint{{X: 10, Y: 100}, {X: 20, Y: 120}}}},
	})
	tt.Chart(Chart{Title: "Per-iteration TPS", Kind: ChartBar})

	if len(res.Charts) != 2 {
		t.Fatalf("recorded %d charts, want 2", len(res.Charts))
	}
	c0 := res.Charts[0]
	if c0.Title != "TPS over time" || c0.Kind != ChartLine {
		t.Errorf("chart[0] = %+v, want line 'TPS over time'", c0)
	}
	if len(c0.Series) != 1 || len(c0.Series[0].Points) != 2 || c0.Series[0].Points[1].Y != 120 {
		t.Errorf("chart[0] series = %+v, want one series of two points ending at y=120", c0.Series)
	}

	var emitted int
	for _, ev := range cap.events {
		if ev.Kind != EventChart {
			continue
		}
		emitted++
		if ev.Chart == nil {
			t.Errorf("EventChart carried a nil Chart")
		}
	}
	if emitted != 2 {
		t.Errorf("emitted %d EventChart events, want 2", emitted)
	}
}
