package core

import (
	"context"
	"time"
)

// Default per-category test timeouts.
const (
	DefaultFunctionalTimeout  = 10 * time.Minute
	DefaultStressTimeout      = 60 * time.Minute
	DefaultPerfTimeout        = 30 * time.Minute
	DefaultLongRunningTimeout = 24 * time.Hour
	DefaultTeardownTimeout    = 5 * time.Minute
	DefaultProvisionTimeout   = 5 * time.Minute
	DefaultCollectTimeout     = 2 * time.Minute
)

// CategoryDefaultTimeout is the hard-coded fallback test timeout for a
// category, used when cfg is nil or carries no timeouts.* override. Prefer
// ResolveCategoryTimeout, which checks config first.
func CategoryDefaultTimeout(c Category) time.Duration {
	switch c {
	case Functional:
		return DefaultFunctionalTimeout
	case Stress:
		return DefaultStressTimeout
	case Perf:
		return DefaultPerfTimeout
	case LongRunning:
		return DefaultLongRunningTimeout
	default:
		return DefaultFunctionalTimeout
	}
}

// categoryTimeoutKey is the timeouts.* config key for a category.
func categoryTimeoutKey(c Category) string {
	switch c {
	case Stress:
		return "timeouts.stress"
	case Perf:
		return "timeouts.perf"
	case LongRunning:
		return "timeouts.long_running"
	default:
		return "timeouts.functional"
	}
}

// ResolveCategoryTimeout resolves a category's default test-body timeout:
// timeouts.<category> from cfg (embedded configs/default.yaml supplies it in
// normal operation), else the hard-coded CategoryDefaultTimeout. cfg may be
// nil.
func ResolveCategoryTimeout(c Category, cfg *Config) time.Duration {
	def := CategoryDefaultTimeout(c)
	if cfg == nil {
		return def
	}
	return cfg.GetDuration(categoryTimeoutKey(c), def)
}

// ResolveProvisionTimeout resolves timeouts.provision from cfg, else
// DefaultProvisionTimeout. cfg may be nil.
func ResolveProvisionTimeout(cfg *Config) time.Duration {
	if cfg == nil {
		return DefaultProvisionTimeout
	}
	return cfg.GetDuration("timeouts.provision", DefaultProvisionTimeout)
}

// ResolveTeardownTimeout resolves timeouts.teardown from cfg, else
// DefaultTeardownTimeout. cfg may be nil.
func ResolveTeardownTimeout(cfg *Config) time.Duration {
	if cfg == nil {
		return DefaultTeardownTimeout
	}
	return cfg.GetDuration("timeouts.teardown", DefaultTeardownTimeout)
}

// ResolveCollectTimeout resolves timeouts.collect from cfg, else
// DefaultCollectTimeout. cfg may be nil.
func ResolveCollectTimeout(cfg *Config) time.Duration {
	if cfg == nil {
		return DefaultCollectTimeout
	}
	return cfg.GetDuration("timeouts.collect", DefaultCollectTimeout)
}

// TestTimeout resolves a test's effective timeout: its own override, else the
// category default (config first, then the hard-coded constant). cfg may be
// nil.
func TestTimeout(t Test, cfg *Config) time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return ResolveCategoryTimeout(t.Category, cfg)
}

// watchdogResult reports how a watched body finished.
type watchdogResult struct {
	timedOut bool
	panicked bool
	panicVal any
	err      error
}

// watchdog runs body in a goroutine bounded by timeout and the parent context.
// On the completion path it safely reads the body's error/panic (channel-close
// happens-before). On the timeout/cancel path it returns without touching those
// fields, so the still-running goroutine cannot race the reader. cancel is
// invoked on overrun to nudge a cooperative body to exit.
func watchdog(ctx context.Context, timeout time.Duration, cancel context.CancelFunc, body func() error) watchdogResult {
	done := make(chan struct{})
	var res watchdogResult
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				res.panicked = true
				res.panicVal = r
			}
		}()
		res.err = body()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		return res
	case <-timer.C:
		cancel()
		return watchdogResult{timedOut: true}
	case <-ctx.Done():
		cancel()
		return watchdogResult{timedOut: true}
	}
}
