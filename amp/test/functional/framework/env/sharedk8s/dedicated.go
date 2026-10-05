package sharedk8s

import (
	"context"
	"fmt"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybdeploy"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// stack is the control plane and backing a DedicatedStack test owns: the same
// apps and knobs as the run-scoped stack, in a namespace of its own, gone with
// the test. A shared-control-plane test has none (Env.own == nil).
type stack struct {
	ampCfg          ampstack.Config
	app             *ampstack.App
	kube            *k8s.Kubectl
	backingUniverse core.Environment
	backingPlan     *reap.Plan
	backingTeardown func(context.Context) error
	// backingProv is the provisioner (its own YBA) a test with a BackingConfig
	// got; nil when the run's shared one served. It is closed only once the
	// backing under it went down cleanly: removing the YBA while its universe's
	// nodes may still be running strands them with no manager, the same
	// invariant teardownProvisionedYBA holds. A stack that fails either step
	// stays in the reap plan instead.
	backingProv backingProvisioner
}

// OnReapPlanChange registers fn to run whenever a dedicated stack comes up or
// goes away, so the reap plan on disk always names what is standing.
func (p *Provider) OnReapPlanChange(fn func()) { p.onPlanChange = fn }

// provisionDedicated brings up a whole stack for one test and carves its cluster
// out of it. ctx is the test's provision budget (timeouts.provision_dedicated),
// which is why nothing here roots itself in the background the way the shared
// bring-up does: this stack lives and dies with one test.
func (p *Provider) provisionDedicated(ctx context.Context, spec core.EnvSpec, seed *core.Seed) (core.Environment, error) {
	if !p.deploy {
		return nil, &core.UnsupportedShapeError{
			Provider: "sharedk8s",
			Shape:    core.DedicatedStack,
			Why:      "an attached control plane cannot stand a stack up per test; run in deploy mode (env.sharedk8s.context without controller_url)",
		}
	}
	st := &stack{ampCfg: p.ampStackFor(spec)}
	st.ampCfg.Context = p.ampCfg.Context
	st.ampCfg.Namespace = DefaultNamespacePrefix + "-" + randHex()
	ns := st.ampCfg.Namespace
	p.log("sharedk8s: dedicated stack %s: provisioning backing and control plane", ns)

	store, td, plan, own, err := p.provisionBackingFor(ctx, ns, spec)
	// Recorded before the error check: a bring-up that failed with its YBA still
	// standing hands the provisioner back, and the stack is what carries it into
	// the reap plan.
	st.backingTeardown, st.backingPlan, st.backingProv = td, plan, own
	if err != nil {
		if own != nil || plan != nil {
			p.trackStack(st, true)
		}
		return nil, err
	}
	if store != nil {
		st.ampCfg.Backing = ybbacking.ExternalBacking(store)
		st.backingUniverse = ybdeploy.UniverseEnvOf(store)
	}
	p.trackStack(st, true)
	fail := func(err error) (core.Environment, error) {
		p.closeStack(st)
		return nil, err
	}

	app := p.newApp(st.ampCfg)
	if _, err := app.Deploy(ctx); err != nil {
		_ = app.Teardown(context.Background())
		return fail(fmt.Errorf("sharedk8s: dedicated stack %s: deploy control plane: %w", ns, err))
	}
	st.app, st.kube = app, app.Kube()
	cl, url, err := app.ControllerClient(ctx)
	if err != nil {
		return fail(fmt.Errorf("sharedk8s: dedicated stack %s: controller client: %w", ns, err))
	}
	env, err := p.carve(ctx, cl, url, seed, st)
	if err != nil {
		return fail(err)
	}
	p.log("sharedk8s: dedicated stack %s ready", ns)
	return env, nil
}

// ampStackFor resolves the control-plane config for one dedicated stack. A test
// declaring its own backing shape gets the ampstack knobs from the SAME overlay
// the backing is deployed from, because the shape reaches the control plane too:
// a controller whose startup migrations run as cross-region DDL needs a
// readiness budget the shipped one does not give it. Without an overlay this is
// the provider's own config unchanged.
func (p *Provider) ampStackFor(spec core.EnvSpec) ampstack.Config {
	if spec.BackingConfig == nil || p.ybDeployCfg == nil {
		return p.cfg.AmpStack
	}
	overlay := spec.BackingConfig(p.ybDeployCfg)
	if len(overlay) == 0 {
		return p.cfg.AmpStack
	}
	// Only the keys the overlay actually names are taken; everything else stays
	// whatever the run resolved, so this cannot quietly re-derive a field the
	// caller set some other way.
	derived := p.ybDeployCfg.Derive(overlay)
	cfg := p.cfg.AmpStack
	if _, ok := overlay["env.ampstack.controller_ready_timeout"]; ok {
		cfg.ControllerReadyTimeout = ampstack.FromConfig(derived).ControllerReadyTimeout
	}
	if _, ok := overlay["env.ampstack.yb_ready_timeout"]; ok {
		cfg.YBReadyTimeout = ampstack.FromConfig(derived).YBReadyTimeout
	}
	for k := range overlay {
		if strings.HasPrefix(k, backingAddressPrefix) {
			cfg.BackingAddress = ampstack.FromConfig(derived).BackingAddress
			break
		}
	}
	return cfg
}

// teardownDedicated drops the whole stack; the cluster goes with its namespace,
// so there is no per-cluster delete first.
func (p *Provider) teardownDedicated(e *Env) error {
	e.pgMu.Lock()
	pfs := e.pfs
	e.pgMu.Unlock()
	closeAll(pfs)
	closeAll(e.takeExtraPFs())
	return p.closeStack(e.own)
}

// closeStack tears the control plane down, then its backing (LIFO, like Close).
// Rooted in the background on the deploy budget: a universe delete outlasts the
// per-test teardown deadline, and a stack left standing costs cloud nodes.
func (p *Provider) closeStack(st *stack) error {
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.DeployTimeout)
	defer cancel()
	var firstErr error
	if st.app != nil {
		_ = st.app.Close(ctx)
		if err := st.app.Teardown(ctx); err != nil {
			firstErr = fmt.Errorf("sharedk8s: dedicated stack %s: teardown control plane: %w", st.ampCfg.Namespace, err)
		}
	}
	if st.backingTeardown != nil {
		if err := st.backingTeardown(ctx); err != nil {
			p.log("sharedk8s: dedicated stack %s: teardown backing failed: %v", st.ampCfg.Namespace, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if st.backingProv != nil && firstErr == nil {
		if err := st.backingProv.Close(ctx); err != nil {
			p.log("sharedk8s: dedicated stack %s: teardown own YBA failed: %v", st.ampCfg.Namespace, err)
			firstErr = err
		}
	}
	// A stack whose teardown failed stays in the plan: the file is the last
	// record of what survived, since RunOne drops the teardown error.
	if firstErr == nil {
		p.trackStack(st, false)
	}
	return firstErr
}

// trackStack mutates the live set and rewrites the plan under one lock, so two
// stacks changing at once cannot leave a stale snapshot on disk.
func (p *Provider) trackStack(st *stack, live bool) {
	p.planMu.Lock()
	defer p.planMu.Unlock()
	p.stacksMu.Lock()
	if p.stacks == nil {
		p.stacks = map[*stack]struct{}{}
	}
	if live {
		p.stacks[st] = struct{}{}
	} else {
		delete(p.stacks, st)
	}
	fn := p.onPlanChange
	p.stacksMu.Unlock()
	if fn != nil {
		fn()
	}
}

func (p *Provider) liveStacks() []*stack {
	p.stacksMu.Lock()
	defer p.stacksMu.Unlock()
	out := make([]*stack, 0, len(p.stacks))
	for st := range p.stacks {
		out = append(out, st)
	}
	return out
}

func (st *stack) addToPlan(out *reap.Plan) {
	if st.backingPlan != nil {
		out.AddCloud(st.backingPlan.Cloud)
		for i := range st.backingPlan.Clouds {
			out.AddCloud(&st.backingPlan.Clouds[i])
		}
		out.Namespaces = append(out.Namespaces, st.backingPlan.Namespaces...)
	}
	out.Namespaces = append(out.Namespaces, reap.NSReap{
		Kind: reap.KindAmp, KubeContext: st.ampCfg.Context, Namespace: st.ampCfg.Namespace,
	})
}

// storageAccess is how this env reaches its backing amp storage database: its
// own stack's when it has one, else the run-scoped control plane's.
func (e *Env) storageAccess() (ampstack.StorageAccess, error) {
	if e.own != nil {
		return e.own.app.StorageAccess(), nil
	}
	return e.prov.storageAccess()
}

// backingAddressPrefix keys only change how the control plane addresses the
// backing, so they must not buy the test a YBA of its own.
const backingAddressPrefix = "env.ampstack.backing_address."

func backingOnlyOverlay(overlay map[string]string) map[string]string {
	out := make(map[string]string, len(overlay))
	for k, v := range overlay {
		if !strings.HasPrefix(k, backingAddressPrefix) {
			out[k] = v
		}
	}
	return out
}
