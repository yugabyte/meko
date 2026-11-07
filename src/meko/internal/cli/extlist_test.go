package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/extensions"
)

// extension list, with no server, shows every known extension: not installed until it is, then its
// state from its status command, the release Meko installed and whether Meko updates it on its own.
func TestExtensionListShowsEachExtension(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	code, out, errs := run("extension", "list", "--data-dir", dir)
	want(t, "nothing installed", code, 0, out+errs, "EXTENSION   STATE           VERSION   UPDATES\n"+
		"Kode        not installed\nPresent     not installed\n")

	const version = "0.20261030.0"
	installedAgentK(t, dir, version)
	code, out, _ = run("extension", "list", "--data-dir", dir)
	want(t, "Kode stopped", code, 0, out, "Kode        not running     "+version+"   automatic")
	if err := os.WriteFile(filepath.Join(extensions.Dir(dir, "kode"), "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run("extension", "update", "kode", "--auto=off", "--data-dir", dir)
	want(t, "--auto=off", code, 0, out, "")
	code, out, _ = run("extension", "list", "--data-dir", dir)
	want(t, "Kode running", code, 0, out,
		"Kode        running         "+version+"   by hand (meko extension update kode)")

	// Present from source: no version, and why Meko doesn't update it.
	src := filepath.Join(t.TempDir(), "me_present")
	fakeExtension(t, src)
	reg, _ := extensions.Load(dir)
	reg.Entries["present"] = extensions.Entry{Command: src}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run("extension", "list", "--json", "--data-dir", dir)
	var rows []extensionRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil || code != 0 || len(rows) != 2 {
		t.Fatalf("--json: exit %d, %v, %q", code, err, out)
	}
	if r := rows[0]; r.Name != "kode" || !r.Installed || r.State != extRunning || r.Version != version ||
		r.AutoUpdate || !r.Updatable {
		t.Errorf("Kode's row %+v", r)
	}
	if r := rows[1]; r.Name != "present" || r.State != extRunning || r.Command != src || r.Updatable ||
		r.NotUpdatable == "" || r.RolledBack == nil {
		t.Errorf("Present's row %+v", r)
	}
}

// A registered extension whose command is missing is damaged, unless it's found again, which list
// doesn't save; one whose status doesn't answer in time says so.
func TestExtensionListDamagedAndSlow(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	old := extensions.StatusTimeout
	extensions.StatusTimeout = 200 * time.Millisecond
	t.Cleanup(func() { extensions.StatusTimeout = old })
	missing := filepath.Join(t.TempDir(), "gone", "me_present")
	// Kode's command moved into its release install in the data folder, where Find looks.
	moved := filepath.Join(extensions.Dir(dir, "kode"), "app", "current", "bin", "agentk")
	fakeExtension(t, moved)
	reg, _ := extensions.Load(dir)
	reg.Entries["present"] = extensions.Entry{Command: missing}
	reg.Entries["kode"] = extensions.Entry{Command: filepath.Join(t.TempDir(), "agentk")}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(extensions.File(dir))
	code, out, _ := run("extension", "list", "--data-dir", dir)
	want(t, "damaged", code, 0, out, "Present     damaged ("+missing+" is missing)")
	want(t, "damaged", code, 0, out, "meko extension delete present forgets it")
	want(t, "found again", code, 0, out, "Kode        running")
	if after, _ := os.ReadFile(extensions.File(dir)); string(after) != string(before) {
		t.Errorf("list changed the registry:\n%s", after)
	}

	slow := filepath.Join(t.TempDir(), "me_present")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 5\n"), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	reg.Entries["present"] = extensions.Entry{Command: slow}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code, out, _ = run("extension", "list", "--data-dir", dir)
	want(t, "slow", code, 0, out, "Present     didn't answer in 200ms")
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("list waited %s for a status that doesn't answer", d)
	}
	if strings.Contains(out, "damaged") {
		t.Errorf("a slow extension is damaged:\n%s", out)
	}
}
