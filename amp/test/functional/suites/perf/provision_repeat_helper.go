package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/metrics"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// Within-stack iterations agree to ~1%; stack-to-stack spread is 20%+.

type benchWorkloadSpec struct {
	testName     string
	runStep      string
	podPrefix    string
	attachPrefix string
	noTPSMsg     string
	deliver      func(ctx context.Context, t *core.T, cfg perfConfig, podExec exec.RemoteExecutor) error
	mkWorkload   workloadFactory
	specs        func(BenchResult) []metricSpec
	datapoints   func(BenchResult) (gated, info []core.Datapoint)
}

func runBenchProvisioned(t *core.T, spec benchWorkloadSpec, cfg perfConfig, bc BenchConfig, id Identity) error {
	gates := benchGatesFromConfig(t.Options)
	hcfg := historyConfigFromConfig(t.Options)
	version := chartVersion(t.Options)
	logf := t.Masker().Logf(log.Printf)
	t.SetPerfIdentity(id.core())

	// Provisions run concurrently: each is its own universe + amp stack + client
	// pod, namespaced by the p<N> suffix, so N iterations cost one wall clock.
	provisions := cfg.Provisions
	provResults := make([]BenchResult, provisions)
	provErrs := make([]error, provisions)
	var wg sync.WaitGroup
	for i := 1; i <= provisions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			provResults[i-1], provErrs[i-1] = runOneProvision(t, spec, cfg, bc, i, provisions, logf)
		}(i)
	}
	wg.Wait()
	if err := errors.Join(provErrs...); err != nil {
		return err
	}
	var shapeErrs []error
	for i, res := range provResults {
		if serr := benchShapeGate(res, spec.specs(res), gates); serr != nil {
			shapeErrs = append(shapeErrs, fmt.Errorf("provision %d/%d: %w", i+1, provisions, serr))
		}
	}

	return t.Step("gate_perf", func(*core.StepHandle) error {
		agg := aggregateProvisions(provResults)
		gated, info := spec.datapoints(agg)
		gateErr := emitAndGateAuto(t, cfg, version, spec.testName, gated, info, id, logf)
		emitHistoryTrend(t, hcfg, spec.testName, version, agg, id, logf)
		return errors.Join(append([]error{gateErr}, shapeErrs...)...)
	})
}

type benchProvision struct {
	t    *core.T
	spec benchWorkloadSpec
	cfg  perfConfig
	bc   BenchConfig
	sfx  string // per-provision suffix; namespaces and pods must not collide
	tag  string
	td   *teardownStack
	logf func(string, ...any)
	sink apps.ArtifactSink

	info      ybk8s.Info
	kube      *k8s.Kubectl
	endpoint  ampEndpoint
	podExec   exec.RemoteExecutor
	sampler   *metrics.Sampler
	epSampler *metrics.Sampler

	benchResult BenchResult
}

// Each provision tears its own stack down on return.
func runOneProvision(t *core.T, spec benchWorkloadSpec, cfg perfConfig, bc BenchConfig, prov, provisions int, logf func(string, ...any)) (_ BenchResult, err error) {
	r := &benchProvision{
		t: t, spec: spec, cfg: cfg, bc: bc,
		sfx:  t.ResourceSuffix(),
		td:   &teardownStack{},
		logf: logf,
	}
	if provisions > 1 {
		r.tag = fmt.Sprintf("p%d", prov)
		r.sfx += r.tag
	}
	r.sink = attachSink{t: t, logf: logf}

	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
		defer cancel()
		if tdErr := r.td.run(tctx, logf); tdErr != nil && err == nil {
			err = fmt.Errorf("teardown: %w", tdErr)
		}
	}()

	phases := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"provision_backing", r.provisionBacking},
		{"wait_backing_ready", r.waitReady},
		{"provision_amp_endpoint", r.provisionEndpoint},
		{"deploy_client_pod", r.deployClientPod},
		{spec.runStep, r.runBenchmark},
	}
	for _, p := range phases {
		fn := p.fn
		if err := t.Step(p.name, func(s *core.StepHandle) error {
			if provisions > 1 {
				s.Set("provision", prov)
			}
			return fn(t.Context)
		}); err != nil {
			return r.benchResult, err
		}
	}
	return r.benchResult, nil
}

func (r *benchProvision) provisionBacking(ctx context.Context) error {
	info, kube, err := deployYBBacking(ctx, r.t, r.cfg.RF, r.sfx, r.td, r.logf)
	if err != nil {
		return err
	}
	r.info = info
	r.kube = kube
	return nil
}

func (r *benchProvision) waitReady(ctx context.Context) error {
	if err := waitBackingYSQL(ctx, r.kube, r.info, r.cfg.RF, r.cfg.ReadyTimeout, r.sink, r.logf); err != nil {
		return err
	}
	r.sampler = startMetricsSampler(ctx, r.t, r.kube, r.td, r.logf)
	return nil
}

func (r *benchProvision) provisionEndpoint(ctx context.Context) error {
	ep, err := provisionAmpEndpoint(ctx, r.t, ampstack.FromConfig(r.t.Options), ybk8s.FromConfig(r.t.Options), r.info, r.sfx, r.sink, r.td, r.logf)
	if err != nil {
		return err
	}
	r.endpoint = ep
	r.epSampler = startEndpointSampler(ctx, r.t, ep, r.td, r.logf)
	return nil
}

func (r *benchProvision) deployClientPod(ctx context.Context) error {
	podExec, err := startPerfClientPod(ctx, r.kube, r.sink, r.td, r.spec.podPrefix+"-"+r.sfx, r.cfg.ClientImage, r.cfg.ClientPullSecret)
	if err != nil {
		return err
	}
	r.podExec = podExec
	if r.spec.deliver != nil {
		return r.spec.deliver(ctx, r.t, r.cfg, r.podExec)
	}
	return nil
}

func (r *benchProvision) runBenchmark(ctx context.Context) error {
	wl := r.spec.mkWorkload(r.endpoint.Conn)
	res, err := RunBenchmark(ctx, r.podExec, wl, r.bc)
	prefix := r.spec.attachPrefix
	if r.tag != "" {
		prefix += "-" + r.tag
	}
	attachBenchSamples(r.sink, res, prefix)
	if err != nil {
		return err
	}
	if len(res.Kept) == 0 || res.TPS.Median == 0 {
		return errors.New(r.spec.noTPSMsg)
	}
	r.benchResult = res
	emitBenchChartsLabeled(r.t, res, r.tag)
	emitMetricSeries(r.t, r.sampler)
	emitMetricSeries(r.t, r.epSampler)
	return nil
}
