package task

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestPgResizeLock is the resize lock against a real database, where the
// MemStore mirror (TestResizeLock) cannot help: MemStore rejects a concurrent
// resize with a plain in-memory check, so it exercises none of the SQLSTATE →
// ErrResizeInProgress mapping that #400 was about. The mapping only runs when a
// live partial unique index raises the violation, and only YSQL's flavour of that
// error exposed the bug.
//
// Needs YugabyteDB (migrations 0002/0003 use YB-specific HASH / SPLIT DDL that
// plain Postgres rejects), so it skips unless AMP_TEST_DATABASE_URL points at
// one — the same gate as TestMigrateRoundTrip.
func TestPgResizeLock(t *testing.T) {
	pool, ctx := migratedPool(t)
	// Registered before the row cleanup below so it runs after it (cleanups are
	// LIFO) — a deferred Close here would close the pool the cleanup still needs.
	t.Cleanup(pool.Close)
	s := NewPgStore(pool)

	// Fresh endpoint ids per run, so repeated runs against the same database
	// neither collide nor depend on cleanup having happened.
	ep, other := uuid.NewString(), uuid.NewString()
	refs := []string{ep, other}
	t.Cleanup(func() {
		// t.Context() is already cancelled by the time cleanups run, so the
		// deletes need a context of their own.
		cleanupCtx := context.WithoutCancel(ctx)
		// task_step has an unqualified FK to task, so the steps go first.
		for _, q := range []string{
			`DELETE FROM task_step WHERE task_id IN (SELECT id FROM task WHERE target_ref = ANY($1))`,
			`DELETE FROM task WHERE target_ref = ANY($1)`,
		} {
			if _, err := pool.Exec(cleanupCtx, q, refs); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})

	resizeSpec := func(ref string) TaskSpec {
		r := ref
		return TaskSpec{
			Kind:      KindResizeEndpoint,
			TargetRef: &r,
			Steps:     []StepSpec{{Kind: StepResizeEndpoint, Args: json.RawMessage(`{}`)}},
		}
	}

	first, err := s.Create(ctx, resizeSpec(ep))
	if err != nil {
		t.Fatalf("first resize: %v", err)
	}

	// The regression: before the fix this came back as a wrapped raw
	// "insert task: … (SQLSTATE 23505)", which the API renders as 500 rather than
	// the documented 409.
	if _, err := s.Create(ctx, resizeSpec(ep)); !errors.Is(err, ErrResizeInProgress) {
		t.Fatalf("concurrent resize err = %v, want ErrResizeInProgress", err)
	}

	// The lock is per endpoint, not global.
	if _, err := s.Create(ctx, resizeSpec(other)); err != nil {
		t.Fatalf("resize of a different endpoint: %v", err)
	}

	// And it self-releases: a terminal task drops out of the index's predicate.
	// Terminating a task goes through the worker's fenced path — claim it, then
	// complete it as its owner — so claim until this test's task comes up (a
	// previous failed run can leave other pending tasks in the schema).
	const owner = "TestPgResizeLock"
	claimed := false
	for range 10 {
		next, err := s.ClaimNext(ctx, owner, time.Minute)
		if err != nil {
			t.Fatalf("claim next task: %v", err)
		}
		if next == nil {
			break
		}
		if next.ID == first.ID {
			claimed = true
			break
		}
	}
	if !claimed {
		t.Fatalf("never claimed the in-flight resize task %s", first.ID)
	}
	if err := s.CompleteTask(ctx, first.ID, owner); err != nil {
		t.Fatalf("complete first resize: %v", err)
	}
	if _, err := s.Create(ctx, resizeSpec(ep)); err != nil {
		t.Fatalf("resize after the holder reached terminal = %v, want admitted", err)
	}
}

// TestPgUniqueConstraintsOnTask pins the assumption isResizeLockViolation rests
// on: the only unique constraints on `task` are its primary key — whose conflict
// Create already absorbs with ON CONFLICT (id) DO NOTHING — and the resize lock.
// That is what makes "a unique violation from Create's task INSERT, for a
// resize_endpoint task" an exact identification of the lock without reading the
// constraint's name out of the error (which YSQL does not expose in any field).
//
// If this fails because a new unique index was added to `task`, isResizeLockViolation
// has to start discriminating between them — and it cannot do that by name on
// YSQL, so the new index needs its own structural signal (e.g. classify before
// the insert, or split the INSERT).
func TestPgUniqueConstraintsOnTask(t *testing.T) {
	pool, ctx := migratedPool(t)
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT c.relname
		   FROM pg_index i
		   JOIN pg_class c ON c.oid = i.indexrelid
		   JOIN pg_class t ON t.oid = i.indrelid
		   JOIN pg_namespace n ON n.oid = t.relnamespace
		  WHERE t.relname = 'task' AND n.nspname = current_schema() AND i.indisunique
		  ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("list unique indexes on task: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate indexes: %v", err)
	}

	want := []string{resizeLockConstraint, "task_pkey"} // ORDER BY relname
	if !slices.Equal(got, want) {
		t.Fatalf("unique indexes on task = %v, want exactly %v — see this test's doc comment", got, want)
	}
}

// migratedPool applies the controller migrations to a schema of this package's
// own and returns a pool pinned to it, so a shared scratch database's
// `controller` schema (and whatever version TestMigrateRoundTrip left it at) is
// never touched. It skips the test when there is no database to talk to.
func migratedPool(t *testing.T) (*db.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AMP_TEST_DATABASE_URL not set; these tests need a YugabyteDB")
	}
	const schema = "task_store_test"
	ctx := t.Context()
	if err := db.Migrate(ctx, url, schema); err != nil {
		t.Fatalf("migrate %s: %v", schema, err)
	}
	pool, err := db.Connect(ctx, url, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return pool, ctx
}
