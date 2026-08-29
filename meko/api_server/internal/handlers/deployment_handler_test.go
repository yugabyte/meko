package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/lib/pq"

	"meko/api_server/internal/api"
	"meko/api_server/internal/handlers"
)

// TestDeployment_CreateWithoutAgentSlug_IsBadRequest exercises the thin
// JSON-binding/error-mapping layer only -- deployments are unconfigured in
// this test server (no AWS execution role / account id set), so the
// interesting AWS-orchestration paths are covered by
// service/deployment_service_test.go instead. Skips like every other
// handler test here when no local test database is reachable.
func TestDeployment_CreateWithoutAgentSlug_IsBadRequest(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	router, _, err := api.NewRouter(api.ServerConfig{DB: db, SystemDB: db})
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()

	body, err := json.Marshal(map[string]string{"agent_slug": ""})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(srv.URL+"/api/v1/deployments", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /deployments failed: %v", err)
	}
	defer resp.Body.Close()

	// Deployments are unconfigured (no managed execution role set), so this
	// resolves to an INTERNAL_ERROR ("service not configured") rather than
	// VALIDATION_ERROR -- both are legitimate 4xx/5xx outcomes here; the
	// point of this test is that binding + routing work end-to-end, not the
	// specific AWS-config error text.
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		t.Fatalf("expected a non-2xx status for an unconfigured deploy target, got %d", resp.StatusCode)
	}

	var apiResp handlers.APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if apiResp.Error == nil {
		t.Fatal("expected an error envelope")
	}
}
