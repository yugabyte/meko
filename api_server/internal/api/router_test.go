package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/lib/pq"

	"meko/api_server/internal/api"
	"meko/api_server/internal/handlers"
)

const testConnStr = "postgresql://localhost:5433/yugabyte?user=yugabyte&password=yugabyte&sslmode=disable"

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testConnStr)
	if err != nil {
		t.Skipf("test database unavailable: open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("test database unavailable: ping: %v", err)
	}
	// Sanity-check that migrations have been applied. Without this the
	// dev-auth middleware fails to look up the dev account and every
	// request returns 401, which masks the real assertion failure.
	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		                WHERE table_schema = 'meko_system' AND table_name = 'users')`,
	).Scan(&exists); err != nil {
		t.Skipf("test database unavailable: schema probe: %v", err)
	}
	if !exists {
		t.Skip("test database missing meko_system schema; run migrations first " +
			"(e.g. migrate -path ./internal/db/migrations -database <conn> up)")
	}
	return db
}

func testConfig(db *sql.DB) api.ServerConfig {
	return api.ServerConfig{DB: db}
}

func TestRouter_HealthEndpoint(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	router, _, err := api.NewRouter(testConfig(db))
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("failed to GET /api/v1/health: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Logf("failed to close response body: %v", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf(
			"expected status %d, got %d",
			http.StatusOK, resp.StatusCode,
		)
	}

	var apiResp handlers.APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if apiResp.Error != nil {
		t.Errorf("expected nil error, got %+v", apiResp.Error)
	}

	dataMap, ok := apiResp.Data.(map[string]any)
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

func TestRouter_NotFound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	router, _, err := api.NewRouter(testConfig(db))
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/nonexistent")
	if err != nil {
		t.Fatalf("failed to GET /api/v1/nonexistent: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Logf("failed to close response body: %v", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf(
			"expected status %d, got %d",
			http.StatusNotFound, resp.StatusCode,
		)
	}
}
