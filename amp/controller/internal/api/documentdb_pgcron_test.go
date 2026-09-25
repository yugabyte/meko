package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// documentdb starts the compute with cron.database_name=documentdb, so adding it
// to a branch that already has pg_cron would move the scheduler and silently stop
// the jobs scheduled in postgres. addBranchExtensions refuses that with a 400 —
// permanent, so the YCP agent reports it rejected instead of retrying a 409 as
// in-flight work. A branch that has documentdb already, or no pg_cron, is not
// affected.
func TestAddDocumentdbToAPgCronBranchIsRefused(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep, Extensions: extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "documentdb-pgcron-pool")
	withWriter := false

	branchOf := func(name string, req api.CreateClusterRequest) (api.ClusterId, api.BranchId) {
		t.Helper()
		req.Name, req.WithWriter = name, &withWriter
		created, err := client.CreateClusterWithResponse(ctx, pool.Id, req)
		if err != nil || created.StatusCode() != http.StatusAccepted {
			t.Fatalf("create cluster %s: %v (%d) %s", name, err, created.StatusCode(), created.Body)
		}
		drainTasks(t, store, backend, prov)
		cl, err := client.GetClusterWithResponse(ctx, pool.Id, *created.JSON202.ClusterId)
		if err != nil || cl.JSON200 == nil {
			t.Fatalf("get cluster %s: %v", name, err)
		}
		return *created.JSON202.ClusterId, cl.JSON200.DefaultBranch.BranchId
	}
	add := func(cluster api.ClusterId, branch api.BranchId, names ...string) *api.AddBranchExtensionsResponse {
		t.Helper()
		res, err := client.AddBranchExtensionsWithResponse(ctx, pool.Id, cluster, branch,
			api.AddBranchExtensionsRequest{Extensions: names})
		if err != nil {
			t.Fatalf("addBranchExtensions %v: %v", names, err)
		}
		return res
	}

	cronCluster, cronBranch := branchOf("cron", api.CreateClusterRequest{Extensions: &[]string{"pg_cron"}})
	res := add(cronCluster, cronBranch, "documentdb")
	if res.StatusCode() != http.StatusBadRequest {
		t.Fatalf("documentdb onto a pg_cron branch: status %d, want 400; body %s", res.StatusCode(), res.Body)
	}
	if !strings.Contains(string(res.Body), "pg_cron") {
		t.Errorf("the 400 should say why (pg_cron); body %s", res.Body)
	}

	// Already documentdb: pg_cron lives in its database from the start, so a
	// re-add is the usual idempotent no-op.
	engine := api.CreateClusterRequestEngine("documentdb")
	ddCluster, ddBranch := branchOf("docs", api.CreateClusterRequest{Engine: &engine})
	if res := add(ddCluster, ddBranch, "documentdb"); res.StatusCode() != http.StatusAccepted {
		t.Errorf("re-adding documentdb to a documentdb branch: status %d, want 202; body %s", res.StatusCode(), res.Body)
	}

	// No pg_cron on the branch: nothing to move.
	vecCluster, vecBranch := branchOf("vec", api.CreateClusterRequest{Extensions: &[]string{"pgvector"}})
	if res := add(vecCluster, vecBranch, "documentdb"); res.StatusCode() != http.StatusAccepted {
		t.Errorf("documentdb onto a pgvector-only branch: status %d, want 202; body %s", res.StatusCode(), res.Body)
	}
}
