package core

import (
	"context"
	"fmt"
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
	// real environment is configured (e.g. "env.docker not configured").
	ProviderUnavailableReason string
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
				results[i] = RunOne(ctx, tests[i], opts)
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
		provisionTimeout = ResolveProvisionTimeout(opts.Config)
	}
	pctx, pcancel := context.WithTimeout(ctx, provisionTimeout)
	env, err := opts.Provider.Provision(pctx, test.Env, seed.Fork("env"))
	pcancel()
	if err != nil {
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

// newResult builds the initial Result and per-test seed.
func newResult(test Test, opts RunOptions) (*Result, *Seed) {
	seed := opts.Seed.ForTest(test.Name)
	result := &Result{
		TestName:    test.Name,
		Suite:       test.Suite,
		Description: test.Description,
		OwningTeam:  test.OwningTeam,
		Category:    test.Category,
		Seed:        seed.Value(),
		StartedAt:   time.Now(),
		Versions:    opts.DetectedVersions,
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
		for k, v := range s.Properties {
			if str, ok := v.(string); ok {
				s.Properties[k] = masker.Mask(str)
			}
		}
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
	// A setup-capable test runs its provision-and-prepare Setup before the body
	// (the body may consume the result via t.Standing); the Setup's teardown is
	// captured and run after collect below, so `run` leaves nothing standing —
	// the difference from `ampqa setup`, which keeps the env up. setupFailed
	// distinguishes a provisioning failure (env_failed) from a body failure.
	var (
		setupTeardown func(context.Context) error
		setupFailed   bool
	)
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
		if test.Run == nil {
			return nil
		}
		return test.Run(t)
	})

	// Collect after RUN, before teardown (best-effort). It runs on its own
	// bounded deadline, independent of the test context, so collection still
	// happens after a test timeout.
	cctx, ccancel := context.WithTimeout(context.Background(), ResolveCollectTimeout(opts.Config))
	_ = env.Collect(cctx, discardArtifactSink{})
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
			result.Status = StatusFailed
			if setupFailed {
				result.Status = StatusEnvFailed
			}
			result.ErrorSummary = wd.err.Error()
		case failed:
			result.Status = StatusFailed
			if failErr != nil {
				result.ErrorSummary = failErr.Error()
			}
		default:
			result.Status = StatusPassed
		}
	}
}

type discardArtifactSink struct{}

func (discardArtifactSink) Add(string, string, []byte) error { return nil }
