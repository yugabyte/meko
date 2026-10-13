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

// Within-stack iterations agree to ~1%; stack-to-stack spread was 20%+ before
// the stacks were pinned and sized Guaranteed (perf.isolation).

type benchWorkloadSpec struct {
	testName     string
	runStep      string
	podPrefix    string
	attachPrefix string
	noTPSMsg     string
	deliver      func(ctx context.Context, t *core.T, cfg perfConfig, podExec exec.RemoteExecutor) error
	// backing, when set, stands the store up instead of the co-located ybk8s
	// deploy: it fills r.ampBacking (and r.kube when the store has a namespace
	// of its own) and registers its teardown on r.td.
	backing func(ctx context.Context, r *benchProvision) error
	// ampOptions configures the amp control plane when the backing shape also
	// reaches it — the multi-region tests derive one config for both. nil reads
	// the run's own options.
	ampOptions *core.Config
	// endpointPgSettings are extra Postgres settings for the measured endpoint.
	endpointPgSettings map[string]string
	mkWorkload         workloadFactory
	specs              func(BenchResult) []metricSpec
	datapoints         func(BenchResult) (gated, info []core.Datapoint)
}

func runBenchProvisioned(t *core.T, spec benchWorkloadSpec, cfg perfConfig, bc BenchConfig, id Identity) error {
	gates := benchGatesFromConfig(t.Options)
	hcfg := historyConfigFromConfig(t.Options)
	version := chartVersion(t.Options)
	logf := t.Masker().Logf(log.Printf)
	id = id.forLiveArm(t.Options)
	t.SetPerfIdentity(id.core())

	// Live baseline: the same workload on concurrent stacks from a prior release; best-effort — unresolvable falls back to the history gate.
	baselineTag := resolveLiveArmTag(t, logf)
	var baseAmp ampstack.Config
	if baselineTag != "" {
		baseAmp = ampstack.FromConfig(t.Options.Derive(baselineOverrides(baselineTag)))
		logf("perf: live baseline arm: %d stack(s) on release %s, concurrent with the run's own", cfg.Provisions, baselineTag)
	}

	// Provisions run concurrently: each is its own universe + amp stack + client
	// pod, namespaced by the p<N> / b<N> suffix, so N iterations cost one wall clock.
	provisions := cfg.Provisions
	arms := provisions
	if baselineTag != "" {
		arms *= 2
	}
	// All arms provision, then start measuring TOGETHER: without the barrier the
	// slower-provisioning arm benchmarks alone at the tail and scores contention-
	// free numbers the other arm never saw.
	barrier := newStartBarrier(arms)
	rate := newRateBarrier(arms)
	provResults := make([]BenchResult, provisions)
	provErrs := make([]error, provisions)
	baseResults := make([]BenchResult, provisions)
	baseErrs := make([]error, provisions)
	zones := armZones(t.Context, t, cfg, logf)
	var wg sync.WaitGroup
	for i := 1; i <= provisions; i++ {
		zcfg := cfg.inZone(zoneFor(zones, i-1))
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			provResults[i-1], provErrs[i-1] = runOneProvision(t, spec, zcfg, bc, provisionArm{n: i, of: provisions, barrier: barrier, rate: rate}, logf)
		}(i)
		if baselineTag == "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			baseResults[i-1], baseErrs[i-1] = runOneProvision(t, spec, zcfg, bc, provisionArm{n: i, of: provisions, baseline: baselineTag, amp: &baseAmp, barrier: barrier, rate: rate}, logf)
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
	baselineUp := baselineTag != ""
	if baselineUp {
		if err := errors.Join(baseErrs...); err != nil {
			logf("perf: WARN live baseline arm failed (%v) — gating on history instead", err)
			baselineUp = false
		}
	}

	return t.Step("gate_perf", func(s *core.StepHandle) error {
		agg := aggregateProvisions(provResults)
		gated, info := spec.datapoints(agg)
		var lbErr error
		if baselineUp {
			baseAgg := aggregateProvisions(baseResults)
			baseGated, _ := spec.datapoints(baseAgg)
			// A benchmark gates 2-4 datapoints, so the quorum the ten-datapoint
			// suites need would let a lone throughput drop pass.
			extra, gerr := liveGate(t, s, spec.testName, id, gated, baseGated, nil, nil,
				provisionPairs(spec, provResults, baseResults), baselineTag, liveGateOpts{MinViolations: 1, NoHistoryBand: true}, logf)
			lbErr = gerr
			info = append(info, extra...)
		}
		// With a live verdict the history comparison is informational only.
		histErr := emitAndGateHistory(t, cfg, version, spec.testName, gated, info, id, !baselineUp, logf)
		emitHistoryTrend(t, hcfg, spec.testName, version, agg, id, logf)
		return errors.Join(append([]error{lbErr, histErr}, shapeErrs...)...)
	})
}

// provisionPairs pairs provision i of the run with provision i of the baseline
// arm, which ran beside it, and returns each gated datapoint's per-pair deltas.
func provisionPairs(spec benchWorkloadSpec, cur, base []BenchResult) map[string][]float64 {
	out := map[string][]float64{}
	for i := range cur {
		if i >= len(base) {
			break
		}
		cg, _ := spec.datapoints(cur[i])
		bg, _ := spec.datapoints(base[i])
		ref := map[string]core.Datapoint{}
		for _, dp := range bg {
			ref[dp.Name] = dp
		}
		for _, dp := range cg {
			if r, ok := ref[dp.Name]; ok {
				if d, ok := regressionPct(r.Value, dp.Value, dp.Direction); ok && dp.Value > 0 {
					out[dp.Name] = append(out[dp.Name], d)
				}
			}
		}
	}
	return out
}

func verdictWord(err error) string {
	if err != nil {
		return "fail"
	}
	return "pass"
}

// One concurrent stack: the run's own p<N>, or the live-baseline release's b<N>.
type provisionArm struct {
	n, of    int
	baseline string
	amp      *ampstack.Config
	barrier  *startBarrier
	rate     *rateBarrier
}

// startBarrier releases every arm's benchmark only when ALL arms have arrived —
// an arm that dies earlier still arrives (deferred), so nobody deadlocks.
type startBarrier struct {
	wg   sync.WaitGroup
	done chan struct{}
}

func newStartBarrier(n int) *startBarrier {
	b := &startBarrier{done: make(chan struct{})}
	b.wg.Add(n)
	go func() { b.wg.Wait(); close(b.done) }()
	return b
}

func (b *startBarrier) wait(ctx context.Context) error {
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a provisionArm) tag() string {
	if a.baseline != "" {
		return fmt.Sprintf("b%d", a.n)
	}
	if a.of > 1 {
		return fmt.Sprintf("p%d", a.n)
	}
	return ""
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

	amp  *ampstack.Config
	info ybk8s.Info
	kube *k8s.Kubectl
	// ampBacking is the store a spec.backing hook provisioned; nil on the
	// ybk8s path, where it derives from info.
	ampBacking *ampstack.Backing
	endpoint   ampEndpoint
	podExec    exec.RemoteExecutor
	sampler    *metrics.Sampler
	epSampler  *metrics.Sampler

	benchResult BenchResult
}

// Each provision tears its own stack down on return.
func runOneProvision(t *core.T, spec benchWorkloadSpec, cfg perfConfig, bc BenchConfig, arm provisionArm, logf func(string, ...any)) (_ BenchResult, err error) {
	r := &benchProvision{
		t: t, spec: spec, cfg: cfg, bc: bc,
		sfx:  t.ResourceSuffix(),
		amp:  arm.amp,
		td:   &teardownStack{},
		logf: logf,
	}
	if r.tag = arm.tag(); r.tag != "" {
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

	var arriveOnce sync.Once
	arrive := func() {
		if arm.barrier != nil {
			arriveOnce.Do(arm.barrier.wg.Done)
		}
	}
	defer arrive()
	if arm.barrier != nil {
		r.bc.StartGate = func(ctx context.Context) error {
			arrive()
			return arm.barrier.wait(ctx)
		}
	}
	var rateOnce sync.Once
	proposeRate := func(v float64) {
		if arm.rate != nil {
			rateOnce.Do(func() { arm.rate.arrive(v) })
		}
	}
	defer proposeRate(0)
	if arm.rate != nil {
		r.bc.RateGate = func(ctx context.Context, v float64) (float64, error) {
			proposeRate(v)
			return arm.rate.wait(ctx)
		}
	}

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
			if arm.of > 1 {
				s.Set("provision", arm.n)
			}
			if arm.baseline != "" {
				s.Set("baseline", arm.baseline)
			}
			s.Set("node_selector", orNone(cfg.ArmNodeSelector))
			return fn(t.Context)
		}); err != nil {
			return r.benchResult, err
		}
	}
	return r.benchResult, nil
}

func (r *benchProvision) provisionBacking(ctx context.Context) error {
	if r.spec.backing != nil {
		return r.spec.backing(ctx, r)
	}
	info, kube, err := deployYBBackingCfg(ctx, r.t, r.cfg.isolateBacking(clampBackingRF(ybk8s.FromConfig(r.t.Options), r.cfg.RF)), r.sfx, r.td, r.logf)
	if err != nil {
		return err
	}
	r.info = info
	r.kube = kube
	return nil
}

func (r *benchProvision) waitReady(ctx context.Context) error {
	if r.ampBacking != nil {
		// A provisioned universe is handed over healthy, and its tservers are
		// not pods this process could sample.
		return nil
	}
	if err := waitBackingYSQL(ctx, r.kube, r.info, r.cfg.RF, r.cfg.ReadyTimeout, r.sink, r.logf); err != nil {
		return err
	}
	if r.amp == nil {
		r.sampler = startMetricsSampler(ctx, r.t, r.kube, r.td, r.logf)
	} else {
		loadOnlySamplers(ctx, r.t, r.kube, nil, r.td, r.logf)
	}
	return nil
}

func (r *benchProvision) provisionEndpoint(ctx context.Context) error {
	ampOpts := r.t.Options
	if r.spec.ampOptions != nil {
		ampOpts = r.spec.ampOptions
	}
	amp := ampstack.FromConfig(ampOpts)
	if r.amp != nil {
		amp = *r.amp
	}
	amp = r.cfg.isolateEndpoints(amp)
	ybCfg := ybk8s.FromConfig(r.t.Options)
	var ep ampEndpoint
	var err error
	if r.ampBacking != nil {
		ep, err = provisionAmpEndpointOn(ctx, r.t, amp, ybCfg.KubeContext, ybCfg.Kubeconfig, *r.ampBacking, r.sfx, perfEndpointSpec{PgSettings: r.spec.endpointPgSettings}, r.sink, r.td, r.logf)
	} else {
		ep, err = provisionAmpEndpointOn(ctx, r.t, amp, ybCfg.KubeContext, ybCfg.Kubeconfig, ampBacking(r.info), r.sfx, perfEndpointSpec{PgSettings: r.spec.endpointPgSettings}, r.sink, r.td, r.logf)
	}
	if err != nil {
		return err
	}
	r.endpoint = ep
	if r.kube == nil {
		// No namespace of the store's own: the client pod runs beside amp.
		r.kube = ep.Kube
	}
	if r.amp == nil {
		r.epSampler = startEndpointSampler(ctx, r.t, ep, r.td, r.logf)
	} else {
		loadOnlySamplers(ctx, r.t, nil, &ep, r.td, r.logf)
	}
	return nil
}

func (r *benchProvision) deployClientPod(ctx context.Context) error {
	podExec, err := startShapedClientPod(ctx, r.kube, r.sink, r.td, r.spec.podPrefix+"-"+r.sfx, r.cfg.ClientImage, r.cfg.ClientPullSecret, r.cfg.clientShape())
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
	if res.FixedErr != nil {
		r.logf("perf: WARN %s fixed-rate window failed, gating the saturated run only: %v", orNone(r.tag), res.FixedErr)
	}
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
	if r.amp == nil {
		emitBenchChartsLabeled(r.t, res, r.tag)
		emitMetricSeries(r.t, r.sampler)
		emitMetricSeries(r.t, r.epSampler)
	}
	return nil
}
