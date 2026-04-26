package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"meko/api_server/internal/api"
	"meko/api_server/internal/handlers"
)

func TestAPIResponse_SuccessEnvelope(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	e, routerErr := api.NewRouter(api.ServerConfig{DB: db})
	if routerErr != nil {
		t.Fatalf("failed to create router: %v", routerErr)
	}
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var apiResp handlers.APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		t.Fatal(err)
	}

	if apiResp.Data == nil {
		t.Fatal("expected data to be non-nil")
	}
	if apiResp.Error != nil {
		t.Errorf("expected nil error, got %+v", apiResp.Error)
	}
}

func TestAPIResponse_ErrorEnvelope(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	e, routerErr := api.NewRouter(api.ServerConfig{DB: db})
	if routerErr != nil {
		t.Fatalf("failed to create router: %v", routerErr)
	}
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(
		srv.URL + "/api/v1/datapacks/nonexistent-dp-envelope",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	var apiResp handlers.APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		t.Fatal(err)
	}

	if apiResp.Error == nil {
		t.Fatal("expected error to be non-nil")
	}
	if apiResp.Error.Code != "NOT_FOUND" {
		t.Errorf("expected error code NOT_FOUND, got %s", apiResp.Error.Code)
	}
}
