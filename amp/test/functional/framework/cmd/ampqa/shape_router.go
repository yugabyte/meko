package main

import (
	"context"
	"sync"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
)

// shapeRouter sends Shape: DedicatedStack provisions to a dedicated provider and
// everything else to the primary. The declaration is binding: there is no knob
// routing a dedicated_stack test onto the shared control plane, and an
// unavailable dedicated provider skips the test rather than falling back.
type shapeRouter struct {
	primary core.EnvProvider
	// buildDedicated returns either a provider or the reason there is none.
	buildDedicated func() (core.EnvProvider, string)

	mu          sync.Mutex
	dedicated   core.EnvProvider
	unavailable string
	built       bool

	// owned tracks the dedicated provider's environments so Teardown routes each
	// back to its owner.
	owned sync.Map // core.Environment → struct{}
}

func newShapeRouter(primary core.EnvProvider, buildDedicated func() (core.EnvProvider, string)) *shapeRouter {
	return &shapeRouter{primary: primary, buildDedicated: buildDedicated}
}

func (r *shapeRouter) dedicatedProvider() (core.EnvProvider, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.built {
		r.dedicated, r.unavailable = r.buildDedicated()
		r.built = true
	}
	return r.dedicated, r.unavailable
}

func (r *shapeRouter) Provision(ctx context.Context, spec core.EnvSpec, seed *core.Seed) (core.Environment, error) {
	if spec.Shape != core.DedicatedStack {
		return r.primary.Provision(ctx, spec, seed)
	}
	prov, reason := r.dedicatedProvider()
	if prov == nil {
		return nil, &core.UnsupportedShapeError{Provider: "exclusivek8s", Shape: spec.Shape, Why: reason}
	}
	env, err := prov.Provision(ctx, spec, seed)
	if env != nil {
		r.owned.Store(env, struct{}{})
	}
	return env, err
}

func (r *shapeRouter) Teardown(ctx context.Context, env core.Environment) error {
	if _, ok := r.owned.LoadAndDelete(env); ok {
		prov, _ := r.dedicatedProvider()
		return prov.Teardown(ctx, env)
	}
	return r.primary.Teardown(ctx, env)
}

func (r *shapeRouter) Attach(ctx context.Context, ref string) (core.Environment, error) {
	return r.primary.Attach(ctx, ref)
}

// Prepare forwards the run-scoped bring-up; the dedicated provider has none.
func (r *shapeRouter) Prepare(ctx context.Context) error {
	if prep, ok := r.primary.(interface{ Prepare(context.Context) error }); ok {
		return prep.Prepare(ctx)
	}
	return nil
}

func (r *shapeRouter) Close() error {
	var firstErr error
	r.mu.Lock()
	dedicated := r.dedicated
	r.mu.Unlock()
	if c, ok := dedicated.(closer); ok {
		firstErr = c.Close()
	}
	if c, ok := r.primary.(closer); ok {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SharesInfraAcrossTests answers for the primary: the infra gate concerns the
// shared control plane, which dedicated-stack tests never touch.
func (r *shapeRouter) SharesInfraAcrossTests() bool {
	return core.ProviderSharesInfra(r.primary)
}

// SharedInfraHealthy answers for the primary, reporting ErrNoSharedInfraProbe
// when it cannot probe so the caller skips instead of recording a green check.
func (r *shapeRouter) SharedInfraHealthy(ctx context.Context) error {
	if hc, ok := r.primary.(core.SharedInfraHealthChecker); ok {
		return hc.SharedInfraHealthy(ctx)
	}
	return core.ErrNoSharedInfraProbe
}

func (r *shapeRouter) StandingEnv() (*core.StandingEnv, bool) {
	if sp, ok := r.primary.(standingProvider); ok {
		return sp.StandingEnv()
	}
	return nil, false
}

func (r *shapeRouter) ReapPlan() *reap.Plan {
	if pp, ok := r.primary.(planProvider); ok {
		return pp.ReapPlan()
	}
	return nil
}
