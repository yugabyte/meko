package junit_test

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/report/junit"
)

type tcaseProps struct {
	Name       string `xml:"name,attr"`
	Properties struct {
		Property []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:"value,attr"`
		} `xml:"property"`
	} `xml:"properties"`
}

type tsuitesProps struct {
	XMLName xml.Name `xml:"testsuites"`
	Suites  []struct {
		Cases []tcaseProps `xml:"testcase"`
	} `xml:"testsuite"`
}

// TestJUnitRendersPerfProperties: perf datapoints render as JUnit <property>
// elements under the testcase, keyed perf.<name>, with the baseline delta and a
// REGRESSED marker when gated.
func TestJUnitRendersPerfProperties(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junit.xml")
	s := junit.New(path)
	res := &core.Result{
		TestName: "perf/tpcc",
		Suite:    "perf",
		Status:   core.StatusPassed,
		Perf: []core.Datapoint{
			{Name: "tpcc.throughput", Value: 1000, Unit: "req/s", Direction: core.HigherIsBetter,
				Baseline: &core.BaselineComparison{Value: 950, DeltaPct: 5.3, Threshold: 10, Regressed: false}},
			{Name: "tpcc.latency_p99", Value: 70, Unit: "ms", Direction: core.LowerIsBetter,
				Baseline: &core.BaselineComparison{Value: 50, DeltaPct: 40, Threshold: 20, Regressed: true}},
		},
	}
	if err := s.ResultEvent(core.ResultEvent{Kind: core.EventTestFinished, TestName: res.TestName, Result: res}); err != nil {
		t.Fatalf("ResultEvent: %v", err)
	}
	if err := s.RunFinished(core.RunSummary{}); err != nil {
		t.Fatalf("RunFinished: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc tsuitesProps
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Suites) != 1 || len(doc.Suites[0].Cases) != 1 {
		t.Fatalf("want one suite/case, got %+v", doc)
	}
	props := doc.Suites[0].Cases[0].Properties.Property
	got := map[string]string{}
	for _, p := range props {
		got[p.Name] = p.Value
	}
	tp, ok := got["perf.tpcc.throughput"]
	if !ok || !contains(tp, "1000 req/s") || !contains(tp, "baseline 950") {
		t.Errorf("perf.tpcc.throughput = %q, want it to carry value + baseline", tp)
	}
	p99, ok := got["perf.tpcc.latency_p99"]
	if !ok || !contains(p99, "REGRESSED") {
		t.Errorf("perf.tpcc.latency_p99 = %q, want a REGRESSED marker", p99)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
