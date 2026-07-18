// Package html renders a run's results into a single self-contained HTML
// report: inline CSS, a tiny inline expand/collapse-all script, no external
// assets (fonts, CDNs, images). It consumes the same core.Result shape the
// jsonfile sink persists, so a report can be built either live (the Sink) or
// after the fact from a completed run's output (the `ampqa report` command).
package html

import (
	"bytes"
	"fmt"
	"html/template"
	"sort"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// RunData is everything Render needs about one run. Results is the only
// required field; the rest are best-effort display metadata and fall back to
// values derived from Results when zero.
type RunData struct {
	Seed      int64
	Parallel  int
	StartedAt time.Time
	EndedAt   time.Time
	Results   []*core.Result
}

// Render builds the full HTML document for data as a string.
func Render(data RunData) (string, error) {
	view := buildView(data)
	var buf bytes.Buffer
	if err := reportTemplate.Execute(&buf, view); err != nil {
		return "", fmt.Errorf("render html report: %w", err)
	}
	return buf.String(), nil
}

type reportView struct {
	GeneratedAt string
	Seed        int64
	Parallel    int
	Total       int
	Duration    string
	Counts      []statusCountView
	Tests       []testView
}

type statusCountView struct {
	Status string
	Count  int
	Class  string
}

type testView struct {
	Name           string
	Suite          string
	Description    string
	Category       string
	Status         string
	StatusClass    string
	Duration       string
	Seed           int64
	Product        string
	YB             string
	ErrorSummary   string
	SkipReason     string
	StepsDropped   int
	MetricsDropped int
	Steps          []stepView
	Metrics        []metricView
	Perf           []perfView
	Charts         []chartView
	Attachments    []attachmentView
	Identity       []kv
}

type stepView struct {
	Name        string
	Status      string
	StatusClass string
	Duration    string
	Depth       int
	Properties  []kv
	Children    []stepView
}

type metricView struct {
	Key   string
	Value string
	Unit  string
}

type perfView struct {
	Name        string
	Value       string
	Unit        string
	Baseline    string // "" when ungated
	StatusClass string // st-pass / st-fail per regression
}

type attachmentView struct {
	Name     string
	Kind     string
	Size     string
	StepName string
	Path     string
}

type kv struct {
	Key   string
	Value string
}

func buildView(data RunData) reportView {
	counts := map[core.Status]int{}
	started, ended := data.StartedAt, data.EndedAt
	for _, r := range data.Results {
		if r == nil {
			continue
		}
		counts[r.Status]++
		if started.IsZero() || (!r.StartedAt.IsZero() && r.StartedAt.Before(started)) {
			started = r.StartedAt
		}
		if !r.EndedAt.IsZero() && r.EndedAt.After(ended) {
			ended = r.EndedAt
		}
	}

	v := reportView{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Seed:        data.Seed,
		Parallel:    data.Parallel,
		Total:       len(data.Results),
	}
	if !started.IsZero() && !ended.IsZero() && ended.After(started) {
		v.Duration = ended.Sub(started).Round(time.Millisecond).String()
	}
	for _, st := range core.AllStatuses {
		if n := counts[st]; n > 0 {
			v.Counts = append(v.Counts, statusCountView{Status: string(st), Count: n, Class: statusClass(st)})
		}
	}

	tests := make([]*core.Result, len(data.Results))
	copy(tests, data.Results)
	sort.SliceStable(tests, func(i, j int) bool {
		if tests[i] == nil || tests[j] == nil {
			return tests[j] == nil
		}
		return tests[i].TestName < tests[j].TestName
	})
	for _, r := range tests {
		if r == nil {
			continue
		}
		v.Tests = append(v.Tests, buildTestView(r))
	}
	return v
}

func buildTestView(r *core.Result) testView {
	tv := testView{
		Name:           r.TestName,
		Suite:          r.Suite,
		Description:    r.Description,
		Category:       r.Category.String(),
		Status:         string(r.Status),
		StatusClass:    statusClass(r.Status),
		Duration:       r.Duration.Round(time.Millisecond).String(),
		Seed:           r.Seed,
		Product:        r.Versions.Product,
		YB:             r.Versions.YB,
		ErrorSummary:   r.ErrorSummary,
		SkipReason:     r.SkipReason,
		StepsDropped:   r.StepsDropped,
		MetricsDropped: r.MetricsDropped,
	}
	for _, s := range r.Steps {
		tv.Steps = append(tv.Steps, buildStepView(s))
	}
	for _, m := range r.Metrics {
		tv.Metrics = append(tv.Metrics, metricView{
			Key:   m.Key,
			Value: fmt.Sprintf("%g", m.Value),
			Unit:  m.Unit,
		})
	}
	tv.Identity = buildIdentityView(r.PerfIdentity)
	for _, dp := range r.Perf {
		tv.Perf = append(tv.Perf, buildPerfView(dp))
	}
	for _, c := range r.Charts {
		tv.Charts = append(tv.Charts, buildChartView(c))
	}
	for _, a := range r.Attachments {
		tv.Attachments = append(tv.Attachments, attachmentView{
			Name:     a.Name,
			Kind:     a.Kind,
			Size:     humanSize(a.Size),
			StepName: a.StepName,
			Path:     a.Path,
		})
	}
	return tv
}

// buildIdentityView flattens a perf result's identity into an ordered key/value
// list for the "variant" block. Nil identity (non-perf test) yields no rows, so
// the block is skipped. Only non-empty fields are shown.
func buildIdentityView(id *core.PerfIdentity) []kv {
	if id == nil {
		return nil
	}
	var out []kv
	add := func(k, v string) {
		if v != "" {
			out = append(out, kv{Key: k, Value: v})
		}
	}
	add("Variant", id.Variant)
	add("YB version", id.YBVersion)
	add("YB image", id.YBImage)
	add("Amp controller", id.AmpControllerImage)
	add("Amp endpoint", id.AmpEndpointImage)
	add("Commit", id.Commit)
	add("Load signature", id.LoadSignature)
	return out
}

func buildPerfView(dp core.Datapoint) perfView {
	pv := perfView{
		Name:        dp.Name,
		Value:       fmt.Sprintf("%g", dp.Value),
		Unit:        dp.Unit,
		StatusClass: "st-pass",
	}
	if b := dp.Baseline; b != nil {
		pv.Baseline = fmt.Sprintf("baseline %g, %+.1f%% (tol %.1f%%)", b.Value, b.DeltaPct, b.Threshold)
		if b.Regressed {
			pv.Baseline += " — REGRESSED"
			pv.StatusClass = "st-fail"
		}
	} else {
		pv.Baseline = "no baseline"
		pv.StatusClass = "st-skip"
	}
	return pv
}

func buildStepView(s *core.Step) stepView {
	if s == nil {
		return stepView{}
	}
	sv := stepView{
		Name:        s.Name,
		Status:      string(s.Status),
		StatusClass: statusClass(s.Status),
		Duration:    s.Duration().Round(time.Millisecond).String(),
		Depth:       s.Depth,
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sv.Properties = append(sv.Properties, kv{Key: k, Value: fmt.Sprintf("%v", s.Properties[k])})
	}
	for _, c := range s.Steps {
		sv.Children = append(sv.Children, buildStepView(c))
	}
	return sv
}

func statusClass(st core.Status) string {
	switch st {
	case core.StatusPassed:
		return "st-pass"
	case core.StatusSkipped:
		return "st-skip"
	case core.StatusFailed, core.StatusBroken, core.StatusEnvFailed, core.StatusTimedOut:
		return "st-fail"
	default:
		return "st-unknown"
	}
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

var reportTemplate = template.Must(template.New("report").Parse(reportHTML))

const reportHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ampqa report{{if .Seed}} — seed {{.Seed}}{{end}}</title>
<style>
:root {
  --bg: #f5f6f8; --fg: #1a1d23; --card-bg: #ffffff; --border: #d8dbe1;
  --muted: #5b6270; --accent: #2f6fed;
  --green-bg: #e3f6e9; --green-fg: #146c2e;
  --red-bg: #fdeaea; --red-fg: #a4161a;
  --yellow-bg: #fff5da; --yellow-fg: #8a5a00;
  --grey-bg: #eceef1; --grey-fg: #444a55;
  --code-bg: #f0f1f4;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #14161a; --fg: #e7e9ee; --card-bg: #1d2026; --border: #2c3038;
    --muted: #9aa2b1; --accent: #6fa1ff;
    --green-bg: #123a22; --green-fg: #6fe19a;
    --red-bg: #3a1414; --red-fg: #ff8f8f;
    --yellow-bg: #3a2f0d; --yellow-fg: #ffd166;
    --grey-bg: #23262d; --grey-fg: #c3c8d1;
    --code-bg: #14161a;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0; padding: 2rem; background: var(--bg); color: var(--fg);
  font: 15px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
}
h1 { font-size: 1.4rem; margin: 0 0 .25rem; }
.meta { color: var(--muted); margin-bottom: 1.5rem; font-size: .9rem; }
.summary {
  display: flex; flex-wrap: wrap; gap: .75rem; margin-bottom: 1.5rem;
}
.stat {
  background: var(--card-bg); border: 1px solid var(--border); border-radius: 10px;
  padding: .6rem 1rem; min-width: 6rem;
}
.stat .n { font-size: 1.3rem; font-weight: 700; display: block; }
.stat .l { color: var(--muted); font-size: .8rem; text-transform: uppercase; letter-spacing: .03em; }
.badge {
  display: inline-block; padding: .15rem .55rem; border-radius: 999px;
  font-size: .78rem; font-weight: 600; text-transform: uppercase; letter-spacing: .02em;
}
.st-pass { background: var(--green-bg); color: var(--green-fg); }
.st-fail { background: var(--red-bg); color: var(--red-fg); }
.st-skip { background: var(--yellow-bg); color: var(--yellow-fg); }
.st-unknown { background: var(--grey-bg); color: var(--grey-fg); }
.toolbar { margin-bottom: 1rem; }
button.tglall {
  background: var(--card-bg); color: var(--fg); border: 1px solid var(--border);
  border-radius: 6px; padding: .4rem .8rem; font-size: .85rem; cursor: pointer; margin-right: .5rem;
}
button.tglall:hover { border-color: var(--accent); }
table.tests { width: 100%; border-collapse: collapse; margin-bottom: .5rem; }
table.tests th, table.tests td {
  text-align: left; padding: .5rem .6rem; border-bottom: 1px solid var(--border); vertical-align: top;
}
table.tests th { color: var(--muted); font-weight: 600; font-size: .8rem; text-transform: uppercase; }
.test-card {
  background: var(--card-bg); border: 1px solid var(--border); border-radius: 10px;
  margin-bottom: .6rem; overflow: hidden;
}
.test-card > summary {
  list-style: none; cursor: pointer; padding: .7rem 1rem; display: flex; align-items: center; gap: .75rem;
}
.test-card > summary::-webkit-details-marker { display: none; }
.test-card > summary .name { font-weight: 600; flex: 1; }
.test-card > summary .dur, .test-card > summary .suite { color: var(--muted); font-size: .85rem; }
.test-body { padding: 0 1rem 1rem; border-top: 1px solid var(--border); }
.desc { margin: .7rem 0 .2rem; font-size: .95rem; color: var(--fg); }
.err {
  background: var(--red-bg); color: var(--red-fg); border-radius: 6px; padding: .5rem .7rem;
  margin: .6rem 0; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: .85rem;
  white-space: pre-wrap; word-break: break-word;
}
.skip { background: var(--yellow-bg); color: var(--yellow-fg); border-radius: 6px; padding: .5rem .7rem; margin: .6rem 0; }
h4 { margin: .9rem 0 .35rem; font-size: .9rem; color: var(--muted); text-transform: uppercase; letter-spacing: .03em; }
.dropped { color: var(--muted); font-size: .8rem; margin-left: .4rem; }
.steps { list-style: none; margin: 0; padding: 0; }
.steps li { padding: .2rem 0 .2rem calc(1rem * var(--depth, 0)); }
.step-row { display: flex; align-items: center; gap: .5rem; }
.step-row .sname { flex: 1; }
.step-row .sdur { color: var(--muted); font-size: .8rem; }
.props { color: var(--muted); font-size: .8rem; margin: 0 0 0 1.3rem; }
table.kv { border-collapse: collapse; font-size: .85rem; }
table.kv td { padding: .15rem .6rem .15rem 0; border: none; }
table.kv td.k { color: var(--muted); }
code { background: var(--code-bg); border-radius: 4px; padding: .05rem .3rem; }
.empty { color: var(--muted); font-style: italic; }
.chart {
  background: var(--card-bg); border: 1px solid var(--border); border-radius: 8px;
  padding: .5rem .7rem; margin: .5rem 0; max-width: 720px;
}
.chart-title { font-size: .85rem; font-weight: 600; margin-bottom: .25rem; }
.chart-svg { width: 100%; height: auto; display: block; }
.chart-data { border-collapse: collapse; font-size: .8rem; margin-top: .5rem; width: 100%; overflow-x: auto; display: block; }
.chart-data th, .chart-data td { text-align: right; padding: .2rem .5rem; border-bottom: 1px solid var(--border); font-variant-numeric: tabular-nums; white-space: nowrap; }
.chart-data th:first-child, .chart-data td:first-child { text-align: left; color: var(--muted); }
.chart-data th { color: var(--muted); font-weight: 600; }
a { color: var(--accent); }
</style>
</head>
<body>
<h1>ampqa run report</h1>
<div class="meta">generated {{.GeneratedAt}}{{if .Seed}} &middot; seed {{.Seed}}{{end}}{{if .Parallel}} &middot; parallel {{.Parallel}}{{end}}{{if .Duration}} &middot; duration {{.Duration}}{{end}}</div>

<div class="summary">
  <div class="stat"><span class="n">{{.Total}}</span><span class="l">total</span></div>
  {{range .Counts}}<div class="stat"><span class="n badge {{.Class}}">{{.Count}}</span><span class="l">{{.Status}}</span></div>
  {{end}}
</div>

<div class="toolbar">
  <button type="button" class="tglall" data-open="true">Expand all</button>
  <button type="button" class="tglall" data-open="false">Collapse all</button>
</div>

{{if not .Tests}}<p class="empty">No test results.</p>{{end}}
{{range .Tests}}
<details class="test-card" open>
  <summary>
    <span class="badge {{.StatusClass}}">{{.Status}}</span>
    <span class="name">{{.Name}}</span>
    <span class="suite">{{.Suite}}{{if .Category}} / {{.Category}}{{end}}</span>
    <span class="dur">{{.Duration}}</span>
  </summary>
  <div class="test-body">
    {{if .Description}}<p class="desc">{{.Description}}</p>{{end}}
    <table class="kv">
      <tr><td class="k">Seed</td><td>{{.Seed}}</td></tr>
      {{if .Product}}<tr><td class="k">Product</td><td>{{.Product}}</td></tr>{{end}}
      {{if .YB}}<tr><td class="k">YB</td><td>{{.YB}}</td></tr>{{end}}
    </table>
    {{if .Identity}}
    <h4>Variant</h4>
    <table class="kv">
      {{range .Identity}}<tr><td class="k">{{.Key}}</td><td><code>{{.Value}}</code></td></tr>
      {{end}}
    </table>
    {{end}}
    {{if .ErrorSummary}}<div class="err">{{.ErrorSummary}}</div>{{end}}
    {{if .SkipReason}}<div class="skip">skip: {{.SkipReason}}</div>{{end}}

    <h4>Steps
      {{if .StepsDropped}}<span class="dropped">({{.StepsDropped}} dropped)</span>{{end}}
    </h4>
    {{if .Steps}}{{template "steps" .Steps}}{{else}}<p class="empty">no steps recorded</p>{{end}}

    <h4>Metrics
      {{if .MetricsDropped}}<span class="dropped">({{.MetricsDropped}} dropped)</span>{{end}}
    </h4>
    {{if .Metrics}}
    <table class="kv">
      {{range .Metrics}}<tr><td class="k">{{.Key}}</td><td>{{.Value}}{{if .Unit}} {{.Unit}}{{end}}</td></tr>
      {{end}}
    </table>
    {{else}}<p class="empty">no metrics recorded</p>{{end}}

    {{if .Perf}}
    <h4>Perf datapoints</h4>
    <table class="kv">
      {{range .Perf}}<tr><td class="k">{{.Name}}</td><td>{{.Value}}{{if .Unit}} {{.Unit}}{{end}} <span class="badge {{.StatusClass}}">{{.Baseline}}</span></td></tr>
      {{end}}
    </table>
    {{end}}

    {{if .Charts}}
    <h4>Charts</h4>
    {{range .Charts}}<div class="chart">{{if .Title}}<div class="chart-title">{{.Title}}</div>{{end}}{{.SVG}}{{.Table}}</div>
    {{end}}
    {{end}}

    {{if .Attachments}}
    <h4>Attachments</h4>
    <table class="kv">
      {{range .Attachments}}<tr><td class="k">{{if .Path}}<a href="{{.Path}}">{{.Name}}</a>{{else}}{{.Name}}{{end}}</td><td>{{.Kind}}, {{.Size}}{{if .StepName}} &middot; {{.StepName}}{{end}}</td></tr>
      {{end}}
    </table>
    {{end}}
  </div>
</details>
{{end}}

<script>
document.querySelectorAll("button.tglall").forEach(function (btn) {
  btn.addEventListener("click", function () {
    var open = btn.getAttribute("data-open") === "true";
    document.querySelectorAll("details.test-card").forEach(function (d) { d.open = open; });
  });
});
</script>
</body>
</html>
{{define "steps"}}
<ul class="steps">
  {{range .}}
  <li style="--depth: {{.Depth}}">
    <div class="step-row">
      <span class="badge {{.StatusClass}}">{{.Status}}</span>
      <span class="sname">{{.Name}}</span>
      <span class="sdur">{{.Duration}}</span>
    </div>
    {{if .Properties}}<table class="props kv">{{range .Properties}}<tr><td class="k">{{.Key}}</td><td>{{.Value}}</td></tr>{{end}}</table>{{end}}
    {{if .Children}}{{template "steps" .Children}}{{end}}
  </li>
  {{end}}
</ul>
{{end}}
`
