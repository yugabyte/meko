package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"meko/api_server/internal/api"
)

func TestMiddleware_CORSHeaders(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	e, err := api.NewRouter(api.ServerConfig{DB: db})
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	srv := httptest.NewServer(e)
	defer srv.Close()

	req, err := http.NewRequest(
		http.MethodOptions, srv.URL+"/api/v1/health", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS request failed: %v", err)
	}
	defer resp.Body.Close()

	origin := resp.Header.Get("Access-Control-Allow-Origin")
	if origin != "*" {
		t.Errorf("expected CORS origin *, got %s", origin)
	}
}

func TestMiddleware_RecoveryViaRouter(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	e, err := api.NewRouter(api.ServerConfig{DB: db})
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d",
			http.StatusOK, resp.StatusCode)
	}
}
