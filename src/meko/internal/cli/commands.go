package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/server"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/update"
	"github.com/yugabyte/meko/src/meko/internal/version"
)

// Timeouts for start and stop. stop waits longer than serve's own shutdown (drain, then
// children), so its SIGKILL is only for a server that hangs.
var (
	startTimeout = 10 * time.Second
	stopTimeout  = drainTimeout + childStopTimeout + 5*time.Second
)

// startOptions are start's and restart's flags.
type startOptions = config.Overrides

// startDetached starts serve (exe, or this binary when it's "") in the background, in its own
// session, and waits until it answers. The server's own stderr goes to logs/start.log, which is
// printed if it never answers, so errors from before logging starts aren't lost.
func startDetached(l paths.Layout, o config.Overrides, exe string, stdout, stderr io.Writer) int {
	if up, err := running(l.Lock); err != nil {
		return fail(stderr, err)
	} else if up {
		if port, err := readInt(l.Port); err == nil {
			newReport(stdout).step(stepOK, "Server", fmt.Sprintf("already running at http://127.0.0.1:%d", port))
		} else {
			newReport(stdout).step(stepWarn, "Server", "already running, and still starting")
		}
		return exitOK
	}
	if err := l.Ensure(); err != nil {
		return fail(stderr, err)
	}
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return fail(stderr, err)
		}
	}
	args := []string{"serve", "--detached", "--data-dir", l.Root}
	if o.Port != 0 {
		args = append(args, "--port", strconv.Itoa(o.Port))
	}
	if o.LogLevel != "" {
		args = append(args, "--log-level", o.LogLevel)
	}
	logf, err := os.OpenFile(l.StartLog, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(stderr, err)
	}
	defer func() { _ = logf.Close() }() // the child has its own copy
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fail(stderr, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			fmt.Fprintf(stderr, "meko: the server stopped while starting\n%s", startLogTail(l))
			return exitError
		case <-time.After(100 * time.Millisecond):
		}
		if c, err := newClient(l); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			st, err := c.status(ctx)
			cancel()
			if err == nil && st.PID == cmd.Process.Pid {
				newReport(stdout).step(stepOK, "Server", fmt.Sprintf("running at http://127.0.0.1:%d (pid %d)", st.Port, st.PID))
				return exitOK
			}
		}
	}
	// Stop it for real before saying so: a retry right away must not find it still running.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(stopTimeout):
		_ = cmd.Process.Kill()
		<-exited
	}
	fmt.Fprintf(stderr, "meko: the server didn't answer within %s, so it was stopped. See %s\n%s",
		startTimeout, l.Log, startLogTail(l))
	return exitError
}

// startLogTail is the end of logs/start.log, indented, or "" if it's empty.
func startLogTail(l paths.Layout) string {
	b, err := os.ReadFile(l.StartLog)
	b = bytes.TrimSpace(b)
	if err != nil || len(b) == 0 {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return "  " + strings.Join(lines, "\n  ") + "\n"
}

// stopMeko stops the server by signal (stopServer) and says what happened.
func stopMeko(l paths.Layout, stdout, stderr io.Writer) int {
	msg, err := stopServer(l)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, msg)
	return exitOK
}

// stopServer sends SIGTERM to the server, waits for it to release the lock, and kills it if it
// takes longer than stopTimeout. It returns what happened, for the user.
func stopServer(l paths.Layout) (string, error) {
	up, err := running(l.Lock)
	if err != nil {
		return "", err
	}
	if !up {
		cleanRunFiles(l) // left behind if the server was killed
		return "Meko isn't running", nil
	}
	pid, err := readInt(l.Pid)
	if err != nil {
		return "", fmt.Errorf(
			"the server is running, but its pid is unknown (%v). Stop the meko serve process yourself", err)
	}
	if err = syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return "", fmt.Errorf("can't stop pid %d: %w", pid, err)
	}
	if waitUnlocked(l.Lock, stopTimeout) {
		return "Meko stopped", nil
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if !waitUnlocked(l.Lock, 5*time.Second) {
		return "", fmt.Errorf("pid %d didn't stop, even after SIGKILL", pid)
	}
	cleanRunFiles(l) // and its children, in process groups the SIGKILL didn't reach
	return fmt.Sprintf("Meko didn't stop within %s, so it was killed", stopTimeout), nil
}

func waitUnlocked(lock string, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if up, err := running(lock); err == nil && !up {
			return true
		}
	}
	return false
}

// statusOptions are status's flags: --verbose for everything, --json for the server's reply.
type statusOptions struct{ verbose, json bool }

// runStatus says whether Meko is running: by default a short checklist and its links, with verbose
// everything (the models, certificate, service, updates and links), with json the server's reply.
func runStatus(l paths.Layout, o statusOptions, stdout, stderr io.Writer) int {
	up, err := running(l.Lock)
	if err != nil {
		return fail(stderr, err)
	}
	if !up {
		switch {
		case o.json:
			b, _ := json.Marshal(map[string]any{"running": false, "data_dir": l.Root})
			fmt.Fprintln(stdout, string(b))
		case o.verbose:
			fmt.Fprintf(stdout, "Meko isn't running (data in %s)\n", l.Root)
			exts, stop := extensionStatuses(l.Root)
			defer stop()
			printSetup(l, exts, stdout)
		default:
			newReport(stdout).headline(stepFail, fmt.Sprintf("Meko isn't running (data in %s)", l.Root))
			fmt.Fprintln(stdout, "  Start it: meko start")
		}
		return exitNotRunning
	}
	// The extensions' own status commands run while the server is asked, and the rows before theirs
	// are printed: each takes a Node start or more, and one that hangs is cut off at its own bound.
	// Returning early (the server didn't answer) stops the ones still running rather than leave them.
	var exts func() extensionResults
	if !o.json {
		var stop func()
		exts, stop = extensionStatuses(l.Root)
		defer stop()
	}
	c, err := newClient(l)
	if err != nil {
		return fail(stderr, fmt.Errorf("the server is running, but %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := c.status(ctx)
	if err != nil {
		return fail(stderr, fmt.Errorf("the server is running, but isn't answering: %w", err))
	}
	switch {
	case o.json:
		b, _ := json.Marshal(st)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		m["running"] = true
		b, _ = json.Marshal(m)
		fmt.Fprintln(stdout, string(b))
	case o.verbose:
		printStatusVerbose(l, st, exts, stdout)
	default:
		printStatusShort(l, c, st, exts, stdout)
	}
	return exitOK
}

// printStatusShort is status's checklist: the context engine, the service, updates and the
// extensions, each a row (and the Meko Cloud link, when it can't be read; otherwise it's the Cloud
// link row), then a ● headline with the version, pid and uptime, the links, as install's summary
// has them, and the commands to run next.
func printStatusShort(l paths.Layout, c *client, st server.StatusReply, exts func() extensionResults,
	stdout io.Writer) {
	r := newReport(stdout)
	k, text := contextEngineStep(st.ContextEngine)
	r.step(k, "Context engine", text)
	switch {
	case st.Web == nil:
	case !st.Web.Enabled:
		r.step(stepWarn, "Web UI", webOffNote)
	case !st.Web.Listening && st.Web.Error != "":
		r.step(stepFail, "Web UI", "not listening: "+st.Web.Error)
	case st.Web.Error != "":
		r.step(stepFail, "Web UI", st.Web.Error)
	}
	if st.Auth == config.AuthNone {
		r.step(stepWarn, "Auth", "none: every user and program on this machine can read and write your memories")
	}
	cl := cloudStatusLine(l)
	if k, text, show := storageStep(st.Storage, strings.HasPrefix(cl, "linked")); show {
		r.step(k, "Storage", text)
	}
	if m := serviceOf(l); m == nil {
		r.step(stepWarn, "Service", "none (meko install sets one up)")
	} else if m.Running() {
		r.step(stepOK, "Service", m.Kind())
	} else {
		r.step(stepWarn, "Service", m.Kind()+", stopped")
	}
	if _, ver, ok := runningRelease(); ok {
		if us, ok := update.ReadState(l.Root); ok {
			switch {
			case us.Error != "":
				r.step(stepWarn, "Updates", "the last check failed: "+us.Error)
			case us.Latest != "" && us.Latest != ver:
				r.step(stepWarn, "Updates", us.Latest+" is available (meko update)")
			default:
				r.step(stepOK, "Updates", "up to date")
			}
		}
	}
	if strings.HasPrefix(cl, "can't") {
		r.step(stepWarn, "Meko Cloud", cl)
	}
	if res := exts(); res.err != nil {
		r.step(stepWarn, "Extensions", "can't read them: "+res.err.Error())
	} else {
		for _, n := range res.names {
			s := res.status[n]
			k, state := stepOK, "running"
			switch {
			case s.TimedOut:
				k, state = stepWarn, res.timedOut(n)
			case !s.Running:
				k, state = stepFail, "not running"
			}
			if !s.TimedOut {
				k, state = extensionState(n, k, state, s.Line, ", %s")
			}
			r.step(k, extensionTitle(n), state)
		}
	}
	r.running(fmt.Sprintf("Meko %s is running", st.Version), fmt.Sprintf("pid %d, up %s", st.PID,
		(time.Duration(st.UptimeSeconds)*time.Second).String()))
	r.links(linkRows(st, cloudRow(l)))
	fmt.Fprintln(stdout, "\nMore detail: meko status --verbose")
	s := readNextState(l, c)
	if st.Web != nil {
		s.webOff = !st.Web.Enabled
	}
	r.next(nextSteps(s, nextUse))
}

// contextEngineStep is the context engine's row: done when ready, failed when it failed, else a warning
// (it's starting, or restarting).
func contextEngineStep(e memory.EngineStatus) (stepKind, string) {
	text := e.State
	if e.Reason != "" {
		text += " (" + e.Reason + ")"
	}
	switch e.State {
	case "ready":
		return stepOK, text
	case "failed":
		return stepFail, text
	}
	return stepWarn, text
}

// linkRows are where people (the web UI) and agents (MCP, and the API) reach Meko, and the Meko
// Cloud it's linked to (cloud).
func linkRows(st server.StatusReply, cloud linkRow) []linkRow {
	var rows []linkRow
	if w := st.Web; w != nil && w.URL != "" && w.Enabled {
		rows = append(rows, linkRow{"Web UI", w.URL, webAccessNote(w)})
	}
	return append(rows, linkRow{"Agents", fmt.Sprintf("http://127.0.0.1:%d/mcp", st.Port), "MCP and API"}, cloud)
}

// extensionTitle is how a known extension is named to people (AgentK), else its name.
func extensionTitle(name string) string {
	if c, ok := extensions.Known[name]; ok && c.Title != "" {
		return c.Title
	}
	return name
}

// extensionState is extension name's row in status, its mark and state, from its status command's
// first line. A line that starts with a mark, as the extensions' own headline does ("✓ AgentK 0.2
// is running (pid 7, up 2h)"), gives the row its mark (a server that runs but doesn't answer is
// "!" though it exits 0) and its state: the line without the mark and the extension's title, or
// one of its old titles, which an older release still prints. Any other line is added to state
// (running or not running) through format, and k (from the exit code) stays.
func extensionState(name string, k stepKind, state, line, format string) (stepKind, string) {
	for mark, kind := range map[string]stepKind{"✓ ": stepOK, "! ": stepWarn, "✗ ": stepFail} {
		if rest, ok := strings.CutPrefix(line, mark); ok {
			for _, t := range append([]string{extensionTitle(name)}, extensions.Known[name].OldTitles...) {
				if after, ok := strings.CutPrefix(rest, t+" "); ok {
					return kind, after
				}
			}
			return kind, rest
		}
	}
	if line == "" {
		return k, state
	}
	return k, state + fmt.Sprintf(format, line)
}

// printStatusVerbose is status --verbose: everything the server and this install say.
func printStatusVerbose(l paths.Layout, st server.StatusReply, exts func() extensionResults, stdout io.Writer) {
	fmt.Fprintf(stdout, "Meko is running\n")
	fmt.Fprintf(stdout, "  Agents:         http://127.0.0.1:%d/mcp (MCP and API; the CLI uses it too)\n", st.Port)
	printWeb(stdout, st.Web)
	fmt.Fprintf(stdout, "  Version:        %s\n", st.Version)
	fmt.Fprintf(stdout, "  PID:            %d\n", st.PID)
	fmt.Fprintf(stdout, "  Uptime:         %s\n", (time.Duration(st.UptimeSeconds) * time.Second).String())
	fmt.Fprintf(stdout, "  Data:           %s\n", st.DataDir)
	contextEngineLine := "  Context engine: " + st.ContextEngine.State
	if e := st.ContextEngine.Embedder; e != nil && e.Model != "" {
		contextEngineLine += fmt.Sprintf(", %s %s (%d dims)", e.Provider, e.Model, e.Dims)
	}
	if st.ContextEngine.Reason != "" {
		contextEngineLine += " (" + st.ContextEngine.Reason + ")"
	}
	fmt.Fprintln(stdout, contextEngineLine)
	if r := st.ContextEngine.Reranker; r != nil && r.Model != "" {
		line := fmt.Sprintf("  Reranker:       %s, %s %s", r.State, r.Provider, r.Model)
		if r.Error != "" {
			line += " (" + r.Error + ")"
		}
		fmt.Fprintln(stdout, line)
	}
	if st.Auth == config.AuthNone {
		fmt.Fprintln(stdout,
			"  Auth:           none (every user and program on this machine can read and write your memories)")
	} else {
		fmt.Fprintln(stdout, "  Auth:           token (`meko token` prints it)")
	}
	if s := st.Storage; s != nil {
		fmt.Fprintf(stdout, "  Storage:        %s of %s used (memories %s, conversations %s, knowledge %s, artifacts %s)\n",
			humanSize(s.UsedBytes), limitSize(s.LimitBytes), humanSize(s.Memories), humanSize(s.Conversations),
			humanSize(s.Knowledge), humanSize(s.Artifacts))
	}
	if len(st.Children) == 0 {
		fmt.Fprintf(stdout, "  Children:       none\n")
	}
	for _, ch := range st.Children {
		line := fmt.Sprintf("  Child:          %s %s, %d restarts", ch.Name, ch.State, ch.Restarts)
		if ch.LastError != "" {
			line += " (" + ch.LastError + ")"
		}
		fmt.Fprintln(stdout, line)
	}
	printSetup(l, exts, stdout)
}

// storageWarnAt is the share of the storage limit from which status's checklist shows a Storage row.
const storageWarnAt = 0.9

// storageStep is the checklist's Storage row: none while there's room, a warning from storageWarnAt
// of the limit, failed when Meko is full (writes are refused). linked says Meko Cloud is linked.
func storageStep(s *memory.StorageStatus, linked bool) (stepKind, string, bool) {
	if s == nil || s.LimitBytes <= 0 {
		return stepOK, "", false
	}
	used := fmt.Sprintf("%s of %s", humanSize(s.UsedBytes), limitSize(s.LimitBytes))
	switch {
	case s.UsedBytes >= s.LimitBytes && linked:
		return stepFail, "full, " + used + ": delete what you don't need, or keep larger datasets in Meko Cloud", true
	case s.UsedBytes >= s.LimitBytes:
		return stepFail, "full, " + used + ": delete what you don't need, or run meko cloud-link for larger datasets",
			true
	case float64(s.UsedBytes) >= storageWarnAt*float64(s.LimitBytes):
		return stepWarn, used + " used", true
	}
	return stepOK, "", false
}

// limitSize is the storage limit as config.toml sets it, in whole MB.
func limitSize(n int64) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MB", n>>20)
	}
	return humanSize(n)
}

// printSetup prints how Meko is installed: its service, its updates, the Meko Cloud link and its
// extensions (exts, whose status commands are under way).
func printSetup(l paths.Layout, exts func() extensionResults, stdout io.Writer) {
	if m := serviceOf(l); m != nil {
		state := "stopped"
		if m.Running() {
			state = "running"
		}
		fmt.Fprintf(stdout, "  Service:        %s, %s (%s)\n", m.Kind(), state, m.File())
	} else {
		fmt.Fprintln(stdout, "  Service:        none (meko install sets one up)")
	}
	if app, ver, ok := runningRelease(); ok {
		line := "  Updates:        release " + ver
		if st, ok := update.ReadState(l.Root); ok {
			switch {
			case st.Error != "":
				line += fmt.Sprintf("; the last check (%s) failed: %s", st.CheckedAt.Local().Format(time.DateTime), st.Error)
			case st.Latest != "" && st.Latest != ver:
				line += fmt.Sprintf("; %s is available (meko update)", st.Latest)
			default:
				line += fmt.Sprintf(", the newest (checked %s)", st.CheckedAt.Local().Format(time.DateTime))
			}
		}
		if p, _ := app.ReadPending(); p != nil && p.To == ver {
			line += fmt.Sprintf("; updated from %s, on probation (%d starts)", p.From, p.Attempts)
		}
		fmt.Fprintln(stdout, line)
	} else {
		fmt.Fprintln(stdout, "  Updates:        none (a build from source)")
	}
	fmt.Fprintln(stdout, "  Meko Cloud:     "+cloudStatusLine(l))
	res := exts()
	if res.err != nil {
		fmt.Fprintf(stdout, "  Extensions:     can't read them: %v\n", res.err)
		return
	}
	for _, n := range res.names {
		s := res.status[n]
		state := "running"
		switch {
		case s.TimedOut:
			state = res.timedOut(n)
		case !s.Running:
			state = "not running"
		}
		if !s.TimedOut {
			_, state = extensionState(n, stepOK, state, s.Line, " (%s)")
		}
		fmt.Fprintf(stdout, "  Extension:      %s, %s\n", n, state)
	}
}

// extensionResults are the registered extensions' status commands' results, by name, with the
// names in order; or why the registry couldn't be read.
type extensionResults struct {
	reg    *extensions.Registry
	names  []string
	status map[string]extensions.StatusResult
	err    error
}

// extensionStatuses starts every registered extension's `<command> status` at once, each bounded
// by extensions.StatusTimeout, and returns a function that waits for them, and one that stops the
// ones still running and waits for them to end (deferred: a no-op once they're all done).
func extensionStatuses(root string) (wait func() extensionResults, stop func()) {
	reg, err := extensions.Load(root)
	if err != nil {
		return func() extensionResults { return extensionResults{err: err} }, func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	results := reg.Statuses(ctx, root, extensions.StatusTimeout)
	wait = func() extensionResults {
		return extensionResults{reg: reg, names: reg.Names(), status: results()}
	}
	return wait, func() {
		cancel()
		results()
	}
}

// timedOut is an extension's state when its status didn't answer in time, naming the command to
// run by hand: meko kode status, or the registered program's for an extension Meko doesn't know.
func (r extensionResults) timedOut(name string) string {
	cat, ok := extensions.Known[name]
	if !ok {
		cat.Command = filepath.Base(r.reg.Entries[name].Command)
	}
	return fmt.Sprintf("didn't answer in %s (%s)", extensions.StatusTimeout, cat.UserCommand("status"))
}

// mcpAccess is how an MCP client reaches this Meko: the MCP URL, on the running server's port or the
// configured one, and the client token it sends; and whether a server runs.
type mcpAccess struct {
	url, token string
	running    bool
}

// readMCPAccess reads the client token, making it if no server has yet. It reads where the install
// keeps it (the data folder, or the Keychain), so it works whether or not the server runs. While one
// runs it only reads: the server keeps the token it loaded at start, so a token made now (after the
// old one was deleted to replace it) would be one the server refuses.
func readMCPAccess(l paths.Layout) (config.Config, mcpAccess, error) {
	if err := l.Ensure(); err != nil {
		return config.Config{}, mcpAccess{}, err
	}
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return cfg, mcpAccess{}, err
	}
	ct, err := clientTokens(l, cfg)
	if err != nil {
		return cfg, mcpAccess{}, err
	}
	up, err := running(l.Lock)
	if err != nil {
		return cfg, mcpAccess{}, err
	}
	var tok string
	if up {
		tok, err = ct.Read()
		if errors.Is(err, fs.ErrNotExist) {
			err = fmt.Errorf("the client token is gone, but the running server still takes the one it loaded at start. " +
				"Restart Meko (meko restart, or meko stop then meko start) to make a new one")
		}
	} else {
		tok, err = ct.Ensure()
	}
	if err != nil {
		return cfg, mcpAccess{}, err
	}
	// Only a running server's port file is current: one killed rather than stopped leaves its file.
	port := cfg.Server.Port
	if up {
		if p, perr := readInt(l.Port); perr == nil {
			port = p
		}
	}
	return cfg, mcpAccess{url: fmt.Sprintf("http://127.0.0.1:%d/mcp", port), token: tok, running: up}, nil
}

// runToken prints the client token (readMCPAccess), and with asJSON the MCP URL, the token, whether
// a server runs and the Meko Cloud link, for extensions (AgentK reads it).
func runToken(l paths.Layout, asJSON bool, stdout, stderr io.Writer) int {
	cfg, a, err := readMCPAccess(l)
	if err != nil {
		return fail(stderr, err)
	}
	if !asJSON {
		fmt.Fprintln(stdout, a.token)
		return exitOK
	}
	// web_url is where browsers find Meko's web UI and its extensions' (AgentK's at /ext/agentk/):
	// "" while it's off ([web] enabled), when web_off says so.
	web, off := webURL(cfg.Web, cfg.Server.Port), !cfg.Web.Enabled
	if a.running {
		if c, cerr := newClient(l); cerr == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if st, serr := c.status(ctx); serr == nil && st.Web != nil && st.Web.URL != "" {
				web, off = st.Web.URL, !st.Web.Enabled
			}
			cancel()
		}
	}
	reply := map[string]any{"url": a.url, "token": a.token, "running": a.running, "web_url": web,
		"cloud": cloudTokenFor(l, cfg)}
	if off {
		reply["web_url"], reply["web_off"] = "", true
	}
	// cloud is the Meko Cloud link the extensions use (docs/cloud.md). One that can't be read (a
	// bad cloud.toml, a locked Keychain) says so rather than failing the reply: this Meko's own
	// URL and token still are, and an extension keeps the link it last read.
	b, _ := json.Marshal(reply)
	fmt.Fprintln(stdout, string(b))
	return exitOK
}

func runVersion(asJSON bool, stdout, stderr io.Writer) int {
	v, err := store.LibVersions()
	if err != nil {
		return fail(stderr, err)
	}
	schema, err := store.LatestSchema()
	if err != nil {
		return fail(stderr, err)
	}
	if asJSON {
		// The updater runs a staged release's meko version --json to check it runs here.
		b, _ := json.Marshal(map[string]any{
			"version": version.Version, "commit": version.Commit, "go": runtime.Version(),
			"os": runtime.GOOS, "arch": runtime.GOARCH, "sqlite": v.SQLite, "sqlite_vec": v.SQLiteVec, "schema": schema,
		})
		fmt.Fprintln(stdout, string(b))
		return exitOK
	}
	fmt.Fprintf(stdout, "meko %s (commit %s)\n", version.Version, version.Commit)
	fmt.Fprintf(stdout, "  Go          %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(stdout, "  SQLite      %s\n", v.SQLite)
	fmt.Fprintf(stdout, "  sqlite-vec  %s\n", v.SQLiteVec)
	fmt.Fprintf(stdout, "  Schema      %d\n", schema)
	return exitOK
}
