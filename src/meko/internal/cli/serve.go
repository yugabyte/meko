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

	"github.com/yugabyte/meko/src/meko/internal/agents"
	"github.com/yugabyte/meko/src/meko/internal/auth"
	"github.com/yugabyte/meko/src/meko/internal/callhome"
	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/contextengine"
	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/logging"
	"github.com/yugabyte/meko/src/meko/internal/mcp"
	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/rest"
	"github.com/yugabyte/meko/src/meko/internal/secrets"
	"github.com/yugabyte/meko/src/meko/internal/server"
	"github.com/yugabyte/meko/src/meko/internal/service"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/storeapi"
	"github.com/yugabyte/meko/src/meko/internal/supervisor"
	"github.com/yugabyte/meko/src/meko/internal/update"
	"github.com/yugabyte/meko/src/meko/internal/version"
	"github.com/yugabyte/meko/src/meko/internal/webui"
)

// Shutdown limits.
const (
	drainTimeout     = 10 * time.Second
	childStopTimeout = 15 * time.Second
	// jobsWait is how long an extension's action that was stopped gets to end, alongside the rest.
	jobsWait = 10 * time.Second
)

// testEmbedder, if set, makes the context engine use a deterministic hashing embedder instead of
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
// database, takes Meko's one port (webListener: 127.0.0.1, or every address with remote access),
// writes the admin token, pid and port files, and serves agents, the CLI and browsers on it, with
// the extensions' handoff once it serves. On the way out it drains requests, stops children,
// checkpoints the WAL, closes the database and removes the run files.
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
		// An exec into another version isn't a failure: the rollback and the update log their own.
		var ex *execError
		if err != nil && !errors.As(err, &ex) {
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
			return &execError{exe: app.Exe(p.From), why: "rolled back"}
		}
		if pending, _ := app.ReadPending(); pending != nil && pending.To == release {
			onProbation = true
			// The context engine keeps the old names of its files until MoveRenamed may move them.
			l = l.BeforeRenames()
		} else {
			makeMekoCLI(app, log)
			renameExtensions(l, log)
			l.MoveRenamed()
		}
	} else {
		renameExtensions(l, log) // a build from source is never on probation
		l.MoveRenamed()
	}
	refreshLaunchAgent(l, log)

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

	// The one port, for agents, the CLI and browsers: taken now, so one another program holds stops
	// serve before anything starts. The extensions hand over once it serves (web.Run).
	web := newWebListener(l, st, log, cfg.Server.Port)
	if err = web.Bind(cfg); err != nil {
		return err
	}
	defer web.Close()
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
	// The Meko Cloud link's, in the same secret store.
	cloudSt, err := cloudStore(l, cfg)
	if err != nil {
		return err
	}

	// The store API the context engine calls, on its unix socket with a per-start token of its own.
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
	noContextEngine := startContextEngine(sup, l, cfg, internalToken, log)
	var cloud *cloudlink.Service // set below, before anything serves
	svc := memory.New(st, contextengine.NewClient(l.ContextEngineSock, internalToken), memory.Options{
		UserID: userID, Log: log, NoContextEngine: noContextEngine, MaxDistance: cfg.ConversationSearch.MaxDistance,
		KBMaxDistance: cfg.KnowledgeBase.MaxDistance, FilesDir: l.KnowledgeFiles,
		MaxContentBytes: cfg.Storage.MaxContentBytes(), BaseURL: fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port),
		CloudLinked: func() bool {
			linked, err := cloud.IsLinked()
			return err == nil && linked
		},
		// On probation after an update: a rollback restores the database, and its rows need their files.
		KeepDeletedFiles: func() bool {
			if !isRelease {
				return false
			}
			p, _ := app.ReadPending()
			return p != nil && p.To == release
		},
		ContextEngineChild: func() (supervisor.Status, bool) {
			for _, c := range sup.Statuses() {
				if c.Name == contextengine.Name {
					return c, true
				}
			}
			return supervisor.Status{}, false
		},
	})
	// Knowledge files a stopped server was indexing go back in the queue, and files no document
	// names (a crash between storing a file and its row) are removed. Both are quick, and finish
	// even when a stop arrives during start. Neither failing stops the server (the context engine is
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
	// context engine missed while down; indexes knowledge files; keeps the call log to its retention;
	// and sends the usage stats.
	embedCtx, stopEmbedder := context.WithCancel(context.Background())
	embedderDone := make(chan struct{})
	go func() {
		defer close(embedderDone)
		var wg sync.WaitGroup
		for _, run := range []func(context.Context){svc.RunCallPruner, svc.RunIndexer, svc.RunExtractor, usage.Run} {
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
	mcpOpts := mcp.Options{Service: svc, Version: version.Version, Log: log, Credential: server.CredentialOf}
	// A datapack's search and memories routes answer what the tools do, recorded as /mcp's calls are.
	restOpts.Tool = func(ctx context.Context, cred, name string, args map[string]any) (string, error) {
		return mcp.Call(ctx, mcpOpts, cred, name, args)
	}
	registerREST := func(g *echo.Group) { rest.Register(g, restOpts) }
	// Updates: a release install checks for them, and the Status page checks and installs them.
	restart := make(chan struct{})
	// Closed once this release is past probation: at once, unless it's on probation, then when
	// finishUpdate finishes the update. What a rolled-back release couldn't undo waits for it.
	pastProbation := make(chan struct{})
	var pastOnce sync.Once
	healthy := func() { pastOnce.Do(func() { close(pastProbation) }) }
	if !onProbation {
		healthy()
	}
	updCtx, stopUpdates := context.WithCancel(context.Background())
	defer stopUpdates()
	var updates server.Updates
	if isRelease {
		if u, off := startUpdates(updCtx, app, release, l, cfg, log, onProbation, healthy, restart, web); u != nil {
			updates = u
		} else {
			updates = update.Off{Version: release, Reason: off}
			startExtensionUpdates(updCtx, l, cfg, log, web)
		}
	} else {
		startExtensionUpdates(updCtx, l, cfg, log, web)
	}
	// A login shell that gives no PATH is logged. One still running when serve stops is killed, since
	// it's out of serve's process group, before the log it would write to closes.
	agents.Log = log
	defer func() {
		agents.StopLoginShell()
		agents.Log = nil
	}()
	// The agents on this computer connect to this Meko, linked to Meko Cloud or not.
	local := agents.Target{Kind: agents.KindLocal, URL: fmt.Sprintf("http://127.0.0.1:%d/mcp", cfg.Server.Port),
		Token: clientToken}
	agentsMgr := &agents.Manager{Lock: agentsLock(l), Target: func() (agents.Target, error) { return local, nil }}
	// The Meko Cloud link: the web UI and meko cloud-link change it; /mcp then offers its tools, the
	// agents an older Meko connected to it come back here, and the running extensions hear.
	cloud = &cloudlink.Service{Store: cloudSt, Check: cloudlink.CheckMCP, CheckAPI: cloudlink.CheckAPI,
		List: cloudlink.ListDatapacks,
		Changed: func(ctx context.Context) {
			go cloud.CloudTools(context.WithoutCancel(ctx))
			web.notify(ctx, "cloud")
		},
		Moved: func(ctx context.Context, before, _ cloudlink.State) agents.Moves {
			m := moveBack(ctx, agentsMgr, before, local)
			logMoves(log, m)
			return m
		}}
	mcpOpts.Cloud = cloud
	// The routes that take a datapack send one this Meko doesn't have to the linked Meko Cloud.
	restOpts.Cloud = cloud
	mcpHandler := mcp.NewHandler(mcpOpts)
	// The server for the settings the listener takes up: [web] (access, and whether browsers are let
	// in) may change while serve runs; the rest is read at start.
	web.newOpts = func(wcfg config.Config) server.Options {
		return server.Options{Info: info, Port: cfg.Server.Port, Token: token, ClientToken: clientToken,
			Auth: cfg.Server.Auth, Logger: log, Children: sup.Statuses, ContextEngine: svc.EngineStatus,
			UsageStats: usage.State, Storage: svc.Storage, MCP: mcpHandler, REST: registerREST, Upload: upload,
			Download: download, UI: ui, Cloud: cloud, Agents: agentsMgr, WebStatus: web.Status, ReloadWeb: web.Reload,
			Web: &server.Web{Access: wcfg.Web.Access, Off: !wcfg.Web.Enabled, Extensions: web.Proxies,
				Installed: web.Installed, Run: web.RunExtension, ExtensionUpdates: web.ExtensionUpdates,
				CheckExtensions: web.CheckExtensions, SetAutoUpdate: web.SetAutoUpdate, Jobs: web.jobs, Updates: updates}}
	}
	webCtx, stopWeb := context.WithCancel(context.Background())
	defer stopWeb()
	webDone := make(chan struct{})
	go func() {
		defer close(webDone)
		web.Run(webCtx)
	}()
	// An already-linked Meko moves back the agents it connected to Meko Cloud, once, at its first
	// start where agents stay here; and reads what Meko Cloud offers before the first agent asks.
	followCtx, stopFollow := context.WithCancel(context.Background())
	followDone := make(chan struct{})
	go func() {
		defer close(followDone)
		moveBackOnce(followCtx, st, cloud, log)
		followPort(followCtx, st, agentsMgr, local, cfg.Server.Port, log)
		// Not on probation: a release rolled back to from before one port serves agents only on
		// 7778, and nothing would move them back.
		select {
		case <-pastProbation:
			moveToOnePortOnce(followCtx, st, agentsMgr, local, log)
		case <-followCtx.Done():
		}
	}()
	go cloud.CloudTools(followCtx)
	// The extensions still running from before this start read Meko again: an update or a rollback may
	// have changed its port, token or link, and nothing else tells them.
	go web.notify(followCtx, "cloud")
	log.Info("Meko started", "url", fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port),
		"pid", pid, "data_dir", l.Root, "version", version.Version, "auth", cfg.Server.Auth)
	if cfg.Server.Auth == config.AuthNone {
		log.Warn("auth is off ([server] auth = \"none\"): every user and program on this machine can read and " +
			"write your memories through /mcp and /api/v1")
	}

	// Without a service to start the new version, serve runs it in its own place once it has stopped.
	var execInto string
	select {
	case <-ctx.Done():
		log.Info("stopping")
	case <-restart:
		// An install from the Extensions page runs in this process: finish it first.
		if web.jobs.Busy() {
			log.Info("waiting for an extension's action to finish before restarting into the update")
			web.jobs.WaitIdle(ctx)
		}
		log.Info("stopping to restart into the update")
		if os.Getenv("MEKO_SERVICE") == "" {
			execInto = app.CurrentExe()
		}
	}
	stopUpdates()
	// An action still running is stopped (an install is cut off; Install again finishes it). Its
	// end is only logged, so this waits briefly, while the listeners drain.
	jobsDone := make(chan struct{})
	go func() {
		defer close(jobsDone)
		web.jobs.Cancel(jobsWait)
	}()
	if onProbation {
		// A clean stop of a version on probation isn't a failed start.
		if err := app.Uncount(release); err != nil {
			log.Warn("can't update the pending update", "error", err)
		}
	}
	stopWeb() // Run drains the port (drainTimeout) before it returns
	<-webDone
	stopEmbedder()
	<-embedderDone
	stopFollow()
	<-followDone
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
	<-jobsDone
	log.Info("Meko stopped")
	// A stop asked for while serve was stopping (meko stop, restart or destroy) wins over the exec.
	if execInto != "" && ctx.Err() == nil {
		return &execError{exe: execInto, why: "restarting into the update"}
	}
	return nil
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
			Update:        callhome.Update{Channel: cfg.Update.Channel, Auto: cfg.Update.Auto},
			ContextEngine: svc.EngineStatus(ctx).State, Extensions: installed}
	}
}

// startContextEngine starts the context engine under sup, or says why it can't (no Node, or the context engine
// isn't built): the server then runs without it, and memory features say why.
func startContextEngine(sup *supervisor.Supervisor, l paths.Layout, cfg config.Config, token string,
	log *slog.Logger) string {
	o := contextengine.Options{Node: cfg.ContextEngine.Node, Dir: cfg.ContextEngine.Dir, TestEmbedder: testEmbedder,
		Models: contextengine.FindModels()}
	node, err := contextengine.FindNode(o)
	if err == nil {
		var dir string
		if dir, err = contextengine.FindDir(o); err == nil {
			sup.Start(contextengine.Spec(node, dir, l, token, o))
			log.Info("context engine starting", "node", node, "dir", dir, "bundled_models", o.Models != "")
			return ""
		}
	}
	log.Warn("the context engine can't start; memory features are off", "reason", err)
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

// execError is Serve's way of saying another version must run in its place: the previous one after
// rolling back a failing update, or the new one after an update with no service to start it. Run
// then execs exe with the same arguments.
type execError struct{ exe, why string }

func (e *execError) Error() string { return e.why + ": " + e.exe }

// makeMekoCLI makes this install's meko-cli command (linkMekoCLI), logging why it can't. Serve runs
// it only once its release is past probation: a release rolled back after making the link would
// leave meko-cli pointing at an older meko, which doesn't know that name and runs as meko.
func makeMekoCLI(app update.App, log *slog.Logger) {
	home, err := os.UserHomeDir()
	if err == nil {
		err = linkMekoCLI(app, home)
	}
	if err != nil {
		log.Warn("can't make the meko-cli command", "error", err)
	}
}

// renameExtensions moves this data folder's release installs of extensions onto app/current's
// program by its current name, with no command on PATH (extensions.Registry.Rename): Kode's
// ~/.local/bin/me_koder (or agentk) gives way to app/current/bin/me_kode, run as meko kode. Like
// makeMekoCLI, serve runs it only once its release is past probation: a Meko rolled back after it
// would have a registry naming a program its catalog doesn't know, and old links it can't make again.
func renameExtensions(l paths.Layout, log *slog.Logger) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	reg, err := extensions.Load(l.Root)
	if err == nil {
		err = reg.Rename(l.Root, home)
	}
	if err != nil {
		log.Warn("can't rename the extensions' commands", "error", err)
	}
}

// refreshLaunchAgent brings the LaunchAgent that runs this data folder up to date with this
// version's (service.Launchd.Refresh), without reloading it: an install whose plist ran
// app/current/bin/meko, which macOS announced as a new background item after every update, moves
// to the launcher that updates leave alone. It runs only under launchd.
func refreshLaunchAgent(l paths.Layout, log *slog.Logger) {
	if os.Getenv("MEKO_SERVICE") != "launchd" {
		return
	}
	m := serviceOf(l)
	d, ok := m.(*service.Launchd)
	if !ok {
		return
	}
	exe, err := os.Executable()
	if app, _, isRelease := runningRelease(); isRelease {
		exe, err = app.CurrentExe(), nil
	}
	if err != nil {
		log.Warn("can't update the LaunchAgent", "error", err)
		return
	}
	// The data folder as the plist spells it, so an unchanged plist stays unchanged.
	dir := d.DataDir()
	changed, err := d.Refresh(service.Spec{Exe: exe, DataDir: dir, ServiceLog: paths.New(dir).ServiceLog})
	if err != nil {
		log.Warn("can't update the LaunchAgent", "error", err)
	} else if changed {
		log.Info("updated the LaunchAgent; it takes effect at the next login", "file", d.File())
	}
}

// finishUpdate finishes the update to release once it has run for after (update.HealthyAfter),
// unless ctx ends first, and then makes the meko-cli command, moves the extensions onto their
// commands' new names (renameExtensions) and calls healthy (when set): what serve held back while
// the release was on probation, such as the agents' move to one port, goes ahead.
func finishUpdate(ctx context.Context, app update.App, release string, after time.Duration, l paths.Layout,
	log *slog.Logger, healthy func()) {
	select {
	case <-ctx.Done():
	case <-time.After(after):
		pruned, err := app.Healthy(release)
		if err != nil {
			log.Warn("can't finish the update", "error", err)
			return
		}
		log.Info("the update is done", "version", release, "pruned", pruned)
		makeMekoCLI(app, log)
		renameExtensions(l, log)
		if healthy != nil {
			healthy()
		}
	}
}

// startUpdates runs a release install's update checks, and finishes an update on probation once it
// has run update.HealthyAfter (finishUpdate). On their own, updates are installed only under the
// service, which starts the new version after serve exits; a server started by hand only reports
// them. The same checks find the extensions' releases, and install those set to Update
// automatically (web's autoUpdate), under the service or not. The updater it returns is the Status
// page's too, even with [update] channel = "off" (which stops only the scheduled checks). When the
// schema can't be read it returns nil and why.
func startUpdates(ctx context.Context, app update.App, release string, l paths.Layout, cfg config.Config,
	log *slog.Logger, onProbation bool, healthy func(), restart chan struct{}, web *webListener) (*update.Updater,
	string) {
	if onProbation {
		go finishUpdate(ctx, app, release, update.HealthyAfter, l, log, healthy)
	}
	schema, err := store.LatestSchema()
	if err != nil {
		log.Warn("updates are off", "error", err)
		return nil, "can't read this version's database schema: " + err.Error()
	}
	var once sync.Once
	service := os.Getenv("MEKO_SERVICE") != ""
	u := &update.Updater{
		App: app, Version: release, Client: releaseClient(cfg.Update, unsignedLog(log)), DataDir: l.Root,
		Interval: time.Duration(cfg.Update.IntervalHours * float64(time.Hour)), FirstCheck: time.Minute,
		Auto: cfg.Update.Auto && service, Schema: schema, Log: log, Channel: cfg.Update.Channel,
		Service: service, Context: ctx,
		Restart: func() { once.Do(func() { close(restart) }) },
	}
	web.watchUpdates(u)
	if cfg.Update.Channel != update.ChannelOff {
		go u.Run(ctx)
	}
	return u, ""
}

// unsignedLog logs the notice of a release staged without a signature check.
func unsignedLog(log *slog.Logger) func(string) {
	return func(s string) { log.Warn("a release was staged without a signature check", "notice", s) }
}

// startExtensionUpdates runs the extensions' update checks for a Meko that doesn't update itself (a
// build from source, or one whose updater couldn't be set up), as startUpdates does with Meko's.
func startExtensionUpdates(ctx context.Context, l paths.Layout, cfg config.Config, log *slog.Logger,
	web *webListener) {
	u := &update.Updater{ExtensionsOnly: true, Version: version.Version,
		Client: releaseClient(cfg.Update, unsignedLog(log)), DataDir: l.Root,
		Interval:   time.Duration(cfg.Update.IntervalHours * float64(time.Hour)),
		FirstCheck: time.Minute, Log: log, Channel: cfg.Update.Channel, Context: ctx}
	web.watchUpdates(u)
	if cfg.Update.Channel != update.ChannelOff {
		go u.Run(ctx)
	}
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
	for _, p := range []string{l.Pid, l.Port, l.Token, l.StoreSock, l.ContextEngineSock} {
		_ = paths.RemoveIfExists(p)
	}
}

// keyAgentsOnThisMeko records that this Meko moved back the agents it had connected to the linked
// Meko Cloud before agents stayed on this Meko, so it runs once.
const keyAgentsOnThisMeko = "agents_on_this_meko"

// moveBackOnce moves back to this Meko the agents connected to the linked Meko Cloud with its key,
// at the first start of a Meko whose agents stay here: one linked before then has agents connected
// to Meko Cloud. It runs once, whether or not Meko Cloud is linked then; a stop during it leaves it
// for the next start.
func moveBackOnce(ctx context.Context, st *store.Store, cloud *cloudlink.Service, log *slog.Logger) {
	if _, done, err := st.Setting(ctx, keyAgentsOnThisMeko); err != nil || done {
		return
	}
	// Under the link's lock (MoveAgents), so it runs in order with a change's own move. The service's
	// Moved logs what it did.
	if _, err := cloud.MoveAgents(ctx); err != nil {
		log.Warn("can't read the Meko Cloud link to move the agents back from it", "error", err)
		return
	}
	if ctx.Err() != nil {
		return
	}
	if err := st.SetSetting(ctx, keyAgentsOnThisMeko, time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Warn("can't record that the agents came back from Meko Cloud", "error", err)
	}
}

// keyAgentsOnOnePort records that this Meko moved the agents connected to the listener agents had
// before one port (127.0.0.1:7778) to its one port, so it runs once.
const keyAgentsOnOnePort = "agents_one_port"

// oldAgentsURL is where a Meko before one port had its agents connect: its agents' listener's MCP URL.
const oldAgentsURL = "http://127.0.0.1:7778/mcp"

// moveToOnePortOnce connects to the one port, once, the agents a Meko before it connected to its
// agents' listener: only those whose meko server is at exactly oldAgentsURL (an Exact move), so one
// pointed by hand at another Meko on this computer is left as it is. It's recorded only when no
// agent failed, so a failed one is tried again at the next start; a stop during it leaves it for
// the next start too.
func moveToOnePortOnce(ctx context.Context, st *store.Store, m *agents.Manager, to agents.Target, log *slog.Logger) {
	if _, done, err := st.Setting(ctx, keyAgentsOnOnePort); err != nil || done {
		return
	}
	from := agents.Target{Kind: agents.KindLocal, URL: oldAgentsURL, Token: to.Token, Exact: true}
	moves := m.Move(ctx, from, to)
	logMoves(log, moves)
	if ctx.Err() != nil || len(moves.Failed) > 0 {
		return
	}
	if err := st.SetSetting(ctx, keyAgentsOnOnePort, time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Warn("can't record that the agents moved to Meko's one port", "error", err)
	}
}

// keyAgentsPort is the port Meko's agents were last connected to (followPort).
const keyAgentsPort = "agents_port"

// followPort moves the agents to this start's port when it isn't the one they were connected to:
// meko install moves Meko to a free port when another program holds its own (pickPort), and
// someone may change [server] port by hand. Only agents whose meko server is at exactly the old
// port's URL move (an Exact move), so one pointed at another Meko on this computer stays. The new
// port is recorded once no agent failed, so a failed one is tried again at the next start. With
// none recorded yet (the first start of a release with followPort), the agents' port is taken to be
// the default: install may have moved Meko off it in the same run that brought followPort.
func followPort(ctx context.Context, st *store.Store, m *agents.Manager, to agents.Target, port int,
	log *slog.Logger) {
	last, ok, err := st.Setting(ctx, keyAgentsPort)
	if err != nil {
		return
	}
	if !ok {
		last = strconv.Itoa(config.DefaultPort)
	}
	now := strconv.Itoa(port)
	if last != now {
		from := agents.Target{Kind: agents.KindLocal, URL: "http://127.0.0.1:" + last + "/mcp", Token: to.Token,
			Exact: true}
		moves := m.Move(ctx, from, to)
		logMoves(log, moves)
		if ctx.Err() != nil || len(moves.Failed) > 0 {
			return
		}
	} else if ok {
		return
	}
	if err := st.SetSetting(ctx, keyAgentsPort, now); err != nil {
		log.Warn("can't record the port Meko's agents use", "error", err)
	}
}

// logMoves logs what a move did to the agents, by name: never a URL's token or a key.
func logMoves(log *slog.Logger, m agents.Moves) {
	for _, a := range m.Moved {
		log.Info("agent moved to Meko's new target", "agent", a.Name)
	}
	for _, a := range m.Failed {
		log.Warn("couldn't move an agent to Meko's new target", "agent", a.Name, "error", a.Error)
	}
}
