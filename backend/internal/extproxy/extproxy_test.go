package extproxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A socket that's gone (the extension stopped, or its home moved) is forgotten, so the next request
// asks where the extension listens again; the browser is told it isn't running.
func TestGoneSocketIsForgotten(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "xp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	gone := 0
	p := New("agentk", "AgentK", func() string { return filepath.Join(dir, "web.sock") }, slog.New(slog.DiscardHandler))
	p.OnGone = func() { gone++ }
	if p.Available() || gone != 1 {
		t.Fatalf("available with no socket, or not forgotten (%d)", gone)
	}
	req := httptest.NewRequest(http.MethodGet, "/ext/agentk/", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || gone != 2 {
		t.Fatalf("request to a gone socket: %d, forgotten %d times", rec.Code, gone)
	}
}
