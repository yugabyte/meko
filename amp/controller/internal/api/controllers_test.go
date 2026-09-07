// These tests drive the controllers through the *generated* Go client
// (api.ClientWithResponses) over a real httptest server, with the backend
// stubbed by core.MemBackend and the in-memory task.MemStore. They validate that the
// controllers are wired to the spec correctly — routing, path/body binding,
// status codes, and response shapes — without a database.
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/config"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/task"
)

// drainTasks runs a real task.Worker over the same store + backend the server
// uses, until every task has reached a terminal status (succeeded/failed). The
// worker is wired with ProductionRegistry over the test's provisioning (its
// fakeDeployer, fakeSCServer-backed recipe store, etc.), so a drained create task
// materializes its rows/recipe/endpoint exactly as production would and a delete
// task runs its cascade. backend must satisfy task.StateStore (core.MemBackend
// and the failing wrappers around it do). It returns once nothing is left
// runnable, so callers can then GET/list the materialized resource (or assert a
// failed task). Safe to call when there are no tasks (it returns promptly).
func drainTasks(t *testing.T, store *task.MemStore, backend core.Backend, prov api.Provisioning) {
	t.Helper()
	state, ok := backend.(task.StateStore)
	if !ok {
		t.Fatalf("backend %T does not satisfy task.StateStore", backend)
	}
	deps := task.Deps{
		State:         state,
		Deployer:      prov.Deployer,
		EndpointYB:    prov.EndpointYB,
		ControllerURL: prov.ControllerURL,
		Recipes:       prov.Recipes,
		Extensions:    prov.Extensions,
	}
	cfg := config.TaskConfig{
		PollInterval:  time.Millisecond,
		LeaseTTL:      30 * time.Second,
		RenewInterval: 10 * time.Second,
		MaxConcurrent: 4,
		ShutdownGrace: 2 * time.Second,
		LockWait:      10 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := task.NewWorker(store, store.Subtasks(), task.ProductionRegistry(deps), store.Locks("api-test"), cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// Wait until every task is terminal (the steps are idempotent and there are no
	// inter-task dependencies, so the backlog drains in one pass), then stop the
	// worker.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if allTasksTerminal(t, store) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drainTasks: tasks did not reach a terminal status within 5s")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainTasks: worker did not stop after cancel")
	}
}

// allTasksTerminal reports whether every task in the store is succeeded or
// failed (a generous page covers the small task counts the api tests create).
func allTasksTerminal(t *testing.T, store *task.MemStore) bool {
	t.Helper()
	tasks, _, err := store.List(context.Background(), task.TaskFilter{}, 1000, 0)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	for _, tk := range tasks {
		if tk.Status != task.StatusSucceeded && tk.Status != task.StatusFailed {
			return false
		}
	}
	return true
}

// soleTask returns the single task in the store, failing if there is not exactly
// one — convenient for the create/delete tests that issue one mutation and then
// inspect the resulting task's terminal status.
func soleTask(t *testing.T, store *task.MemStore) *task.Task {
	t.Helper()
	tasks, total, err := store.List(context.Background(), task.TaskFilter{}, 1000, 0)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if total != 1 || len(tasks) != 1 {
		t.Fatalf("task count = %d, want exactly 1", total)
	}
	return tasks[0]
}

// newTestServer stands up the real server (NewServer) over httptest, backed by
// an in-memory backend and an in-memory task store, and returns the generated
// client pointed at it.
func newTestServer(t *testing.T) (*api.ClientWithResponses, *core.MemBackend, *task.MemStore) {
	// Deploy enabled by default so the endpoint routes are live (a fake deployer
	// returns a derived address without touching a real runtime).
	return newTestServerDeploy(t, true)
}

func newTestServerDeploy(t *testing.T, deployEnabled bool) (*api.ClientWithResponses, *core.MemBackend, *task.MemStore) {
	t.Helper()
	var prov api.Provisioning
	if deployEnabled {
		prov.Deployer = &fakeDeployer{}
	}
	return newTestServerWith(t, core.NewMemBackend(), prov)
}

// newTestServerWithProv builds the server over a fresh MemBackend with the
// given provisioning (a nil Deployer disables the endpoint mutation routes),
// returning the client and the backend.
func newTestServerWithProv(t *testing.T, prov api.Provisioning) (*api.ClientWithResponses, *core.MemBackend) {
	client, backend, _ := newTestServerWith(t, core.NewMemBackend(), prov)
	return client, backend
}

// newTestServerWith builds the server over an explicit backend + provisioning
// (used to inject a failing backend / deployer), returning the client, the
// backend, and the task store wired into the server.
func newTestServerWith(t *testing.T, backend *core.MemBackend, prov api.Provisioning) (*api.ClientWithResponses, *core.MemBackend, *task.MemStore) {
	t.Helper()
	client, tasks := newTestServerBackend(t, backend, prov)
	return client, backend, tasks
}

// newTestServerRaw is newTestServerBackend plus the base URL, for a test that
// has to send a body the generated client cannot express — an omitted field is
// the obvious one, since the client sends what its struct holds and a server
// that filled the gap in from an OpenAPI default would be invisible to it.
func newTestServerRaw(t *testing.T, backend *core.MemBackend, prov api.Provisioning) (*api.ClientWithResponses, *task.MemStore, string) {
	t.Helper()
	tasks := task.NewMemStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{ListenAddr: ":0"}
	cfg.Deploy.Enabled = prov.Deployer != nil
	srv := api.NewServer(cfg, backend, task.NewService(tasks, tasks.Subtasks()), prov, nil, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client, err := api.NewClientWithResponses(ts.URL + "/api/v1")
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return client, tasks, ts.URL + "/api/v1"
}

// newTestServerBackend builds the server over any core.Backend (so a failing
// backend can be injected), returning the client and the wired task store.
func newTestServerBackend(t *testing.T, backend core.Backend, prov api.Provisioning) (*api.ClientWithResponses, *task.MemStore) {
	t.Helper()
	tasks := task.NewMemStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{ListenAddr: ":0"}
	cfg.Deploy.Enabled = prov.Deployer != nil
	srv := api.NewServer(cfg, backend, task.NewService(tasks, tasks.Subtasks()), prov, nil, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client, err := api.NewClientWithResponses(ts.URL + "/api/v1")
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return client, tasks
}

// assertTaskAccepted asserts a 202 response carried a pending Task of the
// expected kind with the Location header pointing at it. It is the shared check
// for the "mutation returns a task" contract. body is the decoded Task
// (resp.JSON202) and locHeader is resp.HTTPResponse.Header.Get("Location").
func assertTaskAccepted(t *testing.T, body *api.Task, locHeader, wantKind string) {
	t.Helper()
	if body == nil {
		t.Fatalf("202 response has no Task body")
	}
	if body.Id == uuid.Nil {
		t.Errorf("task has nil id")
	}
	if body.Kind != wantKind {
		t.Errorf("task kind = %q, want %q", body.Kind, wantKind)
	}
	if body.Status != api.TaskStatusPending {
		t.Errorf("task status = %q, want pending (not yet driven by a worker)", body.Status)
	}
	if want := "/api/v1/tasks/" + body.Id.String(); locHeader != want {
		t.Errorf("Location header = %q, want %q", locHeader, want)
	}
}

func mustCreateUser(t *testing.T, client *api.ClientWithResponses, isAdmin bool) api.User {
	t.Helper()
	resp, err := client.CreateUserWithResponse(context.Background(), api.CreateUserRequest{IsAdmin: &isAdmin})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if resp.StatusCode() != http.StatusCreated || resp.JSON201 == nil {
		t.Fatalf("CreateUser status = %d, body = %s", resp.StatusCode(), resp.Body)
	}
	return *resp.JSON201
}

func TestCreateAndGetUser(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()

	u := mustCreateUser(t, client, true)
	if u.Id == uuid.Nil {
		t.Fatal("created user has nil id")
	}
	if !u.IsAdmin {
		t.Errorf("IsAdmin = false, want true")
	}

	got, err := client.GetUserWithResponse(ctx, u.Id)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.StatusCode() != http.StatusOK || got.JSON200 == nil {
		t.Fatalf("GetUser status = %d, body = %s", got.StatusCode(), got.Body)
	}
	if got.JSON200.Id != u.Id {
		t.Errorf("GetUser id = %s, want %s", got.JSON200.Id, u.Id)
	}

	missing, err := client.GetUserWithResponse(ctx, uuid.New())
	if err != nil {
		t.Fatalf("GetUser(missing): %v", err)
	}
	if missing.StatusCode() != http.StatusNotFound || missing.JSON404 == nil {
		t.Fatalf("GetUser(missing) status = %d, body = %s", missing.StatusCode(), missing.Body)
	}
	if missing.JSON404.Error.Code != "not_found" {
		t.Errorf("error code = %q, want not_found", missing.JSON404.Error.Code)
	}
}

func TestCreateStoragePoolStatusCodes(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)

	// Empty name -> 400.
	bad, err := client.CreateStoragePoolWithResponse(ctx, api.CreateStoragePoolRequest{Name: "  ", OwnerUserId: u.Id})
	if err != nil {
		t.Fatalf("CreateStoragePool(empty): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest || bad.JSON400 == nil {
		t.Fatalf("empty name status = %d, body = %s", bad.StatusCode(), bad.Body)
	}

	// Missing owner_user_id -> 400.
	noOwner, err := client.CreateStoragePoolWithResponse(ctx, api.CreateStoragePoolRequest{Name: "p1"})
	if err != nil {
		t.Fatalf("CreateStoragePool(no owner): %v", err)
	}
	if noOwner.StatusCode() != http.StatusBadRequest || noOwner.JSON400 == nil {
		t.Fatalf("missing owner_user_id status = %d, body = %s", noOwner.StatusCode(), noOwner.Body)
	}

	// Unknown owner -> 404.
	missing, err := client.CreateStoragePoolWithResponse(ctx, api.CreateStoragePoolRequest{Name: "p1", OwnerUserId: uuid.New()})
	if err != nil {
		t.Fatalf("CreateStoragePool(missing user): %v", err)
	}
	if missing.StatusCode() != http.StatusNotFound || missing.JSON404 == nil {
		t.Fatalf("missing owner status = %d, body = %s", missing.StatusCode(), missing.Body)
	}

	// Valid -> 201, owned by the user.
	ok, err := client.CreateStoragePoolWithResponse(ctx, api.CreateStoragePoolRequest{Name: "p1", OwnerUserId: u.Id})
	if err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	if ok.StatusCode() != http.StatusCreated || ok.JSON201 == nil {
		t.Fatalf("create status = %d, body = %s", ok.StatusCode(), ok.Body)
	}
	if ok.JSON201.OwnerUserId != u.Id {
		t.Errorf("owner = %s, want %s", ok.JSON201.OwnerUserId, u.Id)
	}
	if ok.JSON201.Name != "p1" {
		t.Errorf("name = %q, want p1", ok.JSON201.Name)
	}

	// Duplicate name -> 409.
	dup, err := client.CreateStoragePoolWithResponse(ctx, api.CreateStoragePoolRequest{Name: "p1", OwnerUserId: u.Id})
	if err != nil {
		t.Fatalf("CreateStoragePool(dup): %v", err)
	}
	if dup.StatusCode() != http.StatusConflict || dup.JSON409 == nil {
		t.Fatalf("dup status = %d, body = %s", dup.StatusCode(), dup.Body)
	}
}

func TestCreateStoragePoolTrimsName(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)

	// Surrounding whitespace is trimmed before the pool is stored.
	resp, err := client.CreateStoragePoolWithResponse(ctx, api.CreateStoragePoolRequest{Name: "  padded  ", OwnerUserId: u.Id})
	if err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	if resp.StatusCode() != http.StatusCreated || resp.JSON201 == nil {
		t.Fatalf("create status = %d, body = %s", resp.StatusCode(), resp.Body)
	}
	if resp.JSON201.Name != "padded" {
		t.Errorf("stored name = %q, want %q (trimmed)", resp.JSON201.Name, "padded")
	}
}

func TestCreateCluster(t *testing.T) {
	client, backend, tasks := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	prov := api.Provisioning{Deployer: &fakeDeployer{}}

	// Create cluster -> 202 + a pending create_cluster Task with a Location header.
	// The cluster carries the chosen name; its default branch is always "main"
	// (independent of the cluster name), so use a distinct cluster name here.
	pgv := 18
	cp, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{Name: "shop", PgVersion: &pgv})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if cp.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateCluster status = %d, want 202, body = %s", cp.StatusCode(), cp.Body)
	}
	assertTaskAccepted(t, cp.JSON202, cp.HTTPResponse.Header.Get("Location"), "create_cluster")
	if cp.JSON202.ClusterId == nil || *cp.JSON202.ClusterId == uuid.Nil {
		t.Errorf("task cluster_id = %v, want a real cluster id", cp.JSON202.ClusterId)
	}
	clusterID := *cp.JSON202.ClusterId

	// Drive the task to completion, then the cluster + its default branch are
	// materialized: GET the cluster and assert the auto-created default branch.
	drainTasks(t, tasks, backend, prov)
	if got := soleTask(t, tasks); got.Status != task.StatusSucceeded {
		t.Fatalf("create_cluster task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}
	gp, err := client.GetClusterWithResponse(ctx, pool.Id, clusterID)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if gp.StatusCode() != http.StatusOK || gp.JSON200 == nil {
		t.Fatalf("GetCluster status = %d, body = %s", gp.StatusCode(), gp.Body)
	}
	p := *gp.JSON200
	if p.Name != "shop" {
		t.Errorf("cluster name = %q, want %q", p.Name, "shop")
	}
	def := p.DefaultBranch
	if def.ClusterId != p.Id || def.BranchId == uuid.Nil {
		t.Errorf("default branch ids = (%s,%s), want cluster %s and a non-nil branch", def.ClusterId, def.BranchId, p.Id)
	}
	if !def.IsDefault || def.Name != "main" || def.PgVersion != 18 {
		t.Errorf("default branch = %+v, want {is_default name=main pg_version=18}", def)
	}

	// Empty name -> 400 (cheap validation, before any task is planned).
	cpBad, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{Name: ""})
	if err != nil {
		t.Fatalf("CreateCluster(empty): %v", err)
	}
	if cpBad.StatusCode() != http.StatusBadRequest {
		t.Fatalf("CreateCluster(empty) status = %d, body = %s", cpBad.StatusCode(), cpBad.Body)
	}

	// Duplicate name in the same pool: under the async contract this is no longer a
	// synchronous 409. The mutation is accepted (202) and the duplicate surfaces as
	// a FAILED task when its create_cluster step's EnsureCluster hits the conflict.
	dup, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{Name: "shop"})
	if err != nil {
		t.Fatalf("CreateCluster(dup): %v", err)
	}
	if dup.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateCluster(dup) status = %d, want 202, body = %s", dup.StatusCode(), dup.Body)
	}
	dupID := dup.JSON202.Id
	drainTasks(t, tasks, backend, prov)
	dupTask, err := tasks.Get(ctx, dupID)
	if err != nil || dupTask == nil {
		t.Fatalf("get dup task: task=%v err=%v", dupTask, err)
	}
	if dupTask.Status != task.StatusFailed || dupTask.Error == nil {
		t.Errorf("duplicate-name task = {status:%q err:%v}, want failed with an error", dupTask.Status, dupTask.Error)
	}

	// Unknown pool -> 404 (cheap pre-check: the pool must exist before planning).
	missing, err := client.CreateClusterWithResponse(ctx, uuid.New(), api.CreateClusterRequest{Name: "x"})
	if err != nil {
		t.Fatalf("CreateCluster(missing pool): %v", err)
	}
	if missing.StatusCode() != http.StatusNotFound || missing.JSON404 == nil {
		t.Fatalf("CreateCluster(missing pool) status = %d, body = %s", missing.StatusCode(), missing.Body)
	}

	// Unsupported pg_version -> 400 (yb-amp is v18 only; cheap validation).
	badVer := 15
	bv, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{Name: "pg15", PgVersion: &badVer})
	if err != nil {
		t.Fatalf("CreateCluster(bad version): %v", err)
	}
	if bv.StatusCode() != http.StatusBadRequest {
		t.Fatalf("CreateCluster(pg_version=15) status = %d, want 400, body = %s", bv.StatusCode(), bv.Body)
	}
}

// TestCreateClusterWithWriter covers the composite "create cluster": with_writer
// makes the single create_cluster task also deploy the default branch's
// read_write endpoint, sized to resources. Guards that the writer is (a) created
// and (b) sized to the request — and that a bad resource quantity is rejected
// synchronously with 400.
func TestCreateClusterWithWriter(t *testing.T) {
	client, backend, tasks := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}

	withWriter := true
	cpu, mem := "4", "32Gi"
	cp, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name:       "shop",
		WithWriter: &withWriter,
		Resources:  &api.EndpointResources{Cpu: &cpu, Memory: &mem},
	})
	if err != nil {
		t.Fatalf("CreateCluster(with_writer): %v", err)
	}
	if cp.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateCluster(with_writer) status = %d, want 202, body = %s", cp.StatusCode(), cp.Body)
	}

	// One task (create_cluster) with an appended create_endpoint step; drive it.
	drainTasks(t, tasks, backend, prov)
	if got := soleTask(t, tasks); got.Status != task.StatusSucceeded {
		t.Fatalf("create_cluster(with_writer) task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}

	// Exactly one endpoint (the writer) must have been deployed, sized to the
	// requested resources (not the deployer default).
	if len(dep.created) != 1 {
		t.Fatalf("deployed endpoints = %d, want 1 (the writer)", len(dep.created))
	}
	if dep.created[0].CPU != cpu || dep.created[0].Memory != mem {
		t.Errorf("writer deploy resources = (%q, %q), want (%q, %q)", dep.created[0].CPU, dep.created[0].Memory, cpu, mem)
	}

	// with_writer + an invalid cpu quantity -> synchronous 400 (before planning).
	badCPU := "not-a-quantity"
	bad, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name:       "shop2",
		WithWriter: &withWriter,
		Resources:  &api.EndpointResources{Cpu: &badCPU},
	})
	if err != nil {
		t.Fatalf("CreateCluster(bad resources): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest {
		t.Fatalf("CreateCluster(bad resources) status = %d, want 400, body = %s", bad.StatusCode(), bad.Body)
	}
}

// fakeDeployer is a deploy.Deployer that records calls and returns a derived
// address, so the synchronous endpoint deploy/retire wiring can be tested
// without a real runtime. createErr, when set, makes CreateEndpoint fail (to
// exercise the handler's compensating row delete).
type fakeDeployer struct {
	// mu guards the recorded-call slices: drainTasks runs the worker with
	// MaxConcurrent > 1, so steps (hence these deployer methods) can be invoked
	// from several goroutines at once. Without it, the parallel-resize test races.
	mu   sync.Mutex
	addr string
	// version stands in for the deployer's configured endpoint version (the
	// Kubernetes deployer's --endpoint-image-tag): what CreateEndpoint reports
	// having run when the spec asks for nothing. Empty = a deployer with no
	// version to report.
	version   string
	skURL     string // safekeeper base URL EndpointSafekeeperURL returns (fork-point resolution)
	supURL    string // supervisor settings API base URL; empty = unreachable
	created   []deploy.EndpointSpec
	deleted   []string
	resized   []deploy.ResizeSpec
	createErr error
	// resizeOutcome overrides the reported in-place outcome (default: completed);
	// resizeErr, when set, makes ResizeEndpoint fail (exercises a failed task).
	resizeOutcome deploy.ResizeOutcome
	resizeErr     error
}

func (f *fakeDeployer) CreateEndpoint(_ context.Context, spec deploy.EndpointSpec) (deploy.Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, spec)
	if f.createErr != nil {
		return deploy.Endpoint{}, f.createErr
	}
	addr := f.addr
	if addr == "" {
		addr = spec.Name + ".ns.svc.cluster.local:55433"
	}
	// Report the version actually run: the spec's when it asked for one, else the
	// deployer's own default — the resolution every real deployer performs.
	version := spec.Version
	if version == "" {
		version = f.version
	}
	return deploy.Endpoint{ID: spec.Name, Address: addr, Version: version}, nil
}

func (f *fakeDeployer) DeleteEndpoint(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeDeployer) ResizeEndpoint(_ context.Context, spec deploy.ResizeSpec) (deploy.ResizeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resized = append(f.resized, spec)
	if f.resizeErr != nil {
		return deploy.ResizeResult{}, f.resizeErr
	}
	outcome := f.resizeOutcome
	if outcome == "" {
		outcome = deploy.ResizeCompleted
	}
	return deploy.ResizeResult{Outcome: outcome, Message: string(outcome)}, nil
}

// resizedSpecs returns a copy of the recorded resize calls under the lock, so
// tests can read them without racing the worker goroutines.
func (f *fakeDeployer) resizedSpecs() []deploy.ResizeSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]deploy.ResizeSpec(nil), f.resized...)
}

func (f *fakeDeployer) EndpointSafekeeperURL(string) string {
	return f.skURL
}

// supURL is the settings API the settings handlers POST to. Empty by default,
// which the handlers report as "cannot be told" rather than treating as a
// failure — the settings are stored either way.
func (f *fakeDeployer) EndpointSupervisorURL(string) string {
	return f.supURL
}

// TestCreateEndpointDeployFailure: under the async contract the handler no longer
// compensates with a synchronous 500 — it accepts the mutation (202) and the
// deploy failure surfaces as a FAILED create_endpoint task. The step records the
// endpoint row before deploying (it claims the single-writer slot); on a deploy
// failure that record stays (the step is idempotent and converges on retry), but
// no address is recorded — so the endpoint never becomes usable. We assert the
// task failed; the leaked-row contract is exercised by the address-record case
// below where the writer slot must be freed for a retry.
func TestCreateEndpointDeployFailure(t *testing.T) {
	dep := &fakeDeployer{createErr: errors.New("simulated deploy failure")}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", created.StatusCode(), created.Body)
	}
	drainTasks(t, store, backend, prov)
	got := soleTask(t, store)
	if got.Status != task.StatusFailed || got.Error == nil {
		t.Fatalf("create_endpoint task = {status:%q err:%v}, want failed (deploy error)", got.Status, got.Error)
	}
	if len(dep.created) < 1 {
		t.Fatalf("CreateEndpoint deploy calls = %d, want at least 1 (the step attempted the deploy)", len(dep.created))
	}
	// No address was recorded, so the endpoint never became usable.
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.JSON200 == nil {
		t.Fatalf("ListEndpoints body = %s", list.Body)
	}
	for _, ep := range list.JSON200.Endpoints {
		if ep.Address != "" {
			t.Errorf("endpoint %s has address %q after a failed deploy, want none", ep.Id, ep.Address)
		}
	}
}

// addrFailBackend makes SetEndpointAddress fail while delegating everything else
// to MemBackend, to exercise the failure when a deploy succeeds but its address
// can't be recorded. It delegates the task.StateStore surface to the embedded
// MemBackend (overriding only SetEndpointAddress) so the worker can drive against it.
type addrFailBackend struct {
	*core.MemBackend
}

func (addrFailBackend) SetEndpointAddress(context.Context, uuid.UUID, string, string) error {
	return errors.New("simulated address-record failure")
}

// After a successful deploy, a failure recording the address fails the
// create_endpoint task. The step is idempotent: the endpoint record persists (so
// a retry converges) but, because the address never lands, the endpoint stays
// address-less. We assert the task failed and the deploy was attempted.
func TestCreateEndpointAddressRecordFailure(t *testing.T) {
	mem := core.NewMemBackend()
	be := addrFailBackend{mem}
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, store := newTestServerBackend(t, be, prov)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	clusterID := uuid.New()
	branchID := uuid.New()
	mem.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", created.StatusCode(), created.Body)
	}
	drainTasks(t, store, be, prov)
	got := soleTask(t, store)
	if got.Status != task.StatusFailed || got.Error == nil {
		t.Fatalf("create_endpoint task = {status:%q err:%v}, want failed (address-record error)", got.Status, got.Error)
	}
	// The deploy was attempted (the address-record is what failed).
	if len(dep.created) < 1 {
		t.Fatalf("deployer created = %d, want at least 1", len(dep.created))
	}
}

// TestClusterDeleteCascade: delete-cluster is a real async cascade now. Create a
// cluster (with a deployed endpoint on its default branch), delete it -> 202 +
// delete_cluster Task; drive the task; the cluster, its branch, and the endpoint
// are all gone, and the running instance was retired.
func TestClusterDeleteCascade(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	// Seed a cluster with a default branch + a second branch, each carrying a
	// read_write endpoint, so the cascade has multiple branches and endpoints to
	// retire and we can assert no branch survives.
	clusterID := uuid.New()
	branchID := uuid.New()
	branch2ID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branch2ID, Name: "feature", PgVersion: 18})
	if _, err := backend.CreateEndpoint(ctx, clusterID, branchID, core.RoleReadWrite, nil); err != nil {
		t.Fatalf("seed endpoint (main): %v", err)
	}
	if _, err := backend.CreateEndpoint(ctx, clusterID, branch2ID, core.RoleReadWrite, nil); err != nil {
		t.Fatalf("seed endpoint (feature): %v", err)
	}

	delP, err := client.DeleteClusterWithResponse(ctx, pool.Id, clusterID)
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if delP.StatusCode() != http.StatusAccepted {
		t.Fatalf("DeleteCluster status = %d, want 202, body = %s", delP.StatusCode(), delP.Body)
	}
	assertTaskAccepted(t, delP.JSON202, delP.HTTPResponse.Header.Get("Location"), "delete_cluster")

	drainTasks(t, store, backend, prov)
	if got := soleTask(t, store); got.Status != task.StatusSucceeded {
		t.Fatalf("delete_cluster task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}

	// The cluster is gone.
	gp, err := client.GetClusterWithResponse(ctx, pool.Id, clusterID)
	if err != nil {
		t.Fatalf("GetCluster(after delete): %v", err)
	}
	if gp.StatusCode() != http.StatusNotFound {
		t.Fatalf("GetCluster(after delete) status = %d, want 404", gp.StatusCode())
	}
	// Both endpoint instances were retired.
	if len(dep.deleted) != 2 {
		t.Errorf("deployer retire calls = %d, want 2 (both cascaded endpoints)", len(dep.deleted))
	}
	// No branch row survives for the cluster.
	if ids, err := backend.ChildBranchIDs(ctx, clusterID, branchID); err != nil {
		t.Fatalf("ChildBranchIDs: %v", err)
	} else if len(ids) != 0 {
		t.Errorf("child branches after cascade = %d, want 0", len(ids))
	}
	for _, bid := range []uuid.UUID{branchID, branch2ID} {
		if _, err := backend.GetBranch(ctx, pool.Id, clusterID, bid); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("GetBranch(%s) after cascade err = %v, want ErrNotFound (branch row gone)", bid, err)
		}
	}

	// Deleting an unknown cluster -> 404 (cheap pre-check).
	miss, err := client.DeleteClusterWithResponse(ctx, pool.Id, uuid.New())
	if err != nil {
		t.Fatalf("DeleteCluster(missing): %v", err)
	}
	if miss.StatusCode() != http.StatusNotFound {
		t.Fatalf("DeleteCluster(missing) status = %d, want 404", miss.StatusCode())
	}
}

// TestCreateBranchValidation keeps the cheap create-branch validation that stays
// synchronous: an empty name -> 400, a nonexistent cluster -> 404. The happy path
// (a real cluster + the recipe) lives in cluster_storage_test.go.
func TestCreateBranchValidation(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	brBad, err := client.CreateBranchWithResponse(ctx, pool.Id, uuid.New(), api.CreateBranchRequest{Name: ""})
	if err != nil {
		t.Fatalf("CreateBranch(empty): %v", err)
	}
	if brBad.StatusCode() != http.StatusBadRequest {
		t.Fatalf("CreateBranch(empty) status = %d, body = %s", brBad.StatusCode(), brBad.Body)
	}
	br, err := client.CreateBranchWithResponse(ctx, pool.Id, uuid.New(), api.CreateBranchRequest{Name: "feature"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if br.StatusCode() != http.StatusNotFound {
		t.Fatalf("CreateBranch(missing cluster) status = %d, body = %s", br.StatusCode(), br.Body)
	}
}

func TestGetTask(t *testing.T) {
	client, _, tasks := newTestServer(t)
	ctx := context.Background()

	// Unknown task -> 404.
	missing, err := client.GetTaskWithResponse(ctx, uuid.New())
	if err != nil {
		t.Fatalf("GetTask(missing): %v", err)
	}
	if missing.StatusCode() != http.StatusNotFound || missing.JSON404 == nil {
		t.Fatalf("GetTask(missing) status = %d, body = %s", missing.StatusCode(), missing.Body)
	}

	// Seeded task -> 200 with the right kind/status.
	id := uuid.New()
	tasks.Seed(&task.Task{
		ID:        id,
		Kind:      "create_cluster",
		Status:    task.StatusRunning,
		Params:    json.RawMessage(`{"name":"db1"}`),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}, nil)
	got, err := client.GetTaskWithResponse(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.StatusCode() != http.StatusOK || got.JSON200 == nil {
		t.Fatalf("GetTask status = %d, body = %s", got.StatusCode(), got.Body)
	}
	if got.JSON200.Kind != "create_cluster" || got.JSON200.Status != api.TaskStatusRunning {
		t.Errorf("task = {kind:%q status:%q}, want {create_cluster running}", got.JSON200.Kind, got.JSON200.Status)
	}
}

// newFakeTaskService wraps a fresh in-memory task store in a task.Service, for
// tests that build a server but don't need a handle on the underlying store.
func newFakeTaskService() *task.Service {
	store := task.NewMemStore()
	return task.NewService(store, store.Subtasks())
}

// --- per-mutation "returns a task" tests ----------------------------------
// One focused test per async mutation: it returns 202 + a pending Task of the
// right Kind with a Location header, WITHOUT driving the worker (the cheap
// contract check; the materialization is covered by the drive-the-worker tests).

func TestCreateClusterReturnsTask(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	resp, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{Name: "shop"})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode(), resp.Body)
	}
	assertTaskAccepted(t, resp.JSON202, resp.HTTPResponse.Header.Get("Location"), "create_cluster")
}

func TestCreateBranchReturnsTask(t *testing.T) {
	// An explicit fork point lets a branch be planned without a controller
	// or a deployed parent endpoint (the cheap fork-point resolution succeeds).
	client, backend := newTestServerWithProv(t, api.Provisioning{Deployer: &fakeDeployer{}})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	lsn := "0/16ABCD0"
	resp, err := client.CreateBranchWithResponse(ctx, pool.Id, clusterID, api.CreateBranchRequest{Name: "feature", ForkPointLsn: &lsn})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode(), resp.Body)
	}
	assertTaskAccepted(t, resp.JSON202, resp.HTTPResponse.Header.Get("Location"), "branch")
}

func TestCreateEndpointReturnsTask(t *testing.T) {
	client, backend := newTestServerWithProv(t, api.Provisioning{Deployer: &fakeDeployer{}})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	resp, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode(), resp.Body)
	}
	assertTaskAccepted(t, resp.JSON202, resp.HTTPResponse.Header.Get("Location"), "create_endpoint")
}

func TestDeleteEndpointReturnsTask(t *testing.T) {
	client, backend := newTestServerWithProv(t, api.Provisioning{Deployer: &fakeDeployer{}})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})
	ep, err := backend.CreateEndpoint(ctx, clusterID, branchID, core.RoleReadWrite, nil)
	if err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}

	resp, err := client.DeleteEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.ID)
	if err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode(), resp.Body)
	}
	assertTaskAccepted(t, resp.JSON202, resp.HTTPResponse.Header.Get("Location"), "delete_endpoint")
}

func TestDeleteClusterReturnsTask(t *testing.T) {
	client, backend, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	resp, err := client.DeleteClusterWithResponse(ctx, pool.Id, clusterID)
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode(), resp.Body)
	}
	assertTaskAccepted(t, resp.JSON202, resp.HTTPResponse.Header.Get("Location"), "delete_cluster")
}

func TestDeleteBranchReturnsTask(t *testing.T) {
	client, backend, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	clusterID := uuid.New()
	branchID := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	resp, err := client.DeleteBranchWithResponse(ctx, pool.Id, clusterID, branchID)
	if err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode(), resp.Body)
	}
	assertTaskAccepted(t, resp.JSON202, resp.HTTPResponse.Header.Get("Location"), "delete_branch")
}

// TestBranchDeleteCascade: delete-branch is a real async cascade. Seed a branch
// with an endpoint plus a SIBLING branch with its own endpoint that must survive,
// delete the first branch -> 202; drive the task; the branch and its endpoint are
// gone (its instance retired), and the sibling survives untouched.
func TestBranchDeleteCascade(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	clusterID := uuid.New()
	target := uuid.New()
	sibling := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: target, Name: "main", PgVersion: 18, IsDefault: true})
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: sibling, Name: "sibling", PgVersion: 18})
	if _, err := backend.CreateEndpoint(ctx, clusterID, target, core.RoleReadWrite, nil); err != nil {
		t.Fatalf("seed endpoint (target): %v", err)
	}
	if _, err := backend.CreateEndpoint(ctx, clusterID, sibling, core.RoleReadWrite, nil); err != nil {
		t.Fatalf("seed endpoint (sibling): %v", err)
	}

	del, err := client.DeleteBranchWithResponse(ctx, pool.Id, clusterID, target)
	if err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	if del.StatusCode() != http.StatusAccepted {
		t.Fatalf("DeleteBranch status = %d, want 202, body = %s", del.StatusCode(), del.Body)
	}
	assertTaskAccepted(t, del.JSON202, del.HTTPResponse.Header.Get("Location"), "delete_branch")

	drainTasks(t, store, backend, prov)
	if got := soleTask(t, store); got.Status != task.StatusSucceeded {
		t.Fatalf("delete_branch task status = %q, want succeeded; error = %v", got.Status, got.Error)
	}

	// The target branch is gone.
	if _, err := backend.GetBranch(ctx, pool.Id, clusterID, target); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("GetBranch(target) after cascade err = %v, want ErrNotFound", err)
	}
	// Exactly the target's endpoint instance was retired (the sibling's survives).
	if len(dep.deleted) != 1 {
		t.Errorf("deployer retire calls = %d, want 1 (only the target's endpoint)", len(dep.deleted))
	}
	// The sibling branch and its endpoint survive.
	if _, err := backend.GetBranch(ctx, pool.Id, clusterID, sibling); err != nil {
		t.Errorf("GetBranch(sibling) after cascade err = %v, want nil (it must survive)", err)
	}
	if eps, _, err := backend.ListEndpoints(ctx, pool.Id, clusterID, sibling, 100, 0); err != nil {
		t.Fatalf("ListEndpoints(sibling): %v", err)
	} else if len(eps) != 1 {
		t.Errorf("sibling endpoints after cascade = %d, want 1 (survives)", len(eps))
	}
}

// TestDeleteBranchWithChildForkConflict: a branch with a child fork cannot be
// deleted. The handler's synchronous child-fork pre-check returns a clean 409
// (no task is created), so the parent's endpoints are never retired.
func TestDeleteBranchWithChildForkConflict(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	clusterID := uuid.New()
	parent := uuid.New()
	child := uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: parent, Name: "main", PgVersion: 18, IsDefault: true})
	// A child branch forked from the parent (ParentBranchID set).
	parentRef := parent
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: child, Name: "child", PgVersion: 18, ParentBranchID: &parentRef})
	if _, err := backend.CreateEndpoint(ctx, clusterID, parent, core.RoleReadWrite, nil); err != nil {
		t.Fatalf("seed endpoint (parent): %v", err)
	}

	del, err := client.DeleteBranchWithResponse(ctx, pool.Id, clusterID, parent)
	if err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	if del.StatusCode() != http.StatusConflict {
		t.Fatalf("DeleteBranch(parent with child) status = %d, want 409, body = %s", del.StatusCode(), del.Body)
	}
	// No task was created, so nothing was retired.
	if tasks, total, err := store.List(ctx, task.TaskFilter{}, 10, 0); err != nil {
		t.Fatalf("list tasks: %v", err)
	} else if total != 0 || len(tasks) != 0 {
		t.Errorf("task count = %d, want 0 (409 pre-check creates no task)", total)
	}
	if len(dep.deleted) != 0 {
		t.Errorf("deployer retire calls = %d, want 0 (parent's endpoints must not be retired)", len(dep.deleted))
	}
	// The parent branch still exists.
	if _, err := backend.GetBranch(ctx, pool.Id, clusterID, parent); err != nil {
		t.Errorf("GetBranch(parent) after 409 err = %v, want nil (parent must survive)", err)
	}
}

// TestListTasks seeds several tasks and pages through GET /tasks, asserting the
// total/count/order contract (created_at DESC) and the status/kind/cluster_id
// filters.
func TestListTasks(t *testing.T) {
	client, _, tasks := newTestServer(t)
	ctx := context.Background()

	// Seed a mix of kinds, statuses, and clusters with distinct created_at so the
	// DESC order is deterministic. seedTask appends one task at t0+i seconds.
	proj := uuid.New()
	base := time.Now().Add(-time.Hour)
	type seed struct {
		kind    string
		status  task.Status
		cluster *uuid.UUID
	}
	seeds := []seed{
		{"create_cluster", task.StatusSucceeded, &proj},
		{"branch", task.StatusFailed, &proj},
		{"create_endpoint", task.StatusPending, nil},
		{"delete_endpoint", task.StatusRunning, &proj},
		{"create_cluster", task.StatusPending, nil},
	}
	ids := make([]uuid.UUID, len(seeds))
	for i, s := range seeds {
		id := uuid.New()
		ids[i] = id
		tasks.Seed(&task.Task{
			ID:        id,
			Kind:      s.kind,
			Status:    s.status,
			ClusterID: s.cluster,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
			UpdatedAt: base.Add(time.Duration(i) * time.Second),
		}, nil)
	}
	n := len(seeds)

	// Default list: total and count both n, ordered created_at DESC (newest first).
	all, err := client.ListTasksWithResponse(ctx, nil)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if all.StatusCode() != http.StatusOK || all.JSON200 == nil {
		t.Fatalf("ListTasks status = %d, body = %s", all.StatusCode(), all.Body)
	}
	if all.JSON200.Total != n || all.JSON200.Count != n || len(all.JSON200.Tasks) != n {
		t.Fatalf("list = {total:%d count:%d len:%d}, want all %d", all.JSON200.Total, all.JSON200.Count, len(all.JSON200.Tasks), n)
	}
	for i := 1; i < len(all.JSON200.Tasks); i++ {
		if all.JSON200.Tasks[i-1].CreatedAt.Before(all.JSON200.Tasks[i].CreatedAt) {
			t.Errorf("tasks not ordered created_at DESC at index %d", i)
		}
	}
	// Newest seed is the last one appended.
	if all.JSON200.Tasks[0].Id != ids[n-1] {
		t.Errorf("first task = %s, want the newest %s", all.JSON200.Tasks[0].Id, ids[n-1])
	}

	// Page through with limit=2: total stays n, each page <= limit, full coverage.
	limit := 2
	seen := map[uuid.UUID]bool{}
	for offset := 0; offset < n; offset += limit {
		off := offset
		page, err := client.ListTasksWithResponse(ctx, &api.ListTasksParams{Limit: &limit, Offset: &off})
		if err != nil {
			t.Fatalf("ListTasks(offset=%d): %v", off, err)
		}
		if page.JSON200.Total != n {
			t.Errorf("offset=%d total = %d, want %d", off, page.JSON200.Total, n)
		}
		if page.JSON200.Count != len(page.JSON200.Tasks) || page.JSON200.Count > limit {
			t.Errorf("offset=%d count = %d (len %d), want <= %d", off, page.JSON200.Count, len(page.JSON200.Tasks), limit)
		}
		for _, tk := range page.JSON200.Tasks {
			if seen[tk.Id] {
				t.Errorf("task %s returned on more than one page", tk.Id)
			}
			seen[tk.Id] = true
		}
	}
	if len(seen) != n {
		t.Errorf("paged tasks = %d, want %d", len(seen), n)
	}

	// Status filter: two tasks are pending.
	pendingStatus := api.ListTasksParamsStatusPending
	byStatus, err := client.ListTasksWithResponse(ctx, &api.ListTasksParams{Status: &pendingStatus})
	if err != nil {
		t.Fatalf("ListTasks(status=pending): %v", err)
	}
	if byStatus.JSON200.Total != 2 {
		t.Errorf("pending total = %d, want 2", byStatus.JSON200.Total)
	}
	for _, tk := range byStatus.JSON200.Tasks {
		if tk.Status != api.TaskStatusPending {
			t.Errorf("status filter returned a %q task", tk.Status)
		}
	}

	// Kind filter: two tasks are create_cluster.
	kind := "create_cluster"
	byKind, err := client.ListTasksWithResponse(ctx, &api.ListTasksParams{Kind: &kind})
	if err != nil {
		t.Fatalf("ListTasks(kind=create_cluster): %v", err)
	}
	if byKind.JSON200.Total != 2 {
		t.Errorf("create_cluster total = %d, want 2", byKind.JSON200.Total)
	}
	for _, tk := range byKind.JSON200.Tasks {
		if tk.Kind != "create_cluster" {
			t.Errorf("kind filter returned a %q task", tk.Kind)
		}
	}

	// Cluster filter: three tasks are scoped to proj.
	byCluster, err := client.ListTasksWithResponse(ctx, &api.ListTasksParams{ClusterId: &proj})
	if err != nil {
		t.Fatalf("ListTasks(cluster_id): %v", err)
	}
	if byCluster.JSON200.Total != 3 {
		t.Errorf("cluster-scoped total = %d, want 3", byCluster.JSON200.Total)
	}
	for _, tk := range byCluster.JSON200.Tasks {
		if tk.ClusterId == nil || *tk.ClusterId != proj {
			t.Errorf("cluster filter returned a task scoped to %v, want %s", tk.ClusterId, proj)
		}
	}

	// An unrecognized status is rejected with 400 (the handler validates the enum;
	// there is no wire-level validator middleware, so an invalid value would
	// otherwise reach the Postgres ::task_status cast and 500).
	bogus := api.ListTasksParamsStatus("bogus")
	invalid, err := client.ListTasksWithResponse(ctx, &api.ListTasksParams{Status: &bogus})
	if err != nil {
		t.Fatalf("ListTasks(status=bogus): %v", err)
	}
	if invalid.StatusCode() != http.StatusBadRequest {
		t.Fatalf("ListTasks(status=bogus) status = %d, want 400, body = %s", invalid.StatusCode(), invalid.Body)
	}
}

// The one-call create is the door most callers use, so its writer has to be held
// to the same shape as one created through createEndpoint — including memory
// following cpu when a caller names only cpu, which is the whole reason naming
// one dimension is enough.
func TestCreateClusterWithWriterIsHeldToTheFloor(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "writer-floor-pool")
	_ = backend

	withWriter := true
	// Under the floor: refused before anything is planned.
	cpu, thin := "4", "8Gi"
	bad, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name:       "thin",
		WithWriter: &withWriter,
		Resources:  &api.EndpointResources{Cpu: &cpu, Memory: &thin},
	})
	if err != nil {
		t.Fatalf("CreateCluster(thin writer): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", bad.StatusCode(), bad.Body)
	}

	// Naming only cpu: memory follows it rather than reaching the deployer empty
	// and landing on a fallback that predates this rule.
	only := "8"
	ok, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name:       "sized",
		WithWriter: &withWriter,
		Resources:  &api.EndpointResources{Cpu: &only},
	})
	if err != nil || ok.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateCluster(cpu only): status = %d, err = %v; body = %s", ok.StatusCode(), err, ok.Body)
	}
	drainTasks(t, store, backend, prov)
	if len(dep.created) != 1 || dep.created[0].CPU != "8" || dep.created[0].Memory != "64Gi" {
		t.Fatalf("writer deploy spec = %+v, want one {8, 64Gi}", dep.created)
	}
}

// TestListTasksChangeFeed covers the updated_since change feed: the ordering it
// switches to, the case that ordering exists for, its inclusivity, and the
// ordered_by field a reader uses to confirm the parameter was honoured.
func TestListTasksChangeFeed(t *testing.T) {
	client, _, tasks := newTestServer(t)
	ctx := context.Background()

	// The shape the default created_at ordering cannot serve: `old` was created
	// first but touched LAST, so under created_at DESC it sits at the bottom of the
	// page, below rows an incremental reader has already consumed. A watermark over
	// that ordering skips it -- which is the whole reason updated_since reorders.
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	old, mid, recent := uuid.New(), uuid.New(), uuid.New()
	for _, s := range []struct {
		id      uuid.UUID
		created time.Time
		updated time.Time
		status  task.Status
	}{
		{old, base, base.Add(30 * time.Minute), task.StatusFailed},
		{mid, base.Add(time.Minute), base.Add(time.Minute), task.StatusSucceeded},
		{recent, base.Add(2 * time.Minute), base.Add(2 * time.Minute), task.StatusRunning},
	} {
		tasks.Seed(&task.Task{
			ID: s.id, Kind: "create_endpoint", Status: s.status,
			CreatedAt: s.created, UpdatedAt: s.updated,
		}, nil)
	}

	// Default listing: no updated_since, so created_at DESC and ordered_by says so.
	def, err := client.ListTasksWithResponse(ctx, nil)
	if err != nil || def.JSON200 == nil {
		t.Fatalf("ListTasks: %v, body = %s", err, def.Body)
	}
	if def.JSON200.OrderedBy == nil || *def.JSON200.OrderedBy != api.CreatedAtDesc {
		t.Errorf("default ordered_by = %v, want created_at_desc", def.JSON200.OrderedBy)
	}
	if def.JSON200.Tasks[len(def.JSON200.Tasks)-1].Id != old {
		t.Fatalf("expected `old` last under created_at DESC; the premise of this test does not hold")
	}

	// The change feed: a watermark after `mid` and `recent` were stamped still
	// returns `old`, because `old` was UPDATED after it.
	since := base.Add(3 * time.Minute)
	feed, err := client.ListTasksWithResponse(ctx, &api.ListTasksParams{UpdatedSince: &since})
	if err != nil || feed.JSON200 == nil {
		t.Fatalf("ListTasks(updated_since): %v, body = %s", err, feed.Body)
	}
	if feed.JSON200.OrderedBy == nil || *feed.JSON200.OrderedBy != api.UpdatedAtAsc {
		t.Errorf("feed ordered_by = %v, want updated_at_asc", feed.JSON200.OrderedBy)
	}
	if len(feed.JSON200.Tasks) != 1 || feed.JSON200.Tasks[0].Id != old {
		t.Fatalf("feed = %v, want exactly the task updated after the watermark (%s)", feed.JSON200.Tasks, old)
	}
	if feed.JSON200.Total != 1 {
		t.Errorf("feed total = %d, want 1 (total must respect the filter)", feed.JSON200.Total)
	}

	// Ascending, ties on id ascending -- what makes offset paging safe: new rows
	// land at the far end of the page rather than shifting the window.
	from := base
	asc, err := client.ListTasksWithResponse(ctx, &api.ListTasksParams{UpdatedSince: &from})
	if err != nil || asc.JSON200 == nil {
		t.Fatalf("ListTasks(updated_since=base): %v, body = %s", err, asc.Body)
	}
	got := make([]uuid.UUID, 0, len(asc.JSON200.Tasks))
	for _, tk := range asc.JSON200.Tasks {
		got = append(got, tk.Id)
	}
	want := []uuid.UUID{mid, recent, old} // updated_at ASC
	if len(got) != len(want) {
		t.Fatalf("feed ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("feed ids = %v, want %v (updated_at ASC)", got, want)
		}
	}

	// Inclusive: a watermark set to a task's own updated_at returns that task, so a
	// reader resuming from the last row it saw cannot drop a same-instant twin.
	exact := base.Add(30 * time.Minute)
	incl, err := client.ListTasksWithResponse(ctx, &api.ListTasksParams{UpdatedSince: &exact})
	if err != nil || incl.JSON200 == nil {
		t.Fatalf("ListTasks(updated_since=exact): %v, body = %s", err, incl.Body)
	}
	if len(incl.JSON200.Tasks) != 1 || incl.JSON200.Tasks[0].Id != old {
		t.Fatalf("inclusive feed = %v, want the task stamped exactly at the watermark", incl.JSON200.Tasks)
	}
}
