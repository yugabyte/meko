package task

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestPgTaskCancel pins the API-originated write against the real SQL, where the
// MemStore mirror cannot help: its whole point is what the predicate says —
// non-terminal status, and NOT the owner — and MemStore checking its own model
// would prove nothing about the statement that runs.
//
// Needs YugabyteDB; skips unless AMP_TEST_DATABASE_URL points at one.
func TestPgTaskCancel(t *testing.T) {
	pool, ctx := migratedPool(t)
	t.Cleanup(pool.Close)
	resetTasks(t, pool)
	s := NewPgStore(pool)

	tk, err := s.Create(ctx, TaskSpec{
		Kind:   KindUpgradeEndpoints,
		Params: json.RawMessage(`{"version":"v1"}`),
		Steps:  []StepSpec{{Kind: StepPlanEndpointUpgrades, Args: json.RawMessage(`{"version":"v1"}`)}},
	})
	if err != nil {
		t.Fatalf("plan a roll: %v", err)
	}

	// Pending, so the row has no owner at all — the case an owner fence could never
	// match, which is the whole reason this write carries none.
	if err := s.RequestCancel(ctx, tk.ID); err != nil {
		t.Fatalf("RequestCancel on a pending task: %v", err)
	}
	got, err := s.Get(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.CancelRequested() {
		t.Fatal("cancel_requested_at was not stamped")
	}
	if got.Status != StatusPending || got.Owner != nil || got.Error != nil {
		t.Errorf("an API write touched the worker's columns: status=%q owner=%v error=%v", got.Status, got.Owner, got.Error)
	}
	first := *got.CancelRequestedAt

	// Claimed by somebody else, and still writable: the caller is never the owner.
	const owner = "TestPgTaskCancel"
	if claimed, err := s.Claim(ctx, tk.ID, owner); err != nil || claimed == nil {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if err := s.RequestCancel(ctx, tk.ID); err != nil {
		t.Fatalf("RequestCancel on a task owned by another process: %v", err)
	}
	got, _ = s.Get(ctx, tk.ID)
	if !got.CancelRequestedAt.Equal(first) {
		t.Errorf("a repeat request moved cancel_requested_at from %v to %v", first, got.CancelRequestedAt)
	}

	// Terminal refuses, so cancelling finished work is a 409 rather than a silently
	// accepted no-op.
	if err := s.CompleteTask(ctx, tk.ID, owner); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := s.RequestCancel(ctx, tk.ID); !errors.Is(err, ErrTaskTerminal) {
		t.Errorf("RequestCancel on a succeeded task = %v, want ErrTaskTerminal", err)
	}
	if err := s.RequestCancel(ctx, uuid.New()); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("RequestCancel on an unknown task = %v, want ErrTaskNotFound", err)
	}
}
