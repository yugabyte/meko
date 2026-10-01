package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// abortErr is a serialization failure as it reaches the worker: wrapped by the
// store in a legible message, under no sentinel, with the driver error preserved
// via %w — which is what db.IsAbortedTransaction classifies on.
func abortErr() error {
	return fmt.Errorf("branch registration conflicted with a concurrent drop or fork of the ancestor: %w",
		&pgconn.PgError{Code: "40001", Message: "could not serialize access due to concurrent update"})
}

// TestWorkerRetriesSerializationAbort is #578: a step that loses one of the
// controller's write interlocks must be re-run, not fail its task. The interlock
// aborts the loser by design, so failing on it strands an operation for a
// condition that clears the moment the winner commits.
func TestWorkerRetriesSerializationAbort(t *testing.T) {
	store := NewMemStore()
	step := &fakeStep{kind: "k0", failWith: abortErr(), failTimes: 2}
	w := testWorker(store, regWith(step))
	rec := &fakeRecorder{}
	w.metrics = rec

	task, steps := pendingTask("k0")
	store.Seed(task, steps)
	claimAndExecute(t, w)

	if got := step.callCount(); got != 3 {
		t.Fatalf("step calls = %d, want 3 (two aborts then success)", got)
	}
	assertStatus(t, store, task.ID, StatusSucceeded, StatusSucceeded)
	if len(rec.retried) != 2 {
		t.Fatalf("StepRetried calls = %d, want 2", len(rec.retried))
	}
}

// TestWorkerFailsAfterRetryAttempts pins the ATTEMPT bound: a conflict that never
// clears must terminate rather than spin, and fail through the ordinary path so
// the task is durably failed. The step here returns immediately, so the backoffs
// never approach stepRetryBudget and the attempt count is what stops it — the
// wall-clock bound is TestWorkerRetryBudgetStopsExpensiveStep's job.
func TestWorkerFailsAfterRetryAttempts(t *testing.T) {
	store := NewMemStore()
	step := &fakeStep{kind: "k0", failWith: abortErr()} // fails every call
	w := testWorker(store, regWith(step))

	task, steps := pendingTask("k0")
	store.Seed(task, steps)
	claimAndExecute(t, w)

	if got := step.callCount(); got != stepRetryAttempts {
		t.Fatalf("step calls = %d, want stepRetryAttempts (%d)", got, stepRetryAttempts)
	}
	assertStatus(t, store, task.ID, StatusFailed, StatusFailed)
}

// TestWorkerDoesNotRetryOrdinaryFailure pins that the retry is scoped to the
// abort: any other error is a step that cannot succeed by re-running, and
// re-running it would multiply its side effects.
func TestWorkerDoesNotRetryOrdinaryFailure(t *testing.T) {
	store := NewMemStore()
	step := &fakeStep{kind: "k0", failWith: errors.New("boom")}
	w := testWorker(store, regWith(step))

	task, steps := pendingTask("k0")
	store.Seed(task, steps)
	claimAndExecute(t, w)

	if got := step.callCount(); got != 1 {
		t.Fatalf("step calls = %d, want 1 (an ordinary failure is not retried)", got)
	}
	assertStatus(t, store, task.ID, StatusFailed, StatusFailed)
}

// TestWorkerStopsRetryingOnCancellation pins that a shutdown mid-retry leaves the
// task RECLAIMABLE rather than durably failed: execute treats a cancelled context
// as an interruption, and the retry loop must not burn its remaining attempts or
// override that. Without this, a graceful stop during contention would strand
// tasks that a replay would have completed.
func TestWorkerStopsRetryingOnCancellation(t *testing.T) {
	store := NewMemStore()
	step := &fakeStep{kind: "k0", failWith: abortErr()}
	w := testWorker(store, regWith(step))

	task, steps := pendingTask("k0")
	store.Seed(task, steps)

	ctx, cancel := context.WithCancel(t.Context())
	claimed, _, err := store.Claim(ctx, task.ID, w.owner)
	if err != nil || claimed == nil {
		t.Fatalf("Claim: task=%v err=%v", claimed, err)
	}
	// Cancel while the first backoff is in flight.
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	if err := w.execute(ctx, w.logger, claimed); err == nil {
		t.Fatal("execute returned nil, want an interruption error")
	}
	if got := step.callCount(); got >= stepRetryAttempts {
		t.Fatalf("step calls = %d, want fewer than the full budget (%d) after cancellation",
			got, stepRetryAttempts)
	}
	// Left mid-flight for the lease to lapse and a replay to pick up (§8.2).
	assertStatus(t, store, task.ID, StatusRunning, StatusRunning)
}

// TestJitterWithinBounds pins the spread concurrent losers rely on: never zero
// (they would collide again in lockstep) and never above the backoff given.
func TestJitterWithinBounds(t *testing.T) {
	const d = 100 * time.Millisecond
	for range 200 {
		if got := jitter(d); got < d/2 || got > d {
			t.Fatalf("jitter(%v) = %v, want within [%v, %v]", d, got, d/2, d)
		}
	}
	if got := jitter(0); got != 0 {
		t.Fatalf("jitter(0) = %v, want 0", got)
	}
}

// assertStatus reads back the task and its single step, asserting both statuses.
func assertStatus(t *testing.T, store Store, taskID uuid.UUID, wantTask, wantStep Status) {
	t.Helper()
	got, err := store.Get(context.Background(), taskID)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	if got.Status != wantTask {
		t.Errorf("task status = %q, want %q", got.Status, wantTask)
	}
	stepRows, err := store.Steps(context.Background(), taskID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	for i, st := range stepRows {
		if st.Status != wantStep {
			t.Errorf("step %d status = %q, want %q", i, st.Status, wantStep)
		}
	}
}

// slowAbortStep always loses the interlock, and takes long enough doing so that
// the wall-clock budget rather than the attempt count is what stops the retry.
type slowAbortStep struct {
	kind  string
	delay time.Duration
	mu    sync.Mutex
	calls int
}

func (s *slowAbortStep) Kind() string { return s.kind }

func (s *slowAbortStep) Execute(context.Context) StepResult {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	time.Sleep(s.delay)
	return Fail(abortErr())
}

func (s *slowAbortStep) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestWorkerRetryBudgetStopsExpensiveStep pins the wall-clock guard. The unit the
// worker re-runs is the whole step, so a step whose external effects precede its
// retryable write repeats them once per attempt — delete_branch retires endpoint
// instances before tombstoning the branch row. An attempt count bounds the number
// of re-runs but not their cost, so a step that took longer than the budget to lose
// the interlock is failed instead of re-run.
func TestWorkerRetryBudgetStopsExpensiveStep(t *testing.T) {
	store := NewMemStore()
	step := &slowAbortStep{kind: "k0", delay: 25 * time.Millisecond}
	reg := NewRegistry()
	reg.Register(step.kind, func(json.RawMessage, Deps) (Step, error) { return step, nil })
	w := testWorker(store, reg)
	w.retryBudget = 5 * time.Millisecond // exceeded by the step's own first run

	task, steps := pendingTask("k0")
	store.Seed(task, steps)
	claimAndExecute(t, w)

	if got := step.callCount(); got != 1 {
		t.Fatalf("step calls = %d, want 1 (the budget is spent by the first run, so no re-run)", got)
	}
	assertStatus(t, store, task.ID, StatusFailed, StatusFailed)
}
