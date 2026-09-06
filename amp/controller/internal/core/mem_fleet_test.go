package core

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// TestMemAllEndpointDeploys covers the fleet enumeration a roll runs: every live
// endpoint across every cluster, in one read, carrying the same renderable state
// the scoped forms do — and nothing hanging off a branch that is gone.
//
// Liveness is the point. PgBackend tombstones a branch (`dropped_at`) and hard-
// deletes an endpoint, so a `WHERE eb.dropped_at IS NULL` join is what takes a
// dropped branch's endpoints out of the fleet; MemBackend deletes the branch row
// outright, which is the same case. A roll that enumerated them would try to
// redeploy compute for a branch nothing can serve.
func TestMemAllEndpointDeploys(t *testing.T) {
	m := NewMemBackend()
	ctx := t.Context()

	poolID := uuid.New()
	clusterA, branchA := uuid.New(), uuid.New()
	clusterB, branchB := uuid.New(), uuid.New()
	m.SeedBranch(poolID, Branch{ClusterID: clusterA, BranchID: branchA, Name: "main", PgVersion: 18, IsDefault: true})
	m.SeedBranch(poolID, Branch{ClusterID: clusterB, BranchID: branchB, Name: "other", PgVersion: 15, IsDefault: true})

	epA, err := m.CreateEndpoint(ctx, clusterA, branchA, RoleReadWrite, nil)
	if err != nil {
		t.Fatalf("CreateEndpoint(A): %v", err)
	}
	epB, err := m.CreateEndpoint(ctx, clusterB, branchB, RoleReadWrite, nil)
	if err != nil {
		t.Fatalf("CreateEndpoint(B): %v", err)
	}
	if err := m.SetEndpointVersion(ctx, epA.ID, "ep-v1"); err != nil {
		t.Fatalf("SetEndpointVersion: %v", err)
	}
	// Both were deployed: a recorded address is what says so, and the fleet is the
	// endpoints the controller actually deployed (see AllEndpointDeploys).
	for _, id := range []uuid.UUID{epA.ID, epB.ID} {
		if err := m.SetEndpointAddress(ctx, id, "yb-ep.endpoints.svc:55433", ""); err != nil {
			t.Fatalf("SetEndpointAddress: %v", err)
		}
	}

	fleet, err := m.AllEndpointDeploys(ctx)
	if err != nil {
		t.Fatalf("AllEndpointDeploys: %v", err)
	}
	if len(fleet) != 2 {
		t.Fatalf("fleet = %+v, want both endpoints across both clusters", fleet)
	}
	byID := map[uuid.UUID]EndpointDeploy{}
	for _, d := range fleet {
		byID[d.ID] = d
	}
	// The same state the scoped enumerations render from, or a roll would deploy an
	// endpoint with defaults where its row has a value.
	if got := byID[epA.ID]; got.BranchID != branchA || got.PgVersion != 18 || got.Version != "ep-v1" {
		t.Errorf("fleet entry for A = %+v, want branch A, pg18, ep-v1", got)
	}
	if got := byID[epB.ID]; got.PgVersion != 15 || got.Version != "" {
		t.Errorf("fleet entry for B = %+v, want pg15 and an unknown version", got)
	}
	// The by-id read is the same projection, so the two cannot disagree.
	one, err := m.EndpointDeploy(ctx, epA.ID)
	if err != nil {
		t.Fatalf("EndpointDeploy: %v", err)
	}
	if !reflect.DeepEqual(one, byID[epA.ID]) {
		t.Errorf("EndpointDeploy = %+v, want what the fleet enumeration reports (%+v)", one, byID[epA.ID])
	}

	// A dropped branch takes its endpoints out of the fleet.
	if err := m.DeleteEndpoint(ctx, epB.ID); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if err := m.DeleteBranch(ctx, clusterB, branchB); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	fleet, err = m.AllEndpointDeploys(ctx)
	if err != nil {
		t.Fatalf("AllEndpointDeploys after the drop: %v", err)
	}
	if len(fleet) != 1 || fleet[0].ID != epA.ID {
		t.Errorf("fleet after dropping a branch = %+v, want only the endpoint on the live branch", fleet)
	}
}

// TestMemFleetExcludesANeverDeployedEndpoint: create_endpoint records the row before
// deploying, so a create that failed leaves a row whose pod never came up — the
// orphans the GC sweeper reclaims, by this same "no address" condition.
//
// It must not be in the fleet. Rolling one waits out helm's --wait and fails, and
// fail-fast then halts the whole roll: a single leaked orphan would make the upgrade
// API unusable until GC caught up, with a multi-minute timeout as the only symptom.
func TestMemFleetExcludesANeverDeployedEndpoint(t *testing.T) {
	m := NewMemBackend()
	ctx := t.Context()

	poolID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	m.SeedBranch(poolID, Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	deployed, err := m.CreateEndpoint(ctx, clusterID, branchID, RoleReadWrite, nil)
	if err != nil {
		t.Fatalf("CreateEndpoint(deployed): %v", err)
	}
	if err := m.SetEndpointAddress(ctx, deployed.ID, "yb-ep.endpoints.svc:55433", ""); err != nil {
		t.Fatalf("SetEndpointAddress: %v", err)
	}
	// The orphan: a row, but no address was ever recorded for it.
	orphan, err := m.CreateEndpoint(ctx, clusterID, branchID, RoleReadOnly, nil)
	if err != nil {
		t.Fatalf("CreateEndpoint(orphan): %v", err)
	}

	fleet, err := m.AllEndpointDeploys(ctx)
	if err != nil {
		t.Fatalf("AllEndpointDeploys: %v", err)
	}
	if len(fleet) != 1 || fleet[0].ID != deployed.ID {
		t.Fatalf("fleet = %+v, want only the deployed endpoint %s", fleet, deployed.ID)
	}
	// Still readable by id, so naming it explicitly still attempts it — the
	// never-skip-an-explicitly-named-endpoint rule (ac-endpoint-upgrade.md §5.1).
	if _, err := m.EndpointDeploy(ctx, orphan.ID); err != nil {
		t.Errorf("EndpointDeploy(orphan) = %v, want it still readable by id", err)
	}
}
