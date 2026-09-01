package task

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The controller half of meko#985: the applied YCP generation is stamped by the
// worker, after the last step succeeds, so the recorded value means the work
// landed rather than that a 202 was returned. These tests pin the two halves that
// matter — it happens on success, and it does NOT happen on failure.

// regWithState is regWith plus a state store, so the worker can reach
// Deps.State to stamp. regWith deliberately builds a deps-less registry.
func regWithState(state StateStore, steps ...*fakeStep) *Registry {
	r := &Registry{deps: Deps{State: state}, builders: map[string]Builder{}}
	for _, s := range steps {
		st := s
		r.Register(st.kind, func(json.RawMessage, Deps) (Step, error) { return st, nil })
	}
	return r
}

// ycpTask is a pending task carrying a YCP stamp for one resource.
func ycpTask(resource string, id uuid.UUID, gen int64, kinds ...string) (*Task, []*StepRecord) {
	tk, steps := pendingTask(kinds...)
	tk.YCP = &YCPTarget{Resource: resource, ID: id, Generation: gen}
	return tk, steps
}

func TestWorkerStampsYCPGenerationOnSuccess(t *testing.T) {
	cases := []struct {
		name     string
		resource string
		read     func(*fakeStateStore, uuid.UUID) (int64, bool)
	}{
		{"endpoint", YCPResourceEndpoint, func(f *fakeStateStore, id uuid.UUID) (int64, bool) {
			g, ok := f.endpointYCPGeneration[id]
			return g, ok
		}},
		{"branch", YCPResourceBranch, func(f *fakeStateStore, id uuid.UUID) (int64, bool) {
			g, ok := f.branchYCPGeneration[id]
			return g, ok
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemStore()
			state := newFakeStateStore()
			step := &fakeStep{kind: "work"}
			w := testWorkerMax(store, store.Locks(ownerID()), regWithState(state, step), 8)

			target := uuid.New()
			tk, steps := ycpTask(tc.resource, target, 7, step.kind)
			store.Seed(tk, steps)
			claimAndExecute(t, w)

			got, ok := tc.read(state, target)
			if !ok {
				t.Fatalf("no generation stamped on the %s", tc.name)
			}
			if got != 7 {
				t.Errorf("stamped generation = %d, want 7", got)
			}
			if final, _ := store.Get(t.Context(), tk.ID); final.Status != StatusSucceeded {
				t.Errorf("task status = %s, want succeeded", final.Status)
			}
		})
	}
}

// The case the whole issue is about. A step that fails must leave the generation
// unwritten, so the reconcile agent's next tick sees the row still behind and
// re-issues the update instead of believing the 202.
func TestWorkerDoesNotStampWhenAStepFails(t *testing.T) {
	store := NewMemStore()
	state := newFakeStateStore()
	step := &fakeStep{kind: "work", failWith: errors.New("k8s refused the resize")}
	w := testWorkerMax(store, store.Locks(ownerID()), regWithState(state, step), 8)

	endpoint := uuid.New()
	tk, steps := ycpTask(YCPResourceEndpoint, endpoint, 7, step.kind)
	store.Seed(tk, steps)
	claimAndExecute(t, w)

	if _, stamped := state.endpointYCPGeneration[endpoint]; stamped {
		t.Fatalf("generation stamped despite a failed step: %v", state.endpointYCPGeneration)
	}
	final, _ := store.Get(t.Context(), tk.ID)
	if final.Status != StatusFailed {
		t.Fatalf("task status = %s, want failed", final.Status)
	}
}

// Only the LAST step's success counts. A task whose first step succeeds and whose
// second fails has done part of the work, and a partially-applied generation would
// tell the agent the whole update landed.
func TestWorkerDoesNotStampOnAPartialTask(t *testing.T) {
	store := NewMemStore()
	state := newFakeStateStore()
	first := &fakeStep{kind: "first"}
	second := &fakeStep{kind: "second", failWith: errors.New("redeploy failed")}
	w := testWorkerMax(store, store.Locks(ownerID()), regWithState(state, first, second), 8)

	branch := uuid.New()
	tk, steps := ycpTask(YCPResourceBranch, branch, 3, first.kind, second.kind)
	store.Seed(tk, steps)
	claimAndExecute(t, w)

	if first.calls != 1 {
		t.Errorf("first step calls = %d, want 1", first.calls)
	}
	if _, stamped := state.branchYCPGeneration[branch]; stamped {
		t.Fatalf("generation stamped though the second step failed: %v", state.branchYCPGeneration)
	}
}

// The stamp is outside CompleteTask's transaction, so a reclaim has to converge:
// every step is already succeeded and skipped, and the monotonic write re-runs
// without regressing anything.
func TestWorkerStampIsIdempotentOnReclaim(t *testing.T) {
	store := NewMemStore()
	state := newFakeStateStore()
	step := &fakeStep{kind: "work"}
	w := testWorkerMax(store, store.Locks(ownerID()), regWithState(state, step), 8)

	endpoint := uuid.New()
	tk, steps := ycpTask(YCPResourceEndpoint, endpoint, 5, step.kind)
	steps[0].Status = StatusSucceeded // as a reclaim finds it
	tk.Status = StatusRunning
	tk.UpdatedAt = time.Now().Add(-time.Hour)
	store.Seed(tk, steps)
	claimAndExecute(t, w)

	if step.calls != 0 {
		t.Errorf("step calls = %d, want 0 — an already-succeeded step is skipped", step.calls)
	}
	if got := state.endpointYCPGeneration[endpoint]; got != 5 {
		t.Errorf("stamped generation = %d, want 5 on reclaim", got)
	}
}

// Monotonic: an older task completing after a newer one must not drag the value
// back and make the agent re-apply what already landed.
func TestWorkerStampDoesNotRegress(t *testing.T) {
	store := NewMemStore()
	state := newFakeStateStore()
	step := &fakeStep{kind: "work"}
	w := testWorkerMax(store, store.Locks(ownerID()), regWithState(state, step), 8)

	endpoint := uuid.New()
	for _, gen := range []int64{9, 4} {
		tk, steps := ycpTask(YCPResourceEndpoint, endpoint, gen, step.kind)
		store.Seed(tk, steps)
		claimAndExecute(t, w)
	}
	if got := state.endpointYCPGeneration[endpoint]; got != 9 {
		t.Errorf("stamped generation = %d, want 9 (the later, lower task must not regress it)", got)
	}
}

// An unknown resource is refused rather than skipped silently: the recorded
// generation is only trustworthy if every stamp that should have happened did. It
// cannot be reached through createWithID (see TestCreateRejectsUnknownYCPResource),
// so this covers the worker's own defence — the task does not succeed, and nothing
// is stamped.
func TestWorkerUnknownYCPResourceDoesNotSucceed(t *testing.T) {
	store := NewMemStore()
	state := newFakeStateStore()
	step := &fakeStep{kind: "work"}
	w := testWorkerMax(store, store.Locks(ownerID()), regWithState(state, step), 8)

	tk, steps := ycpTask("datapack", uuid.New(), 1, step.kind)
	store.Seed(tk, steps)
	claimAndExecute(t, w)

	final, _ := store.Get(t.Context(), tk.ID)
	if final.Status == StatusSucceeded {
		t.Fatalf("task succeeded with an unknown ycp resource, so it would read as applied")
	}
	if len(state.endpointYCPGeneration)+len(state.branchYCPGeneration) != 0 {
		t.Error("an unknown resource stamped something")
	}
}

// A task with no YCP stamp is every task an operator or the UI plans. It must run
// untouched — and notably must not need a state store at all.
func TestWorkerSkipsStampForNonYCPTasks(t *testing.T) {
	store := NewMemStore()
	step := &fakeStep{kind: "work"}
	w := testWorkerMax(store, store.Locks(ownerID()), regWith(step), 8) // deps-less registry

	tk, steps := pendingTask(step.kind)
	store.Seed(tk, steps)
	claimAndExecute(t, w)

	final, _ := store.Get(t.Context(), tk.ID)
	if final.Status != StatusSucceeded {
		t.Fatalf("task status = %s, want succeeded", final.Status)
	}
}
