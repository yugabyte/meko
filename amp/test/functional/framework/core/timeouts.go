package core

import (
	"context"
	"time"
)

// Default per-category test timeouts.
const (
	DefaultFunctionalTimeout  = 10 * time.Minute
	DefaultStressTimeout      = 60 * time.Minute
	DefaultPerfTimeout        = 150 * time.Minute
	DefaultLongRunningTimeout = 24 * time.Hour
	DefaultTeardownTimeout    = 5 * time.Minute
	// DefaultProvisionTimeout bounds EnvProvider.Provision: a provision that waits
	// on a pod pays a node add + image pull, which 5m turned into an env_failed.
	// The single-node budget. A multi-node yb_k8s backing scales past it — see
	// ResolveProvisionTimeout — so a bigger store buys headroom without making a
	// stuck single-node provision take twice as long to detect.
	DefaultProvisionTimeout = 10 * time.Minute

	// ProvisionPerExtraNode extends the provision budget per store node beyond
	// the first when env.yb_deploy=yb_k8s: pods start in parallel, but a cold
	// node's image pull and PVC bind serialize behind the autoscaler, and the
	// chart rolls the StatefulSets in order. 1 node = 10m, 3 = 15m, 6 = 22m30s.
	ProvisionPerExtraNode = 150 * time.Second
	DefaultCollectTimeout = 2 * time.Minute
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

// ResolveProvisionTimeout resolves timeouts.provision from cfg, else a default
// scaled to the backing the run deploys: the base covers a single-node store,
// and a multi-node yb_k8s backing adds ProvisionPerExtraNode per node beyond
// the first. An explicit timeouts.provision always wins verbatim. cfg may be
// nil.
func ResolveProvisionTimeout(cfg *Config) time.Duration {
	if cfg == nil {
		return DefaultProvisionTimeout
	}
	const unset = time.Duration(-1)
	if d := cfg.GetDuration("timeouts.provision", unset); d != unset {
		return d
	}
	base := DefaultProvisionTimeout
	if cfg.GetString("env.yb_deploy", "") != "yb_k8s" {
		return base
	}
	rf := cfg.GetInt("apps.ybk8s.replication_factor", 1)
	nodes := max(cfg.GetInt("apps.ybk8s.tserver_replicas", 0),
		cfg.GetInt("apps.ybk8s.master_replicas", 0), rf, 1)
	return base + time.Duration(nodes-1)*ProvisionPerExtraNode
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
