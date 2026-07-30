package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"meko/api_server/internal/handlers"
)

// TestKnowledgeBase_AddAndList exercises POST/GET against the new
// meko_system.knowledge_bases multi-tenant table. It assumes the test
// DB has migrations applied as a pre-req.
func TestKnowledgeBase_AddAndList(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-kb-addlist")

	body := map[string]string{"url": "s3://extra-bucket/docs/"}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-kb-addlist/knowledge-bases",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("POST /knowledge-bases failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected status %d, got %d",
			http.StatusCreated, resp.StatusCode)
	}

	var createResp handlers.APIResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&createResp); decErr != nil {
		t.Fatal(decErr)
	}
	if createResp.Error != nil {
		t.Fatalf("expected nil error, got %+v", createResp.Error)
	}
	dataMap, isMap := createResp.Data.(map[string]any)
	if !isMap {
		t.Fatal("expected data to be a map")
	}
	if dataMap["rag_source_id"] == "" || dataMap["rag_source_id"] == nil {
		t.Error("expected non-empty rag_source_id")
	}
	if dataMap["datapack_id"] == "" || dataMap["datapack_id"] == nil {
		t.Error("expected non-empty datapack_id")
	}
	if dataMap["knowledge_base_url"] != "s3://extra-bucket/docs/" {
		t.Errorf("expected URL to round-trip, got %v",
			dataMap["knowledge_base_url"])
	}

	listResp, err := http.Get(
		srv.URL + "/api/v1/datapacks/dp-kb-addlist/knowledge-bases",
	)
	if err != nil {
		t.Fatalf("GET /knowledge-bases failed: %v", err)
	}
	defer listResp.Body.Close()

	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected status %d, got %d",
			http.StatusOK, listResp.StatusCode)
	}

	var listResult handlers.APIResponse
	if decErr := json.NewDecoder(listResp.Body).Decode(&listResult); decErr != nil {
		t.Fatal(decErr)
	}
	items, isArr := listResult.Data.([]any)
	if !isArr {
		t.Fatal("expected data to be an array")
	}
	if len(items) != 1 {
		t.Errorf("expected exactly 1 KB row (no auto-seed), got %d",
			len(items))
	}
}

// TestKnowledgeBase_Delete creates a KB, then deletes it by
// rag_source_id (path parameter) and verifies the response.
func TestKnowledgeBase_Delete(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-kb-del")

	addBody := map[string]string{"url": "s3://del-bucket/data/"}
	b, err := json.Marshal(addBody)
	if err != nil {
		t.Fatal(err)
	}
	addResp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-kb-del/knowledge-bases",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer addResp.Body.Close()
	if addResp.StatusCode != http.StatusCreated {
		t.Fatalf("add KB: expected 201, got %d", addResp.StatusCode)
	}
	var addResult handlers.APIResponse
	if decErr := json.NewDecoder(addResp.Body).Decode(&addResult); decErr != nil {
		t.Fatal(decErr)
	}
	addMap, isMap := addResult.Data.(map[string]any)
	if !isMap {
		t.Fatal("expected create body to be a map")
	}
	ragSourceID, _ := addMap["rag_source_id"].(string)
	if ragSourceID == "" {
		t.Fatal("expected non-empty rag_source_id in create response")
	}

	req, err := http.NewRequest(
		http.MethodDelete,
		srv.URL+"/api/v1/datapacks/dp-kb-del/knowledge-bases/"+ragSourceID,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /knowledge-bases failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status %d, got %d",
			http.StatusOK, resp.StatusCode)
	}
}

// TestKnowledgeBase_DeleteNotFound verifies a delete against a
// non-existent rag_source_id returns 404.
func TestKnowledgeBase_DeleteNotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-kb-delnf")

	req, err := http.NewRequest(
		http.MethodDelete,
		srv.URL+"/api/v1/datapacks/dp-kb-delnf/knowledge-bases/"+
			"00000000-0000-0000-0000-000000000000",
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
		t.Fatalf("expected status %d, got %d",
			http.StatusNotFound, resp.StatusCode)
	}
}

// TestKnowledgeBase_AddValidation_EmptyURL ensures the service-layer
// "url is required" validation surfaces a 400 to the client.
func TestKnowledgeBase_AddValidation_EmptyURL(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-kb-val")

	body := map[string]string{"url": ""}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-kb-val/knowledge-bases",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d",
			http.StatusBadRequest, resp.StatusCode)
	}
}

// TestKnowledgeBase_AddDatapackNotFound ensures requests against an
// unknown datapack name short-circuit at RequireDatapackOwnership
// with a 404 before reaching the KB handler.
func TestKnowledgeBase_AddDatapackNotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{"url": "s3://orphan/docs/"}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/nonexistent-dp/knowledge-bases",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d",
			http.StatusNotFound, resp.StatusCode)
	}
}

// TestKnowledgeBase_ListEmpty verifies an empty list response on a
// freshly-created datapack (no KB is auto-seeded in the multi-tenant
// model).
func TestKnowledgeBase_ListEmpty(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-kb-empty")

	resp, err := http.Get(
		srv.URL + "/api/v1/datapacks/dp-kb-empty/knowledge-bases",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var listResult handlers.APIResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&listResult); decErr != nil {
		t.Fatal(decErr)
	}
	items, isArr := listResult.Data.([]any)
	if !isArr {
		t.Fatal("expected data to be an array")
	}
	if len(items) != 0 {
		t.Errorf("expected 0 KB rows on a fresh datapack, got %d",
			len(items))
	}
}
