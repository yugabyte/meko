// The same amp stack, the same benchmark, under the network of a multi-AZ and
// a multi-region deployment. The universe the perf tier provisions sits on one
// GKE cluster, so its tservers are a fraction of a millisecond apart; a real
// multi-region universe (us-east-2 / us-east-1 / us-west-2) has 12 and 47 ms
// between them, and the measured cost was a 9x slower INSERT (Slack
// C0BTA2Q0290, 2026-09-17). This test injects that latency into the pods'
// own network namespaces (apps/netshape) so the number is reproduced daily,
// on one cluster, and a regression in how amp behaves under it is caught.
//
// Sites: tserver N is site N, and the endpoint is in the topology's own site
// (site 0 by default, beside tserver-0). Every injected round trip is
// measured with ping from inside the pod before a benchmark trusts it: a
// shape that did not bite fails the step, never the product.
//
// Leaders are not preferred anywhere: a helm-deployed YB-in-k8s puts every
// tserver in one placement, so a real deployment's leader affinity to the amp
// region cannot be modelled here and the multi-region numbers are the
// no-preference case, which is the pessimistic one.
package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/netshape"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/sysbench"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	testNameLatencyTopology = "perf/latency_topology"
	latencyTopologyPrefix   = "latency_topology."
	latencyTopologyHarness  = "1"
	// latencyBaselineName is the unshaped run every topology is a ratio of.
	latencyBaselineName = "baseline"
	// A ratio carries the unshaped baseline's own run-to-run swing, so a change
	// under 10% of it is not the topology's.
	latencyRatioFloor = 0.10
)

// The perf.latency_topology.* load shape, declared like every sibling's
// (DefaultSysbenchScript and friends) so configs/default.yaml and the code
// cannot drift apart unnoticed.
const (
	DefaultLatencyWorkload    = "sysbench"
	DefaultLatencyHelperImage = "nicolaka/netshoot:v0.13"
	DefaultLatencyDevice      = "eth0"
	DefaultLatencyVerifyPings = 10
	DefaultLatencyTolerance   = 0.3
)

type latencyTopologyParams struct {
	Workload    string
	HelperImage string
	Device      string
	Topologies  []netshape.Topology
	VerifyPings int
	Tolerance   float64
}

func latencyTopologyParamsFromConfig(c *core.Config) (latencyTopologyParams, error) {
	tps, err := netshape.ParseTopologies(c.GetStringSlice("perf.latency_topology.topologies", nil))
	if err != nil {
		return latencyTopologyParams{}, err
	}
	if len(tps) == 0 {
		return latencyTopologyParams{}, errors.New("perf.latency_topology.topologies is empty: nothing to compare the unshaped run with")
	}
	for _, tp := range tps {
		if tp.Name == latencyBaselineName {
			return latencyTopologyParams{}, fmt.Errorf("topology name %q is reserved for the unshaped run", latencyBaselineName)
		}
	}
	p := latencyTopologyParams{
		Workload:    c.GetString("perf.latency_topology.workload", DefaultLatencyWorkload),
		HelperImage: c.GetString("perf.latency_topology.helper_image", DefaultLatencyHelperImage),
		Device:      c.GetString("perf.latency_topology.device", DefaultLatencyDevice),
		Topologies:  tps,
		VerifyPings: c.GetInt("perf.latency_topology.verify_pings", DefaultLatencyVerifyPings),
		Tolerance:   c.GetFloat("perf.latency_topology.tolerance", DefaultLatencyTolerance),
	}
	if p.Workload != "sysbench" && p.Workload != "pgbench" {
		return latencyTopologyParams{}, fmt.Errorf("perf.latency_topology.workload %q: want sysbench or pgbench", p.Workload)
	}
	return p, nil
}

// occupiedSites is which sites hold a pod under tp: one tserver per site below
// rf, plus wherever tp puts the endpoint.
func occupiedSites(tp netshape.Topology, rf int) map[int]bool {
	occupied := map[int]bool{tp.EndpointSite: true}
	for i := 0; i < rf; i++ {
		occupied[i] = true
	}
	return occupied
}

func init() {
	core.Register(core.Test{
		Name:        testNameLatencyTopology,
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe},
		Tags:        []string{"perf"},
		OwningTeam:  core.TeamCompute,
		Description: "Provision the perf tier's RF3 YB-in-k8s backing, amp control plane, compute endpoint and benchmark client once, benchmark unshaped, then re-benchmark the same loaded dataset under each perf.latency_topology.topologies entry: tc netem delay injected into every tserver's and the endpoint's own network namespace so the sites are the configured round trips apart (multi-AZ, multi-region, the endpoint remote from its universe), each round trip verified with ping before the run counts. sysbench by default, pgbench by perf.latency_topology.workload. Gates every topology's tps and latency against portal history and reports each as a ratio of the unshaped run.",
		Params:      []string{"perf.latency_topology", "perf.sysbench", "perf.pgbench", "perf.bench", "perf.gate", "apps.ybk8s", "env.ampstack"},
		Timeout:     3 * time.Hour,
		Run:         runLatencyTopology,
	})
}

// latencyWorkload is the benchmark driven under every topology: how it gets
// into the client pod, how it binds to the endpoint, and the load it declares.
type latencyWorkload struct {
	deliver func(ctx context.Context, t *core.T, cfg perfConfig, podExec exec.RemoteExecutor) error
	mk      workloadFactory
	load    LoadFields
}

func latencyWorkloadFromConfig(c *core.Config, name string, bc BenchConfig, cfg perfConfig) latencyWorkload {
	if name == "pgbench" {
		params := pgbenchParamsFromConfig(c)
		return latencyWorkload{
			deliver: deliverPgbenchArtifact,
			mk:      func(conn pg.ConnInfo) Workload { return pgbenchWorkloadFor(params, conn) },
			load:    loadFieldsPgbench(bc, cfg, params),
		}
	}
	params := sysbenchParamsFromConfig(c)
	return latencyWorkload{
		deliver: func(ctx context.Context, _ *core.T, _ perfConfig, podExec exec.RemoteExecutor) error {
			return sysbench.EnsureInstalled(ctx, podExec)
		},
		mk:   func(conn pg.ConnInfo) Workload { return sysbenchWorkloadFor(params, conn) },
		load: loadFieldsSysbench(bc, cfg, params),
	}
}

type latencyRun struct {
	t      *core.T
	cfg    perfConfig
	bc     BenchConfig
	params latencyTopologyParams
	wl     latencyWorkload
	td     *teardownStack
	logf   func(string, ...any)
	sink   attachSink
	sfx    string

	info           ybk8s.Info
	kube           *k8s.Kubectl
	endpoint       ampEndpoint
	podExec        exec.RemoteExecutor
	fleet          *netshape.Fleet
	endpointMember *netshape.Member
	results        map[string]BenchResult
	order          []string
}

func runLatencyTopology(t *core.T) (err error) {
	cfg := perfFromConfig(t.Options)
	if reason, ok := perfGate(t.Options, cfg); !ok {
		t.Skip(reason)
		return nil
	}
	if reason, ok := gateNoDoubleProvision(t.Env); !ok {
		t.Skip(reason)
		return nil
	}
	if reason, ok := ampEndpointGate(t.Options); !ok {
		t.Skip(reason)
		return nil
	}
	params, err := latencyTopologyParamsFromConfig(t.Options)
	if err != nil {
		return err
	}
	for _, tp := range params.Topologies {
		if err := tp.CheckSites(occupiedSites(tp, cfg.RF)); err != nil {
			return fmt.Errorf("%w: this stack has sites 0..%d, one per tserver (RF %d) plus site %d for an endpoint of its own", err, cfg.RF, cfg.RF, cfg.RF)
		}
	}
	bc := benchConfigFromConfig(t.Options)
	if bc.SkipLoad || bc.LoadOnly {
		return errors.New("perf.bench.skip_load and load_only do not apply: this test loads once and measures every topology on that dataset")
	}
	logf := t.Masker().Logf(log.Printf)
	r := &latencyRun{
		t: t, cfg: cfg, bc: bc, params: params,
		wl: latencyWorkloadFromConfig(t.Options, params.Workload, bc, cfg),
		td: &teardownStack{}, logf: logf, sfx: t.ResourceSuffix(), results: map[string]BenchResult{},
	}
	r.sink = attachSink{t: t, logf: logf}
	t.SetPerfIdentity(r.identity().core())

	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
		defer cancel()
		if tdErr := r.td.run(tctx, logf); tdErr != nil && err == nil {
			err = fmt.Errorf("teardown: %w", tdErr)
		}
	}()

	phases := []struct {
		name string
		fn   func(context.Context, *core.StepHandle) error
	}{
		{"provision_backing", r.provisionBacking},
		{"wait_backing_ready", r.waitReady},
		{"provision_amp_endpoint", r.provisionEndpoint},
		{"deploy_client_pod", r.deployClientPod},
		{"attach_sites", r.attachSites},
		{"bench_" + latencyBaselineName, func(ctx context.Context, s *core.StepHandle) error {
			return r.bench(ctx, s, latencyBaselineName, false)
		}},
	}
	for _, p := range phases {
		fn := p.fn
		if err := t.Step(p.name, func(s *core.StepHandle) error { return fn(t.Context, s) }); err != nil {
			return err
		}
	}
	for _, tp := range params.Topologies {
		tp := tp
		if err := t.Step("shape_"+tp.Name, func(s *core.StepHandle) error { return r.shape(t.Context, s, tp) }); err != nil {
			_ = r.fleet.Clear(t.Context)
			return err
		}
		benchErr := t.Step("bench_"+tp.Name, func(s *core.StepHandle) error { return r.bench(t.Context, s, tp.Name, true) })
		if err := t.Step("unshape_"+tp.Name, func(s *core.StepHandle) error { return r.unshape(t.Context, s, tp) }); err != nil {
			return errors.Join(benchErr, err)
		}
		if benchErr != nil {
			return benchErr
		}
	}
	return t.Step("gate_perf", r.gate)
}

func (r *latencyRun) identity() Identity {
	load := r.wl.load
	// The site matrix is part of what the numbers mean, so it keys the history.
	load.Harness = latencyTopologyHarness + "-" + netshape.Signature(r.params.Topologies)
	return identityFromConfig(r.t.Options, r.params.Workload, frameworkCommit(), load)
}

func (r *latencyRun) provisionBacking(ctx context.Context, _ *core.StepHandle) error {
	info, kube, err := deployYBBacking(ctx, r.t, r.cfg.RF, r.sfx, r.td, r.logf)
	if err != nil {
		return err
	}
	r.info, r.kube = info, kube
	return nil
}

func (r *latencyRun) waitReady(ctx context.Context, _ *core.StepHandle) error {
	if err := waitBackingYSQL(ctx, r.kube, r.info, r.cfg.RF, r.cfg.ReadyTimeout, r.sink, r.logf); err != nil {
		return err
	}
	startMetricsSampler(ctx, r.t, r.kube, r.td, r.logf)
	return nil
}

func (r *latencyRun) provisionEndpoint(ctx context.Context, _ *core.StepHandle) error {
	ep, err := provisionAmpEndpoint(ctx, r.t, ampstack.FromConfig(r.t.Options), ybk8s.FromConfig(r.t.Options), r.info, r.sfx, r.sink, r.td, r.logf)
	if err != nil {
		return err
	}
	r.endpoint = ep
	startEndpointSampler(ctx, r.t, ep, r.td, r.logf)
	return nil
}

func (r *latencyRun) deployClientPod(ctx context.Context, _ *core.StepHandle) error {
	podExec, err := startPerfClientPod(ctx, r.kube, r.sink, r.td, "ampqa-lattopo-"+r.sfx, r.cfg.ClientImage, r.cfg.ClientPullSecret)
	if err != nil {
		return err
	}
	r.podExec = podExec
	return r.wl.deliver(ctx, r.t, r.cfg, podExec)
}

// attachSites resolves the tserver and endpoint pods, stands a netshape helper
// beside each, and records the cluster's own round trips before any shaping.
func (r *latencyRun) attachSites(ctx context.Context, s *core.StepHandle) error {
	tservers, err := podsWithIPs(ctx, r.kube, tserverSelector)
	if err != nil {
		return err
	}
	if len(tservers) != r.cfg.RF {
		return fmt.Errorf("backing runs %d tserver pod(s), want RF %d, one per site", len(tservers), r.cfg.RF)
	}
	eps, err := podsWithIPs(ctx, r.endpoint.Kube, r.endpoint.podSelector())
	if err != nil {
		return err
	}
	if len(eps) != 1 {
		return fmt.Errorf("endpoint %s has %d pod(s), want exactly one", r.endpoint.EndpointID, len(eps))
	}
	r.fleet = &netshape.Fleet{Device: r.params.Device, Pings: r.params.VerifyPings, Tolerance: r.params.Tolerance}
	for i, p := range tservers {
		r.fleet.Members = append(r.fleet.Members, &netshape.Member{Label: p.Name, Pod: p.Name, IP: p.IP, Site: i})
	}
	r.endpointMember = &netshape.Member{Label: "endpoint", Pod: eps[0].Name, IP: eps[0].IP}
	r.fleet.Members = append(r.fleet.Members, r.endpointMember)
	fleet := r.fleet
	r.td.push("delete netshape helpers", func(ctx context.Context) error {
		fleet.Close(ctx)
		return nil
	})
	for i, m := range r.fleet.Members {
		kube := r.kube
		if m == r.endpointMember {
			kube = r.endpoint.Kube
		}
		h, err := netshape.Attach(ctx, kube, m.Pod, fmt.Sprintf("%s-%d", r.sfx, i), r.params.HelperImage)
		if err != nil {
			return core.EnvFail("attach a netshape helper to %s: %w", m.Pod, err)
		}
		m.Helper = h
		// The endpoint's site is a property of each topology, assigned in
		// shape, so only the tservers can be named by site here.
		key := m.Label
		if m != r.endpointMember {
			key = fmt.Sprintf("site_%d", m.Site)
		}
		s.Set(key, m.Pod+" "+m.IP)
	}
	ms, err := r.fleet.Baseline(ctx)
	if err != nil {
		return core.EnvFail("%w", err)
	}
	for _, m := range ms {
		s.Set(fmt.Sprintf("native_rtt_%s_%s", m.From, m.To), m.Got.Round(10*time.Microsecond).String())
		r.t.Metric(fmt.Sprintf("native_rtt_ms_%s_%s", m.From, m.To), float64(m.Got)/float64(time.Millisecond), "ms")
	}
	return nil
}

type namedIP struct{ Name, IP string }

// podsWithIPs lists the pods matching selector with their IPs, by name.
func podsWithIPs(ctx context.Context, kube *k8s.Kubectl, selector string) ([]namedIP, error) {
	out, err := kube.GetSelector(ctx, "pod", selector, `{range .items[*]}{.metadata.name} {.status.podIP}{"\n"}{end}`)
	if err != nil {
		return nil, fmt.Errorf("list pods %q in %s: %w", selector, kube.Namespace(), err)
	}
	var pods []namedIP
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pods = append(pods, namedIP{Name: f[0], IP: f[1]})
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	if len(pods) == 0 {
		return nil, fmt.Errorf("no pod with an IP matched %q in %s", selector, kube.Namespace())
	}
	return pods, nil
}

// shape applies tp to every pod and measures each separated pair from inside
// the pods, failing on one that did not take.
func (r *latencyRun) shape(ctx context.Context, s *core.StepHandle, tp netshape.Topology) error {
	r.endpointMember.Site = tp.EndpointSite
	if err := r.fleet.Apply(ctx, tp); err != nil {
		return core.EnvFail("%w", err)
	}
	s.Set("endpoint_site", tp.EndpointSite)
	ms, err := r.fleet.Verify(ctx, tp)
	for _, m := range ms {
		key := fmt.Sprintf("rtt_%s_%s", m.From, m.To)
		s.Set(key, fmt.Sprintf("%s (want %s native + %s injected)", m.Got.Round(10*time.Microsecond), m.Native.Round(10*time.Microsecond), m.Want))
		r.t.Metric(fmt.Sprintf("%s_%s_ms", tp.Name, key), float64(m.Got)/float64(time.Millisecond), "ms")
	}
	if err != nil {
		return core.EnvFail("the injected latency did not take, so no benchmark under %q would measure it: %w", tp.Name, err)
	}
	return nil
}

// unshape clears every pod and holds every pair tp delayed back to its native
// cost, so the next topology starts from a clean stack.
func (r *latencyRun) unshape(ctx context.Context, s *core.StepHandle, tp netshape.Topology) error {
	if err := r.fleet.Clear(ctx); err != nil {
		return core.EnvFail("clear the shaping: %w", err)
	}
	ms, err := r.fleet.VerifyCleared(ctx, tp)
	for _, m := range ms {
		s.Set(fmt.Sprintf("rtt_after_clear_%s_%s", m.From, m.To),
			fmt.Sprintf("%s (native %s)", m.Got.Round(10*time.Microsecond), m.Native.Round(10*time.Microsecond)))
	}
	if err != nil {
		return core.EnvFail("%w", err)
	}
	return nil
}

// bench drives the harness once for name. The first call loads the dataset;
// every later one measures the same tables.
func (r *latencyRun) bench(ctx context.Context, s *core.StepHandle, name string, skipLoad bool) error {
	bc := r.bc
	bc.SkipLoad = skipLoad
	res, err := RunBenchmark(ctx, r.podExec, r.wl.mk(r.endpoint.Conn), bc)
	attachBenchSamples(r.sink, res, r.params.Workload+"-"+name)
	if err != nil {
		return err
	}
	if len(res.Kept) == 0 || res.TPS.Median == 0 {
		return fmt.Errorf("%s under %q produced no tps: parse of its output failed (see attached %s-%s-iter-*.out)", r.params.Workload, name, r.params.Workload, name)
	}
	r.results[name] = res
	r.order = append(r.order, name)
	emitBenchChartsLabeled(r.t, res, name)
	s.Set("tps", fmt.Sprintf("%.1f", res.TPS.Median))
	s.Set("latency_avg_ms", fmt.Sprintf("%.2f", res.LatencyAvg.Median))
	s.Set("latency_p95_ms", fmt.Sprintf("%.2f", res.LatencyP95.Median))
	if base, ok := r.results[latencyBaselineName]; ok && name != latencyBaselineName && base.TPS.Median > 0 {
		s.Set("tps_of_baseline", fmt.Sprintf("%.3f", res.TPS.Median/base.TPS.Median))
	}
	return nil
}

// gate gates BOTH each shaped topology's absolute numbers and its ratio to the
// unshaped baseline, because which of the two is stable depends on the
// topology, and the portal's band is per-datapoint MAD-derived so a noisy one
// widens itself rather than firing.
//
// Measured over the first four runs of the shipped set: a topology whose
// latency DOMINATES holds its absolute to 6-7% run to run (multi_region tps
// 6.3%, amp_cross_region 7.3%) while the unshaped baseline swings 33% with
// cluster CPU. So the ratio does NOT cancel that spread where the two arms sit
// in different regimes — it divides a stable shaped number by a noisy
// baseline and lands at 26-28%. Where latency is negligible beside CPU the
// reverse holds: multi_az's absolute swings 35% and its ratio is 5.8%. Gating
// both leaves every topology with one tight datapoint.
//
// The unshaped baseline's own absolutes stay informational: perf/sysbench and
// perf/pgbench already gate that number.
//
// p95 is informational: multi_region's flips between ~127 and ~200 ms from one
// run to the next on the same build.
//
// Each topology's per-iteration shape is also gated in-run (benchShapeGate),
// which is what asserts something while the portal history is still short.
func (r *latencyRun) gate(*core.StepHandle) error {
	base := r.results[latencyBaselineName]
	gates := benchGatesFromConfig(r.t.Options)
	var gated, info []core.Datapoint
	var shapeErrs []error
	for _, name := range r.order {
		res := r.results[name]
		specs := latencyTopologySpecs(name, res, r.params.Workload == "pgbench")
		abs, _ := splitDatapoints(specs)
		if err := benchShapeGate(res, specs, gates); err != nil {
			shapeErrs = append(shapeErrs, fmt.Errorf("topology %q: %w", name, err))
		}
		if name == latencyBaselineName || base.TPS.Median <= 0 || base.LatencyAvg.Median <= 0 {
			info = append(info, abs...)
			continue
		}
		for _, dp := range abs {
			if strings.HasSuffix(dp.Name, ".latency_p95") {
				info = append(info, dp)
			} else {
				gated = append(gated, dp)
			}
		}
		p := latencyTopologyPrefix + name + "."
		tpsRatio := res.TPS.Median / base.TPS.Median
		latRatio := res.LatencyAvg.Median / base.LatencyAvg.Median
		gated = append(gated,
			core.Datapoint{Name: p + "tps_of_baseline", Value: tpsRatio, Unit: "x", Direction: core.HigherIsBetter, MinDelta: latencyRatioFloor * tpsRatio},
			core.Datapoint{Name: p + "latency_avg_of_baseline", Value: latRatio, Unit: "x", Direction: core.LowerIsBetter, MinDelta: latencyRatioFloor * latRatio},
		)
	}
	table := latencyTopologyTable(r.order, r.results)
	r.logf("%s", table)
	r.t.AttachBytes("latency-topology-summary.txt", "log", []byte(table))
	histErr := emitAndGateHistory(r.t, r.cfg, chartVersion(r.t.Options), testNameLatencyTopology, gated, info, r.identity(), true, r.logf)
	return errors.Join(append(shapeErrs, histErr)...)
}

// latencyTopologySpecs is one topology's metric set, carrying the accessors the
// shape gates read (both drivers report periodic progress). pgbench reports no
// percentile, so its p95 is not a datapoint.
func latencyTopologySpecs(name string, res BenchResult, noP95 bool) []metricSpec {
	p := latencyTopologyPrefix + name + "."
	specs := []metricSpec{
		{Name: p + "tps", Unit: "tps", Dir: core.HigherIsBetter, Stats: res.TPS,
			FromSample: sampleTPS, FromPoint: pointTPS},
		{Name: p + "latency_avg", Unit: "ms", Dir: core.LowerIsBetter, Stats: res.LatencyAvg,
			FromSample: sampleLatencyAvg, FromPoint: pointLatency},
	}
	if !noP95 {
		specs = append(specs, metricSpec{Name: p + "latency_p95", Unit: "ms", Dir: core.LowerIsBetter, Stats: res.LatencyP95,
			FromSample: sampleLatencyP95})
	}
	return specs
}

// latencyTopologyTable is the one-screen summary: every topology beside the unshaped run.
func latencyTopologyTable(order []string, results map[string]BenchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-22s %10s %10s %10s %8s\n", "topology", "tps", "avg ms", "p95 ms", "tps/base")
	base := results[latencyBaselineName]
	for _, name := range order {
		res := results[name]
		ratio := "-"
		if base.TPS.Median > 0 {
			ratio = fmt.Sprintf("%.3f", res.TPS.Median/base.TPS.Median)
		}
		fmt.Fprintf(&b, "%-22s %10.1f %10.2f %10.2f %8s\n", name, res.TPS.Median, res.LatencyAvg.Median, res.LatencyP95.Median, ratio)
	}
	return b.String()
}
