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
	"strings"
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
	t.Helper()
	backend := core.NewMemBackend()
	tasks := newFakeTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := api.NewServer(&config.Config{ListenAddr: ":0"}, backend, tasks, api.Provisioning{}, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	client, err := api.NewClientWithResponses(ts.URL + "/api/v1")
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return client, backend, tasks
}

// newTestServerWithProv is newTestServer with provisioning collaborators wired
// in (a fake deployer, etc.), returning just the client.
func newTestServerWithProv(t *testing.T, prov api.Provisioning) *api.ClientWithResponses {
	return newTestServerWith(t, core.NewMemBackend(), prov)
}

// newTestServerWith builds the server over an explicit backend + provisioning
// (used to inject a failing backend), returning just the client.
func newTestServerWith(t *testing.T, backend core.Backend, prov api.Provisioning) *api.ClientWithResponses {
	t.Helper()
	tasks := newFakeTaskStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.NewServer(&config.Config{ListenAddr: ":0"}, backend, tasks, prov, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client, err := api.NewClientWithResponses(ts.URL + "/api/v1")
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return client
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
	bad, err := client.CreateStoragePoolWithResponse(ctx, u.Id, api.CreateStoragePoolRequest{Name: "  "})
	if err != nil {
		t.Fatalf("CreateStoragePool(empty): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest || bad.JSON400 == nil {
		t.Fatalf("empty name status = %d, body = %s", bad.StatusCode(), bad.Body)
	}

	// Unknown owner -> 404.
	missing, err := client.CreateStoragePoolWithResponse(ctx, uuid.New(), api.CreateStoragePoolRequest{Name: "p1"})
	if err != nil {
		t.Fatalf("CreateStoragePool(missing user): %v", err)
	}
	if missing.StatusCode() != http.StatusNotFound || missing.JSON404 == nil {
		t.Fatalf("missing owner status = %d, body = %s", missing.StatusCode(), missing.Body)
	}

	// Valid -> 201, owned by the user.
	ok, err := client.CreateStoragePoolWithResponse(ctx, u.Id, api.CreateStoragePoolRequest{Name: "p1"})
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
	dup, err := client.CreateStoragePoolWithResponse(ctx, u.Id, api.CreateStoragePoolRequest{Name: "p1"})
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
	resp, err := client.CreateStoragePoolWithResponse(ctx, u.Id, api.CreateStoragePoolRequest{Name: "  padded  "})
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
	ct, err := client.CreateTimelineWithResponse(ctx, u.Id, pool.Id, api.CreateTimelineRequest{Name: "main", PgVersion: &pgv})
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
	ctBad, err := client.CreateTimelineWithResponse(ctx, u.Id, pool.Id, api.CreateTimelineRequest{Name: ""})
	if err != nil {
		t.Fatalf("CreateTimeline(empty): %v", err)
	}
	if ctBad.StatusCode() != http.StatusBadRequest {
		t.Fatalf("CreateTimeline(empty) status = %d, body = %s", ctBad.StatusCode(), ctBad.Body)
	}

	// Duplicate name in the same pool -> 409.
	dup, err := client.CreateTimelineWithResponse(ctx, u.Id, pool.Id, api.CreateTimelineRequest{Name: "main"})
	if err != nil {
		t.Fatalf("CreateTimeline(dup): %v", err)
	}
	if dup.StatusCode() != http.StatusConflict || dup.JSON409 == nil {
		t.Fatalf("CreateTimeline(dup) status = %d, body = %s", dup.StatusCode(), dup.Body)
	}

	// Unknown / unowned pool -> 404 (owner-scoped).
	missing, err := client.CreateTimelineWithResponse(ctx, u.Id, uuid.New(), api.CreateTimelineRequest{Name: "x"})
	if err != nil {
		t.Fatalf("CreateTimeline(missing pool): %v", err)
	}
	if missing.StatusCode() != http.StatusNotFound || missing.JSON404 == nil {
		t.Fatalf("CreateTimeline(missing pool) status = %d, body = %s", missing.StatusCode(), missing.Body)
	}

	// Unsupported pg_version -> 400 (yb-light is v17 only).
	badVer := 15
	bv, err := client.CreateTimelineWithResponse(ctx, u.Id, pool.Id, api.CreateTimelineRequest{Name: "pg15", PgVersion: &badVer})
	if err != nil {
		t.Fatalf("CreateTimeline(bad version): %v", err)
	}
	if bv.StatusCode() != http.StatusBadRequest {
		t.Fatalf("CreateTimeline(pg_version=15) status = %d, want 400, body = %s", bv.StatusCode(), bv.Body)
	}
}

// fakeDeployer is a deploy.Deployer that records the spec and returns a fixed
// address, so the create-timeline workflow's endpoint wiring can be tested
// without Docker. (The real Docker path + the storage-controller call are
// covered by the amp-on-Docker e2e, test/e2e/run-amp.sh.)
type fakeDeployer struct {
	addr     string
	lastSpec deploy.EndpointSpec
	deleted  []string // ids passed to DeleteEndpoint
}

func (f *fakeDeployer) CreateEndpoint(_ context.Context, spec deploy.EndpointSpec) (deploy.Endpoint, error) {
	f.lastSpec = spec
	return deploy.Endpoint{ID: spec.Name, Address: f.addr}, nil
}
func (f *fakeDeployer) DeleteEndpoint(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// failEndpointBackend is a MemBackend whose endpoint record-write always fails,
// to exercise the handler's compensating teardown of the already-launched
// container (so it doesn't leak with no record pointing at it).
type failEndpointBackend struct{ *core.MemBackend }

func (failEndpointBackend) CreateEndpoint(context.Context, uuid.UUID, uuid.UUID, string, string) (core.Endpoint, error) {
	return core.Endpoint{}, errors.New("simulated endpoint record-write failure")
}

func TestCreateTimelineRemovesEndpointWhenRecordFails(t *testing.T) {
	dep := &fakeDeployer{addr: "127.0.0.1:54321"}
	client := newTestServerWith(t, failEndpointBackend{core.NewMemBackend()}, api.Provisioning{Deployer: dep})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	ct, err := client.CreateTimelineWithResponse(ctx, u.Id, pool.Id, api.CreateTimelineRequest{Name: "main"})
	if err != nil {
		t.Fatalf("CreateTimeline: %v", err)
	}
	if ct.StatusCode() != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (endpoint record write failed)", ct.StatusCode())
	}
	// The launched container must have been torn down, not leaked.
	if len(dep.deleted) != 1 {
		t.Fatalf("DeleteEndpoint calls = %v, want exactly 1 (container cleaned up after record failure)", dep.deleted)
	}
}

func TestCreateTimelineProvisionsEndpoint(t *testing.T) {
	dep := &fakeDeployer{addr: "127.0.0.1:54321"}
	client := newTestServerWithProv(t, api.Provisioning{Deployer: dep})
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	ct, err := client.CreateTimelineWithResponse(ctx, u.Id, pool.Id, api.CreateTimelineRequest{Name: "main"})
	if err != nil {
		t.Fatalf("CreateTimeline: %v", err)
	}
	if ct.StatusCode() != http.StatusCreated || ct.JSON201 == nil {
		t.Fatalf("CreateTimeline status = %d, body = %s", ct.StatusCode(), ct.Body)
	}
	ep := ct.JSON201.Endpoint
	if ep == nil {
		t.Fatal("create timeline returned no endpoint, want one (deployer was set)")
	}
	if ep.Address != "127.0.0.1:54321" {
		t.Errorf("endpoint address = %q, want 127.0.0.1:54321", ep.Address)
	}
	if ep.Role != api.ReadWrite {
		t.Errorf("endpoint role = %q, want read_write", ep.Role)
	}
	// The deployer was asked to bind the timeline amp created (32-hex ids).
	if dep.lastSpec.TenantID != strings.ReplaceAll(ct.JSON201.TenantId.String(), "-", "") {
		t.Errorf("deployer tenant = %q, want %s", dep.lastSpec.TenantID, ct.JSON201.TenantId)
	}
	if !dep.lastSpec.Bootstrap {
		t.Error("deployer spec Bootstrap = false, want true for a fresh timeline")
	}
}

func TestTimelineDeleteBranchLoggedOnly(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	// Delete timeline -> 202 (logged only, parent not validated).
	del, err := client.DeleteTimelineWithResponse(ctx, u.Id, pool.Id, uuid.New())
	if err != nil {
		t.Fatalf("DeleteTimeline: %v", err)
	}
	if del.StatusCode() != http.StatusAccepted || del.JSON202 == nil {
		t.Fatalf("DeleteTimeline status = %d, body = %s", del.StatusCode(), del.Body)
	}

	// Branch timeline -> 202; empty name -> 400.
	br, err := client.BranchTimelineWithResponse(ctx, u.Id, pool.Id, uuid.New(), api.BranchTimelineRequest{Name: "feature"})
	if err != nil {
		t.Fatalf("BranchTimeline: %v", err)
	}
	if br.StatusCode() != http.StatusAccepted || br.JSON202 == nil {
		t.Fatalf("BranchTimeline status = %d, body = %s", br.StatusCode(), br.Body)
	}
	brBad, err := client.BranchTimelineWithResponse(ctx, u.Id, pool.Id, uuid.New(), api.BranchTimelineRequest{Name: ""})
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

func (f *fakeTaskStore) Plan(context.Context, string, json.RawMessage, []task.StepPlan) (*task.Task, error) {
	return nil, nil
}
func (f *fakeTaskStore) ClaimNext(context.Context, string, time.Duration) (*task.Task, error) {
	return nil, nil
}
func (f *fakeTaskStore) Steps(context.Context, uuid.UUID) ([]task.Step, error)    { return nil, nil }
func (f *fakeTaskStore) StartStep(context.Context, uuid.UUID, int) error          { return nil }
func (f *fakeTaskStore) CompleteStep(context.Context, uuid.UUID, int, json.RawMessage) error {
	return nil
}
func (f *fakeTaskStore) FailStep(context.Context, uuid.UUID, int, error) error { return nil }
func (f *fakeTaskStore) CompleteTask(context.Context, uuid.UUID) error         { return nil }
