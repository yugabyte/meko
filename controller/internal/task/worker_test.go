package task

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-amp/controller/internal/config"
)

// ctxObservingHandler fails iff its context is cancelled — modeling a step (a DB
// call, an HTTP call) that surfaces a cancellation error when the worker is torn
// down mid-step. It lets the cancellation-vs-failure distinction be tested.
type ctxObservingHandler struct {
	kind  string
	calls int
}

func (h *ctxObservingHandler) Kind() string { return h.kind }

func (h *ctxObservingHandler) Run(ctx context.Context, _ StepInput) (json.RawMessage, error) {
	h.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"ok":true}`), nil
}

// These tests drive the real Worker execution loop (worker.go) against the
// in-memory fakeStore and fakeHandlers, validating the write-ahead guarantees of
// amp-controller.md §8: a task runs its ordered steps to completion, a reclaimed
// task resumes at the first non-succeeded step (skipping finished ones), a failed
// step fails the task and halts later steps, and an unknown step kind fails
// rather than silently skips. They call the unexported drain() directly so
// execution is deterministic (no ticker timing).

func testWorker(store Store, reg *Registry) *Worker {
	cfg := config.TaskConfig{PollInterval: time.Millisecond, LeaseTTL: 30 * time.Second}
	return NewWorker(store, reg, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func regWith(handlers ...Handler) *Registry {
	r := NewRegistry()
	for _, h := range handlers {
		r.Register(h)
	}
	return r
}

// pendingTask builds a pending task with steps of the given kinds, all pending.
func pendingTask(kinds ...string) (*Task, []*Step) {
	id := uuid.New()
	now := time.Now()
	t := &Task{ID: id, Kind: "test", Status: StatusPending, CreatedAt: now, UpdatedAt: now}
	var steps []*Step
	for i, k := range kinds {
		steps = append(steps, &Step{TaskID: id, Ordinal: i, Kind: k, Status: StatusPending})
	}
	return t, steps
}

func TestWorkerRunsAllStepsAndCompletes(t *testing.T) {
	store := newFakeStore()
	h0 := &fakeHandler{kind: "k0"}
	h1 := &fakeHandler{kind: "k1"}
	w := testWorker(store, regWith(h0, h1))

	task, steps := pendingTask("k0", "k1")
	store.seed(task, steps)

	w.drain(t.Context())

	if h0.callCount() != 1 || h1.callCount() != 1 {
		t.Fatalf("handler calls = (k0:%d, k1:%d), want (1,1)", h0.callCount(), h1.callCount())
	}
	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusSucceeded {
		t.Fatalf("task status = %q, want succeeded", got.Status)
	}
	stepRows, _ := store.Steps(t.Context(), task.ID)
	for _, st := range stepRows {
		if st.Status != StatusSucceeded {
			t.Errorf("step %d status = %q, want succeeded", st.Ordinal, st.Status)
		}
		if len(st.Result) == 0 {
			t.Errorf("step %d has no result recorded", st.Ordinal)
		}
	}
}

func TestWorkerResumesSkippingSucceededSteps(t *testing.T) {
	store := newFakeStore()
	h0 := &fakeHandler{kind: "k0"}
	h1 := &fakeHandler{kind: "k1"}
	w := testWorker(store, regWith(h0, h1))

	// A task previously claimed by a now-dead owner: running, lease expired,
	// step 0 already succeeded, step 1 still pending. The worker must reclaim it
	// and resume at step 1 without re-running step 0 (§8.1/§8.2).
	id := uuid.New()
	expired := time.Now().Add(-time.Minute)
	owner := "dead-owner"
	task := &Task{ID: id, Kind: "test", Status: StatusRunning, Owner: &owner, LeaseExpiry: &expired, CreatedAt: time.Now()}
	steps := []*Step{
		{TaskID: id, Ordinal: 0, Kind: "k0", Status: StatusSucceeded},
		{TaskID: id, Ordinal: 1, Kind: "k1", Status: StatusPending},
	}
	store.seed(task, steps)

	w.drain(t.Context())

	if h0.callCount() != 0 {
		t.Errorf("already-succeeded step 0 was re-run %d time(s), want 0", h0.callCount())
	}
	if h1.callCount() != 1 {
		t.Errorf("step 1 ran %d time(s), want 1", h1.callCount())
	}
	got, _ := store.Get(t.Context(), id)
	if got.Status != StatusSucceeded {
		t.Fatalf("task status = %q, want succeeded", got.Status)
	}
}

func TestWorkerReplaysInFlightRunningStep(t *testing.T) {
	store := newFakeStore()
	h := &fakeHandler{kind: "k0"}
	w := testWorker(store, regWith(h))

	// The canonical §8.2 crash case: the owner died with the step left RUNNING
	// (started, never completed). The worker skips only *succeeded* steps, so it
	// must re-invoke this one — exactly once — and idempotency makes the replay
	// safe. A regression that skipped a running step (treating it as done) would
	// leave callCount at 0.
	id := uuid.New()
	expired := time.Now().Add(-time.Minute)
	owner := "dead-owner"
	task := &Task{ID: id, Kind: "test", Status: StatusRunning, Owner: &owner, LeaseExpiry: &expired, CreatedAt: time.Now()}
	store.seed(task, []*Step{{TaskID: id, Ordinal: 0, Kind: "k0", Status: StatusRunning}})

	w.drain(t.Context())

	if h.callCount() != 1 {
		t.Errorf("in-flight running step ran %d time(s) on reclaim, want exactly 1 (re-run, not skipped)", h.callCount())
	}
	got, _ := store.Get(t.Context(), id)
	if got.Status != StatusSucceeded {
		t.Fatalf("task status = %q, want succeeded", got.Status)
	}
}

func TestWorkerLeavesTaskReclaimableOnCancellation(t *testing.T) {
	store := newFakeStore()
	h := &ctxObservingHandler{kind: "k0"}
	w := testWorker(store, regWith(h))

	task, steps := pendingTask("k0")
	store.seed(task, steps)
	claimed, err := store.ClaimNext(t.Context(), "me", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: %v (claimed=%v)", err, claimed)
	}

	// The worker is shutting down mid-step: the step ctx is cancelled, so the
	// handler returns a cancellation error. That must NOT durably fail the task —
	// it must be left running/reclaimable so §8.2 resume picks it back up.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := w.execute(ctx, w.logger, claimed); err == nil {
		t.Fatal("execute on a cancelled ctx returned nil, want an interruption error")
	}
	got, _ := store.Get(t.Context(), task.ID)
	if got.Status == StatusFailed {
		t.Fatal("a task interrupted by shutdown was durably FAILED; want left running/reclaimable for §8.2 resume")
	}
}

func TestWorkerFailsTaskAndHaltsLaterSteps(t *testing.T) {
	store := newFakeStore()
	hFail := &fakeHandler{kind: "kfail", failWith: errors.New("boom")}
	hNext := &fakeHandler{kind: "knext"}
	w := testWorker(store, regWith(hFail, hNext))

	task, steps := pendingTask("kfail", "knext")
	store.seed(task, steps)

	w.drain(t.Context())

	if hNext.callCount() != 0 {
		t.Errorf("step after the failed one ran %d time(s), want 0", hNext.callCount())
	}
	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusFailed {
		t.Fatalf("task status = %q, want failed", got.Status)
	}
	if got.Error == nil {
		t.Error("failed task has no error recorded")
	}
	stepRows, _ := store.Steps(t.Context(), task.ID)
	if stepRows[0].Status != StatusFailed {
		t.Errorf("failed step status = %q, want failed", stepRows[0].Status)
	}
	if stepRows[1].Status != StatusPending {
		t.Errorf("step after the failure = %q, want pending (never started)", stepRows[1].Status)
	}
}

func TestWorkerFailsOnUnknownStepKind(t *testing.T) {
	store := newFakeStore()
	w := testWorker(store, NewRegistry()) // empty registry: no handler for "mystery"

	task, steps := pendingTask("mystery")
	store.seed(task, steps)

	w.drain(t.Context())

	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusFailed {
		t.Fatalf("task status = %q, want failed (no handler for step kind)", got.Status)
	}
}

func TestWorkerIgnoresUnexpiredRunningTask(t *testing.T) {
	store := newFakeStore()
	h := &fakeHandler{kind: "k0"}
	w := testWorker(store, regWith(h))

	// A task held by a live owner (lease in the future) must not be reclaimed.
	id := uuid.New()
	future := time.Now().Add(time.Hour)
	owner := "live-owner"
	task := &Task{ID: id, Kind: "test", Status: StatusRunning, Owner: &owner, LeaseExpiry: &future, CreatedAt: time.Now()}
	store.seed(task, []*Step{{TaskID: id, Ordinal: 0, Kind: "k0", Status: StatusPending}})

	w.drain(t.Context())

	if h.callCount() != 0 {
		t.Errorf("a live-owned task was run (%d step calls), want 0", h.callCount())
	}
}

func TestWorkerIdleWhenNothingRunnable(t *testing.T) {
	store := newFakeStore()
	w := testWorker(store, DefaultRegistry())
	ran, err := w.runOne(t.Context())
	if err != nil {
		t.Fatalf("runOne: %v", err)
	}
	if ran {
		t.Error("runOne reported work on an empty store, want idle")
	}
}

func TestWorkerRunsNoopRegistryEndToEnd(t *testing.T) {
	// The DefaultRegistry's noop step should drive a task to completion — the
	// minimal end-to-end exercise of the executor.
	store := newFakeStore()
	w := testWorker(store, DefaultRegistry())
	task, steps := pendingTask(KindNoop)
	store.seed(task, steps)

	w.drain(t.Context())

	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusSucceeded {
		t.Fatalf("noop task status = %q, want succeeded", got.Status)
	}
}
