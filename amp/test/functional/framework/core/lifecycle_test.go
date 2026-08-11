package core_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/fake"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

func runOne(t *testing.T, test core.Test, provider core.EnvProvider, detected core.DetectedVersions) *core.Result {
	t.Helper()
	_, results := core.RunTests(context.Background(), []core.Test{test}, core.RunOptions{
		Parallel:         1,
		Seed:             core.NewRunSeed(1, true),
		Config:           core.NewConfig(),
		Provider:         provider,
		DetectedVersions: detected,
	})
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	return results[0]
}

func TestLifecyclePass(t *testing.T) {
	p := &fake.Provider{}
	r := runOne(t, core.Test{Name: "t", Category: core.Functional, Run: func(*core.T) error { return nil }}, p, core.DetectedVersions{})
	if r.Status != core.StatusPassed {
		t.Errorf("status = %s, want passed (%q)", r.Status, r.ErrorSummary)
	}
	if p.TeardownCount() != 1 {
		t.Errorf("teardown ran %d times, want 1", p.TeardownCount())
	}
}

func TestLifecycleReturnedErrorFails(t *testing.T) {
	p := &fake.Provider{}
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error { return fmt.Errorf("boom") }}, p, core.DetectedVersions{})
	if r.Status != core.StatusFailed {
		t.Errorf("status = %s, want failed", r.Status)
	}
	if r.ErrorSummary != "boom" {
		t.Errorf("error summary = %q, want boom", r.ErrorSummary)
	}
	if p.TeardownCount() != 1 {
		t.Errorf("teardown must still run on failure")
	}
}

func TestLifecycleFailHelper(t *testing.T) {
	p := &fake.Provider{}
	r := runOne(t, core.Test{Name: "t", Run: func(tt *core.T) error {
		tt.Failf("assertion %d", 1)
		return nil
	}}, p, core.DetectedVersions{})
	if r.Status != core.StatusFailed {
		t.Errorf("status = %s, want failed via t.Fail", r.Status)
	}
}

func TestLifecyclePanicIsBroken(t *testing.T) {
	p := &fake.Provider{}
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error { panic("kaboom") }}, p, core.DetectedVersions{})
	if r.Status != core.StatusBroken {
		t.Errorf("status = %s, want broken", r.Status)
	}
	if p.TeardownCount() != 1 {
		t.Errorf("teardown must run after a panic")
	}
}

func TestLifecycleProvisionFailIsEnvFailed(t *testing.T) {
	p := &fake.Provider{FailProvision: true}
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error { return nil }}, p, core.DetectedVersions{})
	if r.Status != core.StatusEnvFailed {
		t.Errorf("status = %s, want env_failed", r.Status)
	}
	if p.TeardownCount() != 0 {
		t.Errorf("teardown must NOT run when provisioning failed, ran %d", p.TeardownCount())
	}
}

// A provider refusing the test's shape is a capability gap, not a failure: the
// universe provider refuses amp-endpoint shapes so those tests can never
// silently pass against raw YugabyteDB.
func TestLifecycleUnsupportedShapeSkips(t *testing.T) {
	p := &fake.Provider{FailProvision: true, ProvisionErr: &core.UnsupportedShapeError{
		Provider: "universe", Shape: core.SharedControlPlane,
		Why: "the universe env is raw YugabyteDB; this test needs an amp endpoint",
	}}
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error { return nil }}, p, core.DetectedVersions{})
	if r.Status != core.StatusSkipped {
		t.Errorf("status = %s, want skipped", r.Status)
	}
	if !strings.Contains(r.SkipReason, "amp endpoint") {
		t.Errorf("skip reason %q must name why the shape was refused", r.SkipReason)
	}
	if p.TeardownCount() != 0 {
		t.Errorf("teardown must NOT run for a refused shape, ran %d", p.TeardownCount())
	}
}

func TestLifecycleVersionOutOfBoundsSkips(t *testing.T) {
	p := &fake.Provider{}
	test := core.Test{
		Name:     "t",
		Versions: core.VersionSpec{MinProduct: "2025.2.0.0"},
		Run:      func(*core.T) error { return nil },
	}
	r := runOne(t, test, p, core.DetectedVersions{Product: "2025.1.0.0"})
	if r.Status != core.StatusSkipped {
		t.Errorf("status = %s, want skipped", r.Status)
	}
	if r.SkipReason == "" {
		t.Errorf("skip reason should be recorded")
	}
	if p.ProvisionCount() != 0 || p.TeardownCount() != 0 {
		t.Errorf("must skip before provisioning: prov=%d teardown=%d", p.ProvisionCount(), p.TeardownCount())
	}
}

func TestLifecycleSlowTestTimesOut(t *testing.T) {
	p := &fake.Provider{}
	test := core.Test{
		Name:    "t",
		Timeout: 30 * time.Millisecond,
		Run: func(*core.T) error {
			time.Sleep(2 * time.Second)
			return nil
		},
	}
	r := runOne(t, test, p, core.DetectedVersions{})
	if r.Status != core.StatusTimedOut {
		t.Errorf("status = %s, want timed_out", r.Status)
	}
	if p.TeardownCount() != 1 {
		t.Errorf("teardown must run after a timeout")
	}
}

func TestLifecycleLiveVersionsRecorded(t *testing.T) {
	p := &fake.Provider{Versions: core.DetectedVersions{Product: "2025.1.0.0", YB: "2.20.0.0"}}
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error { return nil }}, p, core.DetectedVersions{})
	if r.Versions.Product != "2025.1.0.0" || r.Versions.YB != "2.20.0.0" {
		t.Errorf("live versions not recorded from env: %+v", r.Versions)
	}
}

func TestLifecycleParallelNoRaces(t *testing.T) {
	p := &fake.Provider{}
	const n = 20
	tests := make([]core.Test, n)
	for i := range tests {
		tests[i] = core.Test{
			Name:     fmt.Sprintf("t%d", i),
			Category: core.Functional,
			Run: func(tt *core.T) error {
				return tt.Step("work", func(s *core.StepHandle) error {
					s.Set("i", tt.Seed.Value())
					return nil
				})
			},
		}
	}
	summary, results := core.RunTests(context.Background(), tests, core.RunOptions{
		Parallel: 4,
		Seed:     core.NewRunSeed(1, true),
		Provider: p,
	})
	if summary.Counts[core.StatusPassed] != n {
		t.Errorf("passed = %d, want %d", summary.Counts[core.StatusPassed], n)
	}
	for i, r := range results {
		if r == nil || r.Status != core.StatusPassed {
			t.Errorf("result %d not passed: %+v", i, r)
		}
	}
	if p.TeardownCount() != n {
		t.Errorf("teardown ran %d times, want %d", p.TeardownCount(), n)
	}
}

// A test registered with Skip must report skipped WITHOUT provisioning — the
// point of the field is that unimplemented behaviour costs no environment.
func TestLifecycleRegisteredSkipDoesNotProvision(t *testing.T) {
	p := &fake.Provider{}
	ran := false
	test := core.Test{
		Name: "features/not_implemented",
		Skip: "feature X is not implemented",
		Run: func(*core.T) error {
			ran = true
			return nil
		},
	}
	r := runOne(t, test, p, core.DetectedVersions{})
	if r.Status != core.StatusSkipped {
		t.Fatalf("status = %s, want skipped", r.Status)
	}
	if r.SkipReason != "feature X is not implemented" {
		t.Errorf("skip reason = %q, want the registered reason", r.SkipReason)
	}
	if p.ProvisionCount() != 0 {
		t.Errorf("provisioned %d environment(s); a registered skip must be gated before provisioning", p.ProvisionCount())
	}
	if ran {
		t.Error("the test body ran")
	}
}

// The same gate applies to a co-tenant on a shared environment.
func TestLifecycleRegisteredSkipOnSharedEnv(t *testing.T) {
	p := &fake.Provider{}
	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(1, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	r := core.RunOneOnEnv(context.Background(), core.Test{
		Name: "features/not_implemented",
		Skip: "feature X is not implemented",
		Run:  func(*core.T) error { return fmt.Errorf("body must not run") },
	}, env, core.RunOptions{Config: core.NewConfig(), Seed: core.NewRunSeed(1, true)})
	if r.Status != core.StatusSkipped {
		t.Fatalf("status = %s, want skipped", r.Status)
	}
}

// recordingSink captures the whole event stream a run produces.
type recordingSink struct {
	started  int
	finished int
	summary  core.RunSummary
	results  map[string]*core.Result
}

func (r *recordingSink) RunStarted(core.RunInfo) error { r.started++; return nil }
func (r *recordingSink) ResultEvent(ev core.ResultEvent) error {
	if ev.Result != nil && ev.Result.Status != "" {
		if r.results == nil {
			r.results = map[string]*core.Result{}
		}
		r.results[ev.TestName] = ev.Result
	}
	return nil
}
func (r *recordingSink) RunFinished(s core.RunSummary) error { r.finished++; r.summary = s; return nil }

// An environment that never came up must NOT produce an empty run: every
// selected test has to report env_failed with the reason, or the report hides
// both which tests were blocked and why.
func TestReportEnvFailedRunReportsEveryTest(t *testing.T) {
	sink := &recordingSink{}
	tests := []core.Test{
		{Name: "features/a", Suite: "features"},
		{Name: "features/b", Suite: "features"},
		{Name: "perf/c", Suite: "perf"},
	}
	const reason = "environment prepare failed: controller deployment never became available"

	summary, results := core.ReportEnvFailedRun(tests, core.RunOptions{Sink: sink}, reason)

	if len(results) != len(tests) {
		t.Fatalf("got %d results, want one per selected test (%d)", len(results), len(tests))
	}
	if summary.Total != len(tests) || summary.Counts[core.StatusEnvFailed] != len(tests) {
		t.Errorf("summary = %+v, want %d env_failed", summary.Counts, len(tests))
	}
	for _, r := range results {
		if r.Status != core.StatusEnvFailed {
			t.Errorf("%s status = %q, want env_failed", r.TestName, r.Status)
		}
		if r.ErrorSummary != reason {
			t.Errorf("%s error summary = %q, want the prepare reason", r.TestName, r.ErrorSummary)
		}
	}
	// The sinks must see a COMPLETE run, not a truncated stream: the portal
	// derives the run's own status from RunFinished.
	if sink.started != 1 || sink.finished != 1 {
		t.Errorf("run started/finished = %d/%d, want 1/1", sink.started, sink.finished)
	}
	for _, tc := range tests {
		got, ok := sink.results[tc.Name]
		if !ok {
			t.Errorf("sink never saw a terminal result for %s", tc.Name)
			continue
		}
		if got.Status != core.StatusEnvFailed || got.ErrorSummary != reason {
			t.Errorf("sink result for %s = %q / %q", tc.Name, got.Status, got.ErrorSummary)
		}
	}
}

// collectingEnv hands the engine one blob at collect time, so a test can assert
// where those bytes end up.
type collectingEnv struct{}

func (collectingEnv) ControllerURL() string           { return "" }
func (collectingEnv) Versions() core.DetectedVersions { return core.DetectedVersions{} }
func (collectingEnv) Exec() exec.RemoteExecutor       { return nil }
func (collectingEnv) Handles() core.EnvHandles        { return core.EnvHandles{} }
func (collectingEnv) Collect(_ context.Context, into core.ArtifactSink) error {
	return into.Add("controller/controller.log", "log", []byte("evidence"))
}

// collectingProvider serves that env for every test.
type collectingProvider struct{}

func (collectingProvider) Provision(context.Context, core.EnvSpec, *core.Seed) (core.Environment, error) {
	return collectingEnv{}, nil
}
func (collectingProvider) Attach(context.Context, string) (core.Environment, error) {
	return collectingEnv{}, nil
}
func (collectingProvider) Teardown(context.Context, core.Environment) error { return nil }

// What an env collects must land ON THE RESULT, not in a discard sink: a test
// that fails outside a suite flow has no other source of evidence, and the
// engine already pays for the collection either way.
func TestEnvCollectAttachesToResultOnFailure(t *testing.T) {
	failing := core.Test{Name: "t", Run: func(*core.T) error { return fmt.Errorf("boom") }}
	r := runOne(t, failing, collectingProvider{}, core.DetectedVersions{})
	if r.Status != core.StatusFailed {
		t.Fatalf("status = %s, want failed", r.Status)
	}
	if !hasAttachment(r, "controller/controller.log") {
		t.Errorf("collected evidence was dropped; attachments = %+v", r.Attachments)
	}
}

// With on_failure the same collection must NOT be attached to a green run — the
// gate is what keeps a passing suite from hauling a controller log per test.
func TestEnvCollectSkippedForPassingRunOnFailureMode(t *testing.T) {
	cfg, err := core.LoadConfigNoDefaults(nil, nil, []string{"report.collect_diagnostics=on_failure"})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	_, results := core.RunTests(context.Background(),
		[]core.Test{{Name: "t", Run: func(*core.T) error { return nil }}},
		core.RunOptions{Parallel: 1, Seed: core.NewRunSeed(1, true), Config: cfg,
			Provider: collectingProvider{}})
	if len(results) != 1 || results[0].Status != core.StatusPassed {
		t.Fatalf("want one passed result, got %+v", results)
	}
	if hasAttachment(results[0], "controller/controller.log") {
		t.Errorf("a passing run attached diagnostics under on_failure: %+v", results[0].Attachments)
	}
}

func hasAttachment(r *core.Result, name string) bool {
	for _, a := range r.Attachments {
		if a.Name == name {
			return true
		}
	}
	return false
}

/* ---- server metrics for every test ---- */

// metricsEnv records the sampling calls the lifecycle makes and hands back one
// series on drain.
type metricsEnv struct {
	mu       sync.Mutex
	started  int
	drained  int
	bodyRan  bool
	startedB bool // start happened before the body
}

func (m *metricsEnv) ControllerURL() string           { return "" }
func (m *metricsEnv) Versions() core.DetectedVersions { return core.DetectedVersions{} }
func (m *metricsEnv) Exec() exec.RemoteExecutor       { return nil }
func (m *metricsEnv) Handles() core.EnvHandles        { return core.EnvHandles{} }
func (m *metricsEnv) Collect(context.Context, core.ArtifactSink) error {
	return nil
}
func (m *metricsEnv) StartServerMetrics(context.Context, *core.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started++
	m.startedB = !m.bodyRan
}
func (m *metricsEnv) DrainServerMetrics(emit func(core.Metric)) {
	m.mu.Lock()
	m.drained++
	m.mu.Unlock()
	emit(core.Metric{Key: "[ yb tserver] Write RPCs", Value: 42, Unit: "rpc/s"})
}

type metricsProvider struct{ env *metricsEnv }

func (p metricsProvider) Provision(context.Context, core.EnvSpec, *core.Seed) (core.Environment, error) {
	return p.env, nil
}
func (p metricsProvider) Attach(context.Context, string) (core.Environment, error) {
	return p.env, nil
}
func (metricsProvider) Teardown(context.Context, core.Environment) error { return nil }

// Server metrics are collected for EVERY test, not only the perf tier that used
// to ask for them: the sampler is started before the body and drained after, and
// what it collected lands on the result as metrics.
func TestServerMetricsCollectedForEveryTest(t *testing.T) {
	env := &metricsEnv{}
	plain := core.Test{Name: "t", Run: func(tt *core.T) error {
		env.mu.Lock()
		env.bodyRan = true
		env.mu.Unlock()
		return nil
	}}
	r := runOne(t, plain, metricsProvider{env: env}, core.DetectedVersions{})
	if r.Status != core.StatusPassed {
		t.Fatalf("status = %s, want passed", r.Status)
	}
	if env.started != 1 || env.drained != 1 {
		t.Errorf("started=%d drained=%d, want 1/1", env.started, env.drained)
	}
	if !env.startedB {
		t.Error("sampling started after the body ran; the window must cover it")
	}
	if !hasMetric(r, "[ yb tserver] Write RPCs") {
		t.Errorf("collected series did not reach the result; metrics = %+v", r.Metrics)
	}
}

// A test that sampled its own backing (the perf workloads that provision one)
// still gets the sampler stopped — but its series are dropped, or every key it
// already reported would appear twice.
func TestServerMetricsNotDuplicatedWhenTestCollectsItsOwn(t *testing.T) {
	env := &metricsEnv{}
	own := core.Test{Name: "t", Run: func(tt *core.T) error {
		tt.MetricSeries(core.Metric{Key: "[ yb tserver] Write RPCs", Value: 7, Unit: "rpc/s"})
		tt.MarkServerMetricsCollected()
		return nil
	}}
	r := runOne(t, own, metricsProvider{env: env}, core.DetectedVersions{})
	if env.drained != 1 {
		t.Errorf("drained=%d, want 1 — the sampler must be stopped either way", env.drained)
	}
	if n := countMetric(r, "[ yb tserver] Write RPCs"); n != 1 {
		t.Errorf("metric emitted %d times, want exactly 1 (the test's own)", n)
	}
}

// A test whose environment cannot sample must still run and pass: observability
// never decides an outcome.
func TestTestPassesWhenEnvHasNoServerMetrics(t *testing.T) {
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error { return nil }},
		collectingProvider{}, core.DetectedVersions{})
	if r.Status != core.StatusPassed {
		t.Fatalf("status = %s, want passed", r.Status)
	}
}

func hasMetric(r *core.Result, key string) bool { return countMetric(r, key) > 0 }

func countMetric(r *core.Result, key string) int {
	n := 0
	for _, m := range r.Metrics {
		if m.Key == key {
			n++
		}
	}
	return n
}
