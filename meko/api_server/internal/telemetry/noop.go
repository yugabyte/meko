package telemetry

import "context"

// NoopClient satisfies Client without doing anything. Returned when no
// write-key is configured and used by every unit test. Nil-safe callers
// still work, but the router hands out a NoopClient rather than nil so
// services can drop the `if s.telemetry != nil` branches.
type NoopClient struct{}

// NewNoop returns a Client whose methods are all no-ops.
func NewNoop() Client { return NoopClient{} }

// Track discards the event.
func (NoopClient) Track(_ context.Context, _, _ string, _ map[string]any) {}

// Identify discards the traits.
func (NoopClient) Identify(_ context.Context, _ string, _ map[string]any) {}

// Flush is a no-op.
func (NoopClient) Flush(_ context.Context) error { return nil }
