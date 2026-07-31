package core

import (
	"context"
	"fmt"
	"time"
)

// StandingEnv describes an environment left running for manual use after a
// setup-only invocation (`ampqa setup`). It is both the handle a developer
// gets back and the record persisted in the standing-environment registry, so
// a later, separate `ampqa env rm <id>` process can find the environment and
// reconstruct its teardown. Everything here is plain, serializable data — no
// live handles — because the reaping process never shares memory with the one
// that provisioned it.
type StandingEnv struct {
	Name     string            // human label (developer-facing / registry name)
	Shape    string            // environment shape (e.g. "yb_k8s", "yba_k8s")
	Provider string            // logical provider name: how `env rm` rebuilds a teardowner
	Ref      string            // attach ref the provider's Attach understands (env rm)
	TTL      time.Duration     // 0 → never auto-reaped (a standing dev env)
	Info     map[string]string // connection info + teardown-reconstruction data
}

// SetupResult is what a Test.Setup returns: the standing-env handle plus the
// teardown that reaps everything Setup created. The `run` lifecycle invokes
// Teardown at the end (nothing is left running); `ampqa setup` DISCARDS it —
// the environment persists until `ampqa env rm` reconstructs its teardown from
// the registered StandingEnv (Provider + Ref + Info).
type SetupResult struct {
	Env      *StandingEnv
	Teardown func(context.Context) error
}

// SetupOne runs ONLY the setup phase of a test: version-gate, then invoke the
// test's Setup, which provisions the environment and prepares fixtures. It does
// NOT run the test body or any teardown — the environment is LEFT RUNNING for
// manual use. It returns the terminal Result plus the standing-env handle the
// caller registers (nil when the test exposed no usable standing env). Setup is
// bounded by the test's category timeout, so a long real-infra provision
// (a YBA universe create is tens of minutes) has room.
func SetupOne(ctx context.Context, test Test, opts RunOptions) (*Result, *SetupResult) {
	if opts.Seed == nil {
		opts.Seed = NewRunSeed(0, false)
	}
	result, seed := newResult(test, opts)
	emitStarted(opts.Sink, test.Name, result)
	defer finishResult(opts.Sink, test.Name, result, opts.Masker)

	if ok, reason := test.Versions.Qualifies(opts.DetectedVersions); !ok {
		result.Status = StatusSkipped
		result.SkipReason = reason
		return result, nil
	}
	if test.Setup == nil {
		result.Status = StatusBroken
		result.ErrorSummary = "test exposes no Setup phase (ampqa setup requires a setup-capable test)"
		return result, nil
	}

	timeout := TestTimeout(test, opts.Config)
	bctx, bcancel := context.WithTimeout(ctx, timeout)
	defer bcancel()
	t := newT(bctx, nil, seed, opts.Config, opts.Masker, result, sinkEmitter{sink: opts.Sink, testName: test.Name})

	var sr *SetupResult
	wd := watchdog(bctx, timeout, bcancel, func() error {
		r, err := test.Setup(t)
		if err != nil {
			return err
		}
		sr = r
		if r != nil {
			t.Standing = r.Env
		}
		return nil
	})

	switch {
	case wd.timedOut:
		result.Status = StatusTimedOut
		result.ErrorSummary = fmt.Sprintf("setup deadline exceeded after %s", timeout)
		return result, nil
	case wd.panicked:
		result.Status = StatusBroken
		result.ErrorSummary = fmt.Sprintf("panic: %v", wd.panicVal)
		return result, nil
	case wd.err != nil:
		// A Setup failure is a provisioning failure — classify it env_failed.
		result.Status = StatusEnvFailed
		result.ErrorSummary = wd.err.Error()
		return result, nil
	}

	// Safe to read sr here: the body goroutine finished (channel-close
	// happens-before), which the timeout branches above never reach.
	failed, failErr, skipped, skipMsg := t.outcome()
	switch {
	case skipped:
		result.Status = StatusSkipped
		result.SkipReason = skipMsg
		return result, nil
	case failed:
		result.Status = StatusFailed
		if failErr != nil {
			result.ErrorSummary = failErr.Error()
		}
		return result, nil
	}
	result.Status = StatusPassed
	return result, sr
}
