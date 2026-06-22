// Tests for the paginated list endpoints (users, storage pools, timelines,
// endpoints): the limit/offset contract, the total (all matches) vs count
// (this page) fields, and parent 404s on the nested lists. They drive the real
// controllers + generated client over MemBackend, like the other api tests.
package api_test

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-amp/controller/internal/api"
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

func TestListTimelines(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	other := mustCreatePool(t, client, u.Id, "other")

	// Unknown pool -> 404.
	miss, err := client.ListTimelinesWithResponse(ctx, uuid.New(), nil)
	if err != nil {
		t.Fatalf("ListTimelines(missing pool): %v", err)
	}
	if miss.StatusCode() != http.StatusNotFound || miss.JSON404 == nil {
		t.Fatalf("missing pool status = %d, body = %s", miss.StatusCode(), miss.Body)
	}

	// Empty pool -> 200 with total 0.
	empty, err := client.ListTimelinesWithResponse(ctx, pool.Id, nil)
	if err != nil {
		t.Fatalf("ListTimelines(empty): %v", err)
	}
	if empty.StatusCode() != http.StatusOK || empty.JSON200 == nil {
		t.Fatalf("empty list status = %d, body = %s", empty.StatusCode(), empty.Body)
	}
	if empty.JSON200.Total != 0 || empty.JSON200.Count != 0 {
		t.Errorf("empty pool = {total:%d count:%d}, want {0 0}", empty.JSON200.Total, empty.JSON200.Count)
	}

	// Create three timelines in pool, one in the other pool.
	want := []string{"a", "b", "c"}
	for _, name := range want {
		mustCreateTimeline(t, client, pool.Id, name)
	}
	mustCreateTimeline(t, client, other.Id, "z")

	// The pool lists exactly its own three, scoped away from the other pool.
	got, err := client.ListTimelinesWithResponse(ctx, pool.Id, nil)
	if err != nil {
		t.Fatalf("ListTimelines: %v", err)
	}
	if got.StatusCode() != http.StatusOK || got.JSON200 == nil {
		t.Fatalf("list status = %d, body = %s", got.StatusCode(), got.Body)
	}
	if got.JSON200.Total != 3 || got.JSON200.Count != 3 {
		t.Errorf("list = {total:%d count:%d}, want {3 3}", got.JSON200.Total, got.JSON200.Count)
	}
	gotNames := make([]string, 0, len(got.JSON200.Timelines))
	for _, tl := range got.JSON200.Timelines {
		gotNames = append(gotNames, tl.Name)
	}
	sort.Strings(gotNames)
	if !equalStrings(gotNames, want) {
		t.Errorf("timeline names = %v, want %v", gotNames, want)
	}
}

func TestListEndpoints(t *testing.T) {
	dep := &fakeDeployer{addr: "127.0.0.1:5555"}
	client, _ := newTestServerWithProv(t, api.Provisioning{Deployer: dep})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	// Create-timeline records the tenant + main timeline only; compute is a
	// separate resource. The first endpoint is deployed by POSTing it.
	ct, err := client.CreateTimelineWithResponse(ctx, pool.Id, api.CreateTimelineRequest{Name: "main"})
	if err != nil {
		t.Fatalf("CreateTimeline: %v", err)
	}
	if ct.StatusCode() != http.StatusCreated || ct.JSON201 == nil {
		t.Fatalf("CreateTimeline status = %d, body = %s", ct.StatusCode(), ct.Body)
	}
	tl := *ct.JSON201

	// Deploy a read_write endpoint on the timeline (the deployer is set).
	rw := api.CreateEndpointRequestRoleReadWrite
	cep, err := client.CreateEndpointWithResponse(ctx, pool.Id, tl.TimelineId, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if cep.StatusCode() != http.StatusCreated || cep.JSON201 == nil {
		t.Fatalf("CreateEndpoint status = %d, body = %s", cep.StatusCode(), cep.Body)
	}

	eps, err := client.ListEndpointsWithResponse(ctx, pool.Id, tl.TimelineId, nil)
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

	// Unknown timeline under a real pool -> 404.
	missTl, err := client.ListEndpointsWithResponse(ctx, pool.Id, uuid.New(), nil)
	if err != nil {
		t.Fatalf("ListEndpoints(missing timeline): %v", err)
	}
	if missTl.StatusCode() != http.StatusNotFound {
		t.Errorf("missing timeline status = %d, want 404", missTl.StatusCode())
	}

	// Unknown pool -> 404.
	missPool, err := client.ListEndpointsWithResponse(ctx, uuid.New(), tl.TimelineId, nil)
	if err != nil {
		t.Fatalf("ListEndpoints(missing pool): %v", err)
	}
	if missPool.StatusCode() != http.StatusNotFound {
		t.Errorf("missing pool status = %d, want 404", missPool.StatusCode())
	}

	// A real but *different* pool must not see another pool's timeline endpoints:
	// the timeline is resolved within the addressed pool, so this 404s (no leak).
	poolB := mustCreatePool(t, client, u.Id, "poolB")
	crossPool, err := client.ListEndpointsWithResponse(ctx, poolB.Id, tl.TimelineId, nil)
	if err != nil {
		t.Fatalf("ListEndpoints(cross pool): %v", err)
	}
	if crossPool.StatusCode() != http.StatusNotFound {
		t.Errorf("cross-pool endpoint list status = %d, want 404 (timeline not in poolB)", crossPool.StatusCode())
	}
}

func TestListEndpointsEmpty(t *testing.T) {
	client, _, _ := newTestServer(t) // no deployer -> no endpoint provisioned
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	tl := mustCreateTimeline(t, client, pool.Id, "main")

	eps, err := client.ListEndpointsWithResponse(ctx, pool.Id, tl.TimelineId, nil)
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

func mustCreateTimeline(t *testing.T, client *api.ClientWithResponses, poolID uuid.UUID, name string) api.Timeline {
	t.Helper()
	resp, err := client.CreateTimelineWithResponse(context.Background(), poolID, api.CreateTimelineRequest{Name: name})
	if err != nil {
		t.Fatalf("CreateTimeline(%q): %v", name, err)
	}
	if resp.StatusCode() != http.StatusCreated || resp.JSON201 == nil {
		t.Fatalf("CreateTimeline(%q) status = %d, body = %s", name, resp.StatusCode(), resp.Body)
	}
	return *resp.JSON201
}
