package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/server"
	"github.com/yugabyte/meko/src/meko/internal/service"
	"github.com/yugabyte/meko/src/meko/internal/spin"
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
		printRunning(l, runningOptions{kind: nextStart}, stdout)
	}
	return code
}

// webWait is how long start, install and meko ui wait for the port to be listening, when it's being
// taken again (remote access turned on or off), before they print its link.
var webWait = 10 * time.Second

// waitWebListening reads the server's status, and while the port is being taken again (not
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

// engineWait is how long start and install wait for the context engine to be ready (it loads its
// models first) before its row says it's still loading.
var engineWait = time.Minute

// engineLoading reports whether the context engine is on its way to ready: what start and install
// wait through.
func engineLoading(state string) bool {
	return state == "starting" || state == "loading" || state == "restarting"
}

// runningOptions are what start's and install's summaries differ in.
type runningOptions struct {
	kind nextFor // which commands to offer next
	// verb and since, when set, end the headline with how long the run took: "installed in 14s".
	verb  string
	since time.Time
	// pick, when set, asks which extension to add after the links, and adds it: install's picker
	// (pickExtension). It returns the extension added, or "".
	pick func(r report) string
}

// printRunning is the end of start's and install's summary: the context engine's row, which shows
// the dots until it's ready (at most engineWait); the web UI's row, when it's off or not listening;
// a ● headline with the version, the links, and the commands to run next (nextSteps).
func printRunning(l paths.Layout, o runningOptions, stdout io.Writer) {
	r := newReport(stdout)
	c, err := newClient(l)
	if err != nil {
		r.step(stepWarn, "Server", "can't ask it how it's doing: "+err.Error())
		return
	}
	if err = waitEngine(c, stdout); err != nil {
		r.step(stepWarn, "Server", "not answering: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), webWait)
	defer cancel()
	st, err := waitWebListening(ctx, c)
	if err != nil {
		r.step(stepWarn, "Server", "not answering: "+err.Error())
		return
	}
	if w := st.Web; w != nil {
		switch {
		case !w.Enabled:
			r.step(stepWarn, "Web UI", webOffNote)
		case w.Listening && w.Error != "":
			r.step(stepFail, "Web UI", w.Error)
		case w.Listening:
		case w.Error != "":
			r.step(stepFail, "Web UI", "not listening: "+w.Error)
		default:
			r.step(stepWarn, "Web UI", "starting (meko ui waits for it)")
		}
	}
	note := ""
	if o.verb != "" && !o.since.IsZero() {
		note = o.verb + " in " + time.Since(o.since).Round(time.Second).String()
	}
	r.running(fmt.Sprintf("Meko %s is running", st.Version), note)
	r.links(linkRows(st, cloudRow(l)))
	added := ""
	if o.pick != nil {
		added = o.pick(r)
	}
	s := readNextState(l, c)
	if st.Web != nil {
		s.webOff = !st.Web.Enabled
	}
	s.added = added
	r.next(nextSteps(s, o.kind))
}

// waitEngine prints the context engine's row: on a terminal it shows the dots while the engine
// loads, for at most engineWait, then says how it ended ("ready in 2.4s"), or that it's still
// loading. It fails only when the server doesn't answer.
func waitEngine(c *client, stdout io.Writer) error {
	began := time.Now()
	step := spin.Quiet(stdout, "Context engine", "loading")
	st, err := statusWithin(c, 5*time.Second)
	for err == nil && engineLoading(st.ContextEngine.State) && time.Since(began) < engineWait {
		time.Sleep(200 * time.Millisecond)
		st, err = statusWithin(c, 5*time.Second)
	}
	if err != nil {
		step.Clear()
		return err
	}
	color := spin.Color(stdout)
	switch e := st.ContextEngine; {
	case engineLoading(e.State):
		step.End(stepWarn, spin.Note("still loading", "meko status says when it's ready", color))
	case e.State == "ready" && time.Since(began) >= 100*time.Millisecond:
		step.Done("ready in " + seconds(time.Since(began)))
	default:
		step.End(contextEngineStep(e))
	}
	return nil
}

// statusWithin asks the server for its status, waiting at most d.
func statusWithin(c *client, d time.Duration) (server.StatusReply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return c.status(ctx)
}

// seconds is d to a tenth of a second: "2.4s".
func seconds(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()) }

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
