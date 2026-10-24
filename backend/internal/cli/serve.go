package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/auth"
	"github.com/yugabyte/meko/backend/internal/callhome"
	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/logging"
	"github.com/yugabyte/meko/backend/internal/login"
	"github.com/yugabyte/meko/backend/internal/mcp"
	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/memoryengine"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/rest"
	"github.com/yugabyte/meko/backend/internal/secrets"
	"github.com/yugabyte/meko/backend/internal/server"
	"github.com/yugabyte/meko/backend/internal/store"
	"github.com/yugabyte/meko/backend/internal/storeapi"
	"github.com/yugabyte/meko/backend/internal/supervisor"
	"github.com/yugabyte/meko/backend/internal/update"
	"github.com/yugabyte/meko/backend/internal/version"
	"github.com/yugabyte/meko/backend/internal/webui"
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

// testCallHomeFirstCheck, if set, replaces the usage stats' random first delay, so a test sees a
// report within seconds. Only this package's tests set it, as testEmbedder.
var testCallHomeFirstCheck time.Duration

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
// database, listens on 127.0.0.1 (the API listener), writes the admin token, pid and port files,
// and serves; and it runs the web listener beside it (webListener), which hands over from the
// extensions first. On
// the way out it drains requests, stops children, checkpoints the WAL, closes the database and
// removes the run files.
func Serve(ctx context.Context, o ServeOptions) (err error) {
	l, cfg := o.Layout, o.Config
	if err = l.Ensure(); err != nil {
		return err
	}
	lock, err := acquireServeLock(l.Lock)
	if errors.Is(err, errLocked) && os.Getenv("MEKO_SERVICE") == "launchd" {
		// launchd's KeepAlive can't skip one exit code the way systemd's RestartPreventExitStatus=75
		// does, so exiting would only be retried every few seconds: wait for the folder instead.
		lock, err = waitForLock(ctx, l)
		if lock == nil && err == nil {
			return nil // stopped while waiting
		}
	}
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

	// A release install on probation after an update: count this start, and roll back a version
	// that keeps failing to start before it touches the database again.
	app, release, isRelease := runningRelease()
	onProbation := false
	if isRelease {
		p, perr := app.Started(release)
		if perr != nil {
			log.Warn("can't read the pending update", "error", perr)
		}
		if p != nil {
			restored, rerr := store.RestoreBackup(l.DB, p.Schema)
			if rerr != nil {
				return fmt.Errorf("version %s failed to start %d times, and its database can't be rolled back: %w",
					p.To, p.Attempts, rerr)
			}
			if err = app.RollBack(*p); err != nil {
				return err
			}
			msg := fmt.Sprintf("version %s failed to start %d times; rolling back to %s", p.To, p.Attempts, p.From)
			log.Warn(msg, "database_restored", restored)
			// In service.log too: it's what someone reading why the service flapped looks at.
			if f, ferr := os.OpenFile(l.ServiceLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); ferr == nil {
				_, _ = fmt.Fprintf(f, "%s WARN %s\n", time.Now().UTC().Format(time.RFC3339), msg)
				_ = f.Close()
			}
			return &rollbackError{exe: app.Exe(p.From)}
		}
		if pending, _ := app.ReadPending(); pending != nil && pending.To == release {
			onProbation = true
		}
	}

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
	clientToken, err := ensureClientToken(ctx, l, cfg, log)
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
		// On probation after an update: a rollback restores the database, and its rows need their files.
		KeepDeletedFiles: func() bool {
			if !isRelease {
				return false
			}
			p, _ := app.ReadPending()
			return p != nil && p.To == release
		},
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
	// even when a stop arrives during start. Neither failing stops the server (the memory engine is
	// already running, and returning here would skip stopping it): the files wait for the next start.
	boot := context.WithoutCancel(ctx)
	if n, err := svc.RequeueInterrupted(boot); err != nil {
		log.Warn("couldn't queue again the knowledge files the last server didn't finish", "error", err)
	} else if n > 0 {
		log.Info("queued knowledge files again that the last server didn't finish", "count", n)
	}
	if n, err := svc.SweepFiles(boot); err != nil {
		log.Warn("couldn't sweep the knowledge files folder", "error", err)
	} else if n > 0 {
		log.Info("removed knowledge files no document names", "count", n)
	}
	// Usage stats to YugabyteDB's call home service: from release installs, or to [call_home] url.
	usage := callhome.New(callhome.Options{Store: st, Config: cfg.CallHome, Release: isRelease,
		Facts: installFacts(l, cfg, svc), Tools: mcp.ToolNames(), Log: log, FirstCheck: testCallHomeFirstCheck})
	// Embeds turns for conversation search and knowledge-base chunks as they arrive, and any the
	// memory engine missed while down; indexes knowledge files; keeps the call log to its retention;
	// and sends the usage stats.
	embedCtx, stopEmbedder := context.WithCancel(context.Background())
	embedderDone := make(chan struct{})
	go func() {
		defer close(embedderDone)
		var wg sync.WaitGroup
		for _, run := range []func(context.Context){svc.RunCallPruner, svc.RunIndexer, usage.Run} {
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
		Credential: server.Credential, UsageStats: usage, Log: log}
	upload, download := rest.Tickets(restOpts)
	var ui http.Handler
	if dir, err := webui.FindDir(cfg.WebUI.Dir); err == nil {
		ui = webui.Handler(dir)
		log.Info("web UI", "dir", dir)
	} else {
		ui = webui.Missing(err)
		log.Info("no web UI", "reason", err.Error())
	}
	info := server.Info{
		Version: version.Version, Commit: version.Commit, GoVersion: runtime.Version(),
		Libs: libs, DataDir: l.Root, PID: pid, StartedAt: time.Now().UTC(),
	}
	mcpHandler := mcp.NewHandler(mcp.Options{Service: svc, Version: version.Version, Log: log,
		Credential: server.CredentialOf})
	registerREST := func(g *echo.Group) { rest.Register(g, restOpts) }
	logins := login.New(st)
	// The web listener: the web UI and the extensions' for browsers, with local access or the login.
	web := newWebListener(l, st, logins, log)
	logins.OnRevoke(web.RevokeEnded)
	web.newOpts = func(wcfg config.Config) server.Options {
		return server.Options{Info: info, Port: wcfg.Web.Port, Auth: cfg.Server.Auth, Logger: log,
			Children: sup.Statuses, MemoryEngine: svc.EngineStatus, UsageStats: usage.State, MCP: mcpHandler,
			REST: registerREST, Upload: upload, Download: download, UI: ui, Login: logins,
			Web: &server.Web{Access: wcfg.Web.Access, Extensions: web.Proxies}}
	}
	srv := server.New(server.Options{
		Info: info, Port: cfg.Server.Port, Token: token, ClientToken: clientToken, Auth: cfg.Server.Auth, Logger: log,
		Children: sup.Statuses, MemoryEngine: svc.EngineStatus, UsageStats: usage.State, MCP: mcpHandler,
		REST: registerREST, Upload: upload, Download: download, Login: logins,
		WebURL: func() string {
			if u := web.Status(context.Background()).URL; u != "" {
				return u
			}
			return webURL(cfg.Web)
		},
		WebStatus: func() *server.WebStatus { return web.Status(context.Background()) },
	})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	webCtx, stopWeb := context.WithCancel(context.Background())
	defer stopWeb()
	webDone := make(chan struct{})
	go func() {
		defer close(webDone)
		web.Run(webCtx)
	}()
	log.Info("Meko started", "url", fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port),
		"pid", pid, "data_dir", l.Root, "version", version.Version, "auth", cfg.Server.Auth)
	if cfg.Server.Auth == config.AuthNone {
		log.Warn("auth is off ([server] auth = \"none\"): every user and program on this machine can read and " +
			"write your memories through /mcp and /api/v1")
	}

	restart := make(chan struct{})
	updCtx, stopUpdates := context.WithCancel(context.Background())
	defer stopUpdates()
	if isRelease {
		startUpdates(updCtx, app, release, l, cfg, log, onProbation, restart)
	}

	var serveErr error
	select {
	case <-ctx.Done():
		log.Info("stopping")
	case <-restart:
		log.Info("stopping to restart into the update")
	case serveErr = <-served:
	}
	stopUpdates()
	if onProbation && serveErr == nil {
		// A clean stop of a version on probation isn't a failed start.
		if err := app.Uncount(release); err != nil {
			log.Warn("can't update the pending update", "error", err)
		}
	}
	drain, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	stopWeb() // Run drains the web listener (drainTimeout) before it returns
	<-webDone
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

// installFacts is what the usage stats say about this install: versions, platform, settings and
// state, never a name or path.
func installFacts(l paths.Layout, cfg config.Config, svc *memory.Service) func(context.Context) callhome.Install {
	secretsKind := cfg.Secrets.Store
	if secretsKind == "" {
		secretsKind = secrets.DefaultKind()
	}
	service := os.Getenv("MEKO_SERVICE")
	if service != "launchd" && service != "systemd" {
		service = "none"
	}
	return func(ctx context.Context) callhome.Install {
		installed := map[string]bool{}
		reg, err := extensions.Load(l.Root)
		for name := range extensions.Known {
			ok := false
			if err == nil {
				_, ok = reg.Entries[name]
			}
			installed[name] = ok
		}
		return callhome.Install{Version: version.Version, Commit: version.Commit, Platform: runtime.GOOS,
			Arch: runtime.GOARCH, Go: runtime.Version(), Service: service, Auth: cfg.Server.Auth, Secrets: secretsKind,
			Update:       callhome.Update{Channel: cfg.Update.Channel, Auto: cfg.Update.Auto},
			MemoryEngine: svc.EngineStatus(ctx).State, Extensions: installed}
	}
}

// startMemoryEngine starts the memory engine under sup, or says why it can't (no Node, or the memory engine
// isn't built): the server then runs without it, and memory features say why.
func startMemoryEngine(sup *supervisor.Supervisor, l paths.Layout, cfg config.Config, token string,
	log *slog.Logger) string {
	o := memoryengine.Options{Node: cfg.MemoryEngine.Node, Dir: cfg.MemoryEngine.Dir, TestEmbedder: testEmbedder,
		Models: memoryengine.FindModels()}
	node, err := memoryengine.FindNode(o)
	if err == nil {
		var dir string
		if dir, err = memoryengine.FindDir(o); err == nil {
			sup.Start(memoryengine.Spec(node, dir, l, token, o))
			log.Info("memory engine starting", "node", node, "dir", dir, "bundled_models", o.Models != "")
			return ""
		}
	}
	log.Warn("the memory engine can't start; memory features are off", "reason", err)
	return err.Error()
}

// lockRetry is how often a launchd-run serve tries the data folder's lock again while another server
// holds it.
var lockRetry = 5 * time.Second

// waitForLock waits for another server to release the data folder, saying so once on stderr (the
// service's log). It returns a nil lock and error when ctx ends first.
func waitForLock(ctx context.Context, l paths.Layout) (*os.File, error) {
	pid, _ := readInt(l.Pid)
	fmt.Fprintf(os.Stderr, "meko: %v; waiting for it to stop\n", &lockedError{root: l.Root, pid: pid})
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(lockRetry):
		}
		lock, err := acquireLock(l.Lock)
		if !errors.Is(err, errLocked) {
			return lock, err
		}
	}
}

// rollbackError is Serve's way of saying it rolled back a failing update: Run then runs exe, the
// previous version, in its place.
type rollbackError struct{ exe string }

func (e *rollbackError) Error() string { return "rolled back to " + e.exe }

// startUpdates runs a release install's update checks, and finishes an update on probation once it
// has run update.HealthyAfter. Updates are installed only under the service, which starts the new
// version after serve exits; a server started by hand only reports them.
func startUpdates(ctx context.Context, app update.App, release string, l paths.Layout, cfg config.Config,
	log *slog.Logger, onProbation bool, restart chan struct{}) {
	if onProbation {
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(update.HealthyAfter):
				pruned, err := app.Healthy(release)
				if err != nil {
					log.Warn("can't finish the update", "error", err)
					return
				}
				log.Info("the update is done", "version", release, "pruned", pruned)
			}
		}()
	}
	if cfg.Update.Channel == update.ChannelOff {
		return
	}
	schema, err := store.LatestSchema()
	if err != nil {
		log.Warn("updates are off", "error", err)
		return
	}
	var once sync.Once
	u := &update.Updater{
		App: app, Version: release, Client: newReleaseClient(cfg.Update.Repo), DataDir: l.Root,
		Interval: time.Duration(cfg.Update.IntervalHours * float64(time.Hour)), FirstCheck: time.Minute,
		Auto: cfg.Update.Auto && os.Getenv("MEKO_SERVICE") != "", Schema: schema, Log: log,
		Restart: func() { once.Do(func() { close(restart) }) },
	}
	go u.Run(ctx)
}

// keychainRetry is how often serve tries the Keychain again while it refuses (locked, as when the
// Mac has no login session).
var keychainRetry = 10 * time.Second

// ensureClientToken gets the client token from where this install keeps it. A Keychain that
// refuses is waited for, not given up on: under launchd, exiting would only start the server again
// and again.
func ensureClientToken(ctx context.Context, l paths.Layout, cfg config.Config, log *slog.Logger) (string, error) {
	ct, err := clientTokens(l, cfg)
	if err != nil {
		return "", err
	}
	for warned := false; ; {
		tok, err := ct.Ensure()
		if err == nil || ct.Store.Kind() != secrets.KindKeychain {
			return tok, err
		}
		if !warned {
			log.Warn("can't read the client token from the Keychain; trying again every 10 s", "error", err)
			warned = true
		}
		select {
		case <-ctx.Done():
			return "", err
		case <-time.After(keychainRetry):
		}
	}
}

// clientTokens is where this install keeps its client token ([secrets] store).
func clientTokens(l paths.Layout, cfg config.Config) (auth.ClientTokens, error) {
	s, err := secrets.Open(cfg.Secrets.Store, l.Root)
	if err != nil {
		return auth.ClientTokens{}, err
	}
	return auth.ClientTokens{Store: s, File: l.ClientToken}, nil
}

func removeRunFiles(l paths.Layout) {
	for _, p := range []string{l.Pid, l.Port, l.Token, l.StoreSock, l.MemoryEngineSock} {
		_ = paths.RemoveIfExists(p)
	}
}
