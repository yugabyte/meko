package api_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-light/amp/internal/api"
	"github.com/yugabyte/yb-light/amp/internal/core"
	"github.com/yugabyte/yb-light/amp/internal/storagecontroller"
)

// fakeSCServer is an httptest stand-in for the managed storage controller's
// pending-init endpoints. It records the calls the timeline handlers make
// through a real storagecontroller.Client, so the create-timeline recipe path
// is exercised end-to-end without a real controller. failRecord, when set,
// makes the record (POST) return 500 so the handler's error path can be tested.
type fakeSCServer struct {
	mu               sync.Mutex
	recordHit        bool
	recordedTenant   string
	recordedTimeline string
	recordedReq      storagecontroller.PendingTimelineInit
	deleteHit        bool
	failRecord       bool
	// lastRecordLSN is what GetTimeline reports as the timeline's latest LSN —
	// the default branch fork point. Empty omits the field (so the handler's
	// "no usable LSN" path can be exercised).
	lastRecordLSN  string
	getTimelineHit bool
}

// start stands up the httptest server and returns a storagecontroller.Client
// pointed at it. It serves the pending-init recipe endpoints
// (POST/DELETE /control/v1/tenant/{tenant}/timeline/{timeline}/pending_init)
// and GetTimeline (GET /control/v1/tenant/{tenant}/timeline/{timeline}), which
// the branch handler reads to default the fork point.
func (f *fakeSCServer) start(t *testing.T) *storagecontroller.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/control/v1/tenant/", func(w http.ResponseWriter, r *http.Request) {
		// /control/v1/tenant/{tenant}/timeline/{timeline}[/pending_init]
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/control/v1/tenant/"), "/")
		f.mu.Lock()
		defer f.mu.Unlock()

		if !strings.HasSuffix(r.URL.Path, "/pending_init") {
			// GetTimeline: report the latest LSN so the branch handler can default
			// the fork point. Any other shape/method is a 404.
			if r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "timeline" {
				f.getTimelineHit = true
				w.Header().Set("Content-Type", "application/json")
				// Mirror the real controller's describe shape: the timeline's LSNs
				// are nested per shard under "shards", not at the top level.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"shards": []map[string]any{{"last_record_lsn": f.lastRecordLSN}},
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}

		tenant, timeline := parts[0], parts[2]
		switch r.Method {
		case http.MethodPost:
			if f.failRecord {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			f.recordHit = true
			f.recordedTenant = tenant
			f.recordedTimeline = timeline
			_ = json.NewDecoder(r.Body).Decode(&f.recordedReq)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			f.deleteHit = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return storagecontroller.NewClient(ts.Listener.Addr().String())
}

func newTimelineControllers(t *testing.T, sc *storagecontroller.Client) (*api.Controllers, *core.MemBackend) {
	t.Helper()
	backend := core.NewMemBackend()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return api.NewControllers(backend, nil, api.Provisioning{SC: sc}, logger), backend
}

func seedUserPool(t *testing.T, backend *core.MemBackend) uuid.UUID {
	t.Helper()
	u, err := backend.CreateUser(context.Background(), false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	p, err := backend.CreateStoragePool(context.Background(), u.ID, "pool")
	if err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	return p.ID
}

// TestCreateTimelineRecordsPendingInit: with a storage controller wired,
// create-timeline records the pending-init recipe (root timeline: no ancestor,
// start LSN 0/0, pg_version 17) keyed by the 32-hex ids, and does NOT delete it
// (deletion is deferred to the create-endpoint path).
func TestCreateTimelineRecordsPendingInit(t *testing.T) {
	sc := &fakeSCServer{}
	c, backend := newTimelineControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)

	resp, err := c.CreateTimeline(context.Background(), api.CreateTimelineRequestObject{
		PoolId: poolID,
		Body:   &api.CreateTimelineJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateTimeline: %v", err)
	}
	created, ok := resp.(api.CreateTimeline201JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want CreateTimeline201JSONResponse", resp)
	}

	if !sc.recordHit {
		t.Fatal("RecordPendingTimelineInit was not called")
	}
	if want := hex.EncodeToString(created.TenantId[:]); sc.recordedTenant != want {
		t.Errorf("recorded tenant = %q, want %q", sc.recordedTenant, want)
	}
	if want := hex.EncodeToString(created.TimelineId[:]); sc.recordedTimeline != want {
		t.Errorf("recorded timeline = %q, want %q", sc.recordedTimeline, want)
	}
	if sc.recordedReq.BranchName != "main" {
		t.Errorf("branch_name = %q, want main", sc.recordedReq.BranchName)
	}
	if sc.recordedReq.PgVersion != 17 {
		t.Errorf("pg_version = %d, want 17", sc.recordedReq.PgVersion)
	}
	if sc.recordedReq.AncestorTimelineID != nil {
		t.Errorf("ancestor_timeline_id = %v, want nil (root timeline)", *sc.recordedReq.AncestorTimelineID)
	}
	if sc.recordedReq.StartLSN != "0/0" {
		t.Errorf("start_lsn = %q, want 0/0", sc.recordedReq.StartLSN)
	}
	if sc.deleteHit {
		t.Error("DeletePendingTimelineInit was called during create-timeline; it belongs to create-endpoint")
	}
}

// TestCreateTimelineNoStorageController: without a storage controller, the
// handler records the control-plane row only (no panic, still 201).
func TestCreateTimelineNoStorageController(t *testing.T) {
	c, backend := newTimelineControllers(t, nil)
	poolID := seedUserPool(t, backend)

	resp, err := c.CreateTimeline(context.Background(), api.CreateTimelineRequestObject{
		PoolId: poolID,
		Body:   &api.CreateTimelineJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateTimeline: %v", err)
	}
	if _, ok := resp.(api.CreateTimeline201JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateTimeline201JSONResponse", resp)
	}
}

// TestCreateTimelineStorageError: a storage-controller failure recording the
// recipe fails the request.
func TestCreateTimelineStorageError(t *testing.T) {
	sc := &fakeSCServer{failRecord: true}
	c, backend := newTimelineControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)

	_, err := c.CreateTimeline(context.Background(), api.CreateTimelineRequestObject{
		PoolId: poolID,
		Body:   &api.CreateTimelineJSONRequestBody{Name: "main"},
	})
	if err == nil {
		t.Fatal("CreateTimeline: expected error when storage controller fails, got nil")
	}
}

// mustCreateMain creates a tenant + its main timeline through the controller
// (recording the main's recipe too) and returns it, for tests that then branch.
func mustCreateMain(t *testing.T, c *api.Controllers, poolID uuid.UUID, name string) api.Timeline {
	t.Helper()
	resp, err := c.CreateTimeline(context.Background(), api.CreateTimelineRequestObject{
		PoolId: poolID,
		Body:   &api.CreateTimelineJSONRequestBody{Name: name},
	})
	if err != nil {
		t.Fatalf("CreateTimeline: %v", err)
	}
	created, ok := resp.(api.CreateTimeline201JSONResponse)
	if !ok {
		t.Fatalf("CreateTimeline response = %T, want 201", resp)
	}
	return api.Timeline(created)
}

// TestBranchTimelineDefaultsForkToLatestLSN: branching with no fork_point_lsn
// reads the parent's latest LSN from the storage controller, returns a non-main
// timeline carrying the parent + fork LSN, and records a branch recipe whose
// ancestor + start_lsn are set (keyed by the branch's 32-hex id).
func TestBranchTimelineDefaultsForkToLatestLSN(t *testing.T) {
	sc := &fakeSCServer{lastRecordLSN: "0/16ABCD0"}
	c, backend := newTimelineControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)
	main := mustCreateMain(t, c, poolID, "main")

	resp, err := c.BranchTimeline(context.Background(), api.BranchTimelineRequestObject{
		PoolId:     poolID,
		TimelineId: main.TimelineId,
		Body:       &api.BranchTimelineJSONRequestBody{Name: "feature"},
	})
	if err != nil {
		t.Fatalf("BranchTimeline: %v", err)
	}
	created, ok := resp.(api.BranchTimeline201JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want BranchTimeline201JSONResponse", resp)
	}
	br := api.Timeline(created)

	if !sc.getTimelineHit {
		t.Error("GetTimeline was not called to default the fork point")
	}
	if br.IsMain {
		t.Error("branch IsMain = true, want false")
	}
	if br.ParentTimelineId == nil || *br.ParentTimelineId != main.TimelineId {
		t.Errorf("parent_timeline_id = %v, want %v", br.ParentTimelineId, main.TimelineId)
	}
	if br.ForkLsn == nil || *br.ForkLsn != "0/16ABCD0" {
		t.Errorf("fork_lsn = %v, want 0/16ABCD0", br.ForkLsn)
	}
	if br.TenantId != main.TenantId {
		t.Errorf("branch tenant = %v, want the parent's %v", br.TenantId, main.TenantId)
	}
	if br.PgVersion != main.PgVersion {
		t.Errorf("branch pg_version = %d, want inherited %d", br.PgVersion, main.PgVersion)
	}

	if !sc.recordHit {
		t.Fatal("RecordPendingTimelineInit was not called for the branch")
	}
	if want := hex.EncodeToString(br.TimelineId[:]); sc.recordedTimeline != want {
		t.Errorf("recorded timeline = %q, want the branch %q", sc.recordedTimeline, want)
	}
	if sc.recordedReq.AncestorTimelineID == nil {
		t.Fatal("recipe ancestor_timeline_id = nil, want the parent")
	}
	if want := hex.EncodeToString(main.TimelineId[:]); *sc.recordedReq.AncestorTimelineID != want {
		t.Errorf("recipe ancestor = %q, want the parent %q", *sc.recordedReq.AncestorTimelineID, want)
	}
	if sc.recordedReq.StartLSN != "0/16ABCD0" {
		t.Errorf("recipe start_lsn = %q, want 0/16ABCD0", sc.recordedReq.StartLSN)
	}
	if sc.recordedReq.BranchName != "feature" {
		t.Errorf("recipe branch_name = %q, want feature", sc.recordedReq.BranchName)
	}
}

// TestBranchTimelineExplicitForkLSN: an explicit fork_point_lsn is used verbatim
// and the storage controller is not consulted for a default.
func TestBranchTimelineExplicitForkLSN(t *testing.T) {
	sc := &fakeSCServer{}
	c, backend := newTimelineControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)
	main := mustCreateMain(t, c, poolID, "main")

	lsn := "0/ABCDEF1"
	resp, err := c.BranchTimeline(context.Background(), api.BranchTimelineRequestObject{
		PoolId:     poolID,
		TimelineId: main.TimelineId,
		Body:       &api.BranchTimelineJSONRequestBody{Name: "feature", ForkPointLsn: &lsn},
	})
	if err != nil {
		t.Fatalf("BranchTimeline: %v", err)
	}
	created, ok := resp.(api.BranchTimeline201JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want BranchTimeline201JSONResponse", resp)
	}
	if got := api.Timeline(created).ForkLsn; got == nil || *got != lsn {
		t.Errorf("fork_lsn = %v, want %q", got, lsn)
	}
	if sc.getTimelineHit {
		t.Error("describe was called despite an explicit fork_point_lsn")
	}
	if sc.recordedReq.StartLSN != lsn {
		t.Errorf("recipe start_lsn = %q, want %q", sc.recordedReq.StartLSN, lsn)
	}
	if want := hex.EncodeToString(main.TimelineId[:]); sc.recordedReq.AncestorTimelineID == nil || *sc.recordedReq.AncestorTimelineID != want {
		t.Errorf("recipe ancestor = %v, want the parent %q", sc.recordedReq.AncestorTimelineID, want)
	}
}

// TestBranchTimelineInvalidForkLSN: a malformed fork_point_lsn 400s before any
// state is touched.
func TestBranchTimelineInvalidForkLSN(t *testing.T) {
	sc := &fakeSCServer{}
	c, backend := newTimelineControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)
	main := mustCreateMain(t, c, poolID, "main")

	bad := "not-an-lsn"
	resp, err := c.BranchTimeline(context.Background(), api.BranchTimelineRequestObject{
		PoolId:     poolID,
		TimelineId: main.TimelineId,
		Body:       &api.BranchTimelineJSONRequestBody{Name: "feature", ForkPointLsn: &bad},
	})
	if err != nil {
		t.Fatalf("BranchTimeline: %v", err)
	}
	if _, ok := resp.(api.BranchTimeline400JSONResponse); !ok {
		t.Fatalf("response = %T, want BranchTimeline400JSONResponse", resp)
	}
}

// TestBranchTimelineDuplicateName: a branch whose name is already taken in the
// parent's tenant (here, the main timeline's own name) conflicts -> 409.
func TestBranchTimelineDuplicateName(t *testing.T) {
	sc := &fakeSCServer{lastRecordLSN: "0/10"}
	c, backend := newTimelineControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)
	main := mustCreateMain(t, c, poolID, "main")

	resp, err := c.BranchTimeline(context.Background(), api.BranchTimelineRequestObject{
		PoolId:     poolID,
		TimelineId: main.TimelineId,
		Body:       &api.BranchTimelineJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("BranchTimeline: %v", err)
	}
	if _, ok := resp.(api.BranchTimeline409JSONResponse); !ok {
		t.Fatalf("response = %T, want BranchTimeline409JSONResponse", resp)
	}
}

// TestCreateEndpointOnBranchPassesAncestor: deploying an endpoint on a branch
// timeline relays the ancestor + fork LSN to the deployer and forces bootstrap
// off, so the endpoint forks the ancestor instead of initdb'ing an empty
// timeline (the bug where branch endpoints came up with no data).
func TestCreateEndpointOnBranchPassesAncestor(t *testing.T) {
	dep := &fakeDeployer{}
	client, backend := newTestServerWithProv(t, api.Provisioning{Deployer: dep})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	tenantID := uuid.New()
	parentID := uuid.New()
	branchID := uuid.New()
	// A parent (main) and a branch off it, in the same tenant.
	backend.SeedTimeline(pool.Id, core.Timeline{TenantID: tenantID, TimelineID: parentID, Name: "main", PgVersion: 17, IsMain: true})
	backend.SeedTimeline(pool.Id, core.Timeline{TenantID: tenantID, TimelineID: branchID, Name: "feature", PgVersion: 17, ParentTimelineID: &parentID, ForkLSN: "0/236A730"})

	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", created.StatusCode(), created.Body)
	}
	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1", len(dep.created))
	}
	spec := dep.created[0]
	if want := hex.EncodeToString(parentID[:]); spec.AncestorTimelineID != want {
		t.Errorf("AncestorTimelineID = %q, want the parent %q", spec.AncestorTimelineID, want)
	}
	if spec.AncestorLSN != "0/236A730" {
		t.Errorf("AncestorLSN = %q, want 0/236A730", spec.AncestorLSN)
	}
	if spec.Bootstrap {
		t.Error("Bootstrap = true on a branch endpoint, want false (branches clone, never initdb)")
	}
}

// TestCreateEndpointOnMainHasNoAncestor: a main timeline's endpoint carries no
// ancestor (so it bootstraps as before).
func TestCreateEndpointOnMainHasNoAncestor(t *testing.T) {
	dep := &fakeDeployer{}
	client, backend := newTestServerWithProv(t, api.Provisioning{Deployer: dep})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	tlID := uuid.New()
	backend.SeedTimeline(pool.Id, core.Timeline{TenantID: uuid.New(), TimelineID: tlID, Name: "main", PgVersion: 17, IsMain: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	if _, err := client.CreateEndpointWithResponse(ctx, pool.Id, tlID, api.CreateEndpointRequest{Role: &rw}); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1", len(dep.created))
	}
	if spec := dep.created[0]; spec.AncestorTimelineID != "" || spec.AncestorLSN != "" {
		t.Errorf("main endpoint carried ancestry %q@%q, want none", spec.AncestorTimelineID, spec.AncestorLSN)
	}
}

// TestBranchTimelineNoStorageController: without a controller there is no way to
// resolve a default fork point (and no recipe to materialize the branch), so a
// branch with no fork_point_lsn is rejected (400) rather than silently forking
// at 0/0; an explicit fork_point_lsn still works and is recorded on the row.
func TestBranchTimelineNoStorageController(t *testing.T) {
	c, backend := newTimelineControllers(t, nil)
	poolID := seedUserPool(t, backend)
	main := mustCreateMain(t, c, poolID, "main")

	// No fork point + no controller -> 400 (no silent 0/0).
	resp, err := c.BranchTimeline(context.Background(), api.BranchTimelineRequestObject{
		PoolId:     poolID,
		TimelineId: main.TimelineId,
		Body:       &api.BranchTimelineJSONRequestBody{Name: "feature"},
	})
	if err != nil {
		t.Fatalf("BranchTimeline: %v", err)
	}
	if _, ok := resp.(api.BranchTimeline400JSONResponse); !ok {
		t.Fatalf("response = %T, want BranchTimeline400JSONResponse (no SC, no fork_point_lsn)", resp)
	}

	// Explicit fork point + no controller -> 201, recorded on the control row.
	lsn := "0/ABCDEF1"
	resp, err = c.BranchTimeline(context.Background(), api.BranchTimelineRequestObject{
		PoolId:     poolID,
		TimelineId: main.TimelineId,
		Body:       &api.BranchTimelineJSONRequestBody{Name: "feature", ForkPointLsn: &lsn},
	})
	if err != nil {
		t.Fatalf("BranchTimeline(explicit): %v", err)
	}
	created, ok := resp.(api.BranchTimeline201JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want BranchTimeline201JSONResponse", resp)
	}
	if got := api.Timeline(created).ForkLsn; got == nil || *got != lsn {
		t.Errorf("fork_lsn = %v, want %q", got, lsn)
	}
}

// TestBranchTimelineDefaultForkUnavailable: with a controller that reports no
// last_record_lsn for the parent (e.g. not yet materialized), a default branch
// (no fork_point_lsn) is rejected with a clear 400 rather than a wrong fork.
func TestBranchTimelineDefaultForkUnavailable(t *testing.T) {
	sc := &fakeSCServer{lastRecordLSN: ""} // describe -> shards[0].last_record_lsn = ""
	c, backend := newTimelineControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)
	main := mustCreateMain(t, c, poolID, "main")

	resp, err := c.BranchTimeline(context.Background(), api.BranchTimelineRequestObject{
		PoolId:     poolID,
		TimelineId: main.TimelineId,
		Body:       &api.BranchTimelineJSONRequestBody{Name: "feature"},
	})
	if err != nil {
		t.Fatalf("BranchTimeline: %v", err)
	}
	if _, ok := resp.(api.BranchTimeline400JSONResponse); !ok {
		t.Fatalf("response = %T, want BranchTimeline400JSONResponse (no usable LSN)", resp)
	}
	if !sc.getTimelineHit {
		t.Error("describe (TimelineLatestLSN) was not called")
	}
}
