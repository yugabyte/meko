package api_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
)

// End to end through the HTTP surface: the settings must be stored, and an
// endpoint that cannot be told must not turn that into a failure — the row is
// the durable answer and the notify is best effort on top. Reporting it as an
// error would leave the caller unsure whether the settings were saved, which is
// the one thing that is never in doubt.
func TestUpdateSettingsStoresEvenWhenTheEndpointCannotBeTold(t *testing.T) {
	ctx := context.Background()
	backend := core.NewMemBackend()
	// A deployer whose supervisor URL is empty: "cannot be reached".
	client, _, _ := newTestServerWith(t, backend, api.Provisioning{Deployer: &fakeDeployer{}})

	pool := seedUserPool(t, backend)
	cl, err := backend.CreateCluster(ctx, pool, "main", 18)
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	// A cluster comes with its default branch, which is all this needs.
	br := cl.DefaultBranch
	ep, err := backend.CreateEndpoint(ctx, cl.ID, br.BranchID, core.RoleReadWrite, nil)
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	work := map[string]string{"work_mem": "64MB"}
	resp, err := client.UpdateEndpointSettingsWithResponse(ctx, pool, cl.ID, br.BranchID, ep.ID,
		api.UpdateEndpointSettingsJSONRequestBody{PgSettings: &work})
	if err != nil {
		t.Fatalf("UpdateEndpointSettings: %v", err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode(), string(resp.Body))
	}
	if !resp.JSON200.Applied {
		t.Fatal("applied must be true: the row was written")
	}
	if resp.JSON200.Endpoints == nil || len(*resp.JSON200.Endpoints) != 1 {
		t.Fatalf("want one endpoint outcome, got %v", resp.JSON200.Endpoints)
	}
	got := (*resp.JSON200.Endpoints)[0]
	if got.Unreachable == nil {
		t.Fatal("an endpoint with no settings API must be reported unreachable, not silently fine")
	}

	stored, err := backend.GetEndpoint(ctx, pool, cl.ID, br.BranchID, ep.ID)
	if err != nil {
		t.Fatalf("GetEndpoint: %v", err)
	}
	if stored.PgSettings["work_mem"] != "64MB" {
		t.Fatalf("settings were not stored: %v", stored.PgSettings)
	}

	// Omitted surfaces are untouched, not cleared.
	psOnly := api.DaemonSettings{"scale_factor": 8}
	if _, err := client.UpdateEndpointSettingsWithResponse(ctx, pool, cl.ID, br.BranchID, ep.ID,
		api.UpdateEndpointSettingsJSONRequestBody{PageserverSettings: &psOnly}); err != nil {
		t.Fatalf("second update: %v", err)
	}
	stored, err = backend.GetEndpoint(ctx, pool, cl.ID, br.BranchID, ep.ID)
	if err != nil {
		t.Fatalf("GetEndpoint: %v", err)
	}
	if stored.PgSettings["work_mem"] != "64MB" {
		t.Fatalf("an omitted surface was cleared: %v", stored.PgSettings)
	}
	if stored.PageserverSettings["scale_factor"] != float64(8) && stored.PageserverSettings["scale_factor"] != 8 {
		t.Fatalf("pageserver settings not stored: %v", stored.PageserverSettings)
	}
}

// A branch that does not exist is a 404 rather than a silent no-op, so a caller
// with a stale id learns it now instead of wondering why nothing changed.
func TestUpdateBranchSettingsUnknownBranchIs404(t *testing.T) {
	ctx := context.Background()
	backend := core.NewMemBackend()
	client, _, _ := newTestServerWith(t, backend, api.Provisioning{Deployer: &fakeDeployer{}})
	pool := seedUserPool(t, backend)
	cl, err := backend.CreateCluster(ctx, pool, "main", 18)
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	pg := map[string]string{"work_mem": "64MB"}
	resp, err := client.UpdateBranchSettingsWithResponse(ctx, pool, cl.ID, uuid.New(),
		api.UpdateBranchSettingsJSONRequestBody{PgSettings: &pg})
	if err != nil {
		t.Fatalf("UpdateBranchSettings: %v", err)
	}
	if resp.StatusCode() != 404 {
		t.Fatalf("want 404, got %d: %s", resp.StatusCode(), string(resp.Body))
	}
}
