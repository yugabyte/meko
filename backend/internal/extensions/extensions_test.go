package extensions

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestRunPassesTheActionAndReportsFailure(t *testing.T) {
	dir := t.TempDir()
	cmd := filepath.Join(dir, "ext")
	script := "#!/bin/sh\necho \"ext $1\"\n[ \"$1\" = stop ] && exit 3\nexit 0\n"
	if err := os.WriteFile(cmd, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	e := Entry{Command: cmd}
	var out bytes.Buffer
	if err := e.Run("start", &out, &out); err != nil || out.String() != "ext start\n" {
		t.Fatalf("start: %v %q", err, out.String())
	}
	if err := e.Run("stop", &out, &out); err == nil {
		t.Fatal("a failed stop wasn't reported")
	}
	if ok, line := e.Status(); !ok || line != "ext status" {
		t.Errorf("status %v %q", ok, line)
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
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: test
			t.Fatal(err)
		}
	}
	ak := Known["agentk"]
	// An AgentK from before the rename has only agent-k-local; a newer one has agentk too.
	put("agent-k-local")
	if got := ak.Find(home); got != filepath.Join(bin, "agent-k-local") {
		t.Fatalf("found %q", got)
	}
	put("agentk")
	if got := ak.Find(home); got != filepath.Join(bin, "agentk") {
		t.Fatalf("found %q", got)
	}
}
