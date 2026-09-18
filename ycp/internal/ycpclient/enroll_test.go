package ycpclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"meko/ycp/internal/api"
)

// The control plane degrades an environment that ships nowhere rather than refusing
// it, so there is no telemetry code to be fatal about here. What still matters is
// that an unrecognised 409 does not become one: only KIND_MISMATCH is permanent.
func TestEnroll_AnUnknownConflictCodeIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "SOMETHING_ELSE", "message": "not a kind mismatch"},
		})
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k", nil).Enroll(context.Background(), api.EnrollRequest{Name: "amp-acme-prod"})
	if errors.Is(err, ErrKindMismatch) {
		t.Fatalf("an unrelated 409 code was treated as a kind mismatch: %v", err)
	}
	if err == nil {
		t.Fatal("a 409 was treated as success")
	}
}

// A 409 from anything but this control plane's own check — a proxy, an ingress —
// carries no code, and must stay retryable.
func TestEnroll_A409WithNoCodeIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("<html>conflict</html>"))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k", nil).Enroll(context.Background(), api.EnrollRequest{Name: "amp-acme-prod"})
	if errors.Is(err, ErrKindMismatch) {
		t.Fatalf("an uncoded 409 was treated as a permanent refusal: %v", err)
	}
	if err == nil {
		t.Fatal("an uncoded 409 was treated as success")
	}
}

// The attestation has to reach the control plane, not just exist in the struct.
func TestEnroll_SendsTheTelemetryAttestation(t *testing.T) {
	var got api.EnrollRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"environment_id": "amp-acme-prod"},
		})
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k", nil).Enroll(context.Background(), api.EnrollRequest{
		Name:      "amp-acme-prod",
		Telemetry: &api.Telemetry{OtelEnabled: true, ByocEnabled: true, MetricsEndpoint: "https://byoc/otlp"},
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if got.Telemetry == nil || !got.Telemetry.ByocEnabled || got.Telemetry.MetricsEndpoint != "https://byoc/otlp" {
		t.Errorf("the attestation did not reach the control plane: %+v", got.Telemetry)
	}
}
