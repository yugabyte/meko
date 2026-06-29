package task

import (
	"context"
	"encoding/json"
	"fmt"
)

// Builder reconstructs an executable Step from a persisted step's args (the
// task_step.args JSON) and the registry's deps. It decodes the args into the
// step's typed argument struct and binds them — together with the deps — via the
// step's constructor. One Builder per kind; bad args are a real error (the worker
// fails the step rather than run it half-formed).
type Builder func(args json.RawMessage, deps Deps) (Step, error)

// Registry maps a step kind to the Builder that reconstructs it, and holds the
// deps every step is wired with. The worker rebuilds every persisted step
// through here on claim/replay (§8.2) — fully wired and ready to Execute; a
// missing kind fails the task rather than silently skipping work.
type Registry struct {
	deps     Deps
	builders map[string]Builder
}

// NewRegistry returns an empty registry with no deps. Used by tests that
// register their own (deps-ignoring) builders; the real catalogs are built by
// DefaultRegistry / ProductionRegistry, which carry deps.
func NewRegistry() *Registry {
	return &Registry{builders: make(map[string]Builder)}
}

// Register adds a builder for kind. A later registration of the same kind
// replaces the earlier one.
func (r *Registry) Register(kind string, b Builder) {
	r.builders[kind] = b
}

// Build reconstructs the Step for kind from its persisted args, wired with the
// registry's deps. It errors if the kind is unregistered or the args fail to
// decode.
func (r *Registry) Build(kind string, args json.RawMessage) (Step, error) {
	b, ok := r.builders[kind]
	if !ok {
		return nil, fmt.Errorf("no step registered for kind %q", kind)
	}
	return b(args, r.deps)
}

// DefaultRegistry returns a registry holding only the no-op step — enough to
// exercise the executor end-to-end without any collaborators. Used when no real
// provisioning dependencies are wired (e.g. the base skeleton).
func DefaultRegistry(deps Deps) *Registry {
	r := NewRegistry()
	r.deps = deps
	r.Register(KindNoop, buildNoopStep)
	return r
}

// ProductionRegistry returns the registry wired with the real step catalog (the
// step_*.go files) plus the no-op step, all bound to deps. It is the registry the
// worker uses. A step whose deps are absent (e.g. an endpoint step built with no
// deployer) fails when executed; no such task is planned without those deps (see
// plan.go / the API wiring, #113).
func ProductionRegistry(deps Deps) *Registry {
	r := DefaultRegistry(deps)
	r.Register(StepCreateProject, buildCreateProjectStep)
	r.Register(StepCreateBranch, buildCreateBranchStep)
	r.Register(StepRecordBranchRecipe, buildRecordBranchRecipeStep)
	r.Register(StepCreateEndpoint, buildCreateEndpointStep)
	r.Register(StepDeleteEndpoint, buildDeleteEndpointStep)
	return r
}

// KindNoop is a step that does nothing; a placeholder used by DefaultRegistry
// and for diagnostics.
const KindNoop = "noop"

// noopStep succeeds immediately. It is trivially idempotent.
type noopStep struct{}

func (noopStep) Kind() string { return KindNoop }

func (noopStep) Execute(context.Context) StepResult {
	return Ok(json.RawMessage(`{"noop":true}`))
}

func buildNoopStep(json.RawMessage, Deps) (Step, error) { return noopStep{}, nil }
