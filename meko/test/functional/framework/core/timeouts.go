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
	// DefaultProvisionTimeout bounds EnvProvider.Provision. Even a shared meko
	// stack is a whole helm install, and its api-server and langfuse migrations
	// run as DDL on the bundled single-node database, which 10m did not cover on
	// amp-dev. A multi-node database scales past it, see
	// ResolveProvisionTimeout.
	DefaultProvisionTimeout = 30 * time.Minute
	// DefaultDedicatedProvisionTimeout covers a YBA install, an RF=3 universe
	// and a stack: about 16 minutes on itest-dev when nothing is queued.
	DefaultDedicatedProvisionTimeout = 60 * time.Minute

	// ProvisionPerExtraNode extends the provision budget per store node beyond
	// the first when the stack database is not the chart's: pods start in parallel, but a cold
	// node's image pull and PVC bind serialize behind the autoscaler, and the
	// chart rolls the StatefulSets in order. 1 node = 30m, 3 = 35m, 6 = 42m30s.
	ProvisionPerExtraNode = 150 * time.Second
	DefaultCollectTimeout = 2 * time.Minute
	// DefaultSharedInfraProbeTimeout bounds the post-fault shared-stack probe; it
	// must outlast the provider's own recovery budget, not the test's body.
	DefaultSharedInfraProbeTimeout = 16 * time.Minute
)

// The factor every provisioning wait is scaled by, so a contended cluster buys
// headroom in one place instead of thirteen constants. At or below zero is 1.0.
func ResolveProvisionMultiplier(cfg *Config) float64 {
	if cfg == nil {
		return 1
	}
	m := cfg.GetFloat("timeouts.provision_multiplier", 1)
	if m <= 0 {
		return 1
	}
	return m
}

func scaleProvision(cfg *Config, d time.Duration) time.Duration {
	return time.Duration(float64(d) * ResolveProvisionMultiplier(cfg))
}

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
// and a multi-node database adds ProvisionPerExtraNode per node beyond
// the first. An explicit timeouts.provision always wins verbatim. cfg may be
// nil.
func ResolveProvisionTimeout(cfg *Config) time.Duration {
	if cfg == nil {
		return DefaultProvisionTimeout
	}
	const unset = time.Duration(-1)
	if d := cfg.GetDuration("timeouts.provision", unset); d != unset {
		return scaleProvision(cfg, d)
	}
	base := DefaultProvisionTimeout
	if cfg.GetString("db.provider", "yugabyted") == "chart" {
		return scaleProvision(cfg, base)
	}
	nodes := max(cfg.GetInt("db.nodes", 0), cfg.GetInt("db.rf", 3), 1)
	return scaleProvision(cfg, base+time.Duration(nodes-1)*ProvisionPerExtraNode)
}

// ResolveProvisionTimeoutFor is ResolveProvisionTimeout for one test's shape: a
// DedicatedStack test's provision brings up a backing and a control plane of
// its own, so it gets timeouts.provision_dedicated instead. cfg may be nil.
func ResolveProvisionTimeoutFor(cfg *Config, spec EnvSpec) time.Duration {
	if !spec.Dedicated() {
		return ResolveProvisionTimeout(cfg)
	}
	if cfg == nil {
		return DefaultDedicatedProvisionTimeout
	}
	// A test that declares its own backing shape may also declare what standing
	// that shape up costs, through the same overlay the provider deploys from,
	// a universe on cloud VMs across regions does not fit the budget a
	// co-located one needs.
	if spec.BackingConfig != nil {
		if overlay := spec.BackingConfig(cfg); len(overlay) > 0 {
			cfg = cfg.Derive(overlay)
		}
	}
	return scaleProvision(cfg, cfg.GetDuration("timeouts.provision_dedicated", DefaultDedicatedProvisionTimeout))
}

// ResolveTeardownTimeout resolves timeouts.teardown from cfg, else
// DefaultTeardownTimeout. cfg may be nil.
func ResolveTeardownTimeout(cfg *Config) time.Duration {
	if cfg == nil {
		return DefaultTeardownTimeout
	}
	return cfg.GetDuration("timeouts.teardown", DefaultTeardownTimeout)
}

// ResolveSharedInfraProbeTimeout resolves timeouts.shared_infra_probe from cfg,
// else DefaultSharedInfraProbeTimeout. cfg may be nil.
func ResolveSharedInfraProbeTimeout(cfg *Config) time.Duration {
	if cfg == nil {
		return DefaultSharedInfraProbeTimeout
	}
	return cfg.GetDuration("timeouts.shared_infra_probe", DefaultSharedInfraProbeTimeout)
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
