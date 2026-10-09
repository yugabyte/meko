package task

import (
	"fmt"
	"testing"

	"github.com/yugabyte/amp/controller/internal/core"
)

// TestPgFailStepRecordsErrorCode pins error_code against the real SQL: written by
// FailStep, read back through taskColumns, and cleared by CompleteTask alongside
// `error`.
func TestPgFailStepRecordsErrorCode(t *testing.T) {
	pool, ctx := migratedPool(t)
	t.Cleanup(pool.Close)
	resetTasks(t, pool)
	s := NewPgStore(pool)

	tk := seedTask(t, s, Kind{"test", "", "", nil}, "work", "")
	const owner = "TestPgFailStepRecordsErrorCode"
	if claimed, _, err := s.Claim(ctx, tk.ID, owner); err != nil || claimed == nil {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	cause := fmt.Errorf("ensure endpoint record: %w", core.ErrConflict)
	if err := s.FailStep(ctx, tk.ID, tk.StepIDs[0], owner, cause); err != nil {
		t.Fatalf("FailStep: %v", err)
	}
	got, err := s.Get(ctx, tk.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ErrorCode == nil || *got.ErrorCode != ErrorCodeConflict {
		t.Fatalf("error_code = %v, want %q", got.ErrorCode, ErrorCodeConflict)
	}

	if err := s.CompleteTask(ctx, tk.ID, owner); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, _ = s.Get(ctx, tk.ID)
	if got.ErrorCode != nil {
		t.Errorf("error_code = %q after CompleteTask, want nil", *got.ErrorCode)
	}
}
