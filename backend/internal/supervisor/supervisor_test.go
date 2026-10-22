package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
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
	case "crash-leader": // starts a stubborn child in its own process group, then crashes once it's ready
		startStubbornChild()
		for {
			if _, err := os.Stat(os.Getenv("STUB_READY")); err == nil { //nolint:gosec // G703: the test's temp folder
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		os.Exit(1)
	case "leader": // starts a stubborn child in its own process group, then exits on SIGTERM
		startStubbornChild()
		time.Sleep(time.Hour)
	}
}

// startStubbornChild starts a stub that ignores SIGTERM in this process's group and writes its
// pid to $STUB_CHILD_PID.
func startStubbornChild() {
	exe, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	child := exec.Command(exe)
	child.Env = append(os.Environ(), "STUB_MODE=stubborn")
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	pid := []byte(strconv.Itoa(child.Process.Pid))
	// G703: the path is the test's own temp folder.
	if err := os.WriteFile(os.Getenv("STUB_CHILD_PID"), pid, 0o600); err != nil { //nolint:gosec
		os.Exit(2)
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
	if st.Restarts != 2 || !strings.Contains(st.LastError, "exited") { // 3 starts, 2 restarts
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

// Whatever a child leaves in its group gets SIGKILL, whether the child exits on the supervisor's
// SIGTERM or crashes on its own.
func TestLeftoversInTheGroupAreKilled(t *testing.T) {
	for _, mode := range []string{"leader", "crash-leader"} {
		t.Run(mode, func(t *testing.T) {
			s, logs := newSupervisor()
			sp := spec(t, mode)
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			sp.Env = append(sp.Env, "STUB_CHILD_PID="+pidFile)
			sp.StopTimeout = 300 * time.Millisecond
			if mode == "crash-leader" {
				sp.StopTimeout = 2 * time.Second // long enough to see the state during the sweep
			}
			sp.MaxCrashes = 1
			s.Start(sp)
			waitFor(t, s, "running", func(st Status) bool { return st.State == Running })
			var grandchild int
			for deadline := time.Now().Add(5 * time.Second); grandchild == 0; time.Sleep(20 * time.Millisecond) {
				if b, err := os.ReadFile(pidFile); err == nil {
					grandchild, _ = strconv.Atoi(string(b))
				}
				if time.Now().After(deadline) {
					t.Fatal("the leader never started its child")
				}
			}
			if mode == "crash-leader" {
				// The leader crashes about 200 ms after it's ready, and the sweep of its group takes
				// StopTimeout: the child must stop showing as running before that sweep ends.
				time.Sleep(time.Second)
				if st := s.Statuses()[0]; st.State == Running {
					t.Fatalf("a crashed child still shows as running during the sweep: %+v", st)
				}
				waitFor(t, s, "failed", func(st Status) bool { return st.State == Failed })
			}
			if err := s.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(5 * time.Second); alive(grandchild); time.Sleep(20 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("pid %d, left in the group, survived", grandchild)
				}
			}
			if !strings.Contains(logs.String(), "processes left in the child's group") {
				t.Errorf("log: %s", logs.String())
			}
		})
	}
}

func TestUnixHTTPCheck(t *testing.T) {
	// A short folder: macOS limits unix socket paths to 104 bytes, and t.TempDir's can be longer.
	dir, err := os.MkdirTemp("", "sv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ok" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	ctx := context.Background()
	if err := UnixHTTPCheck(sock, "/ok")(ctx); err != nil {
		t.Errorf("2xx: %v", err)
	}
	if err := UnixHTTPCheck(sock, "/down")(ctx); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("503: got %v", err)
	}
	if err := UnixHTTPCheck(filepath.Join(dir, "none.sock"), "/ok")(ctx); err == nil {
		t.Error("a missing socket should fail")
	}
}

// A running child's pid, process group and start are recorded, and the record goes when it stops.
func TestPidFile(t *testing.T) {
	s, _ := newSupervisor()
	sp := spec(t, "healthy")
	sp.PidFile = filepath.Join(t.TempDir(), "stub")
	s.Start(sp)
	st := waitFor(t, s, "running", func(st Status) bool { return st.State == Running })
	b, err := os.ReadFile(sp.PidFile)
	if want, _ := Record(st.PID); err != nil || string(b) != want {
		t.Fatalf("pid file %q, %v; want %q", b, err, want)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sp.PidFile); !os.IsNotExist(err) {
		t.Errorf("pid file left after stop: %v", err)
	}
}

// KillRecorded kills a recorded group whose leader still has the recorded pgid, and never a
// process whose pid was reused (here: this test, recorded with a pgid it doesn't have).
func TestKillRecorded(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := func() (*exec.Cmd, chan struct{}) {
		cmd := exec.Command(exe)
		cmd.Env = append(os.Environ(), "STUB_MODE=stubborn", "STUB_READY="+filepath.Join(dir, "ready"))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		return cmd, exited
	}
	leader, exited := start()
	other, otherExited := start() // a group leader whose pid a stale record names
	defer func() { _ = syscall.Kill(-other.Process.Pid, syscall.SIGKILL) }()
	rec, err := Record(leader.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]string{
		"memory_engine": rec,
		"reused":        fmt.Sprintf("%d %d 0:0\n", other.Process.Pid, other.Process.Pid),
		"group":         fmt.Sprintf("%d %d %s\n", os.Getpid(), os.Getpid()+100000, "x"),
		"old":           fmt.Sprintf("%d %d\n", other.Process.Pid, other.Process.Pid),
		"junk":          "not a pid\n",
	}
	for name, rec := range records {
		if err = os.WriteFile(filepath.Join(dir, name), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	n, err := KillRecorded(dir)
	if err != nil || n != 1 {
		t.Fatalf("killed %d, %v; want 1", n, err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the recorded child is still running")
	}
	select {
	case <-otherExited:
		t.Fatal("a process that reused a recorded pid was killed")
	case <-time.After(200 * time.Millisecond):
	}
	for name := range records {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("record %s left: %v", name, err)
		}
	}
	if n, err := KillRecorded(filepath.Join(dir, "missing")); n != 0 || err != nil {
		t.Errorf("a missing folder: %d %v", n, err)
	}
}
