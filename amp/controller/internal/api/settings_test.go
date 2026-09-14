package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
		w.Header().Set("Content-Type", "application/json")
		// The pre-check runs first and is a different question: "would you take
		// these?" Answering it with the reload's reply would refuse the write
		// before this test's subject — what the reload reported — ever happens.
		if r.URL.Path == "/v1/settings/check" {
			_, _ = w.Write([]byte(`{"not_applied":[]}`))
			return
		}
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{
			"restarted": ["compute_ctl"],
			"reloaded": [],
			"restart_forced_by": ["shared_buffers"],
			"not_applied": [
				{"name": "block_size", "reason": "cannot be set in a configuration file"},
				{"name": "work_memm", "reason": "unrecognized parameter"}
			]
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
	// The only signal that a stored setting is not in force — and the reason has
	// to survive with it, because "unrecognized parameter" and "invalid value"
	// send the operator to different fixes.
	if got.NotApplied == nil || len(*got.NotApplied) != 2 {
		t.Fatalf("not_applied = %v, want both settings", got.NotApplied)
	}
	if n := (*got.NotApplied)[1]; n.Name != "work_memm" || n.Reason != "unrecognized parameter" {
		t.Errorf("not_applied[1] = %+v, want work_memm/unrecognized parameter", n)
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

// The pre-check exists so a typo is a 400 instead of a value that sits in the
// row doing nothing. It has to refuse the write outright: storing settings the
// endpoint has already said it will not use is the failure the check is for.
func TestUpdateSettingsRefusesWhatTheEndpointWillNotAccept(t *testing.T) {
	ctx := context.Background()
	backend := core.NewMemBackend()

	var asked map[string]any
	sup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/settings/check" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&asked)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"not_applied":[
			{"name":"work_memm","reason":"unrecognized parameter"},
			{"name":"work_mem","reason":"invalid value"}
		]}`))
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

	bad := map[string]string{"work_memm": "5MB", "work_mem": "banana"}
	resp, err := client.UpdateEndpointSettingsWithResponse(ctx, pool, cl.ID, br.BranchID, ep.ID,
		api.UpdateEndpointSettingsJSONRequestBody{PgSettings: &bad})
	if err != nil {
		t.Fatalf("UpdateEndpointSettings: %v", err)
	}
	if resp.StatusCode() != 400 {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode(), string(resp.Body))
	}
	// Both names and both reasons, so one edit fixes everything.
	for _, want := range []string{"work_memm", "unrecognized parameter", "work_mem", "invalid value"} {
		if !strings.Contains(string(resp.Body), want) {
			t.Errorf("the 400 does not mention %q: %s", want, string(resp.Body))
		}
	}
	if asked == nil {
		t.Error("the endpoint was never asked")
	}
	// Nothing may be stored: a refused write that half-landed would leave the
	// operator's next read showing settings the compute will never use.
	stored, err := backend.GetEndpoint(ctx, pool, cl.ID, br.BranchID, ep.ID)
	if err != nil {
		t.Fatalf("GetEndpoint: %v", err)
	}
	if len(stored.PgSettings) != 0 {
		t.Errorf("refused settings were stored anyway: %v", stored.PgSettings)
	}
}

// A pod that cannot be asked must not block the edit. Nothing in the model says
// whether one is running, so a paused endpoint, a deleted pod and a deployer
// with no route all look the same from here — and refusing an operator's write
// because their compute is down would be worse than storing it unchecked, which
// is what happened before the check existed.
func TestUpdateSettingsStoresWhenTheEndpointCannotBeAsked(t *testing.T) {
	ctx := context.Background()
	backend := core.NewMemBackend()
	// Empty supervisor URL: no route, so neither the check nor the notify lands.
	client, _, _ := newTestServerWith(t, backend, api.Provisioning{Deployer: &fakeDeployer{}})

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

	work := map[string]string{"work_mem": "64MB"}
	resp, err := client.UpdateEndpointSettingsWithResponse(ctx, pool, cl.ID, br.BranchID, ep.ID,
		api.UpdateEndpointSettingsJSONRequestBody{PgSettings: &work})
	if err != nil {
		t.Fatalf("UpdateEndpointSettings: %v", err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d, want 200: %s", resp.StatusCode(), string(resp.Body))
	}
	stored, err := backend.GetEndpoint(ctx, pool, cl.ID, br.BranchID, ep.ID)
	if err != nil {
		t.Fatalf("GetEndpoint: %v", err)
	}
	if stored.PgSettings["work_mem"] != "64MB" {
		t.Errorf("an uncheckable write was not stored: %v", stored.PgSettings)
	}
}

// A branch-wide change is vetted by the branch's writer, and a reply naming
// nothing lets it through.
func TestUpdateBranchSettingsAsksTheWriter(t *testing.T) {
	ctx := context.Background()
	backend := core.NewMemBackend()

	var hits int
	sup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/settings/check" {
			hits++
			_, _ = w.Write([]byte(`{"not_applied":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"restarted":[],"reloaded":["compute_ctl"]}`))
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
	if _, err := backend.CreateEndpoint(ctx, cl.ID, br.BranchID, core.RoleReadWrite, nil); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	work := map[string]string{"work_mem": "64MB"}
	resp, err := client.UpdateBranchSettingsWithResponse(ctx, pool, cl.ID, br.BranchID,
		api.UpdateBranchSettingsJSONRequestBody{PgSettings: &work})
	if err != nil {
		t.Fatalf("UpdateBranchSettings: %v", err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d, want 200: %s", resp.StatusCode(), string(resp.Body))
	}
	if hits != 1 {
		t.Errorf("the writer was asked %d times, want 1", hits)
	}
	stored, err := backend.GetBranch(ctx, pool, cl.ID, br.BranchID)
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if stored.PgSettings["work_mem"] != "64MB" {
		t.Errorf("an accepted write was not stored: %v", stored.PgSettings)
	}
}

// A branch with no writer has nowhere to send the check — only a pod can judge a
// Postgres setting — and must still be configurable: its settings are the
// baseline whatever endpoint comes next will start on. So the check is skipped
// and the row is written, rather than the branch becoming unconfigurable until
// someone creates a writer.
func TestUpdateBranchSettingsSkipsTheCheckWithNoWriter(t *testing.T) {
	ctx := context.Background()
	backend := core.NewMemBackend()

	var asked int
	sup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/settings/check" {
			asked++
		}
		w.Header().Set("Content-Type", "application/json")
		// If it were asked, it would refuse — so a write that lands proves it
		// was never asked, not that the answer happened to be favourable.
		_, _ = w.Write([]byte(`{"not_applied":[{"name":"work_mem","reason":"invalid value"}]}`))
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
	// No endpoints at all: the branch exists and its settings are still the
	// baseline for whatever starts on it next.
	work := map[string]string{"work_mem": "64MB"}
	resp, err := client.UpdateBranchSettingsWithResponse(ctx, pool, cl.ID, br.BranchID,
		api.UpdateBranchSettingsJSONRequestBody{PgSettings: &work})
	if err != nil {
		t.Fatalf("UpdateBranchSettings: %v", err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d, want 200: %s", resp.StatusCode(), string(resp.Body))
	}
	if asked != 0 {
		t.Errorf("the check ran %d times with no writer, want 0", asked)
	}
	stored, err := backend.GetBranch(ctx, pool, cl.ID, br.BranchID)
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if stored.PgSettings["work_mem"] != "64MB" {
		t.Errorf("the write was not stored: %v", stored.PgSettings)
	}
}

// A read replica is not a writer. The check goes to the branch's read_write
// endpoint or nowhere, so a branch carrying only replicas behaves like one with
// no endpoints.
func TestUpdateBranchSettingsDoesNotAskAReadReplica(t *testing.T) {
	ctx := context.Background()
	backend := core.NewMemBackend()

	var asked int
	sup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/settings/check" {
			asked++
		}
		w.Header().Set("Content-Type", "application/json")
		// Refuses if it is asked, so a stored setting is evidence the check was
		// skipped rather than that the answer happened to be favourable.
		_, _ = w.Write([]byte(`{"not_applied":[{"name":"work_mem","reason":"invalid value"}]}`))
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
	if _, err := backend.CreateEndpoint(ctx, cl.ID, br.BranchID, core.RoleReadOnly, nil); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	work := map[string]string{"work_mem": "64MB"}
	resp, err := client.UpdateBranchSettingsWithResponse(ctx, pool, cl.ID, br.BranchID,
		api.UpdateBranchSettingsJSONRequestBody{PgSettings: &work})
	if err != nil {
		t.Fatalf("UpdateBranchSettings: %v", err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d, want 200: %s", resp.StatusCode(), string(resp.Body))
	}
	if asked != 0 {
		t.Errorf("a read replica was asked to vet a branch change (%d times)", asked)
	}
	stored, err := backend.GetBranch(ctx, pool, cl.ID, br.BranchID)
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if stored.PgSettings["work_mem"] != "64MB" {
		t.Errorf("the write was not stored: %v", stored.PgSettings)
	}
}

// The reconcile agent's generation is recorded by the same write that stores the
// settings. They are the only stamping verbs that are synchronous — there is no
// task and no worker, the handler returns once the row is written — which makes the
// handler the only thing that can honestly say the work landed.
//
// Monotonic, like the worker's own write — an older generation arriving late must
// not drag the column back and make the agent re-apply what already landed — and
// absent when the caller sent none, including on a row that has never been
// stamped, where the column is NULL rather than 0.
func TestUpdateSettingsStampsTheYCPGeneration(t *testing.T) {
	ctx := context.Background()
	backend := core.NewMemBackend()
	client, _, _ := newTestServerWith(t, backend, api.Provisioning{Deployer: &fakeDeployer{}})

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

	endpointGen := func() *int64 {
		t.Helper()
		stored, err := backend.GetEndpoint(ctx, pool, cl.ID, br.BranchID, ep.ID)
		if err != nil {
			t.Fatalf("GetEndpoint: %v", err)
		}
		return stored.YCPGeneration
	}
	branchGen := func() *int64 {
		t.Helper()
		stored, err := backend.GetBranch(ctx, pool, cl.ID, br.BranchID)
		if err != nil {
			t.Fatalf("GetBranch: %v", err)
		}
		return stored.YCPGeneration
	}
	patchEndpoint := func(body api.UpdateEndpointSettingsJSONRequestBody) int {
		t.Helper()
		resp, err := client.UpdateEndpointSettingsWithResponse(ctx, pool, cl.ID, br.BranchID, ep.ID, body)
		if err != nil {
			t.Fatalf("UpdateEndpointSettings: %v", err)
		}
		return resp.StatusCode()
	}

	// A write with no generation leaves the column alone — NULL stays NULL rather
	// than becoming 0, which is the trap in GREATEST: it ignores NULL operands, so
	// the obvious expression would quietly write a value the caller never sent.
	work := map[string]string{"work_mem": "64MB"}
	if code := patchEndpoint(api.UpdateEndpointSettingsJSONRequestBody{PgSettings: &work}); code != 200 {
		t.Fatalf("status %d, want 200", code)
	}
	if got := endpointGen(); got != nil {
		t.Errorf("ycp_generation = %d, want it untouched — the caller sent none", *got)
	}

	gen := func(n int64) *int64 { return &n }
	if code := patchEndpoint(api.UpdateEndpointSettingsJSONRequestBody{
		PgSettings: &work, YcpGeneration: gen(4)}); code != 200 {
		t.Fatalf("status %d, want 200", code)
	}
	if got := endpointGen(); got == nil || *got != 4 {
		t.Fatalf("ycp_generation = %v, want 4", got)
	}

	// An older generation completing late cannot regress it.
	if code := patchEndpoint(api.UpdateEndpointSettingsJSONRequestBody{
		PgSettings: &work, YcpGeneration: gen(2)}); code != 200 {
		t.Fatalf("status %d, want 200", code)
	}
	if got := endpointGen(); got == nil || *got != 4 {
		t.Errorf("ycp_generation = %v, want it held at 4 — the stamp is monotonic", got)
	}

	// The branch verb is the same write, and it is the one an agent leans on: a
	// branch spec declaring only settings issues no task at all, so this PATCH is
	// the only thing that can record the generation for it.
	bwork := map[string]string{"shared_buffers": "1GB"}
	resp, err := client.UpdateBranchSettingsWithResponse(ctx, pool, cl.ID, br.BranchID,
		api.UpdateBranchSettingsJSONRequestBody{PgSettings: &bwork, YcpGeneration: gen(7)})
	if err != nil {
		t.Fatalf("UpdateBranchSettings: %v", err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d, want 200: %s", resp.StatusCode(), string(resp.Body))
	}
	if got := branchGen(); got == nil || *got != 7 {
		t.Errorf("branch ycp_generation = %v, want 7", got)
	}
}

// A refused request stamps nothing. The check runs before the write, so a 400
// stores neither the settings nor the generation — and a generation recorded for
// settings that were rejected is precisely the latch meko#985 removed: the agent
// would read the echo, believe the spec landed, and never retry.
func TestRefusedSettingsStampNoGeneration(t *testing.T) {
	ctx := context.Background()
	backend := core.NewMemBackend()

	sup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"not_applied":[{"name":"work_memm","reason":"unrecognized parameter"}]}`))
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

	n := int64(4)
	bad := map[string]string{"work_memm": "5MB"}
	resp, err := client.UpdateEndpointSettingsWithResponse(ctx, pool, cl.ID, br.BranchID, ep.ID,
		api.UpdateEndpointSettingsJSONRequestBody{PgSettings: &bad, YcpGeneration: &n})
	if err != nil {
		t.Fatalf("UpdateEndpointSettings: %v", err)
	}
	if resp.StatusCode() != 400 {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode(), string(resp.Body))
	}
	stored, err := backend.GetEndpoint(ctx, pool, cl.ID, br.BranchID, ep.ID)
	if err != nil {
		t.Fatalf("GetEndpoint: %v", err)
	}
	if stored.YCPGeneration != nil {
		t.Errorf("ycp_generation = %d, want none — nothing was stored, so nothing landed", *stored.YCPGeneration)
	}
}

// A body naming no settings surface is still a real write — it carries the
// generation — but there is nothing for a compute to re-read, so no endpoint is
// told. That is what lets the reconcile agent stamp a generation without bouncing
// a reload through every endpoint on the branch.
func TestSettingsBodyWithNoSurfaceNotifiesNobody(t *testing.T) {
	ctx := context.Background()
	backend := core.NewMemBackend()

	var notified int
	sup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/settings/reload") {
			notified++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"restarted":[],"reloaded":[],"restart_forced_by":[],"not_applied":[]}`))
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
	if _, err := backend.CreateEndpoint(ctx, cl.ID, br.BranchID, core.RoleReadWrite, nil); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	n := int64(9)
	resp, err := client.UpdateBranchSettingsWithResponse(ctx, pool, cl.ID, br.BranchID,
		api.UpdateBranchSettingsJSONRequestBody{YcpGeneration: &n})
	if err != nil {
		t.Fatalf("UpdateBranchSettings: %v", err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d, want 200: %s", resp.StatusCode(), string(resp.Body))
	}
	if notified != 0 {
		t.Errorf("endpoints notified = %d, want 0 — nothing changed for a compute to re-read", notified)
	}
	if resp.JSON200.Endpoints != nil {
		t.Errorf("endpoints = %v, want none reported", resp.JSON200.Endpoints)
	}
	stored, err := backend.GetBranch(ctx, pool, cl.ID, br.BranchID)
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if stored.YCPGeneration == nil || *stored.YCPGeneration != 9 {
		t.Errorf("branch ycp_generation = %v, want 9 — the write still happened", stored.YCPGeneration)
	}
}
