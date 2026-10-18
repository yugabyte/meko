package supervisor

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the stub child: the supervisor re-runs this test binary with STUB_MODE set.
func TestMain(m *testing.M) {
	switch os.Getenv("STUB_MODE") {
	case "":
		os.Exit(m.Run())
	case "crash":
		os.Exit(1)
	case "stubborn": // ignores SIGTERM, so only SIGKILL stops it
		signal.Ignore(syscall.SIGTERM)
		fallthrough
	case "healthy":
		// G703: the path is the test's own temp folder.
		if err := os.WriteFile(os.Getenv("STUB_READY"), nil, 0o600); err != nil { //nolint:gosec
			os.Exit(2)
		}
		fallthrough
	case "never-ready":
		time.Sleep(time.Hour)
	}
}

func spec(t *testing.T, mode string) Spec {
	t.Helper()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Spec{
		Name:    "stub",
		Path:    exe,
		Env:     []string{"STUB_MODE=" + mode, "STUB_READY=" + ready},
		LogFile: filepath.Join(dir, "stub.log"),
		Health: func(context.Context) error {
			_, err := os.Stat(ready)
			return err
		},
		StartTimeout: 5 * time.Second,
		MinBackoff:   10 * time.Millisecond,
		MaxBackoff:   20 * time.Millisecond,
		StopTimeout:  5 * time.Second,
	}
}

func waitFor(t *testing.T, s *Supervisor, what string, ok func(Status) bool) Status {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if st := s.Statuses()[0]; ok(st) {
			return st
		}
	}
	t.Fatalf("timed out waiting for %s; status %+v", what, s.Statuses()[0])
	return Status{}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func newSupervisor() (*Supervisor, *bytes.Buffer) {
	var logs bytes.Buffer
	return New(slog.New(slog.NewTextHandler(&logs, nil))), &logs
}

func TestRunsAndStops(t *testing.T) {
	s, _ := newSupervisor()
	s.Start(spec(t, "healthy"))
	st := waitFor(t, s, "running", func(st Status) bool { return st.State == Running })
	if st.PID == 0 || !alive(st.PID) {
		t.Fatalf("running without a live pid: %+v", st)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.Statuses()[0].State; got != Stopped {
		t.Errorf("state %s after stop", got)
	}
	if alive(st.PID) {
		t.Errorf("pid %d still alive", st.PID)
	}
}

func TestRestartsThenFails(t *testing.T) {
	s, logs := newSupervisor()
	sp := spec(t, "crash")
	sp.MaxCrashes = 3
	s.Start(sp)
	st := waitFor(t, s, "failed", func(st Status) bool { return st.State == Failed })
	if st.Restarts != 3 || !strings.Contains(st.LastError, "exited") {
		t.Fatalf("status %+v", st)
	}
	if !strings.Contains(logs.String(), "too many crashes") {
		t.Errorf("log: %s", logs.String())
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUnhealthyCountsAsCrash(t *testing.T) {
	s, _ := newSupervisor()
	sp := spec(t, "never-ready")
	sp.StartTimeout = 300 * time.Millisecond
	sp.StopTimeout = time.Second
	sp.MaxCrashes = 1
	s.Start(sp)
	st := waitFor(t, s, "failed", func(st Status) bool { return st.State == Failed })
	if !strings.Contains(st.LastError, "not healthy within") {
		t.Fatalf("status %+v", st)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStopKillsAfterTimeout(t *testing.T) {
	s, logs := newSupervisor()
	sp := spec(t, "stubborn")
	sp.StopTimeout = 300 * time.Millisecond
	s.Start(sp)
	st := waitFor(t, s, "running", func(st Status) bool { return st.State == Running })
	begin := time.Now()
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if alive(st.PID) {
		t.Fatalf("pid %d survived SIGKILL", st.PID)
	}
	if d := time.Since(begin); d < 300*time.Millisecond {
		t.Errorf("stopped in %s: SIGTERM should have been ignored", d)
	}
	if !strings.Contains(logs.String(), "killing it") {
		t.Errorf("log: %s", logs.String())
	}
}
