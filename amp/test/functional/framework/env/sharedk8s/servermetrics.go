package sharedk8s

import (
	"context"
	"sync"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/metrics"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Server-metrics collection for every test on this environment.
//
// The environment is the only party that knows where its servers run: the
// backing YugabyteDB may be the built-in yugabyted in the amp namespace, or a
// ybk8s/YBA universe in a namespace of its own. Pointing a sampler at the amp
// namespace and hoping (what the perf tier did) collected nothing whenever the
// backing lived elsewhere.

// serverSamplers holds the samplers a single test's window owns.
type serverSamplers struct {
	mu      sync.Mutex
	backing *metrics.Sampler
	compute *metrics.Sampler
}

// StartServerMetrics begins sampling the backing servers and the amp compute
// endpoints for the test about to run. Best-effort throughout: a sampler that
// cannot start is logged and skipped, never returned as an error, because
// observability must not decide whether a test passes.
func (e *Env) StartServerMetrics(ctx context.Context, opts *core.Config) {
	if e.kube == nil || opts == nil {
		return
	}
	cfg := metrics.FromConfig(opts)
	if !cfg.Enabled {
		return
	}
	e.samplers.mu.Lock()
	defer e.samplers.mu.Unlock()
	// A shared environment serves co-tenant tests; the first one to arrive starts
	// the samplers and the rest ride along, so the pods are scraped once.
	if e.samplers.backing != nil || e.samplers.compute != nil {
		return
	}

	ns := e.backingNamespace()
	s := metrics.New(e.kube.InNamespace(ns), cfg).WithLogf(e.logf)
	if err := s.Start(ctx); err != nil {
		e.log("sharedk8s: backing metrics not sampled in namespace %q (best-effort): %v", ns, err)
	} else {
		e.samplers.backing = s
	}

	if sel := e.endpointSelector(); sel != "" {
		c := metrics.NewEndpoint(e.kube, cfg, sel).WithLogf(e.logf)
		if err := c.Start(ctx); err != nil {
			e.log("sharedk8s: endpoint metrics not sampled (best-effort): %v", err)
		} else {
			e.samplers.compute = c
		}
	}
}

// DrainServerMetrics stops both samplers and hands over what they collected.
func (e *Env) DrainServerMetrics(emit func(core.Metric)) {
	e.samplers.mu.Lock()
	backing, compute := e.samplers.backing, e.samplers.compute
	e.samplers.backing, e.samplers.compute = nil, nil
	e.samplers.mu.Unlock()
	for _, s := range []*metrics.Sampler{backing, compute} {
		if s == nil {
			continue
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
}

// backingNamespace is where the YugabyteDB serving this control plane actually
// runs: its own namespace for an external backing that told us (a co-located
// ybk8s deploy, a provisioned YBA universe), else the amp namespace, which is
// where the built-in yugabyted lives.
func (e *Env) backingNamespace() string {
	if e.prov != nil {
		if ns := e.prov.ampCfg.Backing.KubeNamespace; ns != "" {
			return ns
		}
	}
	return e.kube.Namespace()
}

// endpointSelector matches this test's compute endpoint pods. Empty when the
// environment has no endpoint of its own yet.
func (e *Env) endpointSelector() string {
	if e.endpointID == "" {
		return ""
	}
	return "app.kubernetes.io/instance=" + ampstack.EndpointWorkloadName(e.endpointID)
}
