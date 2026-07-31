package core

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// TestMemBranchExtensions covers the MemBackend extension set: the opt-in union
// is idempotent (a repeat is a no-op), a fork inherits a snapshot of its
// parent's set, and an absent branch degrades to empty rather than erroring.
func TestMemBranchExtensions(t *testing.T) {
	m := NewMemBackend()
	ctx := t.Context()

	poolID, projectID, root := uuid.New(), uuid.New(), uuid.New()
	m.SeedBranch(poolID, Branch{ProjectID: projectID, BranchID: root, Name: "main", PgVersion: 18, IsDefault: true})

	// A fresh branch has no opted-in extensions.
	if got, _ := m.BranchExtensions(ctx, projectID, root); len(got) != 0 {
		t.Fatalf("initial extensions = %v, want none", got)
	}

	// First opt-in.
	if err := m.AddBranchExtensions(ctx, projectID, root, []string{"pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions: %v", err)
	}
	// Union with an overlap: existing-first order, deduplicated.
	if err := m.AddBranchExtensions(ctx, projectID, root, []string{"pgvector", "rum"}); err != nil {
		t.Fatalf("AddBranchExtensions(union): %v", err)
	}
	got, _ := m.BranchExtensions(ctx, projectID, root)
	if want := []string{"pgvector", "rum"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("extensions = %v, want %v (union, deduped)", got, want)
	}

	// Idempotent: re-adding the same set changes nothing.
	if err := m.AddBranchExtensions(ctx, projectID, root, []string{"rum", "pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions(idempotent): %v", err)
	}
	if got, _ = m.BranchExtensions(ctx, projectID, root); len(got) != 2 {
		t.Fatalf("extensions after idempotent re-add = %v, want 2", got)
	}

	// A fork inherits a snapshot of the parent's set.
	child := uuid.New()
	if err := m.EnsureBranch(ctx, root, projectID, child, "child", "0/10", 18); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if got, _ = m.BranchExtensions(ctx, projectID, child); !reflect.DeepEqual(got, []string{"pgvector", "rum"}) {
		t.Fatalf("forked extensions = %v, want the inherited [pgvector rum]", got)
	}
	// The snapshot is independent: adding to the child does not touch the parent.
	if err := m.AddBranchExtensions(ctx, projectID, child, []string{"postgis"}); err != nil {
		t.Fatalf("AddBranchExtensions(child): %v", err)
	}
	if got, _ = m.BranchExtensions(ctx, projectID, root); len(got) != 2 {
		t.Fatalf("parent extensions after child add = %v, want unchanged 2", got)
	}

	// An absent branch is a no-op, not an error.
	if err := m.AddBranchExtensions(ctx, projectID, uuid.New(), []string{"pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions(absent): %v", err)
	}
}
