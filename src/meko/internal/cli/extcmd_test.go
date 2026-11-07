package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/extensions"
)

// meko kode runs AgentK's registered program with everything after it, MEKO_HOME naming the data
// folder (a leading --data-dir is Meko's), and exits as it does; meko kode --help is AgentK's own.
// An extension that isn't installed says how to install it.
func TestExtensionCommandRunsItsProgram(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	code, _, errs := run("present", "--data-dir", dir, "status")
	want(t, "present, not installed", code, exitError, errs,
		"Presenter isn't installed: add it with meko extension install present")

	prog := filepath.Join(t.TempDir(), "me_kode")
	//nolint:gosec // G306: a test's fake program
	if err := os.WriteFile(prog, []byte("#!/bin/sh\necho \"args: $*\"\necho \"home: $MEKO_HOME\" >&2\nexit 3\n"),
		0o755); err != nil {
		t.Fatal(err)
	}
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: prog, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("kode", "--data-dir", dir, "logs", "-f", "--help")
	if code != 3 || strings.TrimSpace(out) != "args: logs -f --help" || strings.TrimSpace(errs) != "home: "+dir {
		t.Errorf("meko kode logs -f --help: exit %d, output %q, errors %q", code, out, errs)
	}
	t.Setenv("MEKO_HOME", dir)
	code, out, _ = run("kode", "--help")
	if code != 3 || strings.TrimSpace(out) != "args: --help" {
		t.Errorf("meko kode --help: exit %d, output %q", code, out)
	}
	if err := os.Remove(prog); err != nil {
		t.Fatal(err)
	}
	code, _, errs = run("kode", "status")
	want(t, "kode, its program gone", code, exitError, errs, "AgentK is damaged")
}
