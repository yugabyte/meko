package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yugabyte/yb-amp/controller/internal/api"
	"github.com/yugabyte/yb-amp/controller/internal/config"
	"github.com/yugabyte/yb-amp/controller/internal/core"
)

// TestServesSpecAndExplorer checks the controller hosts the OpenAPI document
// and the interactive explorer (Swagger UI) off the API server, and that the
// health endpoints answer.
func TestServesSpecAndExplorer(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.NewServer(&config.Config{ListenAddr: ":0"}, core.NewMemBackend(), newFakeTaskStore(), api.Provisioning{}, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	t.Run("spec", func(t *testing.T) {
		body, ct, code := get(t, ts.URL+"/api/v1/openapi.yaml")
		if code != http.StatusOK {
			t.Fatalf("spec status = %d", code)
		}
		if !strings.HasPrefix(ct, "application/yaml") {
			t.Errorf("spec content-type = %q, want application/yaml", ct)
		}
		if !strings.Contains(body, "openapi: 3.0") {
			t.Errorf("spec body does not look like an OpenAPI 3 document:\n%.120s", body)
		}
	})

	t.Run("explorer", func(t *testing.T) {
		body, _, code := get(t, ts.URL+"/api/v1/docs")
		if code != http.StatusOK {
			t.Fatalf("docs status = %d", code)
		}
		// The Swagger UI index references the spec URL it renders.
		if !strings.Contains(body, "/api/v1/openapi.yaml") {
			t.Errorf("explorer page does not reference the spec URL")
		}
	})

	t.Run("healthz", func(t *testing.T) {
		if _, _, code := get(t, ts.URL+"/healthz"); code != http.StatusOK {
			t.Fatalf("healthz status = %d", code)
		}
	})

	t.Run("readyz", func(t *testing.T) {
		if _, _, code := get(t, ts.URL+"/readyz"); code != http.StatusOK {
			t.Fatalf("readyz status = %d (mem backend is always healthy)", code)
		}
	})
}

func get(t *testing.T, url string) (body, contentType string, code int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(b), resp.Header.Get("Content-Type"), resp.StatusCode
}
