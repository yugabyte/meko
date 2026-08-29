package perf

import (
	"context"
	"fmt"
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

	mu  sync.Mutex
	pfs []*k8s.PortForward
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

// endpointClient carves the arm's own perf endpoint (user → pool → cluster →
// endpoint) and connects to it — what a phase-timing workload needs.
func (a *liveArm) endpointClient(ctx context.Context) (*pg.Client, error) {
	ep, err := createPerfEndpoint(ctx, a.cl, a.sfx)
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

	info, kube, err := deployYBBacking(ctx, t, cfg.RF, sfx, td, logf)
	if err != nil {
		return nil, err
	}
	if err := waitBackingYSQL(ctx, kube, info, cfg.RF, cfg.ReadyTimeout, sink, logf); err != nil {
		return nil, err
	}

	ybCfg := ybk8s.FromConfig(t.Options)
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
		cl: cl, poolID: pool.ID, pgReady: cfg.ReadyTimeout, td: td,
	}, nil
}

// provisionLiveArms stands the current and baseline arms up concurrently. The
// current arm's error fails the caller; the baseline arm is best-effort and
// comes back nil (WARN logged) so the caller can fall back to the history gate.
func provisionLiveArms(t *core.T, cfg perfConfig, tag string, logf func(string, ...any)) (cur, base *liveArm, err error) {
	stepErr := t.Step("provision_arms", func(s *core.StepHandle) error {
		s.Set("baseline", tag)
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
		if baseErr != nil {
			logf("perf: WARN live baseline arm failed (%v) — gating on history instead", baseErr)
		}
		return curErr
	})
	return cur, base, stepErr
}

// liveArmGate compares the two arms' gated datapoints by name: delta in the
// worse direction, allowed by the direction's threshold (drop for
// higher-is-better, rise for lower-is-better). Datapoints only one arm produced
// are skipped — they have no counterpart to accuse. Returns the attachable
// table and the verdict.
func liveArmGate(current, base []core.Datapoint, thr float64Thresholds, tag string) (string, error) {
	ref := map[string]core.Datapoint{}
	for _, dp := range base {
		ref[dp.Name] = dp
	}
	var b strings.Builder
	var violations []string
	fmt.Fprintf(&b, "live baseline: this run vs release %s (same workload, concurrent stacks)\n", tag)
	fmt.Fprintf(&b, "%-40s %14s %14s %9s %11s\n", "datapoint", "current", "baseline", "delta%", "allowed%")
	for _, dp := range current {
		rdp, ok := ref[dp.Name]
		if !ok || dp.Value == 0 || rdp.Value == 0 {
			continue
		}
		d, ok := regressionPct(rdp.Value, dp.Value, dp.Direction)
		if !ok {
			continue
		}
		allowed := thr.RisePct
		if dp.Direction == core.HigherIsBetter {
			allowed = thr.DropPct
		}
		fmt.Fprintf(&b, "%-40s %14.3f %14.3f %+8.1f%% %10.1f%%\n", dp.Name, dp.Value, rdp.Value, d, allowed)
		if allowed > 0 && d > allowed {
			violations = append(violations, fmt.Sprintf("%s %+.1f%% vs the live %s baseline (allowed %.1f%%: current %.4g, baseline %.4g %s)",
				dp.Name, d, tag, allowed, dp.Value, rdp.Value, dp.Unit))
		}
	}
	if len(violations) > 0 {
		return b.String(), fmt.Errorf("perf regression vs the live baseline: %s", strings.Join(violations, "; "))
	}
	return b.String(), nil
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
