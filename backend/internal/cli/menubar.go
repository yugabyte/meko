package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/service"
	"github.com/yugabyte/meko/backend/internal/update"
)

// menuBarJob is the menu bar app's LaunchAgent (service.Launchd with MenuBarLabel).
type menuBarJob interface {
	StartMenuBar(service.MenuBarSpec) error
	Installed() bool
	Remove() error
}

// newMenuBar is the menu bar app's job for the user, or service.ErrNoMenuBar off macOS; tests replace it.
var newMenuBar = func() (menuBarJob, error) {
	if runtime.GOOS != "darwin" {
		return nil, service.ErrNoMenuBar
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return service.NewMenuBar(home, service.Exec), nil
}

// menuBarExe is the meko binary whose Meko.app the job runs: a release's app/current/bin/meko, so
// the job runs each new release's app after an update without being written again, or else this
// binary (a build from source). Tests replace it.
var menuBarExe = func() (string, error) {
	if app, _, ok := update.Running(); ok {
		return app.CurrentExe(), nil
	}
	return os.Executable()
}

// runMenuBar is meko menubar start (start the app now and at each login) and stop (quit it, and
// don't start it at login).
func runMenuBar(l paths.Layout, start bool, stdout, stderr io.Writer) int {
	job, err := newMenuBar()
	if err != nil {
		return fail(stderr, err)
	}
	if !start {
		if err = job.Remove(); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintln(stdout, "Stopped the menu bar app")
		return exitOK
	}
	exe, err := menuBarExe()
	if err != nil {
		return fail(stderr, err)
	}
	app := service.MenuBarAppFor(exe)
	if _, err = os.Stat(filepath.Join(app, "Contents", "MacOS", service.MenuBarProgram)); err != nil {
		return fail(stderr, fmt.Errorf("this Meko has no menu bar app (%s): a build from source makes it with make menubar",
			service.MenuBarApp))
	}
	// launchd would make the log 0644; files in the data folder are the owner's alone.
	log := filepath.Join(l.LogDir, "menubar.log")
	if err = os.MkdirAll(l.LogDir, 0o700); err != nil {
		return fail(stderr, err)
	}
	f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: the data folder's log
	if err != nil {
		return fail(stderr, err)
	}
	_ = f.Close()
	err = job.StartMenuBar(service.MenuBarSpec{App: app, Meko: exe, DataDir: l.Root, Log: log})
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "Started the menu bar app, and it starts at login too")
	return exitOK
}
