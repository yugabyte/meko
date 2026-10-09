package api_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
)

// newExposedEndpoint creates an endpoint in mode with cidrs and runs its create
// task, returning what an update needs to address it.
func newExposedEndpoint(t *testing.T, mode string, cidrs []string) (
	*api.ClientWithResponses, *core.MemBackend, *fakeDeployer, func(), [4]uuid.UUID,
) {
	t.Helper()
	fake := &fakeDeployer{}
	prov := api.Provisioning{Deployer: connDeployer{fake}}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "cidr-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true,
	})
	rw := api.CreateEndpointRequestRoleReadWrite
	m := api.EndpointConnectivityMode(mode)
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{
		Role: &rw, Password: strptr(testPassword),
		Connectivity: &api.EndpointConnectivity{Mode: &m, AllowedCidrs: &cidrs},
	})
	if err != nil || created.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateEndpoint: status=%d err=%v body=%s", created.StatusCode(), err, created.Body)
	}
	drain := func() { drainTasks(t, store, backend, prov) }
	drain()
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if err != nil || list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("ListEndpoints: %v", err)
	}
	return client, backend, fake, drain, [4]uuid.UUID{pool.Id, clusterID, branchID, list.JSON200.Endpoints[0].Id}
}

// The ticket's case (CLOUDGA-37410): an nlb_external endpoint gains a CIDR. The
// list is patched onto the Service with no redeploy, GET reports it only once the
// task has run, and the generation is stamped with it.
func TestUpdateEndpointConnectivityReplacesTheAllowlist(t *testing.T) {
	client, _, fake, drain, ids := newExposedEndpoint(t, deploy.ConnectivityNLBExternal, []string{"103.200.215.98/32"})
	ctx := context.Background()

	want := []string{"103.200.215.98/32", "192.0.2.10/32"}
	gen := int64(2)
	resp, err := client.UpdateEndpointConnectivityWithResponse(ctx, ids[0], ids[1], ids[2], ids[3],
		api.UpdateEndpointConnectivityRequest{AllowedCidrs: want, YcpGeneration: &gen})
	if err != nil || resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("UpdateEndpointConnectivity: status=%d err=%v body=%s", resp.StatusCode(), err, resp.Body)
	}

	pending, _ := client.GetEndpointWithResponse(ctx, ids[0], ids[1], ids[2], ids[3])
	if got := *pending.JSON200.Connectivity.AllowedCidrs; reflect.DeepEqual(got, want) {
		t.Error("GET reports the new list on a 202 alone, before the task ran")
	}

	drain()

	done, _ := client.GetEndpointWithResponse(ctx, ids[0], ids[1], ids[2], ids[3])
	ep := done.JSON200
	if got := *ep.Connectivity.AllowedCidrs; !reflect.DeepEqual(got, want) {
		t.Errorf("allowed_cidrs after the task = %v, want %v", got, want)
	}
	if ep.Connectivity.Mode == nil || string(*ep.Connectivity.Mode) != deploy.ConnectivityNLBExternal {
		t.Errorf("mode = %v, want nlb_external kept", ep.Connectivity.Mode)
	}
	if ep.YcpGeneration == nil || *ep.YcpGeneration != gen {
		t.Errorf("ycp_generation = %v, want %d", ep.YcpGeneration, gen)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.created) != 1 {
		t.Errorf("deploy calls = %d, want only the create — the change patches the Service", len(fake.created))
	}
	if len(fake.sourceRanges) != 1 || !reflect.DeepEqual(fake.sourceRanges[0].cidrs, want) {
		t.Errorf("source-range patches = %+v, want one carrying %v", fake.sourceRanges, want)
	}
}

// An empty list is how the restriction is removed, not a missing field.
func TestUpdateEndpointConnectivityClearsTheAllowlist(t *testing.T) {
	client, _, _, drain, ids := newExposedEndpoint(t, deploy.ConnectivityNLBInternal, []string{"10.0.0.0/16"})
	ctx := context.Background()

	resp, err := client.UpdateEndpointConnectivityWithResponse(ctx, ids[0], ids[1], ids[2], ids[3],
		api.UpdateEndpointConnectivityRequest{AllowedCidrs: []string{}})
	if err != nil || resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("UpdateEndpointConnectivity: status=%d err=%v body=%s", resp.StatusCode(), err, resp.Body)
	}
	drain()
	done, _ := client.GetEndpointWithResponse(ctx, ids[0], ids[1], ids[2], ids[3])
	if c := done.JSON200.Connectivity; c != nil && c.AllowedCidrs != nil && len(*c.AllowedCidrs) != 0 {
		t.Errorf("allowed_cidrs = %v, want none", *c.AllowedCidrs)
	}
}

// The create path's rules apply against the endpoint's CURRENT mode, and a refusal
// plans nothing.
func TestUpdateEndpointConnectivityRefusesWhatCreateWould(t *testing.T) {
	cases := []struct {
		name  string
		mode  string
		cidrs []string
	}{
		{"bare IP", deploy.ConnectivityNLBExternal, []string{"192.0.2.10"}},
		{"cidrs on cluster_ip", deploy.ConnectivityClusterIP, []string{"192.0.2.10/32"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var start []string
			if tc.mode != deploy.ConnectivityClusterIP {
				start = []string{"10.0.0.0/16"}
			}
			client, _, fake, _, ids := newExposedEndpoint(t, tc.mode, start)
			resp, err := client.UpdateEndpointConnectivityWithResponse(context.Background(), ids[0], ids[1], ids[2], ids[3],
				api.UpdateEndpointConnectivityRequest{AllowedCidrs: tc.cidrs})
			if err != nil || resp.StatusCode() != http.StatusBadRequest {
				t.Fatalf("status=%d err=%v body=%s, want 400", resp.StatusCode(), err, resp.Body)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.created) != 1 {
				t.Errorf("deploy calls = %d, want only the create", len(fake.created))
			}
		})
	}
}

func TestUpdateEndpointConnectivityRequiresTheList(t *testing.T) {
	client, _, _, _, ids := newExposedEndpoint(t, deploy.ConnectivityNLBExternal, nil)
	resp, err := client.UpdateEndpointConnectivityWithBodyWithResponse(context.Background(), ids[0], ids[1], ids[2], ids[3],
		"application/json", strings.NewReader(`{}`))
	if err != nil || resp.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status=%d err=%v body=%s, want 400 for a body with no allowed_cidrs", resp.StatusCode(), err, resp.Body)
	}
}

// First-writer-wins, as for resize: a second replacement while one is pending
// would land in an order neither caller chose.
func TestUpdateEndpointConnectivityConflictsWhileOneIsInFlight(t *testing.T) {
	client, _, _, _, ids := newExposedEndpoint(t, deploy.ConnectivityNLBExternal, nil)
	ctx := context.Background()
	body := api.UpdateEndpointConnectivityRequest{AllowedCidrs: []string{"192.0.2.10/32"}}
	first, _ := client.UpdateEndpointConnectivityWithResponse(ctx, ids[0], ids[1], ids[2], ids[3], body)
	if first.StatusCode() != http.StatusAccepted {
		t.Fatalf("first: status=%d body=%s", first.StatusCode(), first.Body)
	}
	second, _ := client.UpdateEndpointConnectivityWithResponse(ctx, ids[0], ids[1], ids[2], ids[3], body)
	if second.StatusCode() != http.StatusConflict {
		t.Fatalf("second: status=%d body=%s, want 409", second.StatusCode(), second.Body)
	}
}
