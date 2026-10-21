package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/supervisor"
)

// TestMain doubles as the meko binary: start re-runs os.Executable() (this test binary)
// as "serve ...".
func TestMain(m *testing.M) {
	for _, k := range []string{paths.HomeEnv, "MEKO_PORT", "MEKO_LOG_LEVEL"} {
		_ = os.Unsetenv(k)
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if mode := os.Getenv("MEKO_TEST_SERVE"); mode != "" {
			hungServe(mode)
		}
		testEmbedder = os.Getenv(testEmbedderEnv)
		os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	// Only in the tests' own process: a test sets it for the serve it starts.
	_ = os.Unsetenv("MEKO_AUTH")
	os.Exit(m.Run())
}

// testEmbedderEnv passes testEmbedder to the serve a test starts (as this test binary).
const testEmbedderEnv = "MEKO_TEST_EMBEDDER"

// hungServe stands in for a serve that takes the lock and writes its pid but never answers;
// "hang-ignore-term" also ignores SIGTERM, so only SIGKILL stops it.
func hungServe(mode string) {
	dir := ""
	for i, a := range os.Args {
		if a == "--data-dir" && i+1 < len(os.Args) {
			dir = os.Args[i+1]
		}
	}
	l := paths.New(dir)
	if mode == "hang-ignore-term" || mode == "hang-with-child" {
		signal.Ignore(syscall.SIGTERM)
	}
	if err := l.Ensure(); err != nil {
		os.Exit(2)
	}
	if mode == "hang-with-child" { // a child in its own process group, recorded as serve records the engine
		child := exec.Command("sleep", "3600")
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		rec, err := supervisor.Record(child.Process.Pid)
		if err != nil {
			os.Exit(2)
		}
		if err := paths.WriteFile(filepath.Join(l.ChildrenDir, "engine"), []byte(rec)); err != nil {
			os.Exit(2)
		}
	}
	lock, err := acquireLock(l.Lock)
	if err != nil {
		os.Exit(2)
	}
	if err := paths.WriteFile(l.Pid, []byte(strconv.Itoa(os.Getpid())+"\n")); err != nil {
		os.Exit(2)
	}
	time.Sleep(time.Hour)
	releaseLock(lock) // keeps the handle (and so the flock) alive through the sleep
	os.Exit(0)
}

// shorten sets a timeout for one test.
func shorten(t *testing.T, v *time.Duration, d time.Duration) {
	t.Helper()
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return tcpPort(t, ln)
}

func tcpPort(t *testing.T, ln net.Listener) int {
	t.Helper()
	a, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("not a TCP address: %v", ln.Addr())
	}
	return a.Port
}

func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func want(t *testing.T, what string, code, wantCode int, text, wantText string) {
	t.Helper()
	if code != wantCode || !strings.Contains(text, wantText) {
		t.Fatalf("%s: exit %d (want %d), output %q (want %q)", what, code, wantCode, text, wantText)
	}
}

// shortDir is a temp folder with a short path: the data folder's unix sockets must fit the OS's
// limit, which t.TempDir() can pass on macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// setup gives a test its own home and data folder, and a free port.
func setup(t *testing.T) (dir, port string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir = shortDir(t)
	port = strconv.Itoa(freePort(t))
	t.Cleanup(func() { run("stop", "--data-dir", dir) })
	return dir, port
}

func TestStartStatusStop(t *testing.T) {
	dir, port := setup(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "Meko is running at http://127.0.0.1:"+port)

	code, out, _ = run("start", "--data-dir", dir, "--port", port)
	want(t, "second start", code, 0, out, "already running")

	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status", code, 0, out, "Children: none")
	want(t, "status's auth", code, 0, out, "Auth:     token")

	code, _, errs = run("serve", "--data-dir", dir, "--port", strconv.Itoa(freePort(t)))
	want(t, "second serve", code, 1, errs, "already running for "+dir)

	code, out, _ = run("stop", "--data-dir", dir)
	want(t, "stop", code, 0, out, "Meko stopped.")

	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status when stopped", code, exitNotRunning, out, "isn't running")

	code, out, _ = run("stop", "--data-dir", dir)
	want(t, "stop when stopped", code, 0, out, "isn't running")

	l := paths.New(dir)
	for _, p := range []string{l.Pid, l.Port, l.Token} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind", p)
		}
	}
}

func TestClientTokenIsKeptAcrossRestarts(t *testing.T) {
	dir, port := setup(t)
	code, tok, errs := run("token", "--data-dir", dir) // before any server: made now
	want(t, "token", code, 0, tok+errs, "")
	if len(strings.TrimSpace(tok)) != 64 {
		t.Fatalf("token %q", tok)
	}
	for i := 0; i < 2; i++ {
		code, out, errs := run("start", "--data-dir", dir, "--port", port)
		want(t, "start", code, 0, out+errs, "is running")
		_, again, _ := run("token", "--data-dir", dir)
		if again != tok {
			t.Fatalf("start %d: token %q, want %q", i, again, tok)
		}
		code, out, _ = run("stop", "--data-dir", dir)
		want(t, "stop", code, 0, out, "stopped")
	}
	if _, err := os.Stat(paths.New(dir).ClientToken); err != nil {
		t.Errorf("stop must keep the client token: %v", err)
	}
}

func TestStartRefusesAnUnknownAuthMode(t *testing.T) {
	dir, port := setup(t)
	t.Setenv("MEKO_AUTH", "off")
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 1, out+errs, `server.auth "off"`)
}

func TestStatusWarnsWhenAuthIsOff(t *testing.T) {
	dir, port := setup(t)
	t.Setenv("MEKO_AUTH", "none")
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status", code, 0, out, "Auth:     none (every user and program on this machine")
}

func TestStalePidFileDoesntBlockStart(t *testing.T) {
	dir, port := setup(t)
	l := paths.New(dir)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{l.Pid: "999999\n", l.Port: "1\n"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start over stale files", code, 0, out+errs, "running at")
	if pid, err := readInt(l.Pid); err != nil || pid == 999999 || !strings.Contains(out, "pid "+strconv.Itoa(pid)) {
		t.Errorf("pid file holds %d (%v); start said %q", pid, err, out)
	}
}

func TestStartReportsTakenPort(t *testing.T) {
	dir, _ := setup(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := strconv.Itoa(tcpPort(t, ln))
	code, _, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start on a taken port", code, 1, errs, "port "+port+" is already in use")
}

func TestServeShutsDownCleanly(t *testing.T) {
	dir, port := setup(t)
	l := paths.New(dir)
	p, _ := strconv.Atoi(port)
	cfg := config.Default()
	cfg.Server.Port = p
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ServeOptions{Layout: l, Config: cfg}) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(l.Port); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve didn't write its port file")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{l.Pid, l.Port, l.Token} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s left behind", f)
		}
	}
	if fi, err := os.Stat(l.DB + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("WAL not checkpointed: %d bytes", fi.Size())
	}
}

func TestUsage(t *testing.T) {
	code, _, errs := run("frobnicate")
	want(t, "unknown command", code, exitUsage, errs, `unknown command "frobnicate"`)
	code, out, _ := run()
	want(t, "no command", code, 0, out, "Usage:")
	code, out, _ = run("version")
	want(t, "version", code, 0, out, "sqlite-vec  v0.1.9")
}

// A start whose server never answers stops it, with SIGKILL if SIGTERM isn't enough, and leaves
// the lock free for a retry.
func TestStartStopsAServerThatNeverAnswers(t *testing.T) {
	dir, port := setup(t)
	t.Setenv("MEKO_TEST_SERVE", "hang-ignore-term")
	shorten(t, &startTimeout, time.Second)
	shorten(t, &stopTimeout, time.Second)
	code, _, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start of a hung server", code, 1, errs, "didn't answer within 1s, so it was stopped")
	if up, err := running(paths.New(dir).Lock); err != nil || up {
		t.Fatalf("lock still held after start returned: %v %v", up, err)
	}
}

// stop kills a server that ignores SIGTERM, then clears its run files.
func TestStopKillsAServerThatIgnoresSIGTERM(t *testing.T) {
	dir, _ := setup(t)
	l := paths.New(dir)
	t.Setenv("MEKO_TEST_SERVE", "hang-ignore-term")
	shorten(t, &stopTimeout, time.Second)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "serve", "--data-dir", dir)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := readInt(l.Pid); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hung serve never wrote its pid")
		}
	}
	code, out, errs := run("stop", "--data-dir", dir)
	want(t, "stop of a server ignoring SIGTERM", code, 0, out+errs, "didn't stop within 1s, so it was killed")
	if _, err := os.Stat(l.Pid); !os.IsNotExist(err) {
		t.Errorf("pid file left behind: %v", err)
	}
}

// stop's SIGKILL path also kills the children a hung server recorded, in their own groups.
// startHungServe starts a serve that hangs after recording a child in its own process group, as
// serve records the engine, and returns it and the child's pid.
func startHungServe(t *testing.T, dir string) (*exec.Cmd, int) {
	t.Helper()
	l := paths.New(dir)
	t.Setenv("MEKO_TEST_SERVE", "hang-with-child")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "serve", "--data-dir", dir)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	var child int
	for deadline := time.Now().Add(10 * time.Second); child == 0; time.Sleep(50 * time.Millisecond) {
		if b, err := os.ReadFile(filepath.Join(l.ChildrenDir, "engine")); err == nil {
			_, _ = fmt.Sscanf(string(b), "%d", &child)
		}
		if time.Now().After(deadline) {
			t.Fatal("the hung serve never recorded its child")
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(-child, syscall.SIGKILL) })
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if up, _ := running(l.Lock); up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hung serve never took the lock")
		}
	}
	t.Setenv("MEKO_TEST_SERVE", "")
	return cmd, child
}

// waitGone waits for the child's process to end, and checks its record is gone.
func waitGone(t *testing.T, dir string, child int, after string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(child, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the child outlived %s", after)
		}
	}
	if _, err := os.Stat(filepath.Join(paths.New(dir).ChildrenDir, "engine")); !os.IsNotExist(err) {
		t.Errorf("the child's record outlived %s: %v", after, err)
	}
}

func TestStopKillsAHungServersChildren(t *testing.T) {
	dir, _ := setup(t)
	shorten(t, &stopTimeout, time.Second)
	_, child := startHungServe(t, dir)
	code, out, errs := run("stop", "--data-dir", dir)
	want(t, "stop", code, 0, out+errs, "so it was killed")
	waitGone(t, dir, child, "stop")
}

// A serve killed from outside leaves its child: stop, finding no server, kills it.
func TestStopKillsAKilledServersChildren(t *testing.T) {
	dir, _ := setup(t)
	serve, child := startHungServe(t, dir)
	_ = serve.Process.Kill()
	if !waitUnlocked(paths.New(dir).Lock, 5*time.Second) {
		t.Fatal("the killed serve's lock wasn't released")
	}
	code, out, errs := run("stop", "--data-dir", dir)
	want(t, "stop", code, 0, out+errs, "isn't running")
	waitGone(t, dir, child, "stop")
}

// A serve killed from outside leaves its child: the next serve kills it before starting its own.
func TestServeKillsAKilledServersChildren(t *testing.T) {
	dir, port := setup(t)
	serve, child := startHungServe(t, dir)
	_ = serve.Process.Kill()
	if !waitUnlocked(paths.New(dir).Lock, 5*time.Second) {
		t.Fatal("the killed serve's lock wasn't released")
	}
	t.Setenv("MEKO_ENGINE_DIR", t.TempDir()) // no engine, so no new record
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "Meko is running")
	waitGone(t, dir, child, "the next serve's start")
}

// serve starts the engine (with the test embedder), status reports it ready, and stop stops it.
func TestServeRunsTheEngine(t *testing.T) {
	engineDir, err := filepath.Abs(filepath.Join("..", "..", "engine"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(engineDir, "dist", "main.js")); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal(err)
		}
		t.Skip("the engine isn't built (make test builds it)")
	}
	dir, port := setup(t)
	t.Setenv("MEKO_ENGINE_DIR", engineDir)
	t.Setenv(testEmbedderEnv, "hash")
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "Meko is running")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		_, out, _ = run("status", "--data-dir", dir)
		if strings.Contains(out, "Engine:   ready, test hash (384 dims)") && strings.Contains(out, "engine running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the engine never got ready:\n%s", out)
		}
	}
	l := paths.New(dir)
	b, err := os.ReadFile(filepath.Join(l.ChildrenDir, "engine"))
	if err != nil {
		t.Fatal(err)
	}
	var enginePid int
	_, _ = fmt.Sscanf(string(b), "%d", &enginePid)
	code, out, _ = run("stop", "--data-dir", dir)
	want(t, "stop", code, 0, out, "Meko stopped.")
	if enginePid == 0 || syscall.Kill(enginePid, 0) == nil {
		t.Errorf("the engine (pid %d) outlived stop", enginePid)
	}
	for _, p := range []string{l.StoreSock, l.EngineSock, filepath.Join(l.ChildrenDir, "engine")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind", p)
		}
	}
}
