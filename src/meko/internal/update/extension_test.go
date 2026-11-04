package update

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The update lock keeps a second taker out, in this process and through the file in another (a
// second Lock on the same data folder stands in for meko update's process).
func TestLockIsHeldAcrossTakers(t *testing.T) {
	dir := t.TempDir()
	a, b := NewLock(dir), NewLock(dir)
	if !a.TryLock() {
		t.Fatal("a free lock wasn't taken")
	}
	if a.TryLock() {
		t.Error("taken twice in one process")
	}
	if b.TryLock() {
		t.Error("taken through the file while held")
	}
	a.Unlock()
	if !b.TryLock() {
		t.Fatal("not taken once let go")
	}
	b.Unlock()
}

// A version a process marked in use (meko _launch, for a session host that outlived later
// updates) isn't pruned; once its mark goes, it is.
func TestPruneKeepsAVersionInUse(t *testing.T) {
	a := newApp(t, "0.20261020.0", "0.20261021.0", "0.20261022.0")
	f, err := a.LockInUse("0.20261020.0")
	if err != nil {
		t.Fatal(err)
	}
	if !a.InUse("0.20261020.0") || a.InUse("0.20261021.0") {
		t.Errorf("InUse: %v %v", a.InUse("0.20261020.0"), a.InUse("0.20261021.0"))
	}
	if gone := a.Prune("0.20261022.0"); len(gone) != 1 || gone[0] != "0.20261021.0" {
		t.Errorf("pruned %v", gone)
	}
	_ = f.Close()
	if gone := a.Prune("0.20261022.0"); len(gone) != 1 || gone[0] != "0.20261020.0" {
		t.Errorf("pruned %v after the mark went", gone)
	}
}

// AgentK's own updater kept its record and bad versions in its home: the record moves into the app
// folder (its at in milliseconds), the bad versions count (a v dropped), and a rollback adds to both
// lists.
func TestLegacyRecords(t *testing.T) {
	a := newApp(t, "0.20261021.0", "0.20261022.0")
	a.Legacy = filepath.Dir(a.Root)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(a.Legacy, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("bad-versions", "v0.20261019.0\n0.20261020.0\n")
	write("update-pending.json", `{"from":"0.20261021.0","to":"0.20261022.0","attempts":2,"at":1761000000000}`)
	bad := a.BadVersions()
	if !bad["0.20261019.0"] || !bad["0.20261020.0"] || len(bad) != 2 {
		t.Errorf("BadVersions = %v", bad)
	}
	p, err := a.ReadPending()
	if err != nil || p == nil || p.From != "0.20261021.0" || p.To != "0.20261022.0" || p.Attempts != 2 ||
		!p.At.Equal(time.UnixMilli(1761000000000)) {
		t.Fatalf("ReadPending = %+v, %v", p, err)
	}
	if _, err := os.Stat(filepath.Join(a.Legacy, "update-pending.json")); !os.IsNotExist(err) {
		t.Errorf("the legacy record stayed: %v", err)
	}
	if err := a.RollBack(*p); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(a.Root, "bad-versions"), filepath.Join(a.Legacy, "bad-versions")} {
		if b, _ := os.ReadFile(f); !strings.Contains(string(b), "0.20261022.0\n") { //nolint:gosec // G304: test
			t.Errorf("%s: %q", f, b)
		}
	}
	if a.Current() != "0.20261021.0" {
		t.Errorf("current %s", a.Current())
	}
}

// A release whose `version --json` prints the bare version (AgentK's from before --json) passes the
// check; one that names another version doesn't. Checks run on the staged binary, and one that
// fails keeps the version out.
func TestCheckBinaryAndChecks(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agentk")
	script := func(body string) {
		if err := os.WriteFile(exe, []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // G306: test
			t.Fatal(err)
		}
	}
	script("echo 0.20261022.0\n")
	if err := CheckBinary(exe, "0.20261022.0"); err != nil {
		t.Errorf("plain version: %v", err)
	}
	script("echo 0.20261021.0\n")
	if err := CheckBinary(exe, "0.20261022.0"); err == nil {
		t.Error("another plain version passed")
	}

	const v = "0.20261024.0"
	f := filepath.Join(t.TempDir(), "x.tar.gz")
	if err := os.WriteFile(f, tarballOf(t, "agentk", "agentk", v), 0o600); err != nil {
		t.Fatal(err)
	}
	a := App{Root: filepath.Join(t.TempDir(), "app"), Name: "agentk", Checks: [][]string{{"version", "--json"}}}
	if err := os.MkdirAll(a.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.Unpack(f, v); err != nil {
		t.Fatalf("a passing check: %v", err)
	}
	a.Checks = [][]string{{"selftest"}} // the fake answers only version --json
	if err := a.Unpack(f, v); err == nil || !strings.Contains(err.Error(), "agentk selftest failed") {
		t.Errorf("a failing check: %v", err)
	}
}

// An extensions-only updater (a Meko built from source) records the extensions' newest releases and
// runs AfterCheck, and never looks for Meko's.
func TestExtensionsOnlyUpdater(t *testing.T) {
	data := t.TempDir()
	c := releaseServer(t, "0.20261030.0", tarball(t, "0.20261030.0"), "")
	ran := make(chan struct{})
	u := &Updater{ExtensionsOnly: true, Version: "0.0.0-dev", Client: c, DataDir: data, Log: slog.New(slog.DiscardHandler),
		FirstCheck: time.Millisecond, Interval: time.Hour,
		Extensions: func(rels []Release) map[string]ExtensionState {
			return map[string]ExtensionState{"presenter": {Latest: "0.20261024.0"}}
		},
		AfterCheck: func(context.Context) { close(ran) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.Run(ctx)
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("AfterCheck didn't run")
	}
	st, ok := ReadState(data)
	if !ok || st.Latest != "" || st.Extensions["presenter"].Latest != "0.20261024.0" {
		t.Errorf("state %+v", st)
	}
}

// A version a live process names in a Running record (AgentK's run/host.json: a session host started
// before meko _launch, or without it) isn't pruned; one a dead process names is.
func TestPruneKeepsAVersionARunningRecordNames(t *testing.T) {
	a := newApp(t, "0.20261020.0", "0.20261021.0", "0.20261022.0")
	rec := filepath.Join(t.TempDir(), "host.json")
	a.Running = []string{rec}
	write := func(body string) {
		if err := os.WriteFile(rec, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(fmt.Sprintf(`{"pid":%d,"appDir":"/x/app/0.20261020.0","version":"0.20261020.0"}`, os.Getpid()))
	if gone := a.Prune("0.20261022.0"); len(gone) != 1 || gone[0] != "0.20261021.0" {
		t.Errorf("pruned %v with the host alive", gone)
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	write(fmt.Sprintf(`{"pid":%d,"appDir":null,"version":"0.20261020.0"}`, dead.Process.Pid))
	if gone := a.Prune("0.20261022.0"); len(gone) != 1 || gone[0] != "0.20261020.0" {
		t.Errorf("pruned %v with the host gone", gone)
	}
}

// While a new Meko is on probation, a scheduled check installs nothing of Meko's, but still runs,
// and so do the extensions' automatic updates.
func TestProbationStillChecksForTheExtensions(t *testing.T) {
	const cur, next = "0.20261021.0", "0.20261030.0"
	a := newApp(t, "0.20261020.0", cur)
	if err := a.Switch("0.20261020.0", cur, 0); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	ran := make(chan struct{})
	u := &Updater{App: a, Version: cur, Client: releaseServer(t, next, tarball(t, next), ""), DataDir: data,
		Auto: true, Log: slog.New(slog.DiscardHandler), FirstCheck: time.Millisecond, Interval: time.Hour,
		Restart:    func() { t.Error("restarted on probation") },
		AfterCheck: func(context.Context) { close(ran) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.Run(ctx)
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("AfterCheck didn't run on probation")
	}
	if st, _ := ReadState(data); st.Latest != next || a.Current() != cur {
		t.Errorf("latest %q, current %q", st.Latest, a.Current())
	}
}
