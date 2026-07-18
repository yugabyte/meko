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
	projectID := uuid.New()
	branchID := uuid.New()
	be.SeedBranch(pool.ID, core.Branch{ProjectID: projectID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	epID := uuid.New()
	dep := &memReplayDeployer{}
	// *core.MemBackend structurally satisfies task.StateStore.
	deps := task.Deps{State: be, Deployer: dep, ControllerURL: "http://sc:50051"}
	args := task.CreateEndpointArgs{EndpointID: epID, ProjectID: projectID, BranchID: branchID, Role: core.RoleReadWrite, PgVersion: 18}

	if res := task.NewCreateEndpointStep(args, deps).Execute(t.Context()); res.Failed() {
		t.Fatalf("first Execute: %v", res.Err)
	}
	// Replay the *same* step (same id) against the same backend — it must converge,
	// not conflict on the single-writer slot (mirrors PgBackend ON CONFLICT (id)).
	if res := task.NewCreateEndpointStep(args, deps).Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v (the second run must converge, not conflict)", res.Err)
	}

	// Exactly one endpoint exists on the branch after two runs.
	eps, _, err := be.ListEndpoints(t.Context(), pool.ID, projectID, branchID, 100, 0)
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

// memReplayDeployer is a runtime-idempotent deployer: the same name yields the
// same derived address, so a replayed step records the same outcome.
type memReplayDeployer struct {
	created []string
	deleted []string
}

func (d *memReplayDeployer) CreateEndpoint(_ context.Context, spec deploy.EndpointSpec) (deploy.Endpoint, error) {
	d.created = append(d.created, spec.Name)
	return deploy.Endpoint{ID: spec.Name, Address: spec.Name + ".endpoints.svc:55433"}, nil
}

func (d *memReplayDeployer) DeleteEndpoint(_ context.Context, id string) error {
	d.deleted = append(d.deleted, id)
	return nil
}

func (d *memReplayDeployer) EndpointSafekeeperURL(id string) string { return "http://" + id + ":7676" }
