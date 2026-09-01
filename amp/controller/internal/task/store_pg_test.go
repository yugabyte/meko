package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestPgTaskLocks runs EVERY first-writer-wins kind against a real database, where
// the MemStore mirror cannot help: MemStore rejects a concurrent task with a plain
// in-memory check, so it exercises none of the SQLSTATE → typed-conflict mapping
// that #400 was about. That mapping only runs when a live partial unique index
// raises the violation, and only YSQL's flavour of the error exposed the bug —
// it carries no constraint name, so the classifier keys on the kind instead.
//
// Driven by allKinds rather than naming resize, because a second locked kind has
// exactly the same exposure and would otherwise ship with only the in-memory twin:
// a wrong index predicate, or a kind whose conflict is not wired, still returns 500
// where the API documents 409, and nothing below the database can tell.
//
// Needs YugabyteDB (migration 0002 uses YB-specific HASH / SPLIT DDL that
// plain Postgres rejects), so it skips unless AMP_TEST_DATABASE_URL points at
// one — the same gate as TestMigrateRoundTrip.
func TestPgTaskLocks(t *testing.T) {
	pool, ctx := migratedPool(t)
	// Registered before the row cleanup below so it runs after it (cleanups are
	// LIFO) — a deferred Close here would close the pool the cleanup still needs.
	t.Cleanup(pool.Close)
	s := NewPgStore(pool)

	// Fresh target ids per run, appended by each subtest, so repeated runs against
	// the same database neither collide nor depend on cleanup having happened.
	var refs []string
	t.Cleanup(func() {
		// t.Context() is already cancelled by the time cleanups run, so the
		// deletes need a context of their own.
		cleanupCtx := context.WithoutCancel(ctx)
		// task_step has an unqualified FK to task, so the steps go first.
		for _, q := range []string{
			`DELETE FROM task_steps WHERE task_id IN (SELECT id FROM tasks WHERE target_ref = ANY($1))`,
			`DELETE FROM tasks WHERE target_ref = ANY($1)`,
		} {
			if _, err := pool.Exec(cleanupCtx, q, refs); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})

	var locked int
	for _, kind := range allKinds {
		if kind.lockIndex == "" {
			continue
		}
		locked++
		t.Run(kind.String(), func(t *testing.T) {
			// Distinct target_refs per kind, so the subtests share the database
			// without contending for the same rows.
			mine, theirs := uuid.NewString(), uuid.NewString()
			refs = append(refs, mine, theirs)

			spec := func(ref string) TaskSpec {
				r := ref
				return TaskSpec{
					Kind:      kind,
					TargetRef: &r,
					Steps:     []StepSpec{{Kind: kind.String(), Args: json.RawMessage(`{}`)}},
				}
			}

			first, err := s.Create(ctx, spec(mine))
			if err != nil {
				t.Fatalf("first %s: %v", kind, err)
			}

			// The regression: before the fix this came back as a wrapped raw
			// "insert task: … (SQLSTATE 23505)", which the API renders as 500 rather
			// than the documented 409. And it must be THIS kind's conflict, not
			// another kind's wording.
			if _, err := s.Create(ctx, spec(mine)); !errors.Is(err, kind.inProgress) {
				t.Fatalf("concurrent %s err = %v, want %v", kind, err, kind.inProgress)
			}

			// The lock is per target, not global.
			if _, err := s.Create(ctx, spec(theirs)); err != nil {
				t.Fatalf("%s on a different target: %v", kind, err)
			}

			// And it self-releases: a terminal task drops out of the index's
			// predicate. Terminating goes through the worker's fenced path — claim,
			// then complete as its owner. The lease a real claim takes is not needed
			// here: this is about the index, and Claim is what stamps the owner the
			// completion is fenced on.
			const owner = "TestPgTaskLocks"
			if claimed, err := s.Claim(ctx, first.ID, owner); err != nil || claimed == nil {
				t.Fatalf("claim the in-flight %s task %s: claimed=%v err=%v", kind, first.ID, claimed, err)
			}
			if err := s.CompleteTask(ctx, first.ID, owner); err != nil {
				t.Fatalf("complete first %s: %v", kind, err)
			}
			if _, err := s.Create(ctx, spec(mine)); err != nil {
				t.Fatalf("%s after the holder reached terminal = %v, want admitted", kind, err)
			}
		})
	}
	if locked == 0 {
		t.Fatal("no kind declares a lockIndex, so this test asserted nothing")
	}
}

// TestPgUniqueConstraintsOnTask holds the Go kind table and the live schema in
// agreement about which kinds are first-writer-wins, in BOTH directions. It is
// what keeps taskLockViolation's structural classification exact: a unique
// violation from Create's task INSERT can only be the index for the kind being
// inserted, because the indexes' predicates each name one kind, and the only
// other unique constraint on `tasks` is its primary key — whose conflict Create
// already absorbs with ON CONFLICT (id) DO NOTHING.
//
// Both directions matter, and neither failure is loud on its own:
//
//   - a Kind naming a lockIndex the schema does not have means the store watches
//     for a violation that can never be raised, so concurrent tasks are admitted
//     and nothing 409s;
//   - an index no Kind names means its 23505 goes unclassified and surfaces as a
//     500 where the API documents 409 — which is #400.
//
// `want` is DERIVED from allKinds rather than written down, so adding a lock needs
// no edit here: declare the lockIndex and ship the migration, and this passes.
func TestPgUniqueConstraintsOnTask(t *testing.T) {
	pool, ctx := migratedPool(t)
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT c.relname
		   FROM pg_index i
		   JOIN pg_class c ON c.oid = i.indexrelid
		   JOIN pg_class t ON t.oid = i.indrelid
		   JOIN pg_namespace n ON n.oid = t.relnamespace
		  WHERE t.relname = 'tasks' AND n.nspname = $1 AND i.indisunique
		  ORDER BY c.relname`, taskSchema)
	if err != nil {
		t.Fatalf("list unique indexes on tasks: %v", err)
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

	want := []string{"tasks_pkey"}
	for _, k := range allKinds {
		if k.lockIndex != "" {
			want = append(want, k.lockIndex)
		}
	}
	sort.Strings(want) // the query is ORDER BY relname
	if !slices.Equal(got, want) {
		t.Fatalf("unique indexes on tasks = %v, want exactly %v (derived from allKinds) — see this test's doc comment", got, want)
	}
}

// scratchURL is the throwaway database this package's Postgres-backed tests share,
// provisioned once by TestMain. Empty when AMP_TEST_DATABASE_URL is unset, which
// is what makes those tests skip.
var scratchURL string

// taskSchema is the --db-schema default: the schema holding the controller's own
// tables, which is what these tests exercise. The resource model lives in the
// fixed db.ResourceSchema.
const taskSchema = "controller"

// TestMain provisions one throwaway DATABASE for the whole package and drops it
// afterwards.
//
// A database rather than a schema because the migrations create two fixed schemas
// — the resource model under --db-schema plus the always-`controller` task log —
// so relocating one isolates nothing. Once per package rather than once per test
// because YugabyteDB does not free a dropped keyspace's name synchronously, so a
// drop-and-recreate between two tests in the same binary races itself.
func TestMain(m *testing.M) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	u, drop, err := db.ScratchMigrated(ctx, url, "amp_test_task_store", taskSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provision scratch database: %v\n", err)
		os.Exit(1)
	}
	scratchURL = u
	code := m.Run()
	drop()
	os.Exit(code)
}

// migratedPool returns a pool over the package's migrated scratch database, or
// skips the test when there is no database to talk to.
func migratedPool(t *testing.T) (*db.Pool, context.Context) {
	t.Helper()
	if scratchURL == "" {
		t.Skip("AMP_TEST_DATABASE_URL not set; these tests need a YugabyteDB")
	}
	ctx := t.Context()
	pool, err := db.Connect(ctx, scratchURL, taskSchema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return pool, ctx
}
