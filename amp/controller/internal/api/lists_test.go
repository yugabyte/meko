// Tests for the paginated list endpoints (users, storage pools, clusters,
// branches, endpoints): the limit/offset contract, the total (all matches) vs
// count (this page) fields, and parent 404s on the nested lists. They drive the
// real controllers + generated client over MemBackend, like the other api tests.
package api_test

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/task"
)

func TestListUsersPagination(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()

	const n = 5
	created := make(map[uuid.UUID]bool, n)
	for range n {
		created[mustCreateUser(t, client, false).Id] = true
	}

	// Default list: total and count both n, all ids present.
	all, err := client.ListUsersWithResponse(ctx, nil)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if all.StatusCode() != http.StatusOK || all.JSON200 == nil {
		t.Fatalf("ListUsers status = %d, body = %s", all.StatusCode(), all.Body)
	}
	if all.JSON200.Total != n {
		t.Errorf("total = %d, want %d", all.JSON200.Total, n)
	}
	if all.JSON200.Count != n || len(all.JSON200.Users) != n {
		t.Errorf("count = %d, len = %d, want %d", all.JSON200.Count, len(all.JSON200.Users), n)
	}

	// Items come back ordered by (created_at, id) — the order both backends
	// promise (mem's createdBefore mirrors pg's ORDER BY created_at, id).
	for i := 1; i < len(all.JSON200.Users); i++ {
		a, b := all.JSON200.Users[i-1], all.JSON200.Users[i]
		if a.CreatedAt.After(b.CreatedAt) ||
			(a.CreatedAt.Equal(b.CreatedAt) && a.Id.String() > b.Id.String()) {
			t.Errorf("users not ordered by (created_at, id) at index %d: %s then %s", i, a.Id, b.Id)
		}
	}

	// Page through with limit=2: total stays n on every page, count matches the
	// page length, and the pages cover the whole set exactly once.
	limit := 2
	seen := map[uuid.UUID]bool{}
	for offset := 0; offset < n; offset += limit {
		off := offset
		page, err := client.ListUsersWithResponse(ctx, &api.ListUsersParams{Limit: &limit, Offset: &off})
		if err != nil {
			t.Fatalf("ListUsers(offset=%d): %v", off, err)
		}
		if page.JSON200.Total != n {
			t.Errorf("offset=%d total = %d, want %d", off, page.JSON200.Total, n)
		}
		if page.JSON200.Count != len(page.JSON200.Users) {
			t.Errorf("offset=%d count = %d, len = %d (must match)", off, page.JSON200.Count, len(page.JSON200.Users))
		}
		if page.JSON200.Count > limit {
			t.Errorf("offset=%d returned %d items, want <= limit %d", off, page.JSON200.Count, limit)
		}
		for _, u := range page.JSON200.Users {
			if seen[u.Id] {
				t.Errorf("user %s returned on more than one page", u.Id)
			}
			seen[u.Id] = true
		}
	}
	if len(seen) != n {
		t.Errorf("paged users = %d, want %d", len(seen), n)
	}

	// Offset past the end is an empty page, but total still reports the full count.
	beyond := n + 10
	tail, err := client.ListUsersWithResponse(ctx, &api.ListUsersParams{Offset: &beyond})
	if err != nil {
		t.Fatalf("ListUsers(beyond): %v", err)
	}
	if tail.JSON200.Total != n || tail.JSON200.Count != 0 || len(tail.JSON200.Users) != 0 {
		t.Errorf("beyond-end page = {total:%d count:%d len:%d}, want {%d 0 0}",
			tail.JSON200.Total, tail.JSON200.Count, len(tail.JSON200.Users), n)
	}
}

func TestListStoragePoolsPaginationClamps(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	for i := range 3 {
		mustCreatePool(t, client, u.Id, fmt.Sprintf("p%d", i))
	}

	// limit=0 and a negative offset are clamped (limit->1, offset->0), not 400.
	zero, neg := 0, -5
	resp, err := client.ListStoragePoolsWithResponse(ctx, &api.ListStoragePoolsParams{Limit: &zero, Offset: &neg})
	if err != nil {
		t.Fatalf("ListStoragePools: %v", err)
	}
	if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
		t.Fatalf("status = %d (clamped pagination must not 400), body = %s", resp.StatusCode(), resp.Body)
	}
	if resp.JSON200.Total != 3 {
		t.Errorf("total = %d, want 3 (full match, ignores limit)", resp.JSON200.Total)
	}
	if resp.JSON200.Count != 1 {
		t.Errorf("count = %d, want 1 (limit clamped up to 1)", resp.JSON200.Count)
	}
}

func TestListClusters(t *testing.T) {
	client, backend, store := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	other := mustCreatePool(t, client, u.Id, "other")

	// Unknown pool -> 404.
	miss, err := client.ListClustersWithResponse(ctx, uuid.New(), nil)
	if err != nil {
		t.Fatalf("ListClusters(missing pool): %v", err)
	}
	if miss.StatusCode() != http.StatusNotFound || miss.JSON404 == nil {
		t.Fatalf("missing pool status = %d, body = %s", miss.StatusCode(), miss.Body)
	}

	// Empty pool -> 200 with total 0.
	empty, err := client.ListClustersWithResponse(ctx, pool.Id, nil)
	if err != nil {
		t.Fatalf("ListClusters(empty): %v", err)
	}
	if empty.StatusCode() != http.StatusOK || empty.JSON200 == nil {
		t.Fatalf("empty list status = %d, body = %s", empty.StatusCode(), empty.Body)
	}
	if empty.JSON200.Total != 0 || empty.JSON200.Count != 0 {
		t.Errorf("empty pool = {total:%d count:%d}, want {0 0}", empty.JSON200.Total, empty.JSON200.Count)
	}

	// Create three clusters in pool, one in the other pool.
	want := []string{"a", "b", "c"}
	for _, name := range want {
		mustCreateCluster(t, client, store, backend, pool.Id, name)
	}
	mustCreateCluster(t, client, store, backend, other.Id, "z")

	// The pool lists exactly its own three, scoped away from the other pool.
	got, err := client.ListClustersWithResponse(ctx, pool.Id, nil)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if got.StatusCode() != http.StatusOK || got.JSON200 == nil {
		t.Fatalf("list status = %d, body = %s", got.StatusCode(), got.Body)
	}
	if got.JSON200.Total != 3 || got.JSON200.Count != 3 {
		t.Errorf("list = {total:%d count:%d}, want {3 3}", got.JSON200.Total, got.JSON200.Count)
	}
	gotNames := make([]string, 0, len(got.JSON200.Clusters))
	for _, p := range got.JSON200.Clusters {
		gotNames = append(gotNames, p.Name)
		// Every cluster carries its auto-created default branch.
		if !p.DefaultBranch.IsDefault {
			t.Errorf("cluster %q default branch is_default = false", p.Name)
		}
	}
	sort.Strings(gotNames)
	if !equalStrings(gotNames, want) {
		t.Errorf("cluster names = %v, want %v", gotNames, want)
	}
}

func TestListBranches(t *testing.T) {
	client, backend, store := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	cluster := mustCreateCluster(t, client, store, backend, pool.Id, "main")

	// A fresh cluster lists exactly its default branch.
	only, err := client.ListBranchesWithResponse(ctx, pool.Id, cluster.Id, nil)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if only.StatusCode() != http.StatusOK || only.JSON200 == nil || only.JSON200.Total != 1 {
		t.Fatalf("fresh cluster branches = %d (status %d), want 1", only.JSON200.Total, only.StatusCode())
	}

	// Fork a branch off the default with an explicit fork point (no storage
	// controller is wired here, so the LSN must be explicit) -> 202 + a branch
	// task; drive it, then the branch row is materialized.
	lsn := "0/16ABCD0"
	br, err := client.CreateBranchWithResponse(ctx, pool.Id, cluster.Id, api.CreateBranchRequest{Name: "feature", ForkPointLsn: &lsn})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if br.StatusCode() != http.StatusAccepted || br.JSON202 == nil {
		t.Fatalf("CreateBranch status = %d, body = %s", br.StatusCode(), br.Body)
	}
	if br.JSON202.Kind != "branch" {
		t.Errorf("task kind = %q, want branch", br.JSON202.Kind)
	}
	drainTasks(t, store, backend, api.Provisioning{Deployer: &fakeDeployer{}, Recipes: &fakeRecipeStore{backend: backend}})

	// Now the cluster lists both branches; the fork carries parent + is_default=false.
	both, err := client.ListBranchesWithResponse(ctx, pool.Id, cluster.Id, nil)
	if err != nil {
		t.Fatalf("ListBranches(after fork): %v", err)
	}
	if both.JSON200 == nil || both.JSON200.Total != 2 {
		t.Errorf("branches after fork = %d, want 2", both.JSON200.Total)
	}
	var feature *api.Branch
	for i := range both.JSON200.Branches {
		if both.JSON200.Branches[i].Name == "feature" {
			feature = &both.JSON200.Branches[i]
		}
	}
	if feature == nil {
		t.Fatal("forked branch 'feature' not listed after drive")
	}
	if feature.IsDefault {
		t.Error("forked branch is_default = true, want false")
	}
	if feature.ParentBranchId == nil || *feature.ParentBranchId != cluster.DefaultBranch.BranchId {
		t.Errorf("parent_branch_id = %v, want the default branch %v", feature.ParentBranchId, cluster.DefaultBranch.BranchId)
	}

	// Unknown cluster under a real pool -> 404.
	missing, err := client.ListBranchesWithResponse(ctx, pool.Id, uuid.New(), nil)
	if err != nil {
		t.Fatalf("ListBranches(missing cluster): %v", err)
	}
	if missing.StatusCode() != http.StatusNotFound {
		t.Errorf("missing cluster status = %d, want 404", missing.StatusCode())
	}
}

func TestListEndpoints(t *testing.T) {
	dep := &fakeDeployer{addr: "127.0.0.1:5555"}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	// Create-cluster records the cluster + default branch only; compute is a
	// separate resource. The first endpoint is deployed by POSTing it.
	cluster := mustCreateCluster(t, client, store, backend, pool.Id, "main")
	branchID := cluster.DefaultBranch.BranchId

	// Deploy a read_write endpoint on the default branch (the deployer is set) ->
	// 202; drive the create_endpoint task, then it shows up in the list with its
	// deployed address.
	rw := api.CreateEndpointRequestRoleReadWrite
	cep, err := client.CreateEndpointWithResponse(ctx, pool.Id, cluster.Id, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if cep.StatusCode() != http.StatusAccepted || cep.JSON202 == nil {
		t.Fatalf("CreateEndpoint status = %d, body = %s", cep.StatusCode(), cep.Body)
	}
	drainTasks(t, store, backend, prov)

	eps, err := client.ListEndpointsWithResponse(ctx, pool.Id, cluster.Id, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if eps.StatusCode() != http.StatusOK || eps.JSON200 == nil {
		t.Fatalf("ListEndpoints status = %d, body = %s", eps.StatusCode(), eps.Body)
	}
	if eps.JSON200.Total != 1 || eps.JSON200.Count != 1 {
		t.Fatalf("endpoints = {total:%d count:%d}, want {1 1}", eps.JSON200.Total, eps.JSON200.Count)
	}
	if got := eps.JSON200.Endpoints[0]; got.Role != api.EndpointRoleReadWrite || got.Address != "127.0.0.1:5555" {
		t.Errorf("endpoint = {role:%q addr:%q}, want {read_write 127.0.0.1:5555}", got.Role, got.Address)
	}

	// Unknown branch under a real pool/cluster -> 404.
	missBr, err := client.ListEndpointsWithResponse(ctx, pool.Id, cluster.Id, uuid.New(), nil)
	if err != nil {
		t.Fatalf("ListEndpoints(missing branch): %v", err)
	}
	if missBr.StatusCode() != http.StatusNotFound {
		t.Errorf("missing branch status = %d, want 404", missBr.StatusCode())
	}

	// Unknown pool -> 404.
	missPool, err := client.ListEndpointsWithResponse(ctx, uuid.New(), cluster.Id, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints(missing pool): %v", err)
	}
	if missPool.StatusCode() != http.StatusNotFound {
		t.Errorf("missing pool status = %d, want 404", missPool.StatusCode())
	}

	// A real but *different* pool must not see another pool's branch endpoints:
	// the branch is resolved within the addressed pool/cluster, so this 404s.
	poolB := mustCreatePool(t, client, u.Id, "poolB")
	crossPool, err := client.ListEndpointsWithResponse(ctx, poolB.Id, cluster.Id, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints(cross pool): %v", err)
	}
	if crossPool.StatusCode() != http.StatusNotFound {
		t.Errorf("cross-pool endpoint list status = %d, want 404 (cluster not in poolB)", crossPool.StatusCode())
	}
}

func TestListEndpointsEmpty(t *testing.T) {
	client, backend, store := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	cluster := mustCreateCluster(t, client, store, backend, pool.Id, "main")
	branchID := cluster.DefaultBranch.BranchId

	eps, err := client.ListEndpointsWithResponse(ctx, pool.Id, cluster.Id, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if eps.StatusCode() != http.StatusOK || eps.JSON200 == nil {
		t.Fatalf("ListEndpoints status = %d, body = %s", eps.StatusCode(), eps.Body)
	}
	if eps.JSON200.Total != 0 || eps.JSON200.Count != 0 || len(eps.JSON200.Endpoints) != 0 {
		t.Errorf("endpoints = {total:%d count:%d len:%d}, want all 0",
			eps.JSON200.Total, eps.JSON200.Count, len(eps.JSON200.Endpoints))
	}
}

// mustCreateCluster creates a cluster through the client, drives the resulting
// create_cluster task to completion against the shared store/backend (so the
// cluster + its default branch are materialized), and returns the cluster read
// back via GET. The drive uses a deployer-enabled provisioning, which is
// sufficient for create_cluster (it plans no endpoint step). Tests whose endpoint
// creates need a specific deployer drive those separately.
func mustCreateCluster(t *testing.T, client *api.ClientWithResponses, store *task.MemStore, backend core.Backend, poolID uuid.UUID, name string) api.Cluster {
	t.Helper()
	resp, err := client.CreateClusterWithResponse(context.Background(), poolID, api.CreateClusterRequest{Name: name})
	if err != nil {
		t.Fatalf("CreateCluster(%q): %v", name, err)
	}
	if resp.StatusCode() != http.StatusAccepted || resp.JSON202 == nil {
		t.Fatalf("CreateCluster(%q) status = %d, body = %s", name, resp.StatusCode(), resp.Body)
	}
	clusterID := *resp.JSON202.ClusterId
	drainTasks(t, store, backend, api.Provisioning{Deployer: &fakeDeployer{}})
	gp, err := client.GetClusterWithResponse(context.Background(), poolID, clusterID)
	if err != nil {
		t.Fatalf("GetCluster(%q): %v", name, err)
	}
	if gp.StatusCode() != http.StatusOK || gp.JSON200 == nil {
		t.Fatalf("GetCluster(%q) status = %d, body = %s", name, gp.StatusCode(), gp.Body)
	}
	return *gp.JSON200
}

// recordingBackend wraps MemBackend to capture the includeFailed flag the
// handler passes to ListClusters. MemBackend itself ignores the flag (it has no
// failed-creation orphans), so this is how we assert the handler's default
// (param omitted -> false, i.e. usable clusters only) and its opt-in wiring —
// the logic the DB-backed pg_clusters_test can't reach through the HTTP layer.
type recordingBackend struct {
	*core.MemBackend
	mu                sync.Mutex
	lastIncludeFailed bool
}

func (r *recordingBackend) ListClusters(ctx context.Context, poolID uuid.UUID, limit, offset int, includeFailed bool) ([]core.Cluster, int, error) {
	r.mu.Lock()
	r.lastIncludeFailed = includeFailed
	r.mu.Unlock()
	return r.MemBackend.ListClusters(ctx, poolID, limit, offset, includeFailed)
}

func (r *recordingBackend) got() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastIncludeFailed
}

// TestListClustersIncludeFailedParam pins the handler's include_failed handling:
// omitted defaults to false (scope to usable clusters), and an explicit value is
// passed through verbatim.
func TestListClustersIncludeFailedParam(t *testing.T) {
	rec := &recordingBackend{MemBackend: core.NewMemBackend()}
	client, _ := newTestServerBackend(t, rec, api.Provisioning{})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	f, tr := false, true
	for _, tt := range []struct {
		name  string
		param *api.ListClustersParams
		want  bool
	}{
		{"omitted defaults to false", &api.ListClustersParams{}, false},
		{"explicit false", &api.ListClustersParams{IncludeFailed: &f}, false},
		{"explicit true", &api.ListClustersParams{IncludeFailed: &tr}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := client.ListClustersWithResponse(ctx, pool.Id, tt.param)
			if err != nil || resp.StatusCode() != http.StatusOK {
				t.Fatalf("ListClusters: status=%d err=%v", resp.StatusCode(), err)
			}
			if got := rec.got(); got != tt.want {
				t.Errorf("handler passed includeFailed=%v, want %v", got, tt.want)
			}
		})
	}
}
