// Package testenv runs a whole Meko server for tests: the store, its API on a unix socket, the
// memory engine (with the hashing embedder: no model download) under a supervisor, the service, and
// the HTTP server on a free port. Tests that need no memory engine use the packages directly.
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
	"github.com/yugabyte/meko/backend/internal/callhome"
	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/mcp"
	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/memoryengine"
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
	// UsageStats is the usage stats' setting. It never sends: Run isn't started, and it has no URL
	// unless Options.CallHome gives one.
	UsageStats *callhome.Sender
}

// Options change what Start runs.
type Options struct {
	Auth string // the [server] auth mode; "" is the default (token)
	// CallHome is the [call_home] config; nil is the default (on, no URL).
	CallHome *config.CallHome
}

// backendDir is this module's root, found from this file.
func backendDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// Start runs a server and the memory engine, and waits until the memory engine is ready. Without a
// built memory engine (or Node) the test is skipped, except in CI, where make test builds it first.
func Start(t *testing.T, o Options) *Env {
	t.Helper()
	missing := func(err error) {
		if os.Getenv("CI") != "" {
			t.Fatal(err)
		}
		t.Skipf("%v (make test builds the memory engine)", err)
	}
	dir, err := memoryengine.FindDir(memoryengine.Options{Dir: filepath.Join(backendDir(), "memory_engine")})
	if err != nil {
		missing(err)
	}
	node, err := memoryengine.FindNode(memoryengine.Options{})
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
	sp := memoryengine.Spec(node, dir, l, internal, memoryengine.Options{TestEmbedder: "hash"})
	sp.MinBackoff, sp.MaxBackoff = 50*time.Millisecond, 100*time.Millisecond
	sup.Start(sp)
	t.Cleanup(func() { _ = sup.Stop(context.Background()) })
	ln, err := server.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("not a TCP address: %v", ln.Addr())
	}
	port := addr.Port
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	svc := memory.New(st, memoryengine.NewClient(l.MemoryEngineSock, internal), memory.Options{UserID: userID, Log: log,
		FilesDir: l.KnowledgeFiles, BaseURL: url})
	// The sweep that embeds turns and knowledge-base chunks, and the indexer, as serve runs them.
	sweep, stopSweep := context.WithCancel(context.Background())
	swept := make(chan struct{}, 2)
	go func() {
		defer func() { swept <- struct{}{} }()
		svc.RunEmbedder(sweep)
	}()
	go func() {
		defer func() { swept <- struct{}{} }()
		svc.RunIndexer(sweep)
	}()
	t.Cleanup(func() {
		stopSweep()
		<-swept
		<-swept
	})

	callHome := config.CallHome{Enabled: true}
	if o.CallHome != nil {
		callHome = *o.CallHome
	}
	usage := callhome.New(callhome.Options{Store: st, Config: callHome, Tools: mcp.ToolNames(), Log: log})
	restOpts := rest.Options{Service: svc, Port: port, ClientToken: ClientToken, Credential: server.Credential,
		UsageStats: usage, Log: log}
	upload, download := rest.Tickets(restOpts)
	srv := server.New(server.Options{
		Info: server.Info{Version: "test", DataDir: root, PID: os.Getpid(), StartedAt: time.Now()},
		Port: port, Token: AdminToken, ClientToken: ClientToken, Auth: o.Auth, Logger: log,
		MemoryEngine: svc.EngineStatus, UsageStats: usage.State,
		MCP: mcp.NewHandler(mcp.Options{Service: svc, Version: "test", Log: log,
			Credential: server.CredentialOf}),
		REST:   func(g *echo.Group) { rest.Register(g, restOpts) },
		Upload: upload, Download: download,
	})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	e := &Env{T: t, Layout: l, Store: st, Service: svc, Server: srv, URL: url, DatapackID: dp, UserID: userID,
		UsageStats: usage}
	e.waitMemoryEngine()
	return e
}

// waitMemoryEngine waits until the memory engine is ready.
func (e *Env) waitMemoryEngine() {
	e.T.Helper()
	var st memory.EngineStatus
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if st = e.Service.EngineStatus(context.Background()); st.State == "ready" {
			return
		}
	}
	log, _ := os.ReadFile(e.Layout.MemoryEngineLog)
	e.T.Fatalf("the memory engine never got ready: %+v\nmemory_engine.log:\n%s", st, log)
}
