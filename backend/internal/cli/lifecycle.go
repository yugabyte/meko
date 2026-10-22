package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	if err != nil || !m.Installed() || m.DataDir() != l.Root {
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

// runStart starts Meko (through its service when it has one), then the extensions.
func runStart(l paths.Layout, o startOptions, stdout, stderr io.Writer) int {
	code := startMeko(l, o, stdout, stderr)
	if code == exitOK {
		runExtensions(l, "start", stdout, stderr)
	}
	return code
}

func startMeko(l paths.Layout, o startOptions, stdout, stderr io.Writer) int {
	m := serviceOf(l)
	if m == nil {
		return startDetached(l, o, "", stdout, stderr)
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
	fmt.Fprintf(stdout, "Meko is running at http://127.0.0.1:%d (pid %d, data in %s, run by %s).\n",
		st.Port, st.PID, l.Root, m.Kind())
	return exitOK
}

// runStop stops the extensions (unless keepExtensions), then Meko: through its service when it
// has one, since a service manager would start a server that only gets a signal again.
func runStop(l paths.Layout, keepExtensions bool, stdout, stderr io.Writer) int {
	if !keepExtensions {
		runExtensions(l, "stop", stdout, stderr)
	}
	if m := serviceOf(l); m != nil {
		if err := m.Stop(); err != nil {
			fmt.Fprintf(stderr, "meko: %v\n", err)
		}
		if up, _ := running(l.Lock); !up {
			cleanRunFiles(l)
			fmt.Fprintln(stdout, "Meko stopped.")
			return exitOK
		}
		// Still running: a server started by hand on the same folder. Stop it as below.
	}
	return stopMeko(l, stdout, stderr)
}

// runRestart restarts Meko alone, never the extensions.
func runRestart(l paths.Layout, o startOptions, stdout, stderr io.Writer) int {
	pid := currentPID(l)
	if m := serviceOf(l); m != nil {
		if err := m.Restart(); err != nil {
			return fail(stderr, err)
		}
		st, ok := waitAnswering(l, serviceStartTimeout, pid)
		if !ok {
			return fail(stderr, fmt.Errorf("the %s service didn't answer within %s. Check: %s, and meko logs",
				m.Kind(), serviceStartTimeout, m.StatusHint()))
		}
		fmt.Fprintf(stdout, "Meko restarted at http://127.0.0.1:%d (pid %d).\n", st.Port, st.PID)
		return exitOK
	}
	if code := stopMeko(l, stdout, stderr); code != exitOK {
		return code
	}
	return startDetached(l, o, "", stdout, stderr)
}

// runExtensions runs `<command> <action>` for every registered extension. A failure is reported
// and the others still run: stopping Meko must not wait on an extension that won't stop.
func runExtensions(l paths.Layout, action string, stdout, stderr io.Writer) {
	reg, err := extensions.Load(l.Root)
	if err != nil {
		fmt.Fprintf(stderr, "meko: can't read the extensions: %v\n", err)
		return
	}
	for _, name := range reg.Names() {
		e := reg.Entries[name]
		if _, err := os.Stat(e.Command); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "meko: extension %s is gone (%s); meko uninstall_extension %s forgets it\n",
				name, e.Command, name)
			continue
		}
		if err := e.Run(action, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "meko: extension %s didn't %s: %v\n", name, action, err)
		}
	}
}
