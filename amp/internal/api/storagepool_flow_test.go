package api_test

import (
	"context"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-light/amp/internal/api"
)

// TestStoragePoolFlow runs the storage-pool workflow a user drives through the
// API: create pools, list them, fetch one — and confirms ownership scoping
// (no sharing, #92): a user sees only their own pools, and cannot fetch
// another user's pool.
func TestStoragePoolFlow(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()

	alice := mustCreateUser(t, client, false)

	// A fresh user owns no pools.
	if names := listPoolNames(t, client, alice.Id); len(names) != 0 {
		t.Fatalf("new user pools = %v, want none", names)
	}

	// Create two pools as Alice.
	alpha := mustCreatePool(t, client, alice.Id, "alpha")
	mustCreatePool(t, client, alice.Id, "beta")

	// List returns exactly Alice's two pools.
	if got, want := listPoolNames(t, client, alice.Id), []string{"alpha", "beta"}; !equalStrings(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}

	// Fetch one pool by id.
	gp, err := client.GetStoragePoolWithResponse(ctx, alice.Id, alpha.Id)
	if err != nil {
		t.Fatalf("GetStoragePool: %v", err)
	}
	if gp.StatusCode() != http.StatusOK || gp.JSON200 == nil {
		t.Fatalf("GetStoragePool status = %d, body = %s", gp.StatusCode(), gp.Body)
	}
	if gp.JSON200.Name != "alpha" || gp.JSON200.OwnerUserId != alice.Id {
		t.Errorf("got {name:%q owner:%s}, want {alpha %s}", gp.JSON200.Name, gp.JSON200.OwnerUserId, alice.Id)
	}

	// A random pool id is a 404.
	gm, err := client.GetStoragePoolWithResponse(ctx, alice.Id, uuid.New())
	if err != nil {
		t.Fatalf("GetStoragePool(missing): %v", err)
	}
	if gm.StatusCode() != http.StatusNotFound {
		t.Fatalf("GetStoragePool(missing) status = %d, body = %s", gm.StatusCode(), gm.Body)
	}

	// Pool names are unique across the placement group.
	dup, err := client.CreateStoragePoolWithResponse(ctx, alice.Id, api.CreateStoragePoolRequest{Name: "alpha"})
	if err != nil {
		t.Fatalf("CreateStoragePool(dup): %v", err)
	}
	if dup.StatusCode() != http.StatusConflict {
		t.Fatalf("dup status = %d, body = %s", dup.StatusCode(), dup.Body)
	}

	// A second user's pool is invisible to Alice, and vice versa (no sharing).
	bob := mustCreateUser(t, client, false)
	gamma := mustCreatePool(t, client, bob.Id, "gamma")

	if names := listPoolNames(t, client, alice.Id); !equalStrings(names, []string{"alpha", "beta"}) {
		t.Errorf("alice list after bob's pool = %v, want [alpha beta]", names)
	}
	if names := listPoolNames(t, client, bob.Id); !equalStrings(names, []string{"gamma"}) {
		t.Errorf("bob list = %v, want [gamma]", names)
	}

	// Alice cannot fetch Bob's pool — owner-scoped, so it 404s rather than leaks.
	cross, err := client.GetStoragePoolWithResponse(ctx, alice.Id, gamma.Id)
	if err != nil {
		t.Fatalf("GetStoragePool(cross): %v", err)
	}
	if cross.StatusCode() != http.StatusNotFound {
		t.Fatalf("cross-user get status = %d, want 404", cross.StatusCode())
	}
}

func mustCreatePool(t *testing.T, client *api.ClientWithResponses, owner uuid.UUID, name string) api.StoragePool {
	t.Helper()
	resp, err := client.CreateStoragePoolWithResponse(context.Background(), owner, api.CreateStoragePoolRequest{Name: name})
	if err != nil {
		t.Fatalf("CreateStoragePool(%q): %v", name, err)
	}
	if resp.StatusCode() != http.StatusCreated || resp.JSON201 == nil {
		t.Fatalf("CreateStoragePool(%q) status = %d, body = %s", name, resp.StatusCode(), resp.Body)
	}
	return *resp.JSON201
}

func listPoolNames(t *testing.T, client *api.ClientWithResponses, owner uuid.UUID) []string {
	t.Helper()
	resp, err := client.ListStoragePoolsWithResponse(context.Background(), owner)
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
