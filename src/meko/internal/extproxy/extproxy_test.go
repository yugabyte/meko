package extproxy

import (
	"log/slog"
	"net"
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

// A socket file left by an extension that died refuses connections: it isn't available, and it's
// forgotten; a listening one is available.
func TestALeftOverSocketIsNotAvailable(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "xp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "web.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	gone := 0
	p := New("agentk", "AgentK", func() string { return sock }, slog.New(slog.DiscardHandler))
	p.OnGone = func() { gone++ }
	if !p.Available() || gone != 0 {
		t.Fatalf("a listening socket isn't available (forgotten %d times)", gone)
	}
	ul, ok := ln.(*net.UnixListener)
	if !ok {
		t.Fatalf("not a unix listener: %T", ln)
	}
	ul.SetUnlinkOnClose(false)
	_ = ul.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("the socket file went with the listener: %v", err)
	}
	if p.Available() || gone != 1 {
		t.Errorf("a left-over socket file is available, or not forgotten (%d)", gone)
	}
}
