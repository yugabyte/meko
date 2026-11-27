package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/service"
	"github.com/yugabyte/meko/src/meko/internal/spin"
	"github.com/yugabyte/meko/src/meko/internal/update"
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

// errNoMenuBarApp means there's no Meko.app beside meko: a build from source without make menubar.
var errNoMenuBarApp = fmt.Errorf("this Meko has no menu bar app (%s): a build from source makes it with make menubar",
	service.MenuBarApp)

// menuBarOff is the file that says the user turned the menu bar app off (meko menubar stop, or
// meko install --no-menubar), so meko install, which starts it on macOS, leaves it off.
func menuBarOff(l paths.Layout) string { return filepath.Join(l.Root, "menubar-off") }

// startMenuBar writes and loads the menu bar app's job for the meko binary exe and starts the app.
func startMenuBar(l paths.Layout, job menuBarJob, exe string) error {
	app := service.MenuBarAppFor(exe)
	if _, err := os.Stat(filepath.Join(app, "Contents", "MacOS", service.MenuBarProgram)); err != nil {
		return errNoMenuBarApp
	}
	// launchd would make the log 0644; files in the data folder are the owner's alone.
	log := filepath.Join(l.LogDir, "menubar.log")
	if err := os.MkdirAll(l.LogDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: the data folder's log
	if err != nil {
		return err
	}
	_ = f.Close()
	return job.StartMenuBar(service.MenuBarSpec{App: app, Meko: exe, DataDir: l.Root, Log: log})
}

// setMenuBarOff records whether the user turned the menu bar app off.
func setMenuBarOff(l paths.Layout, off bool) error {
	if !off {
		return paths.RemoveIfExists(menuBarOff(l))
	}
	if err := os.MkdirAll(l.Root, 0o700); err != nil {
		return err
	}
	return paths.WriteFile(menuBarOff(l), nil)
}

// runMenuBar is meko menubar start (start the app now and at each login) and stop (quit it, don't
// start it at login, and keep meko install from starting it again).
func runMenuBar(l paths.Layout, start bool, stdout, stderr io.Writer) int {
	job, err := newMenuBar()
	if err != nil {
		return fail(stderr, err)
	}
	if !start {
		if err = job.Remove(); err != nil {
			return fail(stderr, err)
		}
		if err = setMenuBarOff(l, true); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintln(stdout, "Stopped the menu bar app")
		return exitOK
	}
	exe, err := menuBarExe()
	if err != nil {
		return fail(stderr, err)
	}
	if err = startMenuBar(l, job, exe); err != nil {
		return fail(stderr, err)
	}
	if err = setMenuBarOff(l, false); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "Started the menu bar app, and it starts at login too")
	return exitOK
}

// installMenuBar is meko install's Menu bar step, on macOS with the service: it starts the menu bar
// app at each login, unless the user turned it off (--no-menubar, which records that, or an earlier
// meko menubar stop). A failure is a warning: Meko itself is installed and running.
func installMenuBar(l paths.Layout, r report, exe string, noMenuBar bool) {
	job, err := newMenuBar()
	if err != nil {
		return // not macOS
	}
	if noMenuBar {
		if err = setMenuBarOff(l, true); err == nil && job.Installed() {
			err = job.Remove()
		}
		if err != nil {
			r.step(stepWarn, "Menu bar", "couldn't turn it off: "+err.Error())
		}
		return
	}
	if _, serr := os.Stat(menuBarOff(l)); serr == nil {
		return // turned off: meko menubar start turns it on
	}
	switch err = startMenuBar(l, job, exe); {
	case errors.Is(err, errNoMenuBarApp):
		// A build from source without make menubar.
	case err != nil:
		r.step(stepWarn, "Menu bar", err.Error()+". Try again with meko menubar start")
	default:
		r.step(stepOK, "Menu bar", "the meko m, at each login"+spin.Dim(" · quit it with ", r.color)+
			spin.Blue("meko menubar stop", r.color))
	}
}
