package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeController stands in for the AMP controller: it records the last resize
// request body and serves a canned task + endpoint read-back, so the client's
// resize flow (POST → poll task → extract in-place outcome → GET endpoint) can
// be exercised without a live controller.
type fakeController struct {
	resizeBody map[string]any
	// result is the task's final `result` payload (in_place_resize / message).
	result map[string]any
	// endpoint is returned by the endpoint read-back GET.
	endpoint AMPEndpoint
}

func (f *fakeController) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	const epPath = "/api/v1/storage-pools/p/clusters/pr/branches/b/endpoints/e"
	mux.HandleFunc(epPath+"/resize", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("resize: method = %s, want POST", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&f.resizeBody)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"t1","kind":"resize_endpoint","status":"pending"}`))
	})
	mux.HandleFunc("/api/v1/tasks/t1", func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{"id": "t1", "kind": "resize_endpoint", "status": "succeeded"}
		if f.result != nil {
			resp["result"] = f.result
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc(epPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(f.endpoint)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDeleteClusterAsync_IssuesDeleteWithoutPolling(t *testing.T) {
	var deleted bool
	var polled bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/storage-pools/p/clusters/pr", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		deleted = true
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"t7","kind":"delete_cluster","status":"pending"}`))
	})
	mux.HandleFunc("/api/v1/tasks/t7", func(w http.ResponseWriter, _ *http.Request) {
		polled = true // must NOT be hit — async delete does not poll
		_, _ = w.Write([]byte(`{"id":"t7","status":"succeeded"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewAMPClient(srv.URL, http.DefaultClient)

	if err := c.DeleteClusterAsync(context.Background(), "p", "pr"); err != nil {
		t.Fatalf("DeleteClusterAsync: %v", err)
	}
	if !deleted {
		t.Error("expected the DELETE to be issued")
	}
	if polled {
		t.Error("DeleteClusterAsync must not poll /tasks")
	}
}

func TestCreateClusterWithWriter_SendsWithWriterAndPollsToCluster(t *testing.T) {
	var body map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/storage-pools/p/clusters", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"t9","kind":"create_cluster","status":"pending"}`))
	})
	mux.HandleFunc("/api/v1/tasks/t9", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"t9","kind":"create_cluster","status":"succeeded","cluster_id":"pr9"}`))
	})
	mux.HandleFunc("/api/v1/storage-pools/p/clusters/pr9", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AMPCluster{ID: "pr9", Name: "orders"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewAMPClient(srv.URL, http.DefaultClient)

	// Polls the task to completion and returns the materialized cluster. Default
	// engine (postgres) and no extensions: neither key is sent, so the default
	// path stays byte-for-byte identical to before engine/extensions existed.
	p, err := c.CreateClusterWithWriter(context.Background(), "p", "orders", "s3cr3t-pw", nil, "4", "8Gi", "", nil)
	if err != nil {
		t.Fatalf("CreateClusterWithWriter: %v", err)
	}
	if p.ID != "pr9" {
		t.Errorf("cluster id = %q, want pr9", p.ID)
	}
	// with_writer must be true and cpu/memory nested under `resources`.
	if body["with_writer"] != true {
		t.Errorf("with_writer = %v, want true", body["with_writer"])
	}
	// The writer is read_write, so the cloud_admin password must be in the body.
	if body["password"] != "s3cr3t-pw" {
		t.Errorf("password = %v, want s3cr3t-pw", body["password"])
	}
	res, _ := body["resources"].(map[string]any)
	if res == nil || res["cpu"] != "4" || res["memory"] != "8Gi" {
		t.Errorf("resources = %v, want cpu=4 memory=8Gi", body["resources"])
	}
	if _, ok := body["engine"]; ok {
		t.Errorf("engine key present for default engine: %v", body["engine"])
	}
	if _, ok := body["extensions"]; ok {
		t.Errorf("extensions key present with none picked: %v", body["extensions"])
	}
}

// TestCreateCluster_SendsEngineAndExtensions asserts the opt-in path: a
// non-default engine and picked extensions are forwarded, while a default
// "postgres" engine and blank extension names are dropped.
func TestCreateCluster_SendsEngineAndExtensions(t *testing.T) {
	var body map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/storage-pools/p/clusters", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"t1","kind":"create_cluster","status":"pending"}`))
	})
	mux.HandleFunc("/api/v1/tasks/t1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"t1","kind":"create_cluster","status":"succeeded","cluster_id":"c1"}`))
	})
	mux.HandleFunc("/api/v1/storage-pools/p/clusters/c1", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AMPCluster{ID: "c1", Name: "vec"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewAMPClient(srv.URL, http.DefaultClient)

	if _, err := c.CreateCluster(context.Background(), "p", "vec", nil, "aidb", []string{"pgvector", "  ", "age"}); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if body["engine"] != "aidb" {
		t.Errorf("engine = %v, want aidb", body["engine"])
	}
	exts, _ := body["extensions"].([]any)
	if len(exts) != 2 || exts[0] != "pgvector" || exts[1] != "age" {
		t.Errorf("extensions = %v, want [pgvector age] (blanks dropped)", body["extensions"])
	}

	// A default "postgres" engine is omitted so the common-path body is
	// unchanged (the controller applies the same default either way).
	body = nil
	if _, err := c.CreateCluster(context.Background(), "p", "vec", nil, "postgres", nil); err != nil {
		t.Fatalf("CreateCluster (default engine): %v", err)
	}
	if _, ok := body["engine"]; ok {
		t.Errorf("engine key present for postgres default: %v", body["engine"])
	}
}

// TestNormalizeAMPBaseURL covers the missing-scheme repair that unblocks a bare
// host registered in meko_system.amp_registration (e.g. "100.84.82.87").
func TestNormalizeAMPBaseURL(t *testing.T) {
	cases := map[string]string{
		"100.84.82.87":      "http://100.84.82.87",
		"100.84.82.87:8080": "http://100.84.82.87:8080",
		"  100.84.82.87/  ": "http://100.84.82.87",
		"http://host:8080":  "http://host:8080",
		"https://host":      "https://host",
		"http://host/":      "http://host",
		"":                  "",
	}
	for in, want := range cases {
		if got := NormalizeAMPBaseURL(in); got != want {
			t.Errorf("NormalizeAMPBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResizeEndpoint_NestsResourcesAndReadsBack(t *testing.T) {
	f := &fakeController{
		result:   map[string]any{"in_place_resize": "completed", "in_place_message": "ok"},
		endpoint: AMPEndpoint{ID: "e", Role: "read_write", Resources: &AMPResources{CPU: "4", Memory: "8Gi"}},
	}
	c := NewAMPClient(f.server(t).URL, http.DefaultClient)

	res, err := c.ResizeEndpoint(context.Background(), "p", "pr", "b", "e", "4", "8Gi")
	if err != nil {
		t.Fatalf("ResizeEndpoint: %v", err)
	}

	// Request body must nest cpu/memory under `resources` (AMP's contract).
	wantBody := map[string]any{"resources": map[string]any{"cpu": "4", "memory": "8Gi"}}
	if got, _ := json.Marshal(f.resizeBody); string(got) != mustJSON(t, wantBody) {
		t.Errorf("resize body = %s, want %s", got, mustJSON(t, wantBody))
	}
	// Outcome + message come from the task result; endpoint is the read-back.
	if res.Outcome != "completed" || res.Message != "ok" {
		t.Errorf("outcome/message = %q/%q, want completed/ok", res.Outcome, res.Message)
	}
	if res.Endpoint.Resources == nil || res.Endpoint.Resources.CPU != "4" || res.Endpoint.Resources.Memory != "8Gi" {
		t.Errorf("endpoint resources = %+v, want cpu=4 memory=8Gi", res.Endpoint.Resources)
	}
}

func TestResizeEndpoint_OmitsUnsetFieldAndDefaultsOutcome(t *testing.T) {
	// Only memory is sent; and with no task result the outcome defaults to
	// "completed" rather than being left blank.
	f := &fakeController{endpoint: AMPEndpoint{ID: "e"}}
	c := NewAMPClient(f.server(t).URL, http.DefaultClient)

	res, err := c.ResizeEndpoint(context.Background(), "p", "pr", "b", "e", "", "8Gi")
	if err != nil {
		t.Fatalf("ResizeEndpoint: %v", err)
	}
	if _, hasCPU := f.resizeBody["resources"].(map[string]any)["cpu"]; hasCPU {
		t.Errorf("resize body should omit cpu when unset: %v", f.resizeBody)
	}
	if res.Outcome != "completed" {
		t.Errorf("outcome = %q, want completed (default)", res.Outcome)
	}
}

func TestResizeEndpoint_PropagatesInfeasibleOutcome(t *testing.T) {
	f := &fakeController{
		result:   map[string]any{"in_place_resize": "infeasible", "in_place_message": "node too small"},
		endpoint: AMPEndpoint{ID: "e"},
	}
	c := NewAMPClient(f.server(t).URL, http.DefaultClient)

	res, err := c.ResizeEndpoint(context.Background(), "p", "pr", "b", "e", "64", "256Gi")
	if err != nil {
		t.Fatalf("ResizeEndpoint: %v", err)
	}
	if res.Outcome != "infeasible" || !strings.Contains(res.Message, "node too small") {
		t.Errorf("outcome/message = %q/%q, want infeasible/…node too small…", res.Outcome, res.Message)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
