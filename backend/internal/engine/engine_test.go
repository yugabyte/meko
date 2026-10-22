package engine

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/backend/internal/paths"
)

// fakeNode writes a script that prints version like node --version.
func fakeNode(t *testing.T, version string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "node")
	script := []byte("#!/bin/sh\necho " + version + "\n")
	if err := os.WriteFile(p, script, 0o700); err != nil { //nolint:gosec // G306: a test script must run
		t.Fatal(err)
	}
	return p
}

func TestFindNode(t *testing.T) {
	for _, c := range []struct {
		version string
		ok      bool
	}{
		{"v22.13.0", true}, {"v22.23.3", true}, {"v24.1.0", true}, {"v23.0.0-rc.1", true},
		{"v22.12.9", false}, {"v20.18.0", false}, {"not a version", false},
	} {
		got, err := FindNode(Options{Node: fakeNode(t, c.version)})
		if c.ok != (err == nil) || c.ok && got == "" {
			t.Errorf("%s: %q, %v", c.version, got, err)
		}
	}
	if _, err := FindNode(Options{Node: filepath.Join(t.TempDir(), "none")}); err == nil ||
		!strings.Contains(err.Error(), "MEKO_NODE") {
		t.Errorf("a missing node: %v", err)
	}
}

func TestFindDir(t *testing.T) {
	dir := t.TempDir()
	if _, err := FindDir(Options{Dir: dir}); err == nil || !strings.Contains(err.Error(), "make engine") {
		t.Errorf("an unbuilt engine: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "main.js"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := FindDir(Options{Dir: dir}); err != nil || got != dir {
		t.Errorf("a built engine: %q, %v", got, err)
	}
}

func TestSpec(t *testing.T) {
	l := paths.New("/data")
	sp := Spec("/bin/node", "/engine", l, "tok", Options{TestEmbedder: "hash"})
	env := strings.Join(sp.Env, "\n")
	for _, want := range []string{"MEKO_ENGINE_SOCKET=/data/run/engine.sock", "MEKO_STORE_SOCKET=/data/run/store.sock",
		"MEKO_INTERNAL_TOKEN=tok", "MEKO_MODELS_DIR=/data/models", "MEM0_TELEMETRY=false",
		"MEKO_ENGINE_TEST_EMBEDDER=hash"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
	if sp.Args[0] != "/engine/dist/main.js" || sp.PidFile != "/data/run/children/engine" || sp.LogFile != l.EngineLog {
		t.Errorf("spec %+v", sp)
	}
}

// serveFake answers every request on a unix socket with status and body.
func serveFake(t *testing.T, status int, body string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mk-") // a short path: unix sockets have a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "e.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint:gosec // a test
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func TestClientErrors(t *testing.T) {
	ctx := context.Background()
	c := NewClient(filepath.Join(t.TempDir(), "none.sock"), "tok")
	if _, err := c.Health(ctx); !errors.Is(err, ErrUnavailable) {
		t.Errorf("no engine: %v", err)
	}
	c = NewClient(serveFake(t, 503, `{"data":null,"error":{"code":"ENGINE_NOT_READY","message":"loading"}}`), "tok")
	var notReady *NotReadyError
	if _, err := c.Search(ctx, SearchRequest{}); !errors.As(err, &notReady) || notReady.Message != "loading" {
		t.Errorf("not ready: %v", err)
	}
	c = NewClient(serveFake(t, 404, `{"data":null,"error":{"code":"NOT_FOUND","message":"no such memory"}}`), "tok")
	var api *APIError
	if err := c.Delete(ctx, "dp", "m"); !errors.As(err, &api) || api.Code != "NOT_FOUND" || api.Status != 404 {
		t.Errorf("an API error: %v", err)
	}
	c = NewClient(serveFake(t, 200, `{"data":{"state":"ready","embedder":{"provider":"p","model":"m","dims":3}}}`), "tok")
	if h, err := c.Health(ctx); err != nil || h.State != "ready" || h.Embedder.Dims != 3 {
		t.Errorf("health: %+v %v", h, err)
	}
	c = NewClient(serveFake(t, 200, `not json`), "tok")
	if _, err := c.Health(ctx); err == nil {
		t.Error("a reply that isn't JSON was accepted")
	}
	c = NewClient(serveFake(t, 503, `{"data":null,"error":{"code":"RERANKER_NOT_READY","message":"downloading"}}`), "tok")
	if _, err := c.Rerank(ctx, "q", []string{"a"}); !errors.Is(err, ErrRerankerNotReady) {
		t.Errorf("reranker not ready: %v", err)
	}
	c = NewClient(serveFake(t, 200, `{"data":{"scores":[1.5,-2]}}`), "tok")
	if s, err := c.Rerank(ctx, "q", []string{"a", "b"}); err != nil || len(s) != 2 || s[0] != 1.5 {
		t.Errorf("rerank: %v %v", s, err)
	}
	c = NewClient(serveFake(t, 200, `{"data":{"state":"ready","embedder":{"provider":"p","model":"m","dims":3},`+
		`"reranker":{"provider":"p","model":"r","state":"loading"}}}`), "tok")
	if h, err := c.Health(ctx); err != nil || h.Reranker == nil || h.Reranker.State != "loading" {
		t.Errorf("health with a reranker: %+v %v", h, err)
	}
}
