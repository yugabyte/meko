package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/agents"
	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/supervisor"
	"github.com/yugabyte/meko/backend/internal/update"
)

// TestMain doubles as the meko binary: start re-runs os.Executable() (this test binary)
// as "serve ...".
func TestMain(m *testing.M) {
	for _, k := range []string{paths.HomeEnv, "MEKO_PORT", "MEKO_LOG_LEVEL"} {
		_ = os.Unsetenv(k)
	}
	// Never the login Keychain: the tests and the servers they start keep secrets in files.
	_ = os.Setenv("MEKO_SECRETS", "file")
	// Never this computer's own agents: only the commands on a test's PATH and in its home, and their
	// configs in its home.
	agents.SystemFolders = nil
	agents.Applications = ""
	agents.LoginShell = ""
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "KIRO_HOME", "XDG_CONFIG_HOME"} {
		_ = os.Unsetenv(k)
	}
	// meko _launch, which the launch tests run as this binary, with a short HealthyAfter.
	if len(os.Args) > 1 && os.Args[1] == "_launch" {
		if d, err := time.ParseDuration(os.Getenv(testHealthyAfterEnv)); err == nil {
			update.HealthyAfter = d
		}
		os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if mode := os.Getenv("MEKO_TEST_SERVE"); mode != "" {
			hungServe(mode)
		}
		testEmbedder = os.Getenv(testEmbedderEnv)
		testCallHomeFirstCheck, _ = time.ParseDuration(os.Getenv(testCallHomeFirstCheckEnv))
		os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	// Only in the tests' own process: a test sets them for the serve it starts.
	for _, k := range []string{"MEKO_AUTH", "MEKO_CALL_HOME", "MEKO_CALL_HOME_URL", "YB_DISABLE_CALLHOME",
		"MEKO_WEB_ACCESS"} {
		_ = os.Unsetenv(k)
	}
	// Never the web listener's real port (AgentK's, 7777): setup gives each test a free one, and this
	// covers the tests that don't call it.
	if ln, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
		if a, ok := ln.Addr().(*net.TCPAddr); ok {
			_ = os.Setenv("MEKO_WEB_PORT", strconv.Itoa(a.Port))
		}
		_ = ln.Close()
	}
	os.Exit(m.Run())
}

// testEmbedderEnv passes testEmbedder to the serve a test starts (as this test binary).
const testEmbedderEnv = "MEKO_TEST_EMBEDDER"

// testHealthyAfterEnv is update.HealthyAfter for meko _launch run as the test binary.
const testHealthyAfterEnv = "MEKO_TEST_HEALTHY_AFTER"

// testCallHomeFirstCheckEnv passes testCallHomeFirstCheck to the serve a test starts.
const testCallHomeFirstCheckEnv = "MEKO_TEST_CALL_HOME_FIRST_CHECK"

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
	if mode == "hang-with-child" { // a child in its own process group, recorded as serve records the memory engine
		child := exec.Command("sleep", "3600")
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		rec, err := supervisor.Record(child.Process.Pid)
		if err != nil {
			os.Exit(2)
		}
		if err := paths.WriteFile(filepath.Join(l.ChildrenDir, "memory_engine"), []byte(rec)); err != nil {
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

// runSh runs meko-cli's command in args.
func runSh(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := RunMekoCLI(args, &out, &errb)
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

// setup gives a test its own home and data folder, and a free port. Its cleanup stops only the
// server: cleanups run last first, so by then a test's fake extension is gone and PATH is restored
// (noRealExtensions), and stopping the extensions would find and stop this machine's real AgentK.
func setup(t *testing.T) (dir, port string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir = shortDir(t)
	port = strconv.Itoa(freePort(t))
	t.Setenv("MEKO_WEB_PORT", strconv.Itoa(freePort(t)))
	t.Cleanup(func() { run("stop", "--data-dir", dir, "--keep-extensions") })
	return dir, port
}

// status runs the extensions' status commands at once, and one that doesn't answer in time is a
// warning that names the command, while the rest of status still prints.
func TestStatusAsksTheExtensionsAtOnce(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "  ✓ Server          running at http://127.0.0.1:"+port)
	old := extensions.StatusTimeout
	extensions.StatusTimeout = 2 * time.Second
	t.Cleanup(func() { extensions.StatusTimeout = old })
	// Registered after the start, so the server's handoff never runs them.
	bin := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // G306: test script
			t.Fatal(err)
		}
		return p
	}
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: script("agentk", "sleep 1\necho 'AgentK 1.0'\n")}
	reg.Entries["presenter"] = extensions.Entry{Command: script("presenter", "echo 'Presenter 1.0'\nsleep 30\n")}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}

	begun := time.Now()
	code, out, _ = run("status", "--data-dir", dir)
	// One after another, they'd take 1 s and then 2 s plus the hung one's pipe wait.
	if took := time.Since(begun); took > 4*time.Second {
		t.Errorf("status took %s", took)
	}
	want(t, "status", code, 0, out, "  ✓ AgentK          running, AgentK 1.0\n"+
		"  ! Presenter       didn't answer in 2s (meko_presenter status)\n")
	want(t, "status's links", code, 0, out, "  Agents (MCP, API)   http://127.0.0.1:"+port+"/mcp\n")

	code, out, _ = run("status", "--data-dir", dir, "--verbose")
	want(t, "status --verbose", code, 0, out, "  Extension:     agentk, running (AgentK 1.0)\n"+
		"  Extension:     presenter, didn't answer in 2s (meko_presenter status)\n")
}

func TestStartStatusStop(t *testing.T) {
	dir, port := setup(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "  ✓ Server          running at http://127.0.0.1:"+port)

	code, out, _ = run("start", "--data-dir", dir, "--port", port)
	want(t, "second start", code, 0, out, "already running")

	code, out, _ = run("status", "--data-dir", dir, "--verbose")
	want(t, "status", code, 0, out, "Children:      none")
	want(t, "status's auth", code, 0, out, "Auth:          token")
	want(t, "status's service", code, 0, out, "Service:       none (meko install sets one up)")
	want(t, "status's updates", code, 0, out, "Updates:       none (a build from source)")
	want(t, "status's usage stats", code, 0, out, "Usage stats:   on, but a build from source sends none")

	code, _, errs = run("serve", "--data-dir", dir, "--port", strconv.Itoa(freePort(t)))
	want(t, "second serve", code, exitLocked, errs, "already running for "+dir)

	code, out, _ = run("status", "--data-dir", dir)
	want(t, "short status", code, 0, out, " is running (pid ")
	want(t, "short status's service", code, 0, out, "  ! Service         none (meko install sets one up)\n")
	want(t, "short status's link for agents", code, 0, out, "  Agents (MCP, API)   http://127.0.0.1:"+port+"/mcp\n")
	want(t, "short status's link for people", code, 0, out, "  Humans (web UI)     http://127.0.0.1:")
	want(t, "short status's way to more", code, 0, out, "More detail: meko status --verbose\n")
	code, out, _ = run("status", "--data-dir", dir, "--json")
	var reply struct {
		Running bool `json:"running"`
		Port    int  `json:"port"`
	}
	if err := json.Unmarshal([]byte(out), &reply); code != 0 || err != nil || !reply.Running ||
		strconv.Itoa(reply.Port) != port {
		t.Errorf("status --json: %d %v %q", code, err, out)
	}

	code, out, _ = run("stop", "--data-dir", dir)
	want(t, "stop", code, 0, out, "Stopping Meko…\n  ✓ Server          stopped\n")

	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status when stopped", code, exitNotRunning, out,
		"✗ Meko isn't running (data in "+dir+")\n  Start it: meko start\n")
	code, out, _ = run("status", "--data-dir", dir, "--json")
	want(t, "status --json when stopped", code, exitNotRunning, out, `{"data_dir":"`+dir+`","running":false}`)

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
	// A killed server's port file is left behind; with no server, agent connect names the configured port.
	if err := paths.WriteFile(paths.New(dir).Port, []byte("1\n")); err != nil {
		t.Fatal(err)
	}
	code, mcpOut, _ := run("agent", "connect", "--data-dir", dir)
	want(t, "agent connect with no server", code, 0, mcpOut, "http://127.0.0.1:7778/mcp")
	code, js, _ := run("token", "--data-dir", dir, "--json")
	want(t, "token --json with no server", code, 0, js, `{"cloud":{"set":false,"url":"","api_key":"","web_url":"",`+
		`"api_url":""},"running":false,"token":"`+strings.TrimSpace(tok)+
		`","url":"http://127.0.0.1:7778/mcp","web_url":"http://127.0.0.1:`+os.Getenv("MEKO_WEB_PORT")+`"}`)
	for i := 0; i < 2; i++ {
		started, out, serrs := run("start", "--data-dir", dir, "--port", port)
		want(t, "start", started, 0, out+serrs, "is running")
		_, again, _ := run("token", "--data-dir", dir)
		if again != tok {
			t.Fatalf("start %d: token %q, want %q", i, again, tok)
		}
		_, js, _ = run("token", "--data-dir", dir, "--json")
		want(t, "token --json with a server", 0, 0, js, `"running":true,"token":"`+strings.TrimSpace(tok)+
			`","url":"http://127.0.0.1:`+port+`/mcp"`)
		stopped, sout, _ := run("stop", "--data-dir", dir)
		want(t, "stop", stopped, 0, sout, "stopped")
	}
	if _, err := os.Stat(paths.New(dir).ClientToken); err != nil {
		t.Errorf("stop must keep the client token: %v", err)
	}

	// Deleted while a server runs, it isn't made again: the server keeps the token it loaded.
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	if err := os.Remove(paths.New(dir).ClientToken); err != nil {
		t.Fatal(err)
	}
	code, _, errs = run("token", "--data-dir", dir)
	want(t, "token with the file gone", code, 1, errs, "Restart Meko")
	if _, err := os.Stat(paths.New(dir).ClientToken); !os.IsNotExist(err) {
		t.Fatalf("token made a new file while the server runs: %v", err)
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
	code, out, _ = run("status", "--data-dir", dir, "--verbose")
	want(t, "status", code, 0, out, "Auth:          none (every user and program on this machine")
}

func TestUsageStatsGoToTheConfiguredURL(t *testing.T) {
	dir, port := setup(t)
	got := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- b
	}))
	defer srv.Close()
	t.Setenv("MEKO_CALL_HOME_URL", srv.URL+"/")
	t.Setenv(testCallHomeFirstCheckEnv, "100ms")
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	code, out, _ = run("status", "--data-dir", dir, "--verbose")
	want(t, "status", code, 0, out, "Usage stats:   on (sent to "+srv.URL+"/)")
	select {
	case b := <-got:
		var e struct {
			CallhomeType string `json:"callhome_type"`
			InstallID    string `json:"install_id"`
			Payload      struct {
				Install struct {
					Version string `json:"version"`
				} `json:"install"`
				Days []json.RawMessage `json:"days"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(b, &e); err != nil || e.CallhomeType != "meko" || e.InstallID == "" ||
			e.Payload.Install.Version != "dev" || len(e.Payload.Days) != 30 {
			t.Errorf("heartbeat %s: %+v %v", b, e, err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no heartbeat")
	}
}

func TestStatusSaysWhatTurnedUsageStatsOff(t *testing.T) {
	dir, port := setup(t)
	t.Setenv("YB_DISABLE_CALLHOME", "true")
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	code, out, _ = run("status", "--data-dir", dir, "--verbose")
	want(t, "status", code, 0, out, "Usage stats:   off (turned off by YB_DISABLE_CALLHOME)")
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
	cfg.Secrets.Store = "file" // never the login Keychain, as MEKO_SECRETS=file does for the other tests
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
// serve records the memory engine, and returns it and the child's pid.
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
		if b, err := os.ReadFile(filepath.Join(l.ChildrenDir, "memory_engine")); err == nil {
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
	if _, err := os.Stat(filepath.Join(paths.New(dir).ChildrenDir, "memory_engine")); !os.IsNotExist(err) {
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
	t.Setenv("MEKO_MEMORY_ENGINE_DIR", t.TempDir()) // no memory engine, so no new record
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	waitGone(t, dir, child, "the next serve's start")
}

// serve starts the memory engine (with the test embedder), status reports it ready, and stop stops it.
func TestServeRunsTheMemoryEngine(t *testing.T) {
	memoryEngineDir, err := filepath.Abs(filepath.Join("..", "..", "memory_engine"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(memoryEngineDir, "dist", "main.js")); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal(err)
		}
		t.Skip("the memory engine isn't built (make test builds it)")
	}
	dir, port := setup(t)
	t.Setenv("MEKO_MEMORY_ENGINE_DIR", memoryEngineDir)
	t.Setenv(testEmbedderEnv, "hash")
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		_, out, _ = run("status", "--data-dir", dir, "--verbose")
		if strings.Contains(out, "Memory engine: ready, test hash (384 dims)") &&
			strings.Contains(out, "memory_engine running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the memory engine never got ready:\n%s", out)
		}
	}
	l := paths.New(dir)
	b, err := os.ReadFile(filepath.Join(l.ChildrenDir, "memory_engine"))
	if err != nil {
		t.Fatal(err)
	}
	var memoryEnginePid int
	_, _ = fmt.Sscanf(string(b), "%d", &memoryEnginePid)
	code, out, _ = run("stop", "--data-dir", dir)
	want(t, "stop", code, 0, out, "  ✓ Server          stopped\n")
	if memoryEnginePid == 0 || syscall.Kill(memoryEnginePid, 0) == nil {
		t.Errorf("the memory engine (pid %d) outlived stop", memoryEnginePid)
	}
	for _, p := range []string{l.StoreSock, l.MemoryEngineSock, filepath.Join(l.ChildrenDir, "memory_engine")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind", p)
		}
	}
}

// withMemoryEngine has the servers a test starts run the built memory engine, with the hashing
// embedder; it skips the test when the memory engine isn't built (outside CI).
func withMemoryEngine(t *testing.T) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "memory_engine"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, "dist", "main.js")); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal(err)
		}
		t.Skip("the memory engine isn't built (make test builds it)")
	}
	t.Setenv("MEKO_MEMORY_ENGINE_DIR", dir)
	t.Setenv(testEmbedderEnv, "hash")
}

// startReady starts a server and waits until its memory engine is ready.
func startReady(t *testing.T, dir, port string) {
	t.Helper()
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, out, _ = run("status", "--data-dir", dir, "--verbose"); strings.Contains(out, "Memory engine: ready") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the memory engine never got ready:\n%s", out)
		}
	}
}

func TestSearchAddAndDatapacks(t *testing.T) {
	withMemoryEngine(t)
	t.Setenv("MEKO_SECRETS", "file") // a name missing here reads the Meko Cloud link
	dir, port := setup(t)
	code, _, errs := runSh("search", "--data-dir", dir, "anything")
	want(t, "search with no server", code, exitNotRunning, errs, "isn't running")
	withStdin(t, "datapacks\n")
	code, _, errs = runSh("--data-dir", dir)
	want(t, "the prompt with no server", code, exitNotRunning, errs,
		"Meko isn't running (data in "+dir+"). Start it: meko start")

	startReady(t, dir, port)
	var out string
	var err error

	code, out, errs = runSh("datapacks", "--data-dir", dir)
	want(t, "datapacks", code, 0, out+errs, "default   Created by Meko on first start.\n")
	const note = "the build cache is in memory_engine/.models"
	code, out, errs = runSh("add_memory", "--data-dir", dir, note, "--datapack", "default")
	want(t, "add_memory", code, 0, out+errs, "Added memory ")
	code, out, errs = runSh("search", "--data-dir", dir, note, "--limit", "3")
	want(t, "search", code, 0, out+errs, note+"  (meko_cli, ")
	code, out, errs = runSh("search", "--data-dir", dir, "x", "--json", "--source", "memory")
	want(t, "search --json", code, 0, out+errs, `"knowledge_base":null`)
	code, _, errs = runSh("add_memory", "--data-dir", dir, "x", "--datapack", "nope")
	want(t, "add_memory to a missing datapack", code, 1, errs, `no datapack is named "nope"`)

	// A datapack made, used by name, and deleted only when the answer is yes.
	code, out, errs = runSh("create_datapack", "--data-dir", dir, "scratch-notes")
	want(t, "create_datapack", code, 0, out+errs, "Created datapack scratch-notes (")
	code, out, errs = runSh("create_datapack", "--data-dir", dir, "scratch-notes")
	want(t, "create_datapack twice", code, 1, out+errs, "CONFLICT")
	code, out, errs = runSh("add_memory", "--data-dir", dir, "kept in scratch", "--datapack", "scratch-notes")
	want(t, "add_memory by name", code, 0, out+errs, "Added memory ")
	withStdin(t, "n\n")
	code, out, errs = runSh("delete_datapack", "--data-dir", dir, "scratch-notes")
	want(t, "delete_datapack answered n", code, 1, errs, "Nothing was deleted\n")
	want(t, "delete_datapack's question", code, 1, out, "This deletes datapack scratch-notes and everything "+
		"in it (memories, conversations, artifacts) for good\nDelete it? [y/N] ")
	if _, out, _ = runSh("datapacks", "--data-dir", dir); !strings.Contains(out, "scratch-notes") {
		t.Errorf("a declined delete_datapack deleted it:\n%s", out)
	}
	withStdin(t, "y\n")
	code, out, errs = runSh("delete_datapack", "--data-dir", dir, "scratch-notes")
	want(t, "delete_datapack", code, 0, out+errs, "Deleted datapack scratch-notes\n")
	if _, out, _ = runSh("datapacks", "--data-dir", dir); strings.Contains(out, "scratch-notes") {
		t.Errorf("the deleted datapack is still listed:\n%s", out)
	}

	// meko-cli's prompt, run as a script: use fills in --datapack, delete_datapack's question takes the
	// next line, a failing line doesn't stop it but sets the exit code, and exit ends it.
	withStdin(t, "create_datapack prompt-notes\nuse prompt-notes\nadd_memory \"kept by the prompt\"\n"+
		"search \"kept by the prompt\" --source memory --json\nadd_memory\nsearch \"unclosed\nuse nosuch\nuse\n"+
		"# a comment\nhelp\n\ndelete_datapack prompt-notes\ny\nsearch \"kept by the prompt\" --source memory --json\n"+
		"exit\ncreate_datapack after-exit\n")
	code, out, errs = runSh("--data-dir", dir)
	want(t, "the prompt's use", code, exitError, out, "Using datapack prompt-notes\nAdded memory ")
	want(t, "the prompt's search in the datapack it uses", code, exitError, out, "kept by the prompt")
	want(t, "the prompt's use, alone", code, exitError, out, "\nUsing datapack prompt-notes\n")
	want(t, "the prompt's help, with --data-dir", code, exitError, out, "Usage: meko-cli [<command> [flags]]")
	want(t, "the prompt's delete_datapack of the datapack in use", code, exitError, out,
		"Delete it? [y/N] \nDeleted datapack prompt-notes\nUsing the default datapack\n")
	want(t, "add_memory at the prompt", code, exitError, errs, "meko-cli: give the memory's text: add_memory \"<text>\"\n")
	want(t, "an unclosed quote", code, exitError, errs, "meko-cli: the line has an unclosed \" quote\n")
	want(t, "use of a missing datapack", code, exitError, errs, `meko-cli: no datapack is named "nosuch"`)
	if strings.Contains(errs, `"prompt-notes"`) || strings.Contains(errs, "unknown command") {
		t.Errorf("a line after the datapack in use was deleted, or help, failed:\n%s", errs)
	}
	if _, out, _ = runSh("datapacks", "--data-dir", dir); strings.Contains(out, "prompt-notes") ||
		strings.Contains(out, "after-exit") {
		t.Errorf("the prompt's datapack wasn't deleted, or a line after exit ran:\n%s", out)
	}

	// upload adds files to the knowledge base and waits until they're indexed.
	notes := filepath.Join(t.TempDir(), "notes.md")
	builds := []byte("# Builds\n\nThe nightly build runs at two in the morning.\n")
	if err = os.WriteFile(notes, builds, 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad.pdf")
	if err = os.WriteFile(bad, []byte("not a pdf at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs = runSh("upload", "--data-dir", dir, notes, "--datapack", "default")
	want(t, "upload", code, 0, out+errs, notes+": indexed, 1 chunk (key ")
	code, out, errs = runSh("upload", "--data-dir", dir, notes)
	want(t, "upload again", code, 0, out+errs, "already in the knowledge base as notes.md")
	want(t, "upload again waits for it", code, 0, out, notes+": indexed, 1 chunk (key ")
	code, out, errs = runSh("upload", "--data-dir", dir, bad, notes)
	want(t, "upload a bad file", code, 1, out+errs, "doesn't look like a PDF")
	want(t, "upload a bad file", code, 1, errs, "1 of 2 files weren't added")
	code, out, errs = runSh("search", "--data-dir", dir, "when does the nightly build run", "--source", "knowledge_base")
	want(t, "search the knowledge base", code, 0, out+errs, "Knowledge base (relevance):")
	code, out, errs = runSh("upload", "--data-dir", dir, notes, "--json")
	want(t, "upload --json", code, 0, out+errs, `"already_uploaded":true`)
	var offer struct{ Key string }
	// --json prints the tool's reply, then the file's final status.
	if err = json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]), &offer); err != nil {
		t.Fatalf("upload --json: %v: %s", err, out)
	}
	code, out, errs = runSh("upload_status", "--data-dir", dir, offer.Key)
	want(t, "upload_status", code, 0, out+errs, offer.Key+": completed. The file is indexed and searchable.")
	code, _, errs = runSh("upload_status", "--data-dir", dir, "no-such-key")
	want(t, "upload_status of an unknown key", code, 1, errs, "NOT_FOUND")

	_, tok, _ := run("token", "--data-dir", dir)
	code, out, _ = run("agent", "connect", "--data-dir", dir)
	want(t, "agent connect", code, 0, out, "http://127.0.0.1:"+port+"/mcp\n")
	want(t, "agent connect", code, 0, out, "Authorization: Bearer "+strings.TrimSpace(tok)+"\n")

	// The default datapack isn't made again, so deleting it says so, and calls that name none then
	// ask for --datapack.
	withStdin(t, "")
	code, out, errs = runSh("delete_datapack", "--data-dir", dir, "default")
	want(t, "delete_datapack default with no answer", code, 1, out+errs, "It's the default datapack")
	want(t, "delete_datapack default with no answer", code, 1, errs, "Nothing was deleted\n")
	code, out, errs = runSh("delete_datapack", "--data-dir", dir, "default", "--force")
	want(t, "delete_datapack default --force", code, 0, out+errs, "Deleted datapack default\n")
	if strings.Contains(out, "Delete it?") {
		t.Errorf("delete_datapack --force asked:\n%s", out)
	}
	code, _, errs = runSh("add_memory", "--data-dir", dir, "x")
	want(t, "add_memory with no default", code, 1, errs, "there's no default datapack (it was deleted): pass --datapack")
	if code, out, errs = runSh("datapacks", "--data-dir", dir); code != 0 || out != "No datapacks\n" {
		t.Errorf("datapacks with none left, unlinked: %d\n%s%s", code, out, errs)
	}
}

// withStdin makes text what the commands read as stdin, for one test.
func withStdin(t *testing.T, text string) {
	t.Helper()
	old := stdin
	stdin = strings.NewReader(text)
	t.Cleanup(func() { stdin = old })
}

// gone checks that a destroyed data folder and its renamed copy are both gone.
func gone(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("%s is still there: %v", dir, err)
	}
	if left, _ := filepath.Glob(dir + ".destroying-*"); len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
}

// destroy asks first and removes nothing unless told yes; told yes, it stops the server and
// removes the folder, and a start afterwards makes a new one.
func TestDestroyAsksThenStopsAndRemoves(t *testing.T) {
	dir, port := setup(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")

	for _, answer := range []string{"n\n", "", "whatever\n"} {
		withStdin(t, answer)
		code, out, errs = run("destroy", "--data-dir", dir)
		want(t, "destroy answered "+strconv.Quote(answer), code, 1, errs, "Nothing was removed\n")
		want(t, "destroy's question", code, 1, out, "Meko is running, so it's stopped first\nDestroy it? [y/N] ")
		want(t, "destroy's summary", code, 1, out, "This removes Meko's data in "+dir+" (")
		code, out, _ = run("status", "--data-dir", dir)
		want(t, "status after a declined destroy", code, 0, out, " is running (pid ")
	}

	withStdin(t, "Yes\n")
	code, out, errs = run("destroy", "--data-dir", dir)
	// A piped answer isn't echoed, so destroy ends the prompt's line itself.
	want(t, "destroy", code, 0, out+errs, "Destroy it? [y/N] \nMeko stopped\nRemoved "+dir+"\n")
	want(t, "destroy's note", code, 0, out,
		"its token no longer works: after meko start, run meko agent connect <agent> again")
	gone(t, dir)
	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status after destroy", code, exitNotRunning, out, "isn't running")

	code, out, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start after destroy", code, 0, out+errs, "is running")
}

func TestDestroyForce(t *testing.T) {
	dir, _ := setup(t)
	if err := os.WriteFile(paths.New(dir).DB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	withStdin(t, "")
	code, out, errs := run("destroy", "--data-dir", dir, "--force")
	want(t, "destroy --force", code, 0, out+errs, "Removed "+dir+"\n")
	if strings.Contains(out, "Destroy it?") || strings.Contains(out, "Meko stopped") {
		t.Errorf("destroy --force asked, or stopped a server that wasn't running:\n%s", out)
	}
	gone(t, dir)
}

func TestDestroyNothingThere(t *testing.T) {
	dir := filepath.Join(shortDir(t), "none")
	code, out, errs := run("destroy", "--data-dir", dir, "--force")
	want(t, "destroy of no folder", code, 0, out+errs, "Nothing to remove: there's no data folder at "+dir)
	empty := shortDir(t)
	code, out, errs = run("destroy", "--data-dir", empty, "--force")
	want(t, "destroy of an empty folder", code, 0, out+errs, "Nothing to remove: the data folder "+empty+" is empty\n")
	// A link to an empty folder is left too: there's nothing to remove.
	link := filepath.Join(shortDir(t), "link")
	if err := os.Symlink(empty, link); err != nil {
		t.Fatal(err)
	}
	code, out, errs = run("destroy", "--data-dir", link, "--force")
	want(t, "destroy of a link to an empty folder", code, 0, out+errs, "is empty\n")
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the link to an empty folder was removed: %v", err)
	}
}

// destroy removes only a folder that looks like Meko's, and never the home folder.
func TestDestroyRefusesAForeignFolder(t *testing.T) {
	dir := shortDir(t)
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run("destroy", "--data-dir", dir, "--force")
	want(t, "destroy of a foreign folder", code, 1, errs, "doesn't look like Meko's data folder")

	// A mistyped --data-dir on token (and then status) gives the folder Meko's run/meko.lock,
	// client.token and empty folders, which still don't make it Meko's.
	if code, _, errs = run("token", "--data-dir", dir); code != 0 {
		t.Fatalf("token: %s", errs)
	}
	run("status", "--data-dir", dir)
	if _, err := os.Stat(paths.New(dir).Lock); err != nil {
		t.Fatalf("status didn't make the lock file, so this doesn't test it: %v", err)
	}
	code, _, errs = run("destroy", "--data-dir", dir, "--force")
	want(t, "destroy of a folder token and status wrote in", code, 1, errs, "doesn't look like Meko's data folder")

	t.Setenv("HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "meko.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs = run("destroy", "--data-dir", dir, "--force")
	want(t, "destroy of the home folder", code, 1, errs, "doesn't look like Meko's data folder")
	if _, err := os.Stat(notes); err != nil {
		t.Errorf("destroy removed a file it refused to: %v", err)
	}
}

// destroy kills a server that ignores SIGTERM, and its recorded child, before removing the folder.
func TestDestroyKillsAHungServer(t *testing.T) {
	dir, _ := setup(t)
	shorten(t, &stopTimeout, time.Second)
	if err := os.WriteFile(paths.New(dir).DB, nil, 0o600); err != nil { // as a real serve makes
		t.Fatal(err)
	}
	_, child := startHungServe(t, dir)
	code, out, errs := run("destroy", "--data-dir", dir, "--force")
	want(t, "destroy of a hung server", code, 0, out+errs, "so it was killed\nRemoved "+dir+"\n")
	waitGone(t, dir, child, "destroy")
	gone(t, dir)
}

// A symlinked data folder loses the folder it points to, and then the link.
func TestDestroyFollowsASymlink(t *testing.T) {
	target := shortDir(t)
	if err := os.WriteFile(paths.New(target).DB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(shortDir(t), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// destroy names the folder with every symlink resolved, and on macOS /tmp is one (to /private/tmp).
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("destroy", "--data-dir", link, "--force")
	want(t, "destroy through a symlink", code, 0, out+errs, "Removed "+resolved+"\n")
	gone(t, target)
	gone(t, link)
}

// A folder that can't be moved aside (its parent isn't writable) is left whole, not half removed.
func TestDestroyLeavesAFolderItCantMove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes in any folder")
	}
	parent := shortDir(t)
	dir := filepath.Join(parent, "meko")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.New(dir).DB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	code, _, errs := run("destroy", "--data-dir", dir, "--force")
	want(t, "destroy in a read-only parent", code, 1, errs, "Nothing was removed")
	if _, err := os.Stat(paths.New(dir).DB); err != nil {
		t.Errorf("meko.db is gone: %v", err)
	}
}

// When removing the moved folder fails partway, the symlink to it goes anyway: it points at nothing.
func TestDestroyRemovesTheLinkWhenRemovalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files in any folder")
	}
	target := shortDir(t)
	if err := os.WriteFile(paths.New(target).DB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(target, "logs")
	if err := os.Mkdir(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "meko.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		moved, _ := filepath.Glob(target + ".destroying-*")
		for _, m := range moved {
			_ = os.Chmod(filepath.Join(m, "logs"), 0o700)
			_ = os.RemoveAll(m)
		}
	})
	link := filepath.Join(shortDir(t), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run("destroy", "--data-dir", link, "--force")
	want(t, "destroy that fails partway", code, 1, errs, "removing it failed partway")
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the link to the moved folder is still there: %v", err)
	}
}

// withExtensionData makes dir a stopped server's data folder holding AgentK's folder (with a file in
// it) and AgentK registered as an extension, whose program logs its arguments and MEKO_HOME to the
// returned log; uninstall fails when failUninstall.
func withExtensionData(t *testing.T, dir string, failUninstall bool) (log, kept string) {
	t.Helper()
	noRealExtensions(t)
	l := paths.New(dir)
	if err := os.WriteFile(l.DB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.LogDir, 0o700); err != nil {
		t.Fatal(err)
	}
	kept = filepath.Join(extensions.Dir(dir, "agentk"), "state.db")
	if err := os.MkdirAll(filepath.Dir(kept), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept, []byte("sessions"), 0o600); err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(shortDir(t), "agentk")
	log = ext + ".log"
	fails := ""
	if failUninstall {
		fails = "[ \"$1\" = uninstall ] && { echo \"can't uninstall\" >&2; exit 4; }\n"
	}
	script := fmt.Sprintf("#!/bin/sh\necho \"$* $MEKO_HOME\" >> %q\n%sexit 0\n", log, fails)
	if err := os.WriteFile(ext, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test script
		t.Fatal(err)
	}
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: ext, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	return log, kept
}

// With extension data in the data folder, destroy asks whether to remove it too: kept, everything
// else of Meko's goes and the extensions' folder and registry stay; removed, each extension's own
// uninstall runs first (with MEKO_HOME) and then the whole folder goes. --force alone keeps it,
// --with-extensions removes it without the second question.
func TestDestroyWithExtensionData(t *testing.T) {
	for _, c := range []struct {
		name, answers string
		flags         []string
		removed       bool
		asked         []string
	}{
		{"kept when asked", "y\nn\n", nil, false, []string{"Destroy it?", "Also uninstall the extensions"}},
		{"removed when asked", "y\ny\n", nil, true, []string{"Destroy it?", "Also uninstall the extensions"}},
		{"--force keeps it", "", []string{"--force"}, false, nil},
		{"--with-extensions removes it", "y\n", []string{"--with-extensions"}, true, []string{"Destroy it?"}},
		{"both ask nothing", "", []string{"--force", "--with-extensions"}, true, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, _ := setup(t)
			log, kept := withExtensionData(t, dir, false)
			held, err := os.OpenFile(kept, os.O_RDWR, 0) //nolint:gosec // G304: test file
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Close() }()
			withStdin(t, c.answers)
			code, out, errs := run(append([]string{"destroy", "--data-dir", dir}, c.flags...)...)
			want(t, "destroy", code, 0, out+errs, "It also holds extension data:\n  AgentK in "+
				extensions.Dir(dir, "agentk")+" (")
			for _, q := range []string{"Destroy it?", "Also uninstall the extensions"} {
				if asked := strings.Contains(out, q); asked != slices.Contains(c.asked, q) {
					t.Errorf("asked %q: %v, want %v:\n%s", q, asked, !asked, out)
				}
			}
			if c.removed {
				want(t, "the extension's uninstall", 0, 0, readLog(t, log),
					"uninstall --purge --delete-worktrees --non-interactive "+dir+"\n")
				gone(t, dir)
				return
			}
			if got := readLog(t, log); got != "" {
				t.Errorf("the extension ran %q", got)
			}
			want(t, "destroy that keeps them", code, 0, out, "kept the extensions' data in it")
			if _, serr := os.Stat(paths.New(dir).DB); !os.IsNotExist(serr) {
				t.Errorf("meko.db is still there: %v", serr)
			}
			if _, err = held.WriteAt([]byte("SESSIONS"), 0); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(kept) //nolint:gosec // G304: test file
			if err != nil || string(b) != "SESSIONS" {
				t.Errorf("the extension's file: %q, %v", b, err)
			}
			if reg, _ := extensions.Load(dir); reg.Entries["agentk"].Command == "" {
				t.Errorf("the registry lost agentk: %+v", reg.Entries)
			}
			if left, _ := filepath.Glob(dir + ".destroying-*"); len(left) > 0 {
				t.Errorf("left behind: %v", left)
			}
		})
	}
}

// A declined destroy and a failing extension uninstall remove nothing.
func TestDestroyWithExtensionDataRemovesNothingOnNoOrFailure(t *testing.T) {
	dir, _ := setup(t)
	_, kept := withExtensionData(t, dir, true)
	withStdin(t, "n\n")
	code, _, errs := run("destroy", "--data-dir", dir)
	want(t, "destroy declined", code, 1, errs, "Nothing was removed\n")
	code, out, errs := run("destroy", "--data-dir", dir, "--force", "--with-extensions")
	want(t, "destroy with a failing uninstall", code, 1, out+errs,
		"uninstalling AgentK failed: ")
	want(t, "destroy with a failing uninstall", code, 1, errs, "Nothing of Meko's was removed")
	for _, p := range []string{paths.New(dir).DB, kept} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s is gone: %v", p, err)
		}
	}
}

// Keeping the extensions, destroy moves Meko's entries aside holding the lock throughout (run/,
// which holds it, last), and a move that fails puts back what was moved.
func TestDestroyKeepingExtensionsHoldsTheLockAndRollsBack(t *testing.T) {
	dir, _ := setup(t)
	_, kept := withExtensionData(t, dir, false)
	l := paths.New(dir)
	var order []string
	old := rename
	t.Cleanup(func() { rename = old })
	rename = func(from, to string) error {
		if up, _ := running(l.Lock); !up {
			t.Errorf("moving %s with the lock free", from)
		}
		order = append(order, filepath.Base(from))
		if filepath.Base(from) == "logs" && !strings.Contains(from, ".destroying-") {
			return errors.New("injected")
		}
		return old(from, to)
	}
	code, _, errs := run("destroy", "--data-dir", dir, "--force")
	want(t, "destroy whose move fails", code, 1, errs, "Nothing was removed")
	for _, p := range []string{l.DB, l.LogDir, kept, extensions.File(dir)} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s isn't back: %v", p, err)
		}
	}
	if left, _ := filepath.Glob(dir + ".destroying-*"); len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
	order = nil
	rename = func(from, to string) error {
		order = append(order, filepath.Base(from))
		return old(from, to)
	}
	code, out, errs := run("destroy", "--data-dir", dir, "--force")
	want(t, "destroy", code, 0, out+errs, "kept the extensions' data in it")
	if len(order) == 0 || order[len(order)-1] != "run" || slices.Contains(order, "extensions") {
		t.Errorf("moved %v: run/ must go last, extensions/ never", order)
	}
}

// A data folder with only extension data (AgentK installed without Meko) has nothing of Meko's to
// remove, unless --with-extensions.
func TestDestroyOnlyExtensionData(t *testing.T) {
	dir, _ := setup(t)
	log, kept := withExtensionData(t, dir, false)
	if err := os.RemoveAll(paths.New(dir).DB); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("destroy", "--data-dir", dir, "--force")
	want(t, "destroy of extension data only", code, 0, out+errs, "Nothing of Meko's to remove: "+dir+
		" holds only extension data")
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the extension's data is gone: %v", err)
	}
	code, out, errs = run("destroy", "--data-dir", dir, "--force", "--with-extensions")
	want(t, "destroy --with-extensions of extension data only", code, 0, out+errs, "Removed")
	want(t, "the extension's uninstall", 0, 0, readLog(t, log), "uninstall --purge")
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Errorf("the extension's data is still there: %v", err)
	}
}

func TestUI(t *testing.T) {
	dir, port := setup(t)
	code, _, errs := run("ui", "--data-dir", dir, "--no-open")
	want(t, "ui when stopped", code, exitNotRunning, errs, "isn't running")

	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	web := "http://127.0.0.1:" + os.Getenv("MEKO_WEB_PORT")

	var opened []string
	open := openBrowser
	openBrowser = func(u string) error { opened = append(opened, u); return nil }
	t.Cleanup(func() { openBrowser = open })

	// With local access the link is the web listener's own address: it carries no credential.
	code, out, _ = run("ui", "--data-dir", dir, "--no-open")
	if code != 0 || strings.TrimSpace(out) != web+"/" || len(opened) != 0 {
		t.Errorf("ui --no-open: %d %q, opened %v", code, out, opened)
	}
	code, out, _ = run("ui", "/ext/agentk/", "--data-dir", dir)
	if link := strings.TrimSpace(out); code != 0 || link != web+"/ext/agentk/" || len(opened) != 1 || opened[0] != link {
		t.Errorf("ui /ext/agentk/: %d %q, opened %v", code, out, opened)
	}
	code, out, _ = run("status", "--data-dir", dir, "--verbose")
	want(t, "status's web line", code, 0, out, "Humans:        "+web+" (web UI), local, from this computer")
	code, out, _ = run("token", "--data-dir", dir, "--json")
	want(t, "token --json's web URL", code, 0, out, `"web_url":"`+web+`"`)
	code, _, errs = run("setup_link", "--data-dir", dir)
	want(t, "setup_link is gone", code, exitUsage, errs, "unknown command")

	// meko ui stop closes the port at once, and it stays closed across a restart until meko ui start.
	code, out, _ = run("ui", "stop", "--data-dir", dir)
	want(t, "ui stop", code, 0, out, "Stopped the web UI")
	//nolint:gosec // G704: the test's own server
	if c, err := net.DialTimeout("tcp", strings.TrimPrefix(web, "http://"), time.Second); err == nil {
		_ = c.Close()
		t.Error("the web port still answers after ui stop")
	}
	code, out, _ = run("ui", "stop", "--data-dir", dir)
	want(t, "ui stop again", code, 0, out, "The web UI is already off")
	code, _, errs = run("ui", "--data-dir", dir, "--no-open")
	want(t, "ui while off", code, exitError, errs, "the web UI is off. Turn it on with meko ui start")
	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status while off", code, 0, out, "Web UI          off (meko ui start turns it on)")
	code, out, _ = run("token", "--data-dir", dir, "--json")
	want(t, "token --json while off", code, 0, out, `"web_off":true,"web_url":""`)
	code, out, errs = run("stop", "--data-dir", dir)
	want(t, "stop", code, 0, out+errs, "Server          stopped")
	code, out, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start while off", code, 0, out+errs, "Web UI          off (meko ui start turns it on)")
	code, out, _ = run("ui", "start", "--data-dir", dir)
	want(t, "ui start", code, 0, out, "Started the web UI at "+web)
	code, out, _ = run("ui", "--data-dir", dir, "--no-open")
	if code != 0 || strings.TrimSpace(out) != web+"/" {
		t.Errorf("ui after ui start: %d %q", code, out)
	}
}

// With Meko stopped, meko ui stop and start only set the switch, which the next start reads; and
// config.toml's own [web] enabled stops them.
func TestUISwitchWhileStopped(t *testing.T) {
	dir, _ := setup(t)
	code, out, _ := run("ui", "stop", "--data-dir", dir)
	want(t, "ui stop", code, 0, out, "Turned the web UI off; it stays off when Meko starts")
	code, out, _ = run("ui", "start", "--data-dir", dir)
	want(t, "ui start", code, 0, out, "Turned the web UI on; it starts with Meko (meko start)")
	cfgPath := paths.New(dir).Config
	if err := os.WriteFile(cfgPath, []byte("[web]\nenabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run("ui", "stop", "--data-dir", dir)
	want(t, "ui stop with config.toml's enabled", code, exitError, errs,
		"config.toml sets [web] enabled; change it there and run meko restart")
	code, _, errs = run("ui", "nope", "stop", "--data-dir", dir)
	want(t, "a path and a word", code, exitUsage, errs, "unexpected argument")
}

func TestSuggestedName(t *testing.T) {
	for in, want := range map[string]string{
		`attachment; filename="meko-default-2026-10-22.zip"`: "meko-default-2026-10-22.zip",
		`attachment; filename="../../etc/passwd"`:            "passwd",
		``:           "meko-export.zip",
		`attachment`: "meko-export.zip",
	} {
		if got := suggestedName(in); got != want {
			t.Errorf("suggestedName(%q) = %q, want %q", in, got, want)
		}
	}
}

// export writes a datapack's JSON to the file the server names, here or at -o, by name, id or the
// default; it won't replace a file unless --force, and an unknown name exits 1.
func TestExport(t *testing.T) {
	memoryEngineDir, err := filepath.Abs(filepath.Join("..", "..", "memory_engine"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(memoryEngineDir, "dist", "main.js")); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal(err)
		}
		t.Skip("the memory engine isn't built (make test builds it)")
	}
	dir, port := setup(t)
	t.Setenv("MEKO_MEMORY_ENGINE_DIR", memoryEngineDir)
	t.Setenv(testEmbedderEnv, "hash")
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, out, _ = run("status", "--data-dir", dir, "--verbose"); strings.Contains(out, "Memory engine: ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the memory engine never got ready:\n%s", out)
		}
	}
	code, out, errs = runSh("add_memory", "--data-dir", dir, "releases ship on Tuesdays")
	want(t, "add_memory", code, 0, out+errs, "Added memory")

	here := t.TempDir()
	t.Chdir(here)
	code, out, errs = runSh("export", "--data-dir", dir)
	want(t, "export the default", code, 0, out+errs, "Exported to meko-default-")
	names, _ := filepath.Glob(filepath.Join(here, "meko-default-*.zip"))
	if len(names) != 1 {
		t.Fatalf("files %v", names)
	}
	z, err := zip.OpenReader(names[0])
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{}
	for _, f := range z.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		entries[f.Name] = string(b)
	}
	_ = z.Close()
	var header struct {
		Format  string `json:"format"`
		Version int    `json:"version"`
	}
	var mem struct {
		Text      string    `json:"text"`
		Embedding []float32 `json:"embedding"`
	}
	if err = json.Unmarshal([]byte(entries["datapack.json"]), &header); err != nil ||
		header.Format != "meko-datapack-export" || header.Version != 2 {
		t.Fatalf("header %v %q", err, entries["datapack.json"])
	}
	if err = json.Unmarshal([]byte(entries["memories.jsonl"]), &mem); err != nil ||
		mem.Text != "releases ship on Tuesdays" || len(mem.Embedding) == 0 {
		t.Fatalf("memories %v %q", err, entries["memories.jsonl"])
	}
	code, _, errs = runSh("export", "--data-dir", dir)
	want(t, "export again", code, 1, errs, "already exists: pass --force")
	code, out, errs = runSh("export", "--data-dir", dir, "--force")
	want(t, "export --force", code, 0, out+errs, "Exported to")

	out2 := filepath.Join(here, "by-name.zip")
	code, out, errs = runSh("export", "default", "--data-dir", dir, "-o", out2)
	want(t, "export by name with -o after it", code, 0, out+errs, "Exported to "+out2)
	code, out, errs = runSh("export", "--data-dir", dir, "-o", filepath.Join(here, "first.zip"), "default")
	want(t, "export with -o before the name", code, 0, out+errs, "first.zip")
	code, _, errs = runSh("export", "nope", "--data-dir", dir)
	want(t, "export an unknown name", code, 1, errs, `no datapack is named "nope"`)
}

// An export is complete only if it opens as a zip holding its datapack.json.
func TestCheckExport(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, entries ...string) string {
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		for _, e := range entries {
			w, _ := z.Create(e)
			_, _ = w.Write([]byte("{}"))
		}
		_ = z.Close()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if err := checkExport(write("ok.zip", "memories.jsonl", "datapack.json")); err != nil {
		t.Errorf("a complete export: %v", err)
	}
	if err := checkExport(write("headless.zip", "memories.jsonl")); err == nil {
		t.Error("an export without datapack.json passed")
	}
	full := write("full.zip", "memories.jsonl", "datapack.json")
	b, _ := os.ReadFile(full)
	cut := filepath.Join(dir, "cut.zip")
	_ = os.WriteFile(cut, b[:len(b)/2], 0o600) //nolint:gosec // G703: the test's own temp folder
	if err := checkExport(cut); err == nil {
		t.Error("a cut-off export passed")
	}
}
