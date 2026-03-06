package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"meko/api_server/internal/handlers"
)

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	data := map[string]string{"status": "ok"}

	handlers.WriteJSON(w, http.StatusOK, data)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", ct)
	}

	var resp handlers.APIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("expected nil error, got %+v", resp.Error)
	}
	if resp.Data == nil {
		t.Fatal("expected data to be non-nil")
	}
}

func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()

	handlers.WriteError(
		w, http.StatusBadRequest, "VALIDATION_ERROR", "name is required",
	)

	if w.Code != http.StatusBadRequest {
		t.Errorf(
			"expected status %d, got %d",
			http.StatusBadRequest, w.Code,
		)
	}

	var resp handlers.APIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error == nil {
		t.Fatal("expected error to be non-nil")
	}
	if resp.Error.Code != "VALIDATION_ERROR" {
		t.Errorf(
			"expected error code VALIDATION_ERROR, got %s",
			resp.Error.Code,
		)
	}
	if resp.Error.Message != "name is required" {
		t.Errorf(
			"expected message 'name is required', got %s",
			resp.Error.Message,
		)
	}
}

func TestWriteErrorWithDetails(t *testing.T) {
	w := httptest.NewRecorder()
	details := []string{"field 'name' is empty", "field 'model' is empty"}

	handlers.WriteErrorWithDetails(
		w, http.StatusUnprocessableEntity,
		"VALIDATION_ERROR", "invalid input", details,
	)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf(
			"expected status %d, got %d",
			http.StatusUnprocessableEntity, w.Code,
		)
	}

	var resp handlers.APIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error == nil {
		t.Fatal("expected error to be non-nil")
	}
	if len(resp.Error.Details) != 2 {
		t.Errorf("expected 2 details, got %d", len(resp.Error.Details))
	}
}
