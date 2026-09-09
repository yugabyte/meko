package perf

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/diag"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// The generic live-baseline arm, for the suites that measure the env's own
// stack (microbench, lifecycle). When a previous release is resolvable, the
// test provisions TWO stacks of its own — the current images and the baseline's
// — and measures both concurrently, so the verdict compares versions under the
// same cluster weather instead of a stored number from another day. The bench
// family grew this first (provision_repeat_helper.go); the arm here carries the
// same idea for workloads that are not client-pod benchmarks.

// provisionedHarness marks the load signature of an arm-provisioned run: the
// numbers move from the env's standing stack to a self-provisioned one, which
// is a different measurement, so the series must not continue the old one.
const provisionedHarness = "provisioned"

// resolveLiveArmTag decides whether this run gets a live baseline arm and which
// release it compares against. "" disables the arm, with the reason logged —
// the caller falls back to the portal history gate.
func resolveLiveArmTag(t *core.T, logf func(string, ...any)) string {
	lb := liveBaselineFromConfig(t.Options)
	if !lb.Enabled {
		return ""
	}
	tag, err := resolveBaselineTag(t.Context, lb, portalGateFromConfig(t.Options).URL, currentAmpTag(t.Options))
	switch {
	case err != nil:
		logf("perf: WARN live baseline unavailable (%v) — gating on history instead", err)
		return ""
	case tag != "" && tag == currentAmpTag(t.Options) && lb.AllowSameImage:
		logf("perf: A/A run — both arms on %s, so every delta reported is harness noise", tag)
		return tag
	case tag == "" || tag == currentAmpTag(t.Options):
		logf("perf: live baseline skipped (no prior release distinct from %q) — gating on history instead", orNone(currentAmpTag(t.Options)))
		return ""
	}
	return tag
}

// liveArm is one self-provisioned arm: its own YB backing, control plane, user
// and pool. It implements the ampMultitenantEnv capability set, so a suite body
// written against the env interface runs unchanged on either arm.
type liveArm struct {
	tag     string // the amp release this arm runs, "" for the current images
	sfx     string
	app     *ampstack.App
	kube    *k8s.Kubectl
	ns      string
	cl      *controller.Client
	poolID  string
	pgReady time.Duration
	td      *teardownStack
	ybKube  *k8s.Kubectl // the arm's own backing namespace

	mu  sync.Mutex
	pfs []*k8s.PortForward
}

// collectDiag dumps the arm's own namespace. Without it a suite body that fails
// against a provisioned arm leaves nothing but the env control plane's logs,
// which never touched the arm (run 5c88ed3c: three create_endpoint deploys hit
// the controller's 35m helm timeout with no controller log to say why).
// The run context is already cancelled in the case this exists for (the perf
// watchdog fires on a timeout overrun), so collection gets its own deadline.
func (a *liveArm) collectDiag(t *core.T, logf func(string, ...any)) {
	if a == nil || a.kube == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
	defer cancel()
	sink := attachSink{t: t, logf: logf}
	diag.CollectK8sDiagnostics(ctx, a.kube, a.ns, sink, logf)
	if a.ybKube != nil {
		diag.CollectK8sDiagnostics(ctx, a.ybKube, a.ybKube.Namespace(), sink, logf)
	}
}

func (a *liveArm) AmpController(context.Context) (*controller.Client, error) { return a.cl, nil }
func (a *liveArm) AmpPoolID() string                                         { return a.poolID }
func (a *liveArm) AmpKube() *k8s.Kubectl                                     { return a.kube }

// ConnectEndpoint mirrors sharedk8s's: wait for the compute workload, forward
// its Service, wait for its Postgres. Forwards are owned by the arm (closed at
// teardown); callers close only the returned client.
func (a *liveArm) ConnectEndpoint(ctx context.Context, ep controller.Endpoint) (*pg.Client, error) {
	if err := ampstack.WaitEndpointWorkloadReady(ctx, a.kube, ep.ID, a.pgReady); err != nil {
		return nil, fmt.Errorf("arm endpoint %s compute never became ready: %w", ep.ID, err)
	}
	pf, err := a.kube.PortForward(ctx, "svc/"+ampstack.EndpointWorkloadName(ep.ID), 0, addrPort(ep.Address, endpointComputePort))
	if err != nil {
		return nil, fmt.Errorf("port-forward arm endpoint %s: %w", ep.ID, err)
	}
	ci := pg.ConnInfo{Host: "127.0.0.1", Port: pf.LocalPort(), User: endpointComputeUser, Database: endpointComputeDB}
	if err := pg.WaitReady(ctx, ci, a.pgReady); err != nil {
		pf.Close()
		return nil, fmt.Errorf("arm endpoint %s postgres not ready: %w", ep.ID, err)
	}
	client, err := pg.Connect(ctx, ci)
	if err != nil {
		pf.Close()
		return nil, fmt.Errorf("connect arm endpoint %s: %w", ep.ID, err)
	}
	a.mu.Lock()
	a.pfs = append(a.pfs, pf)
	a.mu.Unlock()
	return client, nil
}

// endpointClient carves the arm's perf endpoint into the pool provisioning
// already created and connects to it.
func (a *liveArm) endpointClient(ctx context.Context) (*pg.Client, error) {
	ep, err := createPerfEndpointInPool(ctx, a.cl, a.poolID, a.sfx)
	if err != nil {
		return nil, err
	}
	return a.ConnectEndpoint(ctx, ep)
}

func (a *liveArm) teardown(logf func(string, ...any)) {
	a.mu.Lock()
	pfs := a.pfs
	a.pfs = nil
	a.mu.Unlock()
	for _, pf := range pfs {
		_ = pf.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
	defer cancel()
	_ = a.td.run(ctx, logf)
}

// provisionLiveArm stands one arm up: backing, control plane, user, pool. On
// error it has already torn down what it built. tag names the arm in logs and
// resource suffixes ("" = the current images).
func provisionLiveArm(ctx context.Context, t *core.T, cfg perfConfig, amp ampstack.Config, tag, sfx string, logf func(string, ...any)) (arm *liveArm, err error) {
	td := &teardownStack{}
	defer func() {
		if err != nil {
			tctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
			defer cancel()
			_ = td.run(tctx, logf)
		}
	}()
	sink := attachSink{t: t, logf: logf}

	ybBacking := clampBackingRF(ybk8s.FromConfig(t.Options), cfg.RF)
	if cfg.ArmNodeSelector != "" && ybBacking.NodeSelector == "" {
		ybBacking.NodeSelector = cfg.ArmNodeSelector
	}
	info, kube, err := deployYBBackingCfg(ctx, t, ybBacking, sfx, td, logf)
	if err != nil {
		return nil, err
	}
	if err := waitBackingYSQL(ctx, kube, info, cfg.RF, cfg.ReadyTimeout, sink, logf); err != nil {
		return nil, err
	}

	ybCfg := ybk8s.FromConfig(t.Options)
	if cfg.ArmNodeSelector != "" && amp.EndpointNodeSelector == "" {
		amp.EndpointNodeSelector = cfg.ArmNodeSelector
	}
	scfg := ampStackConfig(amp, ybCfg, info, sfx)
	app := ampstack.New(scfg).WithLogf(logf)
	if ybCfg.Kubeconfig != "" {
		app = app.WithKube(k8s.New(k8s.Config{Context: scfg.Context, Namespace: scfg.Namespace}).
			WithRunner(k8s.KubeconfigRunner(ybCfg.Kubeconfig)))
	}
	td.push("teardown arm control plane", func(ctx context.Context) error {
		_ = app.Close(ctx)
		return app.Teardown(ctx)
	})
	if _, err := app.Deploy(ctx); err != nil {
		diag.CollectK8sDiagnostics(ctx, app.Kube(), scfg.Namespace, sink, logf)
		return nil, fmt.Errorf("arm control plane %s deploy: %w", scfg.Namespace, err)
	}
	cl, url, err := app.ControllerClient(ctx)
	if err != nil {
		diag.CollectK8sDiagnostics(ctx, app.Kube(), scfg.Namespace, sink, logf)
		return nil, fmt.Errorf("arm control plane %s: reach controller: %w", scfg.Namespace, err)
	}
	user, err := cl.CreateUser(ctx, controller.CreateUserRequest{})
	if err != nil {
		return nil, fmt.Errorf("arm control plane %s: create user: %w", scfg.Namespace, err)
	}
	pool, err := cl.CreateStoragePool(ctx, controller.CreateStoragePoolRequest{Name: perfPoolName, OwnerUserID: user.ID})
	if err != nil {
		return nil, fmt.Errorf("arm control plane %s: create storage pool: %w", scfg.Namespace, err)
	}
	logf("perf: arm %s ready in %s (controller %s)", orNone(tag), scfg.Namespace, url)
	return &liveArm{
		tag: tag, sfx: sfx, app: app, kube: app.Kube(), ns: scfg.Namespace,
		cl: cl, poolID: pool.ID, pgReady: cfg.ReadyTimeout, td: td, ybKube: kube,
	}, nil
}

// provisionLiveArms stands the current and baseline arms up concurrently. The
// current arm's error fails the caller; the baseline arm is best-effort and
// comes back nil with its error as baseErr, so the caller can fall back to the
// history gate and still record why the arm is missing.
func provisionLiveArms(t *core.T, cfg perfConfig, tag string, logf func(string, ...any)) (cur, base *liveArm, baseErr error, err error) {
	var armErr error
	stepErr := t.Step("provision_arms", func(s *core.StepHandle) error {
		s.Set("baseline", tag)
		s.Set("node_selector", orNone(cfg.ArmNodeSelector))
		baseAmp := ampstack.FromConfig(t.Options.Derive(baselineOverrides(tag)))
		var wg sync.WaitGroup
		var curErr, baseErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			cur, curErr = provisionLiveArm(t.Context, t, cfg, ampstack.FromConfig(t.Options), "", t.ResourceSuffix()+"p", logf)
		}()
		go func() {
			defer wg.Done()
			base, baseErr = provisionLiveArm(t.Context, t, cfg, baseAmp, tag, t.ResourceSuffix()+"b", logf)
		}()
		wg.Wait()
		armErr = baseErr
		if baseErr != nil {
			logf("perf: WARN live baseline arm failed to provision (%v) — gating on history instead", baseErr)
		}
		return curErr
	})
	return cur, base, armErr, stepErr
}

// liveArmPolicy turns per-datapoint deltas into one verdict.
//
// Thr is the flat allowance. Bands raises it per datapoint, never lowers it, to
// that metric's trailing dispersion from the portal (perf.gate.mad_k x MAD).
//
// That band is CROSS-RUN dispersion, and the two arms exist to strip cross-run
// variance out, so using it here is a compromise and not the right statistic.
// The right one is arm-to-arm dispersion, which nothing measures yet. It is used
// because the arms turn out not to share weather: A/A run 9cadd192, both arms on
// one image, reported lifecycle.delete_fork_branch +118.2%, and the A/B beside it
// +124.3%. The band is the only per-metric noise estimate available today, so it
// stands in until enough A/A runs exist to replace it.
//
// It widens more than it looks: on the fbf0de9f replay, 5 of 10 lifecycle
// datapoints land above the flat 35%, lifecycle.load among them at 83.4% because
// its window still holds pre-pinning runs. Expect that to tighten as pinned runs
// age into the window, and read the band as a ceiling on our own confidence
// rather than as a property of the build.
//
// GrossFactor and MinViolations decide when a violation is worth failing on. Ten
// datapoints per test across six nightly perf tests means even a 2% per-datapoint
// false alarm rate reds the board most nights, so a lone moderate outlier is not
// enough: either one datapoint is GrossFactor past its allowance, or
// MinViolations of them agree. Both scale with the band, so a wide band raises
// the gross bar too: on a metric whose own noise is 80%, no single run can
// distinguish a regression below that, and only persistence across runs can.
type liveArmPolicy struct {
	Thr            float64Thresholds
	Bands          map[string]float64
	UseHistoryBand bool
	GrossFactor    float64
	MinViolations  int
}

func (p liveArmPolicy) allowanceFor(dp core.Datapoint) float64 {
	allowed := p.Thr.RisePct
	if dp.Direction == core.HigherIsBetter {
		allowed = p.Thr.DropPct
	}
	if p.UseHistoryBand {
		if band, ok := p.Bands[dp.Name]; ok && band > allowed {
			return band
		}
	}
	return allowed
}

// liveArmGate compares the two arms' gated datapoints by name: delta in the
// worse direction, allowed by policy. Datapoints only one arm produced are
// skipped — they have no counterpart to accuse. curBest/baseBest carry each
// name's best iteration: endpoint pods land on whatever node type the
// autoscaler has (observed run 55349d45: highcpu-8 and standard-8 mixed within
// one arm), so a median-only verdict gates the node lottery; a real regression
// also slows the best draw. nil maps skip that check (single-sample suites).
// Returns the attachable table and the verdict.
// The second return is the violations that were reported but did not fail, so a
// caller can surface them: with the live arm up nothing else asserts, and a
// regression under the quorum would otherwise leave no trace outside the
// attached table.
func liveArmGate(current, base []core.Datapoint, curBest, baseBest map[string]float64, pol liveArmPolicy, tag string) (string, []string, error) {
	ref := map[string]core.Datapoint{}
	for _, dp := range base {
		ref[dp.Name] = dp
	}
	var b strings.Builder
	var gross, moderate []string
	fmt.Fprintf(&b, "live baseline: this run vs release %s (same workload, concurrent stacks)\n", tag)
	fmt.Fprintf(&b, "%-40s %14s %14s %9s %11s %9s %8s\n", "datapoint", "current", "baseline", "delta%", "allowed%", "spread%", "verdict")
	for _, dp := range current {
		rdp, ok := ref[dp.Name]
		if !ok || dp.Value == 0 || rdp.Value == 0 {
			continue
		}
		d, ok := regressionPct(rdp.Value, dp.Value, dp.Direction)
		if !ok {
			continue
		}
		allowed := pol.allowanceFor(dp)
		verdict := "ok"
		switch {
		case allowed <= 0 || d <= allowed:
		// A percentage gate needs signal: one-shot ms-scale ops through two
		// port-forwards jitter by tens of ms, so a violation must also clear an
		// absolute floor. Ordered after the allowance so "floor" marks only the
		// rows the floor actually suppressed.
		case dp.Unit == "ms" && math.Abs(dp.Value-rdp.Value) < liveArmNoiseFloorMS:
			verdict = "floor"
		default:
			verdict = liveArmSuspect(dp, d, allowed, curBest, baseBest)
			if verdict == "suspect" && pol.GrossFactor > 0 && d > allowed*pol.GrossFactor {
				verdict = "gross"
			}
		}
		fmt.Fprintf(&b, "%-40s %14.3f %14.3f %+8.1f%% %10.1f%% %8.1f%% %8s\n",
			dp.Name, dp.Value, rdp.Value, d, allowed, dp.SpreadPct, verdict)
		if verdict != "gross" && verdict != "suspect" {
			continue
		}
		line := fmt.Sprintf("%s %+.1f%% vs the live %s baseline (allowed %.1f%%: current %.4g, baseline %.4g %s)",
			dp.Name, d, tag, allowed, dp.Value, rdp.Value, dp.Unit)
		if verdict == "gross" {
			gross = append(gross, line)
		} else {
			moderate = append(moderate, line)
		}
	}
	if len(gross) > 0 {
		return b.String(), moderate, fmt.Errorf("perf regression vs the live baseline: %s", strings.Join(gross, "; "))
	}
	if len(moderate) >= max(pol.MinViolations, 1) {
		return b.String(), nil, fmt.Errorf("perf regression vs the live baseline, %d datapoints past their allowance: %s",
			len(moderate), strings.Join(moderate, "; "))
	}
	for _, m := range moderate {
		fmt.Fprintf(&b, "below the %d-datapoint quorum, reported but not failed: %s\n", max(pol.MinViolations, 1), m)
	}
	return b.String(), moderate, nil
}

// liveArmSuspect classifies a delta that already cleared its allowance.
func liveArmSuspect(dp core.Datapoint, d, allowed float64, curBest, baseBest map[string]float64) string {
	corroborated := false
	if curBest != nil && baseBest != nil && dp.Direction == core.LowerIsBetter {
		cb, cok := curBest[dp.Name]
		bb, bok := baseBest[dp.Name]
		if cok && bok && cb > 0 && bb > 0 {
			bd, ok := regressionPct(bb, cb, dp.Direction)
			if !ok || bd <= allowed {
				return "best-ok"
			}
			corroborated = true
		}
	}
	// A delta inside the arm's own iteration spread is two draws from one
	// distribution, so it is no verdict. This is the cruder of the two guards:
	// where the best draw regressed too, that corroboration wins.
	if !corroborated && dp.SpreadPct > 0 && d <= dp.SpreadPct {
		return "spread"
	}
	return "suspect"
}

// phaseBarrier pairs the arms' equally-numbered iterations phase by phase, so
// each gated phase runs on both arms under the same cluster weather.
type phaseBarrier struct {
	mu      sync.Mutex
	waiting map[string]chan struct{}
	dead    map[int]chan struct{}
}

func newPhaseBarrier() *phaseBarrier {
	return &phaseBarrier{waiting: map[string]chan struct{}{}, dead: map[int]chan struct{}{}}
}

// pairWaitCap bounds a wait for the partner arm; past it pairing is abandoned.
const pairWaitCap = 10 * time.Minute

// liveArmNoiseFloorMS is the smallest ms delta the live-arm gate treats as signal.
const liveArmNoiseFloorMS = 250

func (b *phaseBarrier) sync(iter int, name string) {
	if b == nil {
		return
	}
	key := fmt.Sprintf("%d/%s", iter, name)
	b.mu.Lock()
	deadCh := b.deadLocked(iter)
	select {
	case <-deadCh:
		b.mu.Unlock()
		return
	default:
	}
	if ch, ok := b.waiting[key]; ok {
		delete(b.waiting, key)
		close(ch)
		b.mu.Unlock()
		return
	}
	ch := make(chan struct{})
	b.waiting[key] = ch
	b.mu.Unlock()
	select {
	case <-ch:
	case <-deadCh:
	case <-time.After(pairWaitCap):
		b.abandon(iter)
	}
}

// abandon releases an iteration's current and future waits (partner died or lagged out).
func (b *phaseBarrier) abandon(iter int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.deadLocked(iter)
	select {
	case <-d:
		return
	default:
	}
	close(d)
	prefix := fmt.Sprintf("%d/", iter)
	for k, ch := range b.waiting {
		if strings.HasPrefix(k, prefix) {
			delete(b.waiting, k)
			close(ch)
		}
	}
}

func (b *phaseBarrier) deadLocked(iter int) chan struct{} {
	d, ok := b.dead[iter]
	if !ok {
		d = make(chan struct{})
		b.dead[iter] = d
	}
	return d
}

// The baseline arm's values, .baseline-suffixed, for the report — never gated.
func liveArmDatapoints(base []core.Datapoint) []core.Datapoint {
	out := make([]core.Datapoint, len(base))
	for i, dp := range base {
		dp.Name += ".baseline"
		out[i] = dp
	}
	return out
}
