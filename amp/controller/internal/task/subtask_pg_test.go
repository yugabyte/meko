package task

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestPgSubtasks is PgSubtasks against a real database, where the MemSubtasks
// mirror cannot help: nothing in `go test ./...` executes a line of this SQL, so
// a typo, a column that does not exist, or a fence whose correlated subquery
// resolves `owner` to the wrong table would all pass unit tests. It covers the
// two things the SQL — rather than the Go — is responsible for: the INSERT ...
// ON CONFLICT (id) DO NOTHING that makes a replayed Create converge, and the
// EXISTS-on-`tasks` fence on each transition.
//
// Same gate as TestPgResizeLock: skips unless AMP_TEST_DATABASE_URL points at a
// YugabyteDB.
func TestPgSubtasks(t *testing.T) {
	pool, ctx := migratedPool(t)
	// Registered before the row cleanup below so it runs after it (cleanups are
	// LIFO) — a deferred Close here would close the pool the cleanup still needs.
	t.Cleanup(pool.Close)
	tasks := NewPgStore(pool)
	subs := NewPgSubtasks(pool)

	ref := uuid.NewString()
	t.Cleanup(func() {
		// t.Context() is already cancelled by the time cleanups run.
		cleanupCtx := context.WithoutCancel(ctx)
		// subtasks has an unqualified FK to tasks, so the children go first — as
		// task_steps does.
		for _, q := range []string{
			`DELETE FROM subtasks WHERE parent_id IN (SELECT id FROM tasks WHERE target_ref = $1)`,
			`DELETE FROM task_steps WHERE task_id IN (SELECT id FROM tasks WHERE target_ref = $1)`,
			`DELETE FROM tasks WHERE target_ref = $1`,
		} {
			if _, err := pool.Exec(cleanupCtx, q, ref); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})

	parent, err := tasks.Create(ctx, TaskSpec{Kind: KindUpgradeEndpoints, TargetRef: &ref})
	if err != nil {
		t.Fatalf("create parent task: %v", err)
	}
	const owner = "TestPgSubtasks"
	if claimed, err := tasks.Claim(ctx, parent.ID, owner); err != nil || claimed == nil {
		t.Fatalf("claim parent: task=%v err=%v", claimed, err)
	}

	args := json.RawMessage(`{"endpoint_id":"e1","version":"v2"}`)
	first, err := subs.Create(ctx, parent.ID, StepUpgradeEndpoint, args)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The replay: ON CONFLICT DO NOTHING writes nothing, and the read-back is what
	// returns the original row.
	second, err := subs.Create(ctx, parent.ID, StepUpgradeEndpoint, args)
	if err != nil {
		t.Fatalf("replayed Create: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("replayed Create id = %s, want %s", second.ID, first.ID)
	}
	if rows, err := subs.ByParent(ctx, parent.ID); err != nil || len(rows) != 1 {
		t.Fatalf("ByParent = %d rows, err=%v, want exactly 1", len(rows), err)
	}

	// A stale owner's writes are refused by the EXISTS-on-tasks fence, silently.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"Start", subs.Start(ctx, first.ID, "someone-else")},
		{"Complete", subs.Complete(ctx, first.ID, "someone-else", json.RawMessage(`{"ok":true}`))},
		{"Fail", subs.Fail(ctx, first.ID, "someone-else", errors.New("boom"))},
	} {
		if tc.err != nil {
			t.Errorf("fenced-out %s = %v, want nil", tc.name, tc.err)
		}
	}
	if got := pgSubtask(t, ctx, subs, parent.ID); got.Status != StatusPending {
		t.Fatalf("status after fenced-out writes = %q, want pending", got.Status)
	}

	if err := subs.Start(ctx, first.ID, owner); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := pgSubtask(t, ctx, subs, parent.ID); got.Status != StatusRunning || got.Owner == nil || *got.Owner != owner {
		t.Fatalf("after Start = status %q owner %v, want running/%s", got.Status, got.Owner, owner)
	}
	if err := subs.Complete(ctx, first.ID, owner, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := pgSubtask(t, ctx, subs, parent.ID); got.Status != StatusSucceeded || string(got.Result) != `{"ok": true}` {
		t.Fatalf("after Complete = status %q result %s, want succeeded with the result", got.Status, got.Result)
	}

	// Fail records the cause on the child and leaves the parent alone.
	if err := subs.Fail(ctx, first.ID, owner, errors.New("helm timed out")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if got := pgSubtask(t, ctx, subs, parent.ID); got.Status != StatusFailed || got.Error == nil || *got.Error != "helm timed out" {
		t.Fatalf("after Fail = status %q error %v, want failed with the cause", got.Status, got.Error)
	}
	gotParent, err := tasks.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get parent: %v", err)
	}
	if gotParent.Status != StatusRunning {
		t.Fatalf("parent status = %q, want running (a child's failure is the parent step's to report)",
			gotParent.Status)
	}
}

func pgSubtask(t *testing.T, ctx context.Context, subs SubtaskStore, parentID uuid.UUID) *Subtask {
	t.Helper()
	rows, err := subs.ByParent(ctx, parentID)
	if err != nil {
		t.Fatalf("ByParent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("subtasks = %d, want exactly 1", len(rows))
	}
	return rows[0]
}
