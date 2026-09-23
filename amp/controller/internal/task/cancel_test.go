package task

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These cover cooperative cancellation (`tasks.cancel_requested_at`): the unfenced store write
// an API caller makes, and what the worker does with a request it finds at a step
// boundary.

func TestWorkerCancelBetweenStepsFailsTaskWithACancelReason(t *testing.T) {
	store := NewMemStore()
	task, steps := pendingTask("k0", "k1")
	h0 := &fakeStep{kind: "k0"}
	h1 := &fakeStep{kind: "k1"}
	// The request lands while step 0 is running, which is the realistic case: an
	// operator cancels work already in flight.
	h0.before = func() {
		if err := store.RequestCancel(t.Context(), task.ID); err != nil {
			t.Errorf("RequestCancel: %v", err)
		}
	}
	w := testWorker(store, regWith(h0, h1))
	store.Seed(task, steps)

	claimAndExecute(t, w)

	if h0.callCount() != 1 {
		t.Fatalf("step 0 ran %d time(s), want 1 — the step in flight when a cancel lands is not interrupted", h0.callCount())
	}
	if h1.callCount() != 0 {
		t.Errorf("step 1 ran %d time(s) after the cancel, want 0", h1.callCount())
	}
	got, _ := store.Get(t.Context(), task.ID)
	if got.Status != StatusFailed {
		t.Fatalf("task status = %q, want failed — a cancelled task must go terminal or the worker re-claims it", got.Status)
	}
	if got.Error == nil || !strings.Contains(*got.Error, ErrCancelled.Error()) {
		t.Errorf("task error = %v, want it to carry %q so a cancel is distinguishable from a real failure", got.Error, ErrCancelled)
	}
	if !got.CancelRequested() {
		t.Error("the cancelled task carries no cancel_requested_at")
	}
}

func TestWorkerCancelAbandonsPendingChildren(t *testing.T) {
	store := NewMemStore()
	subs := store.Subtasks()
	h := &fakeStep{kind: "k0"}
	w := testWorker(store, regWith(h))

	parent, steps := pendingTask("k0")
	store.Seed(parent, steps)
	// A fan-out parent creates every child up front. A terminal parent is never
	// reclaimed, so a child left pending under one is a row nothing will ever run.
	child, err := subs.Create(t.Context(), parent.ID, "k0", json.RawMessage(`{"endpoint":"a"}`))
	if err != nil {
		t.Fatalf("create the child: %v", err)
	}
	if err := store.RequestCancel(t.Context(), parent.ID); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	claimAndExecute(t, w)

	children, err := subs.ByParent(t.Context(), parent.ID)
	if err != nil || len(children) != 1 {
		t.Fatalf("ByParent: children=%v err=%v, want the one child back", children, err)
	}
	if children[0].Status != StatusFailed {
		t.Fatalf("child %s is %q after its parent was cancelled, want failed — a pending child under a terminal parent is an orphan",
			child.ID, children[0].Status)
	}
	if children[0].Error == nil || !strings.Contains(*children[0].Error, ErrCancelled.Error()) {
		t.Errorf("child error = %v, want the cancellation as its reason", children[0].Error)
	}
}

func TestRequestCancelRefusesATerminalTask(t *testing.T) {
	store := NewMemStore()
	done, _ := pendingTask()
	done.Status = StatusSucceeded
	store.Seed(done, nil)

	if err := store.RequestCancel(t.Context(), done.ID); !errors.Is(err, ErrTaskTerminal) {
		t.Errorf("RequestCancel on a succeeded task = %v, want ErrTaskTerminal", err)
	}
	if err := store.RequestCancel(t.Context(), uuid.New()); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("RequestCancel on an unknown task = %v, want ErrTaskNotFound", err)
	}
	got, _ := store.Get(t.Context(), done.ID)
	if got.CancelRequested() {
		t.Error("a refused cancel still stamped the task")
	}
}

// The stamp records when the FIRST request landed: an operator re-issuing a
// cancel while the worker unwinds must not keep pushing it forward.
func TestRequestCancelKeepsTheFirstTimestamp(t *testing.T) {
	store := NewMemStore()
	tk, _ := pendingTask()
	store.Seed(tk, nil)

	if err := store.RequestCancel(t.Context(), tk.ID); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	first, _ := store.Get(t.Context(), tk.ID)
	time.Sleep(time.Millisecond)
	if err := store.RequestCancel(t.Context(), tk.ID); err != nil {
		t.Fatalf("second RequestCancel: %v", err)
	}
	second, _ := store.Get(t.Context(), tk.ID)
	if !second.CancelRequestedAt.Equal(*first.CancelRequestedAt) {
		t.Errorf("cancel_requested_at moved from %v to %v on a repeat request",
			first.CancelRequestedAt, second.CancelRequestedAt)
	}
}

// The write is not owner-fenced: an API caller has no owner, and a pending task
// has none either, so a fence would make it a permanent no-op on the cases that
// matter most.
func TestRequestCancelIsNotOwnerFenced(t *testing.T) {
	store := NewMemStore()
	pending, _ := pendingTask()
	store.Seed(pending, nil)
	if err := store.RequestCancel(t.Context(), pending.ID); err != nil {
		t.Fatalf("RequestCancel on an ownerless pending task: %v", err)
	}

	claimed, _ := pendingTask()
	owner := "some-other-worker"
	claimed.Status = StatusRunning
	claimed.Owner = &owner
	store.Seed(claimed, nil)
	if err := store.RequestCancel(t.Context(), claimed.ID); err != nil {
		t.Fatalf("RequestCancel on a task owned by another process: %v", err)
	}
	got, _ := store.Get(t.Context(), claimed.ID)
	if !got.CancelRequested() {
		t.Error("the write was fenced out: the task owned by another process was not stamped")
	}
}
