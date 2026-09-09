// Package perf is the PERFORMANCE tier: tests that provision a real backing,
// stand an amp control plane + compute endpoint on it, drive a benchmark
// workload over a client pod THROUGH that endpoint, parse throughput + latency,
// and gate those numbers against a stored baseline (a regression fails the
// test). It reuses the same live pieces as the combined stress scenario —
// apps/ybk8s (backing + teardown), apps/ampstack (the control plane),
// apps/clientpod (the in-cluster workload pod), and apps/benchbase (the TPC-C
// driver) — plus the baseline package for the gate.
//
// The product under test is amp, so the full amp path (compute → pageserver →
// YB) is in every measured request. The co-located YugabyteDB is provisioned
// exactly as before, but purely as the control plane's backing store — no
// workload connects to it directly.
//
// Perf tests self-provision real infrastructure (a k8s cluster + a chart), so
// they SKIP cleanly unless the environment gate is satisfied.
// BenchBase is a PUBLIC release, so — unlike the combined stress scenario — a
// perf test needs no private jar secret. Teardown is guaranteed on every path
// (LIFO stack on its own deadline). Registered by init.
package perf

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/baseline"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
)

// Cheap defaults: a small TPC-C is inexpensive but still yields a stable tps.
// Overridable under perf.* (configs/default.yaml).
const (
	DefaultRF          = 3
	DefaultWarehouses  = 4
	DefaultTerminals   = 8
	DefaultClientImage = "eclipse-temurin:21-jdk"
	DefaultRemoteDir   = "/opt/ampqa"
	DefaultMaxHeap     = "2g" // java -Xmx; small TPC-C needs little, fits a modest pod

	DefaultProvisions        = 3
	DefaultBaselinePath      = "baselines/perf.json"
	DefaultThroughputDropPct = 20.0
	DefaultLatencyRisePct    = 35.0
	DefaultReadyTimeout      = 5 * time.Minute
	DefaultHealthTimeout     = 5 * time.Minute
	// One datapoint this far past its allowance fails on its own. Below that it
	// takes DefaultLiveArmMinViolations agreeing: a single moderate outlier out of
	// ten is the shape noise takes here, several at once is not.
	DefaultLiveArmGrossFactor   = 2.0
	DefaultLiveArmMinViolations = 2
	// default-big is the one node pool a benchmark may run on: uniform
	// e2-standard-16, ours, and outside GKE node auto-provisioning. Unpinned, a
	// pod lands in whichever nap-* pool has room, and an arm on the slower
	// machine measured delete_half at 5200ms against 2538ms for the identical
	// image elsewhere (run 84ad29eb). Override with perf.arm_node_selector.
	DefaultArmNodeSelector = "cloud.google.com/gke-nodepool=default-big"
)

// perfConfig is the resolved perf.* configuration.
type perfConfig struct {
	RF int
	// Fresh-stack repeats; the gated headline is the median across them.
	Provisions        int
	Warehouses        int
	Terminals         int
	ClientImage       string
	ClientPullSecret  string
	RemoteDir         string
	MaxHeap           string
	BaselinePath      string
	UpdateBaseline    bool
	ThroughputDropPct float64
	LatencyRisePct    float64
	ReadyTimeout      time.Duration
	HealthTimeout     time.Duration
	// LiveArm is how the two-arm comparison turns deltas into a verdict.
	LiveArm liveArmPolicy
	// ArmNodeSelector pins both arms' tservers and endpoint computes to one
	// machine type, so their delta is the build. Empty disables the pin.
	ArmNodeSelector string
	// Benchmark is the BenchBase workload the perf/tpcc test runs
	// (apps.benchbase.benchmark: tpcc | tpch | ycsb | smallbank | …); default
	// tpcc. Lets a different BenchBase benchmark run config-only, no code change.
	Benchmark string
}

// perfFromConfig reads perf.* into a perfConfig.
func perfFromConfig(c *core.Config) perfConfig {
	return perfConfig{
		RF:                c.GetInt("perf.rf", DefaultRF),
		Provisions:        maxInt(1, c.GetInt("perf.provisions", DefaultProvisions)),
		Warehouses:        c.GetInt("perf.warehouses", DefaultWarehouses),
		Terminals:         c.GetInt("perf.terminals", DefaultTerminals),
		ClientImage:       c.GetString("perf.client_image", DefaultClientImage),
		ClientPullSecret:  c.GetString("perf.client_pull_secret", ""),
		RemoteDir:         c.GetString("perf.remote_dir", DefaultRemoteDir),
		MaxHeap:           c.GetString("perf.max_heap", DefaultMaxHeap),
		BaselinePath:      c.GetString("perf.baseline_path", DefaultBaselinePath),
		UpdateBaseline:    c.GetBool("perf.update_baseline", false),
		ThroughputDropPct: c.GetFloat("perf.throughput_drop_pct", DefaultThroughputDropPct),
		LatencyRisePct:    c.GetFloat("perf.latency_rise_pct", DefaultLatencyRisePct),
		ReadyTimeout:      c.GetDuration("perf.ready_timeout", DefaultReadyTimeout),
		HealthTimeout:     c.GetDuration("perf.health_timeout", DefaultHealthTimeout),
		LiveArm: liveArmPolicy{
			Thr: float64Thresholds{
				DropPct: c.GetFloat("perf.throughput_drop_pct", DefaultThroughputDropPct),
				RisePct: c.GetFloat("perf.latency_rise_pct", DefaultLatencyRisePct),
			},
			UseHistoryBand: c.GetBool("perf.live_arm.use_history_band", true),
			GrossFactor:    c.GetFloat("perf.live_arm.gross_factor", DefaultLiveArmGrossFactor),
			MinViolations:  c.GetInt("perf.live_arm.min_violations", DefaultLiveArmMinViolations),
		},
		ArmNodeSelector: c.GetString("perf.arm_node_selector", DefaultArmNodeSelector),
		Benchmark:       c.GetString("apps.benchbase.benchmark", "tpcc"),
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// thresholds is the perfConfig's regression tolerance in the baseline gate's shape.
func (c perfConfig) thresholds() baseline.Thresholds {
	return baseline.Thresholds{DropPct: c.ThroughputDropPct, RisePct: c.LatencyRisePct}
}

// perfGate returns a skip reason (and false) only when the ENVIRONMENT can't run
// the perf tier: no k8s cluster to deploy into, or no resolvable YB version.
// Selecting a perf test is the intent — there is no separate opt-in flag.
// Needs NO private jar secret — BenchBase is public.
func perfGate(c *core.Config, cfg perfConfig) (string, bool) {
	if c.GetString("apps.ybk8s.kubecontext", "") == "" &&
		c.GetString("apps.ybk8s.kubeconfig", "") == "" &&
		c.GetString("env.sharedk8s.context", "") == "" &&
		c.GetString("env.ampstack.context", "") == "" {
		return "perf needs a k8s cluster: set apps.ybk8s.kubecontext/kubeconfig (or env.sharedk8s.context / env.ampstack.context)", false
	}
	if chartVersion(c) == "" {
		return "perf needs a YB version: set apps.ybk8s.chart_version, versions.yb or versions.yugabytedb", false
	}
	return "", true
}

// ampEndpointGate returns a skip reason (and false) when the run has no way to
// deploy an amp control plane. The heavy benchmarks measure THROUGH a compute
// endpoint, so without the controller chart there is nothing to measure — and
// falling back to the raw backing would silently produce numbers that are not
// comparable with anything this tier reports. The setup-only pgbench preset,
// which drives YSQL by hand, does not call this.
func ampEndpointGate(c *core.Config) (string, bool) {
	if c.GetString("env.ampstack.controller_chart", "") == "" &&
		c.GetString("env.ampstack.controller_manifest", "") == "" {
		return "perf runs through an amp compute endpoint: set env.ampstack.controller_chart (path to the yb-amp-controller Helm chart)", false
	}
	return "", true
}

// chartVersion resolves the YB version key the gate and baseline keying use:
// the explicit chart_version, else whichever YB version the run already sets
// (versions.yb, then versions.yugabytedb) — no perf-specific parameter needed.
// It is the deploy's OWN resolution, so the gate can never pass a version the
// deploy would then reject, and a baseline is keyed on what actually ran.
func chartVersion(c *core.Config) string { return ybk8s.ChartVersion(c) }

// modeReporter is implemented by env providers that expose their resolved
// provisioning mode (env/universe.Env). Declared locally so the gate stays
// decoupled from the concrete package and unit-testable with a bare fake.
type modeReporter interface{ Mode() string }

// gateNoDoubleProvision skips the perf test when the resolved env layer is
// itself a real-infra universe.Env in a provisioning mode: the test
// self-provisions its OWN backing, so running the body would provision a SECOND
// real backing on top of the first. --fake-env (no Mode method) and attach-mode
// envs (a mode outside the provisioning set) pass.
func gateNoDoubleProvision(env core.Environment) (string, bool) {
	mr, ok := env.(modeReporter)
	if !ok {
		return "", true
	}
	switch mr.Mode() {
	case universe.ModeProvisionYugabyted, universe.ModeProvisionYBA:
		return fmt.Sprintf("perf self-provisions its backing; run with --fake-env — env.universe.mode=%s would double-provision", mr.Mode()), false
	}
	return "", true
}

// teardownStack is a LIFO stack of best-effort cleanups run at the end
// regardless of outcome: every cleanup executes (most recent first) even when an
// earlier one errs, and the first error is returned.
type teardownStack struct {
	mu  sync.Mutex
	fns []namedCleanup
}

type namedCleanup struct {
	name string
	fn   func(context.Context) error
}

func (s *teardownStack) push(name string, fn func(context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fns = append(s.fns, namedCleanup{name, fn})
}

func (s *teardownStack) run(ctx context.Context, logf func(string, ...any)) error {
	s.mu.Lock()
	fns := s.fns
	s.fns = nil
	s.mu.Unlock()
	var firstErr error
	for i := len(fns) - 1; i >= 0; i-- {
		c := fns[i]
		if err := c.fn(ctx); err != nil {
			if logf != nil {
				logf("perf teardown: %s (best-effort): %v", c.name, err)
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", c.name, err)
			}
		}
	}
	return firstErr
}

// attachSink records every collected blob on the result AND persists its bytes
// (via T.AttachBytes), so benchbase output and any other collected artifact land
// on disk under the run's artifacts dir — the body-level collect satisfying
// report_kill's collect step with the evidence, not just its size.
type attachSink struct {
	t    *core.T
	logf func(string, ...any)
}

func (s attachSink) Add(name, kind string, content []byte) error {
	s.t.AttachBytes(name, kind, content)
	if s.logf != nil {
		s.logf("collected %s (%s, %d bytes)", name, kind, len(content))
	}
	return nil
}

// sleepCtx waits d, returning early (with ctx.Err) if the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
