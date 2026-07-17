package core

// ChartKind names the shape a Chart renders as. A line plots one polyline per
// series (a time series); a bar plots one rect per point (a category); a
// histogram buckets every point's Y value into count bars.
type ChartKind string

const (
	ChartLine      ChartKind = "line"
	ChartBar       ChartKind = "bar"
	ChartHistogram ChartKind = "histogram"
)

// ChartPoint is one (x, y) sample in a series.
type ChartPoint struct {
	X float64
	Y float64
}

// ChartSeries is one named group of points — a line, a bar group, or the
// values fed to a histogram.
type ChartSeries struct {
	Name   string
	Points []ChartPoint
}

// Chart is a self-contained graph a test emits with T.Chart. Like a Datapoint
// it is deliberately small and typed (no free-form maps): the HTML sink renders
// it as inline SVG in the test's section, the jsonfile sink persists it so
// `ampqa report --from` re-renders it, and the other sinks ignore it. A test
// adds a graph to its report with a single T.Chart call — no bespoke per-test
// report code.
type Chart struct {
	Title  string
	Kind   ChartKind
	XLabel string
	YLabel string
	Series []ChartSeries
}

// Chart records a chart on the result and emits it. The caller builds the
// fully-formed Chart so every sink renders the same graph. It mirrors
// T.Datapoint exactly: append to the result, then emit a matching event for
// live sinks. Charts are a handful per test (not per-cycle), so — like Perf —
// they are kept unbounded.
func (t *T) Chart(c Chart) {
	t.mu.Lock()
	t.result.Charts = append(t.result.Charts, c)
	t.mu.Unlock()
	if t.emit != nil {
		t.emit.emit(ResultEvent{Kind: EventChart, Chart: &c})
	}
}
