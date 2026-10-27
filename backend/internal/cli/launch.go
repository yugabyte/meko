package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/update"
)

// The environment meko _launch gives what it runs.
const (
	// launchedEnv tells an extension's launcher it runs under meko _launch, so it doesn't hand over
	// again.
	launchedEnv = "MEKO_LAUNCHED"
	// updatedFromEnv names the version a version on probation was updated from.
	updatedFromEnv = "MEKO_UPDATED_FROM"
)

// runLaunch is meko _launch <name> <exe> [args...], what an extension's launcher (its release's
// bin/<command>) hands its long-running commands to, and `meko _launch --supported`, which tells it
// this meko has it. exe is that launcher, in <app>/<version>/bin.
//
// For a command in the catalog's Probation (serve), it does for the extension what serve does for
// Meko: it counts a start of a version on probation (update-pending.json), rolls back to the old
// version after update.MaxFailedStarts failed ones, runs the version's launcher as its child (signals
// passed on, its exit code returned), finishes the update once the child has run
// update.HealthyAfter, and takes a clean end (exit 0, a CleanExits code, or a stop signal it passed
// on) off the count. Any other command it execs in its own place. Either way the version's folder is
// marked in use while the process runs (update.App.LockInUse), so pruning keeps it: a session host
// outlives later updates. A launcher outside a release install's app folder (a build from source) is
// just run.
func runLaunch(args []string, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--supported" {
		return exitOK
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, "meko _launch: usage: meko _launch <extension> <its launcher> [args...]")
		return exitUsage
	}
	name, exe, rest := args[0], args[1], args[2:]
	cat, ok := extensions.Known[name]
	if !ok {
		return fail(stderr, fmt.Errorf("_launch: no extension called %q", name))
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return fail(stderr, err)
	}
	env := append(os.Environ(), launchedEnv+"=1")
	_, version, isRelease := update.Of(real)
	if !isRelease {
		return execIn(real, rest, env, nil, stderr)
	}
	app := cat.AppIn(filepath.Dir(filepath.Dir(filepath.Dir(real))), name)
	probation := len(rest) > 0 && slices.Contains(cat.Probation, rest[0])
	if probation {
		p, serr := app.Started(version)
		if serr != nil {
			fmt.Fprintf(stderr, "meko: can't read %s's update record: %v\n", cat.Title, serr)
		}
		if p != nil {
			if rerr := app.RollBack(*p); rerr != nil {
				return fail(stderr, fmt.Errorf("%s %s failed to start %d times, and rolling back to %s failed: %w",
					cat.Title, p.To, update.MaxFailedStarts, p.From, rerr))
			}
			fmt.Fprintf(stderr, "meko: %s %s failed to start %d times; rolled back to %s, which runs instead\n",
				cat.Title, p.To, update.MaxFailedStarts, p.From)
			version = p.From
			real = app.Exe(version)
		} else if p, _ := app.ReadPending(); p != nil && p.To == version {
			env = append(env, updatedFromEnv+"="+p.From)
		}
	}
	inUse, err := app.LockInUse(version)
	if err != nil {
		fmt.Fprintf(stderr, "meko: can't mark %s %s in use: %v\n", cat.Title, version, err)
	}
	if !probation {
		return execIn(real, rest, env, inUse, stderr)
	}
	return superviseLaunch(cat, app, version, real, rest, env, inUse, stderr)
}

// execIn runs exe in this process's place, keeping the in-use lock's file open across the exec.
func execIn(exe string, args, env []string, inUse *os.File, stderr io.Writer) int {
	if inUse != nil {
		if _, err := unix.FcntlInt(inUse.Fd(), unix.F_SETFD, 0); err != nil {
			fmt.Fprintf(stderr, "meko: can't keep the in-use mark across exec: %v\n", err)
		}
	}
	//nolint:gosec // G204: the extension's own launcher, from its app folder
	err := syscall.Exec(exe, append([]string{exe}, args...), env)
	return fail(stderr, fmt.Errorf("can't run %s: %w", exe, err))
}

// superviseLaunch runs a command on probation as this process's child: see runLaunch.
func superviseLaunch(cat extensions.Catalog, app update.App, version, exe string, args, env []string,
	inUse *os.File, stderr io.Writer) int {
	cmd := exec.Command(exe, args...) //nolint:gosec // G204: the extension's own launcher, from its app folder
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdin, os.Stdout, os.Stderr, env
	if inUse != nil {
		cmd.ExtraFiles = []*os.File{inUse}
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		_ = app.Uncount(version) // it never ran: not a start of its own failing
		return fail(stderr, fmt.Errorf("can't run %s: %w", exe, err))
	}
	var stopping atomic.Bool
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				if s != syscall.SIGHUP {
					stopping.Store(true)
				}
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	healthy := time.AfterFunc(update.HealthyAfter, func() {
		if p, _ := app.ReadPending(); p == nil || p.To != version {
			return
		}
		pruned, err := app.Healthy(version)
		if err != nil {
			fmt.Fprintf(stderr, "meko: can't finish %s's update to %s: %v\n", cat.Title, version, err)
			return
		}
		fmt.Fprintf(stderr, "meko: %s %s has run %s, so its update is done", cat.Title, version, update.HealthyAfter)
		if len(pruned) > 0 {
			fmt.Fprintf(stderr, "; removed %v", pruned)
		}
		fmt.Fprintln(stderr)
	})
	err := cmd.Wait()
	healthy.Stop()
	close(done)
	code := exitOK
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
	case err != nil:
		code = 1
	}
	if code == 0 || stopping.Load() || slices.Contains(cat.CleanExits, code) {
		if err := app.Uncount(version); err != nil {
			fmt.Fprintf(stderr, "meko: can't update %s's update record: %v\n", cat.Title, err)
		}
	}
	return code
}
