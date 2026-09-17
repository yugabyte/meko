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

// testConfigFull sets both DB and SystemDB to the same connection, which is
// required for routes that run DevAuth (the auth middleware needs SystemDB to
// resolve the dev account) or any middleware that looks up datapacks.
func testConfigFull(db *sql.DB) api.ServerConfig {
	return api.ServerConfig{DB: db, SystemDB: db}
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

// TestRouter_MemoriesRouteRegistered verifies GET /memories is registered on
// dpDirect (PAT-accepting group). Without datapack_id the handler returns 400
// VALIDATION_ERROR — distinct from Echo's 404 "Not Found" that would appear if
// the route were unregistered.
func TestRouter_MemoriesRouteRegistered(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	router, _, err := api.NewRouter(testConfigFull(db))
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/memories")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %v", errObj["code"])
	}
}

// TestRouter_MemoriesCountRouteRegistered mirrors the above for GET /memories/count.
func TestRouter_MemoriesCountRouteRegistered(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	router, _, err := api.NewRouter(testConfigFull(db))
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/memories/count")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %v", errObj["code"])
	}
}

// TestRouter_PromotedMemoriesRouteRegistered verifies GET /promoted-memories is
// registered on dpDirect. Without datapack_id the handler's access gate returns
// 400 VALIDATION_ERROR.
func TestRouter_PromotedMemoriesRouteRegistered(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	router, _, err := api.NewRouter(testConfigFull(db))
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/promoted-memories")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %v", errObj["code"])
	}
}

// TestRouter_ConversationsRouteRegistered verifies GET /datapacks/:name/conversations
// is registered on dpDirect with RequireDatapackAccess middleware. A non-existent
// datapack returns 404 NOT_FOUND from the middleware — distinct from Echo's generic
// 404 "Not Found" that would appear if the route were unregistered.
func TestRouter_ConversationsRouteRegistered(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	router, _, err := api.NewRouter(testConfigFull(db))
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/datapacks/nonexistent-dp-test/conversations")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	// Middleware NOT_FOUND vs Echo's generic "Not Found" — the former proves
	// the route is registered and RequireDatapackAccess ran.
	if errObj["code"] != "NOT_FOUND" {
		t.Errorf("expected NOT_FOUND (middleware), got %v — route may not be registered", errObj["code"])
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

// TestRouter_KBDocumentRoutesRegistered pins the knowledge-base document
// routes in the routing table: the per-file DELETE and the documents list
// that hands out document_id values. Needs no database (NewRouter only
// constructs repositories), so it runs in CI — unlike the DB-backed tests
// above, which self-skip there.
//
// Scope honesty: Echo's routing table carries no middleware information, so
// dpDirect (PAT) membership and RequireScope are NOT assertable here; that
// contract lives in router.go and is enforced by review. This test guards
// against the routes disappearing or their paths/methods drifting.
func TestRouter_KBDocumentRoutesRegistered(t *testing.T) {
	router, _, err := api.NewRouter(api.ServerConfig{})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	want := map[string]bool{
		"DELETE /api/v1/datapacks/:datapack_id/knowledge-bases/documents/:document_id": false,
		"GET /api/v1/datapacks/:datapack_id/knowledge-bases/documents":                 false,
	}
	for _, r := range router.Routes() {
		key := r.Method + " " + r.Path
		if _, seen := want[key]; seen {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("route not registered: %s", key)
		}
	}
}
