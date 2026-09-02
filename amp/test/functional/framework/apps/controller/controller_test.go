package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

func TestCreateClusterAndWaitSuccess(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/storage-pools/p1/clusters", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/api/v1/tasks/t1")
		writeJSON(w, http.StatusAccepted, map[string]any{"id": "t1", "kind": "create_cluster", "status": "pending"})
	})
	var polls int
	f.on("GET /api/v1/tasks/t1", func(w http.ResponseWriter, _ *http.Request) {
		polls++
		status := "running"
		if polls >= 2 {
			status = "succeeded"
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "t1", "kind": "create_cluster", "status": status})
	})
	task, err := f.client().CreateClusterAndWait(context.Background(), "p1", CreateClusterRequest{Name: "proj"})
	if err != nil {
		t.Fatalf("create cluster and wait: %v", err)
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
	f.on("POST /api/v1/storage-pools/p1/clusters/pr1/branches/b1/endpoints", func(w http.ResponseWriter, r *http.Request) {
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
	f.on("POST /api/v1/storage-pools/p1/clusters", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/api/v1/tasks/t9")
		writeJSON(w, http.StatusAccepted, map[string]any{"id": "t9", "kind": "create_cluster", "status": "pending"})
	})
	f.on("GET /api/v1/tasks/t9", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "t9", "kind": "create_cluster", "status": "failed", "error": "duplicate cluster name",
		})
	})
	_, err := f.client().CreateClusterAndWait(context.Background(), "p1", CreateClusterRequest{Name: "proj"})
	if err == nil {
		t.Fatal("expected a failed-task error")
	}
	if want := "duplicate cluster name"; !contains(err.Error(), want) {
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
		writeJSON(w, http.StatusOK, map[string]any{"id": "t1", "kind": "create_cluster", "status": "succeeded"})
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

// TestCreateClusterAndWaitSurvivesPollBlipWithoutReissuingCreate: the create
// POST succeeds once; the first poll GET blips (EOF), the second succeeds.
// CreateClusterAndWait must retry only the GET and never re-issue the create
// POST — a create that already went through must not be blindly repeated.
func TestCreateClusterAndWaitSurvivesPollBlipWithoutReissuingCreate(t *testing.T) {
	f := newFake()
	defer f.close()
	var posts atomic.Int32
	f.on("POST /api/v1/storage-pools/p1/clusters", func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.Header().Set("Location", "/api/v1/tasks/t1")
		writeJSON(w, http.StatusAccepted, map[string]any{"id": "t1", "kind": "create_cluster", "status": "pending"})
	})
	var gets atomic.Int32
	f.on("GET /api/v1/tasks/t1", func(w http.ResponseWriter, r *http.Request) {
		if gets.Add(1) == 1 {
			hijackDrop(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "t1", "kind": "create_cluster", "status": "succeeded"})
	})
	task, err := f.clientFreshConns().CreateClusterAndWait(context.Background(), "p1", CreateClusterRequest{Name: "proj"})
	if err != nil {
		t.Fatalf("create cluster and wait: %v", err)
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
		writeJSON(w, http.StatusOK, map[string]any{"id": "stuck", "kind": "delete_cluster", "status": "running"})
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
	f.on("DELETE /api/v1/storage-pools/p/clusters/pr", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/api/v1/tasks/loc-id")
		writeJSON(w, http.StatusAccepted, map[string]any{"id": "body-id", "kind": "delete_cluster", "status": "pending"})
	})
	var polled string
	f.on("GET /api/v1/tasks/loc-id", func(w http.ResponseWriter, r *http.Request) {
		polled = r.URL.Path
		writeJSON(w, http.StatusOK, map[string]any{"id": "loc-id", "kind": "delete_cluster", "status": "succeeded"})
	})
	if _, err := f.client().DeleteClusterAndWait(context.Background(), "p", "pr"); err != nil {
		t.Fatalf("delete cluster and wait: %v", err)
	}
	if polled != "/api/v1/tasks/loc-id" {
		t.Errorf("polled %q, want the Location-derived task id", polled)
	}
}

func TestListAllEndpointsPagination(t *testing.T) {
	f := newFake()
	defer f.close()
	const total = 250
	f.on("GET /api/v1/storage-pools/p/clusters/pr/branches/b/endpoints", func(w http.ResponseWriter, r *http.Request) {
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
	f.on("GET /api/v1/storage-pools/p/clusters/missing", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "not_found", "message": "no such cluster"},
		})
	})
	_, err := f.client().GetCluster(context.Background(), "p", "missing")
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

// TestResizeEndpointSendsResourcesAndReadsOutcome: the resize hits the endpoint's
// /resize route with the requested dimensions, and the settled task's in-place
// outcome is decoded off GET /tasks/{id}'s result.
func TestResizeEndpointSendsResourcesAndReadsOutcome(t *testing.T) {
	f := newFake()
	defer f.close()
	var body map[string]any
	f.on("POST /api/v1/storage-pools/p1/clusters/pr1/branches/b1/endpoints/e1/resize", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Location", "/api/v1/tasks/t7")
		writeJSON(w, http.StatusAccepted, map[string]any{"id": "t7", "kind": "resize_endpoint", "status": "pending"})
	})
	f.on("GET /api/v1/tasks/t7", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "t7", "kind": "resize_endpoint", "status": "succeeded",
			"result": map[string]any{"in_place_resize": "completed", "in_place_message": "resized in place"},
		})
	})
	task, err := f.client().ResizeEndpointAndWait(context.Background(), "p1", "pr1", "b1", "e1",
		ResizeEndpointRequest{Resources: EndpointResources{CPU: "4", Memory: "8Gi"}})
	if err != nil {
		t.Fatalf("resize endpoint and wait: %v", err)
	}
	res, _ := body["resources"].(map[string]any)
	if res["cpu"] != "4" || res["memory"] != "8Gi" {
		t.Errorf("request body resources = %v, want cpu=4 memory=8Gi", body["resources"])
	}
	outcome, message := task.InPlaceResize()
	if outcome != ResizeCompleted || message != "resized in place" {
		t.Errorf("in-place outcome = %q (%q), want %q", outcome, message, ResizeCompleted)
	}
}

// TestResizeEndpointOmitsUnsetDimension: an unset dimension must not be sent at
// all — the controller keeps the endpoint's current value only for an ABSENT
// field, so serializing an empty string would be a different request.
func TestResizeEndpointOmitsUnsetDimension(t *testing.T) {
	f := newFake()
	defer f.close()
	var body map[string]any
	f.on("POST /api/v1/storage-pools/p/clusters/pr/branches/b/endpoints/e/resize", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, http.StatusAccepted, map[string]any{"id": "t1", "kind": "resize_endpoint", "status": "pending"})
	})
	if _, _, err := f.client().ResizeEndpoint(context.Background(), "p", "pr", "b", "e",
		ResizeEndpointRequest{Resources: EndpointResources{CPU: "3"}}); err != nil {
		t.Fatalf("resize endpoint: %v", err)
	}
	res, _ := body["resources"].(map[string]any)
	if _, present := res["memory"]; present {
		t.Errorf("request body resources = %v, want no memory key when memory is unset", res)
	}
}

// TestResizeEndpointConflictSurfaced: a concurrent resize is rejected 409
// (first-writer-wins), which the caller must be able to tell from any other error.
func TestResizeEndpointConflictSurfaced(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/storage-pools/p/clusters/pr/branches/b/endpoints/e/resize", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{"code": "conflict", "message": "a resize is already in progress for this endpoint"},
		})
	})
	_, _, err := f.client().ResizeEndpoint(context.Background(), "p", "pr", "b", "e",
		ResizeEndpointRequest{Resources: EndpointResources{CPU: "3"}})
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusConflict || apiErr.Code != "conflict" {
		t.Errorf("api error = %+v, want 409 conflict", apiErr)
	}
}

// TestEndpointResourcesAbsentOnOlderController: an endpoint response without
// `resources` decodes to a nil pointer — the signal a build predates in-place
// resize, which the resize tests gate on.
func TestEndpointResourcesAbsentOnOlderController(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/storage-pools/p/clusters/pr/branches/b/endpoints/e", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": "e", "role": "read_write"})
	})
	ep, err := f.client().GetEndpoint(context.Background(), "p", "pr", "b", "e")
	if err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	if ep.Resources != nil {
		t.Errorf("endpoint resources = %+v, want nil when the response carries no resources", ep.Resources)
	}
}

// The omitted-vs-empty distinction is the whole contract of these two PATCHes:
// the controller reads a missing surface as "leave it alone" and `{}` as "clear
// it". So a request must carry only what the caller named — sending a surface it
// never set would replace that surface's stored settings with an empty one,
// which for the daemons means an endpoint quietly losing its tuning.
func TestUpdateSettingsSendsOnlyTheNamedSurface(t *testing.T) {
	f := newFake()
	defer f.close()

	var body map[string]json.RawMessage
	f.on("PATCH /api/v1/storage-pools/p/clusters/c/branches/b/endpoints/e/settings",
		func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			_, _ = w.Write([]byte(`{"applied":true}`))
		})

	pg := map[string]string{"work_mem": "64MB"}
	got, err := f.client().UpdateEndpointSettings(context.Background(), "p", "c", "b", "e",
		UpdateSettingsRequest{PgSettings: &pg})
	if err != nil {
		t.Fatalf("UpdateEndpointSettings: %v", err)
	}
	if !got.Applied {
		t.Error("applied must decode as true")
	}
	if _, ok := body["pg_settings"]; !ok {
		t.Error("pg_settings must be sent")
	}
	// The endpoint route has no write lease — that surface is the branch's alone
	// — so these three are what an unnamed surface could wrongly clear here.
	for _, k := range []string{"pageserver_settings", "safekeeper_settings"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s was sent but never set — the controller would read that as an edit", k)
		}
	}
	if _, ok := body["write_token_settings"]; ok {
		t.Error("write_token_settings was sent on an endpoint PATCH, which has no such surface")
	}
}

// An empty object is a real instruction ("clear this surface"), and it has to
// survive serialisation as one rather than being dropped as if omitted.
func TestUpdateSettingsSendsAnEmptyObjectAsAClear(t *testing.T) {
	f := newFake()
	defer f.close()

	var raw string
	f.on("PATCH /api/v1/storage-pools/p/clusters/c/branches/b/settings",
		func(w http.ResponseWriter, r *http.Request) {
			b := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(b)
			raw = string(b)
			_, _ = w.Write([]byte(`{"applied":true}`))
		})

	empty := map[string]string{}
	if _, err := f.client().UpdateBranchSettings(context.Background(), "p", "c", "b",
		UpdateSettingsRequest{PgSettings: &empty}); err != nil {
		t.Fatalf("UpdateBranchSettings: %v", err)
	}
	if want := `"pg_settings":{}`; !strings.Contains(raw, want) {
		t.Errorf("body %s does not carry %s", raw, want)
	}
}

// Every field of the per-endpoint outcome is a fact the caller cannot get any
// other way — which services were bounced, which took the change while running,
// which setting cost the restart, and which settings are stored and doing
// nothing. A decode that dropped one would read as a clean success.
func TestUpdateSettingsDecodesEveryOutcomeField(t *testing.T) {
	f := newFake()
	defer f.close()

	f.on("PATCH /api/v1/storage-pools/p/clusters/c/branches/b/settings",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"applied":true,"endpoints":[
				{"endpoint_id":"e1","reloaded":["compute_ctl"]},
				{"endpoint_id":"e2","restarted":["compute_ctl"],"restart_forced_by":["shared_buffers"]},
				{"endpoint_id":"e3","not_applied":[{"name":"block_size","reason":"cannot be set in a configuration file"}]},
				{"endpoint_id":"e4","unreachable":"connection refused"}
			]}`))
		})

	pg := map[string]string{"shared_buffers": "512MB"}
	got, err := f.client().UpdateBranchSettings(context.Background(), "p", "c", "b",
		UpdateSettingsRequest{PgSettings: &pg})
	if err != nil {
		t.Fatalf("UpdateBranchSettings: %v", err)
	}
	if len(got.Endpoints) != 4 {
		t.Fatalf("want 4 outcomes, got %d", len(got.Endpoints))
	}
	if e := got.Endpoints[0]; len(e.Reloaded) != 1 || e.Reloaded[0] != "compute_ctl" {
		t.Errorf("reloaded = %v", e.Reloaded)
	}
	if e := got.Endpoints[1]; len(e.Restarted) != 1 || len(e.RestartForcedBy) != 1 ||
		e.RestartForcedBy[0] != "shared_buffers" {
		t.Errorf("restarted = %v, forced by = %v", e.Restarted, e.RestartForcedBy)
	}
	if e := got.Endpoints[2]; len(e.NotApplied) != 1 || e.NotApplied[0].Name != "block_size" ||
		e.NotApplied[0].Reason == "" {
		t.Errorf("not_applied = %v", e.NotApplied)
	}
	if e := got.Endpoints[3]; e.Unreachable == "" {
		t.Error("unreachable must decode")
	}
}

// unavailable writes the controller's state-store 503 verbatim. No Retry-After:
// the header is whole seconds, too coarse for a unit test.
func unavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{
		"code": "service_unavailable", "message": "state store unavailable; retry the request",
	}})
}

func fastRetry(c *Client) *Client {
	c.UnavailableBudget = 2 * time.Second
	c.UnavailableDelay = time.Millisecond
	return c
}

// Waited out on a write too, not just the idempotent methods.
func TestCreateEndpointWaitsOutStoreUnavailable(t *testing.T) {
	f := newFake()
	defer f.close()

	var calls atomic.Int32
	f.on("POST /api/v1/storage-pools/p/clusters/c/branches/b/endpoints",
		func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) <= 2 {
				unavailable(w)
				return
			}
			writeJSON(w, http.StatusAccepted, Task{ID: "t1", Status: TaskSucceeded})
		})

	cl := fastRetry(f.client())
	task, _, err := cl.CreateEndpoint(context.Background(), "p", "c", "b", CreateEndpointRequest{Role: "read_write"})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if task.ID != "t1" {
		t.Errorf("task id = %q, want t1", task.ID)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3 (two 503s then the accept)", got)
	}
}

// A store that never comes back must surface the 503, not hang to the test's
// own timeout.
func TestStoreUnavailableRetryIsBounded(t *testing.T) {
	f := newFake()
	defer f.close()

	f.on("GET /api/v1/storage-pools/p/clusters/c",
		func(w http.ResponseWriter, _ *http.Request) { unavailable(w) })

	cl := f.client()
	cl.UnavailableBudget = 50 * time.Millisecond
	cl.UnavailableDelay = 10 * time.Millisecond

	start := time.Now()
	_, err := cl.GetCluster(context.Background(), "p", "c")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want the 503 to surface once the budget is spent")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("err = %v, want a 503 APIError", err)
	}
	if elapsed > time.Second {
		t.Errorf("gave up after %s, want the budget to bound it", elapsed)
	}
}

// Every controller 503 carries code "service_unavailable", so matching the code
// alone would swallow answers that promise nothing about the request landing.
func TestOnlyTheStateStore503IsWaitedOut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		code    string
		message string
	}{
		// internal/api/server.go
		{"readyz database unavailable", http.StatusServiceUnavailable, "service_unavailable", "database unavailable"},
		// internal/api/handlers.go
		{"safekeeper not serving", http.StatusServiceUnavailable, "service_unavailable",
			"the parent branch's endpoint is not serving its safekeeper API yet; retry the fork"},
		{"bare 503 from a proxy", http.StatusServiceUnavailable, "", ""},
		{"500 outcome unknown", http.StatusInternalServerError, "internal", "internal error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			defer f.close()

			var calls atomic.Int32
			f.on("POST /api/v1/storage-pools/p/clusters/c/branches/b/endpoints",
				func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					writeJSON(w, tc.status, map[string]any{"error": map[string]string{
						"code": tc.code, "message": tc.message,
					}})
				})

			cl := fastRetry(f.client())
			if _, _, err := cl.CreateEndpoint(context.Background(), "p", "c", "b", CreateEndpointRequest{}); err == nil {
				t.Fatal("want the error to surface")
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("attempts = %d, want 1 (no retry)", got)
			}
		})
	}
}

// waitControllerReady polls Readyz on a 1s cadence and re-resolves a dead
// controller address halfway through its budget; one retry sitting on the full
// budget consumes the window that recovery depends on.
func TestReadyzIsNotWaitedOut(t *testing.T) {
	f := newFake()
	defer f.close()

	var calls atomic.Int32
	f.on("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{
			"code": "service_unavailable", "message": "database unavailable",
		}})
	})

	cl := f.client()
	cl.UnavailableBudget = time.Minute
	cl.UnavailableDelay = 2 * time.Second

	start := time.Now()
	if err := cl.Readyz(context.Background()); err == nil {
		t.Fatal("want the readiness verdict to surface")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Readyz blocked %s, want it to answer at once", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestRetryAfterHonoursTheHeader(t *testing.T) {
	cl := New("http://example.invalid")
	cl.UnavailableDelay = 2 * time.Second

	for _, tc := range []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"absent", "", 2 * time.Second},
		{"garbage", "soon", 2 * time.Second},
		{"zero", "0", 2 * time.Second},
		{"negative", "-5", 2 * time.Second},
		{"honoured above the delay", "5", 5 * time.Second},
		{"capped", "600", unavailableMaxWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hdr := http.Header{}
			if tc.value != "" {
				hdr.Set("Retry-After", tc.value)
			}
			if got := cl.retryAfter(hdr); got != tc.want {
				t.Errorf("retryAfter(%q) = %s, want %s", tc.value, got, tc.want)
			}
		})
	}
	if got := cl.retryAfter(nil); got != 2*time.Second {
		t.Errorf("retryAfter(nil) = %s, want the client delay", got)
	}
}

// captureControllerHang logs this error before SIGQUITing the pod, so losing the
// 503 turns "backing is down" into "controller hung".
func TestCancelledRetryKeepsTheAPIError(t *testing.T) {
	f := newFake()
	defer f.close()

	served := make(chan struct{})
	var once sync.Once
	f.on("GET /api/v1/storage-pools/p/clusters/c", func(w http.ResponseWriter, _ *http.Request) {
		unavailable(w)
		once.Do(func() { close(served) })
	})

	// Delay under budget so the loop reaches its sleep, then cancel from there:
	// a deadline racing the request instead would cancel before any 503 exists.
	cl := f.client()
	cl.UnavailableBudget = 2 * time.Hour
	cl.UnavailableDelay = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-served
		// Margin for the client to finish reading the 503 before cancelling, so
		// the cancel lands in the retry sleep and not in the request.
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := cl.GetCluster(ctx, "p", "c")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to carry the context cause", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want the 503 preserved as the diagnostic", err)
	}
	if !strings.Contains(apiErr.Message, "state store unavailable") {
		t.Errorf("APIError message = %q, want the state-store reason", apiErr.Message)
	}
}

func TestRetryResendsTheRequestBody(t *testing.T) {
	f := newFake()
	defer f.close()

	var bodies []string
	var mu sync.Mutex
	f.on("POST /api/v1/storage-pools/p/clusters/c/branches/b/endpoints",
		func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(b))
			n := len(bodies)
			mu.Unlock()
			if n == 1 {
				unavailable(w)
				return
			}
			writeJSON(w, http.StatusAccepted, Task{ID: "t1", Status: TaskSucceeded})
		})

	cl := fastRetry(f.client())
	if _, _, err := cl.CreateEndpoint(context.Background(), "p", "c", "b",
		CreateEndpointRequest{Role: "read_write"}); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("attempts = %d, want 2", len(bodies))
	}
	if bodies[0] != bodies[1] {
		t.Errorf("retry sent a different body:\n first = %s\nsecond = %s", bodies[0], bodies[1])
	}
	if !strings.Contains(bodies[1], "read_write") {
		t.Errorf("retried body lost its content: %s", bodies[1])
	}
}

func TestStoreUnavailableRetryHonorsContext(t *testing.T) {
	f := newFake()
	defer f.close()

	f.on("GET /api/v1/storage-pools/p/clusters/c",
		func(w http.ResponseWriter, _ *http.Request) { unavailable(w) })

	cl := f.client()
	cl.UnavailableBudget = time.Minute
	cl.UnavailableDelay = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := cl.GetCluster(ctx, "p", "c"); err == nil {
		t.Fatal("want an error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("blocked %s past the context deadline", elapsed)
	}
}

// The framework reaches the controller by exec'ing curl in a relay pod, so an
// apiserver-to-kubelet hiccup surfaces as kubectl's text, not a wrapped error.
// One such blip failed stress/commit_durability on its first task poll — the
// most retry-safe call there is.
func TestIsTransientConnErrClassifiesTimeouts(t *testing.T) {
	relayTimeout := errors.New(`curl GET /api/v1/tasks/fc3422bb: exited 1: error: ` +
		`Internal error occurred: error sending request: Post ` +
		`"https://10.150.0.170:10250/exec/ampqa-cp-dffbaec6/ampqa-http-relay/relay?command=sh": ` +
		`dial tcp 10.150.0.170:10250: i/o timeout`)

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"relay exec dial timeout", relayTimeout, true},
		// The controller pod is being rescheduled (a node scale-down): curl in the
		// relay pod gets exit 7 with this text and no wrapped errno.
		{"relay curl cannot connect", errors.New(`curl GET /api/v1/tasks/cfa33b92 exited 7: curl: (7) Failed to connect to amp-controller port 8080 after 0 ms: Could not connect to server`), true},
		{"net deadline", os.ErrDeadlineExceeded, true},
		{"syscall timeout", syscall.ETIMEDOUT, true},
		{"net.Error that times out", &net.DNSError{IsTimeout: true}, true},
		{"wrapped timeout", fmt.Errorf("get task: %w", os.ErrDeadlineExceeded), true},
		{"eof", io.EOF, true},
		{"refused", syscall.ECONNREFUSED, true},
		{"reset", syscall.ECONNRESET, true},
		// The caller's own budget, not a blip. context.DeadlineExceeded reports
		// Timeout() true, so a bare net.Error check would retry past it.
		{"caller deadline", context.DeadlineExceeded, false},
		{"caller cancel", context.Canceled, false},
		{"wrapped caller deadline", fmt.Errorf("get task: %w", context.DeadlineExceeded), false},
		{"real api error", &APIError{StatusCode: 500, Code: "internal"}, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientConnErr(tc.err); got != tc.want {
				t.Errorf("isTransientConnErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// GetTask retries the relay timeout rather than failing the test on it.
func TestGetTaskRetriesRelayTimeout(t *testing.T) {
	f := newFake()
	defer f.close()

	var calls atomic.Int32
	f.on("GET /api/v1/tasks/t1", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// The transport-level shape of the relay failure: no HTTP response.
			hijackDrop(w, r)
			return
		}
		writeJSON(w, http.StatusOK, Task{ID: "t1", Status: TaskSucceeded})
	})

	task, err := f.clientFreshConns().GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != TaskSucceeded {
		t.Errorf("status = %q, want %q", task.Status, TaskSucceeded)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// A caller's expired deadline must not be retried past, however many attempts
// are left.
func TestGetTaskDoesNotRetryCallerDeadline(t *testing.T) {
	f := newFake()
	defer f.close()

	var calls atomic.Int32
	f.on("GET /api/v1/tasks/t1", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		hijackDrop(w, r)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := f.clientFreshConns().GetTask(ctx, "t1"); err == nil {
		t.Fatal("want an error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("blocked %s past the caller deadline", elapsed)
	}
}

// Tasks survive a controller restart, so a poll that cannot reach it keeps
// polling: the task must still be observed once the controller answers again.
func TestWaitTaskRidesOutAnUnreachableController(t *testing.T) {
	f := newFake()
	defer f.close()
	var calls int
	f.on("GET /api/v1/tasks/t1", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= getTaskRetries+1 {
			hijackDrop(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "t1", "kind": "create_endpoint", "status": "succeeded"})
	})
	task, err := f.client().WaitTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("an unreachable controller that comes back must not fail the wait: %v", err)
	}
	if task.Status != TaskSucceeded {
		t.Errorf("status = %q, want succeeded", task.Status)
	}
}

// A served API error is the controller answering, not a blip: it must fail at
// once rather than burn the unreachable budget.
func TestWaitTaskFailsFastOnAServedAPIError(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/tasks/t1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": "not_found", "message": "no such task"})
	})
	start := time.Now()
	var apiErr *APIError
	_, err := f.client().WaitTask(context.Background(), "t1")
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("a 404 must surface as an *APIError, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("a served error must fail fast, took %s", elapsed)
	}
}
