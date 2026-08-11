package exclusivek8s

import (
	"context"

	"github.com/yugabyte/amp/test/functional/framework/apps/metrics"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Server-metrics collection, per test, for this environment's own stack — the
// same contract sharedk8s implements (core.ServerMetricsEnv). An exclusive
// environment belongs to one test, so there is no co-tenant sharing to arbitrate.

// StartServerMetrics begins sampling the backing servers of this stack.
// Best-effort: a sampler that will not start is logged and skipped.
func (e *Env) StartServerMetrics(ctx context.Context, opts *core.Config) {
	if e.app == nil || opts == nil {
		return
	}
	kube := e.app.Kube()
	if kube == nil {
		return
	}
	cfg := metrics.FromConfig(opts)
	if !cfg.Enabled {
		return
	}
	e.smMu.Lock()
	defer e.smMu.Unlock()
	if e.sampler != nil {
		return
	}
	// An external backing runs in its own namespace and says so; the built-in
	// yugabyted shares the stack's.
	ns := e.app.BackingNamespace()
	if ns == "" {
		ns = e.app.Namespace()
	}
	s := metrics.New(kube.InNamespace(ns), cfg).WithLogf(e.logf)
	if err := s.Start(ctx); err != nil {
		e.logf("exclusivek8s: backing metrics not sampled in namespace %q (best-effort): %v", ns, err)
		return
	}
	e.sampler = s
}

// DrainServerMetrics stops the sampler and hands over what it collected.
func (e *Env) DrainServerMetrics(emit func(core.Metric)) {
	e.smMu.Lock()
	s := e.sampler
	e.sampler = nil
	e.smMu.Unlock()
	if s == nil {
		return
	}
	for _, ser := range s.Stop() {
		emit(core.Metric{
			Key:      ser.Key,
			Value:    ser.Average(),
			Unit:     ser.Unit,
			RoundVal: ser.Round,
			Raw:      ser.Raw,
			Desc:     ser.Desc,
			Series:   ser.Pairs(),
		})
	}
}
