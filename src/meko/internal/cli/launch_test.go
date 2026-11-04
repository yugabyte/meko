package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// launchApp is an extension's app folder in a data folder, with a launcher for each version: body
// runs after it logs its version, MEKO_LAUNCHED, MEKO_UPDATED_FROM and its arguments to log.
func launchApp(t *testing.T, name string, bodies map[string]string) (update.App, string) {
	t.Helper()
	dir := shortDir(t)
	cat := extensions.Known[name]
	app := cat.App(dir, name)
	log := filepath.Join(dir, "launch.log")
	var last string
	for v, body := range bodies {
		if err := os.MkdirAll(filepath.Join(app.Dir(v), "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\necho \"" + v + " launched=$MEKO_LAUNCHED from=$MEKO_UPDATED_FROM $*\" >> " + log +
			"\n" + body + "\n"
		if err := os.WriteFile(app.Exe(v), []byte(script), 0o700); err != nil { //nolint:gosec // G306: test
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(app.Dir(v), "VERSION"), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if v > last {
			last = v
		}
	}
	if err := app.Repoint(last); err != nil {
		t.Fatal(err)
	}
	return app, log
}

// launch runs meko _launch (this test binary) for name's version's launcher.
func launch(t *testing.T, app update.App, name, version string, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, append([]string{"_launch", name, app.Exe(version)}, args...)...) //nolint:gosec // G204: test
	cmd.Env = append(os.Environ(), testHealthyAfterEnv+"=1h")
	return cmd
}

func exitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok { //nolint:errorlint // exec returns it unwrapped
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestLaunchSupported(t *testing.T) {
	code, out, errs := run("_launch", "--supported")
	if code != 0 || out+errs != "" {
		t.Errorf("--supported: %d %q", code, out+errs)
	}
	code, _, errs = run("_launch", "frob", "/bin/true", "serve")
	want(t, "an unknown extension", code, 1, errs, `no extension called "frob"`)
}

// A new version that keeps failing to start is counted at each start, with MEKO_UPDATED_FROM set,
// and on the fourth start rolled back to the old version, which runs instead and is recorded bad.
func TestLaunchRollsBackAFailingVersion(t *testing.T) {
	const old, next = "0.20261024.0", "0.20261025.0"
	app, log := launchApp(t, "presenter", map[string]string{old: "exit 0", next: "exit 1"})
	if err := app.Switch(old, next, 0); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= update.MaxFailedStarts; i++ {
		if code := exitCode(launch(t, app, "presenter", next, "serve").Run()); code != 1 {
			t.Fatalf("start %d: exit %d", i, code)
		}
		if p, _ := app.ReadPending(); p == nil || p.Attempts != i {
			t.Fatalf("start %d: pending %+v", i, p)
		}
	}
	if code := exitCode(launch(t, app, "presenter", next, "serve").Run()); code != 0 {
		t.Fatalf("the rolled-back start: exit %d", code)
	}
	if p, _ := app.ReadPending(); p != nil || app.Current() != old || !app.BadVersions()[next] {
		t.Errorf("after the rollback: pending %+v, current %s, bad %v", p, app.Current(), app.BadVersions())
	}
	b, _ := os.ReadFile(log) //nolint:gosec // G304: test
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 4 || lines[0] != next+" launched=1 from="+old+" serve" ||
		lines[3] != old+" launched=1 from= serve" {
		t.Errorf("log %q", lines)
	}
}

// A clean end (exit 0, or a stop Meko passed on) isn't a failed start; AgentK's exit 75 isn't either.
func TestLaunchDoesntCountACleanStop(t *testing.T) {
	const old, next = "0.20261024.0", "0.20261025.0"
	app, _ := launchApp(t, "agentk", map[string]string{old: "exit 0",
		next: `[ -n "$EXIT_BUSY" ] && exit 75; exec sleep 30`})
	if err := app.Switch(old, next, 0); err != nil {
		t.Fatal(err)
	}
	busy := launch(t, app, "agentk", next, "serve")
	busy.Env = append(busy.Env, "EXIT_BUSY=1")
	if code := exitCode(busy.Run()); code != 75 {
		t.Fatalf("exit %d", code)
	}
	if p, _ := app.ReadPending(); p == nil || p.Attempts != 0 {
		t.Fatalf("pending %+v after exit 75", p)
	}
	cmd := launch(t, app, "agentk", next, "serve")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if p, _ := app.ReadPending(); p != nil && p.Attempts == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // the child is running
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if code := exitCode(cmd.Wait()); code != 128+int(syscall.SIGTERM) {
		t.Errorf("exit %d after SIGTERM", code)
	}
	if p, _ := app.ReadPending(); p == nil || p.Attempts != 0 {
		t.Errorf("pending %+v after a stop", p)
	}
}

// Once the new version has run HealthyAfter its update is done: the record goes, and the versions
// before the one it replaced are pruned.
func TestLaunchFinishesAHealthyUpdate(t *testing.T) {
	const older, old, next = "0.20261023.0", "0.20261024.0", "0.20261025.0"
	app, _ := launchApp(t, "presenter", map[string]string{older: "exit 0", old: "exit 0", next: "sleep 1"})
	if err := app.Switch(old, next, 0); err != nil {
		t.Fatal(err)
	}
	cmd := launch(t, app, "presenter", next, "serve")
	cmd.Env = append(cmd.Env, testHealthyAfterEnv+"=200ms")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "its update is done") {
		t.Fatalf("%v %s", err, out)
	}
	if p, _ := app.ReadPending(); p != nil {
		t.Errorf("pending %+v", p)
	}
	if vs := app.Versions(); strings.Join(vs, " ") != old+" "+next {
		t.Errorf("versions %v", vs)
	}
}

// A command outside Probation (AgentK's host) runs in meko _launch's place, its version marked in
// use while it runs, so pruning keeps it.
func TestLaunchExecsAndMarksTheVersionInUse(t *testing.T) {
	const v = "0.20261024.0"
	app, log := launchApp(t, "agentk", map[string]string{v: `echo "pid $$" >> "$(dirname "$0")/pid"; sleep 1`})
	cmd := launch(t, app, "agentk", v, "host")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(app.Dir(v), "bin", "pid")
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !app.InUse(v) {
		t.Error("not marked in use while it runs")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(pidFile) //nolint:gosec // G304: test
	if strings.TrimSpace(string(b)) != "pid "+strconv.Itoa(cmd.Process.Pid) {
		t.Errorf("ran as %q, not in meko _launch's place (%d)", b, cmd.Process.Pid)
	}
	if app.InUse(v) {
		t.Error("still marked in use after it ended")
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), v+" launched=1 from= host") { //nolint:gosec // G304: test
		t.Errorf("log %q", b)
	}
}

// A launcher outside an app folder (a build from source) is just run.
func TestLaunchRunsASourceBuild(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agentk")
	out := filepath.Join(dir, "out")
	//nolint:gosec // G306: test
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho \"launched=$MEKO_LAUNCHED $*\" > "+out+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	if err := exec.Command(self, "_launch", "agentk", exe, "serve").Run(); err != nil { //nolint:gosec // G204: test
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "launched=1 serve\n" { //nolint:gosec // G304: test
		t.Errorf("%q", b)
	}
}
