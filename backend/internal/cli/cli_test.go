package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	want(t, "status", code, 0, out, "Children:      none")
	want(t, "status's auth", code, 0, out, "Auth:          token")

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
	// A killed server's port file is left behind; with no server, --mcp names the configured port.
	if err := paths.WriteFile(paths.New(dir).Port, []byte("1\n")); err != nil {
		t.Fatal(err)
	}
	code, mcpOut, _ := run("token", "--data-dir", dir, "--mcp")
	want(t, "token --mcp with no server", code, 0, mcpOut, "http://127.0.0.1:7778/mcp")
	for i := 0; i < 2; i++ {
		started, out, serrs := run("start", "--data-dir", dir, "--port", port)
		want(t, "start", started, 0, out+serrs, "is running")
		_, again, _ := run("token", "--data-dir", dir)
		if again != tok {
			t.Fatalf("start %d: token %q, want %q", i, again, tok)
		}
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
	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status", code, 0, out, "Auth:          none (every user and program on this machine")
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
	want(t, "start", code, 0, out+errs, "Meko is running")
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
	want(t, "start", code, 0, out+errs, "Meko is running")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		_, out, _ = run("status", "--data-dir", dir)
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
	want(t, "stop", code, 0, out, "Meko stopped.")
	if memoryEnginePid == 0 || syscall.Kill(memoryEnginePid, 0) == nil {
		t.Errorf("the memory engine (pid %d) outlived stop", memoryEnginePid)
	}
	for _, p := range []string{l.StoreSock, l.MemoryEngineSock, filepath.Join(l.ChildrenDir, "memory_engine")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind", p)
		}
	}
}

func TestSearchAddAndDatapacks(t *testing.T) {
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
	code, _, errs := run("search", "--data-dir", dir, "anything")
	want(t, "search with no server", code, exitNotRunning, errs, "isn't running")

	t.Setenv("MEKO_MEMORY_ENGINE_DIR", memoryEngineDir)
	t.Setenv(testEmbedderEnv, "hash")
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "Meko is running")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, out, _ = run("status", "--data-dir", dir); strings.Contains(out, "Memory engine: ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the memory engine never got ready:\n%s", out)
		}
	}

	code, out, errs = run("datapacks", "--data-dir", dir)
	want(t, "datapacks", code, 0, out+errs, "  default  (Created by Meko on first start.)")
	const note = "the build cache is in memory_engine/.models"
	code, out, errs = run("add_memory", "--data-dir", dir, note, "--datapack", "default")
	want(t, "add_memory", code, 0, out+errs, "Added memory ")
	code, out, errs = run("search", "--data-dir", dir, note, "--limit", "3")
	want(t, "search", code, 0, out+errs, note+"  (meko_cli, ")
	code, out, errs = run("search", "--data-dir", dir, "x", "--json", "--source", "memory")
	want(t, "search --json", code, 0, out+errs, `"knowledge_base":null`)
	code, _, errs = run("add_memory", "--data-dir", dir, "x", "--datapack", "nope")
	want(t, "add_memory to a missing datapack", code, 1, errs, `no datapack is named "nope"`)

	// A datapack made, used by name, and deleted only when the answer is yes.
	code, out, errs = run("create_datapack", "--data-dir", dir, "scratch-notes")
	want(t, "create_datapack", code, 0, out+errs, "Created datapack scratch-notes (")
	code, out, errs = run("create_datapack", "--data-dir", dir, "scratch-notes")
	want(t, "create_datapack twice", code, 1, out+errs, "CONFLICT")
	code, out, errs = run("add_memory", "--data-dir", dir, "kept in scratch", "--datapack", "scratch-notes")
	want(t, "add_memory by name", code, 0, out+errs, "Added memory ")
	withStdin(t, "n\n")
	code, out, errs = run("delete_datapack", "--data-dir", dir, "scratch-notes")
	want(t, "delete_datapack answered n", code, 1, errs, "Nothing was deleted.")
	want(t, "delete_datapack's question", code, 1, out, "This deletes datapack scratch-notes and everything "+
		"in it (memories, conversations, artifacts) for good.\nDelete it? [y/N] ")
	if _, out, _ = run("datapacks", "--data-dir", dir); !strings.Contains(out, "scratch-notes") {
		t.Errorf("a declined delete_datapack deleted it:\n%s", out)
	}
	withStdin(t, "y\n")
	code, out, errs = run("delete_datapack", "--data-dir", dir, "scratch-notes")
	want(t, "delete_datapack", code, 0, out+errs, "Deleted datapack scratch-notes.")
	if _, out, _ = run("datapacks", "--data-dir", dir); strings.Contains(out, "scratch-notes") {
		t.Errorf("the deleted datapack is still listed:\n%s", out)
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
	code, out, errs = run("upload", "--data-dir", dir, notes, "--datapack", "default")
	want(t, "upload", code, 0, out+errs, notes+": indexed, 1 chunk (key ")
	code, out, errs = run("upload", "--data-dir", dir, notes)
	want(t, "upload again", code, 0, out+errs, "already in the knowledge base as notes.md")
	want(t, "upload again waits for it", code, 0, out, notes+": indexed, 1 chunk (key ")
	code, out, errs = run("upload", "--data-dir", dir, bad, notes)
	want(t, "upload a bad file", code, 1, out+errs, "doesn't look like a PDF")
	want(t, "upload a bad file", code, 1, errs, "1 of 2 files weren't added")
	code, out, errs = run("search", "--data-dir", dir, "when does the nightly build run", "--source", "knowledge_base")
	want(t, "search the knowledge base", code, 0, out+errs, "Knowledge base (relevance):")
	code, out, errs = run("upload", "--data-dir", dir, notes, "--json")
	want(t, "upload --json", code, 0, out+errs, `"already_uploaded":true`)
	var offer struct{ Key string }
	// --json prints the tool's reply, then the file's final status.
	if err = json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]), &offer); err != nil {
		t.Fatalf("upload --json: %v: %s", err, out)
	}
	code, out, errs = run("upload_status", "--data-dir", dir, offer.Key)
	want(t, "upload_status", code, 0, out+errs, offer.Key+": completed. The file is indexed and searchable.")
	code, _, errs = run("upload_status", "--data-dir", dir, "no-such-key")
	want(t, "upload_status of an unknown key", code, 1, errs, "NOT_FOUND")

	_, tok, _ := run("token", "--data-dir", dir)
	code, out, _ = run("token", "--data-dir", dir, "--mcp")
	want(t, "token --mcp", code, 0, out, "claude mcp add --transport http meko http://127.0.0.1:"+port+
		"/mcp --header \"Authorization: Bearer "+strings.TrimSpace(tok)+"\"")

	// The default datapack isn't made again, so deleting it says so, and calls that name none then
	// ask for --datapack.
	withStdin(t, "")
	code, out, errs = run("delete_datapack", "--data-dir", dir, "default")
	want(t, "delete_datapack default with no answer", code, 1, out+errs, "It's the default datapack")
	want(t, "delete_datapack default with no answer", code, 1, errs, "Nothing was deleted.")
	code, out, errs = run("delete_datapack", "--data-dir", dir, "default", "--force")
	want(t, "delete_datapack default --force", code, 0, out+errs, "Deleted datapack default.")
	if strings.Contains(out, "Delete it?") {
		t.Errorf("delete_datapack --force asked:\n%s", out)
	}
	code, _, errs = run("add_memory", "--data-dir", dir, "x")
	want(t, "add_memory with no default", code, 1, errs, "there's no default datapack (it was deleted): pass --datapack")
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
	want(t, "start", code, 0, out+errs, "Meko is running")

	for _, answer := range []string{"n\n", "", "whatever\n"} {
		withStdin(t, answer)
		code, out, errs = run("destroy", "--data-dir", dir)
		want(t, "destroy answered "+strconv.Quote(answer), code, 1, errs, "Nothing was removed.")
		want(t, "destroy's question", code, 1, out, "Meko is running, so it's stopped first.\nDestroy it? [y/N] ")
		want(t, "destroy's summary", code, 1, out, "This removes Meko's data in "+dir+" (")
		code, out, _ = run("status", "--data-dir", dir)
		want(t, "status after a declined destroy", code, 0, out, "Meko is running.")
	}

	withStdin(t, "Yes\n")
	code, out, errs = run("destroy", "--data-dir", dir)
	// A piped answer isn't echoed, so destroy ends the prompt's line itself.
	want(t, "destroy", code, 0, out+errs, "Destroy it? [y/N] \nMeko stopped.\nRemoved "+dir+".\n")
	want(t, "destroy's note", code, 0, out, "its token no longer works: remove it (claude mcp remove meko)")
	gone(t, dir)
	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status after destroy", code, exitNotRunning, out, "isn't running")

	code, out, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start after destroy", code, 0, out+errs, "Meko is running")
}

func TestDestroyForce(t *testing.T) {
	dir, _ := setup(t)
	if err := os.WriteFile(paths.New(dir).DB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	withStdin(t, "")
	code, out, errs := run("destroy", "--data-dir", dir, "--force")
	want(t, "destroy --force", code, 0, out+errs, "Removed "+dir+".")
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
	want(t, "destroy of an empty folder", code, 0, out+errs, "Nothing to remove: the data folder "+empty+" is empty.")
	// A link to an empty folder is left too: there's nothing to remove.
	link := filepath.Join(shortDir(t), "link")
	if err := os.Symlink(empty, link); err != nil {
		t.Fatal(err)
	}
	code, out, errs = run("destroy", "--data-dir", link, "--force")
	want(t, "destroy of a link to an empty folder", code, 0, out+errs, "is empty.")
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
	want(t, "destroy of a hung server", code, 0, out+errs, "so it was killed.\nRemoved "+dir+".")
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
	want(t, "destroy through a symlink", code, 0, out+errs, "Removed "+resolved+".")
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
