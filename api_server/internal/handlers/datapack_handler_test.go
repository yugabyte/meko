package handlers_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/lib/pq"

	"meko/api_server/internal/api"
	"meko/api_server/internal/handlers"
)

// setupTestServer wires the API router against the externally-managed
// test database. Migrations are NOT applied here: as a pre-req the
// developer is expected to run the migrations once (e.g. via
// `migrate -path ./internal/db/migrations -database <conn> up`)
// before running integration tests. This keeps the test loop fast
// and avoids dirty-state flakiness from concurrent migrate runs.
func setupTestServer(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()
	db := openTestDB(t)

	cleanupDatapacks(t, db)

	router, err := api.NewRouter(api.ServerConfig{DB: db})
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	srv := httptest.NewServer(router)
	return srv, db
}

// cleanupDatapacks resets per-test state in the multi-tenant schema.
//
// The dev user (cognito_sub='dev-local') is also deleted so that
// DevAuth re-creates it on the next request, which restores the
// user's default agent in meko_system.agents alongside it.
func cleanupDatapacks(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`DELETE FROM meko_system.knowledge_bases`,
		`DELETE FROM meko_system.agent_access_permissions`,
		`DELETE FROM meko_system.agent_access`,
		`DELETE FROM meko_system.langfuse_project_mapping`,
		`DELETE FROM meko_system.langfuse_org_mapping`,
		`DELETE FROM meko_system.datapacks`,
		`DELETE FROM meko_system.agents`,
		`DELETE FROM meko_system.users WHERE cognito_sub = 'dev-local'`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil { //nolint:gosec // hardcoded SQL
			t.Logf("cleanup %s: %v", q, err)
		}
	}
}

func TestDatapack_CreateAndList(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{"name": "test-dp-1"}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks", "application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("POST /datapacks failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusCreated, resp.StatusCode,
		)
	}

	var createResp handlers.APIResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&createResp); decErr != nil {
		t.Fatalf("failed to decode create response: %v", decErr)
	}
	if createResp.Error != nil {
		t.Fatalf("expected nil error, got %+v", createResp.Error)
	}

	dataMap, ok := createResp.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if dataMap["datapack_id"] == "" {
		t.Error("expected non-empty datapack_id")
	}
	if dataMap["agent_id"] == "" {
		t.Error("expected non-empty agent_id")
	}

	listResp, err := http.Get(srv.URL + "/api/v1/datapacks")
	if err != nil {
		t.Fatalf("GET /datapacks failed: %v", err)
	}
	defer listResp.Body.Close()

	if listResp.StatusCode != http.StatusOK {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusOK, listResp.StatusCode,
		)
	}

	var listResult handlers.APIResponse
	if decErr := json.NewDecoder(listResp.Body).Decode(&listResult); decErr != nil {
		t.Fatalf("failed to decode list response: %v", decErr)
	}

	items, ok := listResult.Data.([]any)
	if !ok {
		t.Fatal("expected data to be an array")
	}
	if len(items) < 1 {
		t.Error("expected at least 1 datapack in list")
	}
}

func TestDatapack_Describe(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "test-dp-describe")

	resp, err := http.Get(srv.URL + "/api/v1/datapacks/test-dp-describe")
	if err != nil {
		t.Fatalf("GET /datapacks/test-dp-describe failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusOK, resp.StatusCode,
		)
	}
}

func TestDatapack_DescribeNotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	resp, err := http.Get(
		srv.URL + "/api/v1/datapacks/nonexistent-dp",
	)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusNotFound, resp.StatusCode,
		)
	}
}

func TestDatapack_Update(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "test-dp-update")

	patchBody := map[string]string{
		"datapack_ysql_connection_string": "postgresql://example/db",
	}
	b, err := json.Marshal(patchBody)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(
		http.MethodPatch,
		srv.URL+"/api/v1/datapacks/test-dp-update",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusOK, resp.StatusCode,
		)
	}
}

func TestDatapack_Delete(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "test-dp-delete")

	req, err := http.NewRequest(
		http.MethodDelete,
		srv.URL+"/api/v1/datapacks/test-dp-delete", nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusOK, resp.StatusCode,
		)
	}

	getResp, err := http.Get(
		srv.URL + "/api/v1/datapacks/test-dp-delete",
	)
	if err != nil {
		t.Fatalf("GET after delete failed: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf(
			"expected %d after delete, got %d",
			http.StatusNotFound, getResp.StatusCode,
		)
	}
}

func TestDatapack_Status(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "test-dp-status")

	resp, err := http.Get(
		srv.URL + "/api/v1/datapacks/test-dp-status/status",
	)
	if err != nil {
		t.Fatalf("GET /status failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusOK, resp.StatusCode,
		)
	}

	var result handlers.APIResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&result); decErr != nil {
		t.Fatal(decErr)
	}
	dataMap, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}

	agentCount, ok := dataMap["agent_count"].(float64)
	if !ok {
		t.Fatal("expected agent_count to be a number")
	}
	// At least the default agent should be linked to the datapack.
	if agentCount < 1 {
		t.Errorf("expected at least 1 linked agent, got %v", agentCount)
	}

	// kb_count is no longer auto-populated on datapack create in the
	// multi-tenant schema; validate it is present and non-negative.
	kbCount, ok := dataMap["knowledge_base_count"].(float64)
	if !ok {
		t.Fatal("expected knowledge_base_count to be a number")
	}
	if kbCount < 0 {
		t.Errorf("knowledge_base_count should be >= 0, got %v", kbCount)
	}
}

func TestDatapack_CreateValidation_EmptyName(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{"name": ""}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks", "application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusBadRequest, resp.StatusCode,
		)
	}
}

// TestDatapack_CreateWithExplicitAgent verifies that POST /datapacks
// can scope a datapack to a specific user-created agent, using the
// new user-scoped /agents create endpoint.
func TestDatapack_CreateWithExplicitAgent(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	agentBody := map[string]any{"agent_id": "explicit-agent-1"}
	ab, err := json.Marshal(agentBody)
	if err != nil {
		t.Fatal(err)
	}
	agentResp, err := http.Post(
		srv.URL+"/api/v1/agents",
		"application/json", bytes.NewReader(ab),
	)
	if err != nil {
		t.Fatal(err)
	}
	agentResp.Body.Close()
	if agentResp.StatusCode != http.StatusCreated {
		t.Fatalf(
			"create agent: expected 201, got %d", agentResp.StatusCode,
		)
	}

	body := map[string]string{
		"name":     "test-dp-with-agent",
		"agent_id": "explicit-agent-1",
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks", "application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusCreated, resp.StatusCode,
		)
	}

	var createResp handlers.APIResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&createResp); decErr != nil {
		t.Fatal(decErr)
	}
	dataMap, ok := createResp.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if dataMap["agent_id"] != "explicit-agent-1" {
		t.Errorf(
			"expected agent_id=explicit-agent-1, got %v",
			dataMap["agent_id"],
		)
	}
}

// TestDatapack_CreateWithUnknownAgent verifies that creating a
// datapack against a non-existent agent returns a 5xx body (the
// service surfaces "not found" via fmt.Errorf, which the handler
// maps to 404).
func TestDatapack_CreateWithUnknownAgent(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{
		"name":     "test-dp-bad-agent",
		"agent_id": "ghost-agent",
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks", "application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf(
			"expected 404 for missing agent, got %d", resp.StatusCode,
		)
	}
}

func TestDatapack_CreateWithDescription(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{
		"name":        "test-dp-desc",
		"description": "A test datapack",
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks", "application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("POST /datapacks failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusCreated, resp.StatusCode,
		)
	}

	var createResp handlers.APIResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&createResp); decErr != nil {
		t.Fatalf("failed to decode create response: %v", decErr)
	}
	if createResp.Error != nil {
		t.Fatalf("expected nil error, got %+v", createResp.Error)
	}

	descResp, err := http.Get(
		srv.URL + "/api/v1/datapacks/test-dp-desc",
	)
	if err != nil {
		t.Fatalf("GET /datapacks/test-dp-desc failed: %v", err)
	}
	defer descResp.Body.Close()

	if descResp.StatusCode != http.StatusOK {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusOK, descResp.StatusCode,
		)
	}

	var descResult handlers.APIResponse
	if decErr := json.NewDecoder(descResp.Body).Decode(&descResult); decErr != nil {
		t.Fatalf("failed to decode describe response: %v", decErr)
	}
	dataMap, ok := descResult.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if got := dataMap["description"]; got != "A test datapack" {
		t.Errorf(
			"expected description=%q, got %v",
			"A test datapack", got,
		)
	}
}

func createDatapack(t *testing.T, baseURL, name string) {
	t.Helper()
	body := map[string]string{"name": name}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		baseURL+"/api/v1/datapacks", "application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("failed to create datapack %s: %v", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf(
			"create %s: expected %d, got %d",
			name, http.StatusCreated, resp.StatusCode,
		)
	}
}
