package task

import (
	"context"
	"encoding/json"
)

// Registry maps a step kind to the Handler that runs it. The executor looks a
// step's kind up here; a missing kind fails the task rather than silently
// skipping work.
type Registry struct {
	handlers map[string]Handler
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]Handler)}
}

// Register adds a handler, keyed by its Kind. A later registration of the same
// kind replaces the earlier one.
func (r *Registry) Register(h Handler) {
	r.handlers[h.Kind()] = h
}

// Lookup returns the handler for kind, if registered.
func (r *Registry) Lookup(kind string) (Handler, bool) {
	h, ok := r.handlers[kind]
	return h, ok
}

// DefaultRegistry returns a registry holding only the no-op step — enough to
// exercise the executor end-to-end without any collaborators. Used when no
// real provisioning dependencies are wired (e.g. the base skeleton).
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(NoopHandler{})
	return r
}

// ProductionRegistry returns the registry wired with the real step catalog
// (steps.go), backed by deps. It is the registry the worker uses once the
// controller has its provisioning collaborators. The endpoint steps require a
// deployer; if deps.Deployer is nil they are still registered but will fail if
// ever run — no endpoint task is planned without a deployer (see plan.go / the
// API wiring, #113). The no-op step stays registered for diagnostics/tests.
func ProductionRegistry(deps Deps) *Registry {
	r := NewRegistry()
	r.Register(NoopHandler{})
	r.Register(CreateProjectHandler{State: deps.State})
	r.Register(CreateBranchHandler{State: deps.State})
	r.Register(RecordBranchRecipeHandler{Recipes: deps.Recipes})
	r.Register(CreateEndpointHandler{
		State:     deps.State,
		Recipes:   deps.Recipes,
		Deployer:  deps.Deployer,
		YB:        deps.EndpointYB,
		SCURL:     deps.SCEndpointURL,
		Bootstrap: deps.Bootstrap,
	})
	r.Register(DeleteEndpointHandler{State: deps.State, Deployer: deps.Deployer})
	return r
}

// KindNoop is a step that does nothing; a placeholder until the real step
// catalog exists.
const KindNoop = "noop"

// NoopHandler succeeds immediately. It is trivially idempotent.
type NoopHandler struct{}

func (NoopHandler) Kind() string { return KindNoop }

func (NoopHandler) Run(_ context.Context, _ StepInput) (json.RawMessage, error) {
	return json.RawMessage(`{"noop":true}`), nil
}
