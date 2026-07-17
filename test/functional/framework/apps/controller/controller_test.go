package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeController is a scripted httptest controller. Handlers register by
// "METHOD path"; the default 404s. It records the ordered list of requests.
type fakeController struct {
	mu       sync.Mutex
	requests []string
	handlers map[string]http.HandlerFunc
	srv      *httptest.Server
}

func newFake() *fakeController {
	f := &fakeController{handlers: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		f.requests = append(f.requests, key)
		h := f.handlers[key]
		f.mu.Unlock()
		if h == nil {
			http.Error(w, `{"error":{"code":"not_found","message":"no handler"}}`, http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	return f
}

func (f *fakeController) on(key string, h http.HandlerFunc) { f.handlers[key] = h }

func (f *fakeController) close() { f.srv.Close() }

func (f *fakeController) client() *Client {
	c := New(f.srv.URL, WithHTTPClient(f.srv.Client()))
	c.PollInitial = time.Millisecond
	c.PollMax = 2 * time.Millisecond
	return c
}

// clientFreshConns is like client but disables HTTP keep-alives, so every
// request dials a brand new connection. Tests that hijack-and-drop a
// connection to simulate a port-forward blip use this: with keep-alives on,
// net/http's Transport silently retries an idempotent GET itself when a
// *reused* connection turns out dead, which would mask GetTask's own retry
// logic (the thing under test) behind Go's built-in one.
func (f *fakeController) clientFreshConns() *Client {
	hc := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	c := New(f.srv.URL, WithHTTPClient(hc))
	c.PollInitial = time.Millisecond
	c.PollMax = 2 * time.Millisecond
	return c
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// hijackDrop hijacks the connection and closes it without writing a byte,
// simulating the "GET ...: EOF" a kubectl port-forward blip produces when the
// process dies (or is between death and reconnect) mid-request.
func hijackDrop(w http.ResponseWriter, _ *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("ResponseWriter does not support hijacking")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		panic(err)
	}
	_ = conn.Close()
}

func TestCreateUserSync201(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/users", func(w http.ResponseWriter, _ *http.Request) {
		// Extra unknown fields must be tolerated by the client.
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": "u1", "is_admin": false, "created_at": "2026-01-01T00:00:00Z", "surprise": 7,
		})
	})
	u, err := f.client().CreateUser(context.Background(), CreateUserRequest{})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if u.ID != "u1" {
		t.Errorf("user id = %q, want u1", u.ID)
	}
}

func TestCreateStoragePoolSync201(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/storage-pools", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"id": "p1", "name": "pool", "owner_user_id": "u1"})
	})
	p, err := f.client().CreateStoragePool(context.Background(), CreateStoragePoolRequest{Name: "pool", OwnerUserID: "u1"})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if p.ID != "p1" || p.Name != "pool" {
		t.Errorf("pool = %+v", p)
	}
}

func TestCreateProjectAndWaitSuccess(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/storage-pools/p1/projects", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/api/v1/tasks/t1")
		writeJSON(w, http.StatusAccepted, map[string]any{"id": "t1", "kind": "create_project", "status": "pending"})
	})
	var polls int
	f.on("GET /api/v1/tasks/t1", func(w http.ResponseWriter, _ *http.Request) {
		polls++
		status := "running"
		if polls >= 2 {
			status = "succeeded"
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "t1", "kind": "create_project", "status": status})
	})
	task, err := f.client().CreateProjectAndWait(context.Background(), "p1", CreateProjectRequest{Name: "proj"})
	if err != nil {
		t.Fatalf("create project and wait: %v", err)
	}
	if task.Status != TaskSucceeded {
		t.Errorf("task status = %q, want succeeded", task.Status)
	}
	if polls < 2 {
		t.Errorf("expected to poll until succeeded, polled %d", polls)
	}
}

func TestCreateEndpointSendsRole(t *testing.T) {
	f := newFake()
	defer f.close()
	var body map[string]any
	f.on("POST /api/v1/storage-pools/p1/projects/pr1/branches/b1/endpoints", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Location", "/api/v1/tasks/t1")
		writeJSON(w, http.StatusAccepted, map[string]any{"id": "t1", "kind": "create_endpoint", "status": "pending"})
	})
	f.on("GET /api/v1/tasks/t1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": "t1", "kind": "create_endpoint", "status": "succeeded"})
	})
	if _, err := f.client().CreateEndpointAndWait(context.Background(), "p1", "pr1", "b1", CreateEndpointRequest{Role: "read_write"}); err != nil {
		t.Fatalf("create endpoint and wait: %v", err)
	}
	if body["role"] != "read_write" {
		t.Errorf("request body role = %v, want %q", body["role"], "read_write")
	}
}

func TestAsyncTaskFailureSurfaced(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/storage-pools/p1/projects", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/api/v1/tasks/t9")
		writeJSON(w, http.StatusAccepted, map[string]any{"id": "t9", "kind": "create_project", "status": "pending"})
	})
	f.on("GET /api/v1/tasks/t9", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "t9", "kind": "create_project", "status": "failed", "error": "duplicate project name",
		})
	})
	_, err := f.client().CreateProjectAndWait(context.Background(), "p1", CreateProjectRequest{Name: "proj"})
	if err == nil {
		t.Fatal("expected a failed-task error")
	}
	if want := "duplicate project name"; !contains(err.Error(), want) {
		t.Errorf("error %q does not carry task error %q", err.Error(), want)
	}
}

// TestGetTaskRetriesTransientEOFThenSucceeds: the first GetTask attempt sees a
// dropped connection (EOF, as a dying/reconnecting port-forward produces); the
// client must retry rather than surface the error immediately, and end up
// with the successful result.
func TestGetTaskRetriesTransientEOFThenSucceeds(t *testing.T) {
	f := newFake()
	defer f.close()
	var gets atomic.Int32
	f.on("GET /api/v1/tasks/t1", func(w http.ResponseWriter, r *http.Request) {
		if gets.Add(1) == 1 {
			hijackDrop(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "t1", "kind": "create_project", "status": "succeeded"})
	})
	task, err := f.clientFreshConns().GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTask: unexpected error after transient EOF: %v", err)
	}
	if task.Status != TaskSucceeded {
		t.Errorf("task status = %q, want succeeded", task.Status)
	}
	if n := gets.Load(); n != 2 {
		t.Errorf("GET /tasks/t1 called %d times, want 2 (1 blip + 1 retry)", n)
	}
}

// TestCreateProjectAndWaitSurvivesPollBlipWithoutReissuingCreate: the create
// POST succeeds once; the first poll GET blips (EOF), the second succeeds.
// CreateProjectAndWait must retry only the GET and never re-issue the create
// POST — a create that already went through must not be blindly repeated.
func TestCreateProjectAndWaitSurvivesPollBlipWithoutReissuingCreate(t *testing.T) {
	f := newFake()
	defer f.close()
	var posts atomic.Int32
	f.on("POST /api/v1/storage-pools/p1/projects", func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.Header().Set("Location", "/api/v1/tasks/t1")
		writeJSON(w, http.StatusAccepted, map[string]any{"id": "t1", "kind": "create_project", "status": "pending"})
	})
	var gets atomic.Int32
	f.on("GET /api/v1/tasks/t1", func(w http.ResponseWriter, r *http.Request) {
		if gets.Add(1) == 1 {
			hijackDrop(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "t1", "kind": "create_project", "status": "succeeded"})
	})
	task, err := f.clientFreshConns().CreateProjectAndWait(context.Background(), "p1", CreateProjectRequest{Name: "proj"})
	if err != nil {
		t.Fatalf("create project and wait: %v", err)
	}
	if task.Status != TaskSucceeded {
		t.Errorf("task status = %q, want succeeded", task.Status)
	}
	if n := posts.Load(); n != 1 {
		t.Errorf("create POST issued %d times, want exactly 1 (a poll blip must never re-trigger create)", n)
	}
	if n := gets.Load(); n < 2 {
		t.Errorf("GET /tasks/t1 called %d times, want >= 2 (blip + retry)", n)
	}
}

// TestGetTaskGivesUpAfterExhaustingRetries: a connection that never recovers
// must eventually surface an error, not retry forever.
func TestGetTaskGivesUpAfterExhaustingRetries(t *testing.T) {
	f := newFake()
	defer f.close()
	var gets atomic.Int32
	f.on("GET /api/v1/tasks/dead", func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		hijackDrop(w, r)
	})
	cl := f.clientFreshConns()
	if _, err := cl.GetTask(context.Background(), "dead"); err == nil {
		t.Fatal("expected an error once retries are exhausted")
	}
	if want := int32(getTaskRetries + 1); gets.Load() != want {
		t.Errorf("GET /tasks/dead called %d times, want %d (1 + %d retries)", gets.Load(), want, getTaskRetries)
	}
}

func TestWaitTaskCtxTimeout(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/tasks/stuck", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": "stuck", "kind": "delete_project", "status": "running"})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := f.client().WaitTask(ctx, "stuck")
	if err == nil {
		t.Fatal("expected a ctx-deadline error")
	}
	if !contains(err.Error(), "stuck") {
		t.Errorf("error %q does not name the task", err.Error())
	}
}

func TestLocationHeaderPreferredOverBodyID(t *testing.T) {
	f := newFake()
	defer f.close()
	// Body id and Location id disagree; the Location id must win.
	f.on("DELETE /api/v1/storage-pools/p/projects/pr", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/api/v1/tasks/loc-id")
		writeJSON(w, http.StatusAccepted, map[string]any{"id": "body-id", "kind": "delete_project", "status": "pending"})
	})
	var polled string
	f.on("GET /api/v1/tasks/loc-id", func(w http.ResponseWriter, r *http.Request) {
		polled = r.URL.Path
		writeJSON(w, http.StatusOK, map[string]any{"id": "loc-id", "kind": "delete_project", "status": "succeeded"})
	})
	if _, err := f.client().DeleteProjectAndWait(context.Background(), "p", "pr"); err != nil {
		t.Fatalf("delete project and wait: %v", err)
	}
	if polled != "/api/v1/tasks/loc-id" {
		t.Errorf("polled %q, want the Location-derived task id", polled)
	}
}

func TestListAllEndpointsPagination(t *testing.T) {
	f := newFake()
	defer f.close()
	const total = 250
	f.on("GET /api/v1/storage-pools/p/projects/pr/branches/b/endpoints", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if limit == 0 {
			limit = 100
		}
		eps := []map[string]any{}
		for i := offset; i < offset+limit && i < total; i++ {
			eps = append(eps, map[string]any{"id": fmt.Sprintf("ep-%d", i), "role": "read_only"})
		}
		writeJSON(w, http.StatusOK, map[string]any{"endpoints": eps, "total": total, "count": len(eps)})
	})
	all, err := f.client().ListAllEndpoints(context.Background(), "p", "pr", "b")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != total {
		t.Fatalf("got %d endpoints, want %d", len(all), total)
	}
	if all[0].ID != "ep-0" || all[total-1].ID != "ep-249" {
		t.Errorf("pagination reordered: first=%q last=%q", all[0].ID, all[total-1].ID)
	}
}

func TestErrorEnvelopeDecoded(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/storage-pools/p/projects/missing", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "not_found", "message": "no such project"},
		})
	})
	_, err := f.client().GetProject(context.Background(), "p", "missing")
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusNotFound || apiErr.Code != "not_found" {
		t.Errorf("api error = %+v", apiErr)
	}
}

func TestHealth(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	f.on("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	c := f.client()
	if err := c.Healthz(context.Background()); err != nil {
		t.Errorf("healthz: %v", err)
	}
	if err := c.Readyz(context.Background()); err == nil {
		t.Errorf("readyz should error on 503")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
