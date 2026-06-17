package process

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// discardLogger logs to io.Discard; used where the test doesn't inspect output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// requireSh skips the test when /bin/sh is unavailable.
func requireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

func TestLogWriterSplitsAndFlushes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := &logWriter{logger: logger, stream: "stdout"}

	// A line split across two writes emits exactly once, on the newline.
	w.Write([]byte("hello\nwor"))
	if got := strings.Count(buf.String(), "line="); got != 1 {
		t.Fatalf("after partial second line, line records = %d, want 1", got)
	}
	w.Write([]byte("ld\n\n")) // completes "world", then a blank line (skipped)
	w.flush()                 // nothing buffered

	out := buf.String()
	if c := strings.Count(out, "line="); c != 2 {
		t.Fatalf("line records = %d, want 2 (blank line skipped); output:\n%s", c, out)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Fatalf("missing expected lines in output:\n%s", out)
	}
	if !strings.Contains(out, "stream=stdout") {
		t.Fatalf("missing stream attribute in output:\n%s", out)
	}
}

func TestLogWriterFlushesTrailingPartialLine(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := &logWriter{logger: logger, stream: "stderr"}

	w.Write([]byte("no newline here"))
	if strings.Contains(buf.String(), "line=") {
		t.Fatal("partial line emitted before flush")
	}
	w.flush()
	if !strings.Contains(buf.String(), "no newline here") {
		t.Fatalf("trailing partial line not flushed:\n%s", buf.String())
	}
}

func TestSupervisorRestartsOnExit(t *testing.T) {
	requireSh(t)
	marker := filepath.Join(t.TempDir(), "runs")

	spec := Spec{
		Name: "flaky",
		Path: "sh",
		// $1 is the marker file; record a run, then fail so the supervisor restarts us.
		Args:    []string{"-c", `echo . >> "$1"; exit 1`, "sh", marker},
		Restart: Backoff{Initial: time.Millisecond, Max: 5 * time.Millisecond, ResetAfter: time.Hour},
	}
	sup := NewSupervisor(spec, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()

	// Wait until it has restarted at least twice, proving the loop re-spawns.
	waitFor(t, 3*time.Second, func() bool { return countLines(t, marker) >= 3 })

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if n := countLines(t, marker); n < 2 {
		t.Fatalf("process ran %d times, want >= 2 (no restart)", n)
	}
}

func TestSupervisorRestartsWhenKilled(t *testing.T) {
	requireSh(t)
	pidfile := filepath.Join(t.TempDir(), "pid")

	spec := Spec{
		Name: "killable",
		Path: "sh",
		// Record our PID, then idle until signalled.
		Args:        []string{"-c", `echo $$ > "$1"; trap 'exit 0' TERM; while true; do sleep 0.05; done`, "sh", pidfile},
		Restart:     Backoff{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond, ResetAfter: time.Hour},
		StopTimeout: 2 * time.Second,
	}
	sup := NewSupervisor(spec, discardLogger())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()

	pid1 := waitPidFile(t, pidfile, 0)
	// Externally kill the child, as if it had crashed or been OOM-killed.
	if err := syscall.Kill(pid1, syscall.SIGKILL); err != nil {
		cancel()
		t.Fatalf("kill %d: %v", pid1, err)
	}
	// The supervisor must spawn a replacement with a fresh PID.
	pid2 := waitPidFile(t, pidfile, pid1)
	if pid2 <= 0 || pid2 == pid1 {
		cancel()
		t.Fatalf("expected a restart with a new pid, got %d (was %d)", pid2, pid1)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// waitPidFile polls path until it holds a positive integer PID other than
// exclude, and returns it.
func waitPidFile(t *testing.T, path string, exclude int) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 && pid != exclude {
				return pid
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no new pid in %s (excluding %d) within timeout", path, exclude)
	return 0
}

func TestSupervisorStopsGracefullyOnCancel(t *testing.T) {
	requireSh(t)
	started := filepath.Join(t.TempDir(), "up")

	spec := Spec{
		Name: "graceful",
		Path: "sh",
		// Mark startup, exit cleanly on SIGTERM.
		Args:        []string{"-c", `echo up > "$1"; trap 'exit 0' TERM; while true; do sleep 0.05; done`, "sh", started},
		StopTimeout: 5 * time.Second,
	}
	sup := NewSupervisor(spec, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { _, err := os.Stat(started); return err == nil })

	cancel()
	start := time.Now()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	// SIGTERM is honored, so we return well before the SIGKILL deadline.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("graceful stop took %s, want it to honor SIGTERM promptly", elapsed)
	}
}

func TestSupervisorKillsAfterStopTimeout(t *testing.T) {
	requireSh(t)
	started := filepath.Join(t.TempDir(), "up")

	spec := Spec{
		Name: "stubborn",
		Path: "sh",
		// Ignore SIGTERM entirely; only SIGKILL after StopTimeout ends it.
		Args:        []string{"-c", `echo up > "$1"; trap '' TERM; while true; do sleep 0.05; done`, "sh", started},
		StopTimeout: 300 * time.Millisecond,
	}
	sup := NewSupervisor(spec, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { _, err := os.Stat(started); return err == nil })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return: stubborn process was not killed after StopTimeout")
	}
}

func TestManagerSupervisesMultipleAndStopsAll(t *testing.T) {
	requireSh(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")

	mgr := NewManager(discardLogger())
	for _, mark := range []string{a, b} {
		mgr.Add(Spec{
			Name:        "svc",
			Path:        "sh",
			Args:        []string{"-c", `echo up > "$1"; trap 'exit 0' TERM; while true; do sleep 0.05; done`, "sh", mark},
			StopTimeout: 5 * time.Second,
		})
	}
	if mgr.Len() != 2 {
		t.Fatalf("Len = %d, want 2", mgr.Len())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); mgr.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool {
		_, ea := os.Stat(a)
		_, eb := os.Stat(b)
		return ea == nil && eb == nil
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Manager.Run did not return after cancel")
	}
}

func TestManagerRunWithNoProcessesReturns(t *testing.T) {
	mgr := NewManager(discardLogger())
	done := make(chan struct{})
	go func() { defer close(done); mgr.Run(context.Background()) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run with no processes should return immediately")
	}
}

func TestSupervisorSignalsReady(t *testing.T) {
	requireSh(t)
	var ready atomic.Bool
	spec := Spec{
		Name:          "readyish",
		Path:          "sh",
		Args:          []string{"-c", `trap 'exit 0' TERM; while true; do sleep 0.05; done`},
		ReadyInterval: 5 * time.Millisecond,
		StopTimeout:   2 * time.Second,
		Ready: func(context.Context) error {
			if ready.Load() {
				return nil
			}
			return io.EOF // any non-nil error means "not ready yet"
		},
	}
	sup := NewSupervisor(spec, discardLogger())

	go sup.Run(t.Context())

	// Not ready yet.
	select {
	case <-sup.Ready():
		t.Fatal("Ready closed before the probe passed")
	case <-time.After(50 * time.Millisecond):
	}

	ready.Store(true)
	select {
	case <-sup.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("Ready never closed after the probe started passing")
	}
}

func TestSupervisorReadyTimeoutStopsProbing(t *testing.T) {
	requireSh(t)
	var probes atomic.Int32
	spec := Spec{
		Name:          "neverready",
		Path:          "sh",
		Args:          []string{"-c", `trap 'exit 0' TERM; while true; do sleep 0.05; done`},
		ReadyInterval: 5 * time.Millisecond,
		ReadyTimeout:  60 * time.Millisecond,
		StopTimeout:   2 * time.Second,
		Ready: func(context.Context) error {
			probes.Add(1)
			return io.EOF // never ready
		},
	}
	sup := NewSupervisor(spec, discardLogger())

	go sup.Run(t.Context())

	// The probe gives up after ReadyTimeout, so the count stops climbing and
	// Ready() never closes.
	time.Sleep(200 * time.Millisecond)
	settled := probes.Load()
	select {
	case <-sup.Ready():
		t.Fatal("Ready closed even though the probe never passed")
	default:
	}
	time.Sleep(100 * time.Millisecond)
	if grew := probes.Load() - settled; grew != 0 {
		t.Fatalf("probe kept running after ReadyTimeout (%d more calls)", grew)
	}
}
