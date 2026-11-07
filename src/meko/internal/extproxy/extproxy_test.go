package extproxy

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	p := New("kode", "Kode", func() string { return filepath.Join(dir, "web.sock") }, slog.New(slog.DiscardHandler))
	p.OnGone = func() { gone++ }
	if p.Available() || gone != 1 {
		t.Fatalf("available with no socket, or not forgotten (%d)", gone)
	}
	req := httptest.NewRequest(http.MethodGet, "/ext/kode/", nil)
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
	p := New("kode", "Kode", func() string { return sock }, slog.New(slog.DiscardHandler))
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

// Served at the path its release names, the other of its paths sends a browser there, uncached;
// the request goes on with the path it was served under.
func TestServesAtItsPathAndRedirectsTheOther(t *testing.T) {
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
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := r.URL.Path + " " + r.Header.Get(PrefixHeader)
		_, _ = w.Write([]byte(reply)) //nolint:gosec // G705: a test server's plain-text reply
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	at := "/ext/agentk/"
	p := New("kode", "Kode", func() string { return sock }, slog.New(slog.DiscardHandler))
	p.Old, p.Prefix = []string{"agentk"}, func() string { return at }
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := get("/ext/agentk/p/1"); rec.Code != 200 || rec.Body.String() != "/p/1 /ext/agentk/" {
		t.Errorf("at its old path: %d %q", rec.Code, rec.Body.String())
	}
	rec := get("/ext/kode/p/1?x=1")
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/ext/agentk/p/1?x=1" ||
		rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the other path: %d %q %q", rec.Code, rec.Header().Get("Location"), rec.Header().Get("Cache-Control"))
	}
	at = "/ext/kode/"
	if rec := get("/ext/kode/p/1"); rec.Code != 200 || rec.Body.String() != "/p/1 /ext/kode/" {
		t.Errorf("after the update: %d %q", rec.Code, rec.Body.String())
	}
	if rec := get("/ext/agentk/x"); rec.Code != http.StatusTemporaryRedirect ||
		rec.Header().Get("Location") != "/ext/kode/x" {
		t.Errorf("the old path after the update: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}
