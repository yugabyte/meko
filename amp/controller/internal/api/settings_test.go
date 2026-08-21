package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// The endpoint's own account of what it did has to reach the caller intact.
// Every field here is a fact the caller cannot get any other way: which services
// were bounced, which took the change while running, which setting cost them the
// restart, and which settings are stored and doing nothing at all. A decode that
// dropped one would report a clean success for a change that did not happen.
func TestUpdateSettingsReportsWhatTheEndpointDid(t *testing.T) {
	ctx := context.Background()
	backend := core.NewMemBackend()

	var gotPath string
	sup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"restarted": ["compute_ctl"],
			"reloaded": [],
			"restart_forced_by": ["shared_buffers"],
			"not_applied": ["block_size", "yb_amp.nonsense"]
		}`))
	}))
	defer sup.Close()

	client, _, _ := newTestServerWith(t, backend,
		api.Provisioning{Deployer: &fakeDeployer{supURL: sup.URL}})

	pool := seedUserPool(t, backend)
	cl, err := backend.CreateCluster(ctx, pool, "main", 18)
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	br := cl.DefaultBranch
	ep, err := backend.CreateEndpoint(ctx, cl.ID, br.BranchID, core.RoleReadWrite, nil)
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	work := map[string]string{"shared_buffers": "512MB"}
	resp, err := client.UpdateEndpointSettingsWithResponse(ctx, pool, cl.ID, br.BranchID, ep.ID,
		api.UpdateEndpointSettingsJSONRequestBody{PgSettings: &work})
	if err != nil {
		t.Fatalf("UpdateEndpointSettings: %v", err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode(), string(resp.Body))
	}
	if gotPath != "/v1/settings/reload" {
		t.Errorf("POSTed to %q, want /v1/settings/reload", gotPath)
	}
	if resp.JSON200.Endpoints == nil || len(*resp.JSON200.Endpoints) != 1 {
		t.Fatalf("want one endpoint outcome, got %v", resp.JSON200.Endpoints)
	}
	got := (*resp.JSON200.Endpoints)[0]
	if got.Unreachable != nil {
		t.Fatalf("endpoint answered, must not be unreachable: %s", *got.Unreachable)
	}
	if got.Restarted == nil || len(*got.Restarted) != 1 || (*got.Restarted)[0] != "compute_ctl" {
		t.Errorf("restarted = %v, want [compute_ctl]", got.Restarted)
	}
	if got.RestartForcedBy == nil || (*got.RestartForcedBy)[0] != "shared_buffers" {
		t.Errorf("restart_forced_by = %v, want [shared_buffers]", got.RestartForcedBy)
	}
	// The only signal that a stored setting is not in force.
	if got.NotApplied == nil || len(*got.NotApplied) != 2 {
		t.Fatalf("not_applied = %v, want both names", got.NotApplied)
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
