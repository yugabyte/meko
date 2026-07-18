package perf

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/clientpod"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/baseline"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// The backing glue shared by every perf workload (perf/tpcc, perf/pgbench) and
// the pgbench standing-env preset: deploy the co-located YB, launch the client
// pod, wait for YSQL, and gate datapoints against the baseline. Kept as plain
// functions (not a shared base struct) so each workload owns its own run state.

// deployYBBacking deploys a co-located YugabyteDB cluster in k8s (RF forced up
// to rf), registers its teardown on td, and returns the deploy Info plus a
// namespace-scoped kubectl. The client pod lives in the same namespace, so the
// namespace delete at teardown cascades it.
func deployYBBacking(ctx context.Context, t *core.T, rf int, td *teardownStack, logf func(string, ...any)) (ybk8s.Info, *k8s.Kubectl, error) {
	return deployYBBackingCfg(ctx, t, clampBackingRF(ybk8s.FromConfig(t.Options), rf), td, logf)
}

// clampBackingRF forces k8sCfg's replication factor up to rf and re-couples the
// master/tserver replica counts to it (0 → clamp to RF at deploy). It is the
// shared RF-clamp both the single-run path and each comparison variant apply to
// a resolved ybk8s Config before deploy.
func clampBackingRF(k8sCfg ybk8s.Config, rf int) ybk8s.Config {
	if k8sCfg.ReplicationFactor < rf {
		k8sCfg.ReplicationFactor = rf
	}
	k8sCfg.MasterReplicas = 0
	k8sCfg.TServerReplicas = 0
	return k8sCfg
}

// deployYBBackingCfg deploys the co-located YB backing from an ALREADY-RESOLVED
// ybk8s Config (rather than reading t.Options), so a comparison variant can
// deploy its own image/version-overridden backing. It registers teardown on td
// and returns the deploy Info plus a namespace-scoped kubectl.
func deployYBBackingCfg(ctx context.Context, t *core.T, k8sCfg ybk8s.Config, td *teardownStack, logf func(string, ...any)) (ybk8s.Info, *k8s.Kubectl, error) {
	secrets, err := core.NewSecrets(t.Options)
	if err != nil {
		return ybk8s.Info{}, nil, fmt.Errorf("secrets: %w", err)
	}
	d := ybk8s.New(k8sCfg, t.ResourceSuffix()).WithLogf(logf).WithSecretResolver(secrets.Value)
	td.push("teardown yb-in-k8s (namespace + PVCs + client pod)", func(ctx context.Context) error {
		return d.Teardown(ctx)
	})

	info, err := d.Deploy(ctx)
	if err != nil {
		return ybk8s.Info{}, nil, fmt.Errorf("provision yb in k8s: %w", err)
	}
	kube := k8s.New(k8s.Config{Context: k8sCfg.KubeContext, Namespace: info.Namespace}).
		WithRunner(k8s.KubeconfigRunner(k8sCfg.Kubeconfig))
	return info, kube, nil
}

// startPerfClientPod launches the long-lived in-cluster client pod (sleep-
// forever) on kube's namespace, registers its collect-then-delete teardown on
// td, and returns a RemoteExecutor bound to it (over which a workload driver or
// an artifact deploy runs commands inside the pod).
func startPerfClientPod(ctx context.Context, kube *k8s.Kubectl, sink apps.ArtifactSink, td *teardownStack, name, image, pullSecret string) (exec.RemoteExecutor, error) {
	spec := clientpod.Spec{
		Name:    name,
		Image:   image,
		Command: []string{"sh", "-c", "sleep infinity"},
	}
	if pullSecret != "" {
		spec.ImagePullSecrets = []string{pullSecret}
	}
	cp := clientpod.NewK8s(kube)
	if err := cp.Start(ctx, spec); err != nil {
		return nil, fmt.Errorf("start client pod: %w", err)
	}
	td.push("report-kill client pod (collect logs + delete)", func(ctx context.Context) error {
		return cp.Stop(ctx, sink)
	})
	return cp.Exec(), nil
}

// waitBackingYSQL blocks until the co-located backing actually serves YSQL:
// the tserver statefulset reaches rf ready replicas, YSQL accepts a connection
// over a live tserver pod IP, then a short settle so the Service endpoints the
// in-pod workload connects through are stable. Pod-Ready alone is not
// sufficient — YSQL (:5433) comes up slightly after the tserver RPC port.
func waitBackingYSQL(ctx context.Context, kube *k8s.Kubectl, info ybk8s.Info, rf int, readyTimeout time.Duration) error {
	want := fmt.Sprintf("jsonpath={.status.readyReplicas}=%d", rf)
	if err := kube.WaitFor(ctx, "statefulset/yb-tserver", want, readyTimeout); err != nil {
		return fmt.Errorf("tservers not Ready before workload: %w", err)
	}
	ci, err := freshTserverConn(ctx, kube, info)
	if err != nil {
		return err
	}
	if err := pg.WaitReady(ctx, ci, readyTimeout); err != nil {
		return fmt.Errorf("backing YSQL not accepting connections before workload: %w", err)
	}
	return sleepCtx(ctx, ysqlSettle)
}

// freshTserverConn resolves a currently-live tserver pod IP into a
// coordinator-reachable (tailnet) YSQL ConnInfo for the readiness probe.
func freshTserverConn(ctx context.Context, kube *k8s.Kubectl, info ybk8s.Info) (pg.ConnInfo, error) {
	out, err := kube.GetSelector(ctx, "pod", tserverSelector, "{.items[*].status.podIP}")
	if err != nil {
		return pg.ConnInfo{}, fmt.Errorf("resolve tserver pod IPs: %w", err)
	}
	ips := strings.Fields(out)
	if len(ips) == 0 {
		return pg.ConnInfo{}, fmt.Errorf("no live tserver pod IPs")
	}
	return pg.ConnInfo{Host: ips[0], Port: info.YSQLPort, User: info.User, Database: info.User, SSLMode: "disable"}, nil
}

// emitAndGate loads the baseline, runs the pure gate (gatePerf) on the gated
// datapoints, emits each with its comparison filled, then emits the
// informational datapoints (e.g. the per-metric .cv noise signal) UNGATED,
// persists a newly established/updated baseline, and turns a regression into a
// test failure — the shared body of every perf workload's baseline gate. Only
// gated datapoints participate in the baseline (Compare/establish/update);
// info datapoints are recorded and emitted for the report but never gated.
//
// It first enforces COMPARABILITY: a stored baseline is only gated against when
// its load_signature matches this run's (id). A baseline established at a
// different YB version/image or a different load config is NOT a valid gate —
// gating against it produces the bogus "regressions" this redesign removes — so
// on a mismatch it emits the datapoints UNGATED and WARNs instead of failing.
func emitAndGate(t *core.T, cfg perfConfig, version, testName string, gated, info []core.Datapoint, id Identity, logf func(string, ...any)) error {
	f, err := baseline.Load(cfg.BaselinePath)
	if err != nil {
		return fmt.Errorf("load baseline: %w", err)
	}

	stored, hasStored := f.StoredMeta(version, testName)
	if !comparableToBaseline(stored, hasStored, cfg.UpdateBaseline, id) {
		// Not comparable: emit the datapoints raw (no baseline comparison) and
		// WARN — do not touch the stored baseline, do not fail.
		for _, dp := range gated {
			t.Datapoint(dp)
		}
		for _, dp := range info {
			t.Datapoint(dp)
		}
		logf("perf: baseline not comparable for %s — stored(load=%s image=%s) vs current(load=%s image=%s); skipping gate, recording datapoints only",
			testName, orNone(stored.LoadSignature), orNone(stored.YBImage), orNone(id.LoadSignature), orNone(id.YBImage))
		return nil
	}

	out := gatePerf(f, version, testName, gated, cfg)
	for _, dp := range out.Datapoints {
		t.Datapoint(dp)
	}
	for _, dp := range info {
		t.Datapoint(dp)
	}
	if out.Updated || out.Established {
		// Stamp the run identity so a later run can decide comparability.
		f.StampMeta(version, testName, id.meta())
		if err := baseline.Save(cfg.BaselinePath, f); err != nil {
			return fmt.Errorf("save baseline: %w", err)
		}
	}
	switch {
	case out.Updated:
		logf("perf: baseline updated at %s (--update-baseline)", cfg.BaselinePath)
	case out.Established:
		logf("perf: no baseline for %s@%s — established at %s (WARN, not gated)", testName, version, cfg.BaselinePath)
	case out.Regressed:
		return fmt.Errorf("perf regression vs baseline: %s", regressionSummary(out.Datapoints))
	default:
		logf("perf: all datapoints within thresholds vs baseline %s", cfg.BaselinePath)
	}
	return nil
}

// comparableToBaseline is the PURE comparability decision for the single-run
// gate: a run is comparable to a stored baseline only when their load_signature
// matches. With no stored baseline (a first run) or --update-baseline there is
// nothing to be incomparable with, so the gate proceeds (to establish/refresh).
// A stored baseline whose load_signature differs from the current run's (a
// different YB version/image or a different load config) is NOT comparable — the
// caller then skips the gate and WARNs instead of raising a bogus regression.
func comparableToBaseline(stored baseline.Meta, hasStored, updateBaseline bool, id Identity) bool {
	if !hasStored || updateBaseline {
		return true
	}
	return stored.LoadSignature == id.LoadSignature
}

// orNone renders an empty identity field as "(none)" for a log line.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// attachBenchSamples records the captured output of every benchmark run — the
// discarded warmup and each measured iteration — onto sink under
// <prefix>-warmup.out / <prefix>-iter-<n>.out, so the raw evidence for a
// multi-iteration run lands on disk regardless of parse/gate outcome.
func attachBenchSamples(sink apps.ArtifactSink, res BenchResult, prefix string) {
	if res.Warmup != nil && res.Warmup.Raw != "" {
		_ = sink.Add(prefix+"-warmup.out", "log", []byte(res.Warmup.Raw))
	}
	for i, s := range res.All {
		if s.Raw != "" {
			_ = sink.Add(fmt.Sprintf("%s-iter-%d.out", prefix, i), "log", []byte(s.Raw))
		}
	}
}

// emitBenchCharts adds the benchmark's graphs to the test's report via the
// atomic T.Chart API — one call per non-empty chart, no bespoke per-test report
// code. It is the shared helper every perf workload (perf/tpcc, perf/pgbench)
// calls after RunBenchmark, so the two tests never duplicate chart wiring.
func emitBenchCharts(t *core.T, res BenchResult) {
	for _, c := range benchCharts(res) {
		t.Chart(c)
	}
}

// benchCharts turns a BenchResult into the charts a perf run reports: TPS over
// time and latency over time (a line series per kept iteration that carries a
// progress time series), and per-iteration TPS (a bar across the kept
// iterations — the run-to-run spread that pairs with the CV datapoint). A chart
// whose underlying series is empty (e.g. a workload whose tool reports no
// periodic progress, so Sample.Series is empty) is skipped rather than emitted
// blank. Pure — unit-tested without a live run.
func benchCharts(res BenchResult) []core.Chart {
	var charts []core.Chart

	var tpsSeries, latSeries []core.ChartSeries
	for i, s := range res.Kept {
		name := fmt.Sprintf("iter %d", i+1)
		var tps, lat []core.ChartPoint
		for _, p := range s.Series {
			tps = append(tps, core.ChartPoint{X: p.ElapsedSec, Y: p.TPS})
			if p.LatencyMS != 0 {
				lat = append(lat, core.ChartPoint{X: p.ElapsedSec, Y: p.LatencyMS})
			}
		}
		if len(tps) > 0 {
			tpsSeries = append(tpsSeries, core.ChartSeries{Name: name, Points: tps})
		}
		if len(lat) > 0 {
			latSeries = append(latSeries, core.ChartSeries{Name: name, Points: lat})
		}
	}
	if len(tpsSeries) > 0 {
		charts = append(charts, core.Chart{
			Title: "TPS over time", Kind: core.ChartLine,
			XLabel: "elapsed (s)", YLabel: "tps", Series: tpsSeries,
		})
	}
	if len(latSeries) > 0 {
		charts = append(charts, core.Chart{
			Title: "Latency over time", Kind: core.ChartLine,
			XLabel: "elapsed (s)", YLabel: "latency (ms)", Series: latSeries,
		})
	}

	if len(res.Kept) > 0 {
		var pts []core.ChartPoint
		for i, s := range res.Kept {
			pts = append(pts, core.ChartPoint{X: float64(i + 1), Y: s.TPS})
		}
		charts = append(charts, core.Chart{
			Title: "Per-iteration TPS", Kind: core.ChartBar,
			XLabel: "iteration", YLabel: "tps",
			Series: []core.ChartSeries{{Name: "tps", Points: pts}},
		})
	}
	return charts
}

// shellQuote wraps s in single quotes, escaping embedded single quotes, so it
// is safe as one argument in a remote /bin/sh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
