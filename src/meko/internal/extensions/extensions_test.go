package extensions

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/update"
)

func TestRegistry(t *testing.T) {
	root := t.TempDir()
	r, err := Load(root)
	if err != nil || len(r.Entries) != 0 {
		t.Fatalf("empty registry: %v %v", r, err)
	}
	r.Entries["agentk"] = Entry{Command: "/bin/agent-k-local", InstalledByMeko: true}
	if err = r.Save(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(File(root)); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	again, err := Load(root)
	if err != nil || again.Entries["agentk"].Command != "/bin/agent-k-local" || !again.Entries["agentk"].InstalledByMeko {
		t.Fatalf("reloaded %+v, %v", again, err)
	}
	if strings.Join(again.Names(), ",") != "agentk" {
		t.Errorf("names %v", again.Names())
	}
}

// Every command Meko runs of an extension gets the action and MEKO_HOME naming the data folder.
func TestRunPassesTheActionAndTheDataFolderAndReportsFailure(t *testing.T) {
	dir := t.TempDir()
	cmd := filepath.Join(dir, "ext")
	script := "#!/bin/sh\necho \"ext $* $MEKO_HOME\"\n[ \"$1\" = stop ] && exit 3\nexit 0\n"
	if err := os.WriteFile(cmd, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	t.Setenv("MEKO_HOME", "/elsewhere")
	e := Entry{Command: cmd}
	var out bytes.Buffer
	if err := e.Run("/data/meko", "start", &out, &out); err != nil || out.String() != "ext start /data/meko\n" {
		t.Fatalf("start: %v %q", err, out.String())
	}
	if err := e.Run("/data/meko", "stop", &out, &out); err == nil {
		t.Fatal("a failed stop wasn't reported")
	}
	if ok, line := e.Status("/data/meko"); !ok || line != "ext status /data/meko" {
		t.Errorf("status %v %q", ok, line)
	}
	out.Reset()
	if err := e.Uninstall("/data/meko", Known["agentk"].UninstallArgs, &out, &out); err != nil ||
		out.String() != "ext uninstall --purge --delete-worktrees --non-interactive /data/meko\n" {
		t.Errorf("uninstall: %v %q", err, out.String())
	}
}

func TestFindCommandLooksInLocalBin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	if got := FindCommand("agent-k-local", home); got != "" {
		t.Fatalf("found %q", got)
	}
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	ak := filepath.Join(bin, "agent-k-local")
	if err := os.WriteFile(ak, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: test
		t.Fatal(err)
	}
	if got := FindCommand("agent-k-local", home); got != filepath.Join(bin, "agent-k-local") {
		t.Fatalf("found %q", got)
	}
}

func TestCatalogFindsTheCommandByAnOldName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	put := func(name string) {
		script := []byte("#!/bin/sh\n")
		if err := os.WriteFile(filepath.Join(bin, name), script, 0o700); err != nil { //nolint:gosec // G306: test
			t.Fatal(err)
		}
	}
	ak := Known["agentk"]
	// An AgentK from before the rename has only agent-k-local; a newer one has agentk too.
	put("agent-k-local")
	if got := ak.Find("agentk", home, t.TempDir()); got != filepath.Join(bin, "agent-k-local") {
		t.Fatalf("found %q", got)
	}
	put("agentk")
	if got := ak.Find("agentk", home, t.TempDir()); got != filepath.Join(bin, "agentk") {
		t.Fatalf("found %q", got)
	}
}

// Without the command on PATH or in ~/.local/bin, Find looks in its release install: its folder in
// the data folder, then its home from before the move there (with the command's old name too).
func TestCatalogFindsTheCommandInItsHome(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("PATH", t.TempDir())
	ak := Known["agentk"]
	put := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: test
			t.Fatal(err)
		}
	}
	if got := ak.Find("agentk", home, root); got != "" {
		t.Fatalf("found %q in empty folders", got)
	}
	old := filepath.Join(home, ".agent-k-local", "app", "current", "bin", "agent-k-local")
	put(old)
	if got := ak.Find("agentk", home, root); got != old {
		t.Fatalf("found %q, want the old home's %q", got, old)
	}
	moved := filepath.Join(Dir(root, "agentk"), "app", "current", "bin", "agentk")
	put(moved)
	if got := ak.Find("agentk", home, root); got != moved {
		t.Fatalf("found %q, want the data folder's %q", got, moved)
	}
}

// A ~/.local/bin link an older Meko made into a release serves all of a user's Meko data folders,
// but a release belongs to one: Find passes the link over, and each data folder finds its own
// release's program, or none.
func TestCatalogFindSkipsLinksIntoReleases(t *testing.T) {
	home, a, b := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PATH", t.TempDir())
	pr := Known["present"]
	release := func(root string) update.App {
		app := pr.App(root, "present")
		const v = "0.20261024.0"
		if err := os.MkdirAll(filepath.Join(app.Dir(v), "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(app.Exe(v), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: test
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(app.Dir(v), "VERSION"), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := app.Repoint(v); err != nil {
			t.Fatal(err)
		}
		return app
	}
	appA := release(a)
	link, err := appA.LinkCommand(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := pr.Find("present", home, a); got != appA.CurrentExe() {
		t.Errorf("A found %q, want its own %q", got, appA.CurrentExe())
	}
	if got := pr.Find("present", home, b); got != "" {
		t.Errorf("B found %q, A's release", got)
	}
	appB := release(b)
	if got := pr.Find("present", home, b); got != appB.CurrentExe() {
		t.Errorf("B found %q, want its own %q", got, appB.CurrentExe())
	}
	if pr.Release(b, "present", link) != "" || pr.Release(a, "present", link) != "0.20261024.0" {
		t.Error("Release judged the link by the wrong data folder")
	}
}

// An AgentK installed while its command was agentk moves onto me_kode, with no command on PATH: its
// version gets the new name, the registry runs app/current/bin/me_kode, and the ~/.local/bin links
// of its old names into this data folder go. An old name's link into another data folder's release
// stays.
func TestRenameMovesAnInstallOntoItsNewCommand(t *testing.T) {
	const v = "0.20261030.0"
	home, root, other := t.TempDir(), t.TempDir(), t.TempDir()
	ak := Known["agentk"]
	oldRelease := func(root string) update.App {
		app := ak.App(root, "agentk")
		bin := filepath.Join(app.Dir(v), "bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		//nolint:gosec // G306: a test's program
		if err := os.WriteFile(filepath.Join(bin, "agentk"), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("agentk", filepath.Join(bin, "agent-k-local")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(app.Dir(v), "VERSION"), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(v, filepath.Join(app.Root, "current")); err != nil {
			t.Fatal(err)
		}
		return app
	}
	app := oldRelease(root)
	theirs := oldRelease(other)
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"agentk":        filepath.Join(app.Root, "current", "bin", "agentk"),
		"agent-k-local": filepath.Join(theirs.Root, "current", "bin", "agent-k-local"),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := Load(root)
	r.Entries["agentk"] = Entry{Command: filepath.Join(bin, "agentk"), InstalledByMeko: true}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if err := r.Rename(root, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(bin, "me_kode")); !os.IsNotExist(err) {
		t.Errorf("~/.local/bin/me_kode was made: %v", err)
	}
	if _, err := os.Stat(app.CurrentExe()); err != nil {
		t.Errorf("app/current/bin/me_kode doesn't run: %v", err)
	}
	if again, _ := Load(root); again.Entries["agentk"].Command != app.CurrentExe() ||
		!again.Entries["agentk"].InstalledByMeko {
		t.Errorf("registry %+v", again.Entries["agentk"])
	}
	if _, err := os.Lstat(filepath.Join(bin, "agentk")); !os.IsNotExist(err) {
		t.Errorf("~/.local/bin/agentk is left: %v", err)
	}
	if target, _ := os.Readlink(filepath.Join(bin, "agent-k-local")); target != links["agent-k-local"] {
		t.Errorf("another data folder's link now points at %q", target)
	}
	// Run again (every start), nothing changes.
	if err := r.Rename(root, home); err != nil {
		t.Fatal(err)
	}
	if again, _ := Load(root); again.Entries["agentk"].Command != app.CurrentExe() {
		t.Errorf("registry after a second run %+v", again.Entries["agentk"])
	}
}

// An AgentK installed while its command was me_koder moves onto me_kode the same way: its version,
// whose bin/ has me_koder with agentk and agent-k-local linking to it, gets bin/me_kode, the
// registry runs that, and ~/.local/bin/me_koder goes.
func TestRenameMovesAMeKoderInstallOntoMeKode(t *testing.T) {
	const v = "0.20261105.0"
	home, root := t.TempDir(), t.TempDir()
	app := Known["agentk"].App(root, "agentk")
	bin := filepath.Join(app.Dir(v), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: a test's program
	if err := os.WriteFile(filepath.Join(bin, "me_koder"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"agentk", "agent-k-local"} {
		if err := os.Symlink("me_koder", filepath.Join(bin, n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(app.Dir(v), "VERSION"), []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(v, filepath.Join(app.Root, "current")); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(local, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(local, "me_koder")
	if err := os.Symlink(filepath.Join(app.Root, "current", "bin", "me_koder"), old); err != nil {
		t.Fatal(err)
	}
	r, _ := Load(root)
	r.Entries["agentk"] = Entry{Command: old, InstalledByMeko: true}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if err := r.Rename(root, home); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(bin, "me_kode")); err != nil || target != "me_koder" {
		t.Errorf("bin/me_kode -> %q (%v)", target, err)
	}
	if _, err := os.Stat(app.CurrentExe()); err != nil {
		t.Errorf("app/current/bin/me_kode doesn't run: %v", err)
	}
	if again, _ := Load(root); again.Entries["agentk"].Command != app.CurrentExe() {
		t.Errorf("registry %+v", again.Entries["agentk"])
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Errorf("~/.local/bin/me_koder is left: %v", err)
	}
}

// A command that leaves a process running, holding its output pipe, still returns once it exits:
// its output is read for PipeWait more, then the pipe is closed and the clean exit stands.
func TestRunReturnsWhenAChildHoldsTheOutput(t *testing.T) {
	cmd := filepath.Join(t.TempDir(), "ext")
	script := "#!/bin/sh\necho started\nsleep 5 &\nexit 0\n"
	if err := os.WriteFile(cmd, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	old := PipeWait
	PipeWait = 200 * time.Millisecond
	t.Cleanup(func() { PipeWait = old })
	var out bytes.Buffer
	begun := time.Now()
	if err := (Entry{Command: cmd}).Run(t.TempDir(), "start", &out, &out); err != nil || out.String() != "started\n" {
		t.Fatalf("start: %v %q", err, out.String())
	}
	if took := time.Since(begun); took > 3*time.Second {
		t.Errorf("took %s: the child's pipe held it", took)
	}
}

// writeScript writes an executable shell script named name into dir and returns its path.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	return p
}

// A status's exit code says whether the extension runs; its first line comes back either way.
func TestStatusWithinReportsRunningAndStopped(t *testing.T) {
	dir := t.TempDir()
	up := Entry{Command: writeScript(t, dir, "up", "echo 'Up 1.0'\necho more\n")}
	if r := up.StatusWithin(context.Background(), dir, time.Second); !r.Running || r.TimedOut || r.Line != "Up 1.0" {
		t.Errorf("running: %+v", r)
	}
	down := Entry{Command: writeScript(t, dir, "down", "echo 'Down 1.0'\nexit 3\n")}
	if r := down.StatusWithin(context.Background(), dir, time.Second); r.Running || r.TimedOut || r.Line != "Down 1.0" {
		t.Errorf("stopped: %+v", r)
	}
}

// A status runs with SummaryEnv set, which asks the extension for its headline alone.
func TestStatusWithinAsksForTheHeadline(t *testing.T) {
	dir := t.TempDir()
	e := Entry{Command: writeScript(t, dir, "ext", "echo \"summary=$MEKO_SUMMARY\"\n")}
	if r := e.StatusWithin(context.Background(), dir, time.Second); r.Line != "summary=1" {
		t.Errorf("status's env: %+v", r)
	}
}

// A status that doesn't answer in time is reported as such, without waiting for the process it
// started that still holds its output (the timeout kills the status, not what it started).
func TestStatusWithinTimesOutPastAHungChild(t *testing.T) {
	dir := t.TempDir()
	e := Entry{Command: writeScript(t, dir, "hung", "echo 'Hung 1.0'\nsleep 30\n")}
	begun := time.Now()
	r := e.StatusWithin(context.Background(), dir, 300*time.Millisecond)
	if !r.TimedOut || r.Running || r.Line != "Hung 1.0" {
		t.Errorf("hung: %+v", r)
	}
	if took := time.Since(begun); took > 300*time.Millisecond+statusPipeWait+time.Second {
		t.Errorf("took %s", took)
	}
}

// A status that answers and exits while a process it started still holds its output is running,
// and comes back once statusPipeWait is up.
func TestStatusWithinReturnsWhenAChildHoldsTheOutput(t *testing.T) {
	dir := t.TempDir()
	e := Entry{Command: writeScript(t, dir, "child", "echo 'Child 1.0'\nsleep 30 &\nexit 0\n")}
	begun := time.Now()
	if r := e.StatusWithin(context.Background(), dir, 10*time.Second); !r.Running || r.TimedOut || r.Line != "Child 1.0" {
		t.Errorf("child: %+v", r)
	}
	if took := time.Since(begun); took > statusPipeWait+2*time.Second {
		t.Errorf("took %s: the child's pipe held it", took)
	}
}

// A status that exits by itself just before its deadline, while a process it started holds its
// output past the deadline, is still what its exit code says: it wasn't killed.
func TestStatusWithinTrustsAnExitNearTheDeadline(t *testing.T) {
	dir := t.TempDir()
	e := Entry{Command: writeScript(t, dir, "late", "echo 'Late 1.0'\nsleep 30 &\nsleep 0.3\nexit 0\n")}
	r := e.StatusWithin(context.Background(), dir, 500*time.Millisecond)
	if !r.Running || r.TimedOut || r.Line != "Late 1.0" {
		t.Errorf("late: %+v", r)
	}
}

// Canceling Statuses' context stops the checks still running.
func TestStatusesStopWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{Entries: map[string]Entry{"hung": {Command: writeScript(t, dir, "hung", "exec sleep 30\n")}}}
	ctx, cancel := context.WithCancel(context.Background())
	results := r.Statuses(ctx, dir, time.Minute)
	begun := time.Now()
	cancel()
	if got := results()["hung"]; got.Running {
		t.Errorf("hung: %+v", got)
	}
	if took := time.Since(begun); took > statusPipeWait+2*time.Second {
		t.Errorf("took %s after the cancel", took)
	}
}

// Statuses runs every extension's status at once, and gives each result under its own name.
func TestStatusesRunAtOnce(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{Entries: map[string]Entry{}}
	for _, n := range []string{"a", "b", "c"} {
		r.Entries[n] = Entry{Command: writeScript(t, dir, n, "sleep 1\necho "+n+"\n[ "+n+" = b ] && exit 3\nexit 0\n")}
	}
	begun := time.Now()
	got := r.Statuses(context.Background(), dir, 5*time.Second)()
	if took := time.Since(begun); took > 2500*time.Millisecond {
		t.Errorf("took %s: they ran one after another", took)
	}
	want := map[string]StatusResult{"a": {Running: true, Line: "a"}, "b": {Line: "b"}, "c": {Running: true, Line: "c"}}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("%s: %+v, want %+v", n, got[n], w)
		}
	}
}

// A command npm installed is a `#!/usr/bin/env node` script beside the Node that installed it,
// which the login service's PATH lacks: Meko runs it with that folder first on PATH.
func TestCommandsFindTheNodeTheyWereInstalledWith(t *testing.T) {
	bin := t.TempDir()
	node := "#!/bin/sh\n# A stand-in for Node: answers the handoff the script asks for.\n" +
		"[ \"$2\" = _handoff ] && echo '{\"socket\":\"/run/ext.sock\",\"legacy\":null}'\n"
	nodePath := filepath.Join(bin, "node")
	if err := os.WriteFile(nodePath, []byte(node), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	cmd := filepath.Join(bin, "ext")
	if err := os.WriteFile(cmd, []byte("#!/usr/bin/env node\n"), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // no node anywhere on it
	h, err := Entry{Command: cmd}.Handoff(t.TempDir())
	if err != nil || h.Socket != "/run/ext.sock" {
		t.Fatalf("handoff %+v, %v", h, err)
	}
}

// AgentK's own [update] channel = "off" keeps Update automatically off until it's set; set, the
// switch is what counts.
func TestAutoUpdateReadsTheOldSwitch(t *testing.T) {
	root := t.TempDir()
	cat := Known["agentk"]
	if !cat.AutoUpdate(root, "agentk", Entry{}) {
		t.Error("off with no config")
	}
	if err := os.MkdirAll(Dir(root, "agentk"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(root, "agentk"), "config.toml"),
		[]byte("[update]\nchannel = \"off\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cat.AutoUpdate(root, "agentk", Entry{}) {
		t.Error("on with the old switch off")
	}
	on := true
	if !cat.AutoUpdate(root, "agentk", Entry{AutoUpdate: &on}) {
		t.Error("the switch, once set, didn't count")
	}
	if !Known["present"].AutoUpdate(root, "present", Entry{}) {
		t.Error("present off")
	}
}
