package handlers_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	_ "github.com/lib/pq"

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
	// per-test cleanup helpers spew cryptic "relation does not exist"
	// errors and assertions fail in unhelpful ways.
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

func TestHealthHandler_WithDB(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	h := handlers.HealthHandler(db)
	if err := h(c); err != nil {
		t.Fatal(err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	var resp handlers.APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
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
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	h := handlers.HealthHandler(nil)
	if err := h(c); err != nil {
		t.Fatal(err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	var resp handlers.APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
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
