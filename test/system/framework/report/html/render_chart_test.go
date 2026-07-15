package html_test

import (
	"strings"
	"testing"

	"github.com/yugabyte/yb-amp/test/system/framework/core"
	rhtml "github.com/yugabyte/yb-amp/test/system/framework/report/html"
)

// TestHTMLRendersChartsSelfContained: a result's charts render in a "Charts"
// section as inline SVG (a line as a <polyline>, a bar as a <rect>), carrying a
// datapoint the caller emitted, and the whole document stays self-contained — no
// external asset references (no http, no src=, no <script src).
func TestHTMLRendersChartsSelfContained(t *testing.T) {
	res := &core.Result{
		TestName: "perf/pgbench",
		Suite:    "perf",
		Status:   core.StatusPassed,
		Charts: []core.Chart{
			{
				Title: "TPS over time", Kind: core.ChartLine, XLabel: "elapsed (s)", YLabel: "tps",
				Series: []core.ChartSeries{{Name: "iter 1", Points: []core.ChartPoint{{X: 10, Y: 452}, {X: 20, Y: 501}}}},
			},
			{
				Title: "Per-iteration TPS", Kind: core.ChartBar, XLabel: "iteration", YLabel: "tps",
				Series: []core.ChartSeries{{Name: "tps", Points: []core.ChartPoint{{X: 1, Y: 452}, {X: 2, Y: 501}}}},
			},
		},
	}
	out, err := rhtml.Render(rhtml.RunData{Results: []*core.Result{res}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, want := range []string{
		"Charts",        // the section heading
		"TPS over time", // chart title
		"Per-iteration TPS",
		"<svg",      // inline SVG
		"<polyline", // the line chart's series
		"<rect",     // the bar chart's bars
		"452",       // a series data value made it into the SVG
		"iter 1",    // legend series name
	} {
		if !strings.Contains(out, want) {
			t.Errorf("html missing %q", want)
		}
	}

	// CSP-safe / self-contained: no external asset references anywhere in the doc.
	for _, forbidden := range []string{"http", "src=", "<script src", "xmlns"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("report is not self-contained: contains %q", forbidden)
		}
	}
}

// TestHTMLChartEmptySeriesPlaceholder: a chart with no drawable points renders a
// placeholder rather than a broken SVG, and still produces a well-formed <svg>.
func TestHTMLChartEmptySeriesPlaceholder(t *testing.T) {
	res := &core.Result{
		TestName: "perf/empty",
		Suite:    "perf",
		Status:   core.StatusPassed,
		Charts:   []core.Chart{{Title: "Empty", Kind: core.ChartLine}},
	}
	out, err := rhtml.Render(rhtml.RunData{Results: []*core.Result{res}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "<svg") || !strings.Contains(out, "no data") {
		t.Errorf("empty chart did not render the no-data placeholder svg")
	}
}

// TestHTMLChartTicksTableAndLinks: charts render intermediate axis gridlines and
// a data table of exact values (a graph can't show precise numbers), and
// attachments with a persisted Path render as direct <a href> links.
func TestHTMLChartTicksTableAndLinks(t *testing.T) {
	res := &core.Result{
		TestName: "perf/pgbench", Suite: "perf", Status: core.StatusPassed,
		Charts: []core.Chart{{
			Title: "TPS over time", Kind: core.ChartLine, XLabel: "elapsed (s)", YLabel: "tps",
			Series: []core.ChartSeries{
				{Name: "iter 1", Points: []core.ChartPoint{{X: 5, Y: 100}, {X: 10, Y: 200}, {X: 15, Y: 300}}},
				{Name: "iter 2", Points: []core.ChartPoint{{X: 5, Y: 110}, {X: 10, Y: 210}, {X: 15, Y: 310}}},
			},
		}},
		Attachments: []core.Attachment{
			{Name: "pgbench-iter-0.out", Kind: "log", Size: 1234, Path: "artifacts/perf_pgbench/pgbench-iter-0.out"},
		},
	}
	out, err := rhtml.Render(rhtml.RunData{Results: []*core.Result{res}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// >2 <line>s → intermediate gridlines beyond the two axis lines.
	if n := strings.Count(out, "<line "); n < 4 {
		t.Errorf("expected intermediate gridlines (>2 <line>s), got %d", n)
	}
	// Data table with series columns + exact values a graph can't show precisely.
	for _, want := range []string{`class="chart-data"`, "iter 1", "iter 2", "300", "310"} {
		if !strings.Contains(out, want) {
			t.Errorf("chart data table missing %q", want)
		}
	}
	// Attachment with a Path becomes a direct link.
	if !strings.Contains(out, `<a href="artifacts/perf_pgbench/pgbench-iter-0.out">pgbench-iter-0.out</a>`) {
		t.Errorf("attachment with Path should render a direct <a href> link")
	}
	for _, forbidden := range []string{"http", "src=", "<script src", "xmlns"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("report not self-contained: contains %q", forbidden)
		}
	}
}
