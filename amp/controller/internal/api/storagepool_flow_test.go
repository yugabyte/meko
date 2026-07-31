package api_test

import (
	"context"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
)

// TestStoragePoolFlow runs the storage-pool workflow over the *top-level* pool
// API: pools are addressed by their own id (not under a user), listing returns
// every pool in the placement group, and any pool can be fetched by id
// regardless of who created it (no owner scoping — a pool will eventually be
// acted on by more than its creator). Pool names stay globally unique.
func TestStoragePoolFlow(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()

	alice := mustCreateUser(t, client, false)
	bob := mustCreateUser(t, client, false)

	// No pools yet.
	if names := listPoolNames(t, client); len(names) != 0 {
		t.Fatalf("initial pools = %v, want none", names)
	}

	// Create pools owned by different users.
	alpha := mustCreatePool(t, client, alice.Id, "alpha")
	mustCreatePool(t, client, alice.Id, "beta")
	gamma := mustCreatePool(t, client, bob.Id, "gamma")

	// List is top-level: it returns every pool, not just one user's.
	if got, want := listPoolNames(t, client), []string{"alpha", "beta", "gamma"}; !equalStrings(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}

	// Fetch a pool by id; owner_user_id reflects its creator.
	gp, err := client.GetStoragePoolWithResponse(ctx, alpha.Id)
	if err != nil {
		t.Fatalf("GetStoragePool: %v", err)
	}
	if gp.StatusCode() != http.StatusOK || gp.JSON200 == nil {
		t.Fatalf("GetStoragePool status = %d, body = %s", gp.StatusCode(), gp.Body)
	}
	if gp.JSON200.Name != "alpha" || gp.JSON200.OwnerUserId != alice.Id {
		t.Errorf("got {name:%q owner:%s}, want {alpha %s}", gp.JSON200.Name, gp.JSON200.OwnerUserId, alice.Id)
	}

	// Bob's pool is fetchable by anyone now that pools are top-level (no scoping).
	cross, err := client.GetStoragePoolWithResponse(ctx, gamma.Id)
	if err != nil {
		t.Fatalf("GetStoragePool(gamma): %v", err)
	}
	if cross.StatusCode() != http.StatusOK || cross.JSON200 == nil {
		t.Fatalf("GetStoragePool(gamma) status = %d, want 200, body = %s", cross.StatusCode(), cross.Body)
	}
	if cross.JSON200.OwnerUserId != bob.Id {
		t.Errorf("gamma owner = %s, want %s", cross.JSON200.OwnerUserId, bob.Id)
	}

	// A random pool id is a 404.
	gm, err := client.GetStoragePoolWithResponse(ctx, uuid.New())
	if err != nil {
		t.Fatalf("GetStoragePool(missing): %v", err)
	}
	if gm.StatusCode() != http.StatusNotFound {
		t.Fatalf("GetStoragePool(missing) status = %d, body = %s", gm.StatusCode(), gm.Body)
	}

	// Pool names are unique across the placement group, regardless of owner.
	dup, err := client.CreateStoragePoolWithResponse(ctx, api.CreateStoragePoolRequest{Name: "alpha", OwnerUserId: bob.Id})
	if err != nil {
		t.Fatalf("CreateStoragePool(dup): %v", err)
	}
	if dup.StatusCode() != http.StatusConflict {
		t.Fatalf("dup status = %d, body = %s", dup.StatusCode(), dup.Body)
	}
}

func mustCreatePool(t *testing.T, client *api.ClientWithResponses, owner uuid.UUID, name string) api.StoragePool {
	t.Helper()
	resp, err := client.CreateStoragePoolWithResponse(context.Background(), api.CreateStoragePoolRequest{Name: name, OwnerUserId: owner})
	if err != nil {
		t.Fatalf("CreateStoragePool(%q): %v", name, err)
	}
	if resp.StatusCode() != http.StatusCreated || resp.JSON201 == nil {
		t.Fatalf("CreateStoragePool(%q) status = %d, body = %s", name, resp.StatusCode(), resp.Body)
	}
	return *resp.JSON201
}

// listPoolNames returns the names of every pool the list endpoint reports
// (default pagination), sorted for stable comparison.
func listPoolNames(t *testing.T, client *api.ClientWithResponses) []string {
	t.Helper()
	resp, err := client.ListStoragePoolsWithResponse(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListStoragePools: %v", err)
	}
	if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
		t.Fatalf("ListStoragePools status = %d, body = %s", resp.StatusCode(), resp.Body)
	}
	names := make([]string, 0, len(resp.JSON200.StoragePools))
	for _, p := range resp.JSON200.StoragePools {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
