package html_test

import (
	"strings"
	"testing"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	rhtml "github.com/yugabyte/yb-amp/test/functional/framework/report/html"
)

// TestHTMLRendersPerfDatapoints: a result's perf datapoints render in a "Perf
// datapoints" section with their baseline comparison, and a regressed one is
// tagged with the failure status class.
func TestHTMLRendersPerfDatapoints(t *testing.T) {
	res := &core.Result{
		TestName: "perf/tpcc",
		Suite:    "perf",
		Status:   core.StatusFailed,
		Perf: []core.Datapoint{
			{Name: "tpcc.throughput", Value: 700, Unit: "req/s", Direction: core.HigherIsBetter,
				Baseline: &core.BaselineComparison{Value: 1000, DeltaPct: -30, Threshold: 10, Regressed: true}},
			{Name: "tpcc.first_run", Value: 5, Unit: "req/s", Direction: core.HigherIsBetter},
		},
	}
	out, err := rhtml.Render(rhtml.RunData{Results: []*core.Result{res}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"Perf datapoints",
		"tpcc.throughput",
		"700 req/s",
		"REGRESSED",
		"st-fail", // the regressed datapoint's status class
		"tpcc.first_run",
		"no baseline",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("html missing %q", want)
		}
	}
}

// TestHTMLRendersPerfIdentity: a result's PerfIdentity renders as a "Variant"
// block with its non-empty fields; a result without one has no such block.
func TestHTMLRendersPerfIdentity(t *testing.T) {
	res := &core.Result{
		TestName: "perf/tpcc",
		Suite:    "perf",
		Status:   core.StatusPassed,
		PerfIdentity: &core.PerfIdentity{
			Variant:            "ctrl-A",
			YBImage:            "quay.io/yugabyte/yugabyte:2025.2.1.0",
			AmpControllerImage: "amp/controller:A",
			LoadSignature:      "sig123456789",
		},
	}
	out, err := rhtml.Render(rhtml.RunData{Results: []*core.Result{res}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"Variant",
		"ctrl-A",
		"quay.io/yugabyte/yugabyte:2025.2.1.0",
		"amp/controller:A",
		"sig123456789",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("html missing identity field %q", want)
		}
	}

	plain := &core.Result{TestName: "functional/x", Suite: "functional", Status: core.StatusPassed}
	out2, err := rhtml.Render(rhtml.RunData{Results: []*core.Result{plain}})
	if err != nil {
		t.Fatalf("render plain: %v", err)
	}
	if strings.Contains(out2, "<h4>Variant</h4>") {
		t.Errorf("non-perf result rendered a Variant block")
	}
}
