// Package consumer is the event consumer: it holds leases, processes each held shard's next batch and
// advances the offset in the same transaction, and runs the janitor measurements.
package consumer

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"meko/event-consumers/datapack-metrics-consumer/internal/store"
)

// Handler applies every event of its type from one batch, in order, through tx. It must not commit or
// roll back: the consumer advances the offset in the same transaction, which is what makes a batch's
// effects apply once. Returning an error rolls the batch back.
type Handler func(ctx context.Context, tx *sql.Tx, events []store.Event) error

// Registry maps an event type to its handler.
type Registry struct{ handlers map[string]Handler }

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{handlers: map[string]Handler{}} }

// Register adds the handler for an event type; a type has at most one.
func (r *Registry) Register(eventType string, h Handler) error {
	if _, dup := r.handlers[eventType]; dup {
		return fmt.Errorf("a handler for %q is already registered", eventType)
	}
	r.handlers[eventType] = h
	return nil
}

// Types lists the registered event types.
func (r *Registry) Types() []string {
	out := make([]string, 0, len(r.handlers))
	for t := range r.handlers {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// DefaultRegistry is what the consumer runs with. It is empty until the handler tickets (MEKO-870 and
// later) add theirs, and an event type with no handler holds its shard (ErrNoHandler), so a build with an
// empty registry holds every shard on its first event. Merge the handler (MEKO-870) before this runs anywhere.
func DefaultRegistry() *Registry { return NewRegistry() }
