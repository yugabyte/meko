package task

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeRecorder captures the task-lifecycle events the worker emits, so a test
// can assert instrumentation without pulling in the metrics package.
type fakeRecorder struct {
	mu       sync.Mutex
	started  int
	finished []finishedEvent
}

type finishedEvent struct {
	kind   string
	status string
}

func (r *fakeRecorder) TaskStarted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started++
}

func (r *fakeRecorder) TaskFinished(kind, status string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = append(r.finished, finishedEvent{kind: kind, status: status})
}

// runViaRecorder claims the seeded task and drives it through runTask (the path
// the claim loop uses, where instrumentation lives), returning the recorder.
func runViaRecorder(t *testing.T, w *Worker, store Store) *fakeRecorder {
	t.Helper()
	claimed, err := store.ClaimNext(t.Context(), w.owner, w.cfg.LeaseTTL)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: claimed=%v err=%v, want a task", claimed, err)
	}
	w.runTask(t.Context(), claimed)
	return w.metrics.(*fakeRecorder)
}

func TestWorkerRecordsSucceededTaskMetrics(t *testing.T) {
	store := NewMemStore()
	w := testWorker(store, regWith(&fakeStep{kind: "k0"}))
	w.metrics = &fakeRecorder{}

	task, steps := pendingTask("k0")
	store.Seed(task, steps)

	rec := runViaRecorder(t, w, store)

	if rec.started != 1 {
		t.Fatalf("TaskStarted calls = %d, want 1", rec.started)
	}
	if len(rec.finished) != 1 {
		t.Fatalf("TaskFinished calls = %d, want 1", len(rec.finished))
	}
	if got := rec.finished[0]; got.kind != "test" || got.status != "succeeded" {
		t.Fatalf("finished event = %+v, want {kind:test status:succeeded}", got)
	}
}

func TestWorkerRecordsFailedTaskMetrics(t *testing.T) {
	store := NewMemStore()
	w := testWorker(store, regWith(&fakeStep{kind: "kfail", failWith: errors.New("boom")}))
	w.metrics = &fakeRecorder{}

	task, steps := pendingTask("kfail")
	store.Seed(task, steps)

	rec := runViaRecorder(t, w, store)

	if len(rec.finished) != 1 || rec.finished[0].status != "failed" {
		t.Fatalf("finished events = %+v, want one with status failed", rec.finished)
	}
}

// TestWorkerNilMetricsNoPanic guards the nil-recorder path the default worker
// (and the tests above that don't set metrics) rely on.
func TestWorkerNilMetricsNoPanic(t *testing.T) {
	store := NewMemStore()
	w := testWorker(store, regWith(&fakeStep{kind: "k0"})) // no metrics set

	task, steps := pendingTask("k0")
	store.Seed(task, steps)

	claimed, err := store.ClaimNext(t.Context(), w.owner, w.cfg.LeaseTTL)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: claimed=%v err=%v", claimed, err)
	}
	w.runTask(t.Context(), claimed) // must not panic with a nil recorder
}
