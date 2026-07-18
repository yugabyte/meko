package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/task"
)

// TestEndpointFlow drives the endpoint workflow through the generated client
// under the async-task contract: create returns 202 + a create_endpoint Task,
// and the deployed endpoint (with its address) materializes once the worker runs
// the task. It then covers get, list, the single-writer rule (now a FAILED task,
// not a synchronous 409), read-only endpoints, and retire (a delete_endpoint task
// that, once driven, removes the row). The branch is seeded directly so the test
// exercises the endpoint lifecycle without going through create-project/branch.
func TestEndpointFlow(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "ep-pool")

	projectID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ProjectID: projectID,
		BranchID:  branchID,
		Name:      "main",
		PgVersion: 18,
		IsDefault: true,
	})

	rw := api.CreateEndpointRequestRoleReadWrite
	ro := api.CreateEndpointRequestRoleReadOnly

	// Create a read_write endpoint -> 202 + a create_endpoint Task.
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateEndpoint status = %d, want 202, body = %s", created.StatusCode(), created.Body)
	}
	assertTaskAccepted(t, created.JSON202, created.HTTPResponse.Header.Get("Location"), "create_endpoint")

	// Drive the task: the endpoint row + address are materialized.
	drainTasks(t, store, backend, prov)

	// List -> exactly one endpoint, bound to the branch, read_write, with an address.
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, projectID, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.StatusCode() != http.StatusOK || list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("ListEndpoints status = %d, body = %s", list.StatusCode(), list.Body)
	}
	ep := list.JSON200.Endpoints[0]
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

	// A second read_write on the same branch violates the single-writer rule. Under
	// the async contract this is no longer a synchronous 409 — the create is
	// accepted (202) and the conflict surfaces as a FAILED task when its step's
	// EnsureEndpointRecord hits the single-writer index.
	dup, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint(dup writer): %v", err)
	}
	if dup.StatusCode() != http.StatusAccepted {
		t.Fatalf("dup writer status = %d, want 202, body = %s", dup.StatusCode(), dup.Body)
	}
	dupID := dup.JSON202.Id
	drainTasks(t, store, backend, prov)
	dupTask, err := store.Get(ctx, dupID)
	if err != nil || dupTask == nil {
		t.Fatalf("get dup-writer task: task=%v err=%v", dupTask, err)
	}
	if dupTask.Status != task.StatusFailed || dupTask.Error == nil {
		t.Errorf("dup-writer task = {status:%q err:%v}, want failed (single-writer)", dupTask.Status, dupTask.Error)
	}
	// Still exactly one endpoint (the duplicate did not materialize a second writer).
	if l, _ := client.ListEndpointsWithResponse(ctx, pool.Id, projectID, branchID, nil); l.JSON200 == nil || len(l.JSON200.Endpoints) != 1 {
		t.Errorf("endpoints after a failed dup-writer = %v, want still 1", l.Body)
	}

	// A read_only endpoint is allowed alongside the writer -> 202, and materializes.
	roResp, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &ro})
	if err != nil {
		t.Fatalf("CreateEndpoint(read_only): %v", err)
	}
	if roResp.StatusCode() != http.StatusAccepted {
		t.Fatalf("read_only status = %d, want 202, body = %s", roResp.StatusCode(), roResp.Body)
	}
	drainTasks(t, store, backend, prov)
	if l, _ := client.ListEndpointsWithResponse(ctx, pool.Id, projectID, branchID, nil); l.JSON200 == nil || len(l.JSON200.Endpoints) != 2 {
		t.Errorf("endpoints after read_only = %v, want 2 (writer + replica)", l.Body)
	}

	// Retire the writer -> 202 + a delete_endpoint Task; drive it, then it's gone.
	del, err := client.DeleteEndpointWithResponse(ctx, pool.Id, projectID, branchID, ep.Id)
	if err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if del.StatusCode() != http.StatusAccepted {
		t.Fatalf("DeleteEndpoint status = %d, want 202, body = %s", del.StatusCode(), del.Body)
	}
	assertTaskAccepted(t, del.JSON202, del.HTTPResponse.Header.Get("Location"), "delete_endpoint")
	drainTasks(t, store, backend, prov)

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
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})
	getMissing, err := client.GetEndpointWithResponse(ctx, pool.Id, projectID, branchID, uuid.New())
	if err != nil {
		t.Fatalf("GetEndpoint(missing): %v", err)
	}
	if getMissing.StatusCode() != http.StatusNotFound {
		t.Fatalf("GetEndpoint(missing) status = %d, body = %s", getMissing.StatusCode(), getMissing.Body)
	}
}

// TestEndpointDiskType: disk_type=local is rejected with 400 before any task is
// planned (cheap validation; local disks not yet supported); disk_type=network
// (or omitted) is accepted (202) and, once driven, deploys exactly one endpoint.
func TestEndpointDiskType(t *testing.T) {
	prov := api.Provisioning{Deployer: &fakeDeployer{}}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "disk-pool")
	projectID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	network := api.Network
	local := api.Local

	// disk_type=local -> 400 (cheap validation), and no endpoint exists.
	bad, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &rw, DiskType: &local})
	if err != nil {
		t.Fatalf("CreateEndpoint(local): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest || bad.JSON400 == nil {
		t.Fatalf("local status = %d, want 400; body = %s", bad.StatusCode(), bad.Body)
	}
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, projectID, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.StatusCode() != http.StatusOK || list.JSON200 == nil || len(list.JSON200.Endpoints) != 0 {
		t.Fatalf("after rejected local create, endpoints = %v (want none)", list.Body)
	}

	// disk_type=network -> 202 (the default disk); drive it, then it deployed.
	ok, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &rw, DiskType: &network})
	if err != nil {
		t.Fatalf("CreateEndpoint(network): %v", err)
	}
	if ok.StatusCode() != http.StatusAccepted {
		t.Fatalf("network status = %d, want 202; body = %s", ok.StatusCode(), ok.Body)
	}
	drainTasks(t, store, backend, prov)
	if got := soleTask(t, store); got.Status != task.StatusSucceeded {
		t.Fatalf("create_endpoint(network) task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}
	if l, _ := client.ListEndpointsWithResponse(ctx, pool.Id, projectID, branchID, nil); l.JSON200 == nil || len(l.JSON200.Endpoints) != 1 {
		t.Errorf("endpoints after network create = %v, want 1", l.Body)
	}
}

// TestEndpointResources: an omitted resources block reserves the 2-core / 4Gi
// default, and an explicit block threads through the handler → task → deployer
// spec verbatim (where the chart renders it as request == limit for Guaranteed
// QoS).
func TestEndpointResources(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "res-pool")
	projectID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite

	// (1) omitted resources -> the 2-core / 4Gi default reaches the deployer.
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint(default resources): %v", err)
	}
	if created.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", created.StatusCode(), created.Body)
	}
	drainTasks(t, store, backend, prov)
	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1", len(dep.created))
	}
	if dep.created[0].CPU != "2" || dep.created[0].Memory != "4Gi" {
		t.Errorf("default deploy spec resources = (%q, %q), want (2, 4Gi)", dep.created[0].CPU, dep.created[0].Memory)
	}

	// (2) explicit resources -> threaded through verbatim (on a second branch, so
	// the single-writer rule isn't tripped).
	branch2 := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: branch2, Name: "b2", PgVersion: 18})
	cpu, mem := "8", "16Gi"
	ok, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branch2, api.CreateEndpointRequest{
		Role:      &rw,
		Resources: &api.EndpointResources{Cpu: &cpu, Memory: &mem},
	})
	if err != nil {
		t.Fatalf("CreateEndpoint(explicit resources): %v", err)
	}
	if ok.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", ok.StatusCode(), ok.Body)
	}
	drainTasks(t, store, backend, prov)
	last := dep.created[len(dep.created)-1]
	if last.CPU != cpu || last.Memory != mem {
		t.Errorf("explicit deploy spec resources = (%q, %q), want (%q, %q)", last.CPU, last.Memory, cpu, mem)
	}

	// (3) a malformed quantity is rejected synchronously with 400, rather than
	// being accepted and only failing later in the worker's helm run.
	badCPU, badMem := "2x", "4Gii"
	for _, res := range []*api.EndpointResources{{Cpu: &badCPU}, {Memory: &badMem}} {
		resp, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branch2, api.CreateEndpointRequest{Role: &rw, Resources: res})
		if err != nil {
			t.Fatalf("CreateEndpoint(invalid resources): %v", err)
		}
		if resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("invalid resources %+v: status = %d, want 400; body = %s", res, resp.StatusCode(), resp.Body)
		}
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
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

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
