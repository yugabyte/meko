package core

import (
	"testing"

	"github.com/google/uuid"
)

// The MemBackend's applied-generation setters must behave like PgBackend's, since
// the API tests read the echo through them: nil until something applies a YCP
// update, monotonic afterwards, and a no-op for a row that is not there.
func TestMemYCPGeneration(t *testing.T) {
	m := NewMemBackend()
	ctx := t.Context()

	// A pool has to exist for the branch/endpoint reads, which are scoped through
	// it (GetBranch resolves pool -> cluster -> branch).
	owner := uuid.New()
	if _, err := m.EnsureUser(ctx, owner, false); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	poolID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	if _, err := m.CreateStoragePoolWithID(ctx, poolID, owner, "pool-a"); err != nil {
		t.Fatalf("CreateStoragePoolWithID: %v", err)
	}
	m.SeedBranch(poolID, Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	br, err := m.GetBranch(ctx, poolID, clusterID, branchID)
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if br.YCPGeneration != nil {
		t.Errorf("fresh branch ycp_generation = %d, want nil", *br.YCPGeneration)
	}

	if err := m.SetBranchYCPGeneration(ctx, branchID, 4); err != nil {
		t.Fatalf("SetBranchYCPGeneration: %v", err)
	}
	// An older task completing late must not regress it.
	if err := m.SetBranchYCPGeneration(ctx, branchID, 2); err != nil {
		t.Fatalf("SetBranchYCPGeneration(older): %v", err)
	}
	br, _ = m.GetBranch(ctx, poolID, clusterID, branchID)
	if br.YCPGeneration == nil || *br.YCPGeneration != 4 {
		t.Errorf("branch ycp_generation = %v, want it held at 4", br.YCPGeneration)
	}

	endpointID := uuid.New()
	if err := m.EnsureEndpointRecord(ctx, endpointID, clusterID, branchID, "read_write", "rw", nil, nil, nil, nil); err != nil {
		t.Fatalf("EnsureEndpointRecord: %v", err)
	}
	if err := m.SetEndpointYCPGeneration(ctx, endpointID, 7); err != nil {
		t.Fatalf("SetEndpointYCPGeneration: %v", err)
	}
	got, err := m.GetEndpoint(ctx, poolID, clusterID, branchID, endpointID)
	if err != nil {
		t.Fatalf("GetEndpoint: %v", err)
	}
	if got.YCPGeneration == nil || *got.YCPGeneration != 7 {
		t.Errorf("endpoint ycp_generation = %v, want 7", got.YCPGeneration)
	}

	// Absent rows are no-ops, like every other id-keyed writer here.
	if err := m.SetBranchYCPGeneration(ctx, uuid.New(), 1); err != nil {
		t.Errorf("SetBranchYCPGeneration on an absent branch = %v, want a no-op", err)
	}
	if err := m.SetEndpointYCPGeneration(ctx, uuid.New(), 1); err != nil {
		t.Errorf("SetEndpointYCPGeneration on an absent endpoint = %v, want a no-op", err)
	}
}
