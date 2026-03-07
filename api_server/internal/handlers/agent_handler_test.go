package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"meko/api_server/internal/handlers"
)

func TestAgent_CreateAndList(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-agent-test")

	body := map[string]string{
		"agent_name":     "my-agent",
		"agent_metadata": `{"role":"assistant"}`,
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-agent-test/agents",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("POST /agents failed: %v", err)
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
	if createResp.Error != nil {
		t.Fatalf("expected nil error, got %+v", createResp.Error)
	}
	dataMap, ok := createResp.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if dataMap["agent_id"] == "" {
		t.Error("expected non-empty agent_id")
	}

	listResp, err := http.Get(
		srv.URL + "/api/v1/datapacks/dp-agent-test/agents",
	)
	if err != nil {
		t.Fatalf("GET /agents failed: %v", err)
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
		t.Fatal(decErr)
	}
	items, ok := listResult.Data.([]any)
	if !ok {
		t.Fatal("expected data to be an array")
	}
	// default agent + newly created agent
	if len(items) < 2 {
		t.Errorf("expected at least 2 agents, got %d", len(items))
	}
}

func TestAgent_Delete(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-agent-del")

	body := map[string]string{"agent_name": "del-agent"}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	createResp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-agent-del/agents",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create agent: expected 201, got %d",
			createResp.StatusCode)
	}

	req, err := http.NewRequest(
		http.MethodDelete,
		srv.URL+"/api/v1/datapacks/dp-agent-del/agents/del-agent",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /agents/del-agent failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusOK, resp.StatusCode,
		)
	}
}

func TestAgent_DeleteNotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-agent-delnf")

	req, err := http.NewRequest(
		http.MethodDelete,
		srv.URL+"/api/v1/datapacks/dp-agent-delnf/agents/ghost",
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
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusNotFound, resp.StatusCode,
		)
	}
}

func TestAgent_CreateValidation(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-agent-val")

	body := map[string]string{"agent_name": ""}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-agent-val/agents",
		"application/json", bytes.NewReader(b),
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

func TestAgent_CreateDatapackNotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{"agent_name": "orphan-agent"}
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
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusNotFound, resp.StatusCode,
		)
	}
}
