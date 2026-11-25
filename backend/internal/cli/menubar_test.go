package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/service"
)

// fakeMenuBar is the menu bar app's job, recording what it's asked.
type fakeMenuBar struct {
	installed bool
	started   []service.MenuBarSpec
	removed   int
}

func (f *fakeMenuBar) StartMenuBar(s service.MenuBarSpec) error {
	f.started, f.installed = append(f.started, s), true
	return nil
}
func (f *fakeMenuBar) Installed() bool { return f.installed }
func (f *fakeMenuBar) Remove() error {
	f.removed++
	f.installed = false
	return nil
}

// useFakeMenuBar replaces the menu bar app's job, and the meko binary it goes with (exe), for the test.
func useFakeMenuBar(t *testing.T, exe string) *fakeMenuBar {
	t.Helper()
	f := &fakeMenuBar{}
	oldJob, oldExe := newMenuBar, menuBarExe
	newMenuBar = func() (menuBarJob, error) { return f, nil }
	menuBarExe = func() (string, error) { return exe, nil }
	t.Cleanup(func() { newMenuBar, menuBarExe = oldJob, oldExe })
	return f
}

// menuBarBundle makes an empty Meko.app beside exe.
func menuBarBundle(t *testing.T, exe string) string {
	t.Helper()
	app := service.MenuBarAppFor(exe)
	prog := filepath.Join(app, "Contents", "MacOS", service.MenuBarProgram)
	if err := os.MkdirAll(filepath.Dir(prog), 0o755); err != nil { //nolint:gosec // G301: a test bundle
		t.Fatal(err)
	}
	if err := os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // G306: a program
		t.Fatal(err)
	}
	return app
}

// meko menubar start runs the Meko.app beside the meko binary, at this data folder, and says it
// starts at login; without the bundle it says how to get one. stop removes the job.
func TestMenuBar(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	exe := filepath.Join(t.TempDir(), "bin", "meko")
	f := useFakeMenuBar(t, exe)

	code, out, errs := run("menubar", "start", "--data-dir", dir)
	want(t, "start without the bundle", code, exitError, errs,
		"no menu bar app (Meko.app): a build from source makes it with make menubar")
	if len(f.started) != 0 || out != "" {
		t.Fatalf("started without a bundle: %v %q", f.started, out)
	}

	app := menuBarBundle(t, exe)
	code, out, errs = run("menubar", "start", "--data-dir", dir)
	want(t, "start", code, exitOK, out+errs, "Started the menu bar app, and it starts at login too\n")
	if len(f.started) != 1 {
		t.Fatalf("started %v", f.started)
	}
	got := f.started[0]
	if got.App != app || got.Meko != exe || !paths.SameDir(got.DataDir, dir) ||
		got.Log != filepath.Join(got.DataDir, "logs", "menubar.log") {
		t.Errorf("spec %+v", got)
	}
	if st, err := os.Stat(got.Log); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("the log isn't the owner's alone: %v %v", st, err)
	}

	code, out, errs = run("menubar", "stop", "--data-dir", dir)
	want(t, "stop", code, exitOK, out+errs, "Stopped the menu bar app\n")
	if f.removed != 1 {
		t.Errorf("removed %d times", f.removed)
	}

	code, _, errs = run("menubar")
	want(t, "menubar alone", code, exitUsage, errs, "")
}

// Off macOS there's no menu bar app, and the commands say so.
func TestMenuBarOffMacOS(t *testing.T) {
	old := newMenuBar
	newMenuBar = func() (menuBarJob, error) { return nil, service.ErrNoMenuBar }
	t.Cleanup(func() { newMenuBar = old })
	code, _, errs := run("menubar", "start", "--data-dir", t.TempDir())
	want(t, "start", code, exitError, errs, "meko: the menu bar app runs only on macOS\n")
}
