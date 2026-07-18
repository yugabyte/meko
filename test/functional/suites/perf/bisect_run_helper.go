package perf

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// The bisect ORCHESTRATION: resolve the SAME ordered perf.compare.variants,
// benchmark the reference (index 0) once, then binary-search — provisioning +
// benchmarking + tearing down each PROBED variant on demand — for the first
// regressed one. It is thin glue around runOneVariant (the same per-variant
// benchmark primitive the N-way comparison uses, with guaranteed LIFO teardown
// per probe) and bisectFirstBad (the pure search), so it is unit-testable with a
// fake provisioner + fake workload — no infra.

// probedCandidate pairs a probed variant's benchmark result with the metric
// value the search read and the good/bad verdict it produced.
type probedCandidate struct {
	Index  int
	Result variantResult
	Metric float64
	Bad    bool
}

// bisectOutcome is the full bisect result: the ordered candidate list (for the
// report), every probed candidate in probe order, and the pure search result
// (first-bad index + good/bad boundary).
type bisectOutcome struct {
	Candidates []resolvedVariant
	Probed     []probedCandidate
	Result     bisectResult
	Metric     comparisonMetric
}

// comparisonMetricByName resolves a metric name (perf.compare.bisect.metric) to
// its extractor + unit + direction, erroring on an unknown metric so a typo
// fails fast before any provisioning.
func comparisonMetricByName(name string) (comparisonMetric, error) {
	for _, m := range comparisonMetrics() {
		if m.name == name {
			return m, nil
		}
	}
	var names []string
	for _, m := range comparisonMetrics() {
		names = append(names, m.name)
	}
	return comparisonMetric{}, fmt.Errorf("perf.compare.bisect.metric %q is not a known metric (one of: %s)", name, strings.Join(names, ", "))
}

// runBisect benchmarks the reference (index 0) once, then binary-searches the
// ordered candidates for the first regressed one, provisioning + benchmarking +
// tearing down each PROBED candidate via runOneVariant (guaranteed LIFO teardown
// per probe, before the next). Any provision/benchmark failure aborts and
// returns the partial outcome plus the error — a bisect needs every probe it
// visits. It runs only ~log2(N)+1 benchmarks, not all N.
func runBisect(ctx context.Context, cands []resolvedVariant, prov variantProvisioner, mk workloadFactory, bc BenchConfig, cm comparisonMetric, thresholdPct float64, sink apps.ArtifactSink, logf func(string, ...any)) (bisectOutcome, error) {
	out := bisectOutcome{Candidates: cands, Metric: cm}
	results := map[int]variantResult{}

	refRes, err := runOneVariant(ctx, cands[0], prov, mk, bc, sink, logf)
	results[0] = refRes
	if err != nil {
		return out, fmt.Errorf("reference candidate %q: %w", cands[0].Label, err)
	}
	good := cm.stats(refRes.Result).Median
	if good == 0 {
		return out, fmt.Errorf("reference candidate %q produced no %s to bisect against", cands[0].Label, cm.name)
	}

	benchmark := func(i int) (float64, error) {
		res, err := runOneVariant(ctx, cands[i], prov, mk, bc, sink, logf)
		results[i] = res
		if err != nil {
			return 0, fmt.Errorf("candidate %q: %w", cands[i].Label, err)
		}
		return cm.stats(res.Result).Median, nil
	}

	br, err := bisectFirstBad(len(cands), good, thresholdPct, cm.dir, benchmark)
	out.Result = br
	for _, p := range br.Probes {
		out.Probed = append(out.Probed, probedCandidate{Index: p.Index, Result: results[p.Index], Metric: p.Metric, Bad: p.Bad})
	}
	if err != nil {
		return out, err
	}
	return out, nil
}

// bisectSpec is everything runBisectSuite needs to drive a workload's bisect:
// the shared perf/compare/bench config, the shared load identity
// (loadSig/commit), and the workload-specific glue (client-pod name prefix, the
// driver-delivery step, and the workload factory) — the SAME glue comparison
// mode uses.
type bisectSpec struct {
	perf       perfConfig
	cmp        compareConfig
	bc         BenchConfig
	loadSig    string
	commit     string
	podPrefix  string
	deliver    func(ctx context.Context, podExec exec.RemoteExecutor) error
	mkWorkload workloadFactory
	sink       apps.ArtifactSink
	logf       func(string, ...any)
}

// runBisectSuite is the shared bisect-mode body for perf/tpcc and perf/pgbench:
// resolve the ordered variants (erroring on <2 before any provisioning), resolve
// the bisect metric, run the search (a live provisioner around the same
// backing/client glue), then emit the ordered list + probed candidates + the
// localized first-regressed candidate + chart. The test PASSES (a bisect is
// diagnostic) unless a probe fails to provision/benchmark.
func runBisectSuite(t *core.T, spec bisectSpec) error {
	cm, err := comparisonMetricByName(spec.cmp.BisectMetric)
	if err != nil {
		return err
	}
	cands, err := resolveVariantList(t, spec.perf, spec.cmp.Variants, spec.loadSig, spec.commit, "bisect")
	if err != nil {
		return err
	}
	prov := &liveProvisioner{
		t:         t,
		perf:      spec.perf,
		podPrefix: spec.podPrefix,
		deliver:   spec.deliver,
		logf:      spec.logf,
		sink:      spec.sink,
	}
	var outcome bisectOutcome
	if err := t.Step("bisect_probes", func(*core.StepHandle) error {
		o, e := runBisect(t.Context, cands, prov, spec.mkWorkload, spec.bc, cm, spec.cmp.BisectThresholdPct, spec.sink, spec.logf)
		outcome = o
		return e
	}); err != nil {
		return err
	}
	return t.Step("bisect", func(*core.StepHandle) error {
		return emitBisect(t, spec, outcome)
	})
}

// emitBisect lands the bisect on the report: a per-probe datapoint of the bisect
// metric, the ordered-candidate + probe table as an attachment, the probed-metric
// bar chart with the transition marked, and the shared load identity. It PASSES
// — a bisect is diagnostic — reporting the localized first-regressed candidate.
func emitBisect(t *core.T, spec bisectSpec, o bisectOutcome) error {
	for _, p := range o.Probed {
		if p.Metric == 0 {
			continue
		}
		t.Datapoint(core.Datapoint{
			Name:      fmt.Sprintf("%s [%s]", o.Metric.name, p.Result.Identity.Variant),
			Value:     p.Metric,
			Unit:      o.Metric.unit,
			Direction: o.Metric.dir,
		})
	}
	t.AttachBytes("bisect.txt", "log", []byte(formatBisect(o)))
	if c, ok := bisectChart(o); ok {
		t.Chart(c)
	}
	t.SetPerfIdentity(core.PerfIdentity{
		Variant:       fmt.Sprintf("bisect of %d candidates → first regressed %q", len(o.Candidates), firstBadLabel(o)),
		Commit:        spec.commit,
		LoadSignature: spec.loadSig,
	})
	spec.logf("perf: bisect complete — %d candidates, %d probed, first regressed %q (see bisect.txt)",
		len(o.Candidates), len(o.Probed), firstBadLabel(o))
	return nil
}

// firstBadLabel is the label of the localized first-regressed candidate.
func firstBadLabel(o bisectOutcome) string {
	if o.Result.FirstBad < 0 || o.Result.FirstBad >= len(o.Candidates) {
		return ""
	}
	return o.Candidates[o.Result.FirstBad].Label
}

// bisectChart renders the probed candidates' metric as a bar (one series per
// probed candidate, ordered by index), the first-regressed candidate's bar
// marked in its name. Skipped when nothing was probed. Pure — unit-tested
// without a live run.
func bisectChart(o bisectOutcome) (core.Chart, bool) {
	probed := append([]probedCandidate(nil), o.Probed...)
	sort.Slice(probed, func(i, j int) bool { return probed[i].Index < probed[j].Index })
	var series []core.ChartSeries
	for _, p := range probed {
		name := o.Candidates[p.Index].Label
		if p.Index == o.Result.FirstBad {
			name += " ◀ first regressed"
		}
		series = append(series, core.ChartSeries{
			Name:   name,
			Points: []core.ChartPoint{{X: float64(p.Index), Y: p.Metric}},
		})
	}
	if len(series) == 0 {
		return core.Chart{}, false
	}
	return core.Chart{
		Title:  fmt.Sprintf("Bisect: probed %s by candidate", o.Metric.name),
		Kind:   core.ChartBar,
		XLabel: "candidate (ordered)",
		YLabel: o.Metric.name,
		Series: series,
	}, true
}

// formatBisect renders the bisect as a plain-text report: the ordered candidate
// list (marking probed / reference / first-regressed), the probes in evaluation
// order with metric + verdict, and the localized good/bad boundary. Pure and
// deterministic.
func formatBisect(o bisectOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "perf bisect — metric %s (%s, %s), %d candidates, %d probed\n",
		o.Metric.name, o.Metric.unit, o.Metric.dir, len(o.Candidates), len(o.Probed))
	fmt.Fprintf(&b, "first regressed candidate: %s (index %d); last good: index %d\n\n",
		firstBadLabel(o), o.Result.FirstBad, o.Result.GoodIndex)

	metricByIdx := map[int]float64{}
	verdictByIdx := map[int]string{}
	for _, p := range o.Probed {
		metricByIdx[p.Index] = p.Metric
		if p.Index == 0 {
			verdictByIdx[p.Index] = "reference (good)"
		} else if p.Bad {
			verdictByIdx[p.Index] = "BAD"
		} else {
			verdictByIdx[p.Index] = "good"
		}
	}

	b.WriteString("candidates (ordered oldest→newest):\n")
	for i, c := range o.Candidates {
		marker := "  "
		if i == o.Result.FirstBad {
			marker = "→ "
		}
		if v, ok := verdictByIdx[i]; ok {
			fmt.Fprintf(&b, "%s[%d] %-24s probed: %10.2f  %s\n", marker, i, c.Label, metricByIdx[i], v)
		} else {
			fmt.Fprintf(&b, "%s[%d] %-24s (not probed)\n", marker, i, c.Label)
		}
	}
	b.WriteString("\nprobes (evaluation order):\n")
	for _, p := range o.Probed {
		verdict := "good"
		if p.Index == 0 {
			verdict = "reference"
		} else if p.Bad {
			verdict = "BAD"
		}
		fmt.Fprintf(&b, "  [%d] %-24s %10.2f  %s\n", p.Index, o.Candidates[p.Index].Label, p.Metric, verdict)
	}
	return b.String()
}
