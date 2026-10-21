// Package testenv runs a whole Meko server for tests: the store, its API on a unix socket, the
// Node engine (with the hashing embedder: no model download) under a supervisor, the service, and
// the HTTP server on a free port. Tests that need no engine use the packages directly.
package testenv

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/auth"
	"github.com/yugabyte/meko/backend/internal/engine"
	"github.com/yugabyte/meko/backend/internal/mcp"
	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/rest"
	"github.com/yugabyte/meko/backend/internal/server"
	"github.com/yugabyte/meko/backend/internal/store"
	"github.com/yugabyte/meko/backend/internal/storeapi"
	"github.com/yugabyte/meko/backend/internal/supervisor"
)

// The tokens the server takes.
const (
	AdminToken  = "admin-token-for-tests"
	ClientToken = "client-token-for-tests"
)

// Env is a running server.
type Env struct {
	T          *testing.T
	Layout     paths.Layout
	Store      *store.Store
	Service    *memory.Service
	Server     *server.Server
	URL        string // http://127.0.0.1:<port>
	DatapackID string // the default datapack
	UserID     string
}

// Options change what Start runs.
type Options struct {
	Auth string // the [server] auth mode; "" is the default (token)
}

// backendDir is this module's root, found from this file.
func backendDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// Start runs a server and the engine, and waits until the engine is ready. Without a built engine
// (or Node) the test is skipped, except in CI, where make test builds it first.
func Start(t *testing.T, o Options) *Env {
	t.Helper()
	missing := func(err error) {
		if os.Getenv("CI") != "" {
			t.Fatal(err)
		}
		t.Skipf("%v (make test builds the engine)", err)
	}
	dir, err := engine.FindDir(engine.Options{Dir: filepath.Join(backendDir(), "engine")})
	if err != nil {
		missing(err)
	}
	node, err := engine.FindNode(engine.Options{})
	if err != nil {
		missing(err)
	}
	ctx := context.Background()
	root, err := os.MkdirTemp("/tmp", "mk-") // short: unix sockets have a path length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	l := paths.New(root)
	if err = l.Ensure(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(l.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dp, err := st.EnsureDefaultDatapack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	userID, err := st.EnsureLocalUserID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	internal, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sln, err := storeapi.Listen(l.StoreSock)
	if err != nil {
		t.Fatal(err)
	}
	api := storeapi.New(st, internal, log, nil)
	go func() { _ = api.Serve(sln) }()
	t.Cleanup(func() { _ = api.Shutdown(context.Background()) })

	sup := supervisor.New(log)
	sp := engine.Spec(node, dir, l, internal, engine.Options{TestEmbedder: "hash"})
	sp.MinBackoff, sp.MaxBackoff = 50*time.Millisecond, 100*time.Millisecond
	sup.Start(sp)
	t.Cleanup(func() { _ = sup.Stop(context.Background()) })
	svc := memory.New(st, engine.NewClient(l.EngineSock, internal), memory.Options{UserID: userID, Log: log})
	// The sweep that embeds turns for conversation search, as serve runs it.
	sweep, stopSweep := context.WithCancel(context.Background())
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		svc.RunEmbedder(sweep)
	}()
	t.Cleanup(func() {
		stopSweep()
		<-swept
	})

	ln, err := server.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("not a TCP address: %v", ln.Addr())
	}
	port := addr.Port
	srv := server.New(server.Options{
		Info: server.Info{Version: "test", DataDir: root, PID: os.Getpid(), StartedAt: time.Now()},
		Port: port, Token: AdminToken, ClientToken: ClientToken, Auth: o.Auth, Logger: log,
		Engine: svc.EngineStatus,
		MCP: mcp.NewHandler(mcp.Options{Service: svc, Version: "test", Log: log,
			Credential: server.CredentialOf}),
		REST: func(g *echo.Group) {
			rest.Register(g, rest.Options{Service: svc, Port: port, ClientToken: ClientToken,
				Credential: server.Credential, Log: log})
		},
	})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	e := &Env{T: t, Layout: l, Store: st, Service: svc, Server: srv, URL: fmt.Sprintf("http://127.0.0.1:%d", port),
		DatapackID: dp, UserID: userID}
	e.waitEngine()
	return e
}

// waitEngine waits until the engine is ready.
func (e *Env) waitEngine() {
	e.T.Helper()
	var st memory.EngineStatus
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if st = e.Service.EngineStatus(context.Background()); st.State == "ready" {
			return
		}
	}
	log, _ := os.ReadFile(e.Layout.EngineLog)
	e.T.Fatalf("the engine never got ready: %+v\nengine.log:\n%s", st, log)
}
