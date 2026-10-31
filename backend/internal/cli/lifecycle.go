package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/server"
	"github.com/yugabyte/meko/backend/internal/service"
)

// newManager is this OS's service manager for the user; tests replace it.
var newManager = func() (service.Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return service.For(home, service.Exec)
}

// serviceOf is the installed service that runs this data folder, or nil. A service installed for
// another data folder is left alone.
func serviceOf(l paths.Layout) service.Manager {
	m, err := newManager()
	if err != nil || !m.Installed() || !paths.SameDir(m.DataDir(), l.Root) {
		return nil
	}
	return m
}

// serviceStartTimeout is how long start, restart and install wait for the service's server to
// answer: longer than start's own wait, since the service manager may back off first.
var serviceStartTimeout = time.Minute

// waitAnswering waits until a server holds the lock and answers status, other than the one with
// pid notPID (a server being replaced). It returns its status.
func waitAnswering(l paths.Layout, d time.Duration, notPID int) (server.StatusReply, bool) {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if up, err := running(l.Lock); err != nil || !up {
			continue
		}
		c, err := newClient(l)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		st, err := c.status(ctx)
		cancel()
		if err == nil && st.PID != notPID {
			return st, true
		}
	}
	return server.StatusReply{}, false
}

// currentPID is the running server's pid, or 0.
func currentPID(l paths.Layout) int {
	if up, _ := running(l.Lock); !up {
		return 0
	}
	pid, _ := readInt(l.Pid)
	return pid
}

// runStart starts Meko (through its service when it has one), then the extensions, and prints the
// summary: a row for each, the links, and what to run next.
func runStart(l paths.Layout, o startOptions, stdout, stderr io.Writer) int {
	fmt.Fprintln(stdout, "Starting Meko…")
	code := startMeko(l, o, stdout, stderr)
	if code == exitOK {
		runExtensions(l, "start", stdout, stderr)
		printRunning(l, "is running", [][2]string{
			{"meko ui", "open the web UI"},
			{"meko status", "check on Meko at any time"},
			{"meko-cli", "search and add to what Meko keeps"},
			{"meko agent connect claude", "connect Claude Code to Meko (or codex, kiro, opencode, cursor)"},
		}, stdout)
	}
	return code
}

// webWait is how long start and install wait for the web listener, which binds a moment after the
// API listener answers (after the extensions' handoff), before they print its link.
var webWait = 10 * time.Second

// waitWebListening reads the server's status, and while the web listener is still starting (not
// listening, and not saying why) reads it again until ctx ends. It returns the last status it read.
func waitWebListening(ctx context.Context, c *client) (server.StatusReply, error) {
	st, err := c.status(ctx)
	for err == nil && st.Web != nil && st.Web.Enabled && !st.Web.Listening && st.Web.Error == "" {
		select {
		case <-ctx.Done():
			return st, nil
		case <-time.After(200 * time.Millisecond):
		}
		next, nerr := c.status(ctx)
		if nerr != nil {
			if ctx.Err() != nil {
				return st, nil
			}
			return st, nerr
		}
		st = next
	}
	return st, err
}

// printRunning is the end of start's and install's summary: the memory engine's and web listener's
// rows, a box of links titled "Meko <version> <title>", the commands to run next, and AgentK's
// callout.
func printRunning(l paths.Layout, title string, next [][2]string, stdout io.Writer) {
	r := newReport(stdout)
	c, err := newClient(l)
	if err != nil {
		r.step(stepWarn, "Server", "can't ask it how it's doing: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), webWait)
	defer cancel()
	st, err := waitWebListening(ctx, c)
	if err != nil {
		r.step(stepWarn, "Server", "not answering: "+err.Error())
		return
	}
	k, text := memoryEngineStep(st.MemoryEngine)
	r.step(k, "Memory engine", text)
	if w := st.Web; w != nil {
		switch {
		case !w.Enabled:
			r.step(stepWarn, "Web UI", webOffNote)
		case w.Listening:
			r.step(stepOK, "Web UI", "listening")
		case w.Error != "":
			r.step(stepFail, "Web UI", "not listening: "+w.Error)
		default:
			r.step(stepWarn, "Web UI", "starting (meko ui waits for it)")
		}
	}
	r.box(fmt.Sprintf("Meko %s %s", st.Version, title), linkRows(st, l.Root))
	r.next(next)
	switch {
	case agentKRegistered(l):
		fmt.Fprintln(stdout, "\nTry AgentK, your coding team: meko ui /ext/agentk/")
	case !slices.ContainsFunc(next, func(c [2]string) bool { return strings.HasPrefix(c[0], "meko extension install") }):
		fmt.Fprintln(stdout, "\nAdd AgentK, your coding team: meko extension install agentk")
	}
}

// agentKRegistered reports whether meko start and stop run AgentK.
func agentKRegistered(l paths.Layout) bool {
	reg, err := extensions.Load(l.Root)
	if err != nil {
		return false
	}
	_, ok := reg.Entries["agentk"]
	return ok
}

// errServiceFlags: the service runs serve with only --data-dir, so it reads the port and log level
// from config.toml; start and restart can't pass them on.
var errServiceFlags = errors.New("a service runs Meko here, and it reads config.toml: set [server] port " +
	"or [log] level there, then run meko restart")

func startMeko(l paths.Layout, o startOptions, stdout, stderr io.Writer) int {
	m := serviceOf(l)
	if m == nil {
		return startDetached(l, o, "", stdout, stderr)
	}
	if o.Port != 0 || o.LogLevel != "" {
		return fail(stderr, errServiceFlags)
	}
	if up, err := running(l.Lock); err != nil {
		return fail(stderr, err)
	} else if up {
		return startDetached(l, o, "", stdout, stderr) // says it's already running
	}
	if err := m.Start(); err != nil {
		return fail(stderr, err)
	}
	st, ok := waitAnswering(l, serviceStartTimeout, 0)
	if !ok {
		return fail(stderr, fmt.Errorf("the %s service didn't answer within %s. Check: %s, and meko logs",
			m.Kind(), serviceStartTimeout, m.StatusHint()))
	}
	newReport(stdout).step(stepOK, "Server", fmt.Sprintf("running at http://127.0.0.1:%d (pid %d), through %s",
		st.Port, st.PID, m.Kind()))
	return exitOK
}

// runStop stops the extensions (unless keepExtensions), then Meko: through its service when it
// has one, since a service manager would start a server that only gets a signal again.
func runStop(l paths.Layout, keepExtensions bool, stdout, stderr io.Writer) int {
	fmt.Fprintln(stdout, "Stopping Meko…")
	r := newReport(stdout)
	if !keepExtensions {
		runExtensions(l, "stop", stdout, stderr)
	}
	if m := serviceOf(l); m != nil {
		if err := m.Stop(); err != nil {
			fmt.Fprintf(stderr, "meko: %v\n", err)
		}
		if up, _ := running(l.Lock); !up {
			cleanRunFiles(l)
			r.step(stepOK, "Server", "stopped")
			return exitOK
		}
		// Still running: a server started by hand on the same folder. Stop it as below.
	}
	msg, err := stopServer(l)
	if err != nil {
		return fail(stderr, err)
	}
	r.step(stopStep(msg), "Server", strings.TrimPrefix(msg, "Meko "))
	return exitOK
}

// runRestart restarts Meko alone, never the extensions.
func runRestart(l paths.Layout, o startOptions, stdout, stderr io.Writer) int {
	pid := currentPID(l)
	if m := serviceOf(l); m != nil {
		if o.Port != 0 || o.LogLevel != "" {
			return fail(stderr, errServiceFlags)
		}
		if err := m.Restart(); err != nil {
			return fail(stderr, err)
		}
		st, ok := waitAnswering(l, serviceStartTimeout, pid)
		if !ok {
			return fail(stderr, fmt.Errorf("the %s service didn't answer within %s. Check: %s, and meko logs",
				m.Kind(), serviceStartTimeout, m.StatusHint()))
		}
		newReport(stdout).headline(stepOK, fmt.Sprintf(
			"Meko restarted at http://127.0.0.1:%d (pid %d); the extensions kept running", st.Port, st.PID))
		return exitOK
	}
	fmt.Fprintln(stdout, "Restarting Meko…")
	msg, err := stopServer(l)
	if err != nil {
		return fail(stderr, err)
	}
	newReport(stdout).step(stopStep(msg), "Server", strings.TrimPrefix(msg, "Meko "))
	return startDetached(l, o, "", stdout, stderr)
}

// stopStep is how stopServer's message shows as a row: a warning when it had to kill the server.
func stopStep(msg string) stepKind {
	if strings.HasSuffix(msg, "killed") {
		return stepWarn
	}
	return stepOK
}

// runExtensions runs `<command> <action>` for every registered extension, one after another. A
// failure is reported and the others still run; an extension that hangs holds this up for at most
// its timeout (extensions.StopTimeout for stop), and then Meko goes on. A known extension whose
// command is missing (its home moved, say) is looked for again, and the registry keeps what's found;
// one that isn't found is damaged (extensions.Damaged).
//
// Each runs with extensions.SummaryEnv set, so it prints its own row of start's or stop's summary;
// a failed one gets a ✗ row from Meko after what it printed.
func runExtensions(l paths.Layout, action string, stdout, stderr io.Writer) {
	reg, err := extensions.Load(l.Root)
	if err != nil {
		fmt.Fprintf(stderr, "meko: can't read the extensions: %v\n", err)
		return
	}
	r := newReport(stdout)
	for _, name := range reg.Names() {
		e := reg.Entries[name]
		if _, err := os.Stat(e.Command); errors.Is(err, os.ErrNotExist) {
			found := findAgain(l, reg, name)
			if found == "" {
				r.step(stepFail, extensionTitle(name), extensions.Damaged(name, e.Command))
				continue
			}
			e = reg.Entries[name]
		}
		if err := e.RunSummary(l.Root, action, stdout, stderr); err != nil {
			r.step(stepFail, extensionTitle(name), fmt.Sprintf("didn't %s: %v", action, err))
		}
	}
}

// findAgain looks for a known extension's command again, and saves it in the registry when it's
// found. It returns the command, or "".
func findAgain(l paths.Layout, reg *extensions.Registry, name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return reg.FindAgain(name, home, l.Root)
}
