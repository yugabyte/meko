package perf

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/clientpod"
	"github.com/yugabyte/amp/test/functional/framework/apps/metrics"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/baseline"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/diag"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// The backing glue shared by every perf workload (perf/tpcc, perf/pgbench,
// perf/sysbench) and the pgbench standing-env preset: deploy the co-located YB,
// launch the client pod, wait for YSQL, and gate datapoints against the
// baseline. Kept as plain functions (not a shared base struct) so each workload
// owns its own run state.
//
// The benchmark workloads do NOT connect here — they run through the amp
// compute endpoint this backing stands behind (perf_endpoint_helper.go). The
// direct backing ConnInfo below serves the setup-only pgbench preset, which is
// a hand-driven YSQL load env with no amp in the picture.

// backingConn is the co-located backing's own YSQL target: the tserver Service
// address, which every driver understands and which k8s spreads across tservers
// per TCP connection.
func backingConn(info ybk8s.Info) pg.ConnInfo {
	host, port := ybk8s.FirstHostPort(info.InClusterHostPorts)
	return pg.ConnInfo{Host: host, Port: port, User: info.User, Database: info.User, SSLMode: "disable"}
}

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
		return ybk8s.Info{}, nil, core.EnvFail("secrets: %w", err)
	}
	d := ybk8s.New(k8sCfg, t.ResourceSuffix()).WithLogf(logf).WithSecretResolver(secrets.Value)
	td.push("teardown yb-in-k8s (namespace + PVCs + client pod)", func(ctx context.Context) error {
		return d.Teardown(ctx)
	})

	info, err := d.Deploy(ctx)
	if err != nil {
		// Standing the backing up is ENVIRONMENT work, not a perf regression.
		return ybk8s.Info{}, nil, core.EnvFail("provision yb in k8s: %w", err)
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
		return nil, core.EnvFail("start client pod: %w", err)
	}
	td.push("report-kill client pod (collect logs + delete)", func(ctx context.Context) error {
		return cp.Stop(ctx, sink)
	})
	return cp.Exec(), nil
}

// waitBackingYSQL blocks until the co-located backing actually serves YSQL: the
// tserver statefulset reaches rf ready replicas, YSQL answers a query, then a
// short settle so the Service endpoints the in-pod workload connects through are
// stable. Pod-Ready alone is not sufficient — YSQL (:5433) comes up slightly
// after the tserver RPC port.
//
// The probe runs INSIDE the cluster, via `kubectl exec` into a tserver. It used
// to dial the tserver's pod IP from this process, which only works where the
// coordinator has a route into the pod CIDR — and where it does not, every dial
// times out (packets to an unroutable address, so a timeout rather than a
// refusal) while the backing is perfectly healthy. That cost four tests a full
// readiness budget each on 0.260810.0: `postgres not ready at 10.21.139.36:5433:
// dial error: timeout`, an address reachable from the very client pod that was
// about to run the workload.
//
// Probing from in-cluster also tests the path that matters: the workload runs in
// the client pod, never from here.
func waitBackingYSQL(ctx context.Context, kube *k8s.Kubectl, info ybk8s.Info, rf int, readyTimeout time.Duration, sink apps.ArtifactSink, logf func(string, ...any)) error {
	want := fmt.Sprintf("jsonpath={.status.readyReplicas}=%d", rf)
	if err := kube.WaitFor(ctx, "statefulset/yb-tserver", want, readyTimeout); err != nil {
		collectBackingEvidence(ctx, kube, sink, logf)
		return core.EnvFail("tservers not Ready before workload: %w", err)
	}
	if err := waitYSQLInCluster(ctx, kube, info, readyTimeout); err != nil {
		collectBackingEvidence(ctx, kube, sink, logf)
		return core.EnvFail("backing YSQL not accepting connections before workload: %w", err)
	}
	return sleepCtx(ctx, ysqlSettle)
}

// collectBackingEvidence attaches the backing's own account of itself when it
// never became ready. Nothing did before: a wait_backing_ready failure arrived
// with a controller log, an endpoint log, and nothing about the YugabyteDB it was
// waiting for or the nodes it ran on — so four identical failures on 0.260810.0
// could only be diagnosed by catching a later run live. Best-effort, and never
// allowed to mask the readiness error it is explaining.
func collectBackingEvidence(ctx context.Context, kube *k8s.Kubectl, sink apps.ArtifactSink, logf func(string, ...any)) {
	if sink == nil {
		return
	}
	// The wait's own ctx is typically spent, which is why we are here.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backingEvidenceBudget)
	defer cancel()
	diag.CollectUniverseK8sDiagnostics(cctx, kube, kube.Namespace(), sink, logf)
}

// backingEvidenceBudget bounds the collection: pod logs and describes for an
// RF=3 backing plus the node listings, on a path that has already failed.
const backingEvidenceBudget = 4 * time.Minute

// waitYSQLInCluster polls `ysqlsh -c "select 1"` inside a tserver pod until it
// answers. The pod is re-resolved every attempt, so a tserver that is replaced
// mid-wait does not pin the probe to a pod that no longer exists.
func waitYSQLInCluster(ctx context.Context, kube *k8s.Kubectl, info ybk8s.Info, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, last)
		}
		pod, err := firstTserverPod(ctx, kube)
		if err != nil {
			last = err
		} else {
			// ysqlsh is resolved rather than assumed: a relative `bin/ysqlsh`
			// depends on the container's working directory and exited 127 ("sh:
			// bin/ysqlsh: No such file or directory") for a whole 5-minute budget
			// against a backing whose YSQL had been up for four seconds.
			//
			// -h $(hostname -f): YB binds the advertise address, so loopback is not
			// necessarily listening.
			probe := fmt.Sprintf(
				`YSQLSH=$(command -v ysqlsh || echo %s); "$YSQLSH" -h "$(hostname -f)" -p %d -U %s -d %s -tAc "select 1"`,
				ysqlshFallbackPath, info.YSQLPort, info.User, info.User)
			res, err := kube.ForPod(pod, tserverContainer).Run(ctx, probe)
			switch {
			case err != nil:
				last = fmt.Errorf("exec ysqlsh in %s: %w", pod, err)
			case res.ExitCode != 0:
				last = fmt.Errorf("ysqlsh in %s exited %d: %s", pod, res.ExitCode, strings.TrimSpace(res.Stderr))
			case strings.Contains(res.Stdout, "1"):
				return nil
			default:
				last = fmt.Errorf("ysqlsh in %s answered %q, want 1", pod, strings.TrimSpace(res.Stdout))
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("YSQL did not answer inside the cluster within %s (last: %w)", timeout, last)
		}
		if err := sleepCtx(ctx, ysqlProbeInterval); err != nil {
			return errors.Join(err, last)
		}
	}
}

// firstTserverPod names a currently-existing tserver pod, resolved per attempt.
func firstTserverPod(ctx context.Context, kube *k8s.Kubectl) (string, error) {
	out, err := kube.GetSelector(ctx, "pod", tserverSelector, "{.items[0].metadata.name}")
	if err != nil {
		return "", fmt.Errorf("resolve a tserver pod: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("no tserver pods matched %s", tserverSelector)
	}
	return strings.TrimSpace(out), nil
}

// tserverContainer is the container ysqlsh lives in; ysqlProbeInterval paces the
// probe (a cold RF=3 backing needs tens of seconds, so a tight loop only adds
// exec churn).
const (
	tserverContainer = "yb-tserver"
	// ysqlshFallbackPath is where the YB image keeps ysqlsh when it is not on
	// PATH. Used only if `command -v` finds nothing.
	ysqlshFallbackPath = "/home/yugabyte/bin/ysqlsh"
)

// ysqlProbeInterval paces the probe: a cold RF=3 backing needs tens of seconds,
// so a tight loop only adds exec churn. A var so tests do not have to sleep it.
var ysqlProbeInterval = 5 * time.Second

// startMetricsSampler starts the background YB server-metrics sampler over the
// backing's namespace and registers its drain on td, so whatever it collected
// reaches the report even when the benchmark fails. It returns nil when
// apps.metrics.enabled is false or the sampler could not start — collection is
// observability, never a reason to fail a perf run — and every caller treats a
// nil sampler as "nothing to emit".
func startMetricsSampler(ctx context.Context, t *core.T, kube *k8s.Kubectl, td *teardownStack, logf func(string, ...any)) *metrics.Sampler {
	cfg := metrics.FromConfig(t.Options)
	if !cfg.Enabled {
		return nil
	}
	s := metrics.New(kube, cfg).WithLogf(logf)
	if err := s.Start(ctx); err != nil {
		logf("perf: metrics sampler not started (best-effort): %v", err)
		return nil
	}
	td.push("drain metrics sampler (emit collected series)", func(context.Context) error {
		emitMetricSeries(t, s)
		return nil
	})
	// This test samples the backing it provisioned itself, in that backing's own
	// namespace. The environment samples for every test by default; telling it
	// this one is covered keeps each metric key from being emitted twice.
	t.MarkServerMetricsCollected()
	return s
}

// startEndpointSampler starts the background sampler over the amp COMPUTE
// ENDPOINT pod of this run's own control plane and registers its drain on td.
// Every heavy perf run now measures through that endpoint, so its pod is as
// much a part of the request path as the backing tservers are. Best-effort in
// every direction, exactly like startMetricsSampler.
func startEndpointSampler(ctx context.Context, t *core.T, ep ampEndpoint, td *teardownStack, logf func(string, ...any)) *metrics.Sampler {
	cfg := metrics.FromConfig(t.Options)
	if !cfg.Enabled || ep.Kube == nil {
		return nil
	}
	s := metrics.NewEndpoint(ep.Kube, cfg, ep.podSelector()).WithLogf(logf)
	if err := s.Start(ctx); err != nil {
		logf("perf: endpoint metrics sampler not started (best-effort): %v", err)
		return nil
	}
	td.push("drain endpoint metrics sampler (emit collected series)", func(context.Context) error {
		emitMetricSeries(t, s)
		return nil
	})
	return s
}

// endpointPodsEnv is the capability endpoint-side sampling needs: an env that
// hands out its namespace-scoped kubectl and the selector matching its endpoint
// pods. Declared structurally (like storageEnv) so the sampling is simply
// skipped on an env that cannot serve it.
type endpointPodsEnv interface {
	AmpKube() *k8s.Kubectl
	EndpointPodSelector() string
}

// startEndpointMetricsSampler starts the sampler over the amp COMPUTE ENDPOINT
// pods instead of a YB backing — the only server in the request path of a test
// that runs against an endpoint (perf/microbench). Best-effort in every
// direction: an env without the capability, a disabled app, or a failed start
// all return nil, and the caller emits nothing.
func startEndpointMetricsSampler(ctx context.Context, t *core.T, logf func(string, ...any)) *metrics.Sampler {
	env, ok := t.Env.(endpointPodsEnv)
	if !ok {
		return nil
	}
	kube := env.AmpKube()
	if kube == nil {
		return nil
	}
	cfg := metrics.FromConfig(t.Options)
	if !cfg.Enabled {
		return nil
	}
	s := metrics.NewEndpoint(kube, cfg, env.EndpointPodSelector()).WithLogf(logf)
	if err := s.Start(ctx); err != nil {
		logf("perf: endpoint metrics sampler not started (best-effort): %v", err)
		return nil
	}
	return s
}

// emitMetricSeries stops the sampler and emits one metric per collected key,
// carrying the per-pod series. Sampler.Stop yields the series once, so calling
// this on the success path and again from teardown emits nothing twice.
func emitMetricSeries(t *core.T, s *metrics.Sampler) {
	if s == nil {
		return
	}
	for _, ser := range s.Stop() {
		t.MetricSeries(core.Metric{
			Key:      ser.Key,
			Value:    ser.Average(),
			Unit:     ser.Unit,
			RoundVal: ser.Round,
			Raw:      ser.Raw,
			Desc:     ser.Desc,
			Series:   ser.Pairs(),
		})
	}
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
