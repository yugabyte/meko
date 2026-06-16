// These tests drive the controllers through the *generated* Go client
// (api.ClientWithResponses) over a real httptest server, with the backend
// stubbed by core.MemBackend and a fake task store. They validate that the
// controllers are wired to the spec correctly — routing, path/body binding,
// status codes, and response shapes — without a database.
package api_test

import (
	"context"
	"encoding/json"
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

	srv := api.NewServer(&config.Config{ListenAddr: ":0"}, backend, tasks, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	client, err := api.NewClientWithResponses(ts.URL + "/api/v1")
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return client, backend, tasks
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

func TestTimelinesLoggedOnly(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool, err := client.CreateStoragePoolWithResponse(ctx, u.Id, api.CreateStoragePoolRequest{Name: "pool"})
	if err != nil || pool.JSON201 == nil {
		t.Fatalf("create pool: %v (status %d)", err, pool.StatusCode())
	}
	poolID := pool.JSON201.Id

	// Create timeline -> 202 + ack.
	ct, err := client.CreateTimelineWithResponse(ctx, u.Id, poolID, api.CreateTimelineRequest{Name: "main"})
	if err != nil {
		t.Fatalf("CreateTimeline: %v", err)
	}
	if ct.StatusCode() != http.StatusAccepted || ct.JSON202 == nil {
		t.Fatalf("CreateTimeline status = %d, body = %s", ct.StatusCode(), ct.Body)
	}
	if !ct.JSON202.Acknowledged {
		t.Errorf("ack.Acknowledged = false, want true")
	}

	// Empty name -> 400.
	ctBad, err := client.CreateTimelineWithResponse(ctx, u.Id, poolID, api.CreateTimelineRequest{Name: ""})
	if err != nil {
		t.Fatalf("CreateTimeline(empty): %v", err)
	}
	if ctBad.StatusCode() != http.StatusBadRequest {
		t.Fatalf("CreateTimeline(empty) status = %d, body = %s", ctBad.StatusCode(), ctBad.Body)
	}

	// Delete timeline -> 202.
	del, err := client.DeleteTimelineWithResponse(ctx, u.Id, poolID, uuid.New())
	if err != nil {
		t.Fatalf("DeleteTimeline: %v", err)
	}
	if del.StatusCode() != http.StatusAccepted || del.JSON202 == nil {
		t.Fatalf("DeleteTimeline status = %d, body = %s", del.StatusCode(), del.Body)
	}

	// Branch timeline -> 202; empty name -> 400.
	br, err := client.BranchTimelineWithResponse(ctx, u.Id, poolID, uuid.New(), api.BranchTimelineRequest{Name: "feature"})
	if err != nil {
		t.Fatalf("BranchTimeline: %v", err)
	}
	if br.StatusCode() != http.StatusAccepted || br.JSON202 == nil {
		t.Fatalf("BranchTimeline status = %d, body = %s", br.StatusCode(), br.Body)
	}
	brBad, err := client.BranchTimelineWithResponse(ctx, u.Id, poolID, uuid.New(), api.BranchTimelineRequest{Name: ""})
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
