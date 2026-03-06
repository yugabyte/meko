package handlers_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/lib/pq"

	"meko/api_server/internal/handlers"
)

const testConnStr = "postgresql://localhost:5433/yugabyte?user=yugabyte&password=yugabyte&sslmode=disable"

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testConnStr)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("failed to ping test database: %v", err)
	}
	return db
}

func TestHealthHandler_WithDB(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	handler := handlers.HealthHandler(db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp handlers.APIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("expected nil error, got %+v", resp.Error)
	}

	dataMap, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if dataMap["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", dataMap["status"])
	}
	if dataMap["database"] != "ok" {
		t.Errorf("expected database=ok, got %v", dataMap["database"])
	}
}

func TestHealthHandler_NilDB(t *testing.T) {
	handler := handlers.HealthHandler(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp handlers.APIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	dataMap, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if dataMap["database"] != "unavailable" {
		t.Errorf(
			"expected database=unavailable, got %v",
			dataMap["database"],
		)
	}
}
