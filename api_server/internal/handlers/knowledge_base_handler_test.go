package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"meko/api_server/internal/handlers"
)

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
	if dataMap["knowledge_base_id"] == "" {
		t.Error("expected non-empty knowledge_base_id")
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
	// seed KB + the one we just added
	if len(items) < 2 {
		t.Errorf("expected at least 2 knowledge bases, got %d",
			len(items))
	}
}

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

	delBody := map[string]string{"url": "s3://del-bucket/data/"}
	b, err = json.Marshal(delBody)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(
		http.MethodDelete,
		srv.URL+"/api/v1/datapacks/dp-kb-del/knowledge-bases",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

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

func TestKnowledgeBase_DeleteNotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-kb-delnf")

	delBody := map[string]string{"url": "s3://nonexistent/"}
	b, err := json.Marshal(delBody)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(
		http.MethodDelete,
		srv.URL+"/api/v1/datapacks/dp-kb-delnf/knowledge-bases",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

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

func TestKnowledgeBase_DeleteValidation_EmptyURL(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "dp-kb-delval")

	delBody := map[string]string{"url": ""}
	b, err := json.Marshal(delBody)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(
		http.MethodDelete,
		srv.URL+"/api/v1/datapacks/dp-kb-delval/knowledge-bases",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d",
			http.StatusBadRequest, resp.StatusCode)
	}
}
