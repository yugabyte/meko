// Package testenv runs a whole Meko server for tests: the store, its API on a unix socket, the
// context engine (with the hashing embedder: no model download) under a supervisor, the service, and
// the HTTP server on a free port. Tests that need no context engine use the packages directly.
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

	"github.com/yugabyte/meko/src/meko/internal/auth"
	"github.com/yugabyte/meko/src/meko/internal/callhome"
	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/cloudsync"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/contextengine"
	"github.com/yugabyte/meko/src/meko/internal/mcp"
	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/rest"
	"github.com/yugabyte/meko/src/meko/internal/server"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/storeapi"
	"github.com/yugabyte/meko/src/meko/internal/supervisor"
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
	// Cloud is the Meko Cloud link the REST routes send other datapacks to; nil leaves it out.
	Cloud *cloudlink.Service
	// MCPCloud, if set, is the linked Meko Cloud /mcp sends calls to while it's linked.
	MCPCloud mcp.Cloud
	// ExtractorHarness, if set, answers the memory extractor's calls instead of a real CLI.
	ExtractorHarness memory.Harness
	// Answerer, if set, writes search answers instead of a real CLI.
	Answerer memory.Answerer
	// MaxContentBytes, if set, is the storage limit instead of the default ([storage] max_content_mb).
	MaxContentBytes int64
	// Credential, if set, is the REST routes' credential check instead of the server's, for a
	// request with credentials the server's check doesn't give.
	Credential func(echo.Context) string
}

// backendDir is this module's root, found from this file.
func backendDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// Start runs a server and the context engine, and waits until the context engine is ready. Without a
// built context engine (or Node) the test is skipped, except in CI, where make test builds it first.
func Start(t *testing.T, o Options) *Env {
	t.Helper()
	missing := func(err error) {
		if os.Getenv("CI") != "" {
			t.Fatal(err)
		}
		t.Skipf("%v (make test builds the context engine)", err)
	}
	dir, err := contextengine.FindDir(contextengine.Options{Dir: filepath.Join(backendDir(), "context_engine")})
	if err != nil {
		missing(err)
	}
	node, err := contextengine.FindNode(contextengine.Options{})
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
	sp := contextengine.Spec(node, dir, l, internal, contextengine.Options{TestEmbedder: "hash"})
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
	svc := memory.New(st, contextengine.NewClient(l.ContextEngineSock, internal), memory.Options{UserID: userID, Log: log,
		FilesDir: l.KnowledgeFiles, BaseURL: url, ExtractorHarness: o.ExtractorHarness,
		Answerer: o.Answerer, MaxContentBytes: o.MaxContentBytes, CloudLinked: func() bool {
			if o.MCPCloud == nil {
				return false
			}
			linked, err := o.MCPCloud.IsLinked()
			return err == nil && linked
		}})
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
	mcpOpts := mcp.Options{Service: svc, Version: "test", Log: log, Credential: server.CredentialOf, Cloud: o.MCPCloud}
	credential := server.Credential
	if o.Credential != nil {
		credential = o.Credential
	}
	restOpts := rest.Options{Service: svc, Port: port, ClientToken: ClientToken, Credential: credential,
		UsageStats: usage, Log: log, Cloud: o.Cloud,
		Tool: func(ctx context.Context, cred, name string, args map[string]any) (string, error) {
			return mcp.Call(ctx, mcpOpts, cred, name, args)
		}}
	if o.Cloud != nil {
		restOpts.Sync = &cloudsync.Syncer{Service: svc, Cloud: o.Cloud, Version: "test", Log: log}
	}
	upload, download := rest.Tickets(restOpts)
	srv := server.New(server.Options{
		Info: server.Info{Version: "test", DataDir: root, PID: os.Getpid(), StartedAt: time.Now()},
		Port: port, Token: AdminToken, ClientToken: ClientToken, Auth: o.Auth, Logger: log,
		ContextEngine: svc.EngineStatus, UsageStats: usage.State, Storage: svc.Storage,
		MCP:    mcp.NewHandler(mcpOpts),
		REST:   func(g *echo.Group) { rest.Register(g, restOpts) },
		Upload: upload, Download: download,
	})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	e := &Env{T: t, Layout: l, Store: st, Service: svc, Server: srv, URL: url, DatapackID: dp, UserID: userID,
		UsageStats: usage}
	e.waitContextEngine()
	return e
}

// waitContextEngine waits until the context engine is ready.
func (e *Env) waitContextEngine() {
	e.T.Helper()
	var st memory.EngineStatus
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if st = e.Service.EngineStatus(context.Background()); st.State == "ready" {
			return
		}
	}
	log, _ := os.ReadFile(e.Layout.ContextEngineLog)
	e.T.Fatalf("the context engine never got ready: %+v\ncontext-engine.log:\n%s", st, log)
}
