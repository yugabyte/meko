package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/auth"
	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/logging"
	"github.com/yugabyte/meko/backend/internal/mcp"
	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/memoryengine"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/rest"
	"github.com/yugabyte/meko/backend/internal/server"
	"github.com/yugabyte/meko/backend/internal/store"
	"github.com/yugabyte/meko/backend/internal/storeapi"
	"github.com/yugabyte/meko/backend/internal/supervisor"
	"github.com/yugabyte/meko/backend/internal/version"
)

// Shutdown limits.
const (
	drainTimeout     = 10 * time.Second
	childStopTimeout = 15 * time.Second
)

// testEmbedder, if set, makes the memory engine use a deterministic hashing embedder instead of
// downloading its model. Only this package's tests set it (cli_test.go's TestMain), so no
// environment can switch the shipped binary's model.
var testEmbedder string

// ServeOptions configure Serve.
type ServeOptions struct {
	Layout paths.Layout
	Config config.Config
	Stderr io.Writer // text logs while in a terminal; nil logs only to the file
}

// SignalContext ends on SIGTERM or SIGINT.
func SignalContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	return ctx
}

// Serve runs the server until ctx ends: it locks the data folder, opens and migrates the
// database, listens on 127.0.0.1, writes the admin token, pid and port files, and serves. On
// the way out it drains requests, stops children, checkpoints the WAL, closes the database and
// removes the run files.
func Serve(ctx context.Context, o ServeOptions) (err error) {
	l, cfg := o.Layout, o.Config
	if err = l.Ensure(); err != nil {
		return err
	}
	lock, err := acquireServeLock(l.Lock)
	if errors.Is(err, errLocked) {
		pid, _ := readInt(l.Pid)
		return &lockedError{root: l.Root, pid: pid}
	}
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	// A killed server leaves its run files behind. Replace the pid file now (WriteFile renames over
	// it, so it's never missing), so the lock and the pid file always describe the same process: stop
	// reads the pid while the lock is held. The port and token are written once serve listens.
	defer removeRunFiles(l)
	pid := os.Getpid()
	if err = paths.WriteFile(l.Pid, []byte(strconv.Itoa(pid)+"\n")); err != nil {
		return err
	}
	for _, p := range []string{l.Port, l.Token} {
		if err = paths.RemoveIfExists(p); err != nil {
			return err
		}
	}

	level, _ := cfg.Log.SlogLevel()
	logFile, err := logging.OpenRotating(l.Log, logging.MaxBytes, logging.Keep)
	if err != nil {
		return err
	}
	// Closes last: the log has nowhere to record its own failure, so it joins the error returned.
	defer func() { err = errors.Join(err, logFile.Close()) }()
	log := logging.New(logFile, o.Stderr, level)
	defer func() {
		if err != nil {
			log.Error("server stopped with an error", "error", err)
		}
	}()

	st, err := store.Open(l.DB)
	if err != nil {
		return err
	}
	// Runs before the deferred error log above, so a failed close is logged too.
	defer func() { err = errors.Join(err, st.Close()) }()
	if err = st.Migrate(ctx); err != nil {
		return err
	}
	if _, err = st.EnsureInstallID(ctx); err != nil {
		return err
	}
	if _, err = st.EnsureDefaultDatapack(ctx); err != nil {
		return err
	}
	userID, err := st.EnsureLocalUserID(ctx)
	if err != nil {
		return err
	}
	libs, err := store.LibVersions()
	if err != nil {
		return err
	}

	ln, err := server.Listen(cfg.Server.Port)
	if err != nil {
		return err
	}
	defer ln.Close()
	token, err := auth.NewToken()
	if err != nil {
		return err
	}
	if err = auth.WriteToken(l.Token, token); err != nil {
		return err
	}
	if err = paths.WriteFile(l.Port, []byte(strconv.Itoa(cfg.Server.Port)+"\n")); err != nil {
		return err
	}
	// The clients' token: made on first start, then kept, since clients store it once.
	clientToken, err := auth.EnsureClientToken(l.ClientToken)
	if err != nil {
		return err
	}

	// The store API the memory engine calls, on its unix socket with a per-start token of its own.
	internalToken, err := auth.NewToken()
	if err != nil {
		return err
	}
	sln, err := storeapi.Listen(l.StoreSock)
	if err != nil {
		return err
	}
	sapi := storeapi.New(st, internalToken, log, nil)
	go func() {
		if err := sapi.Serve(sln); err != nil {
			log.Error("store API stopped", "error", err)
		}
	}()

	// Children a killed server left behind (their pid files are in run/children).
	if n, err := supervisor.KillRecorded(l.ChildrenDir); err != nil || n > 0 {
		log.Warn("killed children a previous server left running", "count", n, "error", err)
	}
	sup := supervisor.New(log)
	noMemoryEngine := startMemoryEngine(sup, l, cfg, internalToken, log)
	svc := memory.New(st, memoryengine.NewClient(l.MemoryEngineSock, internalToken), memory.Options{
		UserID: userID, Log: log, NoMemoryEngine: noMemoryEngine, MaxDistance: cfg.ConversationSearch.MaxDistance,
		KBMaxDistance: cfg.KnowledgeBase.MaxDistance, FilesDir: l.KnowledgeFiles,
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port),
		MemoryEngineChild: func() (supervisor.Status, bool) {
			for _, c := range sup.Statuses() {
				if c.Name == memoryengine.Name {
					return c, true
				}
			}
			return supervisor.Status{}, false
		},
	})
	// Knowledge files a stopped server was indexing go back in the queue, and files no document
	// names (a crash between storing a file and its row) are removed. Both are quick, and finish
	// even when a stop arrives during start.
	boot := context.WithoutCancel(ctx)
	if n, err := svc.RequeueInterrupted(boot); err != nil {
		return err
	} else if n > 0 {
		log.Info("queued knowledge files again that the last server didn't finish", "count", n)
	}
	if n, err := svc.SweepFiles(boot); err != nil {
		log.Warn("couldn't sweep the knowledge files folder", "error", err)
	} else if n > 0 {
		log.Info("removed knowledge files no document names", "count", n)
	}
	// Embeds turns for conversation search and knowledge-base chunks as they arrive, and any the
	// memory engine missed while down; indexes knowledge files; and keeps the call log to its retention.
	embedCtx, stopEmbedder := context.WithCancel(context.Background())
	embedderDone := make(chan struct{})
	go func() {
		defer close(embedderDone)
		var wg sync.WaitGroup
		for _, run := range []func(context.Context){svc.RunCallPruner, svc.RunIndexer} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				run(embedCtx)
			}()
		}
		svc.RunEmbedder(embedCtx)
		wg.Wait()
	}()

	restOpts := rest.Options{Service: svc, Port: cfg.Server.Port, ClientToken: clientToken,
		Credential: server.Credential, Log: log}
	upload, download := rest.Tickets(restOpts)
	srv := server.New(server.Options{
		Info: server.Info{
			Version: version.Version, Commit: version.Commit, GoVersion: runtime.Version(),
			Libs: libs, DataDir: l.Root, PID: pid, StartedAt: time.Now().UTC(),
		},
		Port: cfg.Server.Port, Token: token, ClientToken: clientToken, Auth: cfg.Server.Auth, Logger: log,
		Children: sup.Statuses, MemoryEngine: svc.EngineStatus,
		MCP: mcp.NewHandler(mcp.Options{Service: svc, Version: version.Version, Log: log,
			Credential: server.CredentialOf}),
		REST: func(g *echo.Group) {
			rest.Register(g, restOpts)
		},
		Upload: upload, Download: download,
	})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Info("Meko started", "url", fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port),
		"pid", pid, "data_dir", l.Root, "version", version.Version, "auth", cfg.Server.Auth)
	if cfg.Server.Auth == config.AuthNone {
		log.Warn("auth is off ([server] auth = \"none\"): every user and program on this machine can read and " +
			"write your memories through /mcp and /api/v1")
	}

	var serveErr error
	select {
	case <-ctx.Done():
		log.Info("stopping")
	case serveErr = <-served:
	}
	drain, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := srv.Shutdown(drain); err != nil {
		log.Warn("requests didn't finish in time", "error", err)
	}
	stopEmbedder()
	<-embedderDone
	stop, cancelStop := context.WithTimeout(context.Background(), childStopTimeout)
	defer cancelStop()
	if err := sup.Stop(stop); err != nil {
		log.Warn("children didn't stop in time", "error", err)
	}
	if err := sapi.Shutdown(stop); err != nil {
		log.Warn("store API requests didn't finish in time", "error", err)
	}
	if err := st.Checkpoint(context.Background()); err != nil {
		log.Warn("WAL checkpoint failed", "error", err)
	}
	log.Info("Meko stopped")
	return serveErr
}

// startMemoryEngine starts the memory engine under sup, or says why it can't (no Node, or the memory engine
// isn't built): the server then runs without it, and memory features say why.
func startMemoryEngine(sup *supervisor.Supervisor, l paths.Layout, cfg config.Config, token string,
	log *slog.Logger) string {
	o := memoryengine.Options{Node: cfg.MemoryEngine.Node, Dir: cfg.MemoryEngine.Dir, TestEmbedder: testEmbedder}
	node, err := memoryengine.FindNode(o)
	if err == nil {
		var dir string
		if dir, err = memoryengine.FindDir(o); err == nil {
			sup.Start(memoryengine.Spec(node, dir, l, token, o))
			log.Info("memory engine starting", "node", node, "dir", dir)
			return ""
		}
	}
	log.Warn("the memory engine can't start; memory features are off", "reason", err)
	return err.Error()
}

func removeRunFiles(l paths.Layout) {
	for _, p := range []string{l.Pid, l.Port, l.Token, l.StoreSock, l.MemoryEngineSock} {
		_ = paths.RemoveIfExists(p)
	}
}
