package extensions

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
