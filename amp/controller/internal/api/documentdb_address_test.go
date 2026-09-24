package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// documentdb_address is derived, not stored: it is the endpoint's own host on the
// gateway's fixed port, reported only when the branch actually mounts
// documentdb. These pin both halves — a stored column could disagree with the
// pod, and reporting it without the extension would hand out an address nothing
// listens on.
func TestDocumentdbAddressOnlyWithDocumentdb(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep, Extensions: extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "documentdb-addr-pool")
	withWriter := true

	// A plain postgres cluster: no documentdb, so no gateway and no address.
	plain, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "plain", WithWriter: &withWriter, Password: strptr(testPassword),
	})
	if err != nil || plain.StatusCode() != http.StatusAccepted {
		t.Fatalf("create plain cluster: %v (%d)", err, plain.StatusCode())
	}
	drainTasks(t, store, backend, prov)
	plainID := *plain.JSON202.ClusterId
	pc, err := client.GetClusterWithResponse(ctx, pool.Id, plainID)
	if err != nil {
		t.Fatalf("get plain cluster: %v", err)
	}
	eps, err := client.ListEndpointsWithResponse(ctx, pool.Id, plainID, pc.JSON200.DefaultBranch.BranchId, nil)
	if err != nil || eps.JSON200 == nil || len(eps.JSON200.Endpoints) == 0 {
		t.Fatalf("list endpoints (plain): %v", err)
	}
	if a := eps.JSON200.Endpoints[0].DocumentdbAddress; a != nil {
		t.Errorf("plain postgres endpoint reports documentdb_address=%q, want none", *a)
	}

	// A documentdb cluster: the gateway rides along, so the address is reported.
	ddb := api.CreateClusterRequestEngine("documentdb")
	dd, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "docs", Engine: &ddb, WithWriter: &withWriter, Password: strptr(testPassword),
	})
	if err != nil || dd.StatusCode() != http.StatusAccepted {
		t.Fatalf("create documentdb cluster: %v (%d)", err, dd.StatusCode())
	}
	drainTasks(t, store, backend, prov)
	ddID := *dd.JSON202.ClusterId
	dc, err := client.GetClusterWithResponse(ctx, pool.Id, ddID)
	if err != nil {
		t.Fatalf("get documentdb cluster: %v", err)
	}
	deps, err := client.ListEndpointsWithResponse(ctx, pool.Id, ddID, dc.JSON200.DefaultBranch.BranchId, nil)
	if err != nil || deps.JSON200 == nil || len(deps.JSON200.Endpoints) == 0 {
		t.Fatalf("list endpoints (documentdb): %v", err)
	}
	ep := deps.JSON200.Endpoints[0]
	if ep.Address == "" {
		t.Skip("fake deployer reported no address; nothing to derive from")
	}
	if ep.DocumentdbAddress == nil {
		t.Fatalf("documentdb endpoint reports no documentdb_address (address=%q)", ep.Address)
	}
	// Same host as the endpoint — the gateway is a sidecar in its pod — on the
	// gateway's own port, not MongoDB's 27017.
	wantSuffix := ":" + itoa(extensions.DocumentDBGatewayPort)
	if len(*ep.DocumentdbAddress) < len(wantSuffix) ||
		(*ep.DocumentdbAddress)[len(*ep.DocumentdbAddress)-len(wantSuffix):] != wantSuffix {
		t.Errorf("documentdb_address = %q, want it to end in %q", *ep.DocumentdbAddress, wantSuffix)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
