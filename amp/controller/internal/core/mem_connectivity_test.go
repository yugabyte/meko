package core

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// TestMemEndpointDeployProjectionsCarryConnectivity pins the property PgBackend
// gets for free by sharing endpointDeploySelect: MemBackend's four EndpointDeploy
// projections must agree.
//
// Connectivity is the field where disagreeing is expensive rather than cosmetic.
// step_upgrade_endpoint reads through EndpointDeploy and redeploys from it, so a
// projection that renders Connectivity empty makes service-lb.yaml's gate false,
// helm prunes the -lb Service, and an nlb_* endpoint's load balancer is deleted
// while its recorded address still names it. Before this test the four
// projections were hand-built literals and three of them dropped the field.
func TestMemEndpointDeployProjectionsCarryConnectivity(t *testing.T) {
	m := NewMemBackend()
	ctx := t.Context()

	poolID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	m.SeedBranch(poolID, Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	epID := uuid.New()
	// Through EnsureEndpointRecord with a config blob, the way create_endpoint
	// writes it — PgBackend reads connectivity back out of that same jsonb.
	config := []byte(`{"cpu":"8","memory":"16Gi","connectivity":"nlb_internal","allowed_cidrs":["10.0.0.0/16"]}`)
	if err := m.EnsureEndpointRecord(ctx, epID, clusterID, branchID, RoleReadWrite, "", config, nil, nil, nil); err != nil {
		t.Fatalf("EnsureEndpointRecord: %v", err)
	}
	if err := m.SetEndpointVersion(ctx, epID, "ep-v1"); err != nil {
		t.Fatalf("SetEndpointVersion: %v", err)
	}
	// AllEndpointDeploys takes only deployed endpoints.
	if err := m.SetEndpointAddress(ctx, epID, "yb-ep-lb.elb.amazonaws.com:55433", ""); err != nil {
		t.Fatalf("SetEndpointAddress: %v", err)
	}

	want := EndpointDeploy{
		ID: epID, BranchID: branchID, ClusterID: clusterID, PgVersion: 18,
		CPU: "8", Memory: "16Gi", Version: "ep-v1", Role: RoleReadWrite,
		Connectivity: "nlb_internal", AllowedCIDRs: []string{"10.0.0.0/16"},
	}

	byID, err := m.EndpointDeploy(ctx, epID)
	if err != nil {
		t.Fatalf("EndpointDeploy: %v", err)
	}
	branch, err := m.BranchEndpointDeploys(ctx, clusterID, branchID)
	if err != nil {
		t.Fatalf("BranchEndpointDeploys: %v", err)
	}
	fleet, err := m.AllEndpointDeploys(ctx)
	if err != nil {
		t.Fatalf("AllEndpointDeploys: %v", err)
	}
	named, err := m.EndpointDeploysByIDs(ctx, []uuid.UUID{epID})
	if err != nil {
		t.Fatalf("EndpointDeploysByIDs: %v", err)
	}
	for _, tc := range []struct {
		name string
		got  []EndpointDeploy
	}{
		{"EndpointDeploy", []EndpointDeploy{byID}},
		{"BranchEndpointDeploys", branch},
		{"AllEndpointDeploys", fleet},
		{"EndpointDeploysByIDs", named},
	} {
		if len(tc.got) != 1 {
			t.Errorf("%s returned %d endpoints, want 1", tc.name, len(tc.got))
			continue
		}
		if !reflect.DeepEqual(tc.got[0], want) {
			t.Errorf("%s = %+v, want %+v", tc.name, tc.got[0], want)
		}
	}
}

// An endpoint whose config carries no connectivity renders empty, which the
// chart treats as cluster_ip — the behaviour of every endpoint created before
// the field existed.
func TestMemEndpointDeployWithoutConnectivity(t *testing.T) {
	m := NewMemBackend()
	ctx := t.Context()

	poolID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	m.SeedBranch(poolID, Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	epID := uuid.New()
	if err := m.EnsureEndpointRecord(ctx, epID, clusterID, branchID, RoleReadWrite, "",
		[]byte(`{"cpu":"2","memory":"16Gi"}`), nil, nil, nil); err != nil {
		t.Fatalf("EnsureEndpointRecord: %v", err)
	}
	d, err := m.EndpointDeploy(ctx, epID)
	if err != nil {
		t.Fatalf("EndpointDeploy: %v", err)
	}
	if d.Connectivity != "" || d.AllowedCIDRs != nil {
		t.Errorf("connectivity = %q/%v, want empty for a config that has none", d.Connectivity, d.AllowedCIDRs)
	}
}
