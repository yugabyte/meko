package handlers_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"meko/api_server/internal/api"
	"meko/api_server/internal/handlers"
)

// failingLangfuse answers every request with a 500 and counts them.
type failingLangfuse struct{ calls atomic.Int32 }

func (f *failingLangfuse) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	f.calls.Add(1)
	http.Error(w, "langfuse is down", http.StatusInternalServerError)
}

// setupTestServerWithLangfuse is setupTestServer with Langfuse enabled and
// pointed at langfuseURL.
func setupTestServerWithLangfuse(t *testing.T, db *sql.DB, langfuseURL string) *httptest.Server {
	t.Helper()
	router, _, err := api.NewRouter(api.ServerConfig{
		DB:                  db,
		SystemDB:            db,
		LangfuseHost:        langfuseURL,
		LangfuseAdminAPIKey: "test-admin-key",
	})
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func countDatapacksNamed(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM meko_system.datapacks WHERE datapack_name = $1`, name,
	).Scan(&n); err != nil {
		t.Fatalf("count datapacks: %v", err)
	}
	return n
}

// createLangfuseTestDatapack creates a datapack and returns its id.
func createLangfuseTestDatapack(t *testing.T, baseURL, name string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(baseURL+"/api/v1/datapacks", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s: expected 201, got %d", name, resp.StatusCode)
	}
	var envelope handlers.APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("create %s: decode: %v", name, err)
	}
	data, ok := envelope.Data.(map[string]any)
	if !ok {
		t.Fatalf("create %s: data not a map, got %T", name, envelope.Data)
	}
	id, ok := data["datapack_id"].(string)
	if !ok || id == "" {
		t.Fatalf("create %s: no datapack_id in response: %+v", name, data)
	}
	return id
}

func countRows(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// datapackCreationCount sums the lifetime datapack_creation counter across
// users; the tests compare it before and after a request.
func datapackCreationCount(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(
		`SELECT COALESCE(SUM(request_count), 0) FROM meko_system.free_tier_usage
		 WHERE metric_name = 'datapack_creation'`,
	).Scan(&n); err != nil {
		t.Fatalf("read datapack_creation counter: %v", err)
	}
	return n
}

func TestDatapack_Create_LangfuseDownRollsBack(t *testing.T) {
	plain, db := setupTestServer(t)
	defer plain.Close()
	defer db.Close()

	langfuse := &failingLangfuse{}
	lfSrv := httptest.NewServer(langfuse)
	defer lfSrv.Close()
	srv := setupTestServerWithLangfuse(t, db, lfSrv.URL)

	creationsBefore := datapackCreationCount(t, db)
	accessBefore := countRows(t, db, `SELECT COUNT(*) FROM meko_system.agent_access`)
	b, err := json.Marshal(map[string]string{"name": "test-dp-lf-down"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/api/v1/datapacks", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /datapacks: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", resp.StatusCode, raw)
	}
	var envelope handlers.APIResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Error == nil || envelope.Error.Code != "LANGFUSE_UNAVAILABLE" {
		t.Fatalf("error = %+v, want code LANGFUSE_UNAVAILABLE", envelope.Error)
	}
	if strings.Contains(string(raw), strings.TrimPrefix(lfSrv.URL, "http://")) {
		t.Fatalf("response leaks the Langfuse host: %s", raw)
	}
	if langfuse.calls.Load() == 0 {
		t.Fatal("Langfuse was never called")
	}
	if n := countDatapacksNamed(t, db, "test-dp-lf-down"); n != 0 {
		t.Fatalf("%d datapack rows left behind; want the create rolled back", n)
	}
	if after := countRows(t, db, `SELECT COUNT(*) FROM meko_system.agent_access`); after != accessBefore {
		t.Fatalf("agent_access rows %d → %d; the rollback must remove the new datapack's link",
			accessBefore, after)
	}
	if after := datapackCreationCount(t, db); after != creationsBefore {
		t.Fatalf("datapack_creation counter %d → %d; a rolled-back create must not spend quota",
			creationsBefore, after)
	}

	// The name is free again: the same create succeeds once Langfuse is
	// out of the picture.
	createDatapack(t, plain.URL, "test-dp-lf-down")
}

func TestDatapack_Delete_LangfuseDownStillDeletes(t *testing.T) {
	plain, db := setupTestServer(t)
	defer plain.Close()
	defer db.Close()

	id := createLangfuseTestDatapack(t, plain.URL, "test-dp-lf-delete")
	var ownerID string
	if err := db.QueryRow(
		`SELECT user_id FROM meko_system.datapacks WHERE datapack_id = $1`, id,
	).Scan(&ownerID); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	// Give the pack a Langfuse project, as a successful create would have.
	if _, err := db.Exec(
		`INSERT INTO meko_system.langfuse_org_mapping
		 (user_id, langfuse_org_id, langfuse_org_name, langfuse_org_public_key, langfuse_org_secret_key)
		 VALUES ($1, 'org-test', 'meko-org-test', 'pk-org', 'sk-org')`, ownerID,
	); err != nil {
		t.Fatalf("seed org mapping: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO meko_system.langfuse_project_mapping
		 (datapack_id, langfuse_project_id, langfuse_project_name, langfuse_public_key, langfuse_secret_key)
		 VALUES ($1, 'proj-test', 'test-dp-lf-delete', 'pk-p', 'sk-p')`, id,
	); err != nil {
		t.Fatalf("seed project mapping: %v", err)
	}

	langfuse := &failingLangfuse{}
	lfSrv := httptest.NewServer(langfuse)
	defer lfSrv.Close()
	srv := setupTestServerWithLangfuse(t, db, lfSrv.URL)

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/datapacks/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, readErr := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body %s (read err %v)", resp.StatusCode, raw, readErr)
	}
	if langfuse.calls.Load() == 0 {
		t.Fatal("Langfuse project delete was never attempted")
	}
	if n := countDatapacksNamed(t, db, "test-dp-lf-delete"); n != 0 {
		t.Fatalf("datapack row still present after delete")
	}
	var mappings int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM meko_system.langfuse_project_mapping WHERE datapack_id = $1`, id,
	).Scan(&mappings); err != nil {
		t.Fatalf("count mappings: %v", err)
	}
	if mappings != 0 {
		t.Fatal("langfuse project mapping still present after delete")
	}
}
