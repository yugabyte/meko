package proc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// envMap converts the "KEY=VALUE" slice returned by buildChildEnv into a map.
func envMap(kvs []string) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func TestBuildChildEnvScrubAndPassthrough(t *testing.T) {
	// A non-allowlisted, non-YB var must be scrubbed.
	t.Setenv("SOME_RANDOM_VAR", "should-be-dropped")
	// An allowlisted var must pass through.
	t.Setenv("RUST_LOG", "debug")
	// Any YB_-prefixed var must pass through.
	t.Setenv("YB_STORAGE_PASSWORD", "s3cret")
	// RUST_BACKTRACE explicitly unset so we can check the default.
	os.Unsetenv("RUST_BACKTRACE")

	env := envMap(buildChildEnv(map[string]string{"EXTRA_ONE": "1", "RUST_LOG": "info"}))

	if _, ok := env["SOME_RANDOM_VAR"]; ok {
		t.Errorf("non-allowlisted var leaked into child env: %v", env)
	}
	if got := env["YB_STORAGE_PASSWORD"]; got != "s3cret" {
		t.Errorf("YB_ var not passed through: got %q", got)
	}
	if got := env["RUST_BACKTRACE"]; got != "1" {
		t.Errorf("RUST_BACKTRACE default = %q, want \"1\"", got)
	}
	// extraEnv overrides the allowlisted RUST_LOG=debug.
	if got := env["RUST_LOG"]; got != "info" {
		t.Errorf("extraEnv did not override RUST_LOG: got %q", got)
	}
	if got := env["EXTRA_ONE"]; got != "1" {
		t.Errorf("extraEnv EXTRA_ONE = %q, want \"1\"", got)
	}
}

func TestBuildChildEnvBacktracePassThrough(t *testing.T) {
	t.Setenv("RUST_BACKTRACE", "full")
	env := envMap(buildChildEnv(nil))
	if got := env["RUST_BACKTRACE"]; got != "full" {
		t.Errorf("RUST_BACKTRACE passthrough = %q, want \"full\"", got)
	}
}

// TestStartStopRoundTripPidCreate spawns a trivial child in PidCreate mode,
// verifies the launcher-created pidfile is locked with the child's pid, then
// stops it.
func TestStartStopRoundTripPidCreate(t *testing.T) {
	dir := t.TempDir()

	err := StartProcess(
		"child",
		dir,
		"/bin/sh",
		// exec so the shell becomes `sleep`, keeping the same pid and the
		// inherited pidfile fd (and its lock).
		[]string{"-c", "exec sleep 30"},
		nil,
		PidCreate,
		func() error { return nil }, // ready immediately
		5*time.Second,
	)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	pidFilePath := filepath.Join(dir, "child.pid")
	content, err := os.ReadFile(pidFilePath)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		t.Fatalf("parse pidfile %q: %v", content, err)
	}

	// The child must hold the flock (readPidFile classifies it as locked).
	kind, filePid, err := readPidFile(pidFilePath)
	if err != nil {
		t.Fatalf("readPidFile: %v", err)
	}
	if kind != pidLocked || filePid != pid {
		t.Fatalf("readPidFile = (kind=%d, pid=%d), want (locked, %d)", kind, filePid, pid)
	}

	// Ensure the process is really alive before we stop it.
	if err := unix.Kill(pid, 0); err != nil {
		t.Fatalf("child pid %d not alive: %v", pid, err)
	}

	// In production, StopProcess runs from a separate launcher invocation and
	// the service has been reparented to init, which reaps it. Here the test
	// process is the child's parent, so reap it ourselves once it exits (via
	// SIGTERM below); otherwise it lingers as a zombie and kill(pid,0) keeps
	// reporting it alive.
	go func() {
		var ws unix.WaitStatus
		_, _ = unix.Wait4(pid, &ws, 0, nil)
	}()

	if err := StopProcess("child", dir, StopFast); err != nil {
		t.Fatalf("StopProcess: %v", err)
	}

	// After stopping, the process must be gone.
	if err := unix.Kill(pid, 0); err == nil {
		t.Errorf("process %d still alive after StopProcess", pid)
	}
}

func TestStopProcessNoPidFile(t *testing.T) {
	dir := t.TempDir()
	if err := StopProcess("missing", dir, StopFast); err != nil {
		t.Errorf("StopProcess on missing pidfile = %v, want nil", err)
	}
}

// TestCreateLockedPidFileConflict verifies a second lock attempt on the same
// path fails while the first is held, and succeeds once released.
func TestCreateLockedPidFileConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.pid")

	f, err := createLockedPidFile(path)
	if err != nil {
		t.Fatalf("first createLockedPidFile: %v", err)
	}
	if _, err := createLockedPidFile(path); err == nil {
		t.Errorf("second createLockedPidFile succeeded while lock held; want error")
	}
	f.Close() // releases the lock

	f2, err := createLockedPidFile(path)
	if err != nil {
		t.Fatalf("createLockedPidFile after release: %v", err)
	}
	f2.Close()
}

// TestReadPidFileNotHeld verifies a pidfile with no live holder is classified
// as not-held.
func TestReadPidFileNotHeld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "y.pid")
	if err := os.WriteFile(path, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	kind, _, err := readPidFile(path)
	if err != nil {
		t.Fatalf("readPidFile: %v", err)
	}
	if kind != pidNotHeld {
		t.Errorf("readPidFile kind = %d, want pidNotHeld", kind)
	}
}

func TestRepoLock(t *testing.T) {
	dir := t.TempDir()
	l, err := LockRepo(dir)
	if err != nil {
		t.Fatalf("LockRepo: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Second lock after release must succeed.
	l2, err := LockRepo(dir)
	if err != nil {
		t.Fatalf("LockRepo after release: %v", err)
	}
	l2.Close()
}
