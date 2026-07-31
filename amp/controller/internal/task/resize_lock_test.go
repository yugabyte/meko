package task

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestResizeLock exercises the first-writer-wins resize lock at the store level
// (the MemStore mirror of the PgStore partial unique index): an in-flight
// resize_endpoint task holds its endpoint, a concurrent one is rejected, a
// different endpoint is unaffected, and a terminal task releases the lock.
func TestResizeLock(t *testing.T) {
	ep, other := uuid.NewString(), uuid.NewString()
	resizeSpec := func(ref string) TaskSpec {
		r := ref
		return TaskSpec{
			Kind:      KindResizeEndpoint,
			TargetRef: &r,
			Steps:     []StepSpec{{Kind: StepResizeEndpoint, Args: json.RawMessage(`{}`)}},
		}
	}

	t.Run("an in-flight resize blocks a second on the same endpoint", func(t *testing.T) {
		s := NewMemStore()
		if _, err := s.Create(t.Context(), resizeSpec(ep)); err != nil {
			t.Fatalf("first resize: %v", err)
		}
		if _, err := s.Create(t.Context(), resizeSpec(ep)); !errors.Is(err, ErrResizeInProgress) {
			t.Fatalf("second resize err = %v, want ErrResizeInProgress", err)
		}
		// A resize of a DIFFERENT endpoint is not blocked.
		if _, err := s.Create(t.Context(), resizeSpec(other)); err != nil {
			t.Fatalf("resize of a different endpoint: %v", err)
		}
	})

	t.Run("a terminal resize releases the lock", func(t *testing.T) {
		s := NewMemStore()
		ref := ep
		// A previously-completed resize for the same endpoint must not block.
		s.Seed(&Task{ID: uuid.New(), Kind: KindResizeEndpoint, Status: StatusSucceeded, TargetRef: &ref}, nil)
		if _, err := s.Create(t.Context(), resizeSpec(ep)); err != nil {
			t.Fatalf("resize after a terminal one = %v, want admitted", err)
		}
	})
}
