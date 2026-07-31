package core

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"runtime"
	"sync"
	"time"
)

// T is the per-test context handed to a test body. It embeds a context.Context
// for cancellation and carries the environment, seed, resolved options, and the
// step/attachment/metric emission surface.
type T struct {
	context.Context

	Env     Environment
	Seed    *Seed
	Options *Config

	// Standing is the handle to the environment the Setup phase stood up,
	// populated before the body runs when the test exposes a Setup. A body may
	// read it (connection info, endpoints); nil for tests without a Setup phase.
	Standing *StandingEnv

	masker     *Masker
	emit       emitter
	result     *Result
	nonce      string
	maxSteps   int
	maxMetrics int
	mu         sync.Mutex
	failed     bool
	failErr    error
	skipped    bool
	skipMsg    string
}

// Per-test retention caps for the in-memory result snapshot. The live sink
// already received every step/metric; these only bound what a single Result
// retains so a 24h soak emitting per-cycle events can't grow unbounded.
const (
	DefaultMaxStepsRetained   = 10000
	DefaultMaxMetricsRetained = 10000
)

// DefaultCollectDiagnostics is the default report.collect_diagnostics mode:
// gather k8s diagnostics (amp + backing pod logs, describes, events) before
// teardown on EVERY run, pass or fail, so every test result carries its logs.
// Set report.collect_diagnostics=on_failure to collect only on failure.
const DefaultCollectDiagnostics = "always"

// newT builds a test context. emit may be nil (no reporter).
func newT(ctx context.Context, env Environment, seed *Seed, opts *Config, masker *Masker, result *Result, emit emitter) *T {
	if opts == nil {
		opts = NewConfig()
	}
	return &T{
		Context: ctx, Env: env, Seed: seed, Options: opts, masker: masker,
		result: result, emit: emit, nonce: newResourceNonce(),
		maxSteps:   opts.GetInt("report.max_steps_retained", DefaultMaxStepsRetained),
		maxMetrics: opts.GetInt("report.max_metrics_retained", DefaultMaxMetricsRetained),
	}
}

// newResourceNonce draws a short random token unique to this test execution.
// It is NOT seed-derived: two invocations replaying the same --seed still get
// distinct nonces, which is exactly what keeps their infra resource
// names/tags/namespaces from colliding.
func newResourceNonce() string {
	var b [4]byte
	_, _ = cryptorand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ResourceSuffix is the identifier-safe token every provisioned resource name
// (namespaces, SQL databases/tables, cloud tags, project names) embeds so
// concurrent tests never clash. It combines the seed (for traceability back to
// a run) with a per-invocation nonce (for uniqueness across same-seed runs). It
// is all-hex, so it is safe as an unquoted SQL identifier and a DNS label, and
// stable across calls within one test execution. Seed-derived determinism for
// workloads/chaos comes from Seed.Fork, never from this token.
func (t *T) ResourceSuffix() string {
	seedHex := "0"
	if t.Seed != nil {
		seedHex = fmt.Sprintf("%x", uint64(t.Seed.Value()))
	}
	return seedHex + t.nonce
}

// Masker returns the run's secret masker (never nil — a no-op Masker if none
// was configured). Wrap an app's logf with it (masker.Logf(base)) so a resolved
// credential can't leak through that app's own log lines.
func (t *T) Masker() *Masker {
	if t.masker == nil {
		return NewMasker()
	}
	return t.masker
}

// Mask replaces any known secret value in s with the mask — a convenience for
// test/app code building a string that might contain a resolved credential.
func (t *T) Mask(s string) string { return t.masker.Mask(s) }

// Step runs fn as a top-level named span.
func (t *T) Step(name string, fn func(*StepHandle) error) error {
	return runStep(name, 0, t.emit, &t.mu, func(child *Step) {
		t.mu.Lock()
		if t.maxSteps > 0 && len(t.result.Steps) >= t.maxSteps {
			drop := len(t.result.Steps) - t.maxSteps + 1
			t.result.Steps = append(t.result.Steps[:0], t.result.Steps[drop:]...)
			t.result.StepsDropped += drop
		}
		t.result.Steps = append(t.result.Steps, child)
		t.mu.Unlock()
	}, fn)
}

// Attach records an attachment on the result and emits it.
func (t *T) Attach(name, kind string, size int64) {
	a := Attachment{Name: name, Kind: kind, Size: size}
	t.mu.Lock()
	t.result.Attachments = append(t.result.Attachments, a)
	t.mu.Unlock()
	if t.emit != nil {
		t.emit.emit(ResultEvent{Kind: EventAttachment, Attachment: &a})
	}
}

// AttachBytes records an attachment on the result (like Attach — name, kind,
// size) AND emits the event with the raw bytes so a persisting sink writes them
// to disk. Use it wherever the collected blob's content matters (logs, dumps,
// diagnostics); Attach stays metadata-only for callers that only know a size.
func (t *T) AttachBytes(name, kind string, content []byte) {
	a := Attachment{Name: name, Kind: kind, Size: int64(len(content))}
	t.mu.Lock()
	t.result.Attachments = append(t.result.Attachments, a)
	t.mu.Unlock()
	if t.emit != nil {
		t.emit.emit(ResultEvent{Kind: EventAttachment, Attachment: &a, Content: content})
	}
}

// Metric records a metric point on the result and emits it.
func (t *T) Metric(key string, value float64, unit string) {
	m := Metric{Key: key, Value: value, Unit: unit}
	t.mu.Lock()
	if t.maxMetrics > 0 && len(t.result.Metrics) >= t.maxMetrics {
		drop := len(t.result.Metrics) - t.maxMetrics + 1
		t.result.Metrics = append(t.result.Metrics[:0], t.result.Metrics[drop:]...)
		t.result.MetricsDropped += drop
	}
	t.result.Metrics = append(t.result.Metrics, m)
	t.mu.Unlock()
	if t.emit != nil {
		t.emit.emit(ResultEvent{Kind: EventMetric, Metric: &m})
	}
}

// Fail marks the test failed and records the message. Execution continues; the
// test body should return promptly. Use FailNow to stop immediately.
func (t *T) Fail(args ...any) {
	t.mu.Lock()
	t.failed = true
	if t.failErr == nil {
		t.failErr = fmt.Errorf("%s", fmt.Sprint(args...))
	}
	t.mu.Unlock()
}

// Failf is Fail with a format string.
func (t *T) Failf(format string, args ...any) {
	t.mu.Lock()
	t.failed = true
	if t.failErr == nil {
		t.failErr = fmt.Errorf(format, args...)
	}
	t.mu.Unlock()
}

// FailNow marks the test failed and stops the test body (via runtime.Goexit).
func (t *T) FailNow(args ...any) {
	t.Fail(args...)
	runtime.Goexit()
}

// Skip marks the test skipped and stops the test body.
func (t *T) Skip(reason string) {
	t.mu.Lock()
	t.skipped = true
	t.skipMsg = reason
	t.mu.Unlock()
	runtime.Goexit()
}

// Skipf is Skip with a format string.
func (t *T) Skipf(format string, args ...any) {
	t.Skip(fmt.Sprintf(format, args...))
}

// Deadline reports the test's deadline, if any (from the embedded context).
func (t *T) Deadline() (time.Time, bool) {
	return t.Context.Deadline()
}

// WaitFor polls cond until it returns true, the timeout elapses, or the context
// is cancelled. It sleeps interval between polls.
func (t *T) WaitFor(desc string, timeout, interval time.Duration, cond func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := cond()
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", desc, err)
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", timeout, desc)
		}
		select {
		case <-t.Context.Done():
			return fmt.Errorf("cancelled while waiting for %s: %w", desc, t.Context.Err())
		case <-time.After(interval):
		}
	}
}

// outcome captures the flags set during a test body, read only after the body
// goroutine has finished (channel-close happens-before) to stay race-free.
func (t *T) outcome() (failed bool, failErr error, skipped bool, skipMsg string) {
	return t.failed, t.failErr, t.skipped, t.skipMsg
}

// Failed reports whether the test body has marked the test failed (via Fail /
// Failf / FailNow). It does NOT observe a failure signalled purely by a returned
// error — a lifecycle that fails by returning an error must OR this with that
// error when deciding to collect diagnostics.
func (t *T) Failed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failed
}
