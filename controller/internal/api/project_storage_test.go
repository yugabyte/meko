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

	"github.com/yugabyte/yb-amp/controller/internal/api"
	"github.com/yugabyte/yb-amp/controller/internal/core"
	"github.com/yugabyte/yb-amp/controller/internal/storagecontroller"
)

// fakeSCServer is an httptest stand-in for the managed storage controller's
// pending-init endpoints. It records the calls the project/branch handlers make
// through a real storagecontroller.Client, so the create-project recipe path is
// exercised end-to-end without a real controller. The storage controller keeps
// the tenant/timeline vocabulary (the controller maps project->tenant and
// branch->timeline at this boundary). failRecord, when set, makes the record
// (POST) return 500 so the handler's error path can be tested.
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

func newProjectControllers(t *testing.T, sc *storagecontroller.Client) (*api.Controllers, *core.MemBackend) {
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

// TestCreateProjectRecordsPendingInit: with a storage controller wired,
// create-project records the pending-init recipe for the default branch (no
// ancestor, start LSN 0/0, pg_version 17) keyed by the 32-hex ids, and does NOT
// delete it (deletion is deferred to the create-endpoint path).
func TestCreateProjectRecordsPendingInit(t *testing.T) {
	sc := &fakeSCServer{}
	c, backend := newProjectControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)

	resp, err := c.CreateProject(context.Background(), api.CreateProjectRequestObject{
		PoolId: poolID,
		Body:   &api.CreateProjectJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	created, ok := resp.(api.CreateProject201JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want CreateProject201JSONResponse", resp)
	}
	project := api.Project(created)
	def := project.DefaultBranch

	if !sc.recordHit {
		t.Fatal("RecordPendingTimelineInit was not called")
	}
	if want := hex.EncodeToString(project.Id[:]); sc.recordedTenant != want {
		t.Errorf("recorded tenant = %q, want %q", sc.recordedTenant, want)
	}
	if want := hex.EncodeToString(def.BranchId[:]); sc.recordedTimeline != want {
		t.Errorf("recorded timeline = %q, want %q", sc.recordedTimeline, want)
	}
	if sc.recordedReq.BranchName != "main" {
		t.Errorf("branch_name = %q, want main", sc.recordedReq.BranchName)
	}
	if sc.recordedReq.PgVersion != 17 {
		t.Errorf("pg_version = %d, want 17", sc.recordedReq.PgVersion)
	}
	if sc.recordedReq.AncestorTimelineID != nil {
		t.Errorf("ancestor_timeline_id = %v, want nil (default branch)", *sc.recordedReq.AncestorTimelineID)
	}
	if sc.recordedReq.StartLSN != "0/0" {
		t.Errorf("start_lsn = %q, want 0/0", sc.recordedReq.StartLSN)
	}
	if sc.deleteHit {
		t.Error("DeletePendingTimelineInit was called during create-project; it belongs to create-endpoint")
	}
}

// TestCreateProjectNoStorageController: without a storage controller, the
// handler records the control-plane row only (no panic, still 201).
func TestCreateProjectNoStorageController(t *testing.T) {
	c, backend := newProjectControllers(t, nil)
	poolID := seedUserPool(t, backend)

	resp, err := c.CreateProject(context.Background(), api.CreateProjectRequestObject{
		PoolId: poolID,
		Body:   &api.CreateProjectJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, ok := resp.(api.CreateProject201JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateProject201JSONResponse", resp)
	}
}

// TestCreateProjectStorageError: a storage-controller failure recording the
// recipe fails the request.
func TestCreateProjectStorageError(t *testing.T) {
	sc := &fakeSCServer{failRecord: true}
	c, backend := newProjectControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)

	_, err := c.CreateProject(context.Background(), api.CreateProjectRequestObject{
		PoolId: poolID,
		Body:   &api.CreateProjectJSONRequestBody{Name: "main"},
	})
	if err == nil {
		t.Fatal("CreateProject: expected error when storage controller fails, got nil")
	}
}

// mustCreateProjectC creates a project + its default branch through the
// controller (recording the default branch's recipe too) and returns it, for
// tests that then branch.
func mustCreateProjectC(t *testing.T, c *api.Controllers, poolID uuid.UUID, name string) api.Project {
	t.Helper()
	resp, err := c.CreateProject(context.Background(), api.CreateProjectRequestObject{
		PoolId: poolID,
		Body:   &api.CreateProjectJSONRequestBody{Name: name},
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	created, ok := resp.(api.CreateProject201JSONResponse)
	if !ok {
		t.Fatalf("CreateProject response = %T, want 201", resp)
	}
	return api.Project(created)
}

// TestCreateBranchDefaultsForkToLatestLSN: branching with no fork_point_lsn and
// no explicit parent reads the project's default-branch latest LSN from the
// storage controller, returns a non-default branch carrying the parent + fork
// LSN, and records a branch recipe whose ancestor + start_lsn are set (keyed by
// the branch's 32-hex id).
func TestCreateBranchDefaultsForkToLatestLSN(t *testing.T) {
	sc := &fakeSCServer{lastRecordLSN: "0/16ABCD0"}
	c, backend := newProjectControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, poolID, "main")
	def := project.DefaultBranch

	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ProjectId: project.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature"},
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	created, ok := resp.(api.CreateBranch201JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want CreateBranch201JSONResponse", resp)
	}
	br := api.Branch(created)

	if !sc.getTimelineHit {
		t.Error("GetTimeline was not called to default the fork point")
	}
	if br.IsDefault {
		t.Error("branch is_default = true, want false")
	}
	if br.ParentBranchId == nil || *br.ParentBranchId != def.BranchId {
		t.Errorf("parent_branch_id = %v, want the default branch %v", br.ParentBranchId, def.BranchId)
	}
	if br.ForkLsn == nil || *br.ForkLsn != "0/16ABCD0" {
		t.Errorf("fork_lsn = %v, want 0/16ABCD0", br.ForkLsn)
	}
	if br.ProjectId != project.Id {
		t.Errorf("branch project = %v, want %v", br.ProjectId, project.Id)
	}
	if br.PgVersion != def.PgVersion {
		t.Errorf("branch pg_version = %d, want inherited %d", br.PgVersion, def.PgVersion)
	}

	if !sc.recordHit {
		t.Fatal("RecordPendingTimelineInit was not called for the branch")
	}
	if want := hex.EncodeToString(br.BranchId[:]); sc.recordedTimeline != want {
		t.Errorf("recorded timeline = %q, want the branch %q", sc.recordedTimeline, want)
	}
	if sc.recordedReq.AncestorTimelineID == nil {
		t.Fatal("recipe ancestor_timeline_id = nil, want the parent")
	}
	if want := hex.EncodeToString(def.BranchId[:]); *sc.recordedReq.AncestorTimelineID != want {
		t.Errorf("recipe ancestor = %q, want the default branch %q", *sc.recordedReq.AncestorTimelineID, want)
	}
	if sc.recordedReq.StartLSN != "0/16ABCD0" {
		t.Errorf("recipe start_lsn = %q, want 0/16ABCD0", sc.recordedReq.StartLSN)
	}
	if sc.recordedReq.BranchName != "feature" {
		t.Errorf("recipe branch_name = %q, want feature", sc.recordedReq.BranchName)
	}
}

// TestCreateBranchExplicitForkLSN: an explicit fork_point_lsn is used verbatim
// and the storage controller is not consulted for a default.
func TestCreateBranchExplicitForkLSN(t *testing.T) {
	sc := &fakeSCServer{}
	c, backend := newProjectControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, poolID, "main")

	lsn := "0/ABCDEF1"
	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ProjectId: project.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature", ForkPointLsn: &lsn},
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	created, ok := resp.(api.CreateBranch201JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want CreateBranch201JSONResponse", resp)
	}
	if got := api.Branch(created).ForkLsn; got == nil || *got != lsn {
		t.Errorf("fork_lsn = %v, want %q", got, lsn)
	}
	if sc.getTimelineHit {
		t.Error("describe was called despite an explicit fork_point_lsn")
	}
	if sc.recordedReq.StartLSN != lsn {
		t.Errorf("recipe start_lsn = %q, want %q", sc.recordedReq.StartLSN, lsn)
	}
	if want := hex.EncodeToString(project.DefaultBranch.BranchId[:]); sc.recordedReq.AncestorTimelineID == nil || *sc.recordedReq.AncestorTimelineID != want {
		t.Errorf("recipe ancestor = %v, want the default branch %q", sc.recordedReq.AncestorTimelineID, want)
	}
}

// TestCreateBranchInvalidForkLSN: a malformed fork_point_lsn 400s before any
// state is touched.
func TestCreateBranchInvalidForkLSN(t *testing.T) {
	sc := &fakeSCServer{}
	c, backend := newProjectControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, poolID, "main")

	bad := "not-an-lsn"
	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ProjectId: project.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature", ForkPointLsn: &bad},
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if _, ok := resp.(api.CreateBranch400JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateBranch400JSONResponse", resp)
	}
}

// TestCreateBranchDuplicateName: a branch whose name is already taken in the
// project (here, the default branch's own name) conflicts -> 409.
func TestCreateBranchDuplicateName(t *testing.T) {
	sc := &fakeSCServer{lastRecordLSN: "0/10"}
	c, backend := newProjectControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, poolID, "main")

	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ProjectId: project.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if _, ok := resp.(api.CreateBranch409JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateBranch409JSONResponse", resp)
	}
}

// TestCreateEndpointOnBranchPassesAncestor: deploying an endpoint on a non-default
// branch relays the ancestor + fork LSN to the deployer and forces bootstrap
// off, so the endpoint forks the ancestor instead of initdb'ing an empty
// timeline (the bug where branch endpoints came up with no data).
func TestCreateEndpointOnBranchPassesAncestor(t *testing.T) {
	dep := &fakeDeployer{}
	client, backend := newTestServerWithProv(t, api.Provisioning{Deployer: dep})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	projectID := uuid.New()
	parentID := uuid.New()
	branchID := uuid.New()
	// A default branch and a branch off it, in the same project.
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: parentID, Name: "main", PgVersion: 17, IsDefault: true})
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: branchID, Name: "feature", PgVersion: 17, ParentBranchID: &parentID, ForkLSN: "0/236A730"})

	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &rw})
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

// TestCreateEndpointOnDefaultHasNoAncestor: a default branch's endpoint carries
// no ancestor (so it bootstraps as before).
func TestCreateEndpointOnDefaultHasNoAncestor(t *testing.T) {
	dep := &fakeDeployer{}
	client, backend := newTestServerWithProv(t, api.Provisioning{Deployer: dep})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	projectID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: branchID, Name: "main", PgVersion: 17, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	if _, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &rw}); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1", len(dep.created))
	}
	if spec := dep.created[0]; spec.AncestorTimelineID != "" || spec.AncestorLSN != "" {
		t.Errorf("default-branch endpoint carried ancestry %q@%q, want none", spec.AncestorTimelineID, spec.AncestorLSN)
	}
}

// TestCreateBranchNoStorageController: without a controller there is no way to
// resolve a default fork point (and no recipe to materialize the branch), so a
// branch with no fork_point_lsn is rejected (400) rather than silently forking
// at 0/0; an explicit fork_point_lsn still works and is recorded on the row.
func TestCreateBranchNoStorageController(t *testing.T) {
	c, backend := newProjectControllers(t, nil)
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, poolID, "main")

	// No fork point + no controller -> 400 (no silent 0/0).
	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ProjectId: project.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature"},
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if _, ok := resp.(api.CreateBranch400JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateBranch400JSONResponse (no SC, no fork_point_lsn)", resp)
	}

	// Explicit fork point + no controller -> 201, recorded on the control row.
	lsn := "0/ABCDEF1"
	resp, err = c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ProjectId: project.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature", ForkPointLsn: &lsn},
	})
	if err != nil {
		t.Fatalf("CreateBranch(explicit): %v", err)
	}
	created, ok := resp.(api.CreateBranch201JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want CreateBranch201JSONResponse", resp)
	}
	if got := api.Branch(created).ForkLsn; got == nil || *got != lsn {
		t.Errorf("fork_lsn = %v, want %q", got, lsn)
	}
}

// TestCreateBranchDefaultForkUnavailable: with a controller that reports no
// last_record_lsn for the parent (e.g. not yet materialized), a default branch
// (no fork_point_lsn) is rejected with a clear 400 rather than a wrong fork.
func TestCreateBranchDefaultForkUnavailable(t *testing.T) {
	sc := &fakeSCServer{lastRecordLSN: ""} // describe -> shards[0].last_record_lsn = ""
	c, backend := newProjectControllers(t, sc.start(t))
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, poolID, "main")

	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ProjectId: project.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature"},
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if _, ok := resp.(api.CreateBranch400JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateBranch400JSONResponse (no usable LSN)", resp)
	}
	if !sc.getTimelineHit {
		t.Error("describe (TimelineLatestLSN) was not called")
	}
}
