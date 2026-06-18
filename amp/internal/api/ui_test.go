package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yugabyte/yb-light/amp/internal/api"
	"github.com/yugabyte/yb-light/amp/internal/config"
	"github.com/yugabyte/yb-light/amp/internal/core"
)

// TestServesUI checks the controller serves the embedded admin console at the
// server root, as HTML, and that the page drives the documented API base path.
func TestServesUI(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.NewServer(&config.Config{ListenAddr: ":0"}, core.NewMemBackend(), newFakeTaskStore(), api.Provisioning{}, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body, ct, code := get(t, ts.URL+"/")
	if code != http.StatusOK {
		t.Fatalf("ui status = %d", code)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("ui content-type = %q, want text/html", ct)
	}
	// The page is a client of the versioned API; it must reference that base.
	if !strings.Contains(body, "/api/v1") {
		t.Errorf("ui page does not reference the API base path")
	}
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Errorf("ui page does not look like an HTML document:\n%.120s", body)
	}
}
