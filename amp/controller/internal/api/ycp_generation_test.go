package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// The applied-generation contract, end to end through the generated client and a
// real worker: a caller states the generation it is applying, the worker records
// it only when the task actually finishes, and the resource echoes it back so the
// reconcile agent can tell a landed update from a 202 (meko#985).

// TestResizeRecordsAppliedGenerationOnlyOnSuccess is the core of it. A resize is
// accepted, and the endpoint echoes nothing until the task has run.
func TestResizeRecordsAppliedGenerationOnlyOnSuccess(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "gen-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true,
	})

	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil || created.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateEndpoint: status=%d err=%v", created.StatusCode(), err)
	}
	drainTasks(t, store, backend, prov)

	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if err != nil || list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("ListEndpoints: %v", err)
	}
	ep := list.JSON200.Endpoints[0]

	// A create is not a YCP update, so nothing is recorded: creates self-heal
	// through presence and need no echo.
	if ep.YcpGeneration != nil {
		t.Errorf("endpoint ycp_generation after create = %d, want null", *ep.YcpGeneration)
	}

	// Resize, stating the generation being applied.
	// This control plane enforces a memory floor per vCPU, so a resize names both.
	cpu, mem, gen := "4", "32Gi", int64(6)
	resized, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id,
		api.ResizeEndpointRequest{Resources: api.EndpointResources{Cpu: &cpu, Memory: &mem}, YcpGeneration: &gen})
	if err != nil || resized.StatusCode() != http.StatusAccepted {
		t.Fatalf("ResizeEndpoint: status=%d err=%v body=%s", resized.StatusCode(), err, resized.Body)
	}
	// The task says which generation it is applying, so an operator debugging a
	// stuck environment can read it off the task instead of the opaque params blob.
	if resized.JSON202 == nil || resized.JSON202.YcpGeneration == nil || *resized.JSON202.YcpGeneration != gen {
		t.Errorf("task ycp_generation = %v, want %d", resized.JSON202.YcpGeneration, gen)
	}

	// Before the worker runs, the 202 has bought nothing: the row still echoes
	// nothing. This is the whole point — a 202 is not evidence.
	pending, _ := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id)
	if pending.JSON200 == nil {
		t.Fatal("GetEndpoint before the worker ran")
	}
	if pending.JSON200.YcpGeneration != nil {
		t.Errorf("endpoint echoes generation %d on a 202 alone, want null until the task completes",
			*pending.JSON200.YcpGeneration)
	}

	drainTasks(t, store, backend, prov)

	done, _ := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id)
	if done.JSON200 == nil || done.JSON200.YcpGeneration == nil {
		t.Fatalf("endpoint ycp_generation after the task ran = null, want %d", gen)
	}
	if *done.JSON200.YcpGeneration != gen {
		t.Errorf("endpoint ycp_generation = %d, want %d", *done.JSON200.YcpGeneration, gen)
	}
	if done.JSON200.Resources == nil || *done.JSON200.Resources.Cpu != cpu {
		t.Errorf("resources = %+v, want cpu %s applied alongside the generation", done.JSON200.Resources, cpu)
	}
}

// A caller that is not the reconcile agent sends no generation, and must leave
// whatever the row records untouched — the field is for YCP, not for everyone.
func TestResizeWithoutAGenerationRecordsNothing(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "gen-pool-2")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true,
	})
	rw := api.CreateEndpointRequestRoleReadWrite
	if _, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw}); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	drainTasks(t, store, backend, prov)
	list, _ := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	ep := list.JSON200.Endpoints[0]

	// Record a generation the way a YCP-driven resize would.
	if err := backend.SetEndpointYCPGeneration(ctx, ep.Id, 6); err != nil {
		t.Fatalf("SetEndpointYCPGeneration: %v", err)
	}

	cpu, mem := "4", "32Gi"
	resp, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id,
		api.ResizeEndpointRequest{Resources: api.EndpointResources{Cpu: &cpu, Memory: &mem}})
	if err != nil || resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("ResizeEndpoint: status=%d err=%v", resp.StatusCode(), err)
	}
	if resp.JSON202.YcpGeneration != nil {
		t.Errorf("task ycp_generation = %d, want null for a non-YCP caller", *resp.JSON202.YcpGeneration)
	}
	drainTasks(t, store, backend, prov)

	got, _ := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id)
	if got.JSON200.YcpGeneration == nil || *got.JSON200.YcpGeneration != 6 {
		t.Errorf("endpoint ycp_generation = %v, want the recorded 6 left alone", got.JSON200.YcpGeneration)
	}
}

// The branch half, plus the interlock the re-issuing agent needs: a second add
// while one is in flight is a 409, not a duplicate task redeploying the same
// endpoints.
func TestAddBranchExtensionsGenerationAndInterlock(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep, Extensions: extensions.NewCatalog("quay.io/yugabyte")}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "gen-pool-3")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true,
	})

	gen := int64(3)
	added, err := client.AddBranchExtensionsWithResponse(ctx, pool.Id, clusterID, branchID,
		api.AddBranchExtensionsRequest{Extensions: []string{"pgvector"}, YcpGeneration: &gen})
	if err != nil || added.StatusCode() != http.StatusAccepted {
		t.Fatalf("AddBranchExtensions: status=%d err=%v body=%s", added.StatusCode(), err, added.Body)
	}
	if added.JSON202 == nil || added.JSON202.YcpGeneration == nil || *added.JSON202.YcpGeneration != gen {
		t.Errorf("task ycp_generation = %v, want %d", added.JSON202.YcpGeneration, gen)
	}

	// First-writer-wins while that task is non-terminal: a second add is refused.
	// Without it the agent's re-issue-until-confirmed would stack up tasks, each
	// redeploying every endpoint on the branch.
	second, err := client.AddBranchExtensionsWithResponse(ctx, pool.Id, clusterID, branchID,
		api.AddBranchExtensionsRequest{Extensions: []string{"pgvector"}, YcpGeneration: &gen})
	if err != nil {
		t.Fatalf("second AddBranchExtensions: %v", err)
	}
	if second.StatusCode() != http.StatusConflict {
		t.Fatalf("second AddBranchExtensions status = %d, want 409; body = %s", second.StatusCode(), second.Body)
	}

	drainTasks(t, store, backend, prov)

	br, err := client.GetBranchWithResponse(ctx, pool.Id, clusterID, branchID)
	if err != nil || br.JSON200 == nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if br.JSON200.YcpGeneration == nil || *br.JSON200.YcpGeneration != gen {
		t.Fatalf("branch ycp_generation = %v, want %d", br.JSON200.YcpGeneration, gen)
	}
	if br.JSON200.Extensions == nil || len(*br.JSON200.Extensions) != 1 || (*br.JSON200.Extensions)[0] != "pgvector" {
		t.Errorf("branch extensions = %v, want [pgvector] applied alongside the generation", br.JSON200.Extensions)
	}

	// And once terminal, the lock is released.
	third, err := client.AddBranchExtensionsWithResponse(ctx, pool.Id, clusterID, branchID,
		api.AddBranchExtensionsRequest{Extensions: []string{"pgvector"}})
	if err != nil || third.StatusCode() != http.StatusAccepted {
		t.Fatalf("add after the first went terminal: status=%d err=%v", third.StatusCode(), err)
	}
}
