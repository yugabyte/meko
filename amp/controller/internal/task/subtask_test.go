package task

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// These cover the subtask store (subtask.go) against MemSubtasks: the derived id
// that makes a replayed parent converge rather than double-create (§5.5), the
// owner fence on the three transitions, and the deliberate asymmetry with
// Store.FailStep — a failed child does not fail its parent.

// claimedParent seeds a parent task and claims it under owner, which is what the
// child write fence reads.
func claimedParent(t *testing.T, store *MemStore, owner string) *Task {
	t.Helper()
	parent, _ := pendingTask()
	store.Seed(parent, nil)
	claimed, _, err := store.Claim(t.Context(), parent.ID, owner)
	if err != nil || claimed == nil {
		t.Fatalf("claim parent: task=%v err=%v", claimed, err)
	}
	return claimed
}

// TestSubtaskIDIsDerivedFromParentKindAndArgs pins the property the whole replay
// story rests on: the id is a pure function of the work, so two Creates of the
// same child agree and two Creates of different children do not. Minting the id
// with uuid.New() satisfies neither half.
func TestSubtaskIDIsDerivedFromParentKindAndArgs(t *testing.T) {
	parent, other := uuid.New(), uuid.New()
	args := json.RawMessage(`{"endpoint_id":"e1","version":"v2"}`)

	if a, b := subtaskID(parent, StepUpgradeEndpoint, args), subtaskID(parent, StepUpgradeEndpoint, args); a != b {
		t.Fatalf("subtaskID is not deterministic: %s != %s", a, b)
	}
	for _, tc := range []struct {
		name string
		id   uuid.UUID
	}{
		{"different args", subtaskID(parent, StepUpgradeEndpoint, json.RawMessage(`{"endpoint_id":"e2","version":"v2"}`))},
		{"different kind", subtaskID(parent, StepResizeEndpoint, args)},
		{"different parent", subtaskID(other, StepUpgradeEndpoint, args)},
	} {
		if tc.id == subtaskID(parent, StepUpgradeEndpoint, args) {
			t.Errorf("%s: subtaskID collided with the base child (%s)", tc.name, tc.id)
		}
	}
}

// TestSubtaskCreateConvergesOnTheDerivedID is the double-create hazard directly:
// a reclaimed parent re-runs its step from the top and re-creates every child it
// planned. With a minted id that second pass would give the endpoint a second
// child, and the roll a second helm upgrade of the same release.
func TestSubtaskCreateConvergesOnTheDerivedID(t *testing.T) {
	store := NewMemStore()
	subs := store.Subtasks()
	parent := claimedParent(t, store, "owner-a")
	args := json.RawMessage(`{"endpoint_id":"e1","version":"v2"}`)

	first, err := subs.Create(t.Context(), parent.ID, StepUpgradeEndpoint, args)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := subs.Create(t.Context(), parent.ID, StepUpgradeEndpoint, args)
	if err != nil {
		t.Fatalf("replayed Create: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("replayed Create id = %s, want the original %s", second.ID, first.ID)
	}
	rows, err := subs.ByParent(t.Context(), parent.ID)
	if err != nil {
		t.Fatalf("ByParent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("subtasks after a replayed Create = %d, want 1", len(rows))
	}
	// A second endpoint is a second child, so convergence must not be "one child
	// per parent".
	if _, err := subs.Create(t.Context(), parent.ID, StepUpgradeEndpoint,
		json.RawMessage(`{"endpoint_id":"e2","version":"v2"}`)); err != nil {
		t.Fatalf("Create for a second endpoint: %v", err)
	}
	if rows, _ := subs.ByParent(t.Context(), parent.ID); len(rows) != 2 {
		t.Fatalf("subtasks for two endpoints = %d, want 2", len(rows))
	}
}

// TestSubtaskCreateReplayReturnsTheReachedStatus is how a reclaimed parent knows
// what it already did (§5.5 step 2): the child's own status, handed back by the
// idempotent Create. A Create that reset the row to pending would re-run every
// child of the roll.
func TestSubtaskCreateReplayReturnsTheReachedStatus(t *testing.T) {
	store := NewMemStore()
	subs := store.Subtasks()
	parent := claimedParent(t, store, "owner-a")

	sub, err := subs.Create(t.Context(), parent.ID, StepUpgradeEndpoint, json.RawMessage(`{"endpoint_id":"e1"}`))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := subs.Complete(t.Context(), sub.ID, "owner-a", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	replayed, err := subs.Create(t.Context(), parent.ID, StepUpgradeEndpoint, json.RawMessage(`{"endpoint_id":"e1"}`))
	if err != nil {
		t.Fatalf("replayed Create: %v", err)
	}
	if replayed.Status != StatusSucceeded {
		t.Fatalf("replayed Create status = %q, want succeeded", replayed.Status)
	}
	if string(replayed.Result) != `{"ok":true}` {
		t.Fatalf("replayed Create result = %q, want the recorded one", replayed.Result)
	}
}

// TestSubtaskCreateNormalizesEmptyArgs pins that the stored args and the bytes the
// id was derived from are the same: an id derived before normalization would not
// match the row a later Create with explicit `{}` converges on.
func TestSubtaskCreateNormalizesEmptyArgs(t *testing.T) {
	store := NewMemStore()
	subs := store.Subtasks()
	parent := claimedParent(t, store, "owner-a")

	empty, err := subs.Create(t.Context(), parent.ID, KindNoop, nil)
	if err != nil {
		t.Fatalf("Create with nil args: %v", err)
	}
	if string(empty.Args) != "{}" {
		t.Errorf("stored args = %q, want {}", empty.Args)
	}
	explicit, err := subs.Create(t.Context(), parent.ID, KindNoop, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Create with explicit {}: %v", err)
	}
	if explicit.ID != empty.ID {
		t.Fatalf("explicit {} id = %s, want the same row as nil args (%s)", explicit.ID, empty.ID)
	}
}

// TestSubtaskTransitionsAreOwnerFenced: a process that no longer owns the parent
// must not write its children — and, like every task write, the refusal is a
// silent no-op rather than an error, because the work is another instance's now
// and there is nothing to report.
func TestSubtaskTransitionsAreOwnerFenced(t *testing.T) {
	store := NewMemStore()
	subs := store.Subtasks()
	parent := claimedParent(t, store, "owner-a")
	sub, err := subs.Create(t.Context(), parent.ID, KindNoop, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The parent is reclaimed; owner-a's writes are now stale.
	if claimed, _, err := store.Claim(t.Context(), parent.ID, "owner-b"); err != nil || claimed == nil {
		t.Fatalf("reclaim: task=%v err=%v", claimed, err)
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"Start", subs.Start(t.Context(), sub.ID, "owner-a")},
		{"Complete", subs.Complete(t.Context(), sub.ID, "owner-a", json.RawMessage(`{"ok":true}`))},
		{"Fail", subs.Fail(t.Context(), sub.ID, "owner-a", errors.New("boom"))},
	} {
		if tc.err != nil {
			t.Errorf("fenced-out %s = %v, want nil (a silent no-op)", tc.name, tc.err)
		}
	}
	got := onlySubtask(t, subs, parent.ID)
	if got.Status != StatusPending || got.Owner != nil {
		t.Fatalf("subtask after fenced-out writes = status %q owner %v, want pending and unowned",
			got.Status, got.Owner)
	}
	// The new owner still writes.
	if err := subs.Start(t.Context(), sub.ID, "owner-b"); err != nil {
		t.Fatalf("Start under the new owner: %v", err)
	}
	if got := onlySubtask(t, subs, parent.ID); got.Status != StatusRunning || got.Owner == nil || *got.Owner != "owner-b" {
		t.Fatalf("subtask after the new owner started it = status %q owner %v, want running/owner-b",
			got.Status, got.Owner)
	}
}

// TestSubtaskFailLeavesTheParentAlone pins the deliberate asymmetry with
// Store.FailStep, which fails a step AND its task. A child that also failed its
// parent would destroy exactly what the parent's step exists to report: which
// endpoints succeeded, which failed, and which were never attempted (§5.4).
func TestSubtaskFailLeavesTheParentAlone(t *testing.T) {
	store := NewMemStore()
	subs := store.Subtasks()
	parent := claimedParent(t, store, "owner-a")
	sub, err := subs.Create(t.Context(), parent.ID, KindNoop, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := subs.Fail(t.Context(), sub.ID, "owner-a", errors.New("helm timed out")); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	got := onlySubtask(t, subs, parent.ID)
	if got.Status != StatusFailed || got.Error == nil || *got.Error != "helm timed out" {
		t.Fatalf("subtask = status %q error %v, want failed with the cause", got.Status, got.Error)
	}
	gotParent, err := store.Get(t.Context(), parent.ID)
	if err != nil {
		t.Fatalf("Get parent: %v", err)
	}
	if gotParent.Status != StatusRunning {
		t.Fatalf("parent status = %q, want running (the child's failure is the parent step's to report)",
			gotParent.Status)
	}
}

// TestSubtaskByParentIsScopedAndOrdered: one roll's children must not turn up in
// another's reconciliation, and the order is creation order so the parent's
// per-endpoint report is stable across replays.
func TestSubtaskByParentIsScopedAndOrdered(t *testing.T) {
	store := NewMemStore()
	subs := store.Subtasks()
	first := claimedParent(t, store, "owner-a")
	second := claimedParent(t, store, "owner-a")

	var want []uuid.UUID
	for _, ep := range []string{"e1", "e2", "e3"} {
		sub, err := subs.Create(t.Context(), first.ID, StepUpgradeEndpoint,
			json.RawMessage(`{"endpoint_id":"`+ep+`"}`))
		if err != nil {
			t.Fatalf("Create %s: %v", ep, err)
		}
		want = append(want, sub.ID)
	}
	if _, err := subs.Create(t.Context(), second.ID, StepUpgradeEndpoint, json.RawMessage(`{"endpoint_id":"e9"}`)); err != nil {
		t.Fatalf("Create under the second parent: %v", err)
	}

	rows, err := subs.ByParent(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("ByParent: %v", err)
	}
	if len(rows) != len(want) {
		t.Fatalf("ByParent returned %d rows, want %d", len(rows), len(want))
	}
	for i, sub := range rows {
		if sub.ID != want[i] {
			t.Fatalf("row %d = %s, want %s (creation order)", i, sub.ID, want[i])
		}
		if sub.ParentID != first.ID {
			t.Fatalf("row %d belongs to %s, want %s", i, sub.ParentID, first.ID)
		}
	}
}

// TestSubtasksAreNotClaimCandidates is §5.2's whole point, asserted at the seam:
// the claim loop reads `tasks`, subtasks are not in it, so nothing can claim a
// child out from under the parent already running it. Nothing in the claim query
// was changed to achieve that — the schema is what rules it out.
func TestSubtasksAreNotClaimCandidates(t *testing.T) {
	store := NewMemStore()
	subs := store.Subtasks()
	parent := claimedParent(t, store, "owner-a")
	if _, err := subs.Create(t.Context(), parent.ID, StepUpgradeEndpoint, json.RawMessage(`{"endpoint_id":"e1"}`)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cands, err := store.Candidates(t.Context(), 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	for _, c := range cands {
		if c.ID != parent.ID {
			t.Fatalf("Candidates offered %s, which is not the parent task %s", c.ID, parent.ID)
		}
	}
}

// onlySubtask reads back the single subtask of a parent.
func onlySubtask(t *testing.T, subs SubtaskStore, parentID uuid.UUID) *Subtask {
	t.Helper()
	rows, err := subs.ByParent(t.Context(), parentID)
	if err != nil {
		t.Fatalf("ByParent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("subtasks = %d, want exactly 1", len(rows))
	}
	return rows[0]
}
