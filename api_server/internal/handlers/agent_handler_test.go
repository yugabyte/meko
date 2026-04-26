package handlers_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"meko/api_server/internal/handlers"
)

// TestAgent_CreateUser_AndList covers the user-scoped POST /agents
// surface: a freshly created user has the default agent (inserted at
// signup), and a subsequent POST adds a new one.
func TestAgent_CreateUser_AndList(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]any{
		"agent_id":      "agent-create-1",
		"agent_details": json.RawMessage(`{"role":"assistant"}`),
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		srv.URL+"/api/v1/agents",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("POST /agents failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	listResp, err := http.Get(srv.URL + "/api/v1/agents")
	if err != nil {
		t.Fatalf("GET /agents failed: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", listResp.StatusCode)
	}

	var listResult handlers.APIResponse
	if decErr := json.NewDecoder(listResp.Body).Decode(&listResult); decErr != nil {
		t.Fatal(decErr)
	}
	items, ok := listResult.Data.([]any)
	if !ok {
		t.Fatal("expected data to be an array")
	}
	// Default agent (created at signup) plus the one we just inserted.
	if len(items) < 2 {
		t.Errorf("expected at least 2 agents, got %d", len(items))
	}
}

// TestAgent_CreateUser_Conflict verifies POST /agents is strictly
// create-only: re-inserting the same agent_id returns 409 Conflict.
func TestAgent_CreateUser_Conflict(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]any{
		"agent_id": "agent-dup",
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	first, err := http.Post(srv.URL+"/api/v1/agents",
		"application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first create: expected 201, got %d", first.StatusCode)
	}

	second, err := http.Post(srv.URL+"/api/v1/agents",
		"application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d", second.StatusCode)
	}

	var result handlers.APIResponse
	if decErr := json.NewDecoder(second.Body).Decode(&result); decErr != nil {
		t.Fatal(decErr)
	}
	if result.Error == nil || result.Error.Code != "CONFLICT" {
		t.Errorf("expected CONFLICT error code, got %+v", result.Error)
	}
}

// TestAgent_CreateUser_Validation verifies POST /agents rejects an
// empty agent_id with 400.
func TestAgent_CreateUser_Validation(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]any{"agent_id": ""}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(srv.URL+"/api/v1/agents",
		"application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TestAgent_DeleteUser verifies DELETE /agents/:agent_id removes a
// user-scoped agent.
func TestAgent_DeleteUser(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]any{"agent_id": "agent-del"}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	createResp, err := http.Post(srv.URL+"/api/v1/agents",
		"application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", createResp.StatusCode)
	}

	req, err := http.NewRequest(
		http.MethodDelete,
		srv.URL+"/api/v1/agents/agent-del",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// TestAgent_DeleteUser_NotFound verifies a missing agent returns 404.
func TestAgent_DeleteUser_NotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	req, err := http.NewRequest(
		http.MethodDelete,
		srv.URL+"/api/v1/agents/ghost",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestAgent_LinkAndList verifies the link-only flow on
// POST /datapacks/:name/agents and GET /datapacks/:name/agents.
func TestAgent_LinkAndList(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-link-test")

	createBody := map[string]any{"agent_id": "agent-link-1"}
	cb, err := json.Marshal(createBody)
	if err != nil {
		t.Fatal(err)
	}
	createResp, err := http.Post(srv.URL+"/api/v1/agents",
		"application/json", bytes.NewReader(cb))
	if err != nil {
		t.Fatal(err)
	}
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create agent: expected 201, got %d",
			createResp.StatusCode)
	}

	linkBody := map[string]any{"agent_id": "agent-link-1"}
	lb, err := json.Marshal(linkBody)
	if err != nil {
		t.Fatal(err)
	}
	linkResp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-link-test/agents",
		"application/json", bytes.NewReader(lb),
	)
	if err != nil {
		t.Fatalf("POST link failed: %v", err)
	}
	defer linkResp.Body.Close()
	if linkResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", linkResp.StatusCode)
	}

	listResp, err := http.Get(
		srv.URL + "/api/v1/datapacks/dp-link-test/agents",
	)
	if err != nil {
		t.Fatalf("GET datapack agents failed: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", listResp.StatusCode)
	}

	var listResult handlers.APIResponse
	if decErr := json.NewDecoder(listResp.Body).Decode(&listResult); decErr != nil {
		t.Fatal(decErr)
	}
	items, ok := listResult.Data.([]any)
	if !ok {
		t.Fatal("expected data to be an array")
	}
	// Default agent (auto-linked at datapack create) + the one we
	// just linked = 2 minimum.
	if len(items) < 2 {
		t.Errorf("expected at least 2 linked agents, got %d", len(items))
	}
}

// TestAgent_Link_AgentNotFound verifies linking a non-existent agent
// returns 404.
func TestAgent_Link_AgentNotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-link-404")

	body := map[string]any{"agent_id": "ghost"}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-link-404/agents",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestAgent_Unlink verifies DELETE /datapacks/:name/agents/:agent_id
// removes only the link, not the underlying user-scoped agent.
func TestAgent_Unlink(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-unlink-test")

	createBody := map[string]any{"agent_id": "agent-unlink-1"}
	cb, err := json.Marshal(createBody)
	if err != nil {
		t.Fatal(err)
	}
	createResp, err := http.Post(srv.URL+"/api/v1/agents",
		"application/json", bytes.NewReader(cb))
	if err != nil {
		t.Fatal(err)
	}
	createResp.Body.Close()

	linkBody := map[string]any{"agent_id": "agent-unlink-1"}
	lb, err := json.Marshal(linkBody)
	if err != nil {
		t.Fatal(err)
	}
	linkResp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-unlink-test/agents",
		"application/json", bytes.NewReader(lb),
	)
	if err != nil {
		t.Fatal(err)
	}
	linkResp.Body.Close()

	unlinkURL := fmt.Sprintf(
		"%s/api/v1/datapacks/dp-unlink-test/agents/agent-unlink-1",
		srv.URL,
	)
	req, err := http.NewRequest(http.MethodDelete, unlinkURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	listResp, err := http.Get(srv.URL + "/api/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	var listResult handlers.APIResponse
	if decErr := json.NewDecoder(listResp.Body).Decode(&listResult); decErr != nil {
		t.Fatal(decErr)
	}
	items, ok := listResult.Data.([]any)
	if !ok {
		t.Fatal("expected data to be an array")
	}
	found := false
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if m["agent_id"] == "agent-unlink-1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("agent should still exist after unlink (link removed only)")
	}
}

// TestAgent_Unlink_NotFound verifies a missing link returns 404.
func TestAgent_Unlink_NotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-unlink-404")

	req, err := http.NewRequest(
		http.MethodDelete,
		srv.URL+"/api/v1/datapacks/dp-unlink-404/agents/never-linked",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestAgent_Link_DatapackNotFound verifies linking against an unknown
// datapack returns 404 (handled by RequireDatapackOwnership).
func TestAgent_Link_DatapackNotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]any{"agent_id": "any-agent"}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/nonexistent-dp/agents",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}
