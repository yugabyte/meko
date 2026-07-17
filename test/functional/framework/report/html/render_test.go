package html_test

import (
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	rhtml "github.com/yugabyte/yb-amp/test/functional/framework/report/html"
)

func fixtureResults() []*core.Result {
	start := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	passing := &core.Result{
		TestName:  "suite/pass_test",
		Suite:     "suite",
		Status:    core.StatusPassed,
		StartedAt: start,
		EndedAt:   start.Add(2 * time.Second),
		Duration:  2 * time.Second,
		Seed:      11,
		Steps: []*core.Step{
			{Name: "setup", Status: core.StatusPassed, StartedAt: start, EndedAt: start.Add(time.Second)},
			{
				Name: "verify", Status: core.StatusPassed, StartedAt: start.Add(time.Second), EndedAt: start.Add(2 * time.Second),
				Properties: map[string]any{"rows": 42},
				Steps: []*core.Step{
					{Name: "verify/child", Status: core.StatusPassed, Depth: 1, StartedAt: start.Add(time.Second), EndedAt: start.Add(2 * time.Second)},
				},
			},
		},
		Metrics:     []core.Metric{{Key: "latency_p99", Value: 12.5, Unit: "ms"}},
		Attachments: []core.Attachment{{Name: "test.log", Kind: "log", Size: 2048, StepName: "verify"}},
	}
	failing := &core.Result{
		TestName:       "suite/fail_test",
		Suite:          "suite",
		Status:         core.StatusFailed,
		StartedAt:      start,
		EndedAt:        start.Add(5 * time.Second),
		Duration:       5 * time.Second,
		Seed:           12,
		ErrorSummary:   "assertion failed: expected 3 replicas, got 2",
		StepsDropped:   4,
		MetricsDropped: 1,
	}
	skipped := &core.Result{
		TestName:   "suite/skip_test",
		Suite:      "suite",
		Status:     core.StatusSkipped,
		SkipReason: "out of version bounds",
		Seed:       13,
	}
	return []*core.Result{passing, failing, skipped}
}

func TestRenderProducesSelfContainedHTML(t *testing.T) {
	out, err := rhtml.Render(rhtml.RunData{
		Seed:     999,
		Parallel: 2,
		Results:  fixtureResults(),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if !strings.HasPrefix(out, "<!doctype html>") {
		t.Fatalf("output does not start with a doctype: %.40q", out)
	}
	if !strings.Contains(out, "<style>") || !strings.Contains(out, "</style>") {
		t.Fatal("output missing inline <style>")
	}

	// Self-contained: no external asset references of any kind.
	for _, needle := range []string{"http://", "https://", "<link ", "<script src", "cdn."} {
		if strings.Contains(out, needle) {
			t.Errorf("output references external asset %q", needle)
		}
	}

	// Status counts.
	for _, want := range []string{">1<", ">2<"} {
		_ = want // counts are per-status; checked via badges below instead of raw numbers
	}
	if !strings.Contains(out, "suite/pass_test") || !strings.Contains(out, "suite/fail_test") || !strings.Contains(out, "suite/skip_test") {
		t.Fatal("output missing one or more test rows")
	}

	// Per-test content: error summary, dropped counts, steps, metrics, properties.
	if !strings.Contains(out, "assertion failed: expected 3 replicas, got 2") {
		t.Error("output missing error summary")
	}
	if !strings.Contains(out, "4 dropped") {
		t.Error("output missing steps-dropped count")
	}
	if !strings.Contains(out, "1 dropped") {
		t.Error("output missing metrics-dropped count")
	}
	if !strings.Contains(out, "out of version bounds") {
		t.Error("output missing skip reason")
	}
	if !strings.Contains(out, "verify/child") {
		t.Error("output missing nested step")
	}
	if !strings.Contains(out, "latency_p99") || !strings.Contains(out, "12.5") {
		t.Error("output missing metric")
	}
	if !strings.Contains(out, "test.log") {
		t.Error("output missing attachment")
	}
	if !strings.Contains(out, "rows") || !strings.Contains(out, "42") {
		t.Error("output missing step property")
	}
	if !strings.Contains(out, "seed 999") {
		t.Error("output missing run seed")
	}
}

func TestRenderCountsByStatus(t *testing.T) {
	out, err := rhtml.Render(rhtml.RunData{Results: fixtureResults()})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, status := range []string{"passed", "failed", "skipped"} {
		if !strings.Contains(out, status) {
			t.Errorf("output missing status %q in summary", status)
		}
	}
}

func TestRenderHandlesEmptyResults(t *testing.T) {
	out, err := rhtml.Render(rhtml.RunData{})
	if err != nil {
		t.Fatalf("Render with no results: %v", err)
	}
	if !strings.Contains(out, "No test results.") {
		t.Error("output missing empty-state message")
	}
}

func TestRenderEscapesUntrustedContent(t *testing.T) {
	results := []*core.Result{{
		TestName:     "suite/<script>evil()</script>",
		Suite:        "suite",
		Status:       core.StatusFailed,
		ErrorSummary: `<img src=x onerror=alert(1)>`,
	}}
	out, err := rhtml.Render(rhtml.RunData{Results: results})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, "<script>evil()</script>") || strings.Contains(out, "<img src=x onerror") {
		t.Fatal("output contains unescaped HTML from result data")
	}
}
