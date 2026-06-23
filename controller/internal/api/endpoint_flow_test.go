package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-amp/controller/internal/api"
	"github.com/yugabyte/yb-amp/controller/internal/core"
)

// TestEndpointFlow drives the endpoint workflow through the generated client:
// create (which synchronously deploys), get, list, the single-writer rule,
// read-only endpoints, and retire. The backend is the in-memory fake; the
// branch is seeded directly so the test exercises the endpoint lifecycle
// without going through create-project/create-branch.
func TestEndpointFlow(t *testing.T) {
	client, backend, _ := newTestServer(t)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "ep-pool")

	projectID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ProjectID: projectID,
		BranchID:  branchID,
		Name:      "main",
		PgVersion: 17,
		IsDefault: true,
	})

	rw := api.CreateEndpointRequestRoleReadWrite
	ro := api.CreateEndpointRequestRoleReadOnly

	// Create a read_write endpoint -> 201 with the deployed endpoint (address set).
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusCreated || created.JSON201 == nil {
		t.Fatalf("CreateEndpoint status = %d, body = %s", created.StatusCode(), created.Body)
	}
	ep := *created.JSON201
	if ep.ProjectId != projectID || ep.BranchId != branchID {
		t.Errorf("endpoint binding = (%s,%s), want (%s,%s)", ep.ProjectId, ep.BranchId, projectID, branchID)
	}
	if ep.Role != api.EndpointRoleReadWrite {
		t.Errorf("role = %q, want read_write", ep.Role)
	}
	if ep.Address == "" {
		t.Errorf("address = %q, want a deployed address", ep.Address)
	}

	// Get it back -> 200.
	got, err := client.GetEndpointWithResponse(ctx, pool.Id, projectID, branchID, ep.Id)
	if err != nil {
		t.Fatalf("GetEndpoint: %v", err)
	}
	if got.StatusCode() != http.StatusOK || got.JSON200 == nil || got.JSON200.Id != ep.Id {
		t.Fatalf("GetEndpoint status = %d, body = %s", got.StatusCode(), got.Body)
	}

	// List -> exactly one endpoint.
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, projectID, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.StatusCode() != http.StatusOK || list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("ListEndpoints status = %d, body = %s", list.StatusCode(), list.Body)
	}

	// A second read_write on the same branch violates the single-writer rule -> 409.
	dup, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint(dup writer): %v", err)
	}
	if dup.StatusCode() != http.StatusConflict || dup.JSON409 == nil {
		t.Fatalf("dup writer status = %d, body = %s", dup.StatusCode(), dup.Body)
	}

	// A read_only endpoint is allowed alongside the writer -> 201.
	roResp, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &ro})
	if err != nil {
		t.Fatalf("CreateEndpoint(read_only): %v", err)
	}
	if roResp.StatusCode() != http.StatusCreated {
		t.Fatalf("read_only status = %d, body = %s", roResp.StatusCode(), roResp.Body)
	}

	// Retire the writer -> 204.
	del, err := client.DeleteEndpointWithResponse(ctx, pool.Id, projectID, branchID, ep.Id)
	if err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if del.StatusCode() != http.StatusNoContent {
		t.Fatalf("DeleteEndpoint status = %d, want 204, body = %s", del.StatusCode(), del.Body)
	}

	// It is gone now -> get 404.
	gone, err := client.GetEndpointWithResponse(ctx, pool.Id, projectID, branchID, ep.Id)
	if err != nil {
		t.Fatalf("GetEndpoint(after retire): %v", err)
	}
	if gone.StatusCode() != http.StatusNotFound {
		t.Fatalf("GetEndpoint(after retire) status = %d, want 404", gone.StatusCode())
	}
}

// TestEndpointErrors covers the create-time error paths.
func TestEndpointErrors(t *testing.T) {
	client, backend, _ := newTestServer(t)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "err-pool")
	rw := api.CreateEndpointRequestRoleReadWrite

	// Create against a branch that does not exist -> 404.
	missing, err := client.CreateEndpointWithResponse(ctx, pool.Id, uuid.New(), uuid.New(), api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint(missing branch): %v", err)
	}
	if missing.StatusCode() != http.StatusNotFound || missing.JSON404 == nil {
		t.Fatalf("missing branch status = %d, body = %s", missing.StatusCode(), missing.Body)
	}

	// Get against a missing endpoint (branch seeded) -> 404.
	projectID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: branchID, Name: "main", PgVersion: 17, IsDefault: true})
	getMissing, err := client.GetEndpointWithResponse(ctx, pool.Id, projectID, branchID, uuid.New())
	if err != nil {
		t.Fatalf("GetEndpoint(missing): %v", err)
	}
	if getMissing.StatusCode() != http.StatusNotFound {
		t.Fatalf("GetEndpoint(missing) status = %d, body = %s", getMissing.StatusCode(), getMissing.Body)
	}
}

// TestEndpointDeployDisabled: with the provisioner off, endpoint create/retire
// refuse with 501 rather than committing a row + planning a task no worker can
// run.
func TestEndpointDeployDisabled(t *testing.T) {
	client, backend, _ := newTestServerDeploy(t, false)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "off-pool")
	projectID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: branchID, Name: "main", PgVersion: 17, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusNotImplemented {
		t.Fatalf("CreateEndpoint(deploy off) status = %d, want 501; body = %s", created.StatusCode(), created.Body)
	}

	// No endpoint row was committed.
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, projectID, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.StatusCode() != http.StatusOK || list.JSON200 == nil || len(list.JSON200.Endpoints) != 0 {
		t.Fatalf("ListEndpoints after refused create = %d / %v", list.StatusCode(), list.Body)
	}

	del, err := client.DeleteEndpointWithResponse(ctx, pool.Id, projectID, branchID, uuid.New())
	if err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if del.StatusCode() != http.StatusNotImplemented {
		t.Fatalf("DeleteEndpoint(deploy off) status = %d, want 501; body = %s", del.StatusCode(), del.Body)
	}
}
