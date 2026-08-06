package perf

import (
	"fmt"
	"math"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// A/B (N-way) variant comparison is the MEANINGFUL perf comparison: it runs the
// SAME workload at the SAME load across labelled variants that differ only in a
// thing-under-test (YB version/image, amp image, or commit), then reports each
// variant's median + noise and the pairwise delta vs the reference (the first
// variant). Unlike the single-run baseline gate — which can only compare against
// whatever stale value a local cache holds — this is comparing like-with-like,
// so a delta is a real signal rather than a config artifact. This file is the
// PURE core: config parse, the stats, the charts, and the table. The
// orchestration (provision each variant, run once, teardown) lives in
// compare_run.go.

// Variant is one labelled arm of a comparison: a label plus the config-key
// overrides that make it differ from the others (e.g.
// env.ampstack.controller_image, apps.ybk8s.image_tag). Every other knob —
// especially the whole load (perf.bench.*, perf.warehouses, perf.pgbench.*) — is
// held constant, which is what makes the arms comparable.
type Variant struct {
	Label     string
	Overrides map[string]string
}

// compareConfig is the resolved perf.compare.* configuration. Mode picks the
// strategy over the SAME ordered Variants list: "all" (default) benchmarks every
// variant and reports the N-way comparison; "bisect" binary-searches them for
// the first regressed variant. BisectMetric/BisectThresholdPct parameterize the
// bisect (ignored by "all").
type compareConfig struct {
	Variants           []Variant
	FailOnRegression   bool
	Mode               string
	BisectMetric       string
	BisectThresholdPct float64
}

// Shorthand-variant translation targets. yb_version sets versions.yb — the
// run-level YB version ybk8s.ChartVersion resolves (and the chart image tag falls
// back to), which is the key the run pipeline itself uses for the primary arm, so
// it provably changes the backing a variant deploys. image_version sets the amp
// image refs the same way the pipeline does: quay.io/yugabyte/amp-<component>:<tag>.
const (
	variantYBVersionKey       = "versions.yb"
	variantControllerImageKey = "env.ampstack.controller_image"
	variantEndpointImageKey   = "env.ampstack.endpoint_image"
	ampImageRepo              = "quay.io/yugabyte/amp-"

	// primaryVariantLabel is the injected reference arm for an all-shorthand
	// variant list: the run's OWN configuration, no overrides applied.
	primaryVariantLabel = "primary"
)

// compareConfigFromConfig reads perf.compare.* into a compareConfig. Variants is
// a structured list (GetMapSlice) supplied via a -c YAML file, a -jc JSON file,
// or a json_text override, in one of two shapes — never mixed in one list:
//
//	{label, overrides}                  full form: an explicit label plus the
//	                                    dotted config keys that make this arm
//	                                    differ. Used verbatim; the list is the
//	                                    complete set of arms (>=2 required).
//	{image_version, yb_version}         shorthand (what the report portal sends):
//	                                    versions translated to config keys, the
//	                                    version strings auto-labelled, and the
//	                                    run's own configuration prepended as the
//	                                    "primary" reference arm.
func compareConfigFromConfig(c *core.Config) (compareConfig, error) {
	vs, err := variantsFromMaps(c.GetMapSlice("perf.compare.variants", nil))
	if err != nil {
		return compareConfig{}, err
	}
	return compareConfig{
		Variants:           vs,
		FailOnRegression:   c.GetBool("perf.compare.fail_on_regression", false),
		Mode:               c.GetString("perf.compare.mode", CompareModeAll),
		BisectMetric:       c.GetString("perf.compare.bisect.metric", DefaultBisectMetric),
		BisectThresholdPct: c.GetFloat("perf.compare.bisect.threshold_pct", DefaultBisectThresholdPct),
	}, nil
}

// variantsFromMaps turns the raw perf.compare.variants elements into Variants.
// An all-shorthand list is translated, auto-labelled, and gets the "primary"
// reference arm prepended (the run's own config is the thing the extra versions
// are being compared against). An all-full-form list passes through untouched.
// Mixing the two shapes in one list is ambiguous and rejected.
func variantsFromMaps(ms []map[string]any) ([]Variant, error) {
	var vs []Variant
	shorthand := 0
	for i, m := range ms {
		if isShorthandVariant(m) {
			v, err := shorthandVariant(m)
			if err != nil {
				return nil, fmt.Errorf("perf.compare.variants[%d]: %w", i, err)
			}
			vs = append(vs, v)
			shorthand++
			continue
		}
		label, _ := m["label"].(string)
		v := Variant{Label: strings.TrimSpace(label), Overrides: map[string]string{}}
		if ov, ok := m["overrides"].(map[string]any); ok {
			for k, val := range ov {
				v.Overrides[k] = fmt.Sprint(val)
			}
		}
		vs = append(vs, v)
	}
	if shorthand == 0 {
		return vs, nil
	}
	if shorthand != len(vs) {
		return nil, fmt.Errorf("perf.compare.variants mixes the {image_version, yb_version} shorthand with the {label, overrides} form (%d of %d shorthand) — use one shape for the whole list", shorthand, len(vs))
	}
	dedupVariantLabels(vs, primaryVariantLabel)
	return append([]Variant{{Label: primaryVariantLabel, Overrides: map[string]string{}}}, vs...), nil
}

// isShorthandVariant reports whether an element is in the portal's version
// shorthand: no label and no overrides, but a version key (either may be empty —
// shorthandVariant decides whether the pair is usable).
func isShorthandVariant(m map[string]any) bool {
	if _, ok := m["label"]; ok {
		return false
	}
	if _, ok := m["overrides"]; ok {
		return false
	}
	_, hasImage := m["image_version"]
	_, hasYB := m["yb_version"]
	return hasImage || hasYB
}

// shorthandVariant translates one shorthand element into a labelled Variant.
//
// An image_version WITHOUT a yb_version is refused: every comparison workload
// (perf/tpcc, perf/pgbench, perf/sysbench) benchmarks the raw YugabyteDB backing
// the variant provisioner deploys — no amp control plane is in the loop — so an
// amp image alone would produce an arm identical to the reference and report its
// own noise as a delta.
func shorthandVariant(m map[string]any) (Variant, error) {
	image := mapString(m, "image_version")
	ybVersion := mapString(m, "yb_version")
	switch {
	case image == "" && ybVersion == "":
		return Variant{}, fmt.Errorf("shorthand variant sets neither image_version nor yb_version")
	case ybVersion == "":
		return Variant{}, fmt.Errorf("image_version %q without a yb_version: the perf comparison benchmarks the YugabyteDB backing directly (no amp control plane is provisioned), so an amp image alone yields an arm identical to the reference — add a yb_version, or use the {label, overrides} form", image)
	}
	v := Variant{
		Label:     shorthandLabel(image, ybVersion),
		Overrides: map[string]string{variantYBVersionKey: ybVersion},
	}
	if image != "" {
		v.Overrides[variantControllerImageKey] = ampImageRepo + "controller:" + image
		v.Overrides[variantEndpointImageKey] = ampImageRepo + "endpoint:" + image
	}
	return v, nil
}

// shorthandLabel names a shorthand arm by the versions it pins — readable in the
// comparison table and on every per-variant datapoint.
func shorthandLabel(image, ybVersion string) string {
	if image == "" {
		return ybVersion
	}
	return image + "+" + ybVersion
}

// dedupVariantLabels makes auto-generated labels unique in place (a numeric
// suffix per collision), keeping reserved labels — the injected primary arm —
// unclaimable.
func dedupVariantLabels(vs []Variant, reserved ...string) {
	seen := map[string]bool{}
	for _, r := range reserved {
		seen[r] = true
	}
	for i := range vs {
		label := vs[i].Label
		for n := 2; seen[label]; n++ {
			label = fmt.Sprintf("%s-%d", vs[i].Label, n)
		}
		seen[label] = true
		vs[i].Label = label
	}
}

// mapString reads a string field out of a raw config map (absent/nil → "").
func mapString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// VariantMetric is one variant's value for one metric in the comparison: its
// median + noise (CV), and — for every non-reference variant — the signed delta
// vs the reference and whether that delta is significant (beyond combined noise)
// and, if so, a regression (significant AND in the metric's worse direction).
type VariantMetric struct {
	Label       string
	Median      float64
	CV          float64
	DeltaPct    float64 // signed % vs reference (0 for the reference itself)
	Significant bool    // |DeltaPct| > (cvRef+cvThis)*100 — beyond combined noise
	Regressed   bool    // Significant AND worse in Direction (for fail_on_regression)
}

// MetricComparison is one metric compared across every variant.
type MetricComparison struct {
	Metric    string
	Unit      string
	Direction core.Direction
	Variants  []VariantMetric
}

// Comparison is the full N-way comparison: the reference label and, per metric,
// each variant's value + delta-vs-reference + significance.
type Comparison struct {
	Reference string
	Metrics   []MetricComparison
}

// comparisonMetric names one metric and how to pull its aggregate out of a
// BenchResult, so the comparison walks a fixed metric set uniformly.
type comparisonMetric struct {
	name  string
	unit  string
	dir   core.Direction
	stats func(BenchResult) BenchStats
}

// comparisonMetrics is the metric set a comparison reports. A metric the
// workload does not produce (pgbench emits no percentiles) has a zero reference
// median and is skipped, so the table only shows metrics that exist.
func comparisonMetrics() []comparisonMetric {
	return []comparisonMetric{
		{"tps", "tps", core.HigherIsBetter, func(r BenchResult) BenchStats { return r.TPS }},
		{"latency_avg", "ms", core.LowerIsBetter, func(r BenchResult) BenchStats { return r.LatencyAvg }},
		{"latency_p95", "ms", core.LowerIsBetter, func(r BenchResult) BenchStats { return r.LatencyP95 }},
		{"latency_p99", "ms", core.LowerIsBetter, func(r BenchResult) BenchStats { return r.LatencyP99 }},
	}
}

// compareVariants computes the comparison: the FIRST variant is the reference;
// every metric's delta and significance are measured against it. A metric with a
// zero reference median (not produced by this workload) is skipped. Pure and
// unit-tested. The significance rule is delta beyond COMBINED noise:
// |delta%| > (cvRef + cvThis) * 100 — a delta smaller than the two runs' pooled
// coefficient of variation is "within noise", not a real difference.
func compareVariants(results []variantResult) Comparison {
	var cmp Comparison
	if len(results) == 0 {
		return cmp
	}
	cmp.Reference = results[0].Identity.Variant
	ref := results[0].Result
	for _, m := range comparisonMetrics() {
		refStats := m.stats(ref)
		if refStats.Median == 0 {
			continue // metric not produced by this workload
		}
		mc := MetricComparison{Metric: m.name, Unit: m.unit, Direction: m.dir}
		for i, r := range results {
			st := m.stats(r.Result)
			vm := VariantMetric{Label: r.Identity.Variant, Median: st.Median, CV: st.CV}
			if i > 0 && refStats.Median != 0 {
				vm.DeltaPct = (st.Median - refStats.Median) / refStats.Median * 100
				noisePct := (refStats.CV + st.CV) * 100
				vm.Significant = math.Abs(vm.DeltaPct) > noisePct
				vm.Regressed = vm.Significant && worseDirection(m.dir, vm.DeltaPct)
			}
			mc.Variants = append(mc.Variants, vm)
		}
		cmp.Metrics = append(cmp.Metrics, mc)
	}
	return cmp
}

// worseDirection reports whether a signed delta moved the metric the wrong way:
// a lower-is-better metric rising, a higher-is-better metric dropping.
func worseDirection(dir core.Direction, delta float64) bool {
	if dir == core.LowerIsBetter {
		return delta > 0
	}
	return delta < 0
}

// regressedVariants summarizes every significant regression (for the
// fail_on_regression failure message), or "" when none regressed.
func regressedVariants(cmp Comparison) string {
	var parts []string
	for _, m := range cmp.Metrics {
		for _, v := range m.Variants {
			if v.Regressed {
				parts = append(parts, fmt.Sprintf("%s %s %+.1f%% vs %s (beyond noise)", v.Label, m.Metric, v.DeltaPct, cmp.Reference))
			}
		}
	}
	return strings.Join(parts, "; ")
}

// comparisonCharts turns the variant results into the comparison graphs: an
// overlaid TPS-over-time line (ONE SERIES PER VARIANT, from each variant's first
// kept iteration's progress series — skipped for a workload that reports no
// periodic progress) and a per-variant median-TPS bar (one series per variant,
// rendered side by side in a single group). Pure — unit-tested without a live
// run.
func comparisonCharts(results []variantResult) []core.Chart {
	var charts []core.Chart

	var overlay []core.ChartSeries
	for _, r := range results {
		var pts []core.ChartPoint
		for _, s := range r.Result.Kept {
			for _, p := range s.Series {
				pts = append(pts, core.ChartPoint{X: p.ElapsedSec, Y: p.TPS})
			}
			break // first kept iteration is this variant's representative line
		}
		if len(pts) > 0 {
			overlay = append(overlay, core.ChartSeries{Name: r.Identity.Variant, Points: pts})
		}
	}
	if len(overlay) > 0 {
		charts = append(charts, core.Chart{
			Title: "TPS over time by variant", Kind: core.ChartLine,
			XLabel: "elapsed (s)", YLabel: "tps", Series: overlay,
		})
	}

	var bars []core.ChartSeries
	for _, r := range results {
		bars = append(bars, core.ChartSeries{
			Name:   r.Identity.Variant,
			Points: []core.ChartPoint{{X: 0, Y: r.Result.TPS.Median}},
		})
	}
	if len(bars) > 0 {
		charts = append(charts, core.Chart{
			Title: "Median TPS by variant", Kind: core.ChartBar,
			XLabel: "variant", YLabel: "median tps", Series: bars,
		})
	}
	return charts
}

// formatComparison renders the comparison as a plain-text table (variant ×
// metric × median × CV × delta-vs-ref × significance), attached to the report as
// evidence. Pure and deterministic (metric + variant order preserved).
func formatComparison(cmp Comparison) string {
	var b strings.Builder
	fmt.Fprintf(&b, "perf comparison — reference variant: %s\n", cmp.Reference)
	fmt.Fprintf(&b, "significance rule: |delta%%| > (cv_ref + cv_variant) * 100 (delta beyond combined noise)\n\n")
	for _, m := range cmp.Metrics {
		fmt.Fprintf(&b, "[%s] (%s, %s)\n", m.Metric, m.Unit, m.Direction)
		fmt.Fprintf(&b, "  %-24s %14s %8s %12s   %s\n", "variant", "median", "cv%", "delta-vs-ref", "verdict")
		for i, v := range m.Variants {
			verdict := "reference"
			if i > 0 {
				switch {
				case v.Regressed:
					verdict = "REGRESSED (significant)"
				case v.Significant:
					verdict = "significant (improved)"
				default:
					verdict = "within noise"
				}
			}
			delta := "—"
			if i > 0 {
				delta = fmt.Sprintf("%+.1f%%", v.DeltaPct)
			}
			fmt.Fprintf(&b, "  %-24s %14.2f %7.1f%% %12s   %s\n", v.Label, v.Median, v.CV*100, delta, verdict)
		}
		b.WriteString("\n")
	}
	return b.String()
}
