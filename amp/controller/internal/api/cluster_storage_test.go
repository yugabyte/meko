package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/node"
	"github.com/yugabyte/amp/controller/internal/task"
)

// fakeRecipeStore is an in-memory task.RecipeStore that records the calls the
// cluster/branch handlers make, so the create-cluster recipe path is exercised
// end-to-end without a database. The storage layer is flat, so the recipe is keyed
// by a single branch_id (the controller passes the branch's branch_id verbatim at
// this boundary) — but the row it seeds lives in the control plane's own
// cluster-scoped branch table, so the cluster id travels with it. failRecord, when
// set, makes RecordBranchRecipe fail so the handler's error path can be tested.
type fakeRecipeStore struct {
	// backend, when set, is where the fake materializes the branch. In production
	// RecordBranch is the branch's creator on the fork path — the whole branch task
	// is one transaction so a rejected fork leaves nothing — so a fake that only
	// records the call would let these tests pass while the real thing created no
	// branch at all.
	backend *core.MemBackend

	mu              sync.Mutex
	recordHit       bool
	recordedCluster uuid.UUID
	recordedBranch  uuid.UUID
	recordedReq     task.BranchRecipe
	failRecord      bool
}

func (f *fakeRecipeStore) RecordBranchRecipe(_ context.Context, clusterID, branchID uuid.UUID, r task.BranchRecipe) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRecord {
		return fmt.Errorf("record recipe failed")
	}
	f.recordHit = true
	f.recordedCluster = clusterID
	f.recordedBranch = branchID
	f.recordedReq = r
	if f.backend == nil {
		return nil
	}
	// Mirror the real store: declare the branch as part of this call.
	if r.AncestorBranchID == nil {
		return nil // a cluster's default branch is created by create_cluster
	}
	return f.backend.EnsureBranch(context.Background(), *r.AncestorBranchID, clusterID, branchID, r.BranchName, r.StartLSN, r.PgVersion)
}

// newClusterControllersProv builds the controllers for the direct-call tests over
// a fresh MemBackend, wiring a REAL task.Service over a MemStore (the mutating
// handlers now plan tasks, so a nil service would nil-deref). It returns the
// controllers, the backend, and that store so a test can drainTasks against the
// same backend and then read the materialized resource.
func newClusterControllersProv(t *testing.T, prov api.Provisioning) (*api.Controllers, *core.MemBackend, *task.MemStore) {
	t.Helper()
	backend := core.NewMemBackend()
	// The recipe store is the branch's creator on the fork path, so point the fake
	// at the same backend the assertions read (see fakeRecipeStore.backend).
	if fr, ok := prov.Recipes.(*fakeRecipeStore); ok {
		fr.backend = backend
	}
	store := task.NewMemStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return api.NewControllers(backend, task.NewService(store, store.Subtasks()), prov, logger), backend, store
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
func seedWriter(t *testing.T, backend *core.MemBackend, clusterID, branchID uuid.UUID) {
	t.Helper()
	if _, err := backend.CreateEndpoint(context.Background(), clusterID, branchID, core.RoleReadWrite, nil); err != nil {
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

// acceptedClusterTask asserts a direct CreateCluster call returned a 202 + a
// pending create_cluster Task and returns the task's id + cluster id (the task
// stamps the cluster it will create).
func acceptedClusterTask(t *testing.T, resp api.CreateClusterResponseObject) (taskID, clusterID uuid.UUID) {
	t.Helper()
	created, ok := resp.(api.CreateCluster202JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want CreateCluster202JSONResponse", resp)
	}
	body := created.Body
	if body.Kind != "create_cluster" || body.Status != api.TaskStatusPending {
		t.Errorf("task = {kind:%q status:%q}, want {create_cluster pending}", body.Kind, body.Status)
	}
	if want := "/api/v1/tasks/" + body.Id.String(); created.Headers.Location != want {
		t.Errorf("Location = %q, want %q", created.Headers.Location, want)
	}
	if body.ClusterId == nil {
		t.Fatal("task cluster_id = nil, want a real cluster id")
	}
	return body.Id, *body.ClusterId
}

// TestCreateClusterRecordsBranch: with a controller wired,
// create-cluster plans a task that, once driven, records the pending-init recipe
// for the default branch (no ancestor, start LSN 0/0, pg_version 17) keyed by the
// branch id, and does NOT delete it (deletion is deferred to
// create-endpoint). The recipe is now recorded by the record_branch_recipe step.
func TestCreateClusterRecordsBranch(t *testing.T) {
	recipes := &fakeRecipeStore{}
	prov := api.Provisioning{Recipes: recipes}
	c, backend, store := newClusterControllersProv(t, prov)
	poolID := seedUserPool(t, backend)

	resp, err := c.CreateCluster(context.Background(), api.CreateClusterRequestObject{
		PoolId: poolID,
		Body:   &api.CreateClusterJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	_, clusterID := acceptedClusterTask(t, resp)

	// The recipe is recorded by the worker, not synchronously: drive the task.
	drainTasks(t, store, backend, prov)
	if got := soleTask(t, store); got.Status != task.StatusSucceeded {
		t.Fatalf("create_cluster task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}

	// The default branch's id is needed to assert the recipe key — read it back.
	def := getDefaultBranch(t, c, poolID, clusterID)

	if !recipes.recordHit {
		t.Fatal("RecordBranch was not called")
	}
	if recipes.recordedBranch != def.BranchId {
		t.Errorf("recorded branch = %s, want %s", recipes.recordedBranch, def.BranchId)
	}
	if recipes.recordedReq.BranchName != "main" {
		t.Errorf("branch_name = %q, want main", recipes.recordedReq.BranchName)
	}
	if recipes.recordedReq.PgVersion != 18 {
		t.Errorf("pg_version = %d, want 18", recipes.recordedReq.PgVersion)
	}
	if recipes.recordedReq.AncestorBranchID != nil {
		t.Errorf("ancestor_branch_id = %v, want nil (default branch)", recipes.recordedReq.AncestorBranchID)
	}
	if recipes.recordedReq.StartLSN != "0/0" {
		t.Errorf("start_lsn = %q, want 0/0", recipes.recordedReq.StartLSN)
	}
}

// TestCreateClusterNoController: without a controller, the task
// still succeeds (it records the control-plane rows only, no recipe) — no panic.
func TestCreateClusterNoController(t *testing.T) {
	prov := api.Provisioning{}
	c, backend, store := newClusterControllersProv(t, prov)
	poolID := seedUserPool(t, backend)

	resp, err := c.CreateCluster(context.Background(), api.CreateClusterRequestObject{
		PoolId: poolID,
		Body:   &api.CreateClusterJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	acceptedClusterTask(t, resp)
	drainTasks(t, store, backend, prov)
	if got := soleTask(t, store); got.Status != task.StatusSucceeded {
		t.Fatalf("create_cluster task (no SC) status = %q, want succeeded; error = %v", got.Status, got.Error)
	}
}

// TestCreateClusterRecipeError: a recipe-store failure recording the
// recipe now fails the create_cluster TASK (the record_branch_recipe step
// returns the error), not the synchronous handler — which returns 202.
func TestCreateClusterRecipeError(t *testing.T) {
	recipes := &fakeRecipeStore{failRecord: true}
	prov := api.Provisioning{Recipes: recipes}
	c, backend, store := newClusterControllersProv(t, prov)
	poolID := seedUserPool(t, backend)

	resp, err := c.CreateCluster(context.Background(), api.CreateClusterRequestObject{
		PoolId: poolID,
		Body:   &api.CreateClusterJSONRequestBody{Name: "main"},
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	acceptedClusterTask(t, resp)
	drainTasks(t, store, backend, prov)
	got := soleTask(t, store)
	if got.Status != task.StatusFailed || got.Error == nil {
		t.Fatalf("create_cluster task = {status:%q err:%v}, want failed (controller error)", got.Status, got.Error)
	}
}

// soleTaskWithKind returns the single task of the given kind in the store,
// failing if there is not exactly one (the direct-call branch tests create a
// create_cluster task plus the branch task, so they filter by kind).
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

// branchByName lists a cluster's branches through the controller and returns the
// one with the given name, failing if absent.
func branchByName(t *testing.T, c *api.Controllers, poolID, clusterID uuid.UUID, name string) api.Branch {
	t.Helper()
	resp, err := c.ListBranches(context.Background(), api.ListBranchesRequestObject{PoolId: poolID, ClusterId: clusterID})
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
	t.Fatalf("branch %q not found in cluster %s", name, clusterID)
	return api.Branch{}
}

// getDefaultBranch reads a cluster back through the controller and returns its
// default branch.
func getDefaultBranch(t *testing.T, c *api.Controllers, poolID, clusterID uuid.UUID) api.Branch {
	t.Helper()
	resp, err := c.GetCluster(context.Background(), api.GetClusterRequestObject{PoolId: poolID, ClusterId: clusterID})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	got, ok := resp.(api.GetCluster200JSONResponse)
	if !ok {
		t.Fatalf("GetCluster response = %T, want 200", resp)
	}
	return api.Cluster(got).DefaultBranch
}

// mustCreateClusterC creates a cluster + its default branch through the
// controller AND drives the resulting task to completion (recording the default
// branch's recipe too), then returns the materialized cluster, for tests that
// then branch off it. It drains every pending task on the shared store.
func mustCreateClusterC(t *testing.T, c *api.Controllers, store *task.MemStore, backend core.Backend, prov api.Provisioning, poolID uuid.UUID, name string) api.Cluster {
	t.Helper()
	resp, err := c.CreateCluster(context.Background(), api.CreateClusterRequestObject{
		PoolId: poolID,
		Body:   &api.CreateClusterJSONRequestBody{Name: name},
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	_, clusterID := acceptedClusterTask(t, resp)
	drainTasks(t, store, backend, prov)

	gp, err := c.GetCluster(context.Background(), api.GetClusterRequestObject{PoolId: poolID, ClusterId: clusterID})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	got, ok := gp.(api.GetCluster200JSONResponse)
	if !ok {
		t.Fatalf("GetCluster response = %T, want 200", gp)
	}
	return api.Cluster(got)
}

// TestCreateBranchDefaultsForkToFlushLSN: branching with no fork_point_lsn and
// no explicit parent defaults the fork point to the parent's durable WAL end
// (the safekeeper's flush_lsn), read from the parent's read_write endpoint;
// returns a non-default branch carrying the parent + fork LSN, and records a
// branch recipe whose ancestor + start_lsn are set (keyed by the branch's id).
func TestCreateBranchDefaultsForkToFlushLSN(t *testing.T) {
	recipes := &fakeRecipeStore{}
	sk := &fakeSafekeeper{flushLSN: "0/16ABCD0"}
	dep := &fakeDeployer{skURL: sk.start(t)}
	prov := api.Provisioning{Recipes: recipes, Deployer: dep, Node: node.NewClient()}
	c, backend, store := newClusterControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	cluster := mustCreateClusterC(t, c, store, backend, prov, poolID, "main")
	def := cluster.DefaultBranch
	// The parent (default branch) must have a read_write endpoint whose
	// safekeeper holds the fork point.
	seedWriter(t, backend, cluster.Id, def.BranchId)

	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ClusterId: cluster.Id,
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
	br := branchByName(t, c, poolID, cluster.Id, "feature")

	if br.IsDefault {
		t.Error("branch is_default = true, want false")
	}
	if br.ParentBranchId == nil || *br.ParentBranchId != def.BranchId {
		t.Errorf("parent_branch_id = %v, want the default branch %v", br.ParentBranchId, def.BranchId)
	}
	if br.ForkLsn == nil || *br.ForkLsn != "0/16ABCD0" {
		t.Errorf("fork_lsn = %v, want 0/16ABCD0", br.ForkLsn)
	}
	if br.ClusterId != cluster.Id {
		t.Errorf("branch cluster = %v, want %v", br.ClusterId, cluster.Id)
	}
	if br.PgVersion != def.PgVersion {
		t.Errorf("branch pg_version = %d, want inherited %d", br.PgVersion, def.PgVersion)
	}

	if !recipes.recordHit {
		t.Fatal("RecordBranch was not called for the branch")
	}
	if recipes.recordedBranch != br.BranchId {
		t.Errorf("recorded branch = %s, want the branch %s", recipes.recordedBranch, br.BranchId)
	}
	if recipes.recordedReq.AncestorBranchID == nil {
		t.Fatal("recipe ancestor_branch_id = nil, want the parent")
	}
	if *recipes.recordedReq.AncestorBranchID != def.BranchId {
		t.Errorf("recipe ancestor = %s, want the default branch %s", recipes.recordedReq.AncestorBranchID, def.BranchId)
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
	c, backend, store := newClusterControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	cluster := mustCreateClusterC(t, c, store, backend, prov, poolID, "main")

	lsn := "0/ABCDEF1"
	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ClusterId: cluster.Id,
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
	if got := branchByName(t, c, poolID, cluster.Id, "feature").ForkLsn; got == nil || *got != lsn {
		t.Errorf("fork_lsn = %v, want %q", got, lsn)
	}
	if recipes.recordedReq.StartLSN != lsn {
		t.Errorf("recipe start_lsn = %q, want %q", recipes.recordedReq.StartLSN, lsn)
	}
	if want := cluster.DefaultBranch.BranchId; recipes.recordedReq.AncestorBranchID == nil || *recipes.recordedReq.AncestorBranchID != want {
		t.Errorf("recipe ancestor = %v, want the default branch %s", recipes.recordedReq.AncestorBranchID, want)
	}
}

// TestCreateBranchInvalidForkLSN: a malformed fork_point_lsn 400s before any
// task is planned (cheap pre-validation).
func TestCreateBranchInvalidForkLSN(t *testing.T) {
	recipes := &fakeRecipeStore{}
	prov := api.Provisioning{Recipes: recipes}
	c, backend, store := newClusterControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	cluster := mustCreateClusterC(t, c, store, backend, prov, poolID, "main")

	bad := "not-an-lsn"
	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ClusterId: cluster.Id,
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
// cluster (here, the default branch's own name) is no longer a synchronous 409 —
// the create is accepted (202) and the conflict surfaces as a FAILED branch task
// when its create_branch step's EnsureBranch hits the name index. The fork point
// resolves first (a cheap pre-check), so the parent still needs a read_write
// endpoint + safekeeper.
func TestCreateBranchDuplicateName(t *testing.T) {
	recipes := &fakeRecipeStore{}
	sk := &fakeSafekeeper{flushLSN: "0/10"}
	dep := &fakeDeployer{skURL: sk.start(t)}
	prov := api.Provisioning{Recipes: recipes, Deployer: dep, Node: node.NewClient()}
	c, backend, store := newClusterControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	cluster := mustCreateClusterC(t, c, store, backend, prov, poolID, "main")
	seedWriter(t, backend, cluster.Id, cluster.DefaultBranch.BranchId)

	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ClusterId: cluster.Id,
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

// TestCreateBranchNoController: without a controller there is no way to
// resolve a default fork point (and no recipe to materialize the branch), so a
// branch with no fork_point_lsn is rejected (400) rather than silently forking
// at 0/0; an explicit fork_point_lsn still works and is recorded on the row.
func TestCreateBranchNoController(t *testing.T) {
	prov := api.Provisioning{}
	c, backend, store := newClusterControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	cluster := mustCreateClusterC(t, c, store, backend, prov, poolID, "main")

	// No fork point + no controller -> 400 (no silent 0/0); cheap pre-validation.
	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ClusterId: cluster.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature"},
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if _, ok := resp.(api.CreateBranch400JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateBranch400JSONResponse (no SC, no fork_point_lsn)", resp)
	}

	// Explicit fork point + no store -> still 202 (the handler validated what it
	// can), but the TASK fails: record_branch_recipe is what declares the branch,
	// so without a store there is no branch to create and the step says so rather
	// than reporting success having done nothing.
	lsn := "0/ABCDEF1"
	resp, err = c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ClusterId: cluster.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature", ForkPointLsn: &lsn},
	})
	if err != nil {
		t.Fatalf("CreateBranch(explicit): %v", err)
	}
	if _, ok := resp.(api.CreateBranch202JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateBranch202JSONResponse", resp)
	}
	drainTasks(t, store, backend, prov)
	if got := soleTaskWithKind(t, store, "branch"); got.Status != task.StatusFailed {
		t.Fatalf("branch task status = %q, want failed (no storage-layer store)", got.Status)
	}

	// And with a store wired, the same request declares the branch and carries the
	// fork LSN — the task's one step does both.
	provWithStore := api.Provisioning{Recipes: &fakeRecipeStore{}}
	c2, backend2, store2 := newClusterControllersProv(t, provWithStore)
	poolID2 := seedUserPool(t, backend2)
	cluster2 := mustCreateClusterC(t, c2, store2, backend2, provWithStore, poolID2, "main")
	resp, err = c2.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID2,
		ClusterId: cluster2.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature", ForkPointLsn: &lsn},
	})
	if err != nil {
		t.Fatalf("CreateBranch(with store): %v", err)
	}
	if _, ok := resp.(api.CreateBranch202JSONResponse); !ok {
		t.Fatalf("response = %T, want CreateBranch202JSONResponse", resp)
	}
	drainTasks(t, store2, backend2, provWithStore)
	if got := soleTaskWithKind(t, store2, "branch"); got.Status != task.StatusSucceeded {
		t.Fatalf("branch task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}
	if got := branchByName(t, c2, poolID2, cluster2.Id, "feature").ForkLsn; got == nil || *got != lsn {
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
	c, backend, store := newClusterControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	cluster := mustCreateClusterC(t, c, store, backend, prov, poolID, "main")
	// No endpoint seeded on the default branch.

	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ClusterId: cluster.Id,
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
	c, backend, store := newClusterControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	cluster := mustCreateClusterC(t, c, store, backend, prov, poolID, "main")
	seedWriter(t, backend, cluster.Id, cluster.DefaultBranch.BranchId)

	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ClusterId: cluster.Id,
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

// TestCreateBranchDefaultForkSafekeeperUnreachable: a parent whose safekeeper
// never answers gets 503, not 400 — the fork is well-formed and retrying is the
// caller's correct reaction, which invalid_request tells it not to do (#724).
func TestCreateBranchDefaultForkSafekeeperUnreachable(t *testing.T) {
	recipes := &fakeRecipeStore{}
	// Port 1 needs root to bind, so nothing is listening: the dial is refused.
	dep := &fakeDeployer{skURL: "http://127.0.0.1:1"}
	prov := api.Provisioning{Recipes: recipes, Deployer: dep, Node: node.NewClient()}
	c, backend, store := newClusterControllersProv(t, prov)
	poolID := seedUserPool(t, backend)
	cluster := mustCreateClusterC(t, c, store, backend, prov, poolID, "main")
	seedWriter(t, backend, cluster.Id, cluster.DefaultBranch.BranchId)

	resp, err := c.CreateBranch(context.Background(), api.CreateBranchRequestObject{
		PoolId:    poolID,
		ClusterId: cluster.Id,
		Body:      &api.CreateBranchJSONRequestBody{Name: "feature"},
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	got, ok := resp.(api.CreateBranch503JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want CreateBranch503JSONResponse (parent not serving yet)", resp)
	}
	if got.Body.Error.Code != "service_unavailable" {
		t.Errorf("error code = %q, want service_unavailable", got.Body.Error.Code)
	}
	if got.Headers.RetryAfter <= 0 {
		t.Errorf("Retry-After = %d, want a positive number of seconds", got.Headers.RetryAfter)
	}
	// A branch whose fork point never resolved must not have been planned — which
	// is what makes the caller's retry safe.
	branches, _, err := backend.ListBranches(context.Background(), poolID, cluster.Id, 100, 0)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	for _, b := range branches {
		if b.Name == "feature" {
			t.Fatal("the rejected fork was recorded anyway; retrying it would not be safe")
		}
	}
}
