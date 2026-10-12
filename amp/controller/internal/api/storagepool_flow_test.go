package api_test

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
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

// TestStoragePoolRename covers the rename contract: any pool at any time, the
// new name visible immediately, the old one released, and a live pool's name
// still refused.
func TestStoragePoolRename(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()

	alice := mustCreateUser(t, client, false)
	alpha := mustCreatePool(t, client, alice.Id, "alpha")
	mustCreatePool(t, client, alice.Id, "beta")

	renamed, err := client.UpdateStoragePoolWithResponse(ctx, alpha.Id, api.UpdateStoragePoolRequest{Name: "alpha-renamed"})
	if err != nil {
		t.Fatalf("UpdateStoragePool: %v", err)
	}
	if renamed.StatusCode() != http.StatusOK || renamed.JSON200 == nil {
		t.Fatalf("UpdateStoragePool status = %d, body = %s", renamed.StatusCode(), renamed.Body)
	}
	if renamed.JSON200.Name != "alpha-renamed" {
		t.Errorf("name = %q, want alpha-renamed", renamed.JSON200.Name)
	}
	// Rename is in place: same id, same owner, same creation time.
	if renamed.JSON200.Id != alpha.Id || renamed.JSON200.OwnerUserId != alice.Id {
		t.Errorf("got {id:%s owner:%s}, want {%s %s}",
			renamed.JSON200.Id, renamed.JSON200.OwnerUserId, alpha.Id, alice.Id)
	}
	if got, want := listPoolNames(t, client), []string{"alpha-renamed", "beta"}; !equalStrings(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}

	// The vacated name is free again.
	mustCreatePool(t, client, alice.Id, "alpha")

	// A name a live pool holds is still refused.
	clash, err := client.UpdateStoragePoolWithResponse(ctx, alpha.Id, api.UpdateStoragePoolRequest{Name: "beta"})
	if err != nil {
		t.Fatalf("UpdateStoragePool(clash): %v", err)
	}
	if clash.StatusCode() != http.StatusConflict {
		t.Fatalf("clash status = %d, want 409, body = %s", clash.StatusCode(), clash.Body)
	}

	// Renaming a pool to the name it already has is a no-op, not a self-conflict.
	same, err := client.UpdateStoragePoolWithResponse(ctx, alpha.Id, api.UpdateStoragePoolRequest{Name: "alpha-renamed"})
	if err != nil {
		t.Fatalf("UpdateStoragePool(same): %v", err)
	}
	if same.StatusCode() != http.StatusOK {
		t.Fatalf("same-name status = %d, want 200, body = %s", same.StatusCode(), same.Body)
	}

	for _, tc := range []struct {
		name string
		body api.UpdateStoragePoolRequest
		id   uuid.UUID
		want int
	}{
		{"blank name", api.UpdateStoragePoolRequest{Name: "   "}, alpha.Id, http.StatusBadRequest},
		{"missing pool", api.UpdateStoragePoolRequest{Name: "whatever"}, uuid.New(), http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.UpdateStoragePoolWithResponse(ctx, tc.id, tc.body)
			if err != nil {
				t.Fatalf("UpdateStoragePool: %v", err)
			}
			if resp.StatusCode() != tc.want {
				t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode(), tc.want, resp.Body)
			}
		})
	}
}

// TestStoragePoolDelete covers the delete contract: an empty pool goes away and
// releases its name, a pool with a live cluster is refused, and a pool whose
// clusters have all been deleted is empty enough to go — the case that leaves
// tombstoned cluster rows still pointing at the pool (#1083).
func TestStoragePoolDelete(t *testing.T) {
	client, backend, store := newTestServer(t)
	ctx := context.Background()

	alice := mustCreateUser(t, client, false)
	empty := mustCreatePool(t, client, alice.Id, "empty")
	occupied := mustCreatePool(t, client, alice.Id, "occupied")
	cluster := mustCreateCluster(t, client, store, backend, occupied.Id, "db")

	// An empty pool deletes, disappears from the list, and 404s on get.
	del, err := client.DeleteStoragePoolWithResponse(ctx, empty.Id)
	if err != nil {
		t.Fatalf("DeleteStoragePool: %v", err)
	}
	if del.StatusCode() != http.StatusNoContent {
		t.Fatalf("DeleteStoragePool status = %d, want 204, body = %s", del.StatusCode(), del.Body)
	}
	if got, want := listPoolNames(t, client), []string{"occupied"}; !equalStrings(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	gone, err := client.GetStoragePoolWithResponse(ctx, empty.Id)
	if err != nil {
		t.Fatalf("GetStoragePool(deleted): %v", err)
	}
	if gone.StatusCode() != http.StatusNotFound {
		t.Fatalf("GetStoragePool(deleted) status = %d, want 404, body = %s", gone.StatusCode(), gone.Body)
	}

	// The name it held is immediately reusable.
	mustCreatePool(t, client, alice.Id, "empty")

	// Its id is not: a tombstone keeps the primary key, so re-creating on a
	// deleted pool's id is a conflict rather than the idempotent re-create a live
	// id gets.
	reuse, err := client.CreateStoragePoolWithResponse(ctx, api.CreateStoragePoolRequest{
		Id: &empty.Id, Name: "reborn", OwnerUserId: alice.Id,
	})
	if err != nil {
		t.Fatalf("CreateStoragePool(dropped id): %v", err)
	}
	if reuse.StatusCode() != http.StatusConflict {
		t.Fatalf("re-create on a dropped id = %d, want 409, body = %s", reuse.StatusCode(), reuse.Body)
	}

	// Deleting it twice is a 404, not a second success.
	again, err := client.DeleteStoragePoolWithResponse(ctx, empty.Id)
	if err != nil {
		t.Fatalf("DeleteStoragePool(twice): %v", err)
	}
	if again.StatusCode() != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404, body = %s", again.StatusCode(), again.Body)
	}

	// A pool holding a live cluster is refused.
	busy, err := client.DeleteStoragePoolWithResponse(ctx, occupied.Id)
	if err != nil {
		t.Fatalf("DeleteStoragePool(occupied): %v", err)
	}
	if busy.StatusCode() != http.StatusConflict {
		t.Fatalf("occupied status = %d, want 409, body = %s", busy.StatusCode(), busy.Body)
	}

	// Delete the cluster and the pool becomes deletable — the cluster row survives
	// as a tombstone, and blocking on that would strand the pool forever.
	dc, err := client.DeleteClusterWithResponse(ctx, occupied.Id, cluster.Id)
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if dc.StatusCode() != http.StatusAccepted {
		t.Fatalf("DeleteCluster status = %d, want 202, body = %s", dc.StatusCode(), dc.Body)
	}
	drainTasks(t, store, backend, api.Provisioning{Deployer: &fakeDeployer{}})

	drained, err := client.DeleteStoragePoolWithResponse(ctx, occupied.Id)
	if err != nil {
		t.Fatalf("DeleteStoragePool(drained): %v", err)
	}
	if drained.StatusCode() != http.StatusNoContent {
		t.Fatalf("drained status = %d, want 204, body = %s", drained.StatusCode(), drained.Body)
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

// abortingDeleteBackend loses every pool delete to a serialization conflict, as
// PgBackend does once its own re-runs are exhausted.
type abortingDeleteBackend struct {
	*core.MemBackend
}

func (abortingDeleteBackend) DeleteStoragePool(context.Context, uuid.UUID) error {
	return fmt.Errorf("tombstone storage pool: %w", &pgconn.PgError{Code: "40001"})
}

// A delete that lost a serialization race changed nothing, so it answers a
// retryable 503, not a 500 that reads as the controller being broken (#1849).
func TestStoragePoolDeleteAbortIsRetryable(t *testing.T) {
	client, _ := newTestServerBackend(t, abortingDeleteBackend{core.NewMemBackend()}, api.Provisioning{})
	alice := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, alice.Id, "contended")

	del, err := client.DeleteStoragePoolWithResponse(context.Background(), pool.Id)
	if err != nil {
		t.Fatalf("DeleteStoragePool: %v", err)
	}
	if del.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", del.StatusCode(), del.Body)
	}
	if del.HTTPResponse.Header.Get("Retry-After") == "" {
		t.Error("503 carries no Retry-After")
	}
}
