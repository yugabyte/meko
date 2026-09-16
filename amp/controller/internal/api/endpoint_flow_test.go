package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
	"github.com/yugabyte/amp/controller/internal/task"
)

// TestEndpointFlow drives the endpoint workflow through the generated client
// under the async-task contract: create returns 202 + a create_endpoint Task,
// and the deployed endpoint (with its address) materializes once the worker runs
// the task. It then covers get, list, the single-writer rule (now a FAILED task,
// not a synchronous 409), read-only endpoints, and retire (a delete_endpoint task
// that, once driven, removes the row). The branch is seeded directly so the test
// exercises the endpoint lifecycle without going through create-cluster/branch.
func TestEndpointFlow(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "ep-pool")

	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ClusterID: clusterID,
		BranchID:  branchID,
		Name:      "main",
		PgVersion: 18,
		IsDefault: true,
	})

	rw := api.CreateEndpointRequestRoleReadWrite
	ro := api.CreateEndpointRequestRoleReadOnly

	// Create a read_write endpoint -> 202 + a create_endpoint Task.
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw, Password: strptr(testPassword)})
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
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.StatusCode() != http.StatusOK || list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("ListEndpoints status = %d, body = %s", list.StatusCode(), list.Body)
	}
	ep := list.JSON200.Endpoints[0]
	if ep.ClusterId != clusterID || ep.BranchId != branchID {
		t.Errorf("endpoint binding = (%s,%s), want (%s,%s)", ep.ClusterId, ep.BranchId, clusterID, branchID)
	}
	if ep.Role != api.EndpointRoleReadWrite {
		t.Errorf("role = %q, want read_write", ep.Role)
	}
	if ep.Address == "" {
		t.Errorf("address = %q, want a deployed address", ep.Address)
	}

	// Get it back -> 200.
	got, err := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id)
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
	dup, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw, Password: strptr(testPassword)})
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
	if l, _ := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil); l.JSON200 == nil || len(l.JSON200.Endpoints) != 1 {
		t.Errorf("endpoints after a failed dup-writer = %v, want still 1", l.Body)
	}

	// A read_only endpoint is allowed alongside the writer -> 202, and materializes.
	roResp, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &ro})
	if err != nil {
		t.Fatalf("CreateEndpoint(read_only): %v", err)
	}
	if roResp.StatusCode() != http.StatusAccepted {
		t.Fatalf("read_only status = %d, want 202, body = %s", roResp.StatusCode(), roResp.Body)
	}
	drainTasks(t, store, backend, prov)
	if l, _ := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil); l.JSON200 == nil || len(l.JSON200.Endpoints) != 2 {
		t.Errorf("endpoints after read_only = %v, want 2 (writer + replica)", l.Body)
	}

	// Retire the writer -> 202 + a delete_endpoint Task; drive it, then it's gone.
	del, err := client.DeleteEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id)
	if err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if del.StatusCode() != http.StatusAccepted {
		t.Fatalf("DeleteEndpoint status = %d, want 202, body = %s", del.StatusCode(), del.Body)
	}
	assertTaskAccepted(t, del.JSON202, del.HTTPResponse.Header.Get("Location"), "delete_endpoint")
	drainTasks(t, store, backend, prov)

	// It is gone now -> get 404.
	gone, err := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id)
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
	missing, err := client.CreateEndpointWithResponse(ctx, pool.Id, uuid.New(), uuid.New(), api.CreateEndpointRequest{Role: &rw, Password: strptr(testPassword)})
	if err != nil {
		t.Fatalf("CreateEndpoint(missing branch): %v", err)
	}
	if missing.StatusCode() != http.StatusNotFound || missing.JSON404 == nil {
		t.Fatalf("missing branch status = %d, body = %s", missing.StatusCode(), missing.Body)
	}

	// Get against a missing endpoint (branch seeded) -> 404.
	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})
	getMissing, err := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, uuid.New())
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
	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	network := api.Network
	local := api.Local

	// disk_type=local -> 400 (cheap validation), and no endpoint exists.
	bad, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw, DiskType: &local, Password: strptr(testPassword)})
	if err != nil {
		t.Fatalf("CreateEndpoint(local): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest || bad.JSON400 == nil {
		t.Fatalf("local status = %d, want 400; body = %s", bad.StatusCode(), bad.Body)
	}
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.StatusCode() != http.StatusOK || list.JSON200 == nil || len(list.JSON200.Endpoints) != 0 {
		t.Fatalf("after rejected local create, endpoints = %v (want none)", list.Body)
	}

	// disk_type=network -> 202 (the default disk); drive it, then it deployed.
	ok, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw, DiskType: &network, Password: strptr(testPassword)})
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
	if l, _ := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil); l.JSON200 == nil || len(l.JSON200.Endpoints) != 1 {
		t.Errorf("endpoints after network create = %v, want 1", l.Body)
	}
}

// TestClusterExtensions covers externalized extensions: they are set at
// createCluster onto the default branch (a known name is accepted, an unknown one
// is a 400), mounted onto the endpoints that branch deploys, surfaced on the
// Cluster and Branch APIs, and extended per branch with addBranchExtensions
// (which redeploys the branch's endpoints to pick up the new mounts).
func TestClusterExtensions(t *testing.T) {
	// A deployer with a version to report, so the create records one and the
	// redeploy below has something to lose.
	const deployerVersion = "ep-2026.08.1"
	dep := &fakeDeployer{version: deployerVersion}
	prov := api.Provisioning{Deployer: dep, Extensions: extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "ext-pool")

	// Unknown extension -> 400 before any task is planned.
	unknown := []string{"not-a-real-ext"}
	bad, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{Name: "bad", Extensions: &unknown})
	if err != nil {
		t.Fatalf("CreateCluster(unknown ext): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest || bad.JSON400 == nil {
		t.Fatalf("unknown ext status = %d, want 400; body = %s", bad.StatusCode(), bad.Body)
	}

	// Known extension + with_writer -> 202; once driven, the writer endpoint
	// mounts pgvector (resolved to its pinned image).
	want := []string{"pgvector"}
	withWriter := true
	// A non-default size on purpose: the redeploy assertion below can only catch a
	// dropped field if losing it is distinguishable from the deployer's defaults.
	cpu, mem := "4", "32Gi" // non-default, and at the 8 GiB per vCPU this control plane requires
	ok, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "shop", Extensions: &want, WithWriter: &withWriter, Password: strptr(testPassword),
		Resources: &api.EndpointResources{Cpu: &cpu, Memory: &mem},
	})
	if err != nil {
		t.Fatalf("CreateCluster(pgvector): %v", err)
	}
	if ok.StatusCode() != http.StatusAccepted || ok.JSON202 == nil {
		t.Fatalf("pgvector status = %d, want 202; body = %s", ok.StatusCode(), ok.Body)
	}
	clusterID := *ok.JSON202.ClusterId
	drainTasks(t, store, backend, prov)
	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1 (the writer)", len(dep.created))
	}
	exts := dep.created[0].Extensions
	if len(exts) != 1 || exts[0].Name != "pgvector" || exts[0].Image != "quay.io/yugabyte/amp-ext-pgvector:0.8.2" {
		t.Fatalf("deploy spec extensions = %+v, want the pinned pgvector", exts)
	}
	// The set is visible on the Cluster API response.
	gotCluster, err := client.GetClusterWithResponse(ctx, pool.Id, clusterID)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if gotCluster.JSON200 == nil {
		t.Fatalf("GetCluster: body = %s", gotCluster.Body)
	}
	if gotCluster.JSON200.Extensions == nil ||
		len(*gotCluster.JSON200.Extensions) != 1 || (*gotCluster.JSON200.Extensions)[0] != "pgvector" {
		t.Errorf("GetCluster extensions = %v, want [pgvector]", gotCluster.JSON200.Extensions)
	}
	branchID := gotCluster.JSON200.DefaultBranch.BranchId

	// addBranchExtensions(pg_cron) on the default branch -> 202; driving it
	// redeploys the writer with both extensions (sorted), pg_cron carrying
	// Preload:true.
	cron := []string{"pg_cron"}
	cok, err := client.AddBranchExtensionsWithResponse(ctx, pool.Id, clusterID, branchID, api.AddBranchExtensionsRequest{Extensions: cron})
	if err != nil {
		t.Fatalf("AddBranchExtensions(pg_cron): %v", err)
	}
	if cok.StatusCode() != http.StatusAccepted {
		t.Fatalf("pg_cron status = %d, want 202; body = %s", cok.StatusCode(), cok.Body)
	}
	drainTasks(t, store, backend, prov)
	// The redeploy re-renders the endpoint from its stored desired state, so it
	// must carry the endpoint's own Postgres version, size and endpoint version
	// through, not just the changed mounts. This is the only assertion covering
	// that through a real backend: a BranchEndpointDeploys that stopped reporting
	// any of them would otherwise silently resize the endpoint, move its Postgres
	// major version, or revert it to the deployer's configured version on the next
	// extension change.
	// Compared against the CREATE's spec rather than literals, so the assertion
	// does not encode the deployer's defaults — and so it cross-checks two
	// different paths: create renders from the task args, the redeploy renders
	// from BranchEndpointDeploys.
	created, redeployed := dep.created[0], dep.created[len(dep.created)-1]
	// The create renders the request, so it is where the literals belong — and it
	// is what pins the cross-check below to a size that is not a deployer default.
	if created.CPU != cpu || created.Memory != mem {
		t.Fatalf("create spec = %s/%s, want the requested %s/%s", created.CPU, created.Memory, cpu, mem)
	}
	if redeployed.PgVersion != created.PgVersion || redeployed.CPU != created.CPU || redeployed.Memory != created.Memory {
		t.Errorf("redeploy spec = pg%d %s/%s, want pg%d %s/%s (the endpoint's own desired state)",
			redeployed.PgVersion, redeployed.CPU, redeployed.Memory, created.PgVersion, created.CPU, created.Memory)
	}
	// The create asked for no version and the deployer answered with its own; that
	// answer is on the row, so the redeploy must pin it rather than ask for the
	// default again.
	if created.Version != "" {
		t.Errorf("create spec version = %q, want empty (the create pinned nothing)", created.Version)
	}
	if redeployed.Version != deployerVersion {
		t.Errorf("redeploy spec version = %q, want %q (the version the create recorded)", redeployed.Version, deployerVersion)
	}
	// And it is what the API reports the endpoint as running.
	gotEp, err := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, uuid.MustParse(created.EndpointID))
	if err != nil {
		t.Fatalf("GetEndpoint: %v", err)
	}
	if gotEp.JSON200 == nil {
		t.Fatalf("GetEndpoint: body = %s", gotEp.Body)
	}
	if gotEp.JSON200.Version == nil || *gotEp.JSON200.Version != deployerVersion {
		t.Errorf("GetEndpoint version = %v, want %q", gotEp.JSON200.Version, deployerVersion)
	}
	last := redeployed.Extensions
	if len(last) != 2 {
		t.Fatalf("redeploy spec extensions = %+v, want both pg_cron and pgvector", last)
	}
	var sawCron bool
	for _, e := range last {
		if e.Name == "pg_cron" {
			sawCron = true
			if e.Image != "quay.io/yugabyte/amp-ext-pg_cron:1.6.7" || !e.Preload {
				t.Errorf("pg_cron mount = %+v, want pinned image with Preload:true", e)
			}
		}
	}
	if !sawCron {
		t.Errorf("redeploy spec extensions = %+v, want pg_cron present", last)
	}
	gotBranch, err := client.GetBranchWithResponse(ctx, pool.Id, clusterID, branchID)
	if err != nil {
		t.Fatalf("GetBranch(after add): %v", err)
	}
	if gotBranch.JSON200 == nil {
		t.Fatalf("GetBranch(after add): body = %s", gotBranch.Body)
	}
	if gotBranch.JSON200.Extensions == nil || len(*gotBranch.JSON200.Extensions) != 2 {
		t.Errorf("GetBranch extensions after add = %v, want [pg_cron pgvector]", gotBranch.JSON200.Extensions)
	}

	// An unknown name on the add path is also a 400.
	addBad, err := client.AddBranchExtensionsWithResponse(ctx, pool.Id, clusterID, branchID, api.AddBranchExtensionsRequest{Extensions: unknown})
	if err != nil {
		t.Fatalf("AddBranchExtensions(unknown): %v", err)
	}
	if addBad.StatusCode() != http.StatusBadRequest || addBad.JSON400 == nil {
		t.Fatalf("add unknown ext status = %d, want 400; body = %s", addBad.StatusCode(), addBad.Body)
	}
}

// TestClusterExtensionsUnsupported: requesting extensions when the controller
// has no catalog (e.g. the Docker driver / feature off) is rejected with 400,
// rather than silently ignored — on both createCluster and addBranchExtensions.
func TestClusterExtensionsUnsupported(t *testing.T) {
	prov := api.Provisioning{Deployer: &fakeDeployer{}} // no Extensions catalog
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "ext-off-pool")

	want := []string{"pgvector"}
	resp, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{Name: "c1", Extensions: &want})
	if err != nil {
		t.Fatalf("CreateCluster(ext, no catalog): %v", err)
	}
	if resp.StatusCode() != http.StatusBadRequest || resp.JSON400 == nil {
		t.Fatalf("ext without catalog status = %d, want 400; body = %s", resp.StatusCode(), resp.Body)
	}

	// And on the add path against an existing cluster's default branch.
	cluster := mustCreateCluster(t, client, store, backend, pool.Id, "c2")
	addResp, err := client.AddBranchExtensionsWithResponse(ctx, pool.Id, cluster.Id, cluster.DefaultBranch.BranchId, api.AddBranchExtensionsRequest{Extensions: want})
	if err != nil {
		t.Fatalf("AddBranchExtensions(no catalog): %v", err)
	}
	if addResp.StatusCode() != http.StatusBadRequest || addResp.JSON400 == nil {
		t.Fatalf("add ext without catalog status = %d, want 400; body = %s", addResp.StatusCode(), addResp.Body)
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
	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite

	// (1) omitted resources -> the 2-core default, and the memory its cores are
	// owed at 8 GiB each, reach the deployer.
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw, Password: strptr(testPassword)})
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
	if dep.created[0].CPU != "2" || dep.created[0].Memory != "16Gi" {
		t.Errorf("default deploy spec resources = (%q, %q), want (2, 16Gi)", dep.created[0].CPU, dep.created[0].Memory)
	}

	// (2) explicit resources -> threaded through verbatim (on a second branch, so
	// the single-writer rule isn't tripped).
	branch2 := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branch2, Name: "b2", PgVersion: 18})
	cpu, mem := "8", "64Gi"
	ok, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branch2, api.CreateEndpointRequest{
		Role:      &rw,
		Resources: &api.EndpointResources{Cpu: &cpu, Memory: &mem}, Password: strptr(testPassword)})
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
		resp, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branch2, api.CreateEndpointRequest{Role: &rw, Resources: res, Password: strptr(testPassword)})
		if err != nil {
			t.Fatalf("CreateEndpoint(invalid resources): %v", err)
		}
		if resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("invalid resources %+v: status = %d, want 400; body = %s", res, resp.StatusCode(), resp.Body)
		}
	}
}

// memoryNoUnit are the memory quantities Kubernetes accepts and a compute pod
// never means: a bare number (and its exponent form) is a BYTE count, the milli
// suffix is a fraction of a byte, and Ki/k is kilobytes. An endpoint created
// with one can never be resized in place afterwards — a cpu-only resize carries
// the stored value forward and the kubelet refuses to lower the limit below
// current usage — so they are 400s, not sizes (meko#492).
var memoryNoUnit = []string{"4", "4m", "512Ki", "4k", "1e9"}

// memoryWithUnit is the other half of the contract: an explicit Mi-or-larger
// unit, binary or decimal-SI, is what the validation asks for and must pass.
var memoryWithUnit = []string{"4Gi", "512Mi", "1500M", "1.5Gi"}

// memoryUnitCPU is the cpu these shapes are paired with.
//
// The unit contract and the memory-per-vCPU floor are separate rules that both
// apply here, and the smallest value above (512Mi) only clears the floor on a
// small enough endpoint: 62m of a core is owed 496Mi at 8 GiB/vCPU. Pairing
// every case with this cpu keeps the unit variety — Mi, decimal-SI, fractional
// Gi — which is what this test is actually about.
const memoryUnitCPU = "62m"

// TestEndpointMemoryRequiresUnit: create and resize both reject a memory quantity
// with no unit, and accept the same intent expressed with one. Endpoints an older
// controller already stored a unitless value for are repaired by migration 0009,
// not at read time — every reader of the field agrees on what it says.
func TestEndpointMemoryRequiresUnit(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "mem-unit-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})
	rw := api.CreateEndpointRequestRoleReadWrite

	for _, mem := range memoryNoUnit {
		m := mem
		resp, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{
			Role: &rw, Resources: &api.EndpointResources{Memory: &m}, Password: strptr(testPassword)})
		if err != nil {
			t.Fatalf("CreateEndpoint(memory %q): %v", m, err)
		}
		if resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("create with memory %q: status = %d, want 400 (a unitless quantity is a byte count); body = %s", m, resp.StatusCode(), resp.Body)
		}
	}
	// A rejected create is rejected before the task is planned, so it leaves no
	// endpoint behind — the branch's read_write slot is still free below.
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if err != nil || list.JSON200 == nil {
		t.Fatalf("ListEndpoints: err = %v, body = %s", err, list.Body)
	}
	if len(list.JSON200.Endpoints) != 0 {
		t.Fatalf("endpoints after %d rejected creates = %d, want 0", len(memoryNoUnit), len(list.JSON200.Endpoints))
	}

	mem := memoryWithUnit[0]
	unitCPU := memoryUnitCPU
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{
		Role: &rw, Resources: &api.EndpointResources{Cpu: &unitCPU, Memory: &mem}, Password: strptr(testPassword)})
	if err != nil || created.StatusCode() != http.StatusAccepted {
		t.Fatalf("create with memory %q: status = %d, err = %v; body = %s", mem, created.StatusCode(), err, created.Body)
	}
	drainTasks(t, store, backend, prov)
	list, _ = client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("expected one endpoint, body = %s", list.Body)
	}
	epID := list.JSON200.Endpoints[0].Id

	// Every accepted shape survives a resize round-trip; every rejected one is a
	// 400 that leaves the stored reservation where it was.
	for _, m := range memoryWithUnit[1:] {
		mm := m
		resp, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, epID, api.ResizeEndpointRequest{
			Resources: api.EndpointResources{Memory: &mm},
		})
		if err != nil || resp.StatusCode() != http.StatusAccepted {
			t.Fatalf("resize to memory %q: status = %d, err = %v; body = %s", mm, resp.StatusCode(), err, resp.Body)
		}
		drainTasks(t, store, backend, prov)
		got, _ := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, epID)
		if got.JSON200 == nil || got.JSON200.Resources == nil || *got.JSON200.Resources.Memory != mm {
			t.Errorf("stored memory after resize to %q = %+v, want %q", mm, got.JSON200.Resources, mm)
		}
	}
	settled := memoryWithUnit[len(memoryWithUnit)-1]
	for _, m := range memoryNoUnit {
		mm := m
		resp, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, epID, api.ResizeEndpointRequest{
			Resources: api.EndpointResources{Memory: &mm},
		})
		if err != nil {
			t.Fatalf("ResizeEndpoint(memory %q): %v", mm, err)
		}
		if resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("resize to memory %q: status = %d, want 400; body = %s", mm, resp.StatusCode(), resp.Body)
		}
		got, _ := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, epID)
		if got.JSON200 == nil || got.JSON200.Resources == nil || *got.JSON200.Resources.Memory != settled {
			t.Errorf("stored memory after a rejected resize to %q = %+v, want the unchanged %q", mm, got.JSON200.Resources, settled)
		}
	}

	// A cpu-only resize keeps the stored memory, and the deployer is asked for
	// exactly what the endpoint reports — no read-time substitution stands between
	// the two. Small, because a cpu-only resize does not move memory: growing to 6
	// cores against the 1.5Gi settled above would be refused by the floor, which
	// is a different rule than the one under test here.
	newCPU := "100m"
	resp, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, epID, api.ResizeEndpointRequest{
		Resources: api.EndpointResources{Cpu: &newCPU},
	})
	if err != nil || resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("cpu-only resize: status = %d, err = %v; body = %s", resp.StatusCode(), err, resp.Body)
	}
	drainTasks(t, store, backend, prov)
	got, _ := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, epID)
	if got.JSON200 == nil || got.JSON200.Resources == nil ||
		*got.JSON200.Resources.Cpu != newCPU || *got.JSON200.Resources.Memory != settled {
		t.Errorf("size after a cpu-only resize = %+v, want (%s, %s)", got.JSON200.Resources, newCPU, settled)
	}
	if last := dep.resized[len(dep.resized)-1]; last.Memory != settled {
		t.Errorf("deployer was asked to resize to memory %q, want the stored %q", last.Memory, settled)
	}
}

// TestEndpointResize drives the resize workflow: create an endpoint, then resize
// it and confirm (a) the new size is persisted (GetEndpoint reports it), (b) the
// deployer was asked to resize the right instance, (c) the task result reports
// the in-place outcome, and (d) an unspecified dimension keeps its current value.
// Also covers the 400 (bad quantity) and 404 (missing endpoint) error paths.
func TestEndpointResize(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "resize-pool")
	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw, Password: strptr(testPassword)})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	drainTasks(t, store, backend, prov)
	list, _ := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("expected one endpoint, body = %s", list.Body)
	}
	ep := list.JSON200.Endpoints[0]
	// The created endpoint carries its (default) reservation on the API.
	if ep.Resources == nil || ep.Resources.Cpu == nil || *ep.Resources.Cpu != "2" {
		t.Fatalf("created endpoint resources = %+v, want cpu=2", ep.Resources)
	}
	_ = created

	// Resize cpu+memory -> 202 + a resize_endpoint task.
	cpu, mem := "4", "32Gi"
	resized, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id, api.ResizeEndpointRequest{
		Resources: api.EndpointResources{Cpu: &cpu, Memory: &mem},
	})
	if err != nil {
		t.Fatalf("ResizeEndpoint: %v", err)
	}
	if resized.StatusCode() != http.StatusAccepted {
		t.Fatalf("ResizeEndpoint status = %d, want 202; body = %s", resized.StatusCode(), resized.Body)
	}
	assertTaskAccepted(t, resized.JSON202, resized.HTTPResponse.Header.Get("Location"), "resize_endpoint")
	drainTasks(t, store, backend, prov)

	// The deployer was asked to resize this instance to the new size.
	if rs := dep.resizedSpecs(); len(rs) != 1 || rs[0].CPU != "4" || rs[0].Memory != "32Gi" {
		t.Fatalf("deployer resize calls = %+v, want one {cpu:4, memory:32Gi}", rs)
	}
	// The persisted size now reflects the resize.
	got, _ := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id)
	if got.JSON200 == nil || got.JSON200.Resources == nil || *got.JSON200.Resources.Cpu != "4" || *got.JSON200.Resources.Memory != "32Gi" {
		t.Fatalf("resized endpoint resources = %+v, want cpu=4 memory=32Gi", got.JSON200.Resources)
	}
	// The task result reports the in-place outcome.
	taskResp, _ := client.GetTaskWithResponse(ctx, resized.JSON202.Id)
	if taskResp.JSON200 == nil || taskResp.JSON200.Result == nil {
		t.Fatalf("resize task result missing; body = %s", taskResp.Body)
	}
	if res, ok := taskResp.JSON200.Result.(map[string]any); !ok || res["in_place_resize"] != "completed" {
		t.Errorf("task result = %v, want in_place_resize=completed", taskResp.JSON200.Result)
	}

	// Resize only cpu -> memory keeps its current value (32Gi). Down to 3 cores
	// rather than up to 6: memory is unchanged either way, and shrinking keeps it
	// above the per-core floor, where growing to 6 would need 48Gi and is refused
	// (covered by TestEndpointShapeFloor).
	cpuOnly := "3"
	partial, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id, api.ResizeEndpointRequest{
		Resources: api.EndpointResources{Cpu: &cpuOnly},
	})
	if err != nil || partial.StatusCode() != http.StatusAccepted {
		t.Fatalf("ResizeEndpoint(cpu only): status = %d, err = %v", partial.StatusCode(), err)
	}
	drainTasks(t, store, backend, prov)
	if rs := dep.resizedSpecs(); len(rs) == 0 || rs[len(rs)-1].CPU != "3" || rs[len(rs)-1].Memory != "32Gi" {
		t.Errorf("partial resize spec = %+v, want last {3, 32Gi} (memory unchanged)", rs)
	}

	// A malformed quantity is rejected synchronously with 400.
	badCPU := "4x"
	bad, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id, api.ResizeEndpointRequest{
		Resources: api.EndpointResources{Cpu: &badCPU},
	})
	if err != nil {
		t.Fatalf("ResizeEndpoint(bad cpu): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest {
		t.Errorf("bad cpu status = %d, want 400; body = %s", bad.StatusCode(), bad.Body)
	}

	// Resizing a missing endpoint -> 404.
	missing, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, uuid.New(), api.ResizeEndpointRequest{
		Resources: api.EndpointResources{Cpu: &cpu},
	})
	if err != nil {
		t.Fatalf("ResizeEndpoint(missing): %v", err)
	}
	if missing.StatusCode() != http.StatusNotFound {
		t.Errorf("missing endpoint resize status = %d, want 404; body = %s", missing.StatusCode(), missing.Body)
	}
}

// TestEndpointResizeParallel fires many concurrent resize requests at the SAME
// endpoint. The resize is first-writer-wins with a lock: exactly ONE request is
// accepted (202) and holds the endpoint; every other concurrent request is
// rejected with 409 while that resize is in flight. Once the winner completes,
// the lock releases and a fresh resize is accepted again. Run under `-race` this
// also guards the concurrent task-creation path (the lock is enforced there).
func TestEndpointResizeParallel(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "par-pool")
	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	if _, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw, Password: strptr(testPassword)}); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	drainTasks(t, store, backend, prov)
	list, _ := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("expected one endpoint, body = %s", list.Body)
	}
	epID := list.JSON200.Endpoints[0].Id

	// Fire N concurrent resizes, each to a distinct size, WITHOUT draining — so
	// the first-planned resize is still in flight (pending) when the rest arrive.
	// Distinct sizes, each at the memory-per-core floor: this test is about
	// first-writer-wins, so a shape the API refuses would fail it for the wrong
	// reason.
	sizes := []struct{ cpu, mem string }{
		{"1", "8Gi"}, {"2", "16Gi"}, {"3", "24Gi"}, {"4", "32Gi"},
		{"5", "40Gi"}, {"6", "48Gi"}, {"7", "56Gi"}, {"8", "64Gi"},
	}
	type result struct {
		code     int
		cpu, mem string
	}
	results := make([]result, len(sizes))
	var wg sync.WaitGroup
	for i, s := range sizes {
		wg.Add(1)
		go func(i int, cpu, mem string) {
			defer wg.Done()
			resp, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, epID, api.ResizeEndpointRequest{
				Resources: api.EndpointResources{Cpu: &cpu, Memory: &mem},
			})
			if err != nil {
				t.Errorf("parallel resize (%s/%s): %v", cpu, mem, err)
				return
			}
			results[i] = result{resp.StatusCode(), cpu, mem}
		}(i, s.cpu, s.mem)
	}
	wg.Wait()

	// Exactly one winner (202); everyone else is locked out with 409.
	var winners []result
	for _, r := range results {
		switch r.code {
		case http.StatusAccepted:
			winners = append(winners, r)
		case http.StatusConflict: // 409 — expected for the losers
		default:
			t.Errorf("parallel resize got status %d, want 202 or 409", r.code)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("accepted resizes = %d, want exactly 1 (first-writer-wins)", len(winners))
	}
	// Exactly one resize task was planned (the losers never created one).
	tasks, _, err := store.List(ctx, task.TaskFilter{Kind: "resize_endpoint"}, 1000, 0)
	if err != nil {
		t.Fatalf("list resize tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("resize tasks planned = %d, want 1 (only the winner)", len(tasks))
	}

	// Drive the winner; the endpoint lands at exactly the winner's size.
	drainTasks(t, store, backend, prov)
	if got := statusOfTask(t, store, tasks[0].ID); got != task.StatusSucceeded {
		t.Errorf("winning resize task status = %v, want succeeded", got)
	}
	got, _ := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, epID)
	if got.JSON200 == nil || got.JSON200.Resources == nil ||
		*got.JSON200.Resources.Cpu != winners[0].cpu || *got.JSON200.Resources.Memory != winners[0].mem {
		t.Errorf("final size = %+v, want the winner's (%s, %s)", got.JSON200.Resources, winners[0].cpu, winners[0].mem)
	}

	// The lock released: a fresh resize is accepted again.
	nc, nm := "2", "16Gi"
	again, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, epID, api.ResizeEndpointRequest{
		Resources: api.EndpointResources{Cpu: &nc, Memory: &nm},
	})
	if err != nil || again.StatusCode() != http.StatusAccepted {
		t.Fatalf("resize after the in-flight one completed: status = %d, err = %v (lock should have released)", again.StatusCode(), err)
	}
}

// statusOfTask fetches a task's status, failing the test on a read error / nil.
func statusOfTask(t *testing.T, store *task.MemStore, id uuid.UUID) task.Status {
	t.Helper()
	tk, err := store.Get(context.Background(), id)
	if err != nil || tk == nil {
		t.Fatalf("get task %s: task=%v err=%v", id, tk, err)
	}
	return tk.Status
}

// TestEndpointResizeInPlaceOutcomes covers the non-completed in-place results.
// A pending/infeasible in-place resize is NOT a task failure — the size was
// persisted and the Deployment updated, so it converges on restart; the task
// succeeds and its result carries the outcome. A deployer error IS a failure,
// but the size was still persisted first (durable).
func TestEndpointResizeInPlaceOutcomes(t *testing.T) {
	type fixture struct {
		client            *api.ClientWithResponses
		backend           *core.MemBackend
		store             *task.MemStore
		prov              api.Provisioning
		poolID            uuid.UUID
		clusterID, branch uuid.UUID
		epID              uuid.UUID
	}
	seed := func(t *testing.T, dep *fakeDeployer) fixture {
		t.Helper()
		prov := api.Provisioning{Deployer: dep}
		client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
		ctx := context.Background()
		user := mustCreateUser(t, client, false)
		pool := mustCreatePool(t, client, user.Id, "outcome-pool")
		clusterID, branchID := uuid.New(), uuid.New()
		backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})
		rw := api.CreateEndpointRequestRoleReadWrite
		if _, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw, Password: strptr(testPassword)}); err != nil {
			t.Fatalf("CreateEndpoint: %v", err)
		}
		drainTasks(t, store, backend, prov)
		list, _ := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
		return fixture{client, backend, store, prov, pool.Id, clusterID, branchID, list.JSON200.Endpoints[0].Id}
	}

	cpu, mem := "4", "32Gi"

	// (a) infeasible: task succeeds, result reports infeasible, size still persisted.
	t.Run("infeasible still succeeds and persists", func(t *testing.T) {
		f := seed(t, &fakeDeployer{resizeOutcome: deploy.ResizeInfeasible})
		ctx := context.Background()
		resp, err := f.client.ResizeEndpointWithResponse(ctx, f.poolID, f.clusterID, f.branch, f.epID, api.ResizeEndpointRequest{
			Resources: api.EndpointResources{Cpu: &cpu, Memory: &mem},
		})
		if err != nil || resp.StatusCode() != http.StatusAccepted {
			t.Fatalf("resize status = %d, err = %v", resp.StatusCode(), err)
		}
		drainTasks(t, f.store, f.backend, f.prov)
		tk, _ := f.store.Get(ctx, resp.JSON202.Id)
		if tk == nil || tk.Status != task.StatusSucceeded {
			t.Fatalf("infeasible resize task status = %v, want succeeded (durable size set)", statusOf(tk))
		}
		taskResp, _ := f.client.GetTaskWithResponse(ctx, resp.JSON202.Id)
		if res, ok := taskResp.JSON200.Result.(map[string]any); !ok || res["in_place_resize"] != "infeasible" {
			t.Errorf("task result = %v, want in_place_resize=infeasible", taskResp.JSON200.Result)
		}
		got, _ := f.client.GetEndpointWithResponse(ctx, f.poolID, f.clusterID, f.branch, f.epID)
		if *got.JSON200.Resources.Cpu != cpu || *got.JSON200.Resources.Memory != mem {
			t.Errorf("persisted size = (%s,%s), want (%s,%s) despite infeasible in-place", *got.JSON200.Resources.Cpu, *got.JSON200.Resources.Memory, cpu, mem)
		}
	})

	// (b) deployer error: task fails, and the rejected size is NOT persisted — the
	// endpoint keeps its original (create-time) size, so GetEndpoint never reports
	// a size the pod never adopted.
	t.Run("deployer error fails the task and does not persist the size", func(t *testing.T) {
		f := seed(t, &fakeDeployer{resizeErr: errors.New("kube-apiserver rejected the resize")})
		ctx := context.Background()
		resp, err := f.client.ResizeEndpointWithResponse(ctx, f.poolID, f.clusterID, f.branch, f.epID, api.ResizeEndpointRequest{
			Resources: api.EndpointResources{Cpu: &cpu, Memory: &mem},
		})
		if err != nil || resp.StatusCode() != http.StatusAccepted {
			t.Fatalf("resize status = %d, err = %v", resp.StatusCode(), err)
		}
		drainTasks(t, f.store, f.backend, f.prov)
		tk, _ := f.store.Get(ctx, resp.JSON202.Id)
		if tk == nil || tk.Status != task.StatusFailed || tk.Error == nil {
			t.Fatalf("errored resize task = %v, want failed with an error", statusOf(tk))
		}
		// Original create-time defaults (2 cores, and the memory those cores are
		// owed), unchanged by the rejected resize.
		got, _ := f.client.GetEndpointWithResponse(ctx, f.poolID, f.clusterID, f.branch, f.epID)
		if *got.JSON200.Resources.Cpu != "2" || *got.JSON200.Resources.Memory != "16Gi" {
			t.Errorf("size after a rejected resize = (%s,%s), want the unchanged original (2,16Gi) — a rejected size must not be persisted", *got.JSON200.Resources.Cpu, *got.JSON200.Resources.Memory)
		}
	})
}

// statusOf is a nil-safe task status for error messages.
func statusOf(t *task.Task) any {
	if t == nil {
		return "<nil task>"
	}
	return t.Status
}

// TestEndpointResizeDeployDisabled: with the provisioner off, resize refuses with
// 501 (like create/retire) rather than planning a task no worker can run.
func TestEndpointResizeDeployDisabled(t *testing.T) {
	client, backend, _ := newTestServerDeploy(t, false)
	ctx := context.Background()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "off-resize-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	cpu := "4"
	resp, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, uuid.New(), api.ResizeEndpointRequest{
		Resources: api.EndpointResources{Cpu: &cpu},
	})
	if err != nil {
		t.Fatalf("ResizeEndpoint(deploy off): %v", err)
	}
	if resp.StatusCode() != http.StatusNotImplemented {
		t.Fatalf("ResizeEndpoint(deploy off) status = %d, want 501; body = %s", resp.StatusCode(), resp.Body)
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
	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw, Password: strptr(testPassword)})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusNotImplemented {
		t.Fatalf("CreateEndpoint(deploy off) status = %d, want 501; body = %s", created.StatusCode(), created.Body)
	}

	// No endpoint row was committed.
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.StatusCode() != http.StatusOK || list.JSON200 == nil || len(list.JSON200.Endpoints) != 0 {
		t.Fatalf("ListEndpoints after refused create = %d / %v", list.StatusCode(), list.Body)
	}

	del, err := client.DeleteEndpointWithResponse(ctx, pool.Id, clusterID, branchID, uuid.New())
	if err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if del.StatusCode() != http.StatusNotImplemented {
		t.Fatalf("DeleteEndpoint(deploy off) status = %d, want 501; body = %s", del.StatusCode(), del.Body)
	}
}

// TestEndpointLocalDeployer: with --deploy-driver=local the endpoint routes are
// live and the full create/delete flow works, but nothing is launched — the row
// is created with no address, and the delete drops it without retiring an
// instance. This is the path yb_amp_local takes: it spawns its own compute, so
// the controller must keep the record and stay out of the way.
func TestEndpointLocalDeployer(t *testing.T) {
	prov := api.Provisioning{Deployer: deploy.LocalDeployer{}}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "local-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true,
	})

	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID,
		api.CreateEndpointRequest{Role: &rw, Password: strptr(testPassword)})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateEndpoint status = %d, want 202; body = %s", created.StatusCode(), created.Body)
	}
	// The launcher reads the id it has to persist off the accepted task, before
	// the task has run.
	if created.JSON202.TargetRef == nil {
		t.Fatal("accepted create task carries no target_ref; the launcher has no endpoint id to record")
	}
	targetRef := *created.JSON202.TargetRef

	drainTasks(t, store, backend, prov)

	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.StatusCode() != http.StatusOK || list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("ListEndpoints = %d, body = %s", list.StatusCode(), list.Body)
	}
	ep := list.JSON200.Endpoints[0]
	if ep.Id.String() != targetRef {
		t.Errorf("endpoint id = %s, want the task's target_ref %s", ep.Id, targetRef)
	}
	if ep.Role != api.EndpointRoleReadWrite {
		t.Errorf("role = %q, want read_write", ep.Role)
	}
	// Nothing was deployed, so there is no address to hand out. An address here
	// would mean the controller thinks it placed a compute it did not.
	if ep.Address != "" {
		t.Errorf("address = %q, want empty: the local driver launches nothing", ep.Address)
	}

	del, err := client.DeleteEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id)
	if err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if del.StatusCode() != http.StatusAccepted {
		t.Fatalf("DeleteEndpoint status = %d, want 202; body = %s", del.StatusCode(), del.Body)
	}
	drainTasks(t, store, backend, prov)

	after, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints after delete: %v", err)
	}
	if after.StatusCode() != http.StatusOK || after.JSON200 == nil || len(after.JSON200.Endpoints) != 0 {
		t.Fatalf("endpoints after delete = %d, body = %s", after.StatusCode(), after.Body)
	}
}

// TestEndpointNameAndPgSettings: an endpoint carries a name unique to its
// branch, and its own pg_settings are persisted on its row (the branch's stay on
// the branch). The two are no longer merged here — the endpoint's supervisor
// reads both rows and merges them itself — so what the deployer must be handed
// is the endpoint id it needs to find them.
func TestEndpointNameAndPgSettings(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "named-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true,
		// The branch-wide baseline. work_mem is the one the endpoint overrides.
		PgSettings: map[string]string{"work_mem": "16MB", "log_min_duration_statement": "250ms"},
	})

	rw := api.CreateEndpointRequestRoleReadWrite
	name := "primary"
	epSettings := map[string]string{"work_mem": "64MB", "jit": "off"}
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID,
		api.CreateEndpointRequest{Role: &rw, Name: &name, PgSettings: &epSettings, Password: strptr(testPassword)})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateEndpoint = %d, want 202; body = %s", created.StatusCode(), created.Body)
	}
	drainTasks(t, store, backend, prov)

	ep := mustOneEndpoint(t, client, pool.Id, clusterID, branchID)
	if ep.Name != name {
		t.Errorf("endpoint name = %q, want %q", ep.Name, name)
	}
	// The row keeps only the endpoint's OWN settings; the branch's stay on the
	// branch, so a later change to the baseline reaches the endpoint.
	if ep.PgSettings == nil || (*ep.PgSettings)["work_mem"] != "64MB" || len(*ep.PgSettings) != 2 {
		t.Errorf("endpoint pg_settings = %v, want just its own two", ep.PgSettings)
	}

	// The merge is not resolved here any more: the supervisor reads the branch's
	// and the endpoint's pg_settings from amp_registry and merges them in SQL, so
	// a change is a registry write rather than a redeploy. What the deploy spec
	// must carry is the endpoint id the supervisor looks both rows up by —
	// without it the compute silently comes up on derived defaults alone.
	if len(dep.created) != 1 {
		t.Fatalf("deployer creates = %d, want 1", len(dep.created))
	}
	if got := dep.created[0].EndpointID; got != ep.Id.String() {
		t.Errorf("deploy spec EndpointID = %q, want the endpoint's id %q", got, ep.Id)
	}

	// A name already taken on the branch is refused synchronously, so the caller
	// learns immediately rather than by polling a doomed task.
	ro := api.CreateEndpointRequestRoleReadOnly
	dup, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID,
		api.CreateEndpointRequest{Role: &ro, Name: &name})
	if err != nil {
		t.Fatalf("CreateEndpoint(dup name): %v", err)
	}
	if dup.StatusCode() != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409; body = %s", dup.StatusCode(), dup.Body)
	}
}

// mustOneEndpoint returns the branch's single endpoint, failing if there is not
// exactly one. Tests re-read the endpoint after every task rather than reusing
// an earlier copy: the row is what the API actually reports, and a stale copy
// would hide a field the handler forgot to echo.
func mustOneEndpoint(t *testing.T, client *api.ClientWithResponses, poolID, clusterID, branchID uuid.UUID) api.Endpoint {
	t.Helper()
	list, err := client.ListEndpointsWithResponse(context.Background(), poolID, clusterID, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.StatusCode() != http.StatusOK || list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("ListEndpoints = %d, want exactly one endpoint; body = %s", list.StatusCode(), list.Body)
	}
	return list.JSON200.Endpoints[0]
}

// TestEndpointShapeFloor: the memory-per-core floor is enforced where a request
// arrives, not left to the pod. An endpoint accepted under the floor starts and
// serves, so the failure would surface as a database that looks slow rather than
// a request that was wrong — which is why this is a 400 and not a warning.
func TestEndpointShapeFloor(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "floor-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})
	rw := api.CreateEndpointRequestRoleReadWrite

	// Create below the floor: refused, and the message names what would be taken.
	cpu, thin := "4", "8Gi"
	bad, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{
		Role:      &rw,
		Resources: &api.EndpointResources{Cpu: &cpu, Memory: &thin}, Password: strptr(testPassword)})
	if err != nil {
		t.Fatalf("CreateEndpoint(thin): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", bad.StatusCode(), bad.Body)
	}
	if !strings.Contains(string(bad.Body), "32Gi") {
		t.Errorf("the refusal does not say what would be accepted: %s", bad.Body)
	}
	if len(dep.created) != 0 {
		t.Errorf("a refused shape reached the deployer: %+v", dep.created)
	}

	// Naming only cpu is accepted, because memory follows it.
	big := "8"
	ok, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{
		Role:      &rw,
		Resources: &api.EndpointResources{Cpu: &big}, Password: strptr(testPassword)})
	if err != nil || ok.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateEndpoint(cpu only): status = %d, err = %v; body = %s", ok.StatusCode(), err, ok.Body)
	}
	drainTasks(t, store, backend, prov)
	if len(dep.created) != 1 || dep.created[0].Memory != "64Gi" {
		t.Fatalf("deploy spec = %+v, want memory 64Gi for 8 cores", dep.created)
	}

	// Growing cpu alone is the ordinary way to fall under the floor: memory does
	// not move, so the endpoint would end up at a shape the API refuses.
	eps, _, err := backend.ListEndpoints(ctx, pool.Id, clusterID, branchID, 10, 0)
	if err != nil || len(eps) != 1 {
		t.Fatalf("ListEndpoints: %v (%d endpoints)", err, len(eps))
	}
	grow := "32"
	resized, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, eps[0].ID, api.ResizeEndpointRequest{
		Resources: api.EndpointResources{Cpu: &grow},
	})
	if err != nil {
		t.Fatalf("ResizeEndpoint(cpu only): %v", err)
	}
	if resized.StatusCode() != http.StatusBadRequest {
		t.Errorf("cpu-only resize past the floor = %d, want 400; body = %s", resized.StatusCode(), resized.Body)
	}
}

// The floor is the control plane's to set. A pool built to a different
// memory-per-core shape configures its own, and both halves of the rule have to
// follow it: what a request naming only CPU gets, and what a request naming both
// is held to.
func TestEndpointShapeFloorIsConfigurable(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep, MemoryGiBPerVCPU: 4}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "ratio-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})
	rw := api.CreateEndpointRequestRoleReadWrite

	// 4 GiB per core: a shape the built-in 8 would refuse.
	cpu, mem := "4", "16Gi"
	ok, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{
		Role:      &rw,
		Resources: &api.EndpointResources{Cpu: &cpu, Memory: &mem}, Password: strptr(testPassword)})
	if err != nil || ok.StatusCode() != http.StatusAccepted {
		t.Fatalf("a shape meeting the configured floor was refused: status = %d, err = %v; body = %s", ok.StatusCode(), err, ok.Body)
	}
	drainTasks(t, store, backend, prov)

	// And the fill-in follows the configured ratio, not the built-in.
	branch2 := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branch2, Name: "b2", PgVersion: 18})
	only := "8"
	ok2, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branch2, api.CreateEndpointRequest{
		Role:      &rw,
		Resources: &api.EndpointResources{Cpu: &only}, Password: strptr(testPassword)})
	if err != nil || ok2.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateEndpoint(cpu only): status = %d, err = %v", ok2.StatusCode(), err)
	}
	drainTasks(t, store, backend, prov)
	last := dep.created[len(dep.created)-1]
	if last.Memory != "32Gi" {
		t.Errorf("memory filled in = %q, want 32Gi (8 cores at the configured 4 GiB each)", last.Memory)
	}

	// Still a floor, just a different one.
	thin := "8Gi"
	bad, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, uuid.New(), api.CreateEndpointRequest{
		Role:      &rw,
		Resources: &api.EndpointResources{Cpu: &cpu, Memory: &thin}, Password: strptr(testPassword)})
	if err != nil {
		t.Fatalf("CreateEndpoint(under the configured floor): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — a configured floor is still a floor; body = %s", bad.StatusCode(), bad.Body)
	}
}

// A body that names only cpu, sent the way an operator's client sends it: the
// field absent from the JSON entirely.
//
// The generated client cannot express that difference — it marshals its struct,
// so an omitted *string and a server filling the gap in from an OpenAPI default
// look identical from there. Only a raw body shows that memory really follows
// cpu: a fixed default of 16Gi beside a cpu of 4 would be 4 GiB per core, and a
// 400 from the very floor this feature adds.
func TestCreateEndpointRawBodyWithOnlyCPU(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	backend := core.NewMemBackend()
	client, store, base := newTestServerRaw(t, backend, prov)

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "raw-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	url := fmt.Sprintf("%s/storage-pools/%s/clusters/%s/branches/%s/endpoints", base, pool.Id, clusterID, branchID)
	body := `{"role":"read_write","resources":{"cpu":"4"},"password":"hunter2hunter2"}`
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", resp.StatusCode, got)
	}
	drainTasks(t, store, backend, prov)

	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1", len(dep.created))
	}
	if dep.created[0].CPU != "4" || dep.created[0].Memory != "32Gi" {
		t.Errorf("deploy spec = (%q, %q), want (4, 32Gi) — memory must follow cpu, not arrive as a fixed default",
			dep.created[0].CPU, dep.created[0].Memory)
	}
}

// Endpoints created before the floor sit at 2 cores / 4Gi. Holding them to it
// would make them unresizable in every direction — including the shrink that
// brings them closer to it — so the rule for them is "no worse": a resize may not
// lower the memory each core has, and anything that does not is allowed through.
func TestResizeGrandfathersAnEndpointBelowTheFloor(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	backend := core.NewMemBackend()
	client, _, store := newTestServerWith(t, backend, prov)
	ctx := context.Background()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "grandfather-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	// An endpoint as it exists today: 2 cores / 4Gi, a quarter of the floor.
	ep, err := backend.CreateEndpoint(ctx, clusterID, branchID, core.RoleReadWrite, nil)
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if err := backend.UpdateEndpointResources(ctx, ep.ID, "2", "4Gi"); err != nil {
		t.Fatalf("seed the pre-floor size: %v", err)
	}

	// Shrinking cpu raises memory-per-core: allowed, though still under the floor.
	shrink := "1"
	ok, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.ID, api.ResizeEndpointRequest{
		Resources: api.EndpointResources{Cpu: &shrink},
	})
	if err != nil {
		t.Fatalf("ResizeEndpoint(shrink): %v", err)
	}
	if ok.StatusCode() != http.StatusAccepted {
		t.Fatalf("shrinking a below-floor endpoint = %d, want 202 — it moves towards the floor; body = %s", ok.StatusCode(), ok.Body)
	}
	drainTasks(t, store, backend, prov)

	// Growing cpu lowers it further: refused.
	grow := "8"
	bad, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.ID, api.ResizeEndpointRequest{
		Resources: api.EndpointResources{Cpu: &grow},
	})
	if err != nil {
		t.Fatalf("ResizeEndpoint(grow): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest {
		t.Errorf("growing cpu on a below-floor endpoint = %d, want 400; body = %s", bad.StatusCode(), bad.Body)
	}
}

// An endpoint created before resources were tracked at all stores "" for both,
// not "2"/"4Gi" — `core/pg.go` COALESCEs to empty and migration 0009 leaves an
// empty memory alone as the "unset" marker. Those rows are the oldest ones, and
// the grandfathering exists for them most of all.
//
// Reading the floor decision off the raw pair gets them exactly backwards: "" and
// "" resolve to the DEFAULT shape, which meets the floor, so the endpoint reports
// as healthy while the resize target carries the substituted 4Gi and does not —
// and every resize of it, including the shrink towards the floor, is refused.
func TestResizeGrandfathersAnEndpointWithNoStoredResources(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	backend := core.NewMemBackend()
	client, _, store := newTestServerWith(t, backend, prov)
	ctx := context.Background()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "unset-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	// Created and never sized: both fields empty, as the oldest rows are.
	ep, err := backend.CreateEndpoint(ctx, clusterID, branchID, core.RoleReadWrite, nil)
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	// Shrinking cpu raises the memory each core has. It is still under the floor,
	// and it is the move the grandfathering is for.
	shrink := "1"
	ok, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.ID, api.ResizeEndpointRequest{
		Resources: api.EndpointResources{Cpu: &shrink},
	})
	if err != nil {
		t.Fatalf("ResizeEndpoint(shrink): %v", err)
	}
	if ok.StatusCode() != http.StatusAccepted {
		t.Fatalf("shrinking an endpoint with no stored resources = %d, want 202; body = %s", ok.StatusCode(), ok.Body)
	}
	drainTasks(t, store, backend, prov)

	// And growing cpu, which lowers it further, is still refused.
	grow := "8"
	bad, err := client.ResizeEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.ID, api.ResizeEndpointRequest{
		Resources: api.EndpointResources{Cpu: &grow},
	})
	if err != nil {
		t.Fatalf("ResizeEndpoint(grow): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest {
		t.Errorf("growing cpu on a starved endpoint = %d, want 400; body = %s", bad.StatusCode(), bad.Body)
	}
}

// A create whose body arrives with no Content-Length — chunked transfer, what a
// streaming client or a re-chunking proxy sends — makes echo's binder return
// io.EOF, and the generated strict handler then leaves request.Body nil
// (api.gen.go CreateEndpoint). Reading request.Body.Connectivity without a nil
// guard panicked, which middleware.Recover turned into a 500 instead of the
// default-everything create. A zero-Content-Length body does NOT reproduce it:
// echo short-circuits that case and binds a zero-value struct.
func TestCreateEndpointChunkedEmptyBody(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	backend := core.NewMemBackend()
	client, _, base := newTestServerRaw(t, backend, prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "ep-chunked-pool")

	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ClusterID: clusterID,
		BranchID:  branchID,
		Name:      "main",
		PgVersion: 18,
		IsDefault: true,
	})

	pr, pw := io.Pipe()
	if err := pw.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	url := fmt.Sprintf("%s/storage-pools/%s/clusters/%s/branches/%s/endpoints", base, pool.Id, clusterID, branchID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1 // chunked: the trigger for the io.EOF bind path

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST endpoints: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// 400, not 202: a writer needs a password, and an empty body carries none.
	// What is under test is that echo BOUND the empty body instead of failing on
	// it — so the reply has to be the validation error, not a bind error.
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (the empty body binds, then fails validation), body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "password is required") {
		t.Errorf("body = %s, want the password validation error — anything else means the empty body did not bind", body)
	}
}
