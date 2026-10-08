package task_test

// This test lives in an external _test package so it can import core (the task
// package itself stays free of the domain layer). It validates create_endpoint
// replay idempotency against the *real* core.MemBackend — not just the fake —
// since the fake's id-keyed semantics must match the materializing backend for
// the replay-convergence guarantee to mean anything.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/ingress"
	"github.com/yugabyte/amp/controller/internal/task"
)

func TestCreateEndpointStepReplayConvergesAgainstMemBackend(t *testing.T) {
	be := core.NewMemBackend()
	owner, err := be.CreateUser(t.Context(), false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	pool, err := be.CreateStoragePool(t.Context(), owner.ID, "pool")
	if err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	clusterID := uuid.New()
	branchID := uuid.New()
	be.SeedBranch(pool.ID, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	epID := uuid.New()
	dep := &memReplayDeployer{}
	// *core.MemBackend structurally satisfies task.StateStore.
	deps := task.Deps{State: be, Deployer: dep, ControllerURL: "http://sc:50051"}
	args := task.CreateEndpointArgs{EndpointID: epID, ClusterID: clusterID, BranchID: branchID, Role: core.RoleReadWrite, PgVersion: 18}

	if res := task.NewCreateEndpointStep(args, deps).Execute(t.Context()); res.Failed() {
		t.Fatalf("first Execute: %v", res.Err)
	}
	// Replay the *same* step (same id) against the same backend — it must converge,
	// not conflict on the single-writer slot (mirrors PgBackend ON CONFLICT (id)).
	if res := task.NewCreateEndpointStep(args, deps).Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v (the second run must converge, not conflict)", res.Err)
	}

	// Exactly one endpoint exists on the branch after two runs.
	eps, _, err := be.ListEndpoints(t.Context(), pool.ID, clusterID, branchID, 100, 0)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("endpoints on branch after replay = %d, want 1 (idempotent)", len(eps))
	}
	if eps[0].ID != epID {
		t.Errorf("endpoint id = %s, want %s", eps[0].ID, epID)
	}
}

// An nlb_* endpoint's name is minted into its record at create, so the deploy
// and every redeploy read the same one. A replay after the zone changed keeps
// the stored name: that is the name clients may already hold.
func TestCreateEndpointStepMintsTheNLBNameOnce(t *testing.T) {
	be := core.NewMemBackend()
	owner, err := be.CreateUser(t.Context(), false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	pool, err := be.CreateStoragePool(t.Context(), owner.ID, "pool")
	if err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	clusterID, branchID := uuid.New(), uuid.New()
	be.SeedBranch(pool.ID, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	epID := uuid.New()
	dep := &memReplayDeployer{}
	deps := task.Deps{State: be, Deployer: dep, ControllerURL: "http://sc:50051",
		DNSZones: ingress.Zones{Internal: "int.example.com", External: "db.example.com"}}
	args := task.CreateEndpointArgs{EndpointID: epID, ClusterID: clusterID, BranchID: branchID,
		Role: core.RoleReadWrite, PgVersion: 18, Connectivity: deploy.ConnectivityNLBExternal}

	if res := task.NewCreateEndpointStep(args, deps).Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	want := deploy.EndpointName(epID) + ".db.example.com"
	if got := dep.specs[0].DNSName; got != want {
		t.Errorf("deployed DNSName = %q, want %q (the external zone)", got, want)
	}
	rec, err := be.EndpointDeploy(t.Context(), epID)
	if err != nil {
		t.Fatalf("EndpointDeploy: %v", err)
	}
	if rec.DNSName != want {
		t.Errorf("stored DNSName = %q, want %q — a redeploy would withdraw the record", rec.DNSName, want)
	}

	deps.DNSZones.External = "other.example.com"
	if res := task.NewCreateEndpointStep(args, deps).Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v", res.Err)
	}
	if got := dep.specs[1].DNSName; got != want {
		t.Errorf("replayed DNSName = %q, want the stored %q", got, want)
	}
}

// No zone, or a mode with no load balancer of its own: nothing is minted.
func TestCreateEndpointStepMintsNoNameWithoutAZone(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		zones ingress.Zones
	}{
		{deploy.ConnectivityNLBInternal, ingress.Zones{External: "db.example.com"}},
		{deploy.ConnectivityClusterIP, ingress.Zones{Internal: "int.example.com", External: "db.example.com"}},
	} {
		be := core.NewMemBackend()
		owner, _ := be.CreateUser(t.Context(), false)
		pool, _ := be.CreateStoragePool(t.Context(), owner.ID, "pool")
		clusterID, branchID := uuid.New(), uuid.New()
		be.SeedBranch(pool.ID, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})
		dep := &memReplayDeployer{}
		deps := task.Deps{State: be, Deployer: dep, ControllerURL: "http://sc:50051", DNSZones: tc.zones}
		args := task.CreateEndpointArgs{EndpointID: uuid.New(), ClusterID: clusterID, BranchID: branchID,
			Role: core.RoleReadWrite, PgVersion: 18, Connectivity: tc.mode}
		if res := task.NewCreateEndpointStep(args, deps).Execute(t.Context()); res.Failed() {
			t.Fatalf("%s: Execute: %v", tc.mode, res.Err)
		}
		if got := dep.specs[0].DNSName; got != "" {
			t.Errorf("%s with zones %+v: DNSName = %q, want none", tc.mode, tc.zones, got)
		}
	}
}

// memReplayDeployer is a runtime-idempotent deployer: the same name yields the
// same derived address, so a replayed step records the same outcome.
type memReplayDeployer struct {
	created []string
	// specs is what each call was asked to deploy, for the tests that assert a
	// redeploy carried the endpoint's state through.
	specs   []deploy.EndpointSpec
	deleted []string
}

func (d *memReplayDeployer) CreateEndpoint(_ context.Context, spec deploy.EndpointSpec) (deploy.Endpoint, error) {
	d.created = append(d.created, spec.Name)
	d.specs = append(d.specs, spec)
	return deploy.Endpoint{ID: spec.Name, Address: spec.Name + ".endpoints.svc:55433"}, nil
}

func (d *memReplayDeployer) DeleteEndpoint(_ context.Context, id string) error {
	d.deleted = append(d.deleted, id)
	return nil
}

func (d *memReplayDeployer) ResizeEndpoint(_ context.Context, _ deploy.ResizeSpec) (deploy.ResizeResult, error) {
	return deploy.ResizeResult{Outcome: deploy.ResizeCompleted}, nil
}

func (d *memReplayDeployer) EndpointSafekeeperURL(id string) string { return "http://" + id + ":7676" }
func (d *memReplayDeployer) EndpointSupervisorURL(id string) string { return "http://" + id + ":9899" }
