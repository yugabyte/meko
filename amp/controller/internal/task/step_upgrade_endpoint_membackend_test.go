package task_test

// Against a real backend, because the by-id read has to reach the endpoint's BRANCH
// for the pinned pg major — a backend that left it zero would deploy a pg 15
// endpoint on the chart's default major with the fake-backed tests green.

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/task"
)

func TestUpgradeEndpointStepAgainstMemBackend(t *testing.T) {
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
	be.SeedBranch(pool.ID, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 15, IsDefault: true})

	epID := uuid.New()
	dep := &memReplayDeployer{}
	deps := task.Deps{State: be, Deployer: dep, ControllerURL: "http://sc:50051"}
	create := task.CreateEndpointArgs{
		EndpointID: epID, ClusterID: clusterID, BranchID: branchID, Role: core.RoleReadWrite,
		PgVersion: 15, CPU: "8", Memory: "16Gi", Version: "ep-v1",
	}
	if res := task.NewCreateEndpointStep(create, deps).Execute(t.Context()); res.Failed() {
		t.Fatalf("create Execute: %v", res.Err)
	}

	upgrade := task.NewUpgradeEndpointStep(task.UpgradeEndpointArgs{EndpointID: epID, Version: "ep-v2"}, deps)
	if res := upgrade.Execute(t.Context()); res.Failed() {
		t.Fatalf("upgrade Execute: %v", res.Err)
	}

	spec := dep.specs[len(dep.specs)-1]
	if spec.Version != "ep-v2" || spec.PgVersion != 15 || spec.CPU != "8" || spec.Memory != "16Gi" {
		t.Errorf("redeploy spec = %q pg%d %s/%s, want ep-v2 pg15 8/16Gi", spec.Version, spec.PgVersion, spec.CPU, spec.Memory)
	}
	// This deployer reports no version, so the running column falls back to the target.
	ep, err := be.GetEndpoint(t.Context(), pool.ID, clusterID, branchID, epID)
	if err != nil {
		t.Fatalf("GetEndpoint: %v", err)
	}
	if ep.Version != "ep-v2" || ep.TargetVersion != "ep-v2" {
		t.Errorf("recorded version/target = %q/%q, want ep-v2/ep-v2", ep.Version, ep.TargetVersion)
	}

	// Replay converges: same version, same spec, no second endpoint.
	if res := upgrade.Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v (the second run must converge)", res.Err)
	}
	if again := dep.specs[len(dep.specs)-1]; !reflect.DeepEqual(again, spec) {
		t.Errorf("replay redeploy spec = %+v, want the same spec re-applied", again)
	}
}
