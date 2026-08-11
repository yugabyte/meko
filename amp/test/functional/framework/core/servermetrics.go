package core

import "context"

// Server metrics are collected for EVERY test, not just the perf tier.
//
// The sampler existed and `apps.metrics.enabled` defaulted to true, yet results
// carried no server series at all: collection was opt-in per test (four perf
// workloads called it) and, where it was called, it was pointed at the amp
// control-plane namespace — so any run whose YugabyteDB lives elsewhere (a YBA
// universe, a co-located ybk8s deploy) sampled a namespace with no tserver in
// it and gave up with one best-effort log line:
//
//	perf: metrics sampler not started (best-effort): no pods matched
//	app=yb-tserver / app=yb-master in namespace "ampqa-cp-aa1e38d0"
//
// So the environment — the only party that knows where its servers actually run
// — now owns collection, and the lifecycle drives it around every test body.
// The interface is declared here, in the layer that owns the lifecycle, and
// carries no kubernetes or sampler types so this package keeps its place at the
// bottom of the import graph.

// ServerMetricsEnv is implemented by an environment that can sample the servers
// under a test — the backing YugabyteDB's tservers/masters and the amp compute
// endpoints. Both halves are best-effort by contract: observability must never
// decide whether a test passes, so neither method returns an error.
type ServerMetricsEnv interface {
	// StartServerMetrics begins sampling, reading its own knobs from the run
	// config it is handed (apps.metrics.*) — the environment is not required to
	// have been given one of its own. Called once per test, before its body; a
	// second call while sampling is already in flight is a no-op, so a shared
	// environment serving co-tenant tests keeps one sampler per source.
	StartServerMetrics(ctx context.Context, cfg *Config)
	// DrainServerMetrics stops sampling and hands every collected series to emit.
	// Called after the body, on a deadline independent of the test's own, so a
	// test that timed out still reports what its servers were doing.
	DrainServerMetrics(emit func(Metric))
}

// MarkServerMetricsCollected records that the TEST collected server metrics
// itself, which suppresses the environment-level drain. The perf workloads that
// provision their own backing sample that backing directly — the environment
// would otherwise emit a second set of series under the same keys, and the
// report would show each metric twice.
func (t *T) MarkServerMetricsCollected() {
	t.mu.Lock()
	t.serverMetricsCollected = true
	t.mu.Unlock()
}

// ServerMetricsCollected reports whether the test already collected its own.
func (t *T) ServerMetricsCollected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.serverMetricsCollected
}
