package perf

import (
	"context"
	"fmt"
	"strings"

	"github.com/yugabyte/yb-amp/test/system/framework/apps"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/ampstack"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/ybk8s"
	"github.com/yugabyte/yb-amp/test/system/framework/core"
	"github.com/yugabyte/yb-amp/test/system/framework/exec"
)

// The comparison ORCHESTRATION: for each variant, provision its own
// image/version-overridden backing, run the SAME workload at the SAME shared
// load, tear the variant down (guaranteed, LIFO, BEFORE the next variant), then
// compute the pure comparison and emit the report. It is deliberately thin glue
// around RunBenchmark (the real harness) and compareVariants (the pure stats),
// so it is unit-testable with a fake provisioner + fake workload — no infra.

// resolvedVariant is a variant with its overrides applied: a resolved ybk8s
// Config (its own image/version) and the self-describing Identity.
type resolvedVariant struct {
	Label    string
	YBK8s    ybk8s.Config
	Identity Identity
}

// variantResult pairs a variant's identity with its benchmark aggregate.
type variantResult struct {
	Identity Identity
	Result   BenchResult
}

// variantProvisioner stands up the backing + client pod for one variant and
// returns the executor to drive the workload over, the in-cluster YSQL target,
// and a teardown to run (its own budget, LIFO) before the next variant. It
// always returns a non-nil teardown — even on error — so a partially-provisioned
// variant is still reclaimed. Faked in tests.
type variantProvisioner interface {
	Provision(ctx context.Context, v resolvedVariant) (exec.RemoteExecutor, pg.ConnInfo, func(context.Context) error, error)
}

// workloadFactory builds the SAME workload bound to a variant's connection —
// the shared load held constant across variants.
type workloadFactory func(conn pg.ConnInfo) Workload

// runComparison provisions each variant, runs the SAME workload at the SAME
// shared bench config, tears the variant down (guaranteed, before the next),
// and returns each variant's identity + aggregate. Any provision/benchmark
// failure aborts and returns the partial results plus the error — a comparison
// needs every variant.
func runComparison(ctx context.Context, variants []resolvedVariant, prov variantProvisioner, mk workloadFactory, bc BenchConfig, sink apps.ArtifactSink, logf func(string, ...any)) ([]variantResult, error) {
	var out []variantResult
	for _, v := range variants {
		res, err := runOneVariant(ctx, v, prov, mk, bc, sink, logf)
		out = append(out, res)
		if err != nil {
			return out, fmt.Errorf("variant %q: %w", v.Label, err)
		}
	}
	return out, nil
}

// runOneVariant provisions one variant, runs the workload once through the
// harness, and tears the variant down on its own budget before returning
// (guaranteed LIFO per variant, regardless of outcome).
func runOneVariant(ctx context.Context, v resolvedVariant, prov variantProvisioner, mk workloadFactory, bc BenchConfig, sink apps.ArtifactSink, logf func(string, ...any)) (variantResult, error) {
	ex, conn, teardown, err := prov.Provision(ctx, v)
	if teardown != nil {
		defer func() {
			tctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
			defer cancel()
			if tdErr := teardown(tctx); tdErr != nil && logf != nil {
				logf("perf compare: teardown variant %q (best-effort): %v", v.Label, tdErr)
			}
		}()
	}
	if err != nil {
		return variantResult{Identity: v.Identity}, err
	}

	res, err := RunBenchmark(ctx, ex, mk(conn), bc)
	if sink != nil {
		attachBenchSamples(sink, res, "compare-"+sanitizeLabel(v.Label))
	}
	if err != nil {
		return variantResult{Identity: v.Identity, Result: res}, err
	}
	if len(res.Kept) == 0 || res.TPS.Median == 0 {
		return variantResult{Identity: v.Identity, Result: res}, fmt.Errorf("produced no tps (parse failed — see attached compare-%s-iter-*.out)", sanitizeLabel(v.Label))
	}
	return variantResult{Identity: v.Identity, Result: res}, nil
}

// resolveVariants applies each variant's overrides onto an ISOLATED derived
// config and resolves that variant's own ybk8s Config + amp images + identity
// from it (never mutating the shared config). It requires at least two variants
// (a comparison needs a reference and at least one arm) and unique, non-empty
// labels. The load signature is shared (held constant) across every variant.
func resolveVariants(t *core.T, base perfConfig, cc compareConfig, loadSig, commit string) ([]resolvedVariant, error) {
	return resolveVariantList(t, base, cc.Variants, loadSig, commit, "comparison")
}

// resolveVariantList is the shared resolver for both comparison and bisect: it
// applies each variant's overrides onto an ISOLATED derived config and resolves
// that variant's own ybk8s Config + amp images + identity from it (never
// mutating the shared config). It requires at least two variants (both a
// comparison and a bisect need a reference and at least one arm) and unique,
// non-empty labels. The load signature is shared (held constant) across every
// variant. `what` names the caller for the error messages.
func resolveVariantList(t *core.T, base perfConfig, variants []Variant, loadSig, commit, what string) ([]resolvedVariant, error) {
	if len(variants) < 2 {
		return nil, fmt.Errorf("perf.compare.variants needs at least 2 variants for a %s (got %d)", what, len(variants))
	}
	seen := map[string]bool{}
	out := make([]resolvedVariant, 0, len(variants))
	for i, v := range variants {
		if v.Label == "" {
			return nil, fmt.Errorf("perf.compare.variants[%d] has no label", i)
		}
		if seen[v.Label] {
			return nil, fmt.Errorf("perf.compare.variants: duplicate label %q", v.Label)
		}
		seen[v.Label] = true

		derived := t.Options.Derive(v.Overrides)
		ybCfg := clampBackingRF(ybk8s.FromConfig(derived), base.RF)
		amp := ampstack.FromConfig(derived)
		id := buildIdentity(v.Label, ybCfg, amp.ControllerImage, amp.EndpointImage, commit, loadSig)
		out = append(out, resolvedVariant{Label: v.Label, YBK8s: ybCfg, Identity: id})
	}
	return out, nil
}

// comparisonSpec is everything runComparisonSuite needs to drive a workload's
// comparison: the shared perf/compare/bench config, the shared load identity
// (loadSig/commit), and the workload-specific glue (client-pod name prefix, the
// driver-delivery step, and the workload factory).
type comparisonSpec struct {
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

// runComparisonSuite is the shared comparison-mode body for perf/tpcc and
// perf/pgbench: resolve the variants, run them (a live provisioner around the
// same backing/client glue), then emit the comparison table + charts. In
// comparison mode the test PASSES (it is a comparison, not a gate) unless a
// variant fails to provision/benchmark — or fail_on_regression is set and a
// variant regressed significantly vs the reference.
func runComparisonSuite(t *core.T, spec comparisonSpec) error {
	variants, err := resolveVariants(t, spec.perf, spec.cmp, spec.loadSig, spec.commit)
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
	var results []variantResult
	if err := t.Step("run_variants", func(*core.StepHandle) error {
		r, e := runComparison(t.Context, variants, prov, spec.mkWorkload, spec.bc, spec.sink, spec.logf)
		results = r
		return e
	}); err != nil {
		return err
	}
	return t.Step("compare", func(*core.StepHandle) error {
		return emitComparison(t, spec, results)
	})
}

// emitComparison computes the pure comparison and lands it on the report: a
// per-variant datapoint per metric (ungated), the comparison table as an
// attachment, and the per-variant comparison charts. It records the shared load
// identity on the result and, when fail_on_regression is set, fails on a
// significant regression vs the reference.
func emitComparison(t *core.T, spec comparisonSpec, results []variantResult) error {
	cmp := compareVariants(results)
	for _, r := range results {
		for _, dp := range variantDatapoints(r) {
			t.Datapoint(dp)
		}
	}
	t.AttachBytes("comparison.txt", "log", []byte(formatComparison(cmp)))
	for _, c := range comparisonCharts(results) {
		t.Chart(c)
	}
	t.SetPerfIdentity(core.PerfIdentity{
		Variant:       fmt.Sprintf("comparison of %d variants (reference %q)", len(results), cmp.Reference),
		Commit:        spec.commit,
		LoadSignature: spec.loadSig,
	})
	if spec.cmp.FailOnRegression {
		if s := regressedVariants(cmp); s != "" {
			return fmt.Errorf("perf regression vs reference %q: %s", cmp.Reference, s)
		}
	}
	spec.logf("perf: comparison complete — %d variants, reference %q (see comparison.txt)", len(results), cmp.Reference)
	return nil
}

// variantDatapoints emits one datapoint per produced metric for a variant,
// labelled with the variant so the Perf section shows each arm side by side.
// They are ungated (no baseline) — the comparison, not a stale baseline, is the
// signal. A metric the workload does not produce (zero median) is skipped.
func variantDatapoints(r variantResult) []core.Datapoint {
	var dps []core.Datapoint
	add := func(name string, st BenchStats, unit string, dir core.Direction) {
		if st.Median == 0 {
			return
		}
		dps = append(dps, core.Datapoint{
			Name: fmt.Sprintf("%s [%s]", name, r.Identity.Variant), Value: st.Median, Unit: unit, Direction: dir,
		})
	}
	add("tps", r.Result.TPS, "tps", core.HigherIsBetter)
	add("latency_avg", r.Result.LatencyAvg, "ms", core.LowerIsBetter)
	add("latency_p95", r.Result.LatencyP95, "ms", core.LowerIsBetter)
	add("latency_p99", r.Result.LatencyP99, "ms", core.LowerIsBetter)
	return dps
}

// liveProvisioner is the production variantProvisioner: it deploys the variant's
// own backing (its overridden ybk8s Config), launches the client pod, delivers
// the driver, and waits for YSQL — the same glue the single-run path uses, per
// variant, each on its own teardown stack.
type liveProvisioner struct {
	t         *core.T
	perf      perfConfig
	podPrefix string
	deliver   func(ctx context.Context, podExec exec.RemoteExecutor) error
	logf      func(string, ...any)
	sink      apps.ArtifactSink
}

func (p *liveProvisioner) Provision(ctx context.Context, v resolvedVariant) (exec.RemoteExecutor, pg.ConnInfo, func(context.Context) error, error) {
	td := &teardownStack{}
	teardown := func(tc context.Context) error { return td.run(tc, p.logf) }

	info, kube, err := deployYBBackingCfg(ctx, p.t, v.YBK8s, td, p.logf)
	if err != nil {
		return nil, pg.ConnInfo{}, teardown, err
	}
	podName := fmt.Sprintf("%s-%s-%s", p.podPrefix, sanitizeLabel(v.Label), p.t.ResourceSuffix())
	podExec, err := startPerfClientPod(ctx, kube, p.sink, td, podName, p.perf.ClientImage, p.perf.ClientPullSecret)
	if err != nil {
		return nil, pg.ConnInfo{}, teardown, err
	}
	if p.deliver != nil {
		if err := p.deliver(ctx, podExec); err != nil {
			return nil, pg.ConnInfo{}, teardown, err
		}
	}
	if err := waitBackingYSQL(ctx, kube, info, p.perf.RF, p.perf.ReadyTimeout); err != nil {
		return nil, pg.ConnInfo{}, teardown, err
	}
	host, port := ybk8s.FirstHostPort(info.InClusterHostPorts)
	conn := pg.ConnInfo{Host: host, Port: port, User: info.User, Database: info.User, SSLMode: "disable"}
	return podExec, conn, teardown, nil
}

// sanitizeLabel renders a variant label as a DNS-1123-safe, bounded token for a
// pod name / artifact prefix (lowercase, non-alnum → '-', trimmed, capped).
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "v"
	}
	if len(out) > 20 {
		out = strings.Trim(out[:20], "-")
	}
	return out
}
