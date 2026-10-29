package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/update"
)

// withAgentKData is installedAgentK with a file of AgentK's data in its folder, which is returned.
func withAgentKData(t *testing.T, dir string) (update.App, string) {
	t.Helper()
	app := installedAgentK(t, dir, "0.20261030.0")
	data := filepath.Join(extensions.Dir(dir, "agentk"), "state.db")
	if err := os.WriteFile(data, []byte("sessions"), 0o600); err != nil {
		t.Fatal(err)
	}
	return app, data
}

// extension delete asks first, then runs AgentK's own uninstall keeping its data, removes the release
// Meko installed and the command it linked, and forgets it; its data stays.
func TestExtensionDeleteKeepsTheData(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	app, data := withAgentKData(t, dir)
	home, _ := os.UserHomeDir()
	link := filepath.Join(home, ".local", "bin", "agentk")
	uninstalled := filepath.Join(dir, "agentk-uninstall.log")

	stdin = strings.NewReader("n\n")
	t.Cleanup(func() { stdin = os.Stdin })
	code, out, errs := run("extension", "delete", "agentk", "--data-dir", dir)
	want(t, "answered no", code, 1, errs, "Nothing was removed")
	want(t, "answered no", code, 1, out, "Deleting AgentK stops it (every session it runs ends)")
	want(t, "answered no", code, 1, out, "Its data in "+extensions.Dir(dir, "agentk"))
	want(t, "answered no", code, 1, out, "Delete it? [y/N]")
	if _, err := os.Stat(app.Root); err != nil || readLog(t, uninstalled) != "" {
		t.Errorf("a no removed something: %v, %q", err, readLog(t, uninstalled))
	}

	stdin = strings.NewReader("y\n")
	code, out, errs = run("extension", "delete", "agentk", "--data-dir", dir)
	want(t, "answered yes", code, 0, out+errs, "Deleted AgentK. Its data stays in "+extensions.Dir(dir, "agentk"))
	if got := readLog(t, uninstalled); got != "uninstall --non-interactive\n" {
		t.Errorf("AgentK's uninstall ran %q", got)
	}
	if _, err := os.Stat(app.Root); !os.IsNotExist(err) {
		t.Errorf("the releases are still there: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the command is still linked: %v", err)
	}
	if _, err := os.Stat(data); err != nil {
		t.Errorf("its data went: %v", err)
	}
	if reg, _ := extensions.Load(dir); len(reg.Entries) != 0 {
		t.Errorf("still registered: %v", reg.Entries)
	}
	code, _, errs = run("extension", "delete", "agentk", "--data-dir", dir)
	want(t, "deleted again", code, 1, errs, "AgentK isn't installed; its data stays in "+extensions.Dir(dir, "agentk"))
}

// --purge runs AgentK's uninstall that deletes its data, and removes its folder; --force doesn't ask.
func TestExtensionDeletePurge(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	withAgentKData(t, dir)
	stdin = strings.NewReader("")
	t.Cleanup(func() { stdin = os.Stdin })
	code, out, errs := run("extension", "delete", "agentk", "--purge", "--force", "--data-dir", dir)
	want(t, "--purge --force", code, 0, out+errs, "--purge also deletes its data in "+extensions.Dir(dir, "agentk"))
	want(t, "--purge --force", code, 0, out, "AgentK's sessions and worktrees")
	want(t, "--purge --force", code, 0, out, "Deleted AgentK\n")
	if strings.Contains(out, "Delete it?") {
		t.Errorf("--force asked:\n%s", out)
	}
	if got := readLog(t, filepath.Join(dir, "agentk-uninstall.log")); got !=
		strings.Join(extensions.Known["agentk"].UninstallArgs, " ")+"\n" {
		t.Errorf("AgentK's uninstall ran %q", got)
	}
	if _, err := os.Stat(extensions.Dir(dir, "agentk")); !os.IsNotExist(err) {
		t.Errorf("its folder is still there: %v", err)
	}
	code, _, errs = run("extension", "delete", "agentk", "--purge", "--force", "--data-dir", dir)
	want(t, "nothing left", code, 1, errs, "AgentK isn't installed\n")
}

// An uninstall that fails leaves everything; one that's running holds off; a damaged one has no
// uninstall to run, so a purge of it is refused, and a plain delete removes and forgets it.
func TestExtensionDeleteFailsSafely(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	app, data := withAgentKData(t, dir)
	stdin = strings.NewReader("")
	t.Cleanup(func() { stdin = os.Stdin })

	lock := update.NewLock(dir)
	if !lock.TryLock() {
		t.Fatal("lock")
	}
	code, _, errs := run("extension", "delete", "agentk", "--force", "--data-dir", dir)
	lock.Unlock()
	want(t, "another update running", code, 1, errs, "another update is running")

	broken := app.Exe(app.Current())
	//nolint:gosec // G306: a test's fake program
	if err := os.WriteFile(broken, []byte("#!/bin/sh\necho \"can't uninstall\" >&2\nexit 4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, errs = run("extension", "delete", "agentk", "--force", "--data-dir", dir)
	want(t, "a failing uninstall", code, 1, errs, "uninstalling AgentK failed")
	if reg, _ := extensions.Load(dir); len(reg.Entries) != 1 {
		t.Errorf("unregistered: %v", reg.Entries)
	}
	if _, err := os.Stat(app.Root); err != nil {
		t.Errorf("the releases went: %v", err)
	}

	// Damaged: the registry names a command that's gone, and nothing else is found.
	if err := os.RemoveAll(app.Root); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "agentk")
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: missing, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	code, _, errs = run("extension", "delete", "agentk", "--purge", "--force", "--data-dir", dir)
	want(t, "damaged, --purge", code, 1, errs, "so its own uninstall can't run, and a service it installed may "+
		"still use "+extensions.Dir(dir, "agentk")+". Nothing was removed")
	if _, err := os.Stat(data); err != nil {
		t.Errorf("a refused purge deleted the data: %v", err)
	}
	code, out, errs := run("extension", "delete", "agentk", "--force", "--data-dir", dir)
	want(t, "damaged", code, 0, out, "AgentK is damaged ("+missing+" is missing), so its own uninstall can't run")
	want(t, "damaged", code, 0, errs, "so its own uninstall can't run; any service it installed stays")
	want(t, "damaged", code, 0, out, "Deleted AgentK.")
	if reg, _ = extensions.Load(dir); len(reg.Entries) != 0 {
		t.Errorf("still registered: %v", reg.Entries)
	}
	code, out, _ = run("stop", "--data-dir", dir, "--keep-extensions")
	if strings.Contains(out, "damaged") {
		t.Errorf("stop still runs it: exit %d\n%s", code, out)
	}
}

// A release a live process still runs from (AgentK's session host, when its stop failed) is kept,
// with the registration, and delete says what still runs.
func TestExtensionDeleteKeepsAReleaseInUse(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	app, _ := withAgentKData(t, dir)
	f, err := app.LockInUse(app.Current())
	if err != nil {
		t.Fatal(err)
	}
	code, _, errs := run("extension", "delete", "agentk", "--force", "--data-dir", dir)
	want(t, "in use", code, 1, errs, "AgentK's uninstall ran, but something still runs from AgentK "+app.Current())
	if _, serr := os.Stat(app.Root); serr != nil {
		t.Errorf("the releases went: %v", serr)
	}
	if reg, _ := extensions.Load(dir); len(reg.Entries) != 1 {
		t.Errorf("unregistered: %v", reg.Entries)
	}
	_ = f.Close()
	code, out, errs := run("extension", "delete", "agentk", "--force", "--data-dir", dir)
	want(t, "once it stopped", code, 0, out+errs, "Deleted AgentK")
}

// A registered extension Meko doesn't know is only forgotten: nothing is run or removed.
func TestExtensionDeleteForgetsAnUnknownOne(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	cmd := filepath.Join(t.TempDir(), "frob")
	log := fakeExtension(t, cmd)
	reg, _ := extensions.Load(dir)
	reg.Entries["frob"] = extensions.Entry{Command: cmd}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("extension", "delete", "frob", "--force", "--data-dir", dir)
	want(t, "unknown", code, 0, out+errs, "Meko doesn't know how to uninstall frob, so it stays as it is")
	want(t, "unknown", code, 0, out, "Deleted frob\n")
	if readLog(t, log) != "" {
		t.Errorf("it ran %q", readLog(t, log))
	}
	if reg, _ = extensions.Load(dir); len(reg.Entries) != 0 {
		t.Errorf("still registered: %v", reg.Entries)
	}
}
