package api_test

import (
	"context"
	"fmt"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-amp/controller/internal/api"
	"github.com/yugabyte/yb-amp/controller/internal/core"
	"github.com/yugabyte/yb-amp/controller/internal/node"
	"github.com/yugabyte/yb-amp/controller/internal/task"
)

// fakeRecipeStore is an in-memory task.RecipeStore that records the calls the
// project/branch handlers make, so the create-project recipe path is exercised
// end-to-end without a database. The storage layer is flat: the recipe is keyed
// by a single branch_id (the controller passes the branch's branch_id verbatim at
// this boundary). failRecord, when set, makes RecordBranchRecipe fail so the
// handler's error path can be tested.
type fakeRecipeStore struct {
	mu             sync.Mutex
	recordHit      bool
	recordedBranch string
	recordedReq    task.BranchRecipe
	deleteHit      bool
	failRecord     bool
}

func (f *fakeRecipeStore) RecordBranchRecipe(_ context.Context, branchID string, r task.BranchRecipe) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRecord {
		return fmt.Errorf("record recipe failed")
	}
	f.recordHit = true
	f.recordedBranch = branchID
	f.recordedReq = r
	return nil
}

func (f *fakeRecipeStore) ClearBranchRecipe(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteHit = true
	return nil
}

// newProjectControllersProv builds the controllers for the direct-call tests over
// a fresh MemBackend, wiring a REAL task.Service over a MemStore (the mutating
// handlers now plan tasks, so a nil service would nil-deref). It returns the
// controllers, the backend, and that store so a test can drainTasks against the
// same backend and then read the materialized resource.
func newProjectControllersProv(t *testing.T, prov api.Provisioning) (*api.Controllers, *core.MemBackend, *task.MemStore) {
	t.Helper()
	backend := core.NewMemBackend()
	store := task.NewMemStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return api.NewControllers(backend, task.NewService(store), prov, logger), backend, store
}

// fakeSafekeeper is an httptest stand-in for an endpoint's co-located safekeeper.
// It serves GET /v1/branch/{branch_id} reporting flushLSN, which
// the branch handler reads from the parent's read_write endpoint to default the
// fork point (#133). flushLSN empty => 404 (the safekeeper does not serve the
// branch), exercising the "no usable LSN" path.
type fakeSafekeeper struct {
	mu       sync.Mutex
	flushLSN string
	hit      bool
}

func (f *fakeSafekeeper) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/branch/", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hit = true
		if f.flushLSN == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"flush_lsn": f.flushLSN, "commit_lsn": f.flushLSN})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

func (f *fakeSafekeeper) wasHit() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hit
}

// seedWriter records a read_write endpoint on the branch so resolveForkLSN finds
// a node to read the fork point from. The endpoint's safekeeper address comes
// from the fakeDeployer's EndpointSafekeeperURL.
func seedWriter(t *testing.T, backend *core.MemBackend, projectID, branchID uuid.UUID) {
	t.Helper()
	if _, err := backend.CreateEndpoint(context.Background(), projectID, branchID, core.RoleReadWrite, nil); err != nil {
		t.Fatalf("seed read_write endpoint: %v", err)
	}
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

// acceptedProjectTask asserts a direct CreateProject call returned a 202 + a
// pending create_project Task and returns the task's id + project id (the task
// stamps the project it will create).
func acceptedProjectTask(t *testing.T, resp api.CreateProjectResponseObject) (taskID, projectID uuid.UUID) {
	t.Helper()
	created, ok := resp.(api.CreateProject202JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want CreateProject202JSONResponse", resp)
	}
	body := created.Body
	if body.Kind != "create_project" || body.Status != api.TaskStatusPending {
		t.Errorf("task = {kind:%q status:%q}, want {create_project pending}", body.Kind, body.Status)
	}
	if want := "/api/v1/tasks/" + body.Id.String(); created.Headers.Location != want {
		t.Errorf("Location = %q, want %q", created.Headers.Location, want)
	}
	if body.ProjectId == nil {
		t.Fatal("task project_id = nil, want a real project id")
	}
	return body.Id, *body.ProjectId
}

// TestCreateProjectRecordsPendingInit: with a controller wired,
// create-project plans a task that, once driven, records the pending-init recipe
// for the default branch (no ancestor, start LSN 0/0, pg_version 17) keyed by the
// 32-hex branch id, and does NOT delete it (deletion is deferred to
// create-endpoint). The recipe is now recorded by the record_branch_recipe step.
func TestCreateProjectRecordsPendingInit(t *testing.T) {
	recipes := &fakeRecipeStore{}
	prov := api.Provisioning{Recipes: recipes}
	c, backend, store := newProjectControllersProv(t, prov)
	poolID := seedUserPool(t, backend)

	resp, err := c.CreateProject(context.Background(), api.CreateProjectRequestObject{
		PoolId: poolID,
		Body:   &api.CreateProjectJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	_, projectID := acceptedProjectTask(t, resp)

	// The recipe is recorded by the worker, not synchronously: drive the task.
	drainTasks(t, store, backend, prov)
	if got := soleTask(t, store); got.Status != task.StatusSucceeded {
		t.Fatalf("create_project task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}

	// The default branch's id is needed to assert the recipe key — read it back.
	def := getDefaultBranch(t, c, poolID, projectID)

	if !recipes.recordHit {
		t.Fatal("RecordPendingTimelineInit was not called")
	}
	if want := hex.EncodeToString(def.BranchId[:]); recipes.recordedBranch != want {
		t.Errorf("recorded branch = %q, want %q", recipes.recordedBranch, want)
	}
	if recipes.recordedReq.BranchName != "main" {
		t.Errorf("branch_name = %q, want main", recipes.recordedReq.BranchName)
	}
	if recipes.recordedReq.PgVersion != 18 {
		t.Errorf("pg_version = %d, want 18", recipes.recordedReq.PgVersion)
	}
	if recipes.recordedReq.AncestorBranchID != "" {
		t.Errorf("ancestor_branch_id = %v, want nil (default branch)", recipes.recordedReq.AncestorBranchID)
	}
	if recipes.recordedReq.StartLSN != "0/0" {
		t.Errorf("start_lsn = %q, want 0/0", recipes.recordedReq.StartLSN)
	}
	if recipes.deleteHit {
		t.Error("DeletePendingTimelineInit was called during create-project; it belongs to create-endpoint")
	}
}

// TestCreateProjectNoController: without a controller, the task
// still succeeds (it records the control-plane rows only, no recipe) — no panic.
func TestCreateProjectNoController(t *testing.T) {
	prov := api.Provisioning{}
	c, backend, store := newProjectControllersProv(t, prov)
	poolID := seedUserPool(t, backend)

	resp, err := c.CreateProject(context.Background(), api.CreateProjectRequestObject{
		PoolId: poolID,
		Body:   &api.CreateProjectJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	acceptedProjectTask(t, resp)
	drainTasks(t, store, backend, prov)
	if got := soleTask(t, store); got.Status != task.StatusSucceeded {
		t.Fatalf("create_project task (no SC) status = %q, want succeeded; error = %v", got.Status, got.Error)
	}
}

// TestCreateProjectRecipeError: a recipe-store failure recording the
// recipe now fails the create_project TASK (the record_branch_recipe step
// returns the error), not the synchronous handler — which returns 202.
func TestCreateProjectRecipeError(t *testing.T) {
	recipes := &fakeRecipeStore{failRecord: true}
	prov := api.Provisioning{Recipes: recipes}
	c, backend, store := newProjectControllersProv(t, prov)
	poolID := seedUserPool(t, backend)

	resp, err := c.CreateProject(context.Background(), api.CreateProjectRequestObject{
		PoolId: poolID,
		Body:   &api.CreateProjectJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	acceptedProjectTask(t, resp)
	drainTasks(t, store, backend, prov)
	got := soleTask(t, store)
	if got.Status != task.StatusFailed || got.Error == nil {
		t.Fatalf("create_project task = {status:%q err:%v}, want failed (controller error)", got.Status, got.Error)
	}
}

// soleTaskWithKind returns the single task of the given kind in the store,
// failing if there is not exactly one (the direct-call branch tests create a
// create_project task plus the branch task, so they filter by kind).
func soleTaskWithKind(t *testing.T, store *task.MemStore, kind string) *task.Task {
	t.Helper()
	tasks, _, err := store.List(context.Background(), task.TaskFilter{Kind: kind}, 1000, 0)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks of kind %q = %d, want exactly 1", kind, len(tasks))
	}
	return tasks[0]
}

// branchByName lists a project's branches through the controller and returns the
// one with the given name, failing if absent.
func branchByName(t *testing.T, c *api.Controllers, poolID, projectID uuid.UUID, name string) api.Branch {
	t.Helper()
	resp, err := c.ListBranches(context.Background(), api.ListBranchesRequestObject{PoolId: poolID, ProjectId: projectID})
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	got, ok := resp.(api.ListBranches200JSONResponse)
	if !ok {
		t.Fatalf("ListBranches response = %T, want 200", resp)
	}
	for _, b := range api.BranchList(got).Branches {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("branch %q not found in project %s", name, projectID)
	return api.Branch{}
}

// getDefaultBranch reads a project back through the controller and returns its
// default branch.
func getDefaultBranch(t *testing.T, c *api.Controllers, poolID, projectID uuid.UUID) api.Branch {
	t.Helper()
	resp, err := c.GetProject(context.Background(), api.GetProjectRequestObject{PoolId: poolID, ProjectId: projectID})
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	got, ok := resp.(api.GetProject200JSONResponse)
	if !ok {
		t.Fatalf("GetProject response = %T, want 200", resp)
	}
	return api.Project(got).DefaultBranch
}

// mustCreateProjectC creates a project + its default branch through the
// controller AND drives the resulting task to completion (recording the default
// branch's recipe too), then returns the materialized project, for tests that
// then branch off it. It drains every pending task on the shared store.
func mustCreateProjectC(t *testing.T, c *api.Controllers, store *task.MemStore, backend core.Backend, prov api.Provisioning, poolID uuid.UUID, name string) api.Project {
	t.Helper()
	resp, err := c.CreateProject(context.Background(), api.CreateProjectRequestObject{
		PoolId: poolID,
		Body:   &api.CreateProjectJSONRequestBody{Name: name},
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	_, projectID := acceptedProjectTask(t, resp)
	drainTasks(t, store, backend, prov)

	gp, err := c.GetProject(context.Background(), api.GetProjectRequestObject{PoolId: poolID, ProjectId: projectID})
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	got, ok := gp.(api.GetProject200JSONResponse)
	if !ok {
		t.Fatalf("GetProject response = %T, want 200", gp)
	}
	return api.Project(got)
}

// TestCreateBranchDefaultsForkToFlushLSN: branching with no fork_point_lsn and
// no explicit parent defaults the fork point to the parent's durable WAL end
// (the safekeeper's flush_lsn), read from the parent's read_write endpoint;
// returns a non-default branch carrying the parent + fork LSN, and records a
// branch recipe whose ancestor + start_lsn are set (keyed by the branch's 32-hex
// id).
func TestCreateBranchDefaultsForkToFlushLSN(t *testing.T) {
	recipes := &fakeRecipeStore{}
	sk := &fakeSafekeeper{flushLSN: "0/16ABCD0"}
	dep := &fakeDeployer{skURL: sk.start(t)}
	prov := api.Provisioning{Recipes: recipes, Deployer: dep, Node: node.NewClient()}
	c, backend, store := newProjectControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, store, backend, prov, poolID, "main")
	def := project.DefaultBranch
	// The parent (default branch) must have a read_write endpoint whose
	// safekeeper holds the fork point.
	seedWriter(t, backend, project.Id, def.BranchId)

	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ProjectId: project.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature"},
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	created, ok := resp.(api.CreateBranch202JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want CreateBranch202JSONResponse", resp)
	}
	if created.Body.Kind != "branch" || created.Body.Status != api.TaskStatusPending {
		t.Errorf("task = {kind:%q status:%q}, want {branch pending}", created.Body.Kind, created.Body.Status)
	}

	// The fork point is resolved synchronously (the safekeeper is queried before the
	// task is planned); the branch row + recipe are materialized by the worker.
	if !sk.wasHit() {
		t.Error("the parent safekeeper was not queried to default the fork point (cheap pre-resolution)")
	}
	drainTasks(t, store, backend, prov)
	if got := soleTaskWithKind(t, store, "branch"); got.Status != task.StatusSucceeded {
		t.Fatalf("branch task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}
	br := branchByName(t, c, poolID, project.Id, "feature")

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

	if !recipes.recordHit {
		t.Fatal("RecordPendingTimelineInit was not called for the branch")
	}
	if want := hex.EncodeToString(br.BranchId[:]); recipes.recordedBranch != want {
		t.Errorf("recorded branch = %q, want the branch %q", recipes.recordedBranch, want)
	}
	if recipes.recordedReq.AncestorBranchID == "" {
		t.Fatal("recipe ancestor_branch_id = nil, want the parent")
	}
	if want := hex.EncodeToString(def.BranchId[:]); recipes.recordedReq.AncestorBranchID != want {
		t.Errorf("recipe ancestor = %q, want the default branch %q", recipes.recordedReq.AncestorBranchID, want)
	}
	if recipes.recordedReq.StartLSN != "0/16ABCD0" {
		t.Errorf("recipe start_lsn = %q, want 0/16ABCD0", recipes.recordedReq.StartLSN)
	}
	if recipes.recordedReq.BranchName != "feature" {
		t.Errorf("recipe branch_name = %q, want feature", recipes.recordedReq.BranchName)
	}
}

// TestCreateBranchExplicitForkLSN: an explicit fork_point_lsn is used verbatim
// and no node is consulted for a default (so it works without a deployer).
func TestCreateBranchExplicitForkLSN(t *testing.T) {
	recipes := &fakeRecipeStore{}
	sk := &fakeSafekeeper{flushLSN: "0/16ABCD0"}
	dep := &fakeDeployer{skURL: sk.start(t)}
	prov := api.Provisioning{Recipes: recipes, Deployer: dep, Node: node.NewClient()}
	c, backend, store := newProjectControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, store, backend, prov, poolID, "main")

	lsn := "0/ABCDEF1"
	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ProjectId: project.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature", ForkPointLsn: &lsn},
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if _, ok := resp.(api.CreateBranch202JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateBranch202JSONResponse", resp)
	}
	if sk.wasHit() {
		t.Error("the parent safekeeper was queried despite an explicit fork_point_lsn")
	}

	drainTasks(t, store, backend, prov)
	if got := soleTaskWithKind(t, store, "branch"); got.Status != task.StatusSucceeded {
		t.Fatalf("branch task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}
	if got := branchByName(t, c, poolID, project.Id, "feature").ForkLsn; got == nil || *got != lsn {
		t.Errorf("fork_lsn = %v, want %q", got, lsn)
	}
	if recipes.recordedReq.StartLSN != lsn {
		t.Errorf("recipe start_lsn = %q, want %q", recipes.recordedReq.StartLSN, lsn)
	}
	if want := hex.EncodeToString(project.DefaultBranch.BranchId[:]); recipes.recordedReq.AncestorBranchID == "" || recipes.recordedReq.AncestorBranchID != want {
		t.Errorf("recipe ancestor = %v, want the default branch %q", recipes.recordedReq.AncestorBranchID, want)
	}
}

// TestCreateBranchInvalidForkLSN: a malformed fork_point_lsn 400s before any
// task is planned (cheap pre-validation).
func TestCreateBranchInvalidForkLSN(t *testing.T) {
	recipes := &fakeRecipeStore{}
	prov := api.Provisioning{Recipes: recipes}
	c, backend, store := newProjectControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, store, backend, prov, poolID, "main")

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
// project (here, the default branch's own name) is no longer a synchronous 409 —
// the create is accepted (202) and the conflict surfaces as a FAILED branch task
// when its create_branch step's EnsureBranch hits the name index. The fork point
// resolves first (a cheap pre-check), so the parent still needs a read_write
// endpoint + safekeeper.
func TestCreateBranchDuplicateName(t *testing.T) {
	recipes := &fakeRecipeStore{}
	sk := &fakeSafekeeper{flushLSN: "0/10"}
	dep := &fakeDeployer{skURL: sk.start(t)}
	prov := api.Provisioning{Recipes: recipes, Deployer: dep, Node: node.NewClient()}
	c, backend, store := newProjectControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, store, backend, prov, poolID, "main")
	seedWriter(t, backend, project.Id, project.DefaultBranch.BranchId)

	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ProjectId: project.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if _, ok := resp.(api.CreateBranch202JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateBranch202JSONResponse", resp)
	}
	drainTasks(t, store, backend, prov)
	got := soleTaskWithKind(t, store, "branch")
	if got.Status != task.StatusFailed || got.Error == nil {
		t.Fatalf("duplicate-name branch task = {status:%q err:%v}, want failed (name conflict)", got.Status, got.Error)
	}
}

// TestCreateEndpointOnBranchPassesAncestor: deploying an endpoint on a non-default
// branch relays the ancestor + fork LSN to the deployer and forces bootstrap
// off, so the endpoint forks the ancestor instead of initdb'ing an empty
// timeline (the bug where branch endpoints came up with no data).
func TestCreateEndpointOnBranchPassesAncestor(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	projectID := uuid.New()
	parentID := uuid.New()
	branchID := uuid.New()
	// A default branch and a branch off it, in the same project.
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: parentID, Name: "main", PgVersion: 18, IsDefault: true})
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: branchID, Name: "feature", PgVersion: 18, ParentBranchID: &parentID, ForkLSN: "0/236A730"})

	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", created.StatusCode(), created.Body)
	}
	// The deploy spec is built by the create_endpoint step, so drive the task first.
	drainTasks(t, store, backend, prov)
	if got := soleTask(t, store); got.Status != task.StatusSucceeded {
		t.Fatalf("create_endpoint task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}
	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1", len(dep.created))
	}
	spec := dep.created[0]
	if want := hex.EncodeToString(parentID[:]); spec.AncestorBranchID != want {
		t.Errorf("AncestorBranchID = %q, want the parent %q", spec.AncestorBranchID, want)
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
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	projectID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ProjectID: projectID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	if _, err := client.CreateEndpointWithResponse(ctx, pool.Id, projectID, branchID, api.CreateEndpointRequest{Role: &rw}); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	drainTasks(t, store, backend, prov)
	if got := soleTask(t, store); got.Status != task.StatusSucceeded {
		t.Fatalf("create_endpoint task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}
	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1", len(dep.created))
	}
	if spec := dep.created[0]; spec.AncestorBranchID != "" || spec.AncestorLSN != "" {
		t.Errorf("default-branch endpoint carried ancestry %q@%q, want none", spec.AncestorBranchID, spec.AncestorLSN)
	}
}

// TestCreateBranchNoController: without a controller there is no way to
// resolve a default fork point (and no recipe to materialize the branch), so a
// branch with no fork_point_lsn is rejected (400) rather than silently forking
// at 0/0; an explicit fork_point_lsn still works and is recorded on the row.
func TestCreateBranchNoController(t *testing.T) {
	prov := api.Provisioning{}
	c, backend, store := newProjectControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, store, backend, prov, poolID, "main")

	// No fork point + no controller -> 400 (no silent 0/0); cheap pre-validation.
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

	// Explicit fork point + no controller -> 202; the task materializes the branch
	// row carrying the fork LSN (no recipe, since there is no controller).
	lsn := "0/ABCDEF1"
	resp, err = c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ProjectId: project.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature", ForkPointLsn: &lsn},
	})
	if err != nil {
		t.Fatalf("CreateBranch(explicit): %v", err)
	}
	if _, ok := resp.(api.CreateBranch202JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateBranch202JSONResponse", resp)
	}
	drainTasks(t, store, backend, prov)
	if got := soleTaskWithKind(t, store, "branch"); got.Status != task.StatusSucceeded {
		t.Fatalf("branch task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}
	if got := branchByName(t, c, poolID, project.Id, "feature").ForkLsn; got == nil || *got != lsn {
		t.Errorf("fork_lsn = %v, want %q", got, lsn)
	}
}

// TestCreateBranchDefaultForkNoEndpoint: a default branch (no fork_point_lsn) off
// a parent that has no read_write endpoint (not yet materialized) is rejected
// with a clear 400 rather than a wrong fork — there is no safekeeper to read.
func TestCreateBranchDefaultForkNoEndpoint(t *testing.T) {
	recipes := &fakeRecipeStore{}
	sk := &fakeSafekeeper{flushLSN: "0/16ABCD0"}
	dep := &fakeDeployer{skURL: sk.start(t)}
	prov := api.Provisioning{Recipes: recipes, Deployer: dep, Node: node.NewClient()}
	c, backend, store := newProjectControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, store, backend, prov, poolID, "main")
	// No endpoint seeded on the default branch.

	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ProjectId: project.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature"},
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if _, ok := resp.(api.CreateBranch400JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateBranch400JSONResponse (no endpoint to read)", resp)
	}
	if sk.wasHit() {
		t.Error("the safekeeper was queried even though the parent has no read_write endpoint")
	}
}

// TestCreateBranchDefaultForkSafekeeperUnavailable: with a read_write endpoint on
// the parent but its safekeeper reporting no flush_lsn (e.g. the timeline is not
// yet there), a default branch is rejected with a clear 400 rather than a wrong
// fork.
func TestCreateBranchDefaultForkSafekeeperUnavailable(t *testing.T) {
	recipes := &fakeRecipeStore{}
	sk := &fakeSafekeeper{flushLSN: ""} // 404: the safekeeper does not serve the timeline
	dep := &fakeDeployer{skURL: sk.start(t)}
	prov := api.Provisioning{Recipes: recipes, Deployer: dep, Node: node.NewClient()}
	c, backend, store := newProjectControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	project := mustCreateProjectC(t, c, store, backend, prov, poolID, "main")
	seedWriter(t, backend, project.Id, project.DefaultBranch.BranchId)

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
	if !sk.wasHit() {
		t.Error("the parent safekeeper was not queried")
	}
}
