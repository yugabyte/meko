package core

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// TestMemBranchExtensions covers the MemBackend extension set: it is per-branch
// (seeded on the default branch, inherited by a fork), the per-branch opt-in
// unions idempotently and sorted, adding to one branch leaves its siblings
// untouched, and an absent branch degrades to empty rather than erroring.
func TestMemBranchExtensions(t *testing.T) {
	m := NewMemBackend()
	ctx := t.Context()

	poolID, clusterID, root := uuid.New(), uuid.New(), uuid.New()
	m.SeedBranch(poolID, Branch{ClusterID: clusterID, BranchID: root, Name: "main", PgVersion: 18, IsDefault: true})

	// A fresh branch has no opted-in extensions.
	if got, _ := m.BranchExtensions(ctx, clusterID, root); len(got) != 0 {
		t.Fatalf("initial extensions = %v, want none", got)
	}

	// First opt-in on the branch.
	if err := m.AddBranchExtensions(ctx, clusterID, root, []string{"pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions: %v", err)
	}
	// Union with an overlap: sorted, deduplicated.
	if err := m.AddBranchExtensions(ctx, clusterID, root, []string{"pgvector", "rum"}); err != nil {
		t.Fatalf("AddBranchExtensions(union): %v", err)
	}
	got, _ := m.BranchExtensions(ctx, clusterID, root)
	if want := []string{"pgvector", "rum"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("extensions = %v, want %v (union, deduped)", got, want)
	}

	// Idempotent: re-adding the same set changes nothing.
	if err := m.AddBranchExtensions(ctx, clusterID, root, []string{"rum", "pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions(idempotent): %v", err)
	}
	if got, _ = m.BranchExtensions(ctx, clusterID, root); len(got) != 2 {
		t.Fatalf("extensions after idempotent re-add = %v, want 2", got)
	}

	// A fork inherits its parent's set at creation.
	child := uuid.New()
	if err := m.EnsureBranch(ctx, root, clusterID, child, "child", "0/10", 18); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if got, _ = m.BranchExtensions(ctx, clusterID, child); !reflect.DeepEqual(got, []string{"pgvector", "rum"}) {
		t.Fatalf("fork extensions = %v, want inherited [pgvector rum]", got)
	}

	// Adding to the fork is individual: the parent is untouched.
	if err := m.AddBranchExtensions(ctx, clusterID, child, []string{"age"}); err != nil {
		t.Fatalf("AddBranchExtensions(fork): %v", err)
	}
	if got, _ = m.BranchExtensions(ctx, clusterID, child); !reflect.DeepEqual(got, []string{"age", "pgvector", "rum"}) {
		t.Fatalf("fork extensions after add = %v, want [age pgvector rum]", got)
	}
	if got, _ = m.BranchExtensions(ctx, clusterID, root); !reflect.DeepEqual(got, []string{"pgvector", "rum"}) {
		t.Fatalf("parent extensions after fork add = %v, want unchanged [pgvector rum]", got)
	}

	// An absent branch is a no-op, not an error.
	if err := m.AddBranchExtensions(ctx, clusterID, uuid.New(), []string{"pgvector"}); err != nil {
		t.Fatalf("AddBranchExtensions(absent): %v", err)
	}
}
