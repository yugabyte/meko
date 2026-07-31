package registry

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestPgRecordBranchForkValidation is the fork ↔ GC-cutoff validation in
// RecordBranch against a real database — the touch on the ancestor's row,
// its error classification, and the roll-back of the whole registration.
// The Rust stress test (page_store.rs, fork_txn) covers the SQL protocol
// racing a live GC publish; this covers the Go path itself: sentinel
// errors, txn atomicity, and the newly-inserted gate that keeps
// re-records idempotent.
//
// Needs YugabyteDB (the amp_storage migration uses YB-specific HASH DDL),
// so it skips unless AMP_TEST_DATABASE_URL points at one.
func TestPgRecordBranchForkValidation(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AMP_TEST_DATABASE_URL not set; this test needs a YugabyteDB")
	}
	ctx := t.Context()
	if err := db.Migrate(ctx, url, "registry_store_test"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := db.Connect(ctx, url, "registry_store_test")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	s := NewStore(pool)

	newID := func() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }
	parent := newID()
	var children []string
	child := func() string {
		id := newID()
		children = append(children, id)
		return id
	}
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(ctx)
		ids := append([]string{parent}, children...)
		for _, q := range []string{
			`DELETE FROM amp_registry.branches WHERE branch_id = ANY($1)`,
			`DELETE FROM amp_storage.branch_state WHERE branch_id = ANY($1)`,
		} {
			if _, err := pool.Exec(cleanupCtx, q, ids); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})

	// A root branch, with a GC cutoff as the pageserver would have
	// published it: 0/64 = 100.
	if err := s.RecordBranch(ctx, parent, BranchCreateRecord{
		BranchName: "fork-validation-parent", PgVersion: 18, StartLSN: "0/0",
	}); err != nil {
		t.Fatalf("record parent: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE amp_storage.branch_state SET applied_gc_cutoff_lsn = 100 WHERE branch_id = $1`,
		parent); err != nil {
		t.Fatalf("seed cutoff: %v", err)
	}

	fork := func(id, lsn string) error {
		return s.RecordBranch(ctx, id, BranchCreateRecord{
			BranchName: "fork-validation-child", PgVersion: 18,
			AncestorBranchID: &parent, StartLSN: lsn,
		})
	}
	rowCount := func(table, id string) int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE branch_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	// Below the cutoff (0/50 = 80 < 100): rejected as invalid (permanent
	// — the cutoff is monotone, a retry cannot succeed), and the whole
	// registration rolls back — neither row lands.
	below := child()
	if err := fork(below, "0/50"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("fork below cutoff: want ErrInvalid, got %v", err)
	}
	if n := rowCount("amp_storage.branch_state", below); n != 0 {
		t.Fatalf("rejected fork left a branch_state row")
	}
	if n := rowCount("amp_registry.branches", below); n != 0 {
		t.Fatalf("rejected fork left a branches row")
	}

	// At the cutoff (0/64 = 100): registered.
	at := child()
	if err := fork(at, "0/64"); err != nil {
		t.Fatalf("fork at cutoff: %v", err)
	}
	if n := rowCount("amp_storage.branch_state", at); n != 1 {
		t.Fatalf("accepted fork missing its branch_state row")
	}

	// Re-record after the cutoff advanced past the fork point: still a
	// no-op success — the validation is gated on the child row being
	// newly inserted.
	if _, err := pool.Exec(ctx,
		`UPDATE amp_storage.branch_state SET applied_gc_cutoff_lsn = 200 WHERE branch_id = $1`,
		parent); err != nil {
		t.Fatalf("advance cutoff: %v", err)
	}
	if err := fork(at, "0/64"); err != nil {
		t.Fatalf("idempotent re-record must not re-validate: %v", err)
	}

	// A missing ancestor row: not found.
	missing := newID()
	orphan := child()
	if err := s.RecordBranch(ctx, orphan, BranchCreateRecord{
		BranchName: "fork-validation-orphan", PgVersion: 18,
		AncestorBranchID: &missing, StartLSN: "1/0",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fork from missing ancestor: want ErrNotFound, got %v", err)
	}
}
