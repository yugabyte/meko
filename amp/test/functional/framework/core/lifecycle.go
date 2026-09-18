package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// RunOptions configures a run of the worker pool.
type RunOptions struct {
	Parallel         int
	Seed             *Seed
	Config           *Config
	Provider         EnvProvider
	Sink             Sink
	DetectedVersions DetectedVersions // for pre-provision version gating
	ProvisionTimeout time.Duration    // 0 → 5m
	TeardownTimeout  time.Duration    // 0 → DefaultTeardownTimeout
	// Masker redacts resolved secret values from recorded results (the config
	// snapshot and error summaries). nil → no masking (a no-op Masker).
	Masker *Masker
	// ProviderUnavailableReason, when Provider is nil, makes every test skip
	// cleanly with this reason instead of reporting env_failed — used when no
	// real environment is configured (e.g. "env.sharedk8s not configured").
	ProviderUnavailableReason string
	// Attempt places this execution in a retry chain; zero → newResult stamps 1.
	Attempt AttemptInfo
	// Set by whoever provisioned: RunOne for an exclusive environment, the
	// scheduler for a shared one. Zero means not recorded.
	ProvisionDuration time.Duration
}

// RunTests drives tests through the per-test lifecycle across a worker pool of
// size Parallel (min 1 — sequential is just pool size 1, never a separate path).
// Every test provisions and tears down its own environment (the all-exclusive
// path); the packing scheduler in sched adds SHARED co-tenancy on top of the
// same RunOne/RunOneOnEnv drivers. It returns the run summary and per-test
// results in input order.
func RunTests(ctx context.Context, tests []Test, opts RunOptions) (RunSummary, []*Result) {
	if opts.Seed == nil {
		opts.Seed = NewRunSeed(0, false)
	}
	parallel := opts.Parallel
	if parallel < 1 {
		parallel = 1
	}

	started := time.Now()
	if opts.Sink != nil {
		_ = opts.Sink.RunStarted(RunInfo{
			Seed:      opts.Seed.Value(),
			Total:     len(tests),
			Parallel:  parallel,
			StartedAt: started,
		})
	}

	results := make([]*Result, len(tests))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = RunWithEnvRetry(ctx, tests[i], opts, RunOne)
			}
		}()
	}
	for i := range tests {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	summary := RunSummary{
		Counts:    map[Status]int{},
		Total:     len(results),
		StartedAt: started,
		EndedAt:   time.Now(),
	}
	// One entry per test: only a chain's FINAL attempt lands here.
	for _, r := range results {
		summary.Counts[r.Status]++
	}
	if opts.Sink != nil {
		_ = opts.Sink.RunFinished(summary)
	}
	return summary, results
}

// ReportEnvFailedRun records a run whose environment never came up: no test body
// can execute, so EVERY selected test reports env_failed carrying the reason.
// Reporting nothing would be worse than useless — a run that lands with zero
// results looks like a configuration mistake, hides which tests were supposed to
// run, and leaves each test's history with no entry for the day it was blocked.
// It emits the same RunStarted/results/RunFinished shape as a real run, so every
// sink (portal, junit, jsonfile, html) sees a complete run.
func ReportEnvFailedRun(tests []Test, opts RunOptions, reason string) (RunSummary, []*Result) {
	if opts.Seed == nil {
		opts.Seed = NewRunSeed(0, false)
	}
	started := time.Now()
	if opts.Sink != nil {
		_ = opts.Sink.RunStarted(RunInfo{
			Seed:      opts.Seed.Value(),
			Total:     len(tests),
			Parallel:  1,
			StartedAt: started,
		})
	}
	results := make([]*Result, 0, len(tests))
	for _, test := range tests {
		results = append(results, ReportResult(test, opts, StatusEnvFailed, reason))
	}
	summary := RunSummary{
		Counts:    map[Status]int{},
		Total:     len(results),
		StartedAt: started,
		EndedAt:   time.Now(),
	}
	for _, r := range results {
		summary.Counts[r.Status]++
	}
	if opts.Sink != nil {
		_ = opts.Sink.RunFinished(summary)
	}
	return summary, results
}

// RunOne runs a test through the full per-test lifecycle, provisioning and
// tearing down its own environment: version-gate → provision → run (watchdog) →
// collect → teardown. Teardown always runs once provisioning succeeded, under
// its own independent deadline. This is the EXCLUSIVE path (and the worker-pool
// body); the scheduler calls it directly for exclusive tests.
func RunOne(ctx context.Context, test Test, opts RunOptions) *Result {
	result, seed := newResult(test, opts)
	emitStarted(opts.Sink, test.Name, result)
	defer finishResult(opts.Sink, test.Name, result, opts.Masker)

	// Registered skip (unimplemented behaviour) — before provisioning, so a
	// permanently-skipped test never costs an environment.
	if reason := strings.TrimSpace(test.Skip); reason != "" {
		result.Status = StatusSkipped
		result.SkipReason = reason
		return result
	}
	if reason := preflightSkip(test, opts.Config); reason != "" {
		result.Status = StatusSkipped
		result.SkipReason = reason
		return result
	}

	// Version gate — before provisioning (architecture §4.9).
	if ok, reason := test.Versions.Qualifies(opts.DetectedVersions); !ok {
		result.Status = StatusSkipped
		result.SkipReason = reason
		return result
	}

	if opts.Provider == nil {
		if opts.ProviderUnavailableReason != "" {
			result.Status = StatusSkipped
			result.SkipReason = opts.ProviderUnavailableReason
			return result
		}
		result.Status = StatusEnvFailed
		result.ErrorSummary = "no environment provider configured"
		return result
	}

	provisionTimeout := opts.ProvisionTimeout
	if provisionTimeout <= 0 {
		provisionTimeout = ResolveProvisionTimeoutFor(opts.Config, test.Env)
	}
	pctx, pcancel := context.WithTimeout(ctx, provisionTimeout)
	provisionStart := time.Now()
	env, err := opts.Provider.Provision(pctx, test.Env, seed.Fork("env"))
	pcancel()
	opts.ProvisionDuration = time.Since(provisionStart)
	if err != nil {
		// A shape the provider cannot serve is a capability gap: skipping keeps
		// the mismatch visible without failing the run, and refusing (rather
		// than provisioning what the provider does have) keeps a test that
		// needs an amp endpoint from silently passing against raw YB.
		var shape *UnsupportedShapeError
		if errors.As(err, &shape) {
			result.Status = StatusSkipped
			result.SkipReason = err.Error()
			return result
		}
		result.Status = StatusEnvFailed
		result.ErrorSummary = err.Error()
		return result
	}

	// Provision succeeded: teardown always runs, on its own deadline.
	defer func() {
		td := opts.TeardownTimeout
		if td <= 0 {
			td = ResolveTeardownTimeout(opts.Config)
		}
		tctx, tcancel := context.WithTimeout(context.Background(), td)
		defer tcancel()
		_ = opts.Provider.Teardown(tctx, env)
	}()

	result.Versions = env.Versions()
	runBody(ctx, test, env, seed, opts, result)
	return result
}

// RunOneOnEnv runs a test against a pre-provisioned environment — a co-tenant on
// a SHARED environment. It performs the full per-test lifecycle minus provision
// and teardown: the caller owns the environment's lifecycle. Version gating and
// body classification are identical to RunOne.
func RunOneOnEnv(ctx context.Context, test Test, env Environment, opts RunOptions) *Result {
	result, seed := newResult(test, opts)
	emitStarted(opts.Sink, test.Name, result)
	defer finishResult(opts.Sink, test.Name, result, opts.Masker)

	if reason := strings.TrimSpace(test.Skip); reason != "" {
		result.Status = StatusSkipped
		result.SkipReason = reason
		return result
	}
	if reason := preflightSkip(test, opts.Config); reason != "" {
		result.Status = StatusSkipped
		result.SkipReason = reason
		return result
	}

	gate := env.Versions()
	if gate == (DetectedVersions{}) {
		gate = opts.DetectedVersions
	}
	if ok, reason := test.Versions.Qualifies(gate); !ok {
		result.Status = StatusSkipped
		result.SkipReason = reason
		return result
	}
	result.Versions = env.Versions()
	runBody(ctx, test, env, seed, opts, result)
	return result
}

// ReportResult emits a terminal result (start+finish events) for a test that
// never reaches its body — e.g. a shared environment that failed to provision,
// or no provider configured. It exists so the scheduler need not duplicate the
// result/emit preamble.
func ReportResult(test Test, opts RunOptions, status Status, summary string) *Result {
	result, _ := newResult(test, opts)
	emitStarted(opts.Sink, test.Name, result)
	result.Status = status
	if status == StatusSkipped {
		result.SkipReason = summary
	} else {
		result.ErrorSummary = summary
	}
	finishResult(opts.Sink, test.Name, result, opts.Masker)
	return result
}

// verifySharedInfra makes a FaultsSharedInfra test own the damage it leaves:
// a green body with an unserving shared stack becomes a red final step.
func verifySharedInfra(t *T, test Test, opts RunOptions) error {
	if !test.Env.FaultsShared() || !ProviderSharesInfra(opts.Provider) {
		return nil
	}
	hc, ok := opts.Provider.(SharedInfraHealthChecker)
	if !ok {
		return nil
	}
	// Detached from the body budget, like Collect and setup teardown: a test that
	// spent its timeout must not fail this for lack of time.
	ctx, cancel := context.WithTimeout(context.Background(), ResolveSharedInfraProbeTimeout(opts.Config))
	defer cancel()
	err := hc.SharedInfraHealthy(ctx)
	if errors.Is(err, ErrNoSharedInfraProbe) {
		return nil
	}
	return t.Step("shared_infra_recovered", func(*StepHandle) error { return err })
}

// newResult builds the initial Result and per-test seed.
func newResult(test Test, opts RunOptions) (*Result, *Seed) {
	seed := opts.Seed.ForTest(test.Name)
	// Every result carries attempt metadata, so the portal never sees no group.
	at := opts.Attempt
	if at.Group == "" {
		at.Group = newRetryGroup()
	}
	if at.N < 1 {
		at.N = 1
	}
	result := &Result{
		TestName:    test.Name,
		Suite:       test.Suite,
		Description: test.Description,
		OwningTeam:  test.OwningTeam,
		Category:    test.Category,
		Seed:        seed.Value(),
		StartedAt:   time.Now(),
		Versions:    opts.DetectedVersions,
		RetryGroup:  at.Group,
		Attempt:     at.N,
		RetryReason: at.Reason,
	}
	if opts.Config != nil {
		snap := opts.Config.Resolved()
		// Never record the secrets subtree, and mask any secret value that
		// leaked elsewhere into the resolved tree, before it lands in a result.
		sanitizeConfigSnapshot(snap, opts.Masker)
		result.Config = snap
	}
	return result, seed
}

func emitStarted(sink Sink, name string, result *Result) {
	if sink != nil {
		_ = sink.ResultEvent(ResultEvent{Kind: EventTestStarted, TestName: name, Result: result})
	}
}

func finishResult(sink Sink, name string, result *Result, masker *Masker) {
	result.EndedAt = time.Now()
	result.Duration = result.EndedAt.Sub(result.StartedAt)
	if masker != nil {
		result.ErrorSummary = masker.Mask(result.ErrorSummary)
		result.SkipReason = masker.Mask(result.SkipReason)
		maskSteps(result.Steps, masker)
	}
	if sink != nil {
		_ = sink.ResultEvent(ResultEvent{Kind: EventTestFinished, TestName: name, Result: result})
	}
}

// maskSteps masks a step tree's names and string-valued properties in place, so
// a secret a test wrote into a step never survives into a recorded result.
func maskSteps(steps []*Step, masker *Masker) {
	for _, s := range steps {
		s.Name = masker.Mask(s.Name)
		s.maskProps(masker)
		maskSteps(s.Steps, masker)
	}
}

// runBody runs a version-gate-passed, env-provisioned test body under the
// watchdog, collects best-effort, then classifies the outcome into result.
// Shared by RunOne and RunOneOnEnv.
func runBody(ctx context.Context, test Test, env Environment, seed *Seed, opts RunOptions, result *Result) {
	timeout := TestTimeout(test, opts.Config)
	bctx, bcancel := context.WithTimeout(ctx, timeout)
	defer bcancel()

	t := newT(bctx, env, seed, opts.Config, opts.Masker, result, sinkEmitter{sink: opts.Sink, testName: test.Name})
	if opts.ProvisionDuration > 0 {
		t.Metric("env.provision_seconds", opts.ProvisionDuration.Seconds(), "s")
	}
	// A setup-capable test runs its provision-and-prepare Setup before the body
	// (the body may consume the result via t.Standing); the Setup's teardown is
	// captured and run after collect below, so `run` leaves nothing standing —
	// the difference from `ampqa setup`, which keeps the env up. setupFailed
	// distinguishes a provisioning failure (env_failed) from a body failure.
	var (
		setupTeardown func(context.Context) error
		setupFailed   bool
	)
	// Server metrics for EVERY test: the env samples its own backing tservers/
	// masters and the amp endpoints across the body. Started before Setup so a
	// provisioning-heavy test is covered too, and drained after the body on an
	// independent deadline (below) so a timed-out test still reports them.
	sm, hasServerMetrics := env.(ServerMetricsEnv)
	if hasServerMetrics {
		sm.StartServerMetrics(bctx, opts.Config)
	}

	wd := watchdog(bctx, timeout, bcancel, func() error {
		if test.Setup != nil {
			sr, err := test.Setup(t)
			if err != nil {
				setupFailed = true
				return err
			}
			if sr != nil {
				t.Standing = sr.Env
				setupTeardown = sr.Teardown
			}
		}
		if test.Run != nil {
			if err := test.Run(t); err != nil {
				return err
			}
		}
		return verifySharedInfra(t, test, opts)
	})

	// Collect after RUN, before teardown (best-effort). It runs on its own
	// bounded deadline, independent of the test context, so collection still
	// happens after a test timeout.
	//
	// What the env gathers is ATTACHED to the result when the run needs
	// explaining. It used to be dropped into a discard sink: the env did the work
	// — controller logs, pod logs, events — and the bytes went nowhere, so a test
	// that failed outside the amp-feature flow (which collects for itself) landed
	// with a single error line and no evidence at all. Gated by
	// report.collect_diagnostics so a green run does not haul logs per test.
	// Drain before Collect: both run on their own deadlines, and the series are
	// result data (metrics), not attachments. A test that sampled its own backing
	// still drains — to stop the sampler — but its series are discarded, because
	// emitting them again would duplicate every key it already reported.
	if hasServerMetrics {
		if t.ServerMetricsCollected() {
			sm.DrainServerMetrics(func(Metric) {})
		} else {
			sm.DrainServerMetrics(t.MetricSeries)
		}
	}

	cctx, ccancel := context.WithTimeout(context.Background(), ResolveCollectTimeout(opts.Config))
	_ = env.Collect(cctx, collectSink(t, wd, opts))
	ccancel()

	// Reap what Setup created, after collect. Reading setupTeardown is only safe
	// once the body goroutine finished (the watchdog contract) — skip on timeout;
	// the provider-level Teardown in RunOne is the env backstop either way.
	if !wd.timedOut && setupTeardown != nil {
		stctx, stcancel := context.WithTimeout(context.Background(), ResolveTeardownTimeout(opts.Config))
		_ = setupTeardown(stctx)
		stcancel()
	}

	switch {
	case wd.timedOut:
		result.Status = StatusTimedOut
		result.ErrorSummary = fmt.Sprintf("deadline exceeded after %s", timeout)
	case wd.panicked:
		result.Status = StatusBroken
		result.ErrorSummary = fmt.Sprintf("panic: %v", wd.panicVal)
	default:
		failed, failErr, skipped, skipMsg := t.outcome()
		switch {
		case skipped:
			result.Status = StatusSkipped
			result.SkipReason = skipMsg
		case wd.err != nil:
			// A Setup failure, or a body error marked core.EnvFail, is not a product signal.
			result.Status = StatusFailed
			result.ErrorSummary = wd.err.Error()
			if setupFailed || IsEnvFailure(wd.err) || KubeAPIUnreachable(wd.err) {
				result.Status = StatusEnvFailed
			} else if reason, ok := attestConnDrop(env, wd.err); ok {
				result.Status = StatusEnvFailed
				result.ErrorSummary += " [" + reason + "]"
			} else if reason, ok := attestSharedInfraGone(test, opts.Provider); ok {
				result.Status = StatusEnvFailed
				result.ErrorSummary += " [" + reason + "]"
			}
		case failed:
			result.Status = StatusFailed
			if failErr != nil {
				result.ErrorSummary = failErr.Error()
				if reason, ok := attestConnDrop(env, failErr); ok {
					result.Status = StatusEnvFailed
					result.ErrorSummary += " [" + reason + "]"
				} else if KubeAPIUnreachable(failErr) {
					result.Status = StatusEnvFailed
				} else if reason, ok := attestSharedInfraGone(test, opts.Provider); ok {
					result.Status = StatusEnvFailed
					result.ErrorSummary += " [" + reason + "]"
				}
			}
		default:
			result.Status = StatusPassed
		}
	}
}

type discardArtifactSink struct{}

func (discardArtifactSink) Add(string, string, []byte) error { return nil }

// testArtifactSink persists what an env collects onto the test's result, so the
// bytes reach the run's artifacts dir and the report portal.
type testArtifactSink struct{ t *T }

func (s testArtifactSink) Add(name, kind string, content []byte) error {
	s.t.AttachBytes(name, kind, content)
	return nil
}

// collectSink decides whether this run keeps the env's diagnostics. The gate is
// the same report.collect_diagnostics knob the suites use, judged against the
// same notion of failure the result will carry — a timeout and a panic count,
// because those are exactly the runs whose evidence is hardest to reconstruct
// afterwards.
func collectSink(t *T, wd watchdogResult, opts RunOptions) ArtifactSink {
	failed, _, _, _ := t.outcome()
	failed = failed || wd.err != nil || wd.timedOut || wd.panicked
	mode := DefaultCollectDiagnostics
	if opts.Config != nil {
		mode = opts.Config.GetString("report.collect_diagnostics", DefaultCollectDiagnostics)
	}
	if !shouldCollect(mode, failed) {
		return discardArtifactSink{}
	}
	return testArtifactSink{t: t}
}

// shouldCollect mirrors diag.ShouldCollect, which core cannot import (diag
// depends on core), including its choice to read an unrecognised mode as
// on_failure rather than silently hauling logs for every green run.
func shouldCollect(mode string, failed bool) bool {
	switch mode {
	case "never":
		return false
	case "always":
		return true
	default: // on_failure and anything unrecognised
		return failed
	}
}

// attestSharedInfraGone: a verdict reached on a deleted control plane is not one.
func attestSharedInfraGone(test Test, prov EnvProvider) (string, bool) {
	// Only the shared stack: a dedicated test runs in its own namespace, which
	// this answers nothing about.
	if test.Env.Dedicated() || !ProviderSharesInfra(prov) {
		return "", false
	}
	c, ok := prov.(SharedInfraGoneChecker)
	if !ok {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), sharedInfraGoneTimeout)
	defer cancel()
	where, gone := c.SharedInfraGone(ctx)
	if !gone {
		return "", false
	}
	return "env: " + where + " " + sharedInfraGoneMarker + ", so this verdict is not the product's", true
}

const sharedInfraGoneTimeout = 15 * time.Second

// preflightSkip runs the test's Preflight against the run config; a nil config
// (a bare unit call) is asked as an empty one.
func preflightSkip(test Test, cfg *Config) string {
	if test.Preflight == nil {
		return ""
	}
	if cfg == nil {
		cfg = NewConfig()
	}
	return strings.TrimSpace(test.Preflight(cfg))
}
