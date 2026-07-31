package core

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
	"github.com/yugabyte/amp/controller/internal/registry"
)

// TestPgBranchExtensionsInheritance covers the externalized-extension branch
// state end-to-end against a real Postgres/YSQL database: the opt-in union
// (AddBranchExtensions), the durable read (BranchExtensions), and — the point of
// the review ask — that a FORK inherits its parent's opted-in set. The set lives
// on amp_registry.branches (keyed by the 32-hex storage branch id), and fork
// inheritance is a snapshot RecordBranch copies from the ancestor at create time.
//
// This is the "verify branching" integration coverage for extensions that
// yb_amp_local can't do end-to-end (the local Docker path has no ImageVolumes,
// so it can't actually CREATE EXTENSION); actual CREATE EXTENSION + fork on a
// mounted volume is covered by the GKE smoke test (deploy/smoke-test.sh,
// SMOKE_EXTENSIONS=1).
//
// Exercises the real PgBackend + registry.Store SQL. It provisions an isolated
// schema per run and drops it afterwards. Set AMP_TEST_DATABASE_URL to run;
// skipped otherwise so `go test ./...` stays green without a database.
func TestPgBranchExtensionsInheritance(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	schema := fmt.Sprintf("amptest_%s", uuid.New().String()[:8])
	if err := db.Migrate(ctx, url, schema); err != nil {
		t.Fatalf("migrate schema %s: %v", schema, err)
	}
	pool, err := db.Connect(ctx, url, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), fmt.Sprintf("DROP SCHEMA %s CASCADE", schema)); err != nil {
			t.Logf("cleanup: drop schema %s: %v", schema, err)
		}
		pool.Close()
	})

	store := registry.NewStore(pool)
	b := NewPgBackend(pool)

	// A branch's storage-registry row is created by RecordBranch — the shared
	// choke point both the controller and yb_amp_local reach. projectID is
	// irrelevant to the extension set (it lives on the flat storage record), so
	// we pass a fixed one throughout.
	projectID := uuid.New()
	rootID := uuid.New()
	rootHex := storageBranchID(rootID)

	recordBranch := func(id uuid.UUID, name string, ancestorHex *string, startLSN string) {
		t.Helper()
		if err := store.RecordBranch(ctx, storageBranchID(id), registry.BranchCreateRecord{
			BranchName:       name,
			PgVersion:        18,
			AncestorBranchID: ancestorHex,
			StartLSN:         startLSN,
		}); err != nil {
			t.Fatalf("RecordBranch(%s): %v", name, err)
		}
	}
	mustExts := func(id uuid.UUID) []string {
		t.Helper()
		got, err := b.BranchExtensions(ctx, projectID, id)
		if err != nil {
			t.Fatalf("BranchExtensions: %v", err)
		}
		return got
	}

	// --- root branch: no opt-in yet -> empty set. ---
	recordBranch(rootID, "main", nil, "0/0")
	if got := mustExts(rootID); len(got) != 0 {
		t.Fatalf("fresh branch extensions = %v, want none", got)
	}

	// --- opt in (idempotent absolute union). ---
	if err := b.AddBranchExtensions(ctx, projectID, rootID, []string{"pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions: %v", err)
	}
	if got := mustExts(rootID); !reflect.DeepEqual(got, []string{"pgvector"}) {
		t.Fatalf("root extensions = %v, want [pgvector]", got)
	}

	// --- fork BEFORE any further opt-in: the child inherits the parent's set as
	// a snapshot RecordBranch copies from the ancestor. ---
	childID := uuid.New()
	recordBranch(childID, "b1", &rootHex, "0/16B8")
	if got := mustExts(childID); !reflect.DeepEqual(got, []string{"pgvector"}) {
		t.Fatalf("forked child extensions = %v, want inherited [pgvector]", got)
	}

	// --- child opts into more; the union is sorted+deduped and the parent is
	// unaffected (the snapshot is a copy, not a live pointer). ---
	if err := b.AddBranchExtensions(ctx, projectID, childID, []string{"rum", "pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions(child): %v", err)
	}
	if got := mustExts(childID); !reflect.DeepEqual(got, []string{"pgvector", "rum"}) {
		t.Fatalf("child extensions = %v, want sorted union [pgvector rum]", got)
	}
	if got := mustExts(rootID); !reflect.DeepEqual(got, []string{"pgvector"}) {
		t.Fatalf("parent extensions = %v after child add, want unchanged [pgvector]", got)
	}

	// --- re-adding is idempotent (absolute union, no double-apply). ---
	if err := b.AddBranchExtensions(ctx, projectID, childID, []string{"pgvector", "rum"}); err != nil {
		t.Fatalf("AddBranchExtensions(idempotent): %v", err)
	}
	if got := mustExts(childID); !reflect.DeepEqual(got, []string{"pgvector", "rum"}) {
		t.Fatalf("child extensions after idempotent re-add = %v, want [pgvector rum]", got)
	}

	// --- a grandchild forked AFTER the child's extra opt-in inherits the fuller
	// set; a second fork off the (still single-extension) root does not. ---
	grandID := uuid.New()
	childHex := storageBranchID(childID)
	recordBranch(grandID, "b2", &childHex, "0/1C90")
	if got := mustExts(grandID); !reflect.DeepEqual(got, []string{"pgvector", "rum"}) {
		t.Fatalf("grandchild extensions = %v, want inherited [pgvector rum]", got)
	}
	sibID := uuid.New()
	recordBranch(sibID, "b3", &rootHex, "0/16B8")
	if got := mustExts(sibID); !reflect.DeepEqual(got, []string{"pgvector"}) {
		t.Fatalf("second root fork extensions = %v, want inherited [pgvector]", got)
	}

	// --- an absent branch reads empty and is a no-op to add (mirrors the
	// controller's degrade-to-defaults contract). ---
	absent := uuid.New()
	if got := mustExts(absent); len(got) != 0 {
		t.Fatalf("absent branch extensions = %v, want none", got)
	}
	if err := b.AddBranchExtensions(ctx, projectID, absent, []string{"pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions(absent): %v", err)
	}
	if got := mustExts(absent); len(got) != 0 {
		t.Fatalf("absent branch still absent = %v, want none", got)
	}
}
