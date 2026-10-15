package task

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeRecorder captures the task-lifecycle events the worker emits, so a test
// can assert instrumentation without pulling in the metrics package.
type fakeRecorder struct {
	mu        sync.Mutex
	started   int
	finished  []finishedEvent
	retried   []string
	reclaimed int
	pickups   []string
	steps     []string
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

func (r *fakeRecorder) TaskReclaimed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reclaimed++
}

// reclaims reads the count under the lock, for a worker running on another goroutine.
func (r *fakeRecorder) reclaims() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reclaimed
}

func (r *fakeRecorder) StepRetried(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retried = append(r.retried, kind)
}

func (r *fakeRecorder) TaskPickedUp(kind string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pickups = append(r.pickups, kind)
}

func (r *fakeRecorder) StepFinished(kind string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, kind)
}

func (r *fakeRecorder) TaskFinished(kind, status string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = append(r.finished, finishedEvent{kind: kind, status: status})
}

// runViaRecorder claims the seeded task and drives it through runTask (the path
// the claim loop uses, where instrumentation lives), returning the recorder.
func runViaRecorder(t *testing.T, w *Worker) *fakeRecorder {
	t.Helper()
	claimed, lease, err := w.claim(t.Context())
	if err != nil || claimed == nil {
		t.Fatalf("claim: task=%v err=%v, want a task", claimed, err)
	}
	w.runTask(t.Context(), claimed, lease)
	return w.metrics.(*fakeRecorder)
}

func TestWorkerRecordsSucceededTaskMetrics(t *testing.T) {
	store := NewMemStore()
	w := testWorker(store, regWith(&fakeStep{kind: "k0"}))
	w.metrics = &fakeRecorder{}

	task, steps := pendingTask("k0")
	store.Seed(task, steps)

	rec := runViaRecorder(t, w)

	if rec.started != 1 {
		t.Fatalf("TaskStarted calls = %d, want 1", rec.started)
	}
	if len(rec.finished) != 1 {
		t.Fatalf("TaskFinished calls = %d, want 1", len(rec.finished))
	}
	if got := rec.finished[0]; got.kind != "test" || got.status != "succeeded" {
		t.Fatalf("finished event = %+v, want {kind:test status:succeeded}", got)
	}
	if len(rec.pickups) != 1 || rec.pickups[0] != "test" {
		t.Fatalf("TaskPickedUp kinds = %v, want [test]", rec.pickups)
	}
	if len(rec.steps) != 1 || rec.steps[0] != "k0" {
		t.Fatalf("StepFinished kinds = %v, want [k0]", rec.steps)
	}
}

func TestWorkerRecordsFailedTaskMetrics(t *testing.T) {
	store := NewMemStore()
	w := testWorker(store, regWith(&fakeStep{kind: "kfail", failWith: errors.New("boom")}))
	w.metrics = &fakeRecorder{}

	task, steps := pendingTask("kfail")
	store.Seed(task, steps)

	rec := runViaRecorder(t, w)

	if len(rec.finished) != 1 || rec.finished[0].status != "failed" {
		t.Fatalf("finished events = %+v, want one with status failed", rec.finished)
	}
}

// Only work another process left running is a reclaim. The MemStore models no
// difference between a lease that lapsed and one released by a graceful stop, and
// neither does the worker: both are a takeover and both count. Only a first claim
// observes a pickup: a reclaim's wait would include the previous owner's work.
func TestWorkerCountsATakeoverAsAReclaim(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      Status
		owner       func(w *Worker) *string
		want        int
		wantPickups int
	}{
		{"pending", StatusPending, func(*Worker) *string { return nil }, 0, 1},
		{"left running by another process", StatusRunning, func(*Worker) *string { o := "dead-owner"; return &o }, 1, 0},
		{"left running by this process", StatusRunning, func(w *Worker) *string { return &w.owner }, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemStore()
			w := testWorker(store, regWith(&fakeStep{kind: "k0"}))
			w.metrics = &fakeRecorder{}

			id, s0 := uuid.New(), uuid.New()
			store.Seed(
				&Task{ID: id, Kind: "test", Status: tc.status, Owner: tc.owner(w), CreatedAt: time.Now(), StepIDs: []uuid.UUID{s0}},
				[]*StepRecord{{ID: s0, TaskID: id, Kind: "k0", Status: StatusPending}},
			)

			rec := runViaRecorder(t, w)
			if rec.reclaimed != tc.want {
				t.Errorf("TaskReclaimed calls = %d, want %d", rec.reclaimed, tc.want)
			}
			if len(rec.pickups) != tc.wantPickups {
				t.Errorf("TaskPickedUp calls = %d, want %d", len(rec.pickups), tc.wantPickups)
			}
		})
	}
}

// staleCandidates reports every candidate as still pending and unowned, the view
// a worker gets when another process claimed and released the task between its
// candidate read and its acquire.
type staleCandidates struct{ *MemStore }

func (s staleCandidates) Candidates(ctx context.Context, limit int) ([]*Task, error) {
	cands, err := s.MemStore.Candidates(ctx, limit)
	for _, c := range cands {
		c.Status, c.Owner = StatusPending, nil
	}
	return cands, err
}

// A lease given back on SIGTERM frees the task at once, so the candidate can be
// out of date without any lease lapsing. The takeover still counts.
func TestWorkerCountsATakeoverTheCandidateReadMissed(t *testing.T) {
	mem := NewMemStore()
	w := testWorkerMax(staleCandidates{mem}, mem.Locks(ownerID()), regWith(&fakeStep{kind: "k0"}), 8)
	w.metrics = &fakeRecorder{}

	id, s0 := uuid.New(), uuid.New()
	owner := "released-owner"
	mem.Seed(
		&Task{ID: id, Kind: "test", Status: StatusRunning, Owner: &owner, CreatedAt: time.Now(), StepIDs: []uuid.UUID{s0}},
		[]*StepRecord{{ID: s0, TaskID: id, Kind: "k0", Status: StatusPending}},
	)

	if rec := runViaRecorder(t, w); rec.reclaimed != 1 {
		t.Errorf("TaskReclaimed calls = %d, want 1", rec.reclaimed)
	}
}

// TestWorkerNilMetricsNoPanic guards the nil-recorder path the default worker
// (and the tests above that don't set metrics) rely on.
func TestWorkerNilMetricsNoPanic(t *testing.T) {
	store := NewMemStore()
	w := testWorker(store, regWith(&fakeStep{kind: "k0"})) // no metrics set

	task, steps := pendingTask("k0")
	store.Seed(task, steps)

	claimed, lease, err := w.claim(t.Context())
	if err != nil || claimed == nil {
		t.Fatalf("claim: task=%v err=%v", claimed, err)
	}
	w.runTask(t.Context(), claimed, lease) // must not panic with a nil recorder
}
