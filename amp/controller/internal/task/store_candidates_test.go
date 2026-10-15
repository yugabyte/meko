package task

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// The claim contract MemStore has to mirror, because a divergence here is a unit
// test that passes while production behaves differently. PgStore's half of each is
// its SQL; these pin the model against it.

func TestCandidatesAreOldestFirst(t *testing.T) {
	store := NewMemStore()
	now := time.Now()

	// Seeded newest-first, so insertion order is the wrong answer.
	var ids []uuid.UUID
	for i, age := range []time.Duration{0, time.Hour, 2 * time.Hour} {
		task, steps := pendingTask("k0")
		task.CreatedAt = now.Add(-age)
		store.Seed(task, steps)
		ids = append(ids, task.ID)
		_ = i
	}

	got, err := store.Candidates(t.Context(), claimCandidates)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d candidates, want 3", len(got))
	}
	// ids[2] is the oldest, ids[0] the newest.
	want := []uuid.UUID{ids[2], ids[1], ids[0]}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("candidate %d = %s, want %s — Candidates must be oldest-first, "+
				"as PgStore's ORDER BY created_at is; otherwise the oldest work starves",
				i, got[i].ID, want[i])
		}
	}
}

func TestCandidatesHonoursLimit(t *testing.T) {
	store := NewMemStore()
	for range 5 {
		task, steps := pendingTask("k0")
		store.Seed(task, steps)
	}

	for _, tc := range []struct{ limit, want int }{
		{limit: 2, want: 2},
		{limit: 5, want: 5},
		{limit: 99, want: 5},
		{limit: 0, want: 0}, // as SQL LIMIT 0 does
	} {
		got, err := store.Candidates(t.Context(), tc.limit)
		if err != nil {
			t.Fatalf("Candidates(limit=%d): %v", tc.limit, err)
		}
		if len(got) != tc.want {
			t.Errorf("Candidates(limit=%d) returned %d, want %d", tc.limit, len(got), tc.want)
		}
	}
}

// Claim's status fence. It is defence in depth — Candidates already filters
// terminal rows — but it is the last thing standing between a stale candidate read
// and resurrecting a finished task.
func TestClaimRefusesATerminalTask(t *testing.T) {
	for _, status := range []Status{StatusSucceeded, StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			store := NewMemStore()
			task, steps := pendingTask("k0")
			task.Status = status
			store.Seed(task, steps)

			got, _, err := store.Claim(t.Context(), task.ID, "me")
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if got != nil {
				t.Errorf("Claim resurrected a %s task", status)
			}
			back, _ := store.Get(t.Context(), task.ID)
			if back.Status != status {
				t.Errorf("task status = %q after a refused claim, want %q unchanged", back.Status, status)
			}
		})
	}
}

// A missing task is not an error, so a candidate deleted between the read and the
// claim costs one skipped candidate rather than a claim-loop backoff.
func TestClaimOnAMissingTaskIsNotAnError(t *testing.T) {
	store := NewMemStore()
	got, _, err := store.Claim(t.Context(), uuid.New(), "me")
	if err != nil || got != nil {
		t.Errorf("Claim on a missing task = (%v, %v), want (nil, nil)", got, err)
	}
}

// What Claim overwrote is how the worker tells a takeover from a fresh claim, so
// it must be the row as it stood before the claim, not the one it writes.
// TestPgClaimReportsWhatItOverwrote runs the same against PgStore.
func TestClaimReportsWhatItOverwrote(t *testing.T) {
	store := NewMemStore()
	task, steps := pendingTask("k0")
	store.Seed(task, steps)
	assertClaimReportsWhatItOverwrote(t, store, task.ID)
}

func assertClaimReportsWhatItOverwrote(t *testing.T, store Store, id uuid.UUID) {
	t.Helper()
	for _, tc := range []struct {
		owner string
		want  Prior
	}{
		{"owner-a", Prior{Status: StatusPending}},
		{"owner-b", Prior{Status: StatusRunning, Owner: "owner-a"}},
	} {
		claimed, prior, err := store.Claim(t.Context(), id, tc.owner)
		if err != nil || claimed == nil {
			t.Fatalf("Claim as %s: task=%v err=%v", tc.owner, claimed, err)
		}
		if prior != tc.want {
			t.Errorf("Claim as %s reported %+v overwritten, want %+v", tc.owner, prior, tc.want)
		}
		if claimed.Owner == nil || *claimed.Owner != tc.owner {
			t.Errorf("Claim as %s returned owner %v, want the new one", tc.owner, claimed.Owner)
		}
	}
}

// A step's start time is the one timestamp the later transitions must not
// overwrite: it is the only record of how long the step ran.
func TestStartStepRecordsWhenTheStepStarted(t *testing.T) {
	store := NewMemStore()
	task, steps := pendingTask("k0")
	store.Seed(task, steps)
	assertStartStepRecordsStart(t, store, task.ID)
}

func assertStartStepRecordsStart(t *testing.T, store Store, id uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	if _, _, err := store.Claim(ctx, id, "owner-a"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	steps, err := store.Steps(ctx, id)
	if err != nil || len(steps) == 0 {
		t.Fatalf("Steps: %v (%d steps)", err, len(steps))
	}
	if steps[0].StartedAt != nil {
		t.Fatalf("StartedAt = %v before the step ran, want nil", steps[0].StartedAt)
	}
	if err := store.StartStep(ctx, id, steps[0].ID, "owner-a"); err != nil {
		t.Fatalf("StartStep: %v", err)
	}
	if err := store.CompleteStep(ctx, id, steps[0].ID, "owner-a", nil); err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}
	steps, err = store.Steps(ctx, id)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	st := steps[0]
	if st.StartedAt == nil {
		t.Fatal("StartedAt = nil after the step ran and completed")
	}
	if st.StartedAt.After(st.UpdatedAt) {
		t.Errorf("StartedAt %v is after the completion's UpdatedAt %v", st.StartedAt, st.UpdatedAt)
	}
}
