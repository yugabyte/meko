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
	const epPath = "/api/v1/storage-pools/p/projects/pr/branches/b/endpoints/e"
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
