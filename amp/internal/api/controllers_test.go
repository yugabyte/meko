// These tests drive the controllers through the *generated* Go client
// (api.ClientWithResponses) over a real httptest server, with the backend
// stubbed by core.MemBackend and a fake task store. They validate that the
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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-light/amp/internal/api"
	"github.com/yugabyte/yb-light/amp/internal/config"
	"github.com/yugabyte/yb-light/amp/internal/core"
	"github.com/yugabyte/yb-light/amp/internal/deploy"
	"github.com/yugabyte/yb-light/amp/internal/task"
)

// newTestServer stands up the real server (NewServer) over httptest, backed by
// an in-memory backend and a fake task store, and returns the generated client
// pointed at it.
func newTestServer(t *testing.T) (*api.ClientWithResponses, *core.MemBackend, *fakeTaskStore) {
	// Deploy enabled by default so the endpoint routes are live (a fake deployer
	// returns a derived address without touching a real runtime).
	return newTestServerDeploy(t, true)
}

func newTestServerDeploy(t *testing.T, deployEnabled bool) (*api.ClientWithResponses, *core.MemBackend, *fakeTaskStore) {
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
func newTestServerWith(t *testing.T, backend *core.MemBackend, prov api.Provisioning) (*api.ClientWithResponses, *core.MemBackend, *fakeTaskStore) {
	t.Helper()
	client, tasks := newTestServerBackend(t, backend, prov)
	return client, backend, tasks
}

// newTestServerBackend builds the server over any core.Backend (so a failing
// backend can be injected), returning the client and the wired task store.
func newTestServerBackend(t *testing.T, backend core.Backend, prov api.Provisioning) (*api.ClientWithResponses, *fakeTaskStore) {
	t.Helper()
	tasks := newFakeTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{ListenAddr: ":0"}
	cfg.Deploy.Enabled = prov.Deployer != nil
	srv := api.NewServer(cfg, backend, tasks, prov, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client, err := api.NewClientWithResponses(ts.URL + "/api/v1")
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return client, tasks
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

func TestCreateTimeline(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	// Create timeline -> 201 with real (tenant_id, timeline_id).
	pgv := 17
	ct, err := client.CreateTimelineWithResponse(ctx, pool.Id, api.CreateTimelineRequest{Name: "main", PgVersion: &pgv})
	if err != nil {
		t.Fatalf("CreateTimeline: %v", err)
	}
	if ct.StatusCode() != http.StatusCreated || ct.JSON201 == nil {
		t.Fatalf("CreateTimeline status = %d, body = %s", ct.StatusCode(), ct.Body)
	}
	tl := *ct.JSON201
	if tl.TenantId == uuid.Nil || tl.TimelineId == uuid.Nil {
		t.Errorf("timeline has nil ids: tenant=%s timeline=%s", tl.TenantId, tl.TimelineId)
	}
	if !tl.IsMain || tl.Name != "main" || tl.PgVersion != 17 {
		t.Errorf("timeline = %+v, want {is_main name=main pg_version=17}", tl)
	}

	// Empty name -> 400.
	ctBad, err := client.CreateTimelineWithResponse(ctx, pool.Id, api.CreateTimelineRequest{Name: ""})
	if err != nil {
		t.Fatalf("CreateTimeline(empty): %v", err)
	}
	if ctBad.StatusCode() != http.StatusBadRequest {
		t.Fatalf("CreateTimeline(empty) status = %d, body = %s", ctBad.StatusCode(), ctBad.Body)
	}

	// Duplicate name in the same pool -> 409.
	dup, err := client.CreateTimelineWithResponse(ctx, pool.Id, api.CreateTimelineRequest{Name: "main"})
	if err != nil {
		t.Fatalf("CreateTimeline(dup): %v", err)
	}
	if dup.StatusCode() != http.StatusConflict || dup.JSON409 == nil {
		t.Fatalf("CreateTimeline(dup) status = %d, body = %s", dup.StatusCode(), dup.Body)
	}

	// Unknown pool -> 404.
	missing, err := client.CreateTimelineWithResponse(ctx, uuid.New(), api.CreateTimelineRequest{Name: "x"})
	if err != nil {
		t.Fatalf("CreateTimeline(missing pool): %v", err)
	}
	if missing.StatusCode() != http.StatusNotFound || missing.JSON404 == nil {
		t.Fatalf("CreateTimeline(missing pool) status = %d, body = %s", missing.StatusCode(), missing.Body)
	}

	// Unsupported pg_version -> 400 (yb-light is v17 only).
	badVer := 15
	bv, err := client.CreateTimelineWithResponse(ctx, pool.Id, api.CreateTimelineRequest{Name: "pg15", PgVersion: &badVer})
	if err != nil {
		t.Fatalf("CreateTimeline(bad version): %v", err)
	}
	if bv.StatusCode() != http.StatusBadRequest {
		t.Fatalf("CreateTimeline(pg_version=15) status = %d, want 400, body = %s", bv.StatusCode(), bv.Body)
	}
}

// fakeDeployer is a deploy.Deployer that records calls and returns a derived
// address, so the synchronous endpoint deploy/retire wiring can be tested
// without a real runtime. createErr, when set, makes CreateEndpoint fail (to
// exercise the handler's compensating row delete).
type fakeDeployer struct {
	addr      string
	created   []deploy.EndpointSpec
	deleted   []string
	createErr error
}

func (f *fakeDeployer) CreateEndpoint(_ context.Context, spec deploy.EndpointSpec) (deploy.Endpoint, error) {
	f.created = append(f.created, spec)
	if f.createErr != nil {
		return deploy.Endpoint{}, f.createErr
	}
	addr := f.addr
	if addr == "" {
		addr = spec.Name + ".ns.svc.cluster.local:55433"
	}
	return deploy.Endpoint{ID: spec.Name, Address: addr}, nil
}

func (f *fakeDeployer) DeleteEndpoint(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func TestCreateEndpointDeployFailureCompensates(t *testing.T) {
	dep := &fakeDeployer{createErr: errors.New("simulated deploy failure")}
	client, backend := newTestServerWithProv(t, api.Provisioning{Deployer: dep})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	timelineID := uuid.New()
	backend.SeedTimeline(pool.Id, core.Timeline{TenantID: uuid.New(), TimelineID: timelineID, Name: "main", PgVersion: 17, IsMain: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, timelineID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (deploy failed); body = %s", created.StatusCode(), created.Body)
	}
	if len(dep.created) != 1 {
		t.Fatalf("CreateEndpoint deploy calls = %d, want 1", len(dep.created))
	}
	// The committed row must have been removed so it doesn't linger undeployed.
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, timelineID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.JSON200 == nil || len(list.JSON200.Endpoints) != 0 {
		t.Fatalf("endpoints after failed deploy = %v, want none (compensated)", list.Body)
	}
}

// addrFailBackend makes SetEndpointAddress fail while delegating everything else
// to MemBackend, to exercise the compensation when a deploy succeeds but its
// address can't be recorded.
type addrFailBackend struct {
	*core.MemBackend
}

func (addrFailBackend) SetEndpointAddress(context.Context, uuid.UUID, string) error {
	return errors.New("simulated address-record failure")
}

// After a successful deploy, a failure recording the address must retire the
// just-deployed endpoint and remove the row, so the writer slot is freed and a
// retry isn't permanently blocked (mirrors the deploy-failure compensation).
func TestCreateEndpointAddressRecordFailureCompensates(t *testing.T) {
	mem := core.NewMemBackend()
	dep := &fakeDeployer{}
	client, _ := newTestServerBackend(t, addrFailBackend{mem}, api.Provisioning{Deployer: dep})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	timelineID := uuid.New()
	mem.SeedTimeline(pool.Id, core.Timeline{TenantID: uuid.New(), TimelineID: timelineID, Name: "main", PgVersion: 17, IsMain: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, timelineID, api.CreateEndpointRequest{Role: &rw})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.StatusCode() != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (address record failed); body = %s", created.StatusCode(), created.Body)
	}
	// The deployed endpoint must have been retired (compensation), not leaked.
	if len(dep.created) != 1 || len(dep.deleted) != 1 {
		t.Fatalf("deployer calls: created=%d deleted=%d, want 1 and 1 (retired on compensation)", len(dep.created), len(dep.deleted))
	}
	// The row must be gone so the writer slot is freed for a retry.
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, timelineID, nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if list.JSON200 == nil || len(list.JSON200.Endpoints) != 0 {
		t.Fatalf("endpoints after address-record failure = %v, want none (compensated)", list.Body)
	}
}

func TestTimelineDeleteLoggedOnlyAndBranchValidation(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	// Delete timeline -> 202 (logged only, parent not validated).
	del, err := client.DeleteTimelineWithResponse(ctx, pool.Id, uuid.New())
	if err != nil {
		t.Fatalf("DeleteTimeline: %v", err)
	}
	if del.StatusCode() != http.StatusAccepted || del.JSON202 == nil {
		t.Fatalf("DeleteTimeline status = %d, body = %s", del.StatusCode(), del.Body)
	}

	// Branch is real now (#130): a nonexistent parent -> 404, and an empty name
	// -> 400. The happy path (a real parent + the recipe) lives in
	// timeline_storage_test.go, where a fake storage controller is wired.
	br, err := client.BranchTimelineWithResponse(ctx, pool.Id, uuid.New(), api.BranchTimelineRequest{Name: "feature"})
	if err != nil {
		t.Fatalf("BranchTimeline: %v", err)
	}
	if br.StatusCode() != http.StatusNotFound {
		t.Fatalf("BranchTimeline(missing parent) status = %d, body = %s", br.StatusCode(), br.Body)
	}
	brBad, err := client.BranchTimelineWithResponse(ctx, pool.Id, uuid.New(), api.BranchTimelineRequest{Name: ""})
	if err != nil {
		t.Fatalf("BranchTimeline(empty): %v", err)
	}
	if brBad.StatusCode() != http.StatusBadRequest {
		t.Fatalf("BranchTimeline(empty) status = %d, body = %s", brBad.StatusCode(), brBad.Body)
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
	tasks.put(&task.Task{
		ID:        id,
		Kind:      "create_database",
		Status:    task.StatusRunning,
		Params:    json.RawMessage(`{"name":"db1"}`),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	})
	got, err := client.GetTaskWithResponse(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.StatusCode() != http.StatusOK || got.JSON200 == nil {
		t.Fatalf("GetTask status = %d, body = %s", got.StatusCode(), got.Body)
	}
	if got.JSON200.Kind != "create_database" || got.JSON200.Status != api.Running {
		t.Errorf("task = {kind:%q status:%q}, want {create_database running}", got.JSON200.Kind, got.JSON200.Status)
	}
}

// fakeTaskStore is a task.Store whose only meaningful method is Get; the
// controllers only read task status. The mutation methods satisfy the
// interface and are unused by the API.
type fakeTaskStore struct {
	tasks map[uuid.UUID]*task.Task
}

func newFakeTaskStore() *fakeTaskStore {
	return &fakeTaskStore{tasks: make(map[uuid.UUID]*task.Task)}
}

func (f *fakeTaskStore) put(t *task.Task) { f.tasks[t.ID] = t }

func (f *fakeTaskStore) Get(_ context.Context, id uuid.UUID) (*task.Task, error) {
	return f.tasks[id], nil
}

func (f *fakeTaskStore) Plan(_ context.Context, kind string, params json.RawMessage, _ []task.StepPlan) (*task.Task, error) {
	tk := &task.Task{
		ID:        uuid.New(),
		Kind:      kind,
		Status:    task.StatusPending,
		Params:    params,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	f.tasks[tk.ID] = tk
	return tk, nil
}
func (f *fakeTaskStore) ClaimNext(context.Context, string, time.Duration) (*task.Task, error) {
	return nil, nil
}
func (f *fakeTaskStore) Steps(context.Context, uuid.UUID) ([]task.Step, error) { return nil, nil }
func (f *fakeTaskStore) StartStep(context.Context, uuid.UUID, int) error       { return nil }
func (f *fakeTaskStore) CompleteStep(context.Context, uuid.UUID, int, json.RawMessage) error {
	return nil
}
func (f *fakeTaskStore) FailStep(context.Context, uuid.UUID, int, error) error { return nil }
func (f *fakeTaskStore) CompleteTask(context.Context, uuid.UUID) error         { return nil }
