package ycp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The client speaks the api_server {"data":…,"error":…} envelope: a successful
// enroll returns the identity out of "data".
func TestClient_Enroll_DecodesEnvelope(t *testing.T) {
	var gotPath, gotKind string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var req EnrollRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotKind = req.Kind
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"environment_id":"amp-acme","status_token":"ycp_sts_1","mcp_base_url":"x"},"error":null}`))
	}))
	defer srv.Close()

	resp, err := NewClient(srv.URL, nil).Enroll(context.Background(), EnrollRequest{Name: "acme", Kind: Kind})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if gotPath != "/api/v1/agent/enroll" {
		t.Errorf("enroll path = %q", gotPath)
	}
	if gotKind != "amp" {
		t.Errorf("enroll kind = %q, want amp", gotKind)
	}
	if resp.EnvironmentID != "amp-acme" || resp.StatusToken != "ycp_sts_1" {
		t.Errorf("enroll resp = %+v", resp)
	}
}

// A non-200 enroll surfaces the envelope's error message (e.g. a kind mismatch),
// not just the status code.
func TestClient_Enroll_SurfacesErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"data":null,"error":{"code":"KIND_MISMATCH","message":"environment is kind \"meko\""}}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, nil).Enroll(context.Background(), EnrollRequest{Name: "acme", Kind: Kind})
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("Enroll err = %v, want the kind-mismatch message", err)
	}
}

// A 401 on status means the token is no longer valid; the client reports it as
// ErrUnauthorized so the loop can re-enroll.
func TestClient_PostStatus_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"data":null,"error":{"code":"UNAUTHORIZED","message":"invalid status token"}}`))
	}))
	defer srv.Close()

	err := NewClient(srv.URL, nil).PostStatus(context.Background(), "amp-acme", "stale", StatusReport{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("PostStatus err = %v, want ErrUnauthorized", err)
	}
}

// A healthy status report carries the bearer token and the envId, and a 204 is
// success.
func TestClient_PostStatus_Success(t *testing.T) {
	var gotAuth, gotEnv string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotEnv = r.URL.Query().Get("envId")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := NewClient(srv.URL, nil).PostStatus(context.Background(), "amp-acme", "ycp_sts_1",
		StatusReport{EnvironmentState: StateReady, Resources: []ResourceStatus{{Kind: "storage_pool", ID: "p1"}}})
	if err != nil {
		t.Fatalf("PostStatus: %v", err)
	}
	if gotAuth != "Bearer ycp_sts_1" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotEnv != "amp-acme" {
		t.Errorf("envId = %q", gotEnv)
	}
}
