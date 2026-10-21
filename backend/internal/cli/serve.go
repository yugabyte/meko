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
	"syscall"
	"time"

	"github.com/yugabyte/meko/backend/internal/auth"
	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/engine"
	"github.com/yugabyte/meko/backend/internal/logging"
	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/paths"
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

// testEmbedder, if set, makes the engine use a deterministic hashing embedder instead of
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

	// The store API the engine calls, on its unix socket with a per-start token of its own.
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
	noEngine := startEngine(sup, l, cfg, internalToken, log)
	svc := memory.New(st, engine.NewClient(l.EngineSock, internalToken), memory.Options{
		UserID: userID, Log: log, NoEngine: noEngine, MaxDistance: cfg.ConversationSearch.MaxDistance,
		EngineChild: func() (supervisor.Status, bool) {
			for _, c := range sup.Statuses() {
				if c.Name == engine.Name {
					return c, true
				}
			}
			return supervisor.Status{}, false
		},
	})
	// Embeds turns for conversation search as they arrive, and any the engine missed while down.
	embedCtx, stopEmbedder := context.WithCancel(context.Background())
	embedderDone := make(chan struct{})
	go func() {
		defer close(embedderDone)
		svc.RunEmbedder(embedCtx)
	}()

	srv := server.New(server.Options{
		Info: server.Info{
			Version: version.Version, Commit: version.Commit, GoVersion: runtime.Version(),
			Libs: libs, DataDir: l.Root, PID: pid, StartedAt: time.Now().UTC(),
		},
		Port: cfg.Server.Port, Token: token, Logger: log, Children: sup.Statuses, Engine: svc.EngineStatus,
	})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Info("Meko started", "url", fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port),
		"pid", pid, "data_dir", l.Root, "version", version.Version)

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

// startEngine starts the memory engine under sup, or says why it can't (no Node, or the engine
// isn't built): the server then runs without it, and memory features say why.
func startEngine(sup *supervisor.Supervisor, l paths.Layout, cfg config.Config, token string, log *slog.Logger) string {
	o := engine.Options{Node: cfg.Engine.Node, Dir: cfg.Engine.Dir, TestEmbedder: testEmbedder}
	node, err := engine.FindNode(o)
	if err == nil {
		var dir string
		if dir, err = engine.FindDir(o); err == nil {
			sup.Start(engine.Spec(node, dir, l, token, o))
			log.Info("memory engine starting", "node", node, "dir", dir)
			return ""
		}
	}
	log.Warn("the memory engine can't start; memory features are off", "reason", err)
	return err.Error()
}

func removeRunFiles(l paths.Layout) {
	for _, p := range []string{l.Pid, l.Port, l.Token, l.StoreSock, l.EngineSock} {
		_ = paths.RemoveIfExists(p)
	}
}
