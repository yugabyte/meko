package cli

import (
	"bytes"
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko-local/backend/internal/config"
	"github.com/yugabyte/meko-local/backend/internal/paths"
)

// TestMain doubles as the meko-local binary: start re-runs os.Executable() (this test binary)
// as "serve ...".
func TestMain(m *testing.M) {
	for _, k := range []string{paths.HomeEnv, "MEKO_LOCAL_PORT", "MEKO_LOCAL_LOG_LEVEL"} {
		_ = os.Unsetenv(k)
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
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

// setup gives a test its own home and data folder, and a free port.
func setup(t *testing.T) (dir, port string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir = t.TempDir()
	port = strconv.Itoa(freePort(t))
	t.Cleanup(func() { run("stop", "--data-dir", dir) })
	return dir, port
}

func TestStartStatusStop(t *testing.T) {
	dir, port := setup(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "Meko Local is running at http://127.0.0.1:"+port)

	code, out, _ = run("start", "--data-dir", dir, "--port", port)
	want(t, "second start", code, 0, out, "already running")

	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status", code, 0, out, "Children: none")

	code, _, errs = run("serve", "--data-dir", dir, "--port", strconv.Itoa(freePort(t)))
	want(t, "second serve", code, 1, errs, "already running for "+dir)

	code, out, _ = run("stop", "--data-dir", dir)
	want(t, "stop", code, 0, out, "Meko Local stopped.")

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
