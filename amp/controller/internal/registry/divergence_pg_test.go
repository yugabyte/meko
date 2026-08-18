package registry

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
)

// TestPgProductDeleteTombstonesTheBranch pins the contract the tombstone rests
// on: `DELETE /api/v1/…/branches/{id}` keeps the row and stamps `dropped_at`,
// and every "is this branch live?" reader — including the metrics gauge — stops
// counting it from then on.
func TestPgProductDeleteTombstonesTheBranch(t *testing.T) {
	ctx := t.Context()
	pool := migratedPool(t)
	store := NewStore(pool)
	backend := core.NewPgBackend(pool)

	clusterID := seedCluster(t, pool)
	branchUUID := uuid.New()

	if err := store.RecordBranch(ctx, clusterID, branchUUID, BranchCreateRecord{
		BranchName: "divergence", PgVersion: 18, StartLSN: "0/0",
	}); err != nil {
		t.Fatalf("record branch: %v", err)
	}
	before, err := store.RegistryCounts(ctx)
	if err != nil {
		t.Fatalf("count branches: %v", err)
	}

	if err := backend.DeleteBranch(ctx, clusterID, branchUUID); err != nil {
		t.Fatalf("product-path delete: %v", err)
	}

	after, err := store.RegistryCounts(ctx)
	if err != nil {
		t.Fatalf("recount branches: %v", err)
	}
	if after != before-1 {
		t.Errorf("branch gauge = %d after deleting one of %d, want %d", after, before, before-1)
	}

	// The row is kept, not deleted: a branch id is never reused, and its pages and
	// WAL in amp_storage outlive the drop.
	var dropped bool
	if err := pool.QueryRow(ctx,
		`SELECT dropped_at IS NOT NULL FROM amp_registry.branches
		  WHERE cluster_id = $1::uuid AND branch_id = $2::uuid`,
		clusterID.String(), branchUUID.String()).Scan(&dropped); err != nil {
		t.Fatalf("read back the tombstone: %v", err)
	}
	if !dropped {
		t.Fatal("product-path delete removed the row instead of tombstoning it")
	}
}

// TestPgTombstoneGuards covers what the database will not enforce for a
// tombstone. An UPDATE that never writes `branch_id` leaves every RESTRICT
// foreign key unexamined, so the guarantees a DELETE would get for free have to
// be asserted in SQL — and a tombstoned row still satisfies an FK, so "the
// parent exists" does not mean "the parent is alive".
func TestPgTombstoneGuards(t *testing.T) {
	ctx := t.Context()
	pool := migratedPool(t)
	store := NewStore(pool)
	backend := core.NewPgBackend(pool)
	clusterID := seedCluster(t, pool)

	// A live root branch with a live endpoint on it.
	rootID := uuid.New()
	if err := store.RecordBranch(ctx, clusterID, rootID, BranchCreateRecord{
		BranchName: "guards-root", PgVersion: 18, StartLSN: "0/0",
	}); err != nil {
		t.Fatalf("record root: %v", err)
	}
	endpointID := uuid.New()
	if err := backend.EnsureEndpointRecord(ctx, endpointID, clusterID, rootID, "read_write", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("create product endpoint: %v", err)
	}

	// A branch with a live endpoint cannot be tombstoned. The endpoints FK does
	// not see the UPDATE, so the guard is asserted in SQL.
	if err := backend.DeleteBranch(ctx, clusterID, rootID); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("tombstone a branch with a live endpoint = %v, want ErrConflict", err)
	}
	// Same for the cluster-wide cascade.
	if err := backend.DeleteCluster(ctx, clusterID); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("tombstone a cluster with a live endpoint = %v, want ErrConflict", err)
	}

	// With the endpoint retired the drop goes through.
	if err := backend.DeleteEndpoint(ctx, endpointID); err != nil {
		t.Fatalf("retire endpoint: %v", err)
	}
	if err := backend.DeleteBranch(ctx, clusterID, rootID); err != nil {
		t.Fatalf("tombstone after retiring the endpoint: %v", err)
	}

	// A fork may not be created under a tombstoned parent. The parent's row
	// survives the drop, so the FK is satisfied — only an explicit liveness filter
	// stops the async create_branch step from resurrecting the lineage.
	if err := backend.EnsureBranch(ctx, rootID, clusterID, uuid.New(),
		"guards-child", "0/16B8", 18); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("fork under a tombstoned parent = %v, want ErrNotFound", err)
	}
}
