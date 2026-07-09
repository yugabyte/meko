package handlers_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"meko/api_server/internal/handlers"
)

func TestArtifact_PutAndGet_SmallFile(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	// Ensure artifacts table exists (migration must be applied).
	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		               WHERE table_schema='meko_system' AND table_name='artifacts')`,
	).Scan(&exists); err != nil || !exists {
		t.Skip("meko_system.artifacts table missing; run migration 000006 first")
	}

	createDatapack(t, srv.URL, "dp-artifact-put")

	content := []byte("hello artifact content")
	body := map[string]string{
		"filename":       "hello.txt",
		"content_type":   "text/plain",
		"content_base64": base64.StdEncoding.EncodeToString(content),
	}
	b, _ := json.Marshal(body)

	putResp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-artifact-put/artifacts",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("POST /artifacts: %v", err)
	}
	defer putResp.Body.Close()

	if putResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", putResp.StatusCode)
	}

	var putEnv handlers.APIResponse
	if err := json.NewDecoder(putResp.Body).Decode(&putEnv); err != nil {
		t.Fatal(err)
	}
	if putEnv.Error != nil {
		t.Fatalf("unexpected error: %+v", putEnv.Error)
	}

	data, ok := putEnv.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data map")
	}
	hash, _ := data["content_hash"].(string)
	if hash == "" {
		t.Fatal("expected non-empty content_hash in response")
	}
	if data["stored_in"] != "db" {
		t.Errorf("expected stored_in=db, got %v", data["stored_in"])
	}

	// GET
	getResp, err := http.Get(srv.URL + "/api/v1/datapacks/dp-artifact-put/artifacts/" + hash)
	if err != nil {
		t.Fatalf("GET /artifacts/:hash: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", getResp.StatusCode)
	}

	var getEnv handlers.APIResponse
	if err := json.NewDecoder(getResp.Body).Decode(&getEnv); err != nil {
		t.Fatal(err)
	}
	getData, ok := getEnv.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data map on GET")
	}
	b64, _ := getData["content_base64"].(string)
	if b64 == "" {
		t.Fatal("expected content_base64 in GET response")
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode content_base64: %v", err)
	}
	if string(decoded) != string(content) {
		t.Errorf("content mismatch: want %q, got %q", content, decoded)
	}
}

func TestArtifact_Put_Idempotent(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		               WHERE table_schema='meko_system' AND table_name='artifacts')`,
	).Scan(&exists); err != nil || !exists {
		t.Skip("meko_system.artifacts table missing; run migration 000006 first")
	}

	createDatapack(t, srv.URL, "dp-artifact-idem")

	body := map[string]string{
		"filename":       "idem.txt",
		"content_type":   "text/plain",
		"content_base64": base64.StdEncoding.EncodeToString([]byte("idempotent bytes")),
	}
	b, _ := json.Marshal(body)

	firstResp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-artifact-idem/artifacts",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("first PUT: %v", err)
	}
	defer firstResp.Body.Close()

	var firstEnv handlers.APIResponse
	_ = json.NewDecoder(firstResp.Body).Decode(&firstEnv)
	firstData, _ := firstEnv.Data.(map[string]any)
	firstHash, _ := firstData["content_hash"].(string)

	b, _ = json.Marshal(body) // re-marshal since body was consumed
	secondResp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-artifact-idem/artifacts",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("second PUT: %v", err)
	}
	defer secondResp.Body.Close()

	var secondEnv handlers.APIResponse
	_ = json.NewDecoder(secondResp.Body).Decode(&secondEnv)
	secondData, _ := secondEnv.Data.(map[string]any)
	secondHash, _ := secondData["content_hash"].(string)

	if firstHash != secondHash {
		t.Errorf("idempotency: hashes differ %s vs %s", firstHash, secondHash)
	}
}

func TestArtifact_Put_MissingFields_Returns400(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		               WHERE table_schema='meko_system' AND table_name='artifacts')`,
	).Scan(&exists); err != nil || !exists {
		t.Skip("meko_system.artifacts table missing; run migration 000006 first")
	}

	createDatapack(t, srv.URL, "dp-artifact-val")

	body := map[string]string{"filename": "only-filename.txt"} // missing content_type + content_base64
	b, _ := json.Marshal(body)

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-artifact-val/artifacts",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestArtifact_Get_UnknownHash_Returns404(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		               WHERE table_schema='meko_system' AND table_name='artifacts')`,
	).Scan(&exists); err != nil || !exists {
		t.Skip("meko_system.artifacts table missing; run migration 000006 first")
	}

	createDatapack(t, srv.URL, "dp-artifact-404")

	resp, err := http.Get(
		srv.URL + "/api/v1/datapacks/dp-artifact-404/artifacts/deadbeef0000000000000000000000000000000000000000000000000000dead",
	)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestArtifact_Put_InvalidBase64_Returns400(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		               WHERE table_schema='meko_system' AND table_name='artifacts')`,
	).Scan(&exists); err != nil || !exists {
		t.Skip("meko_system.artifacts table missing; run migration 000006 first")
	}

	createDatapack(t, srv.URL, "dp-artifact-b64")

	body := map[string]string{
		"filename":       "f.txt",
		"content_type":   "text/plain",
		"content_base64": "!!NOT_VALID_BASE64!!",
	}
	b, _ := json.Marshal(body)

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/dp-artifact-b64/artifacts",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}
