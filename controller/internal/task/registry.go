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

// DefaultRegistry returns the registry the worker uses by default. For now it
// holds only the no-op step, enough to exercise the executor end-to-end.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(NoopHandler{})
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
