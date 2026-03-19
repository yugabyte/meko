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
	mekoDb "meko/api_server/internal/db"
	"meko/api_server/internal/handlers"
)

func setupTestServer(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()
	db := openTestDB(t)

	if _, err := mekoDb.RunMigrations(db); err != nil {
		t.Fatalf("failed to run migrations: %v", err)
	}

	cleanupDatapacks(t, db)

	router, err := api.NewRouter(api.ServerConfig{DB: db})
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	srv := httptest.NewServer(router)
	return srv, db
}

func cleanupDatapacks(t *testing.T, db *sql.DB) {
	t.Helper()
	tables := []string{
		"meko_system.agent_knowledge_base",
		"meko_system.agent_registry",
		"meko_system.datapack_knowledge_base_registry",
		"meko_system.datapack_registry",
	}
	for _, tbl := range tables {
		if _, err := db.Exec("DELETE FROM " + tbl); err != nil { //nolint:gosec // hardcoded table names
			t.Logf("cleanup %s: %v", tbl, err)
		}
	}
}

func TestDatapack_CreateAndList(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{
		"name":                "test-dp-1",
		"seed_knowledge_base": "s3://bucket/docs/",
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
		"llm_model": "DeepSeek-R1",
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
	if agentCount < 1 {
		t.Error("expected at least 1 agent")
	}

	kbCount, ok := dataMap["knowledge_base_count"].(float64)
	if !ok {
		t.Fatal("expected knowledge_base_count to be a number")
	}
	if kbCount < 1 {
		t.Error("expected at least 1 knowledge base")
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

func TestDatapack_CreateValidation_NoKnowledgeBase(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{"name": "test-no-kb"}
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

func TestDatapack_CreateWithModels(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{
		"name":                "test-dp-with-models",
		"embed_model":         "text-embedding-3-small",
		"inference_model":     "GPT-4o",
		"seed_knowledge_base": "s3://bucket/docs/",
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

	descResp, err := http.Get(
		srv.URL + "/api/v1/datapacks/test-dp-with-models",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer descResp.Body.Close()

	var result handlers.APIResponse
	if decErr := json.NewDecoder(descResp.Body).Decode(&result); decErr != nil {
		t.Fatal(decErr)
	}
	dataMap, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if dataMap["llm_model"] != "GPT-4o" {
		t.Errorf(
			"expected llm_model=GPT-4o, got %v", dataMap["llm_model"],
		)
	}
	if dataMap["embedding_model"] != "text-embedding-3-small" {
		t.Errorf(
			"expected embedding_model=text-embedding-3-small, got %v",
			dataMap["embedding_model"],
		)
	}
}

func createDatapack(t *testing.T, baseURL, name string) {
	t.Helper()
	body := map[string]string{
		"name":                name,
		"seed_knowledge_base": "s3://test-bucket/docs/",
	}
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
